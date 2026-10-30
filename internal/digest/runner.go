package digest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// defaultRetryInterval is the pause between attempts while Memos or Telegram
// is unreachable (inside the DIGEST_CATCHUP window).
const defaultRetryInterval = 5 * time.Minute

// finishTimeout bounds the bookkeeping writes after a send.
const finishTimeout = 30 * time.Second

// Recording a delivered digest is retried with doubling pauses, at most
// recordAttempts times, all inside finishTimeout.
const (
	recordAttempts    = 5
	recordBackoffBase = time.Second
)

// Store is the digest bookkeeping the runner needs; *sqlite.DB implements it.
type Store interface {
	// LastDigestRun returns the run with the latest local date.
	LastDigestRun(ctx context.Context) (r Run, ok bool, err error)
	// RecordDigestRun stores the outcome of one local day.
	RecordDigestRun(ctx context.Context, r Run) error
	// SeenNotes returns note name -> local date it was first shown.
	SeenNotes(ctx context.Context) (map[string]string, error)
	// MarkSeen records the listed notes and forgets those not in inbox.
	MarkSeen(ctx context.Context, listed []string, localDate string, inbox []string) error
}

// Sender delivers one digest message to a chat WITH a notification sound
// (the only message the bot sends that way); *telegram.API implements it.
type Sender interface {
	SendWithSound(ctx context.Context, chatID int64, text string) error
}

// Summarizer produces short LLM summaries keyed by note name. It is optional
// (the LLM digest summary, US4); it must not fail the digest, so a note it
// cannot summarize is simply absent from the map.
type Summarizer interface {
	Summarize(ctx context.Context, notes []NoteSummary) map[string]string
}

// Config configures the Runner. The values are validated by internal/config.
type Config struct {
	// ChatID is DIGEST_CHAT_ID.
	ChatID int64
	// Time is DIGEST_TIME, "HH:MM" in Loc.
	Time string
	// Loc is the configured time zone (TZ).
	Loc *time.Location
	// Catchup is DIGEST_CATCHUP: how long after the slot a missed digest is
	// still attempted.
	Catchup time.Duration
	// MaxNotes is DIGEST_MAX_NOTES.
	MaxNotes int
	// RetryInterval is the pause between attempts (default 5m).
	RetryInterval time.Duration
	Log           *zap.Logger
}

