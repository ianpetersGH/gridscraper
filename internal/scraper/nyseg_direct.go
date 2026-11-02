package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/jgoulah/gridscraper/internal/config"
	"github.com/jgoulah/gridscraper/pkg/models"
)

// AuthError represents an authentication failure
type AuthError struct {
	StatusCode int
	Message    string
}

func (e *AuthError) Error() string {
	return e.Message
}

// NYSEGDirectScraper uses browser-authenticated CSV export (legacy type name).
type NYSEGDirectScraper struct {
	cookies   []config.Cookie
	authToken string
	username  string
	password  string
	visible   bool
}

// SetVisible sets whether to show the browser window
func (s *NYSEGDirectScraper) SetVisible(visible bool) {
	s.visible = visible
}

// NewNYSEGDirectScraper creates a NYSEG browser CSV scraper
func NewNYSEGDirectScraper(cookies []config.Cookie) *NYSEGDirectScraper {
	return &NYSEGDirectScraper{cookies: cookies}
}

// NewNYSEGDirectScraperWithToken creates a NYSEG browser CSV scraper with auth token
func NewNYSEGDirectScraperWithToken(cookies []config.Cookie, authToken string) *NYSEGDirectScraper {
	return &NYSEGDirectScraper{
		cookies:   cookies,
		authToken: authToken,
	}
}

// NewNYSEGDirectScraperWithCredentials creates a NYSEG browser CSV scraper with credentials for auto-login
func NewNYSEGDirectScraperWithCredentials(cookies []config.Cookie, authToken, username, password string) *NYSEGDirectScraper {
	return &NYSEGDirectScraper{
		cookies:   cookies,
		authToken: authToken,
		username:  username,
		password:  password,
	}
}

// RefreshAuth performs login and refreshes cookies and auth token
func (s *NYSEGDirectScraper) RefreshAuth(ctx context.Context) ([]config.Cookie, string, error) {
	if s.username == "" || s.password == "" {
		return nil, "", fmt.Errorf("cannot auto-refresh: no username/password configured")
	}

	fmt.Println("Refreshing authentication...")

	opts := NYSEGBrowserOptions(s.visible)

	allocCtx, cancel := chromedp.NewExecAllocator(ctx, opts...)
	defer cancel()

	browserCtx, cancel := chromedp.NewContext(allocCtx, chromedp.WithErrorf(func(string, ...interface{}) { fmt.Println("NYSEG browser protocol error (details suppressed)") }))
	defer cancel()

	browserCtx, cancel = context.WithTimeout(browserCtx, 60*time.Second)
	defer cancel()

	// Set up network monitoring to capture auth token from API requests
	var capturedAuthToken string
	var tokenCaptured bool
	var authMu sync.Mutex
	chromedp.ListenTarget(browserCtx, func(ev interface{}) {
		switch ev := ev.(type) {
		case *network.EventRequestWillBeSent:
			if !isNYSEGAPIURL(ev.Request.URL) {
				return
			}
			authMu.Lock()
			defer authMu.Unlock()
			// Capture auth token only from the expected API host.
			if !tokenCaptured {
				// Try Up-Authorization first
				if authToken, ok := ev.Request.Headers["Up-Authorization"]; ok {
					if authStr, ok := authToken.(string); ok && authStr != "" {
						capturedAuthToken = authStr
						tokenCaptured = true
						fmt.Printf("  [DEBUG] Captured Up-Authorization token\n")
					}
				}
				// Also try X-Authentication as fallback
				if !tokenCaptured {
					if authToken, ok := ev.Request.Headers["X-Authentication"]; ok {
						if authStr, ok := authToken.(string); ok && authStr != "" {
							capturedAuthToken = authStr
							tokenCaptured = true
							fmt.Printf("  [DEBUG] Captured X-Authentication token\n")
						}
					}
				}
			}
		}
	})

	if err := chromedp.Run(browserCtx, network.Enable()); err != nil {
		return nil, "", fmt.Errorf("enabling network: %w", safeNYSEGError(err))
	}
	if err := openNYSEGLogin(browserCtx); err != nil {
		return nil, "", err
	}
	if err := chromedp.Run(browserCtx,
		chromedp.SendKeys(`input#_com_liferay_login_web_portlet_LoginPortlet_login`, s.username, chromedp.ByQuery),
		chromedp.SendKeys(`input#_com_liferay_login_web_portlet_LoginPortlet_password`, s.password, chromedp.ByQuery),
		chromedp.Sleep(500*time.Millisecond),
		chromedp.Click(`button[type="submit"]`, chromedp.ByQuery),
		chromedp.Sleep(5*time.Second), // Wait longer for redirect and auth
	); err != nil {
		return nil, "", fmt.Errorf("submitting NYSEG login form: %w", safeNYSEGError(err))
	}

	// Navigate to insights to trigger API calls that will include the auth token
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(nysegInsightsURL),
		chromedp.Sleep(3*time.Second), // Wait for page to load
		chromedp.WaitVisible(`div.engage-insights-explore`, chromedp.ByQuery),
		chromedp.Sleep(3*time.Second), // Wait for API calls to complete
	); err != nil {
		return nil, "", fmt.Errorf("navigating to insights: %w", safeNYSEGError(err))
	}

	// Extract cookies
	freshCookies, err := ExtractCookies(browserCtx)
	if err != nil {
		return nil, "", fmt.Errorf("extracting cookies: %w", safeNYSEGError(err))
	}

	authMu.Lock()
	defer authMu.Unlock()
	// Use the auth token captured from network requests
	if capturedAuthToken == "" {
		return nil, "", fmt.Errorf("could not capture auth token from network requests (did the page load correctly?)")
	}

	fmt.Println("✓ Authentication refreshed successfully")
	return freshCookies, capturedAuthToken, nil
}

