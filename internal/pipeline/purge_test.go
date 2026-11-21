package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

type fakePurger struct {
	mu     sync.Mutex
	cutoff []time.Time
	err    error
}

func (f *fakePurger) PurgeDone(_ context.Context, olderThan time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cutoff = append(f.cutoff, olderThan)
	if f.err != nil {
		return 0, f.err
	}

	return 1, nil
}

func (f *fakePurger) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.cutoff)
}

func runPurge(t *testing.T, p *fakePurger, now time.Time, loop ...pipeline.Purger) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	var target pipeline.Purger = p
	if len(loop) > 0 {
		target = loop[0]
	}
	go func() {
		pipeline.PurgeLoop(ctx, target, 5*time.Millisecond, func() time.Time { return now }, nil)
		close(finished)
	}()
	deadline := time.After(2 * time.Second)
	for p.count() < 3 {
		select {
		case <-deadline:
			t.Fatalf("only %d purges", p.count())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-finished
}

func TestPurgeLoopRunsAtStartAndOnTick(t *testing.T) {
	p := &fakePurger{}
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	runPurge(t, p, now)

	if got, want := p.cutoff[0], now.Add(-30*24*time.Hour); !got.Equal(want) {
		t.Errorf("cutoff = %v, want %v", got, want)
	}
	if pipeline.DoneRetention != 30*24*time.Hour {
		t.Errorf("retention = %v, want 30 days", pipeline.DoneRetention)
	}
	if pipeline.PurgeInterval != 24*time.Hour {
		t.Errorf("interval = %v, want 24h", pipeline.PurgeInterval)
	}
}

func TestPurgeLoopSurvivesErrors(t *testing.T) {
	p := &fakePurger{err: errors.New("disk full")}
	runPurge(t, p, time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC))
}

type fakeReplyStore struct {
	stale   []pipeline.StartReply
	listErr error
	takeErr error
	gone    map[int64]bool // taken by someone else between list and take
	cutoff  time.Time
	taken   []int64
	order   *[]string // records "take <id>"; shared with a fakeReplier's log
}

func (f *fakeReplyStore) StaleStartReplies(_ context.Context, olderThan time.Time) ([]pipeline.StartReply, error) {
	f.cutoff = olderThan

	return f.stale, f.listErr
}

func (f *fakeReplyStore) TakeStartReply( //nolint:revive // the four results are the specified signature
	_ context.Context, id int64,
) (int64, int64, bool, error) {
	f.taken = append(f.taken, id)
	if f.order != nil {
		*f.order = append(*f.order, fmt.Sprintf("take %d", id))
	}
	if f.takeErr != nil || f.gone[id] {
		return 0, 0, false, f.takeErr
	}
	for _, r := range f.stale {
		if r.RecordingID == id {
			return r.ChatID, r.MessageID, true, nil
		}
	}

	return 0, 0, false, nil
}

// orderedReplier logs each edit into the shared order.
type orderedReplier struct {
	fakeReplier

	order *[]string
}

func (o *orderedReplier) Edit(ctx context.Context, chatID, msgID int64, text string) (int64, error) {
	*o.order = append(*o.order, fmt.Sprintf("edit %d", msgID))

	return o.fakeReplier.Edit(ctx, chatID, msgID, text)
}

// The janitor takes the row first and edits only what it took, so an upload
// that took the row between the listing and the edit keeps its outcome.
func TestStartReplyJanitorTakesBeforeEditing(t *testing.T) {
	var order []string
	st := &fakeReplyStore{
		stale: []pipeline.StartReply{
			{RecordingID: 1, ChatID: -1001234567890, MessageID: 10},
			{RecordingID: 2, ChatID: -1001234567890, MessageID: 20},
		},
		gone:  map[int64]bool{1: true},
		order: &order,
	}
	rp := &orderedReplier{order: &order}

	n, err := pipeline.StartReplyJanitor{Store: st, Replier: rp}.PurgeStartReplies(context.Background(), time.Now())
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if want := []string{"take 1", "take 2", "edit 20"}; !slices.Equal(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

func TestStartReplyJanitor(t *testing.T) {
	st := &fakeReplyStore{stale: []pipeline.StartReply{
		{RecordingID: 1, ChatID: -1001234567890, MessageID: 10},
		{RecordingID: 2, ChatID: -1001234567890, MessageID: 20},
	}}
	rp := &fakeReplier{}
	cutoff := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)

	n, err := pipeline.StartReplyJanitor{Store: st, Replier: rp}.PurgeStartReplies(context.Background(), cutoff)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if !st.cutoff.Equal(cutoff) {
		t.Errorf("cutoff = %v", st.cutoff)
	}
	if len(rp.edits) != 2 || rp.edits[0].msg != 10 || rp.edits[1].msg != 20 ||
		rp.edits[0].text != "⚠ recording not received" {
		t.Errorf("edits = %+v", rp.edits)
	}
	if len(st.taken) != 2 || st.taken[0] != 1 || st.taken[1] != 2 {
		t.Errorf("taken = %v", st.taken)
	}
}

func TestStartReplyJanitorEditErrorStillRemovesRow(t *testing.T) {
	st := &fakeReplyStore{stale: []pipeline.StartReply{{RecordingID: 1, ChatID: 5, MessageID: 10}}}
	rp := &fakeReplier{editErr: errors.New("message gone")}

	n, err := pipeline.StartReplyJanitor{Store: st, Replier: rp}.PurgeStartReplies(context.Background(), time.Now())
	if err != nil || n != 1 || len(st.taken) != 1 {
		t.Fatalf("n=%d err=%v taken=%v", n, err, st.taken)
	}
}

func TestStartReplyJanitorStoreErrors(t *testing.T) {
	boom := errors.New("boom")
	j := pipeline.StartReplyJanitor{Store: &fakeReplyStore{listErr: boom}, Replier: &fakeReplier{}}
	if _, err := j.PurgeStartReplies(context.Background(), time.Now()); !errors.Is(err, boom) {
		t.Errorf("list err = %v", err)
	}
	st := &fakeReplyStore{stale: []pipeline.StartReply{{RecordingID: 1}}, takeErr: boom}
	j = pipeline.StartReplyJanitor{Store: st, Replier: &fakeReplier{}}
	if _, err := j.PurgeStartReplies(context.Background(), time.Now()); !errors.Is(err, boom) {
		t.Errorf("take err = %v", err)
	}
}

func TestStartReplyJanitorCancelledEditStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st := &fakeReplyStore{stale: []pipeline.StartReply{{RecordingID: 1}}}
	rp := &fakeReplier{editErr: context.Canceled}
	n, err := pipeline.StartReplyJanitor{Store: st, Replier: rp}.PurgeStartReplies(ctx, time.Now())
	if !errors.Is(err, context.Canceled) || n != 0 || len(st.taken) != 0 {
		t.Fatalf("n=%d err=%v taken=%v", n, err, st.taken)
	}
}