// Runner sends the morning digest once per local day (FR-027..FR-031).
type Runner struct {
	cfg        Config
	notes      NotesReader
	store      Store
	sender     Sender
	summarizer Summarizer
	maxNotes   int
	retryEvery time.Duration
	log        *zap.Logger

	// handled is the local date this process already finished (sent, empty or
	// skipped), set even when recording it failed, so it never repeats a day.
	handled string

	// now and sleep are replaced by tests; sleep returns ctx.Err() when ctx
	// ends first.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// New validates cfg and builds a Runner.
func New(cfg Config, notes NotesReader, store Store, sender Sender) (*Runner, error) {
	if _, _, ok := parseHHMM(cfg.Time); !ok {
		return nil, errors.New("digest: time must be HH:MM")
	}
	if cfg.Loc == nil {
		return nil, errors.New("digest: nil time zone")
	}
	if cfg.ChatID == 0 {
		return nil, errors.New("digest: no chat")
	}
	if cfg.Catchup <= 0 {
		return nil, errors.New("digest: catch-up window must be positive")
	}
	log := cfg.Log
	if log == nil {
		log = zap.NewNop()
	}
	retry := cfg.RetryInterval
	if retry <= 0 {
		retry = defaultRetryInterval
	}

	return &Runner{
		cfg: cfg, notes: notes, store: store, sender: sender,
		maxNotes: cfg.MaxNotes, retryEvery: retry, log: log,
		now: time.Now, sleep: sleepCtx,
	}, nil
}

// SetSummarizer enables LLM line summaries (nil disables them).
func (r *Runner) SetSummarizer(s Summarizer) { r.summarizer = s }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("digest: wait: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}

// Run catches up a digest missed today, then fires at each daily slot until
// ctx is cancelled. A digest in progress at shutdown records nothing, so the
// next start catches it up (if still inside the window).
func (r *Runner) Run(ctx context.Context) {
	now := r.now()
	// An unreadable last run is not "no run": it only lets runDay decide,
	// because runDay re-reads it with retries and sends nothing while unknown.
	last, err := r.lastRunDate(ctx)
	if err != nil {
		last = ""
	}
	if ShouldCatchUp(now, r.cfg.Time, r.cfg.Loc, r.cfg.Catchup, last) {
		r.runDay(ctx, r.slotFor(now))
	}
	for ctx.Err() == nil {
		now = r.now()
		next := NextRun(now, r.cfg.Time, r.cfg.Loc)
		if r.sleep(ctx, next.Sub(now)) != nil {
			return
		}
		r.runDay(ctx, next)
	}
}

// slotFor returns the digest instant of now's local day.
func (r *Runner) slotFor(now time.Time) time.Time {
	h, m, _ := parseHHMM(r.cfg.Time)
	y, mo, d := now.In(r.cfg.Loc).Date()

	return slotOn(y, mo, d, h, m, r.cfg.Loc)
}

// dayDone reports whether date was already handled: recorded as last, or
// finished by this process even though recording it failed.
func (r *Runner) dayDone(date, last string) bool { return last == date || r.handled == date }

// lastRunDate returns the newest recorded local date, "" when none. A read
// error is returned, never mapped to "" (that would mean "no run yet").
func (r *Runner) lastRunDate(ctx context.Context) (string, error) {
	last, ok, err := r.store.LastDigestRun(ctx)
	if err != nil {
		return "", fmt.Errorf("read last digest run: %w", err)
	}
	if !ok {
		return "", nil
	}

	return last.LocalDate, nil
}

// runDay produces the digest of slot's local day: at most one outcome is
// recorded, and none when ctx ends first.
func (r *Runner) runDay(ctx context.Context, slot time.Time) {
	date := slot.In(r.cfg.Loc).Format(dateLayout)
	deadline := slot.Add(r.cfg.Catchup)

	// FR-027: if today's outcome cannot be read, wait and retry within the
	// catch-up window; at its end (or shutdown) give up without sending.
	var last string
	lerr := r.retry(ctx, deadline, func() error {
		var rerr error
		last, rerr = r.lastRunDate(ctx)

		return rerr
	})
	if lerr != nil {
		r.log.Warn("digest not sent: last run unreadable", zap.String("date", date), zap.Error(lerr))
		return
	}
	if r.dayDone(date, last) {
		return
	}

	var inbox, listed []NoteSummary
	var parts []string
	err := r.retry(ctx, deadline, func() error {
		var err error
		if inbox, err = r.notes.ListInbox(ctx); err != nil {
			return fmt.Errorf("list inbox: %w", err)
		}

		return nil
	})
	if err == nil {
		seen, serr := r.store.SeenNotes(ctx)
		if serr != nil {
			r.log.Warn("digest: read seen notes failed", zap.Error(serr))
		}
		listed = listedNotes(inbox, seenSet(seen), r.maxNotes)
		text, ok := Compose(inbox, seenSet(seen), r.summaries(ctx, listed), r.maxNotes)
		if !ok {
			r.finish(ctx, date, StatusEmpty, nil, nil)
			return
		}
		parts = SplitMessage(text, TelegramMaxUnits)
		err = r.sendParts(ctx, deadline, parts)
	}
	switch {
	case err == nil:
		// Delivered: record it even if shutdown began meanwhile, or a
		// restart inside the window would send the digest twice.
		r.finish(ctx, date, StatusSent, listed, inbox)
	case ctx.Err() != nil:
		return
	default:
		r.log.Warn("digest skipped: dependency stayed unreachable", zap.String("date", date), zap.Error(err))
		r.finish(ctx, date, StatusSkipped, nil, nil)
	}
}

// sendParts sends the parts in order, every one with sound (a split digest is
// rare and a quiet part would be easy to miss). A failed part is retried
// without resending the parts already delivered.
func (r *Runner) sendParts(ctx context.Context, deadline time.Time, parts []string) error {
	next := 0

	return r.retry(ctx, deadline, func() error {
		for next < len(parts) {
			if err := r.sender.SendWithSound(ctx, r.cfg.ChatID, parts[next]); err != nil {
				return fmt.Errorf("send digest: %w", err)
			}
			next++
		}

		return nil
	})
}

// summaries asks the optional summarizer about the notes that will be listed.
func (r *Runner) summaries(ctx context.Context, listed []NoteSummary) map[string]string {
	if r.summarizer == nil || len(listed) == 0 {
		return nil
	}

	return r.summarizer.Summarize(ctx, listed)
}

// finish records the day's outcome. For a sent or empty digest it also
// updates digest_seen: only the notes actually listed are marked, and notes
// gone from the inbox are forgotten. Errors are logged; the message (if any)
// is already out, so there is nothing left to roll back.
func (r *Runner) finish(ctx context.Context, date string, status RunStatus, listed, inbox []NoteSummary) {
	// Bookkeeping must survive a shutdown that cancelled ctx after the send.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if status != StatusSkipped {
		if err := r.store.MarkSeen(ctx, names(listed), date, names(inbox)); err != nil {
			r.log.Warn("digest: record seen notes failed", zap.Error(err))
		}
	}
	run := Run{LocalDate: date, Status: status, SentAt: r.now(), NoteCount: len(listed)}
	// Whatever happens to the write, this process must not run the day again.
	r.handled = date
	if err := r.recordRun(ctx, run); err != nil {
		r.log.Error("digest: record run failed", zap.String("date", date), zap.Error(err))
		return
	}
	r.log.Info("digest done", zap.String("date", date), zap.String("status", string(status)),
		zap.Int("notes", len(listed)))
}

// recordRun stores run, retrying with doubling pauses (bounded by
// recordAttempts and ctx's timeout) so a transient failure after a delivered
// digest does not allow a second one after a restart (FR-027).
func (r *Runner) recordRun(ctx context.Context, run Run) error {
	pause := recordBackoffBase
	var err error
	for attempt := 1; ; attempt++ {
		if err = r.store.RecordDigestRun(ctx, run); err == nil {
			return nil
		}
		if attempt >= recordAttempts {
			break
		}
		r.log.Warn("digest: record run failed, retrying", zap.Int("attempt", attempt),
			zap.Duration("in", pause), zap.Error(err))
		if serr := r.sleep(ctx, pause); serr != nil {
			return errors.Join(err, serr)
		}
		pause *= 2
	}

	return fmt.Errorf("record digest run: %w", err)
}

// retry runs op until it succeeds, pausing retryEvery between attempts, and
// gives up once another pause would pass deadline or ctx ends.
func (r *Runner) retry(ctx context.Context, deadline time.Time, op func() error) error {
	for {
		err := op()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || r.now().Add(r.retryEvery).After(deadline) {
			return err
		}
		r.log.Warn("digest attempt failed, retrying", zap.Duration("in", r.retryEvery), zap.Error(err))
		if serr := r.sleep(ctx, r.retryEvery); serr != nil {
			return serr
		}
	}
}

func seenSet(seen map[string]string) map[string]bool {
	set := make(map[string]bool, len(seen))
	for name := range seen {
		set[name] = true
	}

	return set
}

func names(notes []NoteSummary) []string {
	out := make([]string, 0, len(notes))
	for _, n := range notes {
		out = append(out, n.Name)
	}

	return out
}