// Scrape fetches usage data from NYSEG using browser-based CSV download
// The v2/usage API only returns billing period summaries, so we use browser
// automation to download the CSV which contains hourly data.
func (s *NYSEGDirectScraper) Scrape(ctx context.Context, daysToFetch int) ([]models.UsageData, error) {
	if s.username == "" || s.password == "" {
		return nil, fmt.Errorf("username and password required for NYSEG scraping")
	}

	fmt.Println("Starting browser-based CSV download for hourly data...")

	opts := NYSEGBrowserOptions(s.visible)

	allocCtx, cancel := chromedp.NewExecAllocator(ctx, opts...)
	defer cancel()

	browserCtx, cancel := chromedp.NewContext(allocCtx, chromedp.WithErrorf(func(string, ...interface{}) { fmt.Println("NYSEG browser protocol error (details suppressed)") }))
	defer cancel()

	browserCtx, cancel = context.WithTimeout(browserCtx, 3*time.Minute)
	defer cancel()

	// Track auth token and x-authentication for direct API calls
	var capturedUpAuth string
	var capturedXAuth string
	var tokenMu sync.Mutex

	// Listen for network requests to capture auth tokens
	chromedp.ListenTarget(browserCtx, func(ev interface{}) {
		switch ev := ev.(type) {
		case *network.EventRequestWillBeSent:
			// Capture auth tokens from API requests
			if isNYSEGAPIURL(ev.Request.URL) {
				tokenMu.Lock()
				if capturedUpAuth == "" {
					if authToken, ok := ev.Request.Headers["Up-Authorization"]; ok {
						if authStr, ok := authToken.(string); ok && authStr != "" {
							capturedUpAuth = authStr
							fmt.Printf("  [AUTH] Captured Up-Authorization token\n")
						}
					}
				}
				if capturedXAuth == "" {
					if xAuth, ok := ev.Request.Headers["X-Authentication"]; ok {
						if xAuthStr, ok := xAuth.(string); ok && xAuthStr != "" {
							capturedXAuth = xAuthStr
							fmt.Printf("  [AUTH] Captured X-Authentication token\n")
						}
					}
				}
				tokenMu.Unlock()
			}
		}
	})

	// Enable network monitoring
	if err := chromedp.Run(browserCtx,
		network.Enable(),
	); err != nil {
		return nil, fmt.Errorf("enabling network: %w", safeNYSEGError(err))
	}

	fmt.Println("Opening NYSEG login page (no credentials submitted yet)...")
	if err := openNYSEGLogin(browserCtx); err != nil {
		return nil, err
	}
	fmt.Println("Submitting NYSEG login form...")
	if err := chromedp.Run(browserCtx,
		chromedp.SendKeys(`input#_com_liferay_login_web_portlet_LoginPortlet_login`, s.username, chromedp.ByQuery),
		chromedp.SendKeys(`input#_com_liferay_login_web_portlet_LoginPortlet_password`, s.password, chromedp.ByQuery),
		chromedp.Sleep(500*time.Millisecond),
		chromedp.Click(`button[type="submit"]`, chromedp.ByQuery),
	); err != nil {
		return nil, fmt.Errorf("submitting NYSEG login form: %w", safeNYSEGError(err))
	}

	// Wait a bit for login to process, then explicitly navigate to insights
	// (the NYSEG login page doesn't auto-redirect, we need to navigate manually)
	fmt.Println("Waiting for login to process...")
	if err := chromedp.Run(browserCtx,
		chromedp.Sleep(5*time.Second),
	); err != nil {
		return nil, fmt.Errorf("waiting after login: %w", safeNYSEGError(err))
	}

	// Navigate to insights page (don't wait for auto-redirect, navigate explicitly)
	fmt.Println("Navigating to insights page...")
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(nysegInsightsURL),
		chromedp.WaitVisible(`div.engage-insights-explore`, chromedp.ByQuery),
		chromedp.Sleep(2*time.Second),
	); err != nil {
		return nil, fmt.Errorf("navigating to insights page: %w", safeNYSEGError(err))
	}

	// Wait for page to load and API calls to complete (which captures auth tokens)
	fmt.Println("Waiting for page to fully load and capture auth tokens...")
	if err := chromedp.Run(browserCtx, chromedp.Sleep(5*time.Second)); err != nil {
		return nil, fmt.Errorf("waiting for page: %w", safeNYSEGError(err))
	}

	// Check if we captured the auth tokens
	tokenMu.Lock()
	upAuth := capturedUpAuth
	tokenMu.Unlock()

	if upAuth == "" {
		return nil, fmt.Errorf("could not capture Up-Authorization token from API requests")
	}

	// The existing UI flow exports the portal-selected range. Do not pretend
	// computing local dates changes the portal's export request.
	fmt.Printf("NYSEG CSV uses portal-selected range; configured lookback (%d days) is not applied by this exporter.\n", daysToFetch)

	// Set up listener to capture the download API response (promise_id)
	var capturedPromiseID string
	var downloadRequestID network.RequestID
	var downloadMu sync.Mutex
	promiseIDChan := make(chan string, 1) // Channel to signal when promise_id is captured

	// Add a new listener for the download response
	chromedp.ListenTarget(browserCtx, func(ev interface{}) {
		switch ev := ev.(type) {
		case *network.EventRequestWillBeSent:
			// Log headers for download requests
			if isNYSEGAPIURL(ev.Request.URL) && strings.Contains(ev.Request.URL, "/usage/usage/download") {
				downloadMu.Lock()
				downloadRequestID = ev.RequestID
				downloadMu.Unlock()
			}
		case *network.EventResponseReceived:
			// Look for the download API response
			if isNYSEGAPIURL(ev.Response.URL) && strings.Contains(ev.Response.URL, "/usage/usage/download") {
				fmt.Printf("  [NETWORK] Download API response: status=%d\n", ev.Response.Status)
			}
		case *network.EventLoadingFinished:
			// Check if this is the download request finishing
			downloadMu.Lock()
			reqID := downloadRequestID
			downloadMu.Unlock()
			if ev.RequestID == reqID && reqID != "" {
				// Get the response body in a goroutine
				go func() {
					var body []byte
					if err := chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
						var err error
						body, err = network.GetResponseBody(reqID).Do(ctx)
						return err
					})); err != nil {
						fmt.Println("  [NETWORK] Could not read export response")
						return
					}
					// Parse the promise_id from the response
					var resp struct {
						PromiseID string `json:"promise_id"`
					}
					if err := json.Unmarshal(body, &resp); err == nil && resp.PromiseID != "" {
						downloadMu.Lock()
						capturedPromiseID = resp.PromiseID
						downloadMu.Unlock()
						// Signal that we got the promise_id
						select {
						case promiseIDChan <- resp.PromiseID:
						default:
						}
					}
				}()
			}
		}
	})

	// Click on "Download my energy usage data" to open the modal
	fmt.Println("Opening download modal...")
	var modalResult string
	if err := chromedp.Run(browserCtx,
		chromedp.Evaluate(`
			(function() {
				// Find and click the download link/button
				const elements = document.querySelectorAll('*');
				for (const el of elements) {
					if (el.textContent && el.textContent.trim() === 'Download my energy usage data') {
						el.click();
						return 'CLICKED: ' + el.tagName + '.' + el.className;
					}
				}
				return 'NOT_FOUND';
			})()
		`, &modalResult),
		chromedp.Sleep(3*time.Second), // Wait for modal to open
	); err != nil {
		return nil, fmt.Errorf("opening download modal: %w", safeNYSEGError(err))
	}
	if modalResult == "NOT_FOUND" {
		return nil, fmt.Errorf("could not find 'Download my energy usage data' button")
	}

	// Wait for modal to actually appear by checking for "Select File Format" text
	var modalVisible bool
	for i := 0; i < 5; i++ {
		if err := chromedp.Run(browserCtx,
			chromedp.Evaluate(`document.body.innerText.includes('Select File Format')`, &modalVisible),
		); err != nil {
			return nil, fmt.Errorf("checking for modal: %w", safeNYSEGError(err))
		}
		if modalVisible {
			fmt.Println("Modal is visible")
			break
		}
		fmt.Printf("Waiting for modal to appear (attempt %d/5)...\n", i+1)
		if err := chromedp.Run(browserCtx, chromedp.Sleep(1*time.Second)); err != nil {
			return nil, fmt.Errorf("waiting for modal: %w", safeNYSEGError(err))
		}
	}
	if !modalVisible {
		return nil, fmt.Errorf("modal did not appear after clicking download button")
	}

	// Select CSV format and click download
	fmt.Println("Selecting CSV format and initiating download...")

	var csvResult string
	if err := chromedp.Run(browserCtx,
		// Click on CSV radio button with proper events
		chromedp.Evaluate(`
			(function() {
				// Helper to click with proper events
				function clickWithEvents(el) {
					el.focus();
					el.dispatchEvent(new MouseEvent('mousedown', {bubbles: true, cancelable: true, view: window}));
					el.dispatchEvent(new MouseEvent('mouseup', {bubbles: true, cancelable: true, view: window}));
					el.dispatchEvent(new MouseEvent('click', {bubbles: true, cancelable: true, view: window}));
					// Also dispatch change event for form elements
					el.dispatchEvent(new Event('change', {bubbles: true}));
				}

				// Approach 1: Find input[type=radio] with label containing "CSV"
				const inputRadios = document.querySelectorAll('input[type="radio"]');
				for (const radio of inputRadios) {
					// Check the associated label
					if (radio.labels && radio.labels.length > 0) {
						const labelText = radio.labels[0].innerText.trim();
						if (labelText === 'CSV') {
							clickWithEvents(radio);
							// Also click the label for good measure
							clickWithEvents(radio.labels[0]);
							return 'clicked input[type=radio] with label "CSV" (checked=' + radio.checked + ')';
						}
					}
					// Also check parent element text
					const parent = radio.parentElement;
					if (parent) {
						const parentText = parent.innerText.trim();
						if (parentText === 'CSV') {
							clickWithEvents(radio);
							return 'clicked input[type=radio] with parent text "CSV" (checked=' + radio.checked + ')';
						}
					}
				}

				// Approach 2: Find role="radio" elements containing "CSV"
				const roleRadios = document.querySelectorAll('[role="radio"]');
				for (const radio of roleRadios) {
					const text = (radio.innerText || radio.textContent || '').trim();
					if (text.includes('CSV') && !text.includes('XML')) {
						clickWithEvents(radio);
						return 'clicked [role=radio] containing CSV: ' + text;
					}
				}

				// Approach 3: Find label element containing "CSV" and click it
				const labels = document.querySelectorAll('label');
				for (const label of labels) {
					if (label.innerText.trim() === 'CSV') {
						clickWithEvents(label);
						return 'clicked label with text "CSV"';
					}
				}

				return 'CSV not found - no matching elements';
			})()
		`, &csvResult),
		chromedp.Sleep(1*time.Second),
	); err != nil {
		return nil, fmt.Errorf("selecting CSV format: %w", safeNYSEGError(err))
	}

	// Click the Download file button
	fmt.Println("Clicking Download file button...")

	// Intercept fetch/XMLHttpRequest to capture the download response
	if err := chromedp.Run(browserCtx,
		chromedp.Evaluate(`
			// Intercept the download API response
			window.__downloadPromiseID = null;
			window.__downloadError = null;

			// Hook into fetch
			const originalFetch = window.fetch;
			window.fetch = async function(...args) {
				const response = await originalFetch.apply(this, args);
				const url = args[0];
				if (url && url.toString().includes('usage/usage/download')) {
					const clonedResponse = response.clone();
					try {
						const json = await clonedResponse.json();
						if (json.promise_id) {
							window.__downloadPromiseID = json.promise_id;
						}
					} catch (e) {
					}
				}
				return response;
			};
			true
		`, nil),
	); err != nil {
		return nil, fmt.Errorf("setting up fetch interceptor: %w", safeNYSEGError(err))
	}

	// Now click the download button using native chromedp click (sends real mouse events)

	// First, find the button and get its selector
	var buttonSelector string
	if err := chromedp.Run(browserCtx,
		chromedp.Evaluate(`
			(function() {
				// Find "Download file" button and return a unique selector
				const buttons = document.querySelectorAll('button, span, div');
				for (const el of buttons) {
					const text = el.textContent.trim().toLowerCase();
					if (text === 'download file') {
						// Try to build a unique selector
						if (el.id) return '#' + el.id;
						if (el.className) {
							const classes = el.className.split(' ').filter(c => c).join('.');
							return el.tagName.toLowerCase() + '.' + classes;
						}
						return el.tagName.toLowerCase() + ':contains("Download file")';
					}
				}
				return '';
			})()
		`, &buttonSelector),
	); err != nil {
		return nil, fmt.Errorf("finding download button: %w", safeNYSEGError(err))
	}

	// Use chromedp's native click with the selector, or fall back to JavaScript
	if buttonSelector != "" && !strings.Contains(buttonSelector, ":contains") {
		if err := chromedp.Run(browserCtx,
			chromedp.Click(buttonSelector, chromedp.ByQuery),
			chromedp.Sleep(2*time.Second),
		); err != nil {
			fmt.Println("Native click failed; trying JavaScript click")
			// Fall back to JavaScript click
			if err := chromedp.Run(browserCtx,
				chromedp.Evaluate(`
					(function() {
						const buttons = document.querySelectorAll('button, span, div');
						for (const el of buttons) {
							if (el.textContent.trim().toLowerCase() === 'download file') {
								// Dispatch proper mouse events
								el.dispatchEvent(new MouseEvent('mousedown', {bubbles: true, cancelable: true, view: window}));
								el.dispatchEvent(new MouseEvent('mouseup', {bubbles: true, cancelable: true, view: window}));
								el.dispatchEvent(new MouseEvent('click', {bubbles: true, cancelable: true, view: window}));
								return 'JS click with events: ' + el.tagName;
							}
						}
						return 'NOT_FOUND';
					})()
				`, nil),
				chromedp.Sleep(2*time.Second),
			); err != nil {
				return nil, fmt.Errorf("clicking download button: %w", safeNYSEGError(err))
			}
		}
		fmt.Println("Download button clicked via native chromedp")
	} else {
		// JavaScript click with proper events
		var downloadBtnResult string
		if err := chromedp.Run(browserCtx,
			chromedp.Evaluate(`
				(function() {
					const buttons = document.querySelectorAll('button, span, div');
					for (const el of buttons) {
						if (el.textContent.trim().toLowerCase() === 'download file') {
							// Dispatch proper mouse events to simulate real click
							el.dispatchEvent(new MouseEvent('mousedown', {bubbles: true, cancelable: true, view: window}));
							el.dispatchEvent(new MouseEvent('mouseup', {bubbles: true, cancelable: true, view: window}));
							el.dispatchEvent(new MouseEvent('click', {bubbles: true, cancelable: true, view: window}));
							return 'CLICKED with events: ' + el.tagName + '.' + el.className;
						}
					}
					return 'NOT_FOUND';
				})()
			`, &downloadBtnResult),
			chromedp.Sleep(2*time.Second),
		); err != nil {
			return nil, fmt.Errorf("clicking download button: %w", safeNYSEGError(err))
		}
		if downloadBtnResult == "NOT_FOUND" {
			return nil, fmt.Errorf("could not find 'Download file' button in modal")
		}
	}

	// Wait for promise_id from network listener (with timeout)
	fmt.Println("Waiting for download API response...")
	var promiseID string

	// Wait up to 15 seconds for the promise_id via channel
	select {
	case promiseID = <-promiseIDChan:
	case <-browserCtx.Done():
		return nil, browserCtx.Err()
	case <-time.After(15 * time.Second):
		fmt.Println("Timeout waiting for promise_id from network listener, trying fallbacks...")
	}

	// Fallback 1: Check the mutex-protected variable (in case goroutine completed but channel missed)
	if promiseID == "" {
		downloadMu.Lock()
		promiseID = capturedPromiseID
		downloadMu.Unlock()
	}

	// Fallback 2: Try fetch interceptor
	if promiseID == "" {
		var fetchPromiseID string
		if err := chromedp.Run(browserCtx,
			chromedp.Evaluate(`window.__downloadPromiseID`, &fetchPromiseID),
		); err == nil && fetchPromiseID != "" && fetchPromiseID != "null" {
			promiseID = fetchPromiseID
		}
	}

	if promiseID == "" || promiseID == "null" {
		// Print diagnostic info
		fmt.Println("ERROR: Could not capture promise_id. Diagnostic info:")
		fmt.Println("  - Check if download API request appeared in [NETWORK] logs above")
		return nil, fmt.Errorf("could not capture promise_id from download request (check if CSV was selected)")
	}

	// Create HTTP client for polling
	client := &http.Client{Timeout: 60 * time.Second}

	// Build headers for polling using captured auth token
	headers := http.Header{}
	headers.Set("Accept", "*/*")
	headers.Set("Accept-Language", "en-US,en;q=0.9")
	headers.Set("Content-Type", "application/json")
	headers.Set("Origin", "https://energymanager.nyseg.com")
	headers.Set("Referer", "https://energymanager.nyseg.com/")
	headers.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	headers.Set("Up-Authorization", upAuth)

	// Poll for CSV
	data, err := s.pollForCSV(browserCtx, promiseID, client, headers)
	if err != nil {
		return nil, fmt.Errorf("polling for CSV: %w", err)
	}

	fmt.Printf("✓ Parsed %d hourly usage records\n", len(data))

	// Extract and save fresh cookies for future use
	freshCookies, err := ExtractCookies(browserCtx)
	if err == nil && len(freshCookies) > 0 {
		s.cookies = freshCookies
	}

	return data, nil
}

