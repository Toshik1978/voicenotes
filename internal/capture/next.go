package capture

import "time"

// LLM budget (data-model.md): after 3 attempts or 2 minutes the enrich step
// is skipped with llm_skipped='unavailable'.
const (
	llmMaxAttempts = 3
	llmMaxDuration = 2 * time.Minute
)

// Policy carries the injected configuration of the state machine.
type Policy struct {
	// MaxInterval caps the backoff (RETRY_MAX_INTERVAL).
	MaxInterval time.Duration
	// Rand returns values in [0,1) for backoff jitter; nil disables jitter.
	Rand func() float64
}

// Next applies the outcome of one attempt at c.Step and returns the updated
// capture, following the transitions in data-model.md "Steps". It is pure:
// c is not modified and now is the only clock.
func (p Policy) Next(c Capture, o Outcome, now time.Time) Capture {
	if c.Step == StepDone {
		return c
	}
	c.UpdatedAt = now
	c.QueueReplyEdit = false

	switch o.Class {
	case ClassOK:
		return advance(c, now)
	case ClassPermanent:
		return p.permanent(c, o, now)
	case ClassMisconfig:
		return p.retry(c, o, now, true)
	case ClassTemporary:
		return p.retry(c, o, now, false)
	}

	// Unknown class: never drop a capture.
	return p.retry(c, o, now, false)
}

// advance moves to the following step and clears all retry state.
func advance(c Capture, now time.Time) Capture {
	c.Step = c.Step.next()
	c.Status = StatusPending
	if c.Step == StepDone {
		c.Status = StatusDone
	}
	c.Attempts = 0
	c.WaitingOn = ""
	c.Misconfig = false
	c.LastError = ""
	c.SendNewReply = false
	c.EnrichStartedAt = time.Time{}
	c.NextAttemptAt = now

	return c
}

func (p Policy) permanent(c Capture, o Outcome, now time.Time) Capture {
	switch c.Step {
	case StepEnrich:
		// The LLM is optional (FR-018): keep the transcript, no placeholder.
		c = advanceTo(c, StepWriteNote, now)
		c.LLMSkipped = SkippedInvalidOutput

		return c
	case StepDownload, StepTranscribe:
		// Placeholder note; the capture is kept.
		c = advanceTo(c, StepWriteNote, now)
		c.Failure = o.Reason
		return c
	case StepWriteNote:
		if !c.MinimalNote {
			c.MinimalNote = true // retry once with a minimal note
			c.Status = StatusPending
			c.Attempts = 0
			c.WaitingOn = ""
			c.Misconfig = false
			c.NextAttemptAt = now

			return c
		}

		return p.retry(c, o, now, false)
	case StepFinalizeReply:
		// The adapter sends a new silent reply, then the step counts as ok.
		c = advance(c, now)
		c.SendNewReply = true
		return c
	case StepCleanup, StepDone:
		// Nothing more can be done: do not hold the row open.
		return advance(c, now)
	}

	return advance(c, now)
}

func advanceTo(c Capture, step Step, now time.Time) Capture {
	c = advance(c, now)
	c.Step = step
	c.Status = StatusPending
	return c
}

// retry moves the capture into (or keeps it in) waiting with a backoff.
func (p Policy) retry(c Capture, o Outcome, now time.Time, misconfig bool) Capture {
	attempts := c.Attempts + 1

	if c.Step == StepEnrich {
		if c.EnrichStartedAt.IsZero() {
			c.EnrichStartedAt = now
		}
		if attempts >= llmMaxAttempts || now.Sub(c.EnrichStartedAt) >= llmMaxDuration {
			c = advance(c, now)
			c.LLMSkipped = SkippedUnavailable
			return c
		}
	}

	// The LLM step is invisible to the user: no "queued" edit, no waiting_on.
	userVisible := c.Step != StepEnrich
	c.QueueReplyEdit = userVisible && c.Status != StatusWaiting
	c.Status = StatusWaiting
	c.Attempts = attempts
	c.WaitingOn = ""
	if userVisible {
		c.WaitingOn = o.WaitingOn
	}
	c.Misconfig = misconfig
	c.LastError = o.Reason
	c.NextAttemptAt = now.Add(Backoff(attempts, p.MaxInterval, p.Rand))

	return c
}
