package scraper

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // Keep NYSEG local-time interpretation independent of the host image.

	"github.com/jgoulah/gridscraper/pkg/models"
)

var nysegLocation = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err)
	}
	return loc
}()
var decimalKWh = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]+)?|\.[0-9]+)$`)
var formattedKWh = regexp.MustCompile(`^(?:[0-9]+|[0-9]{1,3}(?:,[0-9]{3})+)(?:\.[0-9]+)?$|^\.[0-9]+$`)

// ParseNormalizedNYSEGCSV imports ONLY the documented normalized schema, not a
// guessed utility/Google Sheets format. Timestamps must carry RFC3339 offsets.
// All rows are validated before callers may open/write a database.
func ParseNormalizedNYSEGCSV(r io.Reader) ([]models.UsageData, error) {
	reader := csv.NewReader(r)
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("reading CSV header: %w", err)
	}
	if len(header) != 3 || strings.Join(header, ",") != "start_time,end_time,kwh" {
		return nil, fmt.Errorf("expected exactly start_time,end_time,kwh columns (in that order)")
	}
	var data []models.UsageData
	for row := 2; ; row++ {
		rec, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("CSV row %d: invalid CSV: %w", row, err)
		}
		start, err := parseOffsetTime(strings.TrimSpace(rec[0]))
		if err != nil {
			return nil, fmt.Errorf("CSV row %d start_time: RFC3339 with explicit offset required", row)
		}
		end, err := parseOffsetTime(strings.TrimSpace(rec[1]))
		if err != nil {
			return nil, fmt.Errorf("CSV row %d end_time: RFC3339 with explicit offset required", row)
		}
		value := strings.TrimSpace(rec[2])
		if !decimalKWh.MatchString(value) {
			return nil, fmt.Errorf("CSV row %d kwh: plain nonnegative decimal required (no units)", row)
		}
		kwh, err := parseKWh(value)
		if err != nil {
			return nil, fmt.Errorf("CSV row %d kwh: %w", row, err)
		}
		data = append(data, nysegReading(start, end, kwh))
	}
	return validatedNYSEGRows(data)
}

func nysegReading(start, end time.Time, kwh float64) models.UsageData {
	local := start.In(nysegLocation)
	date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, nysegLocation)
	return models.UsageData{Date: date, StartTime: start, EndTime: end, KWh: kwh, Service: "nyseg"}
}
func validatedNYSEGRows(data []models.UsageData) ([]models.UsageData, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("CSV has no readings")
	}
	if err := models.ValidateIntervals(data); err != nil {
		return nil, err
	}
	return data, nil
}

// parseNYSEGCSVReader accepts a bounded portal schema: Date, Start Time, End
// Time, and Usage/Net Units/Net with explicit kWh units in the header or Unit(s).
// Unknown formats fail closed instead of silently losing rows or guessing units.
func parseNYSEGCSVReader(r io.Reader) ([]models.UsageData, error) {
	reader := csv.NewReader(r)
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("reading CSV header: %w", err)
	}
	cols := map[string]int{}
	usageHasUnits := false
	for i, h := range header {
		h = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))
		key := ""
		switch h {
		case "date":
			key = "date"
		case "start time":
			key = "start"
		case "end time":
			key = "end"
		case "unit", "units", "unit of measure", "unit of measurement":
			key = "unit"
		case "usage", "net units", "net":
			key = "usage"
		case "usage (kwh)", "usage(kwh)", "usage kwh", "usage [kwh]", "net units (kwh)", "net (kwh)", "net(kwh)":
			key = "usage"
			usageHasUnits = true
		}
		if key != "" {
			if _, ok := cols[key]; ok {
				return nil, fmt.Errorf("duplicate CSV column")
			}
			cols[key] = i
		}
	}
	for _, key := range []string{"date", "start", "end", "usage"} {
		if _, ok := cols[key]; !ok {
			return nil, fmt.Errorf("portal CSV requires Date, Start Time, End Time, and Usage/Net Units with explicit kWh units")
		}
	}
	unitCol, hasUnitCol := cols["unit"]
	if !usageHasUnits && !hasUnitCol {
		return nil, fmt.Errorf("portal CSV must declare kWh units; normalize unsupported exports explicitly")
	}
	var data []models.UsageData
	for row := 2; ; row++ {
		rec, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("CSV row %d: %w", row, err)
		}
		if hasUnitCol && !strings.EqualFold(strings.TrimSpace(rec[unitCol]), "kwh") {
			return nil, fmt.Errorf("CSV row %d: unit must be kWh", row)
		}
		date, err := parseNYSEGDate(rec[cols["date"]])
		if err != nil {
			return nil, fmt.Errorf("CSV row %d date: %w", row, err)
		}
		start, err := parsePortalTime(rec[cols["start"]], date)
		if err != nil {
			return nil, fmt.Errorf("CSV row %d start_time: %w", row, err)
		}
		endValue := strings.TrimSpace(rec[cols["end"]])
		endDate := date
		// Midnight at the end of a clock-only interval belongs to the next day.
		if (endValue == "00:00" || endValue == "00:00:00" || endValue == "12:00 AM") && (start.Hour() != 0 || start.Minute() != 0) {
			endDate = date.AddDate(0, 0, 1)
		}
		if endValue == "24:00" || endValue == "24:00:00" {
			endValue = "00:00"
			endDate = date.AddDate(0, 0, 1)
		}
		end, err := parsePortalTime(endValue, endDate)
		if err != nil {
			return nil, fmt.Errorf("CSV row %d end_time: %w", row, err)
		}
		if start.In(nysegLocation).Format("2006-01-02") != date.Format("2006-01-02") {
			return nil, fmt.Errorf("CSV row %d: Date disagrees with start_time in America/New_York", row)
		}
		kwh, err := parseKWh(rec[cols["usage"]])
		if err != nil {
			return nil, fmt.Errorf("CSV row %d usage: %w", row, err)
		}
		data = append(data, nysegReading(start, end, kwh))
	}
	return validatedNYSEGRows(data)
}

func parseOffsetTime(s string) (time.Time, error) {
	// Go's RFC3339 parser permits some non-RFC offset/clock spellings; constrain them.
	if !regexpRFC3339.MatchString(s) {
		return time.Time{}, fmt.Errorf("explicit RFC3339 offset required")
	}
	return time.Parse(time.RFC3339Nano, s)
}

var regexpRFC3339 = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.[0-9]{1,9})?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$`)

