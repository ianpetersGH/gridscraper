package scraper

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, ctype, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{ctype}}, Body: io.NopCloser(strings.NewReader(body))}
}

const portalFixture = "Date,Start Time,End Time,Usage (kWh)\n2026-01-01,00:00,01:00,0\n"

func TestPollExportDoesNotLeakSecrets(t *testing.T) {
	const secret = "TEST_SECRET_SENTINEL"
	signed := "https://fixture.s3.us-east-1.amazonaws.com/export.csv?X-Amz-Signature=" + secret
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			if !isNYSEGAPIURL(r.URL.String()) || r.Header.Get("Up-Authorization") != secret {
				t.Fatal("missing scoped authentication")
			}
			return response(200, "application/json", `{"code":"PROMISE_FOUND","promise_url":"`+signed+`"}`), nil
		}
		if r.URL.String() != signed || len(r.Header) != 0 {
			t.Fatalf("export download got auth headers or wrong URL")
		}
		return response(200, "text/csv", portalFixture), nil
	})}
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	rows, pollErr := (&NYSEGDirectScraper{}).pollForCSV(context.Background(), "fixture_job", client, http.Header{"Up-Authorization": []string{secret}})
	w.Close()
	os.Stdout = old
	logs, _ := io.ReadAll(r)
	r.Close()
	if pollErr != nil || len(rows) != 1 || rows[0].KWh != 0 || calls != 2 {
		t.Fatalf("poll: %+v %v calls=%d", rows, pollErr, calls)
	}
	if strings.Contains(string(logs), secret) || strings.Contains(string(logs), signed) {
		t.Fatalf("secret leaked: %s", logs)
	}
}

func TestPollFailureSuppressesBodyAndURL(t *testing.T) {
	for _, status := range []int{401, 403, 500, 302} {
		calls := 0
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			resp := response(status, "text/plain", "TEST_BODY_SECRET")
			resp.Header.Set("Location", "https://evil.invalid/?token=TEST_URL_SECRET")
			return resp, nil
		})}
		_, err := (&NYSEGDirectScraper{}).pollForCSV(context.Background(), "job", client, nil)
		if err == nil || strings.Contains(err.Error(), "SECRET") || calls != 1 {
			t.Fatalf("status %d: %v, %d calls", status, err, calls)
		}
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("https://test.invalid?token=TEST_URL_SECRET")
	})}
	_, err := (&NYSEGDirectScraper{}).pollForCSV(context.Background(), "job", client, nil)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("network error leaked: %v", err)
	}
}

func TestPartialExportNeverImported(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		cancel()
		return response(200, "application/json", `{"code":"PROMISE_FOUND_PARTIAL_DATA","promise_url":"https://fixture.s3.amazonaws.com/data.csv"}`), nil
	})}
	_, err := (&NYSEGDirectScraper{}).pollForCSV(ctx, "job", client, nil)
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("partial result consumed: %v calls=%d", err, calls)
	}
}

func TestNYSEGURLPolicyAndLimits(t *testing.T) {
	for _, s := range []string{"http://bucket.s3.amazonaws.com/x", "https://localhost/x", "https://user:pass@bucket.s3.amazonaws.com/x", "https://bucket.s3.amazonaws.com.evil.invalid/x", "https://bucket.s3.amazonaws.com:443/x", "https://evilamazonaws.com/x"} {
		if validateNYSEGDownloadURL(s) == nil {
			t.Errorf("accepted %s", s)
		}
	}
	for _, s := range []string{"https://bucket.s3.amazonaws.com/x?signature=fixture", "https://bucket.s3.us-east-1.amazonaws.com/x", "https://s3.us-east-1.amazonaws.com/bucket/x"} {
		if err := validateNYSEGDownloadURL(s); err != nil {
			t.Error(err)
		}
	}
	for _, s := range []string{"https://" + nysegAPIHost + ".evil.invalid/usage", "http://" + nysegAPIHost + "/usage", "https://user@" + nysegAPIHost + "/usage"} {
		if isNYSEGAPIURL(s) {
			t.Errorf("trusted %s", s)
		}
	}
	if _, err := readNYSEGBody(strings.NewReader("12345"), 4); err == nil {
		t.Fatal("missing body size limit")
	}
	if got := safeNYSEGError(errors.New("token=SECRET")).Error(); strings.Contains(got, "SECRET") {
		t.Fatal(got)
	}
}

func TestBrowserSecurityFlags(t *testing.T) {
	// A fake executable records real chromedp process arguments without visiting
	// a website, starting Chromium or receiving any real credentials.
	for _, optOut := range []string{"", "1"} {
		t.Run("optout="+optOut, func(t *testing.T) {
			dir := t.TempDir()
			exe := filepath.Join(dir, "fake-chrome")
			argsPath := filepath.Join(dir, "args")
			if err := os.WriteFile(exe, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGS_FILE\"\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ARGS_FILE", argsPath)
			t.Setenv("GRIDSCRAPER_NO_SANDBOX", optOut)
			opts := append(NYSEGBrowserOptions(false), chromedp.ExecPath(exe))
			ctx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
			defer cancel()
			ctx, cancel = chromedp.NewContext(ctx)
			defer cancel()
			if err := chromedp.Run(ctx); err == nil {
				t.Fatal("fake browser unexpectedly succeeded")
			}
			b, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatal(err)
			}
			args := string(b)
			if strings.Contains(args, "--no-sandbox\n") != (optOut == "1" && os.Geteuid() != 0) {
				t.Fatalf("incorrect sandbox policy: %s", args)
			}
			for _, bad := range []string{"--disable-web-security", "--disable-features=IsolateOrigins", "--disable-features=site-per-process", "--remote-debugging-port=9222", "chrome-debug-profile", "--disable-ipc-flooding-protection"} {
				if strings.Contains(args, bad) {
					t.Errorf("unsafe flag: %s", bad)
				}
			}
			if !strings.Contains(args, "--site-per-process\n") {
				t.Fatal("site isolation not enabled")
			}
		})
	}
}
