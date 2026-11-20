package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/voicenotes/internal/mic"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
	micrec "github.com/Toshik1978/voicenotes/internal/recorder"
	"github.com/Toshik1978/voicenotes/internal/store/sqlite"
)

// End-to-end direct mode (US1, acceptance scenarios 1.1-1.6): a real
// recorder with fake keys, audio and clock hands recordings over HTTP to the
// real service mux, whose pipeline talks to fake speech-to-text, LLM, Memos
// and Telegram.

const (
	micNotifyChat   = int64(-1001234567890)
	micInputPath    = "/dev/input/event7"
	micRecorderName = "bedside"
	micDebounce     = 500 * time.Millisecond
	micMinLength    = 2 * time.Second
	micMaxLength    = 10 * time.Minute
	micSaid         = "Remember to call the plumber about the boiler on Monday."
	micTask         = "Call the plumber about the boiler on Monday"
	// micAudioBytes is one second of s16le 16 kHz mono audio.
	micAudioBytes = mic.SampleRate * mic.FrameSize
)

// micFirstTap is when every scenario's first tap happens: far ahead of the
// real clock the worker runs on, like a recorder clock that runs fast. A
// capture must still be due at once, not at its start time (vn-c82.8).
func micFirstTap() time.Time { return time.Date(2099, 11, 19, 23, 14, 5, 250000000, time.UTC) }

// micAudioFixture is the audio the fake capture delivers per recording.
func micAudioFixture() []byte {
	b := make([]byte, micAudioBytes)
	for i := range b {
		b[i] = byte(i * 7)
	}

	return b
}

// ---------------------------------------------------------------- fakes

// micClock is a manual recorder.Clock: time moves only by set, After fires
// when set reaches its deadline and tickers fire only through tick.
type micClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []micWaiter
	tickers map[time.Duration][]chan time.Time
}

type micWaiter struct {
	at time.Time
	ch chan time.Time
}

func newMicClock(now time.Time) *micClock {
	return &micClock{now: now, tickers: map[time.Duration][]chan time.Time{}}
}

func (c *micClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *micClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, micWaiter{at: c.now.Add(d), ch: ch})

	return ch
}

func (c *micClock) NewTicker(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.tickers[d] = append(c.tickers[d], ch)

	return ch, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.tickers[d] = slices.DeleteFunc(c.tickers[d], func(x chan time.Time) bool { return x == ch })
	}
}

// set moves the clock to t and fires the After channels now due.
func (c *micClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
	c.waiters = slices.DeleteFunc(c.waiters, func(w micWaiter) bool {
		if w.at.After(t) {
			return false
		}

		w.ch <- t

		return true
	})
}

// hasTicker reports whether a ticker with period d is running.
func (c *micClock) hasTicker(d time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.tickers[d]) > 0
}

// tick fires every ticker with period d once.
func (c *micClock) tick(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ch := range c.tickers[d] {
		select {
		case ch <- c.now:
		default:
		}
	}
}

// micKeys is the receiver's key input. It records every open and whether
// the input was grabbed (FR-202: the host never sees the volume key).
type micKeys struct {
	events chan micrec.KeyEvent

	mu      sync.Mutex
	grabbed []string
}

func (k *micKeys) Open(path string) (micrec.KeyStream, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.grabbed = append(k.grabbed, path)

	return micrec.KeyStream{Events: k.events, Close: func() error { return nil }, Grabbed: true}, nil
}

func (k *micKeys) grabs() []string {
	k.mu.Lock()
	defer k.mu.Unlock()

	return slices.Clone(k.grabbed)
}

// micAudio delivers the fixture into the frame queue as 10 ms chunks the
// moment capture starts.
type micAudio struct{}

