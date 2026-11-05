package scraper

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
	"github.com/jgoulah/gridscraper/internal/config"
	"github.com/jgoulah/gridscraper/pkg/models"
)

const nysegInsightsURL = "https://energymanager.nyseg.com/insights"

// NYSEGScraper scrapes data from NYSEG
type NYSEGScraper struct {
	cookies []config.Cookie
	visible bool
}

// NewNYSEGScraper creates a new NYSEG scraper
func NewNYSEGScraper(cookies []config.Cookie, visible bool) *NYSEGScraper {
	return &NYSEGScraper{
		cookies: cookies,
		visible: visible,
	}
}

// Scrape fetches usage data from NYSEG by downloading CSV
func (s *NYSEGScraper) Scrape(ctx context.Context, daysToFetch int) ([]models.UsageData, error) {
	// Create temp directory for downloads
	downloadDir, err := os.MkdirTemp("", "gridscraper-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir: %w", safeNYSEGError(err))
	}
	defer os.RemoveAll(downloadDir)

	// Create browser context with download directory
	opts := NYSEGBrowserOptions(s.visible)

	allocCtx, cancel := chromedp.NewExecAllocator(ctx, opts...)
	defer cancel()

	browserCtx, cancel := chromedp.NewContext(allocCtx, chromedp.WithErrorf(func(string, ...interface{}) { fmt.Println("NYSEG browser protocol error (details suppressed)") }))
	defer cancel()

	// Set timeout
	browserCtx, cancel = context.WithTimeout(browserCtx, 3*time.Minute)
	defer cancel()

	// Set download behavior
	if err := chromedp.Run(browserCtx,
		browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorAllow).
			WithDownloadPath(downloadDir).
			WithEventsEnabled(true),
	); err != nil {
		return nil, fmt.Errorf("setting download behavior: %w", safeNYSEGError(err))
	}

	// Set cookies and navigate
	if err := SetCookies(browserCtx, s.cookies); err != nil {
		return nil, fmt.Errorf("setting cookies: %w", safeNYSEGError(err))
	}

	// Navigate to insights page
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(nysegInsightsURL),
		chromedp.WaitVisible(`div.engage-insights-explore`, chromedp.ByQuery),
	); err != nil {
		return nil, fmt.Errorf("navigating to insights page: %w", safeNYSEGError(err))
	}

	// Click the download button
	fmt.Println("Clicking download link...")
	if err := chromedp.Run(browserCtx,
		chromedp.Click(`div#engage-insights-explore__download-button`, chromedp.ByQuery),
		chromedp.Sleep(2*time.Second),
	); err != nil {
		return nil, fmt.Errorf("clicking download link: %w", safeNYSEGError(err))
	}

	// Wait for popup/modal and select options
	if err := chromedp.Run(browserCtx,
		chromedp.WaitVisible(`input#bill-period`, chromedp.ByQuery),
		chromedp.Click(`input#bill-period`, chromedp.ByQuery),
		chromedp.Sleep(500*time.Millisecond),
		chromedp.Click(`input#csv`, chromedp.ByQuery),
		chromedp.Sleep(500*time.Millisecond),
	); err != nil {
		return nil, fmt.Errorf("selecting download options: %w", safeNYSEGError(err))
	}

	// Find and click the download button using JavaScript
	fmt.Println("Looking for download button...")
	var buttonFound bool
	if err := chromedp.Run(browserCtx,
		chromedp.Evaluate(`
			(function() {
				// Look for buttons with download/export text
				const buttons = document.querySelectorAll('button, input[type="submit"], a.button');
				for (let btn of buttons) {
					const text = btn.textContent.toLowerCase();
					if (text.includes('download') || text.includes('export') || btn.type === 'submit') {
						btn.click();
						return true;
					}
				}
				return false;
			})()
		`, &buttonFound),
	); err != nil {
		return nil, fmt.Errorf("searching for download button: %w", safeNYSEGError(err))
	}

	if !buttonFound {
		return nil, fmt.Errorf("could not find download button")
	}

	fmt.Println("Download button clicked, waiting for file...")

	// Wait for download to complete
	time.Sleep(5 * time.Second)

	// Find the downloaded CSV file
	files, err := os.ReadDir(downloadDir)
	if err != nil {
		return nil, fmt.Errorf("reading download directory: %w", safeNYSEGError(err))
	}

	var csvPath string
	for _, file := range files {
		if strings.HasSuffix(file.Name(), ".csv") {
			csvPath = filepath.Join(downloadDir, file.Name())
			break
		}
	}

	if csvPath == "" {
		return nil, fmt.Errorf("no CSV file downloaded")
	}

	// Parse the CSV
	data, err := parseNYSEGCSV(csvPath)
	if err != nil {
		return nil, fmt.Errorf("parsing CSV: %w", err)
	}

	return data, nil
}

// parseNYSEGCSV shares the validated interval parser used by the active CSV path.
func parseNYSEGCSV(path string) ([]models.UsageData, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening CSV: %w", safeNYSEGError(err))
	}
	defer file.Close()
	return parseNYSEGCSVReader(file)
}