type replyingPurger struct {
	fakePurger

	cutoffs []time.Time
	err     error
}

func (r *replyingPurger) PurgeStartReplies(_ context.Context, olderThan time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cutoffs = append(r.cutoffs, olderThan)

	return 1, r.err
}

func runReplyLoop(t *testing.T, rp pipeline.ReplyPurger, now time.Time, seen func() int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		pipeline.ReplyLoop(ctx, rp, 5*time.Millisecond, func() time.Time { return now }, nil)
		close(finished)
	}()
	deadline := time.After(2 * time.Second)
	for seen() < 3 {
		select {
		case <-deadline:
			t.Fatalf("only %d passes", seen())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-finished
}

func (r *replyingPurger) passes() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.cutoffs)
}

func TestReplyLoopPurgesAtStartAndOnTick(t *testing.T) {
	p := &replyingPurger{}
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	runReplyLoop(t, p, now, p.passes)

	p.mu.Lock()
	defer p.mu.Unlock()
	if got, want := p.cutoffs[0], now.Add(-24*time.Hour); !got.Equal(want) {
		t.Errorf("cutoff = %v, want %v", got, want)
	}
	if pipeline.StartReplyRetention != 24*time.Hour {
		t.Errorf("retention = %v", pipeline.StartReplyRetention)
	}
	if pipeline.StartReplyInterval != time.Hour {
		t.Errorf("interval = %v, want 1h", pipeline.StartReplyInterval)
	}
}

func TestReplyLoopSurvivesErrors(t *testing.T) {
	p := &replyingPurger{err: errors.New("disk full")}
	runReplyLoop(t, p, time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC), p.passes)
}

func TestPurgeLoopLeavesStartRepliesToReplyLoop(t *testing.T) {
	p := &replyingPurger{}
	runPurge(t, &p.fakePurger, time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC), p)
	if n := p.passes(); n != 0 {
		t.Fatalf("PurgeLoop purged start replies %d times; ReplyLoop owns them", n)
	}
}

func TestExpiredHeld(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	hold := 168 * time.Hour
	files := []pipeline.StoredFile{
		{Name: "1.wav", ModTime: now.Add(-hold - time.Second), Regular: true},
		{Name: "2.wav", ModTime: now.Add(-hold), Regular: true},
		{Name: "3.wav", ModTime: now.Add(-time.Hour), Regular: true},
		{Name: "sub", ModTime: now.Add(-2 * hold), Regular: false},
		{Name: "4.wav", ModTime: now.Add(-2 * hold), Regular: true},
	}
	got := pipeline.ExpiredHeld(files, now, hold)
	if len(got) != 2 || got[0] != "1.wav" || got[1] != "4.wav" {
		t.Fatalf("expired = %v, want [1.wav 4.wav]", got)
	}
}

// heldWorld is a DATA_DIR with files/ and nospeech/, each holding a file
// older than the hold and one younger.
func heldWorld(t *testing.T, now time.Time, hold time.Duration) (filesDir, heldDir string) {
	t.Helper()
	dataDir := t.TempDir()
	filesDir = filepath.Join(dataDir, "files")
	heldDir = filepath.Join(dataDir, pipeline.NoSpeechDir)
	for _, d := range []string{filesDir, heldDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, age := range map[string]time.Duration{"old.wav": hold + time.Minute, "new.wav": hold - time.Minute} {
			p := filepath.Join(d, name)
			if err := os.WriteFile(p, []byte("RIFF"), 0o600); err != nil {
				t.Fatal(err)
			}
			mt := now.Add(-age)
			if err := os.Chtimes(p, mt, mt); err != nil {
				t.Fatal(err)
			}
		}
	}

	return filesDir, heldDir
}