// pollForCSV polls the export job, not the v2 billing-summary API. Partial
// results are never imported. Polling does not resubmit login credentials.
func (s *NYSEGDirectScraper) pollForCSV(ctx context.Context, promiseID string, client *http.Client, headers http.Header) ([]models.UsageData, error) {
	if !promiseIDPattern.MatchString(promiseID) {
		return nil, fmt.Errorf("invalid export job identifier")
	}
	pollURL := "https://" + nysegAPIHost + "/promix/" + promiseID
	// Custom authentication headers must never be forwarded through redirects.
	safeClient := *client
	safeClient.Jar = nil
	safeClient.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	for attempt := 0; attempt < 30; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		fmt.Printf("Polling CSV export attempt %d/30...\n", attempt+1)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
		if err != nil {
			return nil, fmt.Errorf("creating export request: %w", safeNYSEGError(err))
		}
		req.Header = headers.Clone()
		resp, err := safeClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("export request: %w", safeNYSEGError(err))
		}
		body, readErr := readNYSEGBody(resp.Body, 64<<20)
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, &AuthError{StatusCode: resp.StatusCode, Message: "NYSEG export authentication failed; login was not retried"}
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
			return nil, fmt.Errorf("export endpoint returned HTTP %d", resp.StatusCode)
		}
		if readErr != nil {
			return nil, readErr
		}
		contentType := strings.ToLower(resp.Header.Get("Content-Type"))
		if resp.StatusCode == http.StatusOK && (strings.Contains(contentType, "csv") || strings.Contains(contentType, "text/plain")) {
			return parseNYSEGCSVReader(strings.NewReader(string(body)))
		}
		var result struct {
			Code string `json:"code"`
			URL  string `json:"promise_url"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("invalid export status response (body suppressed)")
		}
		if strings.Contains(result.Code, "ERROR") || strings.Contains(result.Code, "FAILED") {
			return nil, fmt.Errorf("NYSEG CSV generation failed (remote details suppressed)")
		}
		if result.Code != "PROMISE_FOUND" {
			continue
		} // Includes partial results: keep waiting.
		if err := validateNYSEGDownloadURL(result.URL); err != nil {
			return nil, err
		}
		csvReq, err := http.NewRequestWithContext(ctx, http.MethodGet, result.URL, nil)
		if err != nil {
			return nil, fmt.Errorf("creating CSV request: %w", safeNYSEGError(err))
		}
		// No NYSEG auth/cookie headers on signed object-storage URLs.
		csvResp, err := safeClient.Do(csvReq)
		if err != nil {
			return nil, fmt.Errorf("downloading CSV: %w", safeNYSEGError(err))
		}
		csvBody, readErr := readNYSEGBody(csvResp.Body, 64<<20)
		csvResp.Body.Close()
		if csvResp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("CSV download returned HTTP %d", csvResp.StatusCode)
		}
		if readErr != nil {
			return nil, readErr
		}
		return parseNYSEGCSVReader(strings.NewReader(string(csvBody)))
	}
	return nil, fmt.Errorf("CSV generation timed out without a complete export after 30 attempts")
}
