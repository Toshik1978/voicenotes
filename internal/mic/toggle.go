// Package mic holds the pure decision logic of the USB microphone recorder:
// the tap state machine and the recording identity. It performs no I/O; the
// recorder feeds it events and executes the actions it returns.
package mic

import "time"

// EndReason says why a recording was stopped. The values travel on the wire.
type EndReason string

// The exact set of end reasons.
const (
	EndTap          EndReason = "tap"
	EndMaxLength    EndReason = "max_length"
	EndReceiverLost EndReason = "receiver_lost"
	EndInterrupted  EndReason = "interrupted"
)

// RefuseReason says why a recording cannot start right now. The empty value
// means recording is possible.
type RefuseReason string

// Reasons the recorder passes to [Toggle.Tap] when it cannot record.
const (
	RefuseStorageFull RefuseReason = "storage_full"
	RefuseNoDevice    RefuseReason = "no_audio_device"
)

// ActionKind identifies what the recorder must do.
type ActionKind int

// The action kinds. ActionNone (the zero value) means nothing to do.
const (
	ActionNone ActionKind = iota
	ActionStart
	ActionStop
	ActionDiscard
	ActionRefuse
)

// Action is the recorder's instruction. Which fields are set depends on Kind:
// Start has ID and At; Stop has ID and End; Discard has ID; Refuse has Refusal.
type Action struct {
	Kind    ActionKind
	ID      int64
	At      time.Time
	End     EndReason
	Refusal RefuseReason
}

// Toggle is the tap state machine. It is not safe for concurrent use; the
// recorder loop owns it.
type Toggle struct {
	debounce, minLen, maxLen time.Duration
	newID                    func() int64

	recording  bool
	id         int64
	startedAt  time.Time
	lastToggle time.Time
	toggled    bool // lastToggle is set
}

// NewToggle returns an idle machine. newID supplies a recording ID at each
// start (use [NewID] in production).
func NewToggle(debounce, minLen, maxLen time.Duration, newID func() int64) *Toggle {
	return &Toggle{debounce: debounce, minLen: minLen, maxLen: maxLen, newID: newID}
}

// Recording reports whether a recording is in progress.
func (g *Toggle) Recording() bool { return g.recording }

// Tap handles a tap at t. unavailable is the recorder's current inability to
// record (storage full, no device), or "" when it can; it only matters when
// idle.
func (g *Toggle) Tap(t time.Time, unavailable RefuseReason) Action {
	if g.debounced(t) {
		return Action{}
	}
	if !g.recording {
		if unavailable != "" {
			return Action{Kind: ActionRefuse, Refusal: unavailable}
		}
		g.recording = true
		g.id = g.newID()
		g.startedAt = t
		g.lastToggle, g.toggled = t, true

		return Action{Kind: ActionStart, ID: g.id, At: t}
	}
	if t.Sub(g.startedAt) < g.minLen {
		return g.end(t, Action{Kind: ActionDiscard, ID: g.id})
	}

	return g.end(t, Action{Kind: ActionStop, ID: g.id, End: EndTap})
}

// Tick handles the passage of time: it stops a recording that reached the
// maximum length.
func (g *Toggle) Tick(t time.Time) Action {
	if !g.recording || t.Sub(g.startedAt) < g.maxLen {
		return Action{}
	}
	return g.end(t, Action{Kind: ActionStop, ID: g.id, End: EndMaxLength})
}

// DeviceLost handles the microphone disappearing at t. A recording below the
// minimum length is discarded, otherwise it is stopped as receiver_lost.
func (g *Toggle) DeviceLost(t time.Time) Action {
	if !g.recording {
		return Action{}
	}
	if t.Sub(g.startedAt) < g.minLen {
		return g.end(t, Action{Kind: ActionDiscard, ID: g.id})
	}

	return g.end(t, Action{Kind: ActionStop, ID: g.id, End: EndReceiverLost})
}

// Interrupt handles a recorder shutdown or write failure at t. A recording
// below the minimum length is discarded like a short tapped one (FR-209),
// otherwise it is stopped as interrupted.
func (g *Toggle) Interrupt(t time.Time) Action {
	if !g.recording {
		return Action{}
	}
	if t.Sub(g.startedAt) < g.minLen {
		return g.end(t, Action{Kind: ActionDiscard, ID: g.id})
	}

	return g.end(t, Action{Kind: ActionStop, ID: g.id, End: EndInterrupted})
}

func (g *Toggle) debounced(t time.Time) bool {
	return g.toggled && t.Sub(g.lastToggle) < g.debounce
}

func (g *Toggle) end(t time.Time, a Action) Action {
	g.recording = false
	g.lastToggle, g.toggled = t, true
	return a
}
