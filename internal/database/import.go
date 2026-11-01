package database

import (
	"fmt"
	"time"

	"github.com/jgoulah/gridscraper/pkg/models"
)

func parseStoredTime(s, service string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err == nil {
		return t.UTC(), nil
	}
	if service == "nyseg" {
		return time.Time{}, fmt.Errorf("legacy NYSEG timestamp has no reliable offset; use a fresh database and reimport offset-qualified source (no automatic migration)")
	}
	// Retain compatibility for old Con Edison records; never guess NYSEG offsets.
	return time.ParseInLocation("2006-01-02 15:04:05", s, time.Local)
}

// InsertUsageBatch atomically inserts validated NYSEG intervals. Exact existing
// readings are idempotent; corrected values/endpoints and overlaps are errors,
// not silently ignored changes. No implicit migration of legacy wall times.
func (db *DB) InsertUsageBatch(data []models.UsageData) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	for i, d := range data {
		if d.Service != "nyseg" || d.Date.IsZero() {
			return 0, fmt.Errorf("record %d: NYSEG service and date required", i+1)
		}
	}
	if err := models.ValidateIntervals(data); err != nil {
		return 0, err
	}
	tx, err := db.conn.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT start_time,end_time,kwh FROM usage_data WHERE service = 'nyseg'`)
	if err != nil {
		return 0, err
	}
	existing := map[string]models.UsageData{}
	var combined []models.UsageData
	for rows.Next() {
		var start, end string
		var kwh float64
		if err := rows.Scan(&start, &end, &kwh); err != nil {
			rows.Close()
			return 0, fmt.Errorf("legacy or invalid NYSEG record; use a fresh database and reimport offset-qualified source")
		}
		a, ea := parseStoredTime(start, "nyseg")
		b, eb := parseStoredTime(end, "nyseg")
		if ea != nil || eb != nil || start != a.UTC().Format(time.RFC3339Nano) || end != b.UTC().Format(time.RFC3339Nano) {
			rows.Close()
			return 0, fmt.Errorf("legacy/noncanonical NYSEG timestamps; use a fresh database and reimport offset-qualified source (no automatic migration)")
		}
		d := models.UsageData{StartTime: a, EndTime: b, KWh: kwh, Service: "nyseg"}
		existing[start] = d
		combined = append(combined, d)
	}
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil {
		return 0, rowErr
	}
	var pending []models.UsageData
	for _, d := range data {
		key := d.StartTime.UTC().Format(time.RFC3339Nano)
		if prev, ok := existing[key]; ok {
			if !prev.EndTime.Equal(d.EndTime) || prev.KWh != d.KWh {
				return 0, fmt.Errorf("conflicting existing NYSEG interval; batch unchanged")
			}
			continue
		}
		pending = append(pending, d)
		combined = append(combined, d)
	}
	if err := models.ValidateIntervals(combined); err != nil {
		return 0, fmt.Errorf("incoming/existing NYSEG intervals: %w", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO usage_data (date,start_time,end_time,kwh,service,created_at,published) VALUES (?,?,?,?,?,?,0)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for _, d := range pending {
		_, err = stmt.Exec(d.Date.Format("2006-01-02"), d.StartTime.UTC().Format(time.RFC3339Nano), d.EndTime.UTC().Format(time.RFC3339Nano), d.KWh, "nyseg", time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return 0, fmt.Errorf("inserting NYSEG batch: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(pending), nil
}
