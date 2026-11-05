package scraper

import (
	"io"
	"os"
	"strings"

	"github.com/jgoulah/gridscraper/pkg/models"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizedCSV(t *testing.T) {
	csv := "start_time,end_time,kwh\n2026-11-01T01:00:00-04:00,2026-11-01T01:00:00-05:00,0\n2026-11-01T01:00:00-05:00,2026-11-01T02:00:00-05:00,1.25\n"
	rows, err := ParseNormalizedNYSEGCSV(strings.NewReader(csv))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].KWh != 0 || rows[1].KWh != 1.25 || rows[1].StartTime.Sub(rows[0].StartTime) != time.Hour {
		t.Fatalf("bad readings: %+v", rows)
	}
	if rows[0].Date.Format("2006-01-02") != "2026-11-01" || rows[0].Service != "nyseg" {
		t.Fatalf("bad metadata: %+v", rows[0])
	}
}

func TestNormalizedCSVRejectsInvalid(t *testing.T) {
	header := "start_time,end_time,kwh\n"
	valid := "2026-01-01T00:00:00-05:00,2026-01-01T01:00:00-05:00,0\n"
	for name, body := range map[string]string{
		"nan":           strings.ReplaceAll(valid, ",0\n", ",NaN\n"),
		"inf":           strings.ReplaceAll(valid, ",0\n", ",Inf\n"),
		"negative":      strings.ReplaceAll(valid, ",0\n", ",-1\n"),
		"negative zero": strings.ReplaceAll(valid, ",0\n", ",-0\n"),
		"units":         strings.ReplaceAll(valid, ",0\n", ",1 Wh\n"),
		"suffix":        strings.ReplaceAll(valid, ",0\n", ",1 kWh\n"),
		"empty value":   strings.ReplaceAll(valid, ",0\n", ",\n"),
		"no offset":     "2026-11-01T01:00:00,2026-11-01T02:00:00,1\n",
		"reversed":      "2026-01-01T01:00:00Z,2026-01-01T00:00:00Z,1\n",
		"zero interval": "2026-01-01T00:00:00Z,2026-01-01T00:00:00Z,1\n",
		"duplicate":     valid + valid,
		"overlap":       valid + "2026-01-01T00:30:00-05:00,2026-01-01T02:00:00-05:00,1\n",
		"short row":     "2026-01-01T00:00:00Z,1\n",
		"empty":         "",
		"malformed":     "not a timestamp,not a timestamp,1\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseNormalizedNYSEGCSV(strings.NewReader(header + body)); err == nil {
				t.Fatal("accepted invalid CSV")
			}
		})
	}
	for _, h := range []string{"start_time,end_time,usage", "start_time,start_time,kwh", "start_time,end_time,kwh,extra"} {
		if _, err := ParseNormalizedNYSEGCSV(strings.NewReader(h + "\n" + valid)); err == nil {
			t.Fatalf("accepted header %s", h)
		}
	}
}

func TestNYSEGPortalCSV(t *testing.T) {
	for _, csv := range []string{
		"Date,Start Time,End Time,Usage (kWh)\n11/1/2026,2026-11-01 01:00:00-04:00,2026-11-01 01:00:00-05:00,0\n",
		"\ufeffDate,Start Time,End Time,Net Units,Unit\n2026-01-01,00:00,01:00,2,kWh\n",
	} {
		rows, err := parseNYSEGCSVReader(strings.NewReader(csv))
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].StartTime.IsZero() || rows[0].EndTime.Sub(rows[0].StartTime) != time.Hour {
			t.Fatalf("bad rows: %+v", rows)
		}
	}
}

