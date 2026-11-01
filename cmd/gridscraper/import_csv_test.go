package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jgoulah/gridscraper/internal/config"
	"github.com/jgoulah/gridscraper/internal/database"
	"github.com/jgoulah/gridscraper/internal/scraper"
	"github.com/jgoulah/gridscraper/pkg/models"
	"github.com/spf13/cobra"
)

const normalizedFixture = "start_time,end_time,kwh\n2026-11-01T01:00:00-04:00,2026-11-01T01:00:00-05:00,0\n2026-11-01T01:00:00-05:00,2026-11-01T02:00:00-05:00,1.5\n"

func setupCLI(t *testing.T) (string, *cobra.Command, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	oldDB, oldCfg := dbPath, cfgFile
	dbPath = filepath.Join(dir, "test.db")
	cfgFile = filepath.Join(dir, "nonexistent-config.json")
	t.Cleanup(func() { dbPath = oldDB; cfgFile = oldCfg })
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	return dir, cmd, &out
}
func TestImportCLIAtomicAndIdempotent(t *testing.T) {
	dir, cmd, out := setupCLI(t)
	file := filepath.Join(dir, "normalized.csv")
	if err := os.WriteFile(file, []byte(normalizedFixture), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runImportCSV(cmd, []string{"nyseg", file}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "inserted 2, unchanged 0") {
		t.Fatal(out.String())
	}
	out.Reset()
	if err := runImportCSV(cmd, []string{"nyseg", file}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "inserted 0, unchanged 2") {
		t.Fatal(out.String())
	}
	// A late invalid row must leave an existing DB byte-for-byte unchanged.
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(normalizedFixture+"invalid,invalid,-1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runImportCSV(cmd, []string{"nyseg", file}); err == nil {
		t.Fatal("accepted invalid file")
	}
	after, err := os.ReadFile(dbPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("DB changed on validation failure")
	}
	db, err := database.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.ListUsage("nyseg")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows: %+v %v", rows, err)
	}
	if _, err := os.Stat(cfgFile); !os.IsNotExist(err) {
		t.Fatal("import touched config")
	}
}
func TestInvalidImportDoesNotCreateDatabase(t *testing.T) {
	dir, cmd, _ := setupCLI(t)
	file := filepath.Join(dir, "invalid.csv")
	if err := os.WriteFile(file, []byte("start_time,end_time,kwh\ninvalid,invalid,1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runImportCSV(cmd, []string{"nyseg", file}); err == nil {
		t.Fatal("accepted invalid file")
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatal("invalid import created database")
	}
	if err := runImportCSV(cmd, []string{"coned", file}); err == nil {
		t.Fatal("accepted unsupported service")
	}
}
func TestFetchNYSEGDoesNotRetryOrRewriteConfig(t *testing.T) {
	_, cmd, _ := setupCLI(t)
	// Synthetic JSON config, never real user credentials.
	const contents = `{"cookies":{"nyseg_username":"fixture-user","nyseg_password":"fixture-password"},"nyseg_days_to_fetch":2}`
	if err := os.WriteFile(cfgFile, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	old := fetchNYSEG
	t.Cleanup(func() { fetchNYSEG = old })
	calls := 0
	fetchNYSEG = func(context.Context, []config.Cookie, string, string, string, bool, int) ([]models.UsageData, error) {
		calls++
		return nil, errors.New("synthetic scrape failure")
	}
	if err := runFetch(cmd, []string{"nyseg"}); err == nil {
		t.Fatal("expected scrape error")
	}
	if calls != 1 {
		t.Fatalf("credential submissions retried: %d", calls)
	}
	got, err := os.ReadFile(cfgFile)
	if err != nil || string(got) != contents {
		t.Fatalf("config rewritten: %v", err)
	}
}
func TestFetchNYSEGSuccessPreservesJSON(t *testing.T) {
	_, cmd, _ := setupCLI(t)
	const contents = `{"cookies":{"nyseg_username":"fixture-user","nyseg_password":"fixture-password"}}`
	if err := os.WriteFile(cfgFile, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	old := fetchNYSEG
	t.Cleanup(func() { fetchNYSEG = old })
	calls := 0
	fetchNYSEG = func(context.Context, []config.Cookie, string, string, string, bool, int) ([]models.UsageData, error) {
		calls++
		return scraper.ParseNormalizedNYSEGCSV(strings.NewReader(normalizedFixture))
	}
	if err := runFetch(cmd, []string{"nyseg"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(cfgFile)
	if err != nil || string(got) != contents || calls != 1 {
		t.Fatalf("success path changed JSON or retried: calls=%d err=%v", calls, err)
	}
}

func TestRawNYSEGDiagnosticsDisabled(t *testing.T) {
	_, cmd, _ := setupCLI(t)
	for _, run := range []func(*cobra.Command, []string) error{runDebug, runCapture, runInspect} {
		err := run(cmd, []string{"nyseg"})
		if err == nil || !strings.Contains(err.Error(), "disabled") {
			t.Fatalf("unsafe diagnostic available: %v", err)
		}
	}
}
