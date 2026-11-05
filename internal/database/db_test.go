package database

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jgoulah/gridscraper/pkg/models"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	db, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
func reading(start string, kwh float64) models.UsageData {
	s, err := time.Parse(time.RFC3339, start)
	if err != nil {
		panic(err)
	}
	return models.UsageData{Date: s, StartTime: s, EndTime: s.Add(time.Hour), KWh: kwh, Service: "nyseg"}
}

func TestUTCStorageDedupe(t *testing.T) {
	db := testDB(t)
	a := reading("2026-11-01T01:00:00-04:00", 0)
	b := reading("2026-11-01T01:00:00-05:00", 2)
	n, err := db.InsertUsageBatch([]models.UsageData{a, b})
	if err != nil || n != 2 {
		t.Fatalf("insert: %d %v", n, err)
	}
	a.StartTime = a.StartTime.UTC()
	a.EndTime = a.EndTime.UTC()
	n, err = db.InsertUsageBatch([]models.UsageData{a})
	if err != nil || n != 0 {
		t.Fatalf("dedupe: %d %v", n, err)
	}
	rows, err := db.ListUsage("nyseg")
	if err != nil || len(rows) != 2 {
		t.Fatalf("list: %+v %v", rows, err)
	}
	for _, r := range rows {
		if r.StartTime.Location() != time.UTC {
			t.Fatalf("not UTC: %v", r.StartTime)
		}
	}
	var start string
	if err := db.conn.QueryRow("SELECT start_time FROM usage_data WHERE kwh=0").Scan(&start); err != nil || start != "2026-11-01T05:00:00Z" {
		t.Fatalf("stored: %s %v", start, err)
	}
	unpublished, err := db.ListUnpublishedUsage("nyseg")
	if err != nil || len(unpublished) != 2 {
		t.Fatal(err)
	}
	got, err := db.GetUsage(a.Date, "nyseg")
	if err != nil || got == nil || got.StartTime.IsZero() {
		t.Fatal(err)
	}
}

func TestBatchAtomicValidationAndConflict(t *testing.T) {
	db := testDB(t)
	a := reading("2026-01-01T00:00:00Z", 1)
	if err := db.InsertUsage(&a); err != nil {
		t.Fatal(err)
	}
	b := reading("2026-01-01T01:00:00Z", 2)
	conflict := a
	conflict.KWh = 3
	if _, err := db.InsertUsageBatch([]models.UsageData{b, conflict}); err == nil {
		t.Fatal("accepted conflict")
	}
	bad := b
	bad.KWh = math.NaN()
	if _, err := db.InsertUsageBatch([]models.UsageData{b, bad}); err == nil {
		t.Fatal("accepted NaN")
	}
	rows, err := db.ListUsage("nyseg")
	if err != nil || len(rows) != 1 {
		t.Fatalf("partial insert: %+v %v", rows, err)
	}
}

func TestBatchRollbackOnSQLFailure(t *testing.T) {
	db := testDB(t)
	if _, err := db.conn.Exec(`CREATE TRIGGER reject_fixture BEFORE INSERT ON usage_data WHEN NEW.kwh=99 BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	a := reading("2026-01-01T00:00:00Z", 1)
	b := reading("2026-01-01T01:00:00Z", 99)
	if _, err := db.InsertUsageBatch([]models.UsageData{a, b}); err == nil {
		t.Fatal("expected SQL failure")
	}
	rows, err := db.ListUsage("nyseg")
	if err != nil || len(rows) != 0 {
		t.Fatalf("SQL failure partially committed: %+v %v", rows, err)
	}
}

func TestExistingOverlapAndSubsecondDedupe(t *testing.T) {
	db := testDB(t)
	a := reading("2026-01-01T00:00:00Z", 1)
	a.StartTime = a.StartTime.Add(123456789)
	a.EndTime = a.EndTime.Add(123456789)
	if err := db.InsertUsage(&a); err != nil {
		t.Fatal(err)
	}
	if n, err := db.InsertUsageBatch([]models.UsageData{a}); err != nil || n != 0 {
		t.Fatalf("subsecond dedupe: %d %v", n, err)
	}
	b := reading("2026-01-01T00:30:00Z", 1)
	if err := db.InsertUsage(&b); err == nil {
		t.Fatal("accepted overlap with existing DB interval")
	}
}

func TestLegacyNYSEGRefused(t *testing.T) {
	db := testDB(t)
	_, err := db.conn.Exec(`INSERT INTO usage_data(date,start_time,end_time,kwh,service,created_at) VALUES ('2026-11-01','2026-11-01 01:00:00','2026-11-01 02:00:00',1,'nyseg','2026-11-01')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ListUsage("nyseg")
	if err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("legacy read: %v", err)
	}
	a := reading("2026-11-01T01:00:00-04:00", 1)
	if err := db.InsertUsage(&a); err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("legacy write: %v", err)
	}
}