func TestPortalKeepsHourlyZerosAndHandlesMidnight(t *testing.T) {
	csv := "Date,Start Time,End Time,Usage (kWh)\n2026-06-01,22:00,23:00,0\n2026-06-01,23:00,00:00,1.25\n"
	file := filepath.Join(t.TempDir(), "portal.csv")
	if err := os.WriteFile(file, []byte(csv), 0600); err != nil {
		t.Fatal(err)
	}
	rows, err := parseNYSEGCSV(file)
	if err != nil || len(rows) != 2 || rows[0].KWh != 0 || rows[1].KWh != 1.25 || rows[1].EndTime.Sub(rows[1].StartTime) != time.Hour {
		t.Fatalf("lost hourly readings: %+v %v", rows, err)
	}
	if rows[1].EndTime.In(nysegLocation).Format("2006-01-02 15:04") != "2026-06-02 00:00" {
		t.Fatal(rows[1].EndTime)
	}
	csv = strings.Replace(csv, ",00:00,", ",24:00,", 1)
	if _, err := parseNYSEGCSVReader(strings.NewReader(csv)); err != nil {
		t.Fatal(err)
	}
}

func FuzzNYSEGCSV(f *testing.F) {
	for _, s := range []string{"", portalFixture, "Date,Start Time,End Time,Usage (kWh)\n2026-11-01,01:00,02:00,1\n", "start_time,end_time,kwh\n2026-11-01T01:00:00-04:00,2026-11-01T01:00:00-05:00,0\n"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, parse := range []func(io.Reader) ([]models.UsageData, error){ParseNormalizedNYSEGCSV, parseNYSEGCSVReader} {
			rows, err := parse(strings.NewReader(s))
			if err == nil {
				if len(rows) == 0 {
					t.Fatal("empty success")
				}
				if err := models.ValidateIntervals(rows); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
}

func TestNYSEGPortalCSVRejectsInvalid(t *testing.T) {
	for _, csv := range []string{
		"Date,Start Time,End Time,Usage (kWh)\n2026-11-01,01:00,02:00,1\n", // DST fold
		"Date,Start Time,End Time,Usage (kWh)\n2026-03-08,02:00,03:00,1\n", // DST gap
		"Date,Start Time,End Time,Usage (kWh)\n2026-01-01,00:00,01:00,NaN\n",
		"Date,Start Time,End Time,Usage (kWh)\n2026-01-01,00:00,01:00,-1\n",
		"Date,Start Time,End Time,Usage (Wh)\n2026-01-01,00:00,01:00,1\n",
		"Date,Start Time,End Time,Usage\n2026-01-01,00:00,01:00,1\n",
		"Date,Start Time,End Time,Usage (kWh),Unit\n2026-01-01,00:00,01:00,1,Wh\n",
		"Date,Start Time,End Time,Usage (kWh)\n2026-01-01,00:00,invalid,1\n",
		"Date,Usage (kWh)\n2026-01-01,1\n",
		"Date,Start Time,End Time,Usage (kWh)\ninvalid,00:00,01:00,1\n",
	} {
		if _, err := parseNYSEGCSVReader(strings.NewReader(csv)); err == nil {
			t.Fatalf("accepted invalid CSV %s", csv)
		}
	}
}

func TestKWh(t *testing.T) {
	for _, s := range []string{"NaN", "+Inf", "-Inf", "-1", "-0", "1 2", "1,2", "1Wh", "1e999", "", "1e2"} {
		if _, err := parseKWh(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
	for s, want := range map[string]float64{"0": 0, "1.25 kWh": 1.25, "1,234.50": 1234.5} {
		got, err := parseKWh(s)
		if err != nil || got != want {
			t.Errorf("%q: %v %v", s, got, err)
		}
	}
}

func TestNYSEGDateDST(t *testing.T) {
	for _, s := range []string{"2026-11-01 01:30:00", "2026-03-08 02:30:00"} {
		if _, err := parseNYSEGDate(s); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
	a, err := parseNYSEGDate("2026-11-01T01:00:00-04:00")
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseNYSEGDate("2026-11-01T01:00:00-05:00")
	if err != nil || b.Sub(a) != time.Hour {
		t.Fatalf("offsets lost: %v", err)
	}
}
