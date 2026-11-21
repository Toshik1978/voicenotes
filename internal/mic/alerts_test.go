package mic

import (
	"slices"
	"testing"
	"time"
)

const (
	msgMissing   = "🎙 bed: receiver not connected — taps won't record"
	msgConnected = "🎙 bed: receiver connected"
	msgFull      = "🎙 bed: storage full — new recordings refused"
	msgStorageOK = "🎙 bed: storage OK"
	msgSilent    = "🎙 bed: not heard from since 11:05"
	msgBack      = "🎙 bed: back online"
)

func TestAlertsEval(t *testing.T) {
	loc := time.FixedZone("test", 3*3600)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	silence := 10 * time.Minute
	fresh := now.Add(-time.Minute)
	stale := time.Date(2026, 3, 1, 8, 5, 0, 0, time.UTC) // 11:05 in loc

	none := AlertSet(0)
	missing := NewAlertSet(AlertMissingReceiver)
	full := NewAlertSet(AlertStorageFull)
	silent := NewAlertSet(AlertSilent)
	missingFull := NewAlertSet(AlertMissingReceiver, AlertStorageFull)

	tests := []struct {
		name     string
		status   RecorderStatus
		messages []string
		final    AlertSet
	}{
		{
			"ok to ok emits nothing",
			RecorderStatus{Name: "bed", LastSeen: fresh, ReceiverPresent: true},
			nil, none,
		},
		{
			"receiver missing",
			RecorderStatus{Name: "bed", LastSeen: fresh},
			[]string{msgMissing},
			missing,
		},
		{
			"missing receiver unchanged",
			RecorderStatus{Name: "bed", LastSeen: fresh, Outstanding: missing},
			nil, missing,
		},
		{
			"receiver back",
			RecorderStatus{Name: "bed", LastSeen: fresh, ReceiverPresent: true, Outstanding: missing},
			[]string{msgConnected},
			none,
		},
		{
			"storage full",
			RecorderStatus{Name: "bed", LastSeen: fresh, ReceiverPresent: true, StorageFull: true},
			[]string{msgFull},
			full,
		},
		{
			"storage full unchanged",
			RecorderStatus{Name: "bed", LastSeen: fresh, ReceiverPresent: true, StorageFull: true, Outstanding: full},
			nil, full,
		},
		{
			"storage ok",
			RecorderStatus{Name: "bed", LastSeen: fresh, ReceiverPresent: true, Outstanding: full},
			[]string{msgStorageOK},
			none,
		},
		{
			"both conditions start together",
			RecorderStatus{Name: "bed", LastSeen: fresh, StorageFull: true},
			[]string{msgFull, msgMissing},
			missingFull,
		},
		{
			"receiver goes missing while storage is full",
			RecorderStatus{Name: "bed", LastSeen: fresh, StorageFull: true, Outstanding: full},
			[]string{msgMissing},
			missingFull,
		},
		{
			"storage fills while receiver is missing",
			RecorderStatus{Name: "bed", LastSeen: fresh, StorageFull: true, Outstanding: missing},
			[]string{msgFull},
			missingFull,
		},
		{
			"both persist",
			RecorderStatus{Name: "bed", LastSeen: fresh, StorageFull: true, Outstanding: missingFull},
			nil, missingFull,
		},
		{
			"storage ok while receiver still missing",
			RecorderStatus{Name: "bed", LastSeen: fresh, Outstanding: missingFull},
			[]string{msgStorageOK},
			missing,
		},
		{
			"receiver back while storage still full",
			RecorderStatus{
				Name:            "bed",
				LastSeen:        fresh,
				ReceiverPresent: true,
				StorageFull:     true,
				Outstanding:     missingFull,
			},
			[]string{msgConnected},
			full,
		},
		{
			"both clear together",
			RecorderStatus{Name: "bed", LastSeen: fresh, ReceiverPresent: true, Outstanding: missingFull},
			[]string{msgStorageOK, msgConnected},
			none,
		},
		{
			"one clears as the other starts",
			RecorderStatus{Name: "bed", LastSeen: fresh, StorageFull: true, Outstanding: missing},
			[]string{msgFull},
			missingFull,
		},
		{
			"receiver swaps for storage",
			RecorderStatus{
				Name:            "bed",
				LastSeen:        fresh,
				ReceiverPresent: true,
				StorageFull:     true,
				Outstanding:     missing,
			},
			[]string{msgFull, msgConnected},
			full,
		},
		{
			"silent uses configured location",
			RecorderStatus{Name: "bed", LastSeen: stale, ReceiverPresent: true},
			[]string{msgSilent},
			silent,
		},
		{
			"silent reports only silent",
			RecorderStatus{Name: "bed", LastSeen: stale, StorageFull: true},
			[]string{msgSilent},
			silent,
		},
		{
			"silent keeps earlier conditions without repeating them",
			RecorderStatus{Name: "bed", LastSeen: stale, Outstanding: missing},
			[]string{msgSilent},
			missing | silent,
		},
		{
			"silent does not recover earlier conditions from a stale check-in",
			RecorderStatus{Name: "bed", LastSeen: stale, ReceiverPresent: true, Outstanding: missingFull},
			[]string{msgSilent},
			missingFull | silent,
		},
		{
			"silent unchanged",
			RecorderStatus{Name: "bed", LastSeen: stale, StorageFull: true, Outstanding: silent},
			nil, silent,
		},
		{
			"back online",
			RecorderStatus{Name: "bed", LastSeen: fresh, ReceiverPresent: true, Outstanding: silent},
			[]string{msgBack},
			none,
		},
		{
			"back online with the receiver still missing",
			RecorderStatus{Name: "bed", LastSeen: fresh, Outstanding: missing | silent},
			[]string{msgBack},
			missing,
		},
		{
			"back online with the receiver back",
			RecorderStatus{Name: "bed", LastSeen: fresh, ReceiverPresent: true, Outstanding: missing | silent},
			[]string{msgBack, msgConnected},
			none,
		},
		{
			"back online and storage full",
			RecorderStatus{Name: "bed", LastSeen: fresh, ReceiverPresent: true, StorageFull: true, Outstanding: silent},
			[]string{msgBack, msgFull},
			full,
		},
		{
			"silent to missing receiver",
			RecorderStatus{Name: "bed", LastSeen: fresh, Outstanding: silent},
			[]string{msgBack, msgMissing},
			missing,
		},
		{
			"exactly at silence is not silent",
			RecorderStatus{Name: "bed", LastSeen: now.Add(-silence), ReceiverPresent: true},
			nil, none,
		},
		{
			"just past silence",
			RecorderStatus{Name: "bed", LastSeen: now.Add(-silence - time.Second), ReceiverPresent: true},
			[]string{"🎙 bed: not heard from since 14:49"},
			silent,
		},
	}
	a := Alerts{Location: loc}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			steps := a.Eval(tc.status, now, silence)
			var msgs []string
			prev := tc.status.Outstanding
			for _, s := range steps {
				msgs = append(msgs, s.Message)
				if d := s.Outstanding ^ prev; d == 0 || d&(d-1) != 0 {
					t.Errorf(
						"step %q moves %v to %v, want exactly one condition changed",
						s.Message,
						prev,
						s.Outstanding,
					)
				}
				prev = s.Outstanding
			}
			if !slices.Equal(msgs, tc.messages) || prev != tc.final {
				t.Fatalf("Eval = %q ending at %v, want %q ending at %v", msgs, prev, tc.messages, tc.final)
			}
		})
	}
}

