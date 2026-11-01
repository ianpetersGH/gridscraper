package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jgoulah/gridscraper/internal/config"
	"github.com/jgoulah/gridscraper/internal/scraper"
	"github.com/jgoulah/gridscraper/pkg/models"
	"github.com/spf13/cobra"
)

var fetchVisible bool

var fetchNYSEG = func(ctx context.Context, cookies []config.Cookie, token, username, password string, visible bool, days int) ([]models.UsageData, error) {
	s := scraper.NewNYSEGDirectScraperWithCredentials(cookies, token, username, password)
	s.SetVisible(visible)
	return s.Scrape(ctx, days)
}

var fetchCmd = &cobra.Command{
	Use:   "fetch [service]",
	Short: "Fetch usage data from a service",
	Long: `Scrapes electrical usage data from the specified service using saved cookies.
Data will be stored in the local SQLite database.

Available services: nyseg, coned`,
	Args: cobra.ExactArgs(1),
	RunE: runFetch,
}

func init() {
	fetchCmd.Flags().BoolVar(&fetchVisible, "visible", false, "Show browser window (for debugging)")
	rootCmd.AddCommand(fetchCmd)
}

func runFetch(cmd *cobra.Command, args []string) error {
	fmt.Printf("=== Fetch started at %s ===\n", time.Now().Format("2006-01-02 15:04:05 MST"))

	service := args[0]

	// Validate service
	if service != "nyseg" && service != "coned" {
		return fmt.Errorf("unknown service: %s (available: nyseg, coned)", service)
	}

	// Load config
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Get cookies, auth token, and credentials for service
	var cookies []config.Cookie
	var authToken, customerUUID, username, password, challengeAnswer string
	switch service {
	case "nyseg":
		cookies = cfg.Cookies.NYSEG
		authToken = cfg.Cookies.NYSEGAuthToken
		username = cfg.Cookies.NYSEGUsername
		password = cfg.Cookies.NYSEGPassword
	case "coned":
		cookies = cfg.Cookies.ConEd
		authToken = cfg.Cookies.ConEdAuthToken
		customerUUID = cfg.Cookies.ConEdCustomerUUID
		username = cfg.Cookies.ConEdUsername
		password = cfg.Cookies.ConEdPassword
		challengeAnswer = cfg.Cookies.ConEdChallengeAnswer
	}

	if service == "nyseg" && (username == "" || password == "") {
		return fmt.Errorf("NYSEG browser CSV fetch requires configured username/password; cookies alone are not supported; use import-csv for offline history")
	}
	// Check if we have either cookies+token OR username+password for auto-auth
	if len(cookies) == 0 && (username == "" || password == "") {
		return fmt.Errorf("no authentication configured for %s. Add username/password to config.yaml or run 'gridscraper login %s'", service, service)
	}

	// Open database
	db, err := openDB()
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer db.Close()

	// Create scraper based on service
	ctx := cmd.Context()
	var data []models.UsageData

	// Get service-specific days to fetch
	var daysToFetch int
	if service == "nyseg" {
		daysToFetch = cfg.GetNYSEGDaysToFetch()
	} else {
		daysToFetch = cfg.GetConEdDaysToFetch()
	}

	if service == "nyseg" {
		// This is browser-authenticated CSV export, not the v2 hourly API.
		// Scrape logs in once. Never retry credential submissions or rewrite config.
		fmt.Printf("Fetching NYSEG browser CSV (configured lookback %d days; portal range may differ)...\n", daysToFetch)
		data, err = fetchNYSEG(ctx, cookies, authToken, username, password, fetchVisible, daysToFetch)
		if err != nil {
			return fmt.Errorf("NYSEG scrape failed (not retried): %w", err)
		}
	} else if service == "coned" {
		// Con Edison scraper with auth token, customer UUID, and challenge answer
		conedScraper := scraper.NewConEdScraperWithCredentials(cookies, authToken, customerUUID, username, password, challengeAnswer)
		conedScraper.SetVisible(fetchVisible)

		// If we have username/password but no cookies, do proactive login
		if len(cookies) == 0 && username != "" && password != "" {
			fmt.Println("No cookies found, performing initial login...")
			freshCookies, freshToken, freshUUID, err := conedScraper.RefreshAuth(ctx)
			if err != nil {
				return fmt.Errorf("initial login failed: %w", err)
			}
			cfg.Cookies.ConEd = freshCookies
			cfg.Cookies.ConEdAuthToken = freshToken
			cfg.Cookies.ConEdCustomerUUID = freshUUID
			if err := saveConfig(cfg); err != nil {
				fmt.Printf("Warning: Could not save credentials: %v\n", err)
			} else {
				fmt.Println("✓ Login successful, credentials saved")
			}
		}

		// Scrape data with automatic auth refresh on failure
		fmt.Printf("Fetching data from %s (last %d days)...\n", service, daysToFetch)
		var err error
		data, err = conedScraper.Scrape(ctx, daysToFetch)

		// If scraping failed, retry once with existing tokens (they should still be valid)
		if err != nil {
			fmt.Printf("⚠ Scraping failed: %v\n", err)
			fmt.Printf("⚠ Retrying...\n")

			data, err = conedScraper.Scrape(ctx, daysToFetch)

			if err != nil {
				return fmt.Errorf("scraping failed after retry: %w", err)
			}
		}
	}

	if len(data) == 0 {
		fmt.Println("No data found")
		return nil
	}

	if service == "nyseg" {
		for i := range data {
			data[i].Service = service
		}
		inserted, err := db.InsertUsageBatch(data)
		if err != nil {
			return fmt.Errorf("storing NYSEG batch: %w", err)
		}
		fmt.Printf("Validated %d records; inserted %d, unchanged %d\n", len(data), inserted, len(data)-inserted)
		return nil
	}

	// Store data (duplicates will be ignored by UNIQUE constraint)
	totalRecords := 0

	for _, record := range data {
		// Set service name
		record.Service = service

		// Insert new data (INSERT OR IGNORE will skip duplicates based on UNIQUE constraint)
		if err := db.InsertUsage(&record); err != nil {
			return fmt.Errorf("inserting usage data: %w", err)
		}

		totalRecords++
	}

	fmt.Printf("✓ Processed %d records (duplicates automatically skipped by database)\n", totalRecords)
	return nil
}