func (micAudio) Open(_ mic.Receiver, q *micrec.FrameQueue) (micrec.AudioStream, error) {
	audio := micAudioFixture()
	const chunk = mic.SampleRate / 100 * mic.FrameSize
	for off := 0; off < len(audio); off += chunk {
		if !q.Offer(slices.Clone(audio[off : off+chunk])) {
			return micrec.AudioStream{}, errors.New("frame queue full")
		}
	}

	return micrec.AudioStream{Close: func() error { return nil }}, nil
}

// micDiscovery always finds the receiver.
type micDiscovery struct{}

func (micDiscovery) Discover() (mic.Receiver, bool, error) {
	return mic.Receiver{Card: 2, InputPath: micInputPath}, true, nil
}

// micIDs hands out recording IDs in order.
type micIDs struct {
	mu   sync.Mutex
	next int64
}

func (m *micIDs) newID() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++

	return m.next
}

// tgEvent is one Telegram call.
type tgEvent struct {
	op        string // send, edit or delete
	chat, msg int64
	text      string
}

// micTelegram is the fake Telegram: each send gets a new message ID.
type micTelegram struct {
	mu     sync.Mutex
	nextID int64
	events []tgEvent
}

func (tg *micTelegram) Send(_ context.Context, chatID, _ int64, text string) (int64, error) {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	tg.nextID++
	id := 700 + tg.nextID
	tg.events = append(tg.events, tgEvent{op: "send", chat: chatID, msg: id, text: text})

	return id, nil
}

func (tg *micTelegram) Edit(_ context.Context, chatID, msgID int64, text string) (int64, error) {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	tg.events = append(tg.events, tgEvent{op: "edit", chat: chatID, msg: msgID, text: text})

	return msgID, nil
}

func (tg *micTelegram) Delete(_ context.Context, chatID, msgID int64) error {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	tg.events = append(tg.events, tgEvent{op: "delete", chat: chatID, msg: msgID})

	return nil
}

func (tg *micTelegram) log() []tgEvent {
	tg.mu.Lock()
	defer tg.mu.Unlock()

	return slices.Clone(tg.events)
}

// find returns the first event with op on msg.
func (tg *micTelegram) find(op string, msg int64) (tgEvent, bool) {
	for _, e := range tg.log() {
		if e.op == op && e.msg == msg {
			return e, true
		}
	}

	return tgEvent{}, false
}

// micUpload is one recording upload as the service saw it.
type micUpload struct {
	path, startedAt, endReason string
}

// micUploads wraps the service mux and records every recording upload.
type micUploads struct {
	mu   sync.Mutex
	list []micUpload
}

func (u *micUploads) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/mic/v1/recordings/") {
			u.mu.Lock()
			u.list = append(u.list, micUpload{
				path:      r.URL.Path,
				startedAt: r.Header.Get("X-Recording-Started-At"),
				endReason: r.Header.Get("X-Recording-End-Reason"),
			})
			u.mu.Unlock()
		}
		next.ServeHTTP(w, r)
	})
}

func (u *micUploads) all() []micUpload {
	u.mu.Lock()
	defer u.mu.Unlock()

	return slices.Clone(u.list)
}

