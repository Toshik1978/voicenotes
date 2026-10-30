package digest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

var errDown = errors.New("down")

type fakeReader struct {
	notes []NoteSummary
	fails int // number of leading calls that fail
	calls int
}

func (f *fakeReader) ListInbox(context.Context) ([]NoteSummary, error) {
	f.calls++
	if f.calls <= f.fails {
		return nil, errDown
	}

	return f.notes, nil
}

type sent struct {
	chat int64
	text string
}

type fakeSender struct {
	mu    sync.Mutex
	got   []sent
	fails map[int]bool // 1-based attempt numbers that fail
	tries int
}

func (f *fakeSender) SendWithSound(_ context.Context, chatID int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tries++
	if f.fails[f.tries] {
		return errDown
	}
	f.got = append(f.got, sent{chatID, text})

	return nil
}

type markCall struct {
	listed, inbox []string
	date          string
}

type fakeStore struct {
	runs  []Run
	seen  map[string]string
	marks []markCall
	// readFails is how many LastDigestRun calls still fail; reads counts all.
	readFails int
	reads     int
	// recordFails is how many RecordDigestRun calls still fail; records
	// counts all attempts.
	recordFails int
	records     int
}

func (f *fakeStore) LastDigestRun(context.Context) (Run, bool, error) {
	f.reads++
	if f.readFails > 0 {
		f.readFails--
		return Run{}, false, errors.New("database is locked")
	}
	if len(f.runs) == 0 {
		return Run{}, false, nil
	}

	return f.runs[len(f.runs)-1], true, nil
}

func (f *fakeStore) RecordDigestRun(ctx context.Context, r Run) error {
	f.records++
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.recordFails > 0 {
		f.recordFails--
		return errors.New("database is locked")
	}
	f.runs = append(f.runs, r)

	return nil
}

func (f *fakeStore) SeenNotes(context.Context) (map[string]string, error) { return f.seen, nil }

func (f *fakeStore) MarkSeen(ctx context.Context, listed []string, date string, inbox []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.marks = append(f.marks, markCall{listed, inbox, date})
	return nil
}

type fakeSummarizer struct{ calls int }

func (f *fakeSummarizer) Summarize(_ context.Context, notes []NoteSummary) map[string]string {
	f.calls++
	return map[string]string{notes[0].Name: "LLM line"}
}

// harness wires a Runner to fakes and a fake clock. sleep advances the clock
// instead of waiting and cancels ctx after maxSleeps calls.
type harness struct {
	r          *Runner
	reader     *fakeReader
	sender     *fakeSender
	store      *fakeStore
	now        time.Time
	sleeps     []time.Duration
	maxSleeps  int
	cancel     context.CancelFunc
	ctx        context.Context //nolint:containedctx // test harness owns the run context
	summarizer *fakeSummarizer
}

func newHarness(t *testing.T, now time.Time) *harness {
	t.Helper()
	h := &harness{
		reader: &fakeReader{}, sender: &fakeSender{}, store: &fakeStore{seen: map[string]string{}},
		now: now, maxSleeps: 1000,
	}
	h.ctx, h.cancel = context.WithCancel(context.Background())
	t.Cleanup(h.cancel)
	r, err := New(Config{
		ChatID: -1001234567890, Time: "07:30", Loc: time.UTC, Catchup: 3 * time.Hour, MaxNotes: 30,
		RetryInterval: 10 * time.Minute, Log: zap.NewNop(),
	}, h.reader, h.store, h.sender)
	if err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return h.now }
	r.sleep = func(ctx context.Context, d time.Duration) error {
		h.sleeps = append(h.sleeps, d)
		h.now = h.now.Add(d)
		if len(h.sleeps) >= h.maxSleeps {
			h.cancel()
		}

		return ctx.Err()
	}
	h.r = r

	return h
}

func at(day, hh, mm int) time.Time { return time.Date(2026, 3, day, hh, mm, 0, 0, time.UTC) }