func TestAlertsEvalNilLocationUsesUTC(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	st := RecorderStatus{Name: "bed", LastSeen: now.Add(-time.Hour), ReceiverPresent: true}
	steps := Alerts{}.Eval(st, now, time.Minute)
	if want := "🎙 bed: not heard from since 11:00"; len(steps) != 1 || steps[0].Message != want {
		t.Fatalf("steps = %+v, want one %q", steps, want)
	}
}

func TestAlertSetStringAndParse(t *testing.T) {
	tests := []struct {
		set  AlertSet
		text string
	}{
		{0, "ok"},
		{NewAlertSet(AlertSilent), "silent"},
		{NewAlertSet(AlertStorageFull), "storage_full"},
		{NewAlertSet(AlertMissingReceiver), "missing_receiver"},
		{NewAlertSet(AlertSilent, AlertMissingReceiver, AlertStorageFull), "missing_receiver,storage_full,silent"},
	}
	for _, tc := range tests {
		if got := tc.set.String(); got != tc.text {
			t.Errorf("String(%d) = %q, want %q", tc.set, got, tc.text)
		}
		got, err := ParseAlertSet(tc.text)
		if err != nil || got != tc.set {
			t.Errorf("ParseAlertSet(%q) = %v, %v, want %v", tc.text, got, err, tc.set)
		}
	}
	for _, in := range []string{"", "ok,silent", "silent,silent", "bogus", "silent,"} {
		if in == "" {
			if got, err := ParseAlertSet(in); err != nil || got != 0 {
				t.Errorf("ParseAlertSet(empty) = %v, %v, want none", got, err)
			}
			continue
		}
		if _, err := ParseAlertSet(in); err == nil {
			t.Errorf("ParseAlertSet(%q) succeeded, want error", in)
		}
	}
	if !NewAlertSet(AlertSilent).Has(AlertSilent) || NewAlertSet(AlertSilent).Has(AlertStorageFull) {
		t.Error("Has is wrong")
	}
}