// newMicLLM is an LLM that turns every transcript into the same text with
// exactly one task.
func newMicLLM(t *testing.T) *httptest.Server {
	t.Helper()
	answer, err := json.Marshal(map[string]any{"language": "en", "text": micSaid, "tasks": []string{micTask}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": string(answer)}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	return srv
}

// ---------------------------------------------------------------- harness

type micE2E struct {
	world    *e2eWorld
	tg       *micTelegram
	db       *sqlite.DB
	clock    *micClock
	keys     *micKeys
	uploads  *micUploads
	rec      *micrec.Recorder
	spool    *micrec.DirSpool
	spoolDir string
}

// startMicE2E wires the service (pipeline and microphone API, as wire does)
// behind httptest and runs a real recorder against it until the test ends.
func startMicE2E(t *testing.T) *micE2E {
	t.Helper()
	llm := newMicLLM(t)
	cfg := e2eConfig()
	cfg.MicUploadToken, cfg.MicNotifyChatID, cfg.MicUploadMaxBytes = micTestToken, micNotifyChat, 31457280
	cfg.LLMEnabled, cfg.LLMBaseURL, cfg.LLMModel, cfg.LLMTimeout = true, llm.URL, "fake-model", 5*time.Second
	cfg.LLMTasks, cfg.TargetLanguage = true, "en"

	dir := t.TempDir()
	db, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	filesDir := filepath.Join(dir, "files")
	m := &micE2E{
		world: &e2eWorld{filesDir: filesDir}, tg: &micTelegram{}, db: db,
		clock: newMicClock(micFirstTap().Add(-time.Minute)), keys: &micKeys{events: make(chan micrec.KeyEvent)},
		uploads: &micUploads{}, spoolDir: filepath.Join(t.TempDir(), "spool"),
	}
	p, err := newPipeline(cfg, zap.NewNop(), db, pipeline.Deps{
		Transcriber: voiceTranscriber{text: micSaid}, Notes: m.world, Replier: m.tg, Downloader: m.world,
		Files: pipeline.NewDirFiles(filesDir), Log: zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("newPipeline: %v", err)
	}
	// Cleanups run last-in first-out, after t.Context is cancelled: the
	// recorder stops, then the server, the start notices and the workers,
	// and the store closes last.
	var workers sync.WaitGroup
	t.Cleanup(workers.Wait)
	for _, w := range []*pipeline.Worker{p.mainWorker, p.docsWorker} {
		workers.Go(func() { w.Run(t.Context()) })
	}
	mux := http.NewServeMux()
	micHandler := mountMic(mux, cfg, zap.NewNop(), db, p.ingester, m.tg, filesDir)
	t.Cleanup(micHandler.Wait)
	srv := httptest.NewServer(m.uploads.wrap(mux))
	t.Cleanup(srv.Close)

	m.startRecorder(t, srv)

	return m
}

func (m *micE2E) startRecorder(t *testing.T, srv *httptest.Server) {
	t.Helper()
	spool, err := micrec.NewDirSpool(m.spoolDir, m.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	client, err := micrec.NewClient(srv.URL, micTestToken, micRecorderName, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	ids := &micIDs{next: 4100}
	rec, err := micrec.New(micrec.Deps{
		Keys: m.keys, Audio: micAudio{}, Discovery: micDiscovery{}, Spool: spool,
		Handoff: client, Notifier: client, CheckIn: client, Log: zap.NewNop(),
	}, micrec.Options{
		Clock: m.clock, NewID: ids.newID, Recorder: micRecorderName,
		Debounce: micDebounce, MinLength: micMinLength, MaxLength: micMaxLength,
		CheckinInterval: time.Minute, RetryMaxInterval: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.rec, m.spool = rec, spool

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = rec.Run(t.Context())
	}()
	t.Cleanup(func() { <-done })

	// The session's one-second ticker exists once the receiver is open.
	waitFor(t, "the receiver session", func() bool {
		return rec.Status().ReceiverPresent && m.clock.hasTicker(time.Second)
	})
}

// tapAt presses the button at the given time. The clock is only moved
// while the recorder is between events, so the tap is timed exactly at.
func (m *micE2E) tapAt(t *testing.T, at time.Time) {
	t.Helper()
	m.clock.set(at)
	select {
	case m.keys.events <- micrec.KeyTap:
	case <-time.After(10 * time.Second):
		t.Fatal("the recorder never read the tap")
	}
}

// startRecording taps at `at` and waits until the recording runs and its
// "🎙 recording…" message is stored, so the outcome edits it. It returns
// the start message.
func (m *micE2E) startRecording(t *testing.T, at time.Time, id int64) tgEvent {
	t.Helper()
	m.tapAt(t, at)
	waitFor(t, "the recording to start", func() bool { return m.rec.Status().Recording })
	waitFor(t, "the stored start notice", func() bool {
		ok, err := m.db.HasStartReply(t.Context(), id)
		return err == nil && ok
	})
	var start tgEvent
	for _, e := range m.tg.log() {
		if e.op == "send" && e.text == "🎙 recording…" {
			start = e
		}
	}
	if start.chat != micNotifyChat {
		t.Fatalf("start notice = %+v, want 🎙 recording… in the notify chat", start)
	}

	return start
}

// waitForSaved waits until the start message was edited and the queue is
// empty, and returns the edit.
func (m *micE2E) waitForSaved(t *testing.T, start tgEvent) tgEvent {
	t.Helper()
	waitFor(t, "the start message edited", func() bool {
		_, ok := m.tg.find("edit", start.msg)
		return ok
	})
	waitFor(t, "an empty queue", func() bool {
		n, err := m.db.CountUnfinished(t.Context())
		return err == nil && n == 0
	})
	edit, _ := m.tg.find("edit", start.msg)

	return edit
}

// checkSaved checks the outcome of one recording that started at
// micFirstTap: the confirmation and the note with its audio.
func checkSaved(t *testing.T, edit tgEvent, note pipeline.NoteInput, attached string) {
	t.Helper()
	if edit.chat != micNotifyChat || !strings.HasPrefix(edit.text, "✓ saved · 1 task · ") ||
		!strings.Contains(edit.text, ">open</a>") {
		t.Errorf("confirmation = %+v, want ✓ saved · 1 task · open in the notify chat", edit)
	}
	if !note.ReceivedAt.Equal(micFirstTap()) {
		t.Errorf("note time = %v, want the first tap %v", note.ReceivedAt, micFirstTap())
	}
	for _, want := range []string{micSaid, micTask, "#inbox"} {
		if !strings.Contains(note.Content, want) {
			t.Errorf("note content %q lacks %q", note.Content, want)
		}
	}
	if want := string(mic.Header(micAudioBytes)) + string(micAudioFixture()); attached != want {
		t.Errorf("attached audio is %d bytes, want the %d-byte WAV of what was captured", len(attached), len(want))
	}
}

// ---------------------------------------------------------------- scenarios

// Scenarios 1.1, 1.2, 1.4, 1.5: each tap pair makes exactly one capture,
// its "🎙 recording…" becomes "✓ saved · 1 task · open", the input is
// grabbed and the note is timed at the first tap.
func TestMicDirectTapPairsMakeOneNoteEach(t *testing.T) {
	m := startMicE2E(t)

	if got := m.keys.grabs(); !slices.Equal(got, []string{micInputPath}) || !m.rec.Status().InputGrabbed {
		t.Fatalf("grabbed inputs = %q (status %v), want the receiver's input held exclusively",
			got, m.rec.Status().InputGrabbed)
	}

	start1 := m.startRecording(t, micFirstTap(), 4101)
	m.tapAt(t, micFirstTap().Add(5*time.Second))
	edit1 := m.waitForSaved(t, start1)

	second := micFirstTap().Add(time.Hour)
	start2 := m.startRecording(t, second, 4102)
	m.tapAt(t, second.Add(8*time.Second))
	edit2 := m.waitForSaved(t, start2)

	notes, _, _ := m.world.snapshot()
	attached := m.world.attachedData()
	if len(notes) != 2 || len(attached) != 2 {
		t.Fatalf("%d notes, %d attachments; want one note with its audio per tap pair", len(notes), len(attached))
	}
	checkSaved(t, edit1, notes[0], attached[0])
	if !notes[1].ReceivedAt.Equal(second) {
		t.Errorf("second note time = %v, want its first tap %v", notes[1].ReceivedAt, second)
	}
	if !strings.HasPrefix(edit2.text, "✓ saved · 1 task · ") || start2.msg == start1.msg {
		t.Errorf("second recording: start %+v, edit %+v; want its own message edited", start2, edit2)
	}

	ups := m.uploads.all()
	if len(ups) != 2 || ups[0].endReason != string(mic.EndTap) || ups[1].endReason != string(mic.EndTap) {
		t.Errorf("uploads = %+v, want two ended by a tap", ups)
	}
	if ups[0].startedAt != micFirstTap().Format(time.RFC3339Nano) {
		t.Errorf("first upload started at %q, want the first tap", ups[0].startedAt)
	}
	for _, e := range m.tg.log() {
		if e.op == "delete" || (e.op == "send" && e.text != "🎙 recording…") {
			t.Errorf("unexpected Telegram call %+v", e)
		}
	}
	checkSpoolEmpty(t, m)
}

// Scenario 1.3: talking past MIC_MAX_LENGTH stops the recording by itself,
// and it is processed like a tapped stop.
func TestMicDirectAutoStopsAtMaxLength(t *testing.T) {
	m := startMicE2E(t)

	start := m.startRecording(t, micFirstTap(), 4101)
	m.clock.set(micFirstTap().Add(micMaxLength))
	m.clock.tick(time.Second)
	edit := m.waitForSaved(t, start)

	if m.rec.Status().Recording {
		t.Error("still recording after the maximum length")
	}
	notes, _, _ := m.world.snapshot()
	attached := m.world.attachedData()
	if len(notes) != 1 || len(attached) != 1 {
		t.Fatalf("%d notes, %d attachments; want one note with its audio", len(notes), len(attached))
	}
	checkSaved(t, edit, notes[0], attached[0])
	if ups := m.uploads.all(); len(ups) != 1 || ups[0].endReason != string(mic.EndMaxLength) {
		t.Errorf("uploads = %+v, want one ended at max_length", ups)
	}
	checkSpoolEmpty(t, m)
}

// Scenario 1.6: a second tap within MIC_MIN_LENGTH discards the recording:
// no capture, and the "🎙 recording…" message is deleted.
func TestMicDirectDoubleTapDeletesStartAndKeepsNothing(t *testing.T) {
	m := startMicE2E(t)

	start := m.startRecording(t, micFirstTap(), 4101)
	m.tapAt(t, micFirstTap().Add(time.Second))
	waitFor(t, "the start message deleted", func() bool {
		_, ok := m.tg.find("delete", start.msg)
		return ok
	})

	if del, _ := m.tg.find("delete", start.msg); del.chat != micNotifyChat {
		t.Errorf("delete = %+v, want the start message in the notify chat", del)
	}
	if m.rec.Status().Recording {
		t.Error("still recording after the double tap")
	}
	if ups := m.uploads.all(); len(ups) != 0 {
		t.Errorf("uploads = %+v, want none", ups)
	}
	if has, err := m.db.HasRecording(t.Context(), micNotifyChat, 4101); err != nil || has {
		t.Errorf("recording stored = %v (%v), want no capture", has, err)
	}
	if n, err := m.db.CountUnfinished(t.Context()); err != nil || n != 0 {
		t.Errorf("unfinished captures = %d (%v), want none", n, err)
	}
	if notes, _, _ := m.world.snapshot(); len(notes) != 0 {
		t.Errorf("notes = %+v, want none", notes)
	}
	for _, e := range m.tg.log() {
		if e.op == "edit" {
			t.Errorf("unexpected edit %+v", e)
		}
	}
	checkSpoolEmpty(t, m)
}

// checkSpoolEmpty checks that the recorder holds no recording: every one
// was handed over or discarded.
func checkSpoolEmpty(t *testing.T, m *micE2E) {
	t.Helper()
	waitFor(t, "an empty spool", func() bool {
		entries, err := os.ReadDir(m.spoolDir)
		return err == nil && len(entries) == 0
	})
	if recs, err := m.spool.Finished(); err != nil || len(recs) != 0 {
		t.Errorf("spool still holds %v (%v)", recs, err)
	}
}