func inbox(names ...string) []NoteSummary {
	out := make([]NoteSummary, 0, len(names))
	for i, n := range names {
		out = append(out, NoteSummary{
			Name: "memos/" + n, URL: "https://memos.example.org/memos/" + n,
			Content: "note " + n, CreateTime: at(1, 9, i),
		})
	}

	return out
}

func TestNewValidates(t *testing.T) {
	ok := Config{ChatID: 1, Time: "07:30", Loc: time.UTC, Catchup: time.Hour, MaxNotes: 5}
	for name, mut := range map[string]func(*Config){
		"time":    func(c *Config) { c.Time = "7" },
		"loc":     func(c *Config) { c.Loc = nil },
		"chat":    func(c *Config) { c.ChatID = 0 },
		"catchup": func(c *Config) { c.Catchup = 0 },
	} {
		c := ok
		mut(&c)
		if _, err := New(c, &fakeReader{}, &fakeStore{}, &fakeSender{}); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := New(ok, &fakeReader{}, &fakeStore{}, &fakeSender{}); err != nil {
		t.Fatalf("valid config: %v", err)
	}
}

func TestRunDaySendsAndRecords(t *testing.T) {
	h := newHarness(t, at(2, 7, 30))
	h.reader.notes = inbox("a", "b")
	h.store.seen = map[string]string{"memos/a": "2026-03-01"}

	h.r.runDay(h.ctx, at(2, 7, 30))

	if len(h.sender.got) != 1 || h.sender.got[0].chat != -1001234567890 {
		t.Fatalf("sent = %+v", h.sender.got)
	}
	if !strings.Contains(h.sender.got[0].text, "1 new, 1 from earlier") {
		t.Errorf("text = %q", h.sender.got[0].text)
	}
	want := Run{LocalDate: "2026-03-02", Status: StatusSent, SentAt: at(2, 7, 30), NoteCount: 2}
	if len(h.store.runs) != 1 || h.store.runs[0] != want {
		t.Fatalf("runs = %+v, want %+v", h.store.runs, want)
	}
	m := h.store.marks
	if len(m) != 1 || m[0].date != "2026-03-02" || len(m[0].listed) != 2 || len(m[0].inbox) != 2 {
		t.Errorf("marks = %+v", m)
	}
}

func TestRunDayEmptySendsNothing(t *testing.T) {
	h := newHarness(t, at(2, 7, 30))
	h.r.runDay(h.ctx, at(2, 7, 30))

	if len(h.sender.got) != 0 {
		t.Fatalf("sent %+v, want nothing", h.sender.got)
	}
	if len(h.store.runs) != 1 || h.store.runs[0].Status != StatusEmpty {
		t.Fatalf("runs = %+v", h.store.runs)
	}
	if len(h.store.marks) != 1 || len(h.store.marks[0].inbox) != 0 {
		t.Errorf("marks = %+v, want one prune with an empty inbox", h.store.marks)
	}
}

func TestRunDayCappedNotesAreNotMarkedSeen(t *testing.T) {
	h := newHarness(t, at(2, 7, 30))
	h.r.maxNotes = 1
	h.reader.notes = inbox("a", "b", "c")
	h.r.runDay(h.ctx, at(2, 7, 30))

	m := h.store.marks[0]
	if len(m.listed) != 1 || m.listed[0] != "memos/a" || len(m.inbox) != 3 {
		t.Errorf("mark = %+v", m)
	}
	if h.store.runs[0].NoteCount != 1 {
		t.Errorf("note count = %d", h.store.runs[0].NoteCount)
	}
}

func TestRunDayRetriesUnreachableMemos(t *testing.T) {
	h := newHarness(t, at(2, 7, 30))
	h.reader.notes = inbox("a")
	h.reader.fails = 2
	h.r.runDay(h.ctx, at(2, 7, 30))

	if len(h.sleeps) != 2 || h.sleeps[0] != 10*time.Minute {
		t.Errorf("sleeps = %v", h.sleeps)
	}
	if len(h.sender.got) != 1 || h.store.runs[0].Status != StatusSent {
		t.Fatalf("sent=%d runs=%+v", len(h.sender.got), h.store.runs)
	}
}

func TestRunDaySkipsWhenMemosStaysDown(t *testing.T) {
	h := newHarness(t, at(2, 7, 30))
	h.reader.fails = 1 << 30
	h.r.runDay(h.ctx, at(2, 7, 30))

	if len(h.sender.got) != 0 || len(h.store.runs) != 1 || h.store.runs[0].Status != StatusSkipped {
		t.Fatalf("sent=%d runs=%+v", len(h.sender.got), h.store.runs)
	}
	if h.now.After(at(2, 10, 30)) {
		t.Errorf("retried past the catch-up window, now = %v", h.now)
	}
	if len(h.store.marks) != 0 {
		t.Errorf("marks = %+v, want none", h.store.marks)
	}
}

func TestRunDaySendFailureIsRetriedThenSkippedWithoutSeen(t *testing.T) {
	h := newHarness(t, at(2, 7, 30))
	h.reader.notes = inbox("a")
	h.sender.fails = map[int]bool{}
	for i := 1; i < 100; i++ {
		h.sender.fails[i] = true
	}
	h.r.runDay(h.ctx, at(2, 7, 30))

	if len(h.sender.got) != 0 || h.store.runs[0].Status != StatusSkipped {
		t.Fatalf("sent=%d runs=%+v", len(h.sender.got), h.store.runs)
	}
	if len(h.store.marks) != 0 {
		t.Errorf("seen was recorded without a send: %+v", h.store.marks)
	}
}

func TestRunDaySendFailureThenSuccess(t *testing.T) {
	h := newHarness(t, at(2, 7, 30))
	h.reader.notes = inbox("a")
	h.sender.fails = map[int]bool{1: true}
	h.r.runDay(h.ctx, at(2, 7, 30))

	if len(h.sender.got) != 1 || h.store.runs[0].Status != StatusSent || len(h.store.marks) != 1 {
		t.Fatalf("sent=%d runs=%+v marks=%+v", len(h.sender.got), h.store.runs, h.store.marks)
	}
}

func TestRunDayResumesMultiPartAtTheFailedPart(t *testing.T) {
	h := newHarness(t, at(2, 7, 30))
	h.r.maxNotes = 0
	for i := range 60 {
		n := inbox(fmt.Sprint("n", i))[0]
		n.URL += "/" + strings.Repeat("x", 100)
		h.reader.notes = append(h.reader.notes, n)
	}
	h.sender.fails = map[int]bool{2: true} // second part fails once
	h.r.runDay(h.ctx, at(2, 7, 30))

	if len(h.sender.got) < 2 {
		t.Fatalf("want a multi-part digest, got %d parts", len(h.sender.got))
	}
	if h.sender.tries != len(h.sender.got)+1 {
		t.Errorf("tries = %d for %d parts: part 1 was resent", h.sender.tries, len(h.sender.got))
	}
	if h.store.runs[0].Status != StatusSent {
		t.Errorf("runs = %+v", h.store.runs)
	}
}

func TestRunDayOncePerDay(t *testing.T) {
	h := newHarness(t, at(2, 7, 30))
	h.reader.notes = inbox("a")
	h.store.runs = []Run{{LocalDate: "2026-03-02", Status: StatusSent}}
	h.r.runDay(h.ctx, at(2, 7, 30))

	if h.reader.calls != 0 || len(h.sender.got) != 0 || len(h.store.runs) != 1 {
		t.Fatalf("second run today: calls=%d sent=%d runs=%d", h.reader.calls, len(h.sender.got), len(h.store.runs))
	}
}

func TestRunDayShutdownRecordsNothing(t *testing.T) {
	h := newHarness(t, at(2, 7, 30))
	h.reader.fails = 1 << 30
	h.maxSleeps = 1
	h.r.runDay(h.ctx, at(2, 7, 30))

	if len(h.store.runs) != 0 {
		t.Fatalf("runs = %+v: a shutdown must leave the day open for catch-up", h.store.runs)
	}
}

func TestRunDayUsesSummarizer(t *testing.T) {
	h := newHarness(t, at(2, 7, 30))
	h.summarizer = &fakeSummarizer{}
	h.r.summarizer = h.summarizer
	h.reader.notes = inbox("a")
	h.r.runDay(h.ctx, at(2, 7, 30))

	if h.summarizer.calls != 1 || !strings.Contains(h.sender.got[0].text, "LLM line") {
		t.Fatalf("calls=%d sent=%+v", h.summarizer.calls, h.sender.got)
	}
}

func TestRunCatchesUpAtStartupThenWaitsForNextSlot(t *testing.T) {
	h := newHarness(t, at(2, 8, 0))
	h.reader.notes = inbox("a")
	h.maxSleeps = 2 // wait for the slot, then cancel after the next day's run
	h.r.Run(h.ctx)

	if len(h.sender.got) != 2 {
		t.Fatalf("sent %d digests, want catch-up plus next day", len(h.sender.got))
	}
	if len(h.sleeps) < 1 || h.sleeps[0] != 23*time.Hour+30*time.Minute {
		t.Errorf("sleeps = %v, want 23h30m to the next slot first", h.sleeps)
	}
	if h.store.runs[0].LocalDate != "2026-03-02" || h.store.runs[1].LocalDate != "2026-03-03" {
		t.Errorf("runs = %+v", h.store.runs)
	}
}

func TestRunNoCatchUpBeforeTodaysSlot(t *testing.T) {
	h := newHarness(t, at(2, 6, 0))
	h.maxSleeps = 1
	h.r.Run(h.ctx)

	if len(h.sleeps) != 1 || h.sleeps[0] != 90*time.Minute {
		t.Errorf("sleeps = %v", h.sleeps)
	}
	if len(h.store.runs) != 0 || h.reader.calls != 0 {
		t.Errorf("ran early: runs=%+v calls=%d", h.store.runs, h.reader.calls)
	}
}

func TestRunNoCatchUpAfterWindow(t *testing.T) {
	h := newHarness(t, at(2, 12, 0))
	h.maxSleeps = 1
	h.r.Run(h.ctx)

	if h.reader.calls != 0 {
		t.Errorf("caught up %v after the slot, window is 3h", 4*time.Hour+30*time.Minute)
	}
}

// cancelSender cancels the run context right after a successful send, like a
// SIGTERM landing while the Bot API call is in flight.
type cancelSender struct {
	fakeSender

	cancel context.CancelFunc
}

func (c *cancelSender) SendWithSound(ctx context.Context, chatID int64, text string) error {
	err := c.fakeSender.SendWithSound(ctx, chatID, text)
	c.cancel()

	return err
}

func TestRunDayRecordsDeliveredDigestDespiteShutdown(t *testing.T) {
	h := newHarness(t, at(2, 7, 30))
	h.reader.notes = inbox("a")
	cs := &cancelSender{cancel: h.cancel}
	h.r.sender = cs
	h.r.runDay(h.ctx, at(2, 7, 30))

	if len(cs.got) != 1 {
		t.Fatalf("sent %d", len(cs.got))
	}
	if len(h.store.runs) != 1 || h.store.runs[0].Status != StatusSent || len(h.store.marks) != 1 {
		t.Fatalf("runs=%+v marks=%+v: delivered digest not recorded", h.store.runs, h.store.marks)
	}
}

func TestRunDayRetriesRecordingADeliveredDigest(t *testing.T) {
	h := newHarness(t, at(2, 7, 30))
	h.reader.notes = inbox("a")
	h.store.recordFails = 2
	cs := &cancelSender{cancel: h.cancel}
	h.r.sender = cs
	h.r.runDay(h.ctx, at(2, 7, 30))

	if len(cs.got) != 1 || len(h.store.runs) != 1 || h.store.runs[0].Status != StatusSent {
		t.Fatalf("sent=%d runs=%+v, want one digest recorded", len(cs.got), h.store.runs)
	}
	if h.store.records != 3 {
		t.Errorf("record attempts = %d, want 3", h.store.records)
	}
}

func TestRunDayUnrecordableDigestIsBoundedAndNeverResentInProcess(t *testing.T) {
	h := newHarness(t, at(2, 7, 30))
	h.reader.notes = inbox("a")
	h.store.recordFails = 1 << 30
	h.r.runDay(h.ctx, at(2, 7, 30))

	if len(h.sender.got) != 1 {
		t.Fatalf("sent %d, want 1", len(h.sender.got))
	}
	if h.store.records < 2 || h.store.records > 10 {
		t.Errorf("record attempts = %d, want a small bounded number", h.store.records)
	}
	before := h.store.records
	h.r.runDay(h.ctx, at(2, 7, 30))
	if len(h.sender.got) != 1 || h.store.records != before {
		t.Fatalf("second runDay: sent=%d records=%d->%d, want no resend", len(h.sender.got), before, h.store.records)
	}
}

func TestRunDayUnreadableLastRunNeverSendsASecondDigest(t *testing.T) {
	h := newHarness(t, at(2, 7, 30))
	h.reader.notes = inbox("a")
	h.store.runs = []Run{{LocalDate: "2026-03-02", Status: StatusSent}}
	h.store.readFails = 1 << 30
	h.r.runDay(h.ctx, at(2, 7, 30))

	if h.reader.calls != 0 || len(h.sender.got) != 0 || len(h.store.runs) != 1 {
		t.Fatalf("unreadable last run: calls=%d sent=%d runs=%d", h.reader.calls, len(h.sender.got), len(h.store.runs))
	}
	if h.now.After(at(2, 10, 30)) {
		t.Errorf("retried past the catch-up window, now = %v", h.now)
	}
	if len(h.sleeps) == 0 || h.sleeps[0] != 10*time.Minute {
		t.Errorf("sleeps = %v, want bounded retry pauses", h.sleeps)
	}
}

func TestRunDayTransientReadErrorStillSkipsADaySentAlready(t *testing.T) {
	h := newHarness(t, at(2, 7, 30))
	h.reader.notes = inbox("a")
	h.store.runs = []Run{{LocalDate: "2026-03-02", Status: StatusSent}}
	h.store.readFails = 2
	h.r.runDay(h.ctx, at(2, 7, 30))

	if len(h.sender.got) != 0 || len(h.store.runs) != 1 {
		t.Fatalf("sent=%d runs=%d, want no second digest", len(h.sender.got), len(h.store.runs))
	}
}

func TestRunDayTransientReadErrorThenNoRunSends(t *testing.T) {
	h := newHarness(t, at(2, 7, 30))
	h.reader.notes = inbox("a")
	h.store.readFails = 1
	h.r.runDay(h.ctx, at(2, 7, 30))

	if len(h.sender.got) != 1 || len(h.store.runs) != 1 || h.store.runs[0].Status != StatusSent {
		t.Fatalf("sent=%d runs=%+v", len(h.sender.got), h.store.runs)
	}
}

func TestRunDayUnreadableLastRunStopsOnShutdown(t *testing.T) {
	h := newHarness(t, at(2, 7, 30))
	h.store.readFails = 1 << 30
	h.maxSleeps = 1
	h.r.runDay(h.ctx, at(2, 7, 30))

	if len(h.sleeps) != 1 || len(h.sender.got) != 0 || len(h.store.runs) != 0 {
		t.Fatalf("sleeps=%v sent=%d runs=%d", h.sleeps, len(h.sender.got), len(h.store.runs))
	}
}

func TestRunCatchUpWithUnreadableLastRunNeverResends(t *testing.T) {
	h := newHarness(t, at(2, 8, 0))
	h.reader.notes = inbox("a")
	h.store.runs = []Run{{LocalDate: "2026-03-02", Status: StatusSent}}
	h.store.readFails = 1 << 30
	h.maxSleeps = 30
	h.r.Run(h.ctx)

	if len(h.sender.got) != 0 || len(h.store.runs) != 1 {
		t.Fatalf(
			"sent=%d runs=%d, want no digest while the last run is unreadable",
			len(h.sender.got),
			len(h.store.runs),
		)
	}
}
