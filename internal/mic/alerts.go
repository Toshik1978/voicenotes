package mic

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Alert names one recorder condition. The names are stored in the recorder
// row and travel as-is.
type Alert string

// The exact set of conditions. AlertOK is the stored name of an empty
// [AlertSet]: no condition is outstanding.
const (
	AlertOK              Alert = "ok"
	AlertMissingReceiver Alert = "missing_receiver"
	AlertStorageFull     Alert = "storage_full"
	AlertSilent          Alert = "silent"
)

// storedOrder lists every condition in the order of its bit and of the
// stored form.
func storedOrder() [3]Alert { return [3]Alert{AlertMissingReceiver, AlertStorageFull, AlertSilent} }

// evalOrder is the order in which Eval reports changes: silence first, so
// "back online" precedes what the fresh check-in shows.
func evalOrder() [3]Alert { return [3]Alert{AlertSilent, AlertStorageFull, AlertMissingReceiver} }

// AlertSet is the set of outstanding conditions of a recorder, each tracked
// independently. The zero value is "ok".
type AlertSet uint8

// NewAlertSet returns the set holding the given conditions.
func NewAlertSet(alerts ...Alert) AlertSet {
	var s AlertSet
	for _, a := range alerts {
		s |= bit(a)
	}

	return s
}

func bit(a Alert) AlertSet {
	for i, c := range storedOrder() {
		if c == a {
			return 1 << i
		}
	}

	return 0
}

// Has reports whether condition a is outstanding.
func (s AlertSet) Has(a Alert) bool { b := bit(a); return b != 0 && s&b != 0 }

// String is the stored form: "ok" for the empty set, else the condition
// names joined by commas in a fixed order.
func (s AlertSet) String() string {
	if s == 0 {
		return string(AlertOK)
	}
	order := storedOrder()
	names := make([]string, 0, len(order))
	for _, c := range order {
		if s.Has(c) {
			names = append(names, string(c))
		}
	}

	return strings.Join(names, ",")
}

// ParseAlertSet parses the stored form written by [AlertSet.String]. An empty
// string is "ok".
func ParseAlertSet(text string) (AlertSet, error) {
	if text == "" || text == string(AlertOK) {
		return 0, nil
	}
	var s AlertSet
	for name := range strings.SplitSeq(text, ",") {
		b := bit(Alert(name))
		if b == 0 {
			return 0, fmt.Errorf("mic: unknown recorder alert %q", name)
		}
		if s&b != 0 {
			return 0, errors.New("mic: repeated recorder alert")
		}
		s |= b
	}

	return s, nil
}

// RecorderStatus is what [Alerts.Eval] needs to know about a recorder: its
// last check-in and the conditions already reported for it.
type RecorderStatus struct {
	Name            string
	LastSeen        time.Time
	ReceiverPresent bool
	StorageFull     bool
	Outstanding     AlertSet
}

// AlertStep is one message to send and the outstanding set to store once it
// was sent.
type AlertStep struct {
	Outstanding AlertSet
	Message     string
}

// Alerts decides which recorder alerts to raise. Location is the zone of the
// HH:MM in the silence message; nil means UTC.
type Alerts struct {
	Location *time.Location
}

// Eval compares the conditions at now with status.Outstanding and returns one
// step per condition that started (its alert) or cleared (its recovery), in
// evalOrder. Each step's Outstanding is the set after that one change, so
// the caller sends each message and stores its set before the next; a crash
// repeats at most one message. A persisting condition yields nothing.
//
// While the recorder is silent its last check-in is stale: only silence is
// evaluated and the other conditions keep their reported state, so a box
// coming back sends "back online" without repeating or dropping the rest.
func (a Alerts) Eval(status RecorderStatus, now time.Time, silence time.Duration) []AlertStep {
	var want AlertSet
	if now.Sub(status.LastSeen) > silence {
		want = status.Outstanding | NewAlertSet(AlertSilent)
	} else {
		if status.StorageFull {
			want |= NewAlertSet(AlertStorageFull)
		}
		if !status.ReceiverPresent {
			want |= NewAlertSet(AlertMissingReceiver)
		}
	}

	var steps []AlertStep
	cur := status.Outstanding
	for _, c := range evalOrder() {
		b := bit(c)
		if cur&b == want&b {
			continue
		}
		cur ^= b
		steps = append(steps, AlertStep{Outstanding: cur, Message: a.message(status, c, cur&b != 0)})
	}

	return steps
}

// message is the alert text for condition c when it starts, or its recovery
// text when it clears.
func (a Alerts) message(s RecorderStatus, c Alert, started bool) string {
	switch {
	case c == AlertSilent && started:
		loc := a.Location
		if loc == nil {
			loc = time.UTC
		}
		return fmt.Sprintf("🎙 %s: not heard from since %s", s.Name, s.LastSeen.In(loc).Format("15:04"))
	case c == AlertSilent:
		return fmt.Sprintf("🎙 %s: back online", s.Name)
	case c == AlertStorageFull && started:
		return fmt.Sprintf("🎙 %s: storage full — new recordings refused", s.Name)
	case c == AlertStorageFull:
		return fmt.Sprintf("🎙 %s: storage OK", s.Name)
	case started:
		return fmt.Sprintf("🎙 %s: receiver not connected — taps won't record", s.Name)
	default:
		return fmt.Sprintf("🎙 %s: receiver connected", s.Name)
	}
}
