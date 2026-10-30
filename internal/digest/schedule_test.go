package digest

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}

	return loc
}

func TestNextRun(t *testing.T) {
	berlin := mustLoc(t, "Europe/Berlin")
	ny := mustLoc(t, "America/New_York")
	d := func(loc *time.Location, y int, m time.Month, day, h, mi int) time.Time {
		return time.Date(y, m, day, h, mi, 0, 0, loc)
	}
	tests := []struct {
		name string
		now  time.Time
		hhmm string
		loc  *time.Location
		want time.Time
	}{
		{"before today's slot", d(berlin, 2026, 6, 10, 6, 0), "07:30", berlin, d(berlin, 2026, 6, 10, 7, 30)},
		{
			"exactly at the slot goes to tomorrow",
			d(berlin, 2026, 6, 10, 7, 30),
			"07:30",
			berlin,
			d(berlin, 2026, 6, 11, 7, 30),
		},
		{"after today's slot", d(berlin, 2026, 6, 10, 20, 0), "07:30", berlin, d(berlin, 2026, 6, 11, 7, 30)},
		{"month rollover", d(berlin, 2026, 6, 30, 20, 0), "07:30", berlin, d(berlin, 2026, 7, 1, 7, 30)},
		{"now given in another zone", d(time.UTC, 2026, 6, 10, 23, 0), "07:30", berlin, d(berlin, 2026, 6, 11, 7, 30)},
		{
			"berlin: eve of spring forward keeps wall clock",
			d(berlin, 2026, 3, 28, 8, 0),
			"07:30",
			berlin,
			d(berlin, 2026, 3, 29, 7, 30),
		},
		{
			"berlin: day after spring forward",
			d(berlin, 2026, 3, 29, 8, 0),
			"07:30",
			berlin,
			d(berlin, 2026, 3, 30, 7, 30),
		},
		{
			"berlin: gap slot fires after the gap",
			d(berlin, 2026, 3, 29, 0, 30),
			"02:30",
			berlin,
			d(berlin, 2026, 3, 29, 3, 30),
		},
		{
			"berlin: fall back, overlap slot fires first time",
			d(berlin, 2026, 10, 25, 0, 30),
			"02:30",
			berlin,
			time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC),
		},
		{
			"berlin: between the two overlap slots goes to tomorrow",
			time.Date(2026, 10, 25, 1, 15, 0, 0, time.UTC),
			"02:30",
			berlin,
			d(berlin, 2026, 10, 26, 2, 30),
		},
		{"berlin: eve of fall back", d(berlin, 2026, 10, 24, 8, 0), "07:30", berlin, d(berlin, 2026, 10, 25, 7, 30)},
		{"ny: gap slot fires after the gap", d(ny, 2026, 3, 8, 0, 30), "02:30", ny, d(ny, 2026, 3, 8, 3, 30)},
		{"ny: day after spring forward", d(ny, 2026, 3, 8, 8, 0), "07:30", ny, d(ny, 2026, 3, 9, 7, 30)},
		{
			"ny: fall back, overlap slot fires first time",
			d(ny, 2026, 11, 1, 0, 30),
			"01:30",
			ny,
			time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC),
		},
		{"ny: eve of fall back", d(ny, 2026, 10, 31, 9, 0), "07:30", ny, d(ny, 2026, 11, 1, 7, 30)},
		{"midnight slot", d(ny, 2026, 11, 1, 0, 0), "00:00", ny, d(ny, 2026, 11, 2, 0, 0)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NextRun(tc.now, tc.hhmm, tc.loc)
			if !got.Equal(tc.want) {
				t.Errorf("NextRun = %v, want %v", got, tc.want)
			}
			if !got.After(tc.now) {
				t.Errorf("NextRun %v is not after now %v", got, tc.now)
			}
		})
	}
}