func parsePortalTime(s string, date time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"15:04:05", "15:04", "3:04 PM", "3:04PM"} {
		clock, err := time.Parse(layout, s)
		if err == nil {
			return parseNYSEGDate(date.Format("2006-01-02") + " " + clock.Format("15:04:05"))
		}
	}
	if len(s) <= 10 || (s[10] != 'T' && s[10] != ' ') {
		return time.Time{}, fmt.Errorf("interval endpoint requires a time, not just a date")
	}
	return parseNYSEGDate(s)
}

// parseNYSEGDate never interprets a timezone-less portal value in the machine's
// timezone. Reject both nonexistent spring hours and ambiguous autumn hours.
func parseNYSEGDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := parseOffsetTime(s); err == nil {
		return t, nil
	}
	if len(s) > 10 && s[10] == ' ' {
		if t, err := parseOffsetTime(s[:10] + "T" + s[11:]); err == nil {
			return t, nil
		}
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "1/2/2006", "01/02/2006", "2006-01-02", "1/2/06", "01/02/06", "Jan 2, 2006", "January 2, 2006"} {
		wall, err := time.Parse(layout, s)
		if err != nil {
			continue
		}
		// Collect all offsets around the date; do not rely on time.Date's arbitrary
		// choice for a fold/gap. NYSEG's location uses one-hour seasonal transitions.
		offsets := map[int]bool{}
		for _, delta := range []time.Duration{-48 * time.Hour, 0, 48 * time.Hour} {
			_, o := wall.Add(delta).In(nysegLocation).Zone()
			offsets[o] = true
		}
		var matches []time.Time
		for offset := range offsets {
			candidate := wall.Add(-time.Duration(offset) * time.Second).In(nysegLocation)
			if candidate.Format("2006-01-02 15:04:05") == wall.Format("2006-01-02 15:04:05") {
				matches = append(matches, candidate)
			}
		}
		if len(matches) != 1 {
			return time.Time{}, fmt.Errorf("ambiguous or nonexistent America/New_York local time; provide an explicit offset via normalized CSV")
		}
		return matches[0], nil
	}
	return time.Time{}, fmt.Errorf("unsupported NYSEG date/time")
}

func parseKWh(s string) (float64, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSpace(strings.TrimSuffix(strings.ToLower(s), "kwh"))
	if !formattedKWh.MatchString(s) {
		return 0, fmt.Errorf("usage must be a nonnegative decimal in kWh")
	}
	n, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || math.Signbit(n) {
		return 0, fmt.Errorf("usage must be finite and nonnegative")
	}
	return n, nil
}
