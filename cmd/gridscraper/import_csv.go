package main

import (
	"fmt"
	"os"

	"github.com/jgoulah/gridscraper/internal/scraper"
	"github.com/spf13/cobra"
)

var importCSVCmd = &cobra.Command{
	Use:   "import-csv nyseg <file>",
	Short: "Atomically import normalized NYSEG interval CSV (RFC3339 timestamps, kWh)",
	Long: `Import exactly start_time,end_time,kwh columns. Timestamps require explicit
RFC3339 offsets. kwh must be a finite, nonnegative plain decimal. No authentication,
network requests, config loading, or Home Assistant writes are performed. The full
file is validated before the database is opened; conflicts roll back the batch.`,
	Args: cobra.ExactArgs(2),
	RunE: runImportCSV,
}

func init() { rootCmd.AddCommand(importCSVCmd) }

func runImportCSV(cmd *cobra.Command, args []string) error {
	if args[0] != "nyseg" {
		return fmt.Errorf("import-csv supports only nyseg")
	}
	f, err := os.Open(args[1])
	if err != nil {
		return fmt.Errorf("opening CSV: %w", err)
	}
	data, parseErr := scraper.ParseNormalizedNYSEGCSV(f)
	closeErr := f.Close()
	if parseErr != nil {
		return fmt.Errorf("validating CSV (database untouched): %w", parseErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing CSV: %w", closeErr)
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	inserted, err := db.InsertUsageBatch(data)
	if err != nil {
		return fmt.Errorf("import rolled back: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Validated %d records; inserted %d, unchanged %d\n", len(data), inserted, len(data)-inserted)
	return nil
}