func exists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Stat(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	return err == nil
}

func TestHeldAudioPurgesOnlyExpiredHeldFiles(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	hold := 168 * time.Hour
	filesDir, heldDir := heldWorld(t, now, hold)

	n, err := pipeline.HeldAudio{Dir: heldDir, Hold: hold}.PurgeHeld(context.Background(), now)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v, want 1 removed", n, err)
	}
	if exists(t, filepath.Join(heldDir, "old.wav")) {
		t.Error("expired held audio survived")
	}
	if !exists(t, filepath.Join(heldDir, "new.wav")) {
		t.Error("held audio inside the hold was deleted")
	}
	for _, name := range []string{"old.wav", "new.wav"} {
		if !exists(t, filepath.Join(filesDir, name)) {
			t.Errorf("files/%s touched by the held purge", name)
		}
	}
}

func TestHeldAudioMissingDirIsEmpty(t *testing.T) {
	h := pipeline.HeldAudio{Dir: filepath.Join(t.TempDir(), "nospeech"), Hold: time.Hour}
	if n, err := h.PurgeHeld(context.Background(), time.Now()); err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestHeldAudioCancelledStops(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	_, heldDir := heldWorld(t, now, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n, err := pipeline.HeldAudio{Dir: heldDir, Hold: time.Hour}.PurgeHeld(ctx, now)
	if !errors.Is(err, context.Canceled) || n != 0 || !exists(t, filepath.Join(heldDir, "old.wav")) {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestOrphanSweepLeavesHeldAudio(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	filesDir, heldDir := heldWorld(t, now, 2*time.Hour)

	if _, err := pipeline.SweepOrphanFiles(context.Background(), filesDir, fakeRefs{}, now, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"old.wav", "new.wav"} {
		if !exists(t, filepath.Join(heldDir, name)) {
			t.Errorf("orphan sweep removed nospeech/%s", name)
		}
	}
}

type heldPurger struct {
	fakePurger

	nows []time.Time
	err  error
}

func (h *heldPurger) PurgeHeld(_ context.Context, now time.Time) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nows = append(h.nows, now)

	return 1, h.err
}

func TestPurgeLoopPurgesHeldAudio(t *testing.T) {
	p := &heldPurger{}
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	runPurge(t, &p.fakePurger, now, p)

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.nows) == 0 {
		t.Fatal("held audio never purged")
	}
	if !p.nows[0].Equal(now) {
		t.Errorf("now = %v, want %v", p.nows[0], now)
	}
}

func TestPurgeLoopSurvivesHeldAudioErrors(t *testing.T) {
	p := &heldPurger{err: errors.New("disk full")}
	runPurge(t, &p.fakePurger, time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC), p)
}

type forgettingPurger struct {
	fakePurger

	forgetCutoffs []time.Time
	names         []string
	forgetErr     error
}

func (f *forgettingPurger) ForgetRecorders(_ context.Context, notSeenSince time.Time) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgetCutoffs = append(f.forgetCutoffs, notSeenSince)
	names := f.names
	f.names = nil

	return names, f.forgetErr
}

// PurgeLoop forgets recorders not heard from for RecorderRetention and logs
// each forgotten name, so a renamed or retired recorder stops degrading health.
func TestPurgeLoopForgetsRetiredRecorders(t *testing.T) {
	p := &forgettingPurger{names: []string{"old-name", "spare"}}
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	core, logs := observer.New(zapcore.InfoLevel)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		pipeline.PurgeLoop(ctx, p, time.Hour, func() time.Time { return now }, zap.New(core))
		close(finished)
	}()
	deadline := time.After(2 * time.Second)
	for logs.FilterMessage("recorder forgotten").Len() < 2 {
		select {
		case <-deadline:
			t.Fatal("forgotten recorders not logged")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-finished

	if pipeline.RecorderRetention != 30*24*time.Hour {
		t.Errorf("retention = %v, want 30 days", pipeline.RecorderRetention)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.forgetCutoffs) == 0 || !p.forgetCutoffs[0].Equal(now.Add(-30*24*time.Hour)) {
		t.Errorf("cutoffs = %v", p.forgetCutoffs)
	}
	entries := logs.FilterMessage("recorder forgotten").All()
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		name, _ := e.ContextMap()["recorder"].(string)
		got = append(got, name)
	}
	if !slices.Equal(got, []string{"old-name", "spare"}) {
		t.Errorf("logged = %v", got)
	}
}

func TestPurgeLoopSurvivesForgetErrors(t *testing.T) {
	p := &forgettingPurger{forgetErr: errors.New("disk full")}
	runPurge(t, &p.fakePurger, time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC), p)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.forgetCutoffs) == 0 {
		t.Fatal("recorders never forgotten")
	}
}
