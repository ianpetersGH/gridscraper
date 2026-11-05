package scraper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

const nysegAPIHost = "engage-api-gw-dod79bsd.ue.gateway.dev"

var promiseIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

func validateNYSEGDownloadURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Fragment != "" {
		return fmt.Errorf("invalid CSV download URL (suppressed)")
	}
	host := strings.ToLower(u.Hostname())
	// Existing portal exports use S3. Reject unexpected destinations rather than
	// allowing a portal response to turn into an arbitrary internal HTTP request.
	if !strings.HasSuffix(host, ".amazonaws.com") || !(strings.HasPrefix(host, "s3.") || strings.Contains(host, ".s3.") || strings.HasSuffix(host, ".s3.amazonaws.com")) {
		return fmt.Errorf("CSV download host is not an approved HTTPS S3 endpoint")
	}
	return nil
}

func readNYSEGBody(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading NYSEG response: %w", safeNYSEGError(err))
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("NYSEG response exceeds size limit")
	}
	return b, nil
}

// NYSEGBrowserOptions keeps site isolation and the browser sandbox enabled by
// default, including when chromedp would otherwise disable sandbox for root.
// Only a non-root container may explicitly opt out of sandbox; see README.
func NYSEGBrowserOptions(visible bool) []chromedp.ExecAllocatorOption {
	opts := append([]chromedp.ExecAllocatorOption(nil), chromedp.DefaultExecAllocatorOptions[:]...)
	return append(opts,
		chromedp.Flag("headless", !visible && os.Getenv("GRIDSCRAPER_VIRTUAL_DISPLAY") != "1"),
		chromedp.Flag("no-sandbox", os.Getenv("GRIDSCRAPER_NO_SANDBOX") == "1" && os.Geteuid() != 0),
		chromedp.Flag("disable-dev-shm-usage", true),
		// chromedp defaults disable site-per-process and several protection features.
		chromedp.Flag("disable-features", "Translate,BlinkGenPropertyTrees"),
		chromedp.Flag("site-per-process", true),
		chromedp.Flag("disable-ipc-flooding-protection", false),
		chromedp.Flag("disable-client-side-phishing-detection", false),
		chromedp.Flag("safebrowsing-disable-auto-update", false),
		chromedp.WindowSize(1920, 1080),
	)
}

// openNYSEGLogin is deliberately credential-free. A navigation/selector failure
// here must not be reported as rejected credentials or retried as a login.
func openNYSEGLogin(ctx context.Context) error {
	if err := chromedp.Run(ctx,
		chromedp.Navigate("https://sso.nyseg.com/"),
		chromedp.Sleep(2*time.Second),
		chromedp.WaitVisible(`input#_com_liferay_login_web_portlet_LoginPortlet_login`, chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("opening NYSEG login page (no credentials submitted): %w", safeNYSEGError(err))
	}
	return nil
}

// openNYSEGInsights bounds the legacy selector wait and emits only booleans.
// Never dump authenticated page text, account identifiers, URLs or HTML.
func openNYSEGInsights(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	err := chromedp.Run(waitCtx,
		chromedp.Navigate(nysegInsightsURL),
		chromedp.WaitVisible(`div.engage-insights-explore`, chromedp.ByQuery),
	)
	if err == nil {
		return nil
	}
	var state struct {
		SSO      bool `json:"sso"`
		Energy   bool `json:"energy"`
		Login    bool `json:"login"`
		Password bool `json:"password"`
		Verify   bool `json:"verify"`
		Alert    bool `json:"alert"`
		Download bool `json:"download"`
	}
	probeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	probeErr := chromedp.Run(probeCtx, chromedp.Evaluate(`(() => ({
        sso: location.hostname === "sso.nyseg.com",
        energy: location.hostname === "energymanager.nyseg.com",
        login: !!document.querySelector("input#_com_liferay_login_web_portlet_LoginPortlet_login"),
        password: !!document.querySelector("input[type=password]"),
        verify: !!document.querySelector('input[autocomplete="one-time-code"]'),
        alert: Array.from(document.querySelectorAll('.alert-danger,.alert-error,[role=alert]')).some(e => !!e.innerText.trim()),
        download: !!document.body && document.body.innerText.includes("Download my energy usage data")
    }))()`, &state))
	if probeErr != nil {
		return fmt.Errorf("waiting for NYSEG Energy Manager (page state unavailable): %w", safeNYSEGError(err))
	}
	// Permit a changed container layout only when the expected export UI exists
	// at the exact trusted portal host. The next stage still verifies the modal.
	if state.Energy && state.Download {
		return nil
	}
	return fmt.Errorf("waiting for NYSEG Energy Manager (sso=%t energy=%t login_form=%t password_field=%t verification_field=%t alert=%t download_ui=%t): %w",
		state.SSO, state.Energy, state.Login, state.Password, state.Verify, state.Alert, state.Download, safeNYSEGError(err))
}

func isNYSEGAPIURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == nysegAPIHost && u.User == nil
}

// Browser/HTTP errors can embed signed URLs or even script source. Do not
// propagate their arbitrary text to logs. Preserve useful cancellation classes.
func safeNYSEGError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	// Emit only constant allowlisted codes, never arbitrary text, signed URLs,
	// script source, request headers or a response containing user credentials.
	for _, code := range []string{
		"net::ERR_HTTP2_PROTOCOL_ERROR", "net::ERR_CONNECTION_RESET",
		"net::ERR_CONNECTION_CLOSED", "net::ERR_CONNECTION_TIMED_OUT",
		"net::ERR_NAME_NOT_RESOLVED", "net::ERR_CERT_DATE_INVALID",
		"net::ERR_CERT_AUTHORITY_INVALID", "net::ERR_SSL_PROTOCOL_ERROR",
	} {
		if strings.Contains(err.Error(), code) {
			return errors.New(code)
		}
	}
	return errors.New("operation failed (remote details suppressed to protect credentials)")
}