func TestNextRunNeverSkipsADay(t *testing.T) {
	for _, name := range []string{"Europe/Berlin", "America/New_York"} {
		loc := mustLoc(t, name)
		for _, hhmm := range []string{"00:00", "01:30", "02:30", "03:00", "07:30", "23:59"} {
			now := time.Date(2026, 1, 1, 12, 0, 0, 0, loc)
			prev := ""
			for range 400 {
				next := NextRun(now, hhmm, loc)
				if !next.After(now) {
					t.Fatalf("%s %s: NextRun %v not after %v", name, hhmm, next, now)
				}
				date := next.In(loc).Format("2006-01-02")
				if date == prev {
					t.Fatalf("%s %s: date %s scheduled twice", name, hhmm, date)
				}
				if prev != "" {
					p, _ := time.ParseInLocation("2006-01-02", prev, loc)
					if want := p.AddDate(0, 0, 1).Format("2006-01-02"); date != want {
						t.Fatalf("%s %s: skipped from %s to %s", name, hhmm, prev, date)
					}
				}
				prev = date
				now = next
			}
		}
	}
}

func TestNextRunInvalidHHMM(t *testing.T) {
	for _, bad := range []string{"", "7:30", "24:00", "07:60", "abc", "07-30"} {
		if got := NextRun(time.Now(), bad, time.UTC); !got.IsZero() {
			t.Errorf("NextRun(%q) = %v, want zero time", bad, got)
		}
	}
}

func TestShouldCatchUp(t *testing.T) {
	berlin := mustLoc(t, "Europe/Berlin")
	at := func(h, m int) time.Time { return time.Date(2026, 6, 10, h, m, 0, 0, berlin) }
	tests := []struct {
		name string
		now  time.Time
		hhmm string
		win  time.Duration
		last string
		want bool
	}{
		{"before the slot", at(7, 0), "07:30", 3 * time.Hour, "", false},
		{"exactly at the slot", at(7, 30), "07:30", 3 * time.Hour, "", true},
		{"within the window", at(9, 0), "07:30", 3 * time.Hour, "2026-06-09", true},
		{"exactly at the window edge", at(10, 30), "07:30", 3 * time.Hour, "", true},
		{"past the window", at(10, 31), "07:30", 3 * time.Hour, "", false},
		{"already ran today", at(8, 0), "07:30", 3 * time.Hour, "2026-06-10", false},
		{"ran only on an earlier day", at(8, 0), "07:30", 3 * time.Hour, "2026-06-01", true},
		{"zero window", at(8, 0), "07:30", 0, "", false},
		{"invalid hhmm", at(8, 0), "nope", 3 * time.Hour, "", false},
		{
			"local date differs from UTC date",
			time.Date(2026, 6, 9, 22, 30, 0, 0, time.UTC),
			"00:10",
			3 * time.Hour,
			"2026-06-10",
			false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldCatchUp(tc.now, tc.hhmm, berlin, tc.win, tc.last); got != tc.want {
				t.Errorf("ShouldCatchUp = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestShouldCatchUpDSTDays(t *testing.T) {
	berlin := mustLoc(t, "Europe/Berlin")
	ny := mustLoc(t, "America/New_York")
	// Spring forward: slot 02:30 resolves to 03:30 local; 04:00 is 30 minutes after.
	if !ShouldCatchUp(time.Date(2026, 3, 29, 4, 0, 0, 0, berlin), "02:30", berlin, time.Hour, "") {
		t.Error("berlin spring-forward: want catch-up 30m after the shifted slot")
	}
	if ShouldCatchUp(time.Date(2026, 3, 29, 3, 0, 0, 0, berlin), "02:30", berlin, time.Hour, "") {
		t.Error("berlin spring-forward: 03:00 is before the shifted slot")
	}
	if !ShouldCatchUp(time.Date(2026, 3, 8, 3, 45, 0, 0, ny), "02:30", ny, time.Hour, "") {
		t.Error("ny spring-forward: want catch-up 15m after the shifted slot")
	}
	// Fall back: slot is the first 02:30 CEST (00:30 UTC); 02:15 CET (01:15 UTC) is 45m later.
	if !ShouldCatchUp(time.Date(2026, 10, 25, 1, 15, 0, 0, time.UTC), "02:30", berlin, time.Hour, "") {
		t.Error("berlin fall-back: want catch-up inside the window")
	}
	if ShouldCatchUp(time.Date(2026, 10, 25, 1, 45, 0, 0, time.UTC), "02:30", berlin, time.Hour, "") {
		t.Error("berlin fall-back: window measured from the first occurrence has passed")
	}
}
