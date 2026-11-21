package mic

import (
	"testing"
	"time"
)

func TestAlertsEval(t *testing.T) {
	loc := time.FixedZone("test", 3*3600)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	silence := 10 * time.Minute
	fresh := now.Add(-time.Minute)
	stale := time.Date(2026, 3, 1, 8, 5, 0, 0, time.UTC) // 11:05 in loc

	tests := []struct {
		name    string
		status  RecorderStatus
		want    Alert
		message string
	}{
		{
			"ok to ok emits nothing",
			RecorderStatus{Name: "bed", LastSeen: fresh, ReceiverPresent: true, Alert: AlertOK},
			AlertOK,
			"",
		},
		{
			"receiver missing",
			RecorderStatus{Name: "bed", LastSeen: fresh, Alert: AlertOK},
			AlertMissingReceiver,
			"🎙 bed: receiver not connected — taps won't record",
		},
		{
			"missing receiver unchanged",
			RecorderStatus{Name: "bed", LastSeen: fresh, Alert: AlertMissingReceiver},
			AlertMissingReceiver,
			"",
		},
		{
			"receiver back",
			RecorderStatus{Name: "bed", LastSeen: fresh, ReceiverPresent: true, Alert: AlertMissingReceiver},
			AlertOK,
			"🎙 bed: receiver connected",
		},
		{
			"storage full",
			RecorderStatus{Name: "bed", LastSeen: fresh, ReceiverPresent: true, StorageFull: true, Alert: AlertOK},
			AlertStorageFull,
			"🎙 bed: storage full — new recordings refused",
		},
		{
			"storage full beats missing receiver",
			RecorderStatus{Name: "bed", LastSeen: fresh, StorageFull: true, Alert: AlertStorageFull},
			AlertStorageFull,
			"",
		},
		{
			"storage ok",
			RecorderStatus{Name: "bed", LastSeen: fresh, ReceiverPresent: true, Alert: AlertStorageFull},
			AlertOK,
			"🎙 bed: storage OK",
		},
		{
			"silent uses configured location",
			RecorderStatus{Name: "bed", LastSeen: stale, ReceiverPresent: true, Alert: AlertOK},
			AlertSilent,
			"🎙 bed: not heard from since 11:05",
		},
		{
			"silent beats storage full",
			RecorderStatus{Name: "bed", LastSeen: stale, StorageFull: true, Alert: AlertSilent},
			AlertSilent,
			"",
		},
		{
			"back online",
			RecorderStatus{Name: "bed", LastSeen: fresh, ReceiverPresent: true, Alert: AlertSilent},
			AlertOK,
			"🎙 bed: back online",
		},
		{
			"exactly at silence is not silent",
			RecorderStatus{Name: "bed", LastSeen: now.Add(-silence), ReceiverPresent: true, Alert: AlertOK},
			AlertOK,
			"",
		},
		{
			"just past silence",
			RecorderStatus{
				Name:            "bed",
				LastSeen:        now.Add(-silence - time.Second),
				ReceiverPresent: true,
				Alert:           AlertOK,
			},
			AlertSilent,
			"🎙 bed: not heard from since 14:49",
		},
		{
			"silent to missing receiver announces the new alert",
			RecorderStatus{Name: "bed", LastSeen: fresh, Alert: AlertSilent},
			AlertMissingReceiver,
			"🎙 bed: receiver not connected — taps won't record",
		},
	}
	a := Alerts{Location: loc}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := a.Eval(tc.status, now, silence)
			if got != tc.want || msg != tc.message {
				t.Fatalf("Eval = (%q, %q), want (%q, %q)", got, msg, tc.want, tc.message)
			}
		})
	}
}

func TestAlertsEvalNilLocationUsesUTC(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	st := RecorderStatus{Name: "bed", LastSeen: now.Add(-time.Hour), ReceiverPresent: true, Alert: AlertOK}
	_, msg := Alerts{}.Eval(st, now, time.Minute)
	if want := "🎙 bed: not heard from since 11:00"; msg != want {
		t.Fatalf("message = %q, want %q", msg, want)
	}
}
