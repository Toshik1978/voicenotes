package mic

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) //nolint:gochecknoglobals // immutable test fixture

const (
	debounce = 500 * time.Millisecond
	minLen   = 2 * time.Second
	maxLen   = 10 * time.Minute
)

func newToggle(ids ...int64) *Toggle {
	i := 0

	return NewToggle(debounce, minLen, maxLen, func() int64 {
		id := ids[i%len(ids)]
		i++
		return id
	})
}

func TestToggleIdleTap(t *testing.T) {
	tg := newToggle(42)
	if got := tg.Tap(t0, ""); got != (Action{Kind: ActionStart, ID: 42, At: t0}) {
		t.Fatalf("first tap: %+v", got)
	}
	if !tg.Recording() {
		t.Fatal("want recording")
	}
}

func TestToggleRefuse(t *testing.T) {
	for _, r := range []RefuseReason{RefuseStorageFull, RefuseNoDevice} {
		tg := newToggle(1)
		got := tg.Tap(t0, r)
		if got != (Action{Kind: ActionRefuse, Refusal: r}) {
			t.Fatalf("%s: %+v", r, got)
		}
		if tg.Recording() {
			t.Fatal("must stay idle")
		}
		// A refusal is not a toggle: the next tap is not debounced.
		if got := tg.Tap(t0.Add(time.Millisecond), ""); got.Kind != ActionStart {
			t.Fatalf("after refuse: %+v", got)
		}
	}
}

func TestToggleIdleDebounce(t *testing.T) {
	tg := newToggle(1, 2)
	tg.Tap(t0, "")
	tg.Tap(t0.Add(minLen), "") // stop
	if got := tg.Tap(t0.Add(minLen+debounce-1), ""); got.Kind != ActionNone {
		t.Fatalf("debounced: %+v", got)
	}
	// Debounce wins over refusal.
	if got := tg.Tap(t0.Add(minLen+debounce-1), RefuseNoDevice); got.Kind != ActionNone {
		t.Fatalf("debounced refuse: %+v", got)
	}
	if got := tg.Tap(t0.Add(minLen+debounce), ""); got.Kind != ActionStart || got.ID != 2 {
		t.Fatalf("after debounce: %+v", got)
	}
}

func TestToggleRecordingTap(t *testing.T) {
	tests := []struct {
		name string
		at   time.Duration
		want Action
	}{
		{"debounced", debounce - 1, Action{}},
		{"below min discards", minLen - 1, Action{Kind: ActionDiscard, ID: 7}},
		{"at min stops", minLen, Action{Kind: ActionStop, ID: 7, End: EndTap}},
		{"later stops", minLen * 3, Action{Kind: ActionStop, ID: 7, End: EndTap}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tg := newToggle(7)
			tg.Tap(t0, "")
			if got := tg.Tap(t0.Add(tc.at), RefuseStorageFull); got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
			if tg.Recording() != (tc.want.Kind == ActionNone) {
				t.Fatalf("recording=%v", tg.Recording())
			}
		})
	}
}

func TestToggleTick(t *testing.T) {
	tg := newToggle(7)
	if got := tg.Tick(t0); got.Kind != ActionNone {
		t.Fatalf("idle tick: %+v", got)
	}
	tg.Tap(t0, "")
	if got := tg.Tick(t0.Add(maxLen - 1)); got.Kind != ActionNone {
		t.Fatalf("early tick: %+v", got)
	}
	at := t0.Add(maxLen)
	if got := tg.Tick(at); got != (Action{Kind: ActionStop, ID: 7, End: EndMaxLength}) {
		t.Fatalf("max tick: %+v", got)
	}
	if tg.Recording() {
		t.Fatal("want idle")
	}
	// last_toggle is the max-length stop; a tap inside debounce is ignored,
	// afterwards it starts a new recording.
	if got := tg.Tap(at.Add(debounce-1), ""); got.Kind != ActionNone {
		t.Fatalf("debounce after max: %+v", got)
	}
	if got := tg.Tap(at.Add(debounce), ""); got.Kind != ActionStart {
		t.Fatalf("restart: %+v", got)
	}
}

func TestToggleDeviceLost(t *testing.T) {
	tg := newToggle(7)
	if got := tg.DeviceLost(t0); got.Kind != ActionNone {
		t.Fatalf("idle: %+v", got)
	}
	tg.Tap(t0, "")
	if got := tg.DeviceLost(t0.Add(minLen - 1)); got != (Action{Kind: ActionDiscard, ID: 7}) {
		t.Fatalf("short: %+v", got)
	}
	tg.Tap(t0.Add(time.Hour), "")
	want := Action{Kind: ActionStop, ID: 7, End: EndReceiverLost}
	if got := tg.DeviceLost(t0.Add(time.Hour + minLen)); got != want {
		t.Fatalf("long: %+v", got)
	}
	if tg.Recording() {
		t.Fatal("want idle")
	}
}

func TestToggleInterrupted(t *testing.T) {
	tg := newToggle(7)
	if got := tg.Interrupt(t0); got.Kind != ActionNone {
		t.Fatalf("idle: %+v", got)
	}
	tg.Tap(t0, "")
	if got := tg.Interrupt(t0.Add(time.Millisecond)); got != (Action{Kind: ActionStop, ID: 7, End: EndInterrupted}) {
		t.Fatalf("interrupt: %+v", got)
	}
	if tg.Recording() {
		t.Fatal("want idle")
	}
}

func TestEndReasonValues(t *testing.T) {
	for r, want := range map[EndReason]string{
		EndTap: "tap", EndMaxLength: "max_length", EndReceiverLost: "receiver_lost", EndInterrupted: "interrupted",
	} {
		if string(r) != want {
			t.Errorf("%q != %q", r, want)
		}
	}
}

func TestNewID(t *testing.T) {
	seen := map[int64]bool{}
	for range 1000 {
		id := NewID()
		if id <= 0 {
			t.Fatalf("non-positive id %d", id)
		}
		seen[id] = true
	}
	if len(seen) < 990 {
		t.Fatalf("ids not random: %d distinct", len(seen))
	}
}
