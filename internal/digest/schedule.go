package digest

import (
	"time"
)

// dateLayout is the local-date format stored in digest_runs.local_date.
const dateLayout = "2006-01-02"

// parseHHMM parses a strict "HH:MM" 24-hour time, as validated by config.
func parseHHMM(hhmm string) (hour, minute int, ok bool) {
	t, err := time.Parse("15:04", hhmm)
	if err != nil || len(hhmm) != 5 {
		return 0, 0, false
	}
	return t.Hour(), t.Minute(), true
}

// slotOn returns the instant of the HH:MM slot on the given local calendar day.
//
// Daylight-saving transitions are resolved to one defined instant per day:
//   - A wall-clock time inside a spring-forward gap does not exist; the slot
//     moves forward by the gap length, so it fires at the same instant a
//     clock would show after the jump (02:30 becomes 03:30).
//   - A wall-clock time that occurs twice in a fall-back overlap fires at its
//     first occurrence.
func slotOn(year int, month time.Month, day, hour, minute int, loc *time.Location) time.Time {
	t := time.Date(year, month, day, hour, minute, 0, 0, loc)
	want := hour*60 + minute

	// Gap: Go may land before the gap (NY) or after it (Berlin). Only the
	// former shows an earlier wall clock; push it forward by the difference.
	if got := t.Hour()*60 + t.Minute(); t.Day() == day && got < want {
		t = t.Add(time.Duration(want-got) * time.Minute)
	}
	// Overlap: Go may pick the second occurrence; prefer the first.
	if e := t.Add(-time.Hour); e.Day() == day && e.Hour() == t.Hour() && e.Minute() == t.Minute() {
		t = e
	}

	return t
}

// NextRun returns the first digest instant strictly after now for the given
// HH:MM slot in loc. It returns the zero time when hhmm is not a valid HH:MM.
// Each local calendar day has exactly one slot, so no day is skipped or
// repeated across DST changes.
func NextRun(now time.Time, hhmm string, loc *time.Location) time.Time {
	hour, minute, ok := parseHHMM(hhmm)
	if !ok {
		return time.Time{}
	}
	local := now.In(loc)
	y, m, d := local.Date()
	for i := range 3 {
		// time.Date normalises day overflow, so this walks calendar days.
		c := time.Date(y, m, d+i, 12, 0, 0, 0, loc)
		slot := slotOn(c.Year(), c.Month(), c.Day(), hour, minute, loc)
		if slot.After(now) {
			return slot
		}
	}

	return time.Time{} // unreachable: the slot two days ahead is always after now
}

// ShouldCatchUp reports whether a digest missed today should still be sent
// now: today's slot has passed, no more than window ago, and lastRunDate (the
// newest digest_runs local_date, "YYYY-MM-DD" in loc, empty when there is
// none) is not today. See research R6.
func ShouldCatchUp(now time.Time, hhmm string, loc *time.Location, window time.Duration, lastRunDate string) bool {
	hour, minute, ok := parseHHMM(hhmm)
	if !ok {
		return false
	}
	local := now.In(loc)
	if lastRunDate == local.Format(dateLayout) {
		return false
	}
	y, m, d := local.Date()
	slot := slotOn(y, m, d, hour, minute, loc)
	if now.Before(slot) {
		return false
	}

	return now.Sub(slot) <= window
}
