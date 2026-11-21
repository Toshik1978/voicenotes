package mic

import (
	"fmt"
	"time"
)

// Alert names the outstanding condition of a recorder. The values are stored
// in the recorder row and travel as-is.
type Alert string

// The exact set of alerts. AlertOK means no condition is outstanding.
const (
	AlertOK              Alert = "ok"
	AlertMissingReceiver Alert = "missing_receiver"
	AlertStorageFull     Alert = "storage_full"
	AlertSilent          Alert = "silent"
)

// RecorderStatus is what [Alerts.Eval] needs to know about a recorder: its
// last check-in and the outstanding Alert already stored for it.
type RecorderStatus struct {
	Name            string
	LastSeen        time.Time
	ReceiverPresent bool
	StorageFull     bool
	Alert           Alert
}

// Alerts decides which recorder alerts to raise. Location is the zone of the
// HH:MM in the silence message; nil means UTC.
type Alerts struct {
	Location *time.Location
}

// Eval computes the desired alert for the status at now and returns it. The
// message is non-empty only when the alert differs from status.Alert, so the
// caller sends it once per change and stores the returned alert.
func (a Alerts) Eval(status RecorderStatus, now time.Time, silence time.Duration) (Alert, string) {
	want := desiredAlert(status, now, silence)
	if want == status.Alert {
		return want, ""
	}
	return want, a.message(status, want)
}

func desiredAlert(s RecorderStatus, now time.Time, silence time.Duration) Alert {
	switch {
	case now.Sub(s.LastSeen) > silence:
		return AlertSilent
	case s.StorageFull:
		return AlertStorageFull
	case !s.ReceiverPresent:
		return AlertMissingReceiver
	default:
		return AlertOK
	}
}

// message is the alert text, or the recovery text for the cleared alert when
// the recorder is back to ok.
func (a Alerts) message(s RecorderStatus, want Alert) string {
	switch want {
	case AlertSilent:
		loc := a.Location
		if loc == nil {
			loc = time.UTC
		}
		return fmt.Sprintf("🎙 %s: not heard from since %s", s.Name, s.LastSeen.In(loc).Format("15:04"))
	case AlertStorageFull:
		return fmt.Sprintf("🎙 %s: storage full — new recordings refused", s.Name)
	case AlertMissingReceiver:
		return fmt.Sprintf("🎙 %s: receiver not connected — taps won't record", s.Name)
	case AlertOK:
		return a.recovery(s)
	}

	return ""
}

// recovery is the text for an alert that just cleared.
func (Alerts) recovery(s RecorderStatus) string {
	switch s.Alert {
	case AlertSilent:
		return fmt.Sprintf("🎙 %s: back online", s.Name)
	case AlertStorageFull:
		return fmt.Sprintf("🎙 %s: storage OK", s.Name)
	case AlertMissingReceiver:
		return fmt.Sprintf("🎙 %s: receiver connected", s.Name)
	case AlertOK:
		return ""
	}

	return ""
}
