package models

import (
	"fmt"
	"math"
	"sort"
)

// ValidateIntervals rejects malformed, duplicate and overlapping intervals.
// Gaps and zero consumption are valid. Export/net-negative data needs a separate model.
func ValidateIntervals(data []UsageData) error {
	ordered := append([]UsageData(nil), data...)
	for i, d := range ordered {
		if d.StartTime.IsZero() || d.EndTime.IsZero() || !d.EndTime.After(d.StartTime) {
			return fmt.Errorf("record %d: end_time must be after a nonzero start_time", i+1)
		}
		if math.IsNaN(d.KWh) || math.IsInf(d.KWh, 0) || math.Signbit(d.KWh) {
			return fmt.Errorf("record %d: kwh must be finite and nonnegative", i+1)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].StartTime.Before(ordered[j].StartTime) })
	for i := 1; i < len(ordered); i++ {
		if ordered[i].StartTime.Before(ordered[i-1].EndTime) {
			return fmt.Errorf("duplicate or overlapping intervals in input")
		}
	}
	return nil
}
