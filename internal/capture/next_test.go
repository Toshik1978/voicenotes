package capture

import (
	"testing"
	"time"
)

func now0() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

func policy() Policy {
	return Policy{MaxInterval: 5 * time.Minute, Rand: func() float64 { return 0.5 }}
}

func at(step Step) Capture {
	return Capture{Kind: KindAudio, Step: step, Status: StatusPending}
}

func TestNextOK(t *testing.T) {
	order := []Step{StepDownload, StepTranscribe, StepEnrich, StepWriteNote, StepFinalizeReply, StepCleanup}
	for i, s := range order {
		c := at(s)
		c.Status, c.WaitingOn, c.Attempts, c.Misconfig = StatusWaiting, WaitingMemos, 3, true
		got := policy().Next(c, OK(), now0())
		want := StepDone
		if i+1 < len(order) {
			want = order[i+1]
		}
		if got.Step != want || got.Attempts != 0 || got.WaitingOn != "" || got.Misconfig || got.LastError != "" ||
			got.QueueReplyEdit {
			t.Errorf("%s: %+v", s, got)
		}
		wantStatus := StatusPending
		if want == StepDone {
			wantStatus = StatusDone
		}
		if got.Status != wantStatus || !got.NextAttemptAt.Equal(now0()) || !got.UpdatedAt.Equal(now0()) {
			t.Errorf("%s: status/time %+v", s, got)
		}
	}
}

func TestNextDoneIsTerminal(t *testing.T) {
	c := Capture{Step: StepDone, Status: StatusDone}
	for _, o := range []Outcome{OK(), Temporary("x"), Permanent("y")} {
		if got := policy().Next(c, o, now0()); got != c {
			t.Errorf("done changed: %+v", got)
		}
	}
}

func TestNextTemporary(t *testing.T) {
	c := at(StepTranscribe)
	got := policy().Next(c, Temporary("http 503").On(WaitingSTT), now0())
	if got.Status != StatusWaiting || got.Attempts != 1 || got.WaitingOn != WaitingSTT ||
		!got.QueueReplyEdit || got.Misconfig || got.LastError != "http 503" ||
		!got.NextAttemptAt.Equal(now0().Add(5*time.Second)) || got.Step != StepTranscribe {
		t.Errorf("first: %+v", got)
	}
	got2 := policy().Next(got, Temporary("http 503").On(WaitingSTT), now0().Add(10*time.Second))
	if got2.Attempts != 2 || got2.QueueReplyEdit || !got2.NextAttemptAt.Equal(now0().Add(20*time.Second)) {
		t.Errorf("second: %+v", got2)
	}
}

func TestNextMisconfig(t *testing.T) {
	got := policy().Next(at(StepWriteNote), Misconfig("http 403").On(WaitingMemos), now0())
	if got.Status != StatusWaiting || !got.Misconfig || got.Failure != "" || got.Step != StepWriteNote ||
		got.Attempts != 1 || !got.QueueReplyEdit || got.WaitingOn != WaitingMemos {
		t.Errorf("%+v", got)
	}
	// A later temporary clears the misconfiguration flag.
	got = policy().Next(got, Temporary("http 503").On(WaitingMemos), now0())
	if got.Misconfig {
		t.Error("misconfig flag must follow the latest outcome")
	}
}

func TestNextPermanentFallsBackToPlaceholder(t *testing.T) {
	for _, s := range []Step{StepDownload, StepTranscribe} {
		c := at(s)
		c.Attempts = 2
		got := policy().Next(c, Permanent("http 415"), now0())
		if got.Step != StepWriteNote || got.Failure != "http 415" || got.Status != StatusPending || got.Attempts != 0 {
			t.Errorf("%s: %+v", s, got)
		}
	}
}

func TestNextPermanentEnrichKeepsTranscript(t *testing.T) {
	c := at(StepEnrich)
	c.Attempts = 2
	got := policy().Next(c, Permanent("http 422"), now0())
	if got.Step != StepWriteNote || got.Failure != "" || got.LLMSkipped != SkippedInvalidOutput ||
		got.Status != StatusPending || got.Attempts != 0 {
		t.Errorf("%+v", got)
	}
}

func TestNextPermanentWriteNote(t *testing.T) {
	first := policy().Next(at(StepWriteNote), Permanent("http 400"), now0())
	if first.Step != StepWriteNote || !first.MinimalNote || first.Status != StatusPending || first.Attempts != 0 {
		t.Fatalf("first: %+v", first)
	}
	second := policy().Next(first, Permanent("http 400").On(WaitingMemos), now0())
	if second.Status != StatusWaiting || second.Attempts != 1 || second.Failure != "" || second.Step != StepWriteNote {
		t.Errorf("second must degrade to temporary: %+v", second)
	}
}

func TestNextPermanentFinalizeReply(t *testing.T) {
	got := policy().Next(at(StepFinalizeReply), Permanent("message deleted"), now0())
	if got.Step != StepCleanup || !got.SendNewReply || got.Status != StatusPending {
		t.Errorf("%+v", got)
	}
}

func TestNextPermanentCleanupFinishes(t *testing.T) {
	got := policy().Next(at(StepCleanup), Permanent("gone"), now0())
	if got.Step != StepDone || got.Status != StatusDone {
		t.Errorf("%+v", got)
	}
}

func TestNextEnrichBudget(t *testing.T) {
	p := policy()
	c := at(StepEnrich)
	c1 := p.Next(c, Temporary("http 503"), now0())
	if c1.Step != StepEnrich || c1.Status != StatusWaiting || c1.QueueReplyEdit || c1.WaitingOn != "" ||
		!c1.EnrichStartedAt.Equal(now0()) {
		t.Fatalf("1: %+v", c1)
	}
	c2 := p.Next(c1, Temporary("http 503"), now0().Add(10*time.Second))
	if c2.Step != StepEnrich || c2.Attempts != 2 {
		t.Fatalf("2: %+v", c2)
	}
	c3 := p.Next(c2, Temporary("http 503"), now0().Add(30*time.Second))
	assertSkipped(t, "third attempt", c3, now0().Add(30*time.Second))

	// time budget
	d := p.Next(c1, Misconfig("http 401"), now0().Add(2*time.Minute))
	assertSkipped(t, "two minutes", d, now0().Add(2*time.Minute))
	e := p.Next(c1, Temporary("x"), now0().Add(2*time.Minute-time.Second))
	if e.Step != StepEnrich {
		t.Errorf("under budget must keep waiting: %+v", e)
	}
}

func assertSkipped(t *testing.T, name string, c Capture, now time.Time) {
	t.Helper()
	if c.Step != StepWriteNote || c.LLMSkipped != SkippedUnavailable || c.Status != StatusPending ||
		c.Attempts != 0 || c.WaitingOn != "" || !c.EnrichStartedAt.IsZero() || c.Failure != "" || !c.NextAttemptAt.Equal(now) {
		t.Errorf("%s: %+v", name, c)
	}
}

func TestNextUnknownClassIsTemporary(t *testing.T) {
	got := policy().Next(at(StepDownload), Outcome{Class: Class(42)}, now0())
	if got.Status != StatusWaiting {
		t.Errorf("%+v", got)
	}
}

func TestNextNilRand(t *testing.T) {
	got := Policy{MaxInterval: time.Minute}.Next(at(StepDownload), Temporary("x"), now0())
	if got.Status != StatusWaiting || got.NextAttemptAt.Before(now0()) {
		t.Errorf("%+v", got)
	}
}
