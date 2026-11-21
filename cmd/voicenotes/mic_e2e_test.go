package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/voicenotes/internal/health"
	"github.com/Toshik1978/voicenotes/internal/mic"
	"github.com/Toshik1978/voicenotes/internal/micapi"
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

// micFault is what the fake network does to one request.
type micFault int

const (
	faultNone      micFault = iota
	faultDown               // the request never reaches the service
	faultLoseReply          // the service handles it, but the answer is lost
	faultNotWAV             // the request arrives labelled as something else than audio/wav
)

// micNet is the controllable network between recorder and service: a
// RoundTripper whose rule decides, per request, how it fails.
type micNet struct {
	next http.RoundTripper

	mu   sync.Mutex
	rule func(*http.Request) micFault
	lost int
}

func (n *micNet) setRule(rule func(*http.Request) micFault) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.rule = rule
}

func (n *micNet) lostReplies() int {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.lost
}

func (n *micNet) RoundTrip(r *http.Request) (*http.Response, error) {
	n.mu.Lock()
	rule := n.rule
	n.mu.Unlock()

	fault := faultNone
	if rule != nil {
		fault = rule(r)
	}

	switch fault {
	case faultDown:
		if r.Body != nil {
			_ = r.Body.Close()
		}

		return nil, errors.New("network is down")
	case faultLoseReply:
		resp, err := n.next.RoundTrip(r)
		if err != nil {
			return nil, err
		}
		_ = resp.Body.Close()
		n.mu.Lock()
		n.lost++
		n.mu.Unlock()

		return nil, errors.New("connection reset before the reply")
	case faultNotWAV:
		r = r.Clone(r.Context())
		r.Header.Set("Content-Type", "audio/mpeg")
	case faultNone:
	}

	return n.next.RoundTrip(r)
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
	srv      *httptest.Server
	net      *micNet
	svc      *micServiceClock
	alerts   *micapi.AlertLoop
}

// micSilence is MIC_SILENCE_ALERT in the alert scenarios.
const micSilence = 15 * time.Minute

// micServiceClock is the service's clock: it stamps check-ins and drives
// the alert loop, so a scenario can let "no check-ins" pass without waiting.
type micServiceClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *micServiceClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *micServiceClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// micSetup varies how the recorder is set up.
type micSetup struct {
	token string // the recorder's upload token; empty: the service's
	// audio and discovery replace the fakes that always work.
	audio     micrec.AudioSource
	discovery micrec.Discovery
	// noReceiver: no receiver is found at start, so there is no session to
	// wait for.
	noReceiver bool
	// prepare lays out the recorder's spool before the recorder starts.
	prepare func(t *testing.T, dir string)
	// spoolMinFree is MIC_SPOOL_MIN_FREE.
	spoolMinFree int64
}

// startMicE2E wires the service (pipeline and microphone API, as wire does)
// behind httptest and runs a real recorder against it until the test ends.
func startMicE2E(t *testing.T) *micE2E {
	t.Helper()

	return startMicE2EWith(t, micSetup{})
}

// startMicE2EWith is startMicE2E with the recorder set up as given.
func startMicE2EWith(t *testing.T, setup micSetup) *micE2E {
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
	m.svc = &micServiceClock{now: time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)}
	micHandler := mountMic(mux, cfg, zap.NewNop(), db, p.ingester, m.tg, filesDir, micapi.WithClock(m.svc.Now))
	m.alerts = micapi.NewAlertLoop(alertStore{db: db}, m.tg, micNotifyChat, micSilence, time.UTC)
	m.alerts.SetClock(m.svc.Now)
	t.Cleanup(micHandler.Wait)
	srv := httptest.NewServer(m.uploads.wrap(mux))
	t.Cleanup(srv.Close)

	m.srv = srv
	m.net = &micNet{next: srv.Client().Transport}
	if setup.prepare != nil {
		setup.prepare(t, m.spoolDir)
	}
	m.startRecorder(t, srv, cmp.Or(setup.token, micTestToken), setup)

	return m
}

func (m *micE2E) startRecorder(t *testing.T, srv *httptest.Server, token string, setup micSetup) {
	t.Helper()
	spool, err := micrec.NewDirSpool(m.spoolDir, m.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	client, err := micrec.NewClient(srv.URL, token, micRecorderName, &http.Client{Transport: m.net})
	if err != nil {
		t.Fatal(err)
	}
	ids := &micIDs{next: 4100}
	var audio micrec.AudioSource = micAudio{}
	if setup.audio != nil {
		audio = setup.audio
	}
	var discovery micrec.Discovery = micDiscovery{}
	if setup.discovery != nil {
		discovery = setup.discovery
	}
	rec, err := micrec.New(micrec.Deps{
		Keys: m.keys, Audio: audio, Discovery: discovery, Spool: spool,
		Handoff: client, Notifier: client, CheckIn: client, Log: zap.NewNop(),
	}, micrec.Options{
		Clock: m.clock, NewID: ids.newID, Recorder: micRecorderName,
		Debounce: micDebounce, MinLength: micMinLength, MaxLength: micMaxLength,
		CheckinInterval: time.Minute, RetryMaxInterval: 5 * time.Minute, SpoolMinFree: setup.spoolMinFree,
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

	if setup.noReceiver {
		waitFor(t, "the recorder to run", func() bool { return rec.Status().Running })
		return
	}

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

// beginRecording taps at `at` and waits until the recording runs.
func (m *micE2E) beginRecording(t *testing.T, at time.Time) {
	t.Helper()
	m.tapAt(t, at)
	waitFor(t, "the recording to start", func() bool { return m.rec.Status().Recording })
}

// startRecording taps at `at` and waits until the recording runs and its
// "🎙 recording…" message is stored under id, so the outcome edits it. It
// returns that message: the one sent after the tap.
func (m *micE2E) startRecording(t *testing.T, at time.Time, id int64) tgEvent {
	t.Helper()
	before := len(m.tg.log())
	m.beginRecording(t, at)
	waitFor(t, "the stored start notice", func() bool {
		ok, err := m.db.HasStartReply(t.Context(), id)
		return err == nil && ok
	})
	var start tgEvent
	for _, e := range m.tg.log()[before:] {
		if e.op == "send" && e.text == "🎙 recording…" {
			start = e
			break
		}
	}
	if start.chat != micNotifyChat {
		t.Fatalf("start notice = %+v, want 🎙 recording… in the notify chat", start)
	}

	return start
}

// advanceUntil moves the recorder's clock on, firing its backoff waits,
// until cond holds; no real backoff is slept.
func (m *micE2E) advanceUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	waitFor(t, what, func() bool {
		m.clock.set(m.clock.Now().Add(time.Minute))
		return cond()
	})
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

// ---------------------------------------------------------------- proxy mode

// Scenarios 2.1, 2.2, 2.4, 2.6: with the service unreachable the recorder
// still records at once, sends no start notice and keeps every recording;
// once the service is back they arrive oldest first, one note each, and the
// outcomes arrive as new messages.
func TestMicProxyDeliversOldestFirstAfterAnOutage(t *testing.T) {
	m := startMicE2E(t)
	m.net.setRule(func(*http.Request) micFault { return faultDown })

	taps := []time.Time{micFirstTap(), micFirstTap().Add(time.Hour), micFirstTap().Add(2 * time.Hour)}
	for i, at := range taps {
		m.beginRecording(t, at)
		m.tapAt(t, at.Add(5*time.Second))
		waitFor(t, "the recording to be kept", func() bool {
			recs, err := m.spool.Finished()
			return err == nil && len(recs) == i+1
		})
	}
	if ups := m.uploads.all(); len(ups) != 0 {
		t.Fatalf("uploads = %+v, want none reached the service", ups)
	}
	if evs := m.tg.log(); len(evs) != 0 {
		t.Fatalf("Telegram calls = %+v, want no start notice while the service is down", evs)
	}

	m.net.setRule(nil)
	m.advanceUntil(t, "every recording handed over", func() bool {
		recs, err := m.spool.Finished()
		n, cerr := m.db.CountUnfinished(t.Context())
		notes, _, _ := m.world.snapshot()
		return err == nil && cerr == nil && len(recs) == 0 && n == 0 && len(notes) == len(taps)
	})

	wantPaths := []string{"/mic/v1/recordings/4101", "/mic/v1/recordings/4102", "/mic/v1/recordings/4103"}
	uploads := m.uploads.all()
	gotPaths := make([]string, 0, len(uploads))
	for _, u := range uploads {
		gotPaths = append(gotPaths, u.path)
	}
	if !slices.Equal(gotPaths, wantPaths) {
		t.Errorf("upload order = %q, want oldest first %q", gotPaths, wantPaths)
	}
	notes, _, _ := m.world.snapshot()
	attached := m.world.attachedData()
	if len(attached) != len(taps) {
		t.Fatalf("%d attachments, want one per recording", len(attached))
	}
	for i, at := range taps {
		if !notes[i].ReceivedAt.Equal(at) {
			t.Errorf("note %d time = %v, want its first tap %v", i, notes[i].ReceivedAt, at)
		}
	}
	sends := 0
	for _, e := range m.tg.log() {
		if e.op != "send" || !strings.HasPrefix(e.text, "✓ saved · 1 task · ") || e.chat != micNotifyChat {
			t.Errorf("unexpected Telegram call %+v, want only the outcomes as new messages", e)
			continue
		}
		sends++
	}
	if sends != len(taps) {
		t.Errorf("%d outcome messages, want %d", sends, len(taps))
	}
}

// Scenarios 2.3, 2.4: the service stored a recording but its confirmation
// never arrived. The recorder keeps its copy, sends it again, gets a
// duplicate confirmation and only then deletes it; there is one note.
func TestMicProxyLostConfirmationYieldsOneNote(t *testing.T) {
	m := startMicE2E(t)
	m.net.setRule(func(r *http.Request) micFault {
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/mic/v1/recordings/") {
			return faultLoseReply
		}

		return faultNone
	})

	start := m.startRecording(t, micFirstTap(), 4101)
	m.tapAt(t, micFirstTap().Add(5*time.Second))
	waitFor(t, "the lost confirmation", func() bool { return m.net.lostReplies() == 1 })
	edit := m.waitForSaved(t, start)

	// The service has the recording, yet the recorder has no confirmation:
	// its copy must still be there.
	if recs, err := m.spool.Finished(); err != nil || len(recs) != 1 {
		t.Fatalf("spool holds %v (%v), want the recording kept until a confirmation arrives", recs, err)
	}

	m.net.setRule(nil)
	m.advanceUntil(t, "the resend to be confirmed", func() bool {
		recs, err := m.spool.Finished()
		return err == nil && len(recs) == 0
	})

	if ups := m.uploads.all(); len(ups) != 2 || ups[0].path != ups[1].path {
		t.Errorf("uploads = %+v, want the same recording sent twice", ups)
	}
	notes, _, _ := m.world.snapshot()
	attached := m.world.attachedData()
	if len(notes) != 1 || len(attached) != 1 {
		t.Fatalf("%d notes, %d attachments; want exactly one of each", len(notes), len(attached))
	}
	checkSaved(t, edit, notes[0], attached[0])
	if n, err := m.db.CountUnfinished(t.Context()); err != nil || n != 0 {
		t.Errorf("unfinished captures = %d (%v), want none", n, err)
	}
}

// Scenario 2.5: a wrong token is refused with 401 and stores nothing; the
// recorder keeps the recording and reports itself misconfigured.
func TestMicProxyWrongTokenStoresNothing(t *testing.T) {
	m := startMicE2EWith(t, micSetup{token: "FAKE-wrong-token-0123456789abcdef"})

	m.beginRecording(t, micFirstTap())
	m.tapAt(t, micFirstTap().Add(5*time.Second))
	waitFor(t, "the recorder to report misconfigured", func() bool { return m.rec.Status().Misconfigured })

	if ups := m.uploads.all(); len(ups) == 0 {
		t.Fatal("the recorder never tried to upload")
	}
	if recs, err := m.spool.Finished(); err != nil || len(recs) != 1 {
		t.Errorf("spool holds %v (%v), want the recording kept", recs, err)
	}
	if has, err := m.db.HasRecording(t.Context(), micNotifyChat, 4101); err != nil || has {
		t.Errorf("recording stored = %v (%v), want nothing stored", has, err)
	}
	if notes, _, _ := m.world.snapshot(); len(notes) != 0 {
		t.Errorf("notes = %+v, want none", notes)
	}
	if evs := m.tg.log(); len(evs) != 0 {
		t.Errorf("Telegram calls = %+v, want none", evs)
	}
	rep := micrec.ComputeHealth(m.rec.Status(), m.clock.Now(), time.Hour)
	if !rep.Misconfigured || rep.Status != health.StatusDegraded {
		t.Errorf("health = %+v, want degraded and misconfigured", rep)
	}

	// The endpoint refuses a bare request the same way.
	req, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		m.srv.URL+"/mic/v1/recorders/"+micRecorderName,
		http.NoBody,
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := m.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("request without a credential: status %d, want 401", resp.StatusCode)
	}
}

// Scenario 2.6 (unreachable at the start): the start notice fails but
// recording and upload go on, and the outcome arrives as a new message.
func TestMicProxyFailedStartNoticeSendsOutcomeAsNewMessage(t *testing.T) {
	m := startMicE2E(t)
	m.net.setRule(func(r *http.Request) micFault {
		if strings.HasSuffix(r.URL.Path, "/started") {
			return faultDown
		}

		return faultNone
	})

	m.beginRecording(t, micFirstTap())
	m.tapAt(t, micFirstTap().Add(5*time.Second))
	waitFor(t, "the outcome message", func() bool {
		for _, e := range m.tg.log() {
			if e.op == "send" && strings.HasPrefix(e.text, "✓ saved · 1 task · ") {
				return true
			}
		}

		return false
	})

	notes, _, _ := m.world.snapshot()
	if len(notes) != 1 {
		t.Fatalf("%d notes, want one", len(notes))
	}
	for _, e := range m.tg.log() {
		if e.op != "send" || e.chat != micNotifyChat {
			t.Errorf("unexpected Telegram call %+v, want one new outcome message", e)
		}
	}
	checkSpoolEmpty(t, m)
}

// The 415 policy: a recording the service refuses for good is kept, skipped
// and reported, and the next recording still goes through.
func TestMicProxyRejectedRecordingIsKeptSkippedAndReported(t *testing.T) {
	m := startMicE2E(t)
	m.net.setRule(func(r *http.Request) micFault {
		if r.Method == http.MethodPut && r.URL.Path == "/mic/v1/recordings/4101" {
			return faultNotWAV
		}

		return faultNone
	})

	start1 := m.startRecording(t, micFirstTap(), 4101)
	m.tapAt(t, micFirstTap().Add(5*time.Second))
	waitFor(t, "the recording to be rejected", func() bool { return m.rec.Status().Rejected == 1 })

	second := micFirstTap().Add(time.Hour)
	start2 := m.startRecording(t, second, 4102)
	m.tapAt(t, second.Add(5*time.Second))
	edit2 := m.waitForSaved(t, start2)

	if !strings.HasPrefix(edit2.text, "✓ saved · 1 task · ") {
		t.Errorf("second outcome = %+v, want it saved", edit2)
	}
	recs, err := m.spool.Finished()
	if err != nil || len(recs) != 1 || recs[0].ID != 4101 {
		t.Fatalf("spool holds %v (%v), want only the rejected recording kept", recs, err)
	}
	if has, err := m.db.HasRecording(t.Context(), micNotifyChat, 4101); err != nil || has {
		t.Errorf("rejected recording stored = %v (%v), want nothing", has, err)
	}
	if notes, _, _ := m.world.snapshot(); len(notes) != 1 {
		t.Errorf("%d notes, want one (the second recording)", len(notes))
	}
	if _, edited := m.tg.find("edit", start1.msg); edited {
		t.Error("the rejected recording's start message was edited")
	}
	rep := micrec.ComputeHealth(m.rec.Status(), m.clock.Now(), time.Hour)
	if rep.Rejected != 1 || rep.Status != health.StatusDegraded {
		t.Errorf("health = %+v, want degraded with one rejected recording", rep)
	}
}

// ---------------------------------------------------------------- interruptions and alerts

// micRecoveredID is the recording a killed recorder left in its spool.
const micRecoveredID = int64(4050)

// micLostAudio is the fixture capture that can report the device lost.
type micLostAudio struct{ lost chan error }

func (a micLostAudio) Open(rcv mic.Receiver, q *micrec.FrameQueue) (micrec.AudioStream, error) {
	s, err := micAudio{}.Open(rcv, q)
	s.Lost = a.lost

	return s, err
}

// micSwitchDiscovery finds the receiver only while present is set.
type micSwitchDiscovery struct{ present atomic.Bool }

func (d *micSwitchDiscovery) Discover() (mic.Receiver, bool, error) {
	if !d.present.Load() {
		return mic.Receiver{}, false, nil
	}

	return mic.Receiver{Card: 2, InputPath: micInputPath}, true, nil
}

// alertTexts returns the recorder alerts sent to the notify chat, in order.
func (m *micE2E) alertTexts() []string {
	var out []string
	for _, e := range m.tg.log() {
		if e.op == "send" && e.chat == micNotifyChat && strings.HasPrefix(e.text, "🎙 "+micRecorderName+": ") {
			out = append(out, e.text)
		}
	}

	return out
}

// passAlerts runs the alert loop once.
func (m *micE2E) passAlerts(t *testing.T) {
	t.Helper()
	if err := m.alerts.Pass(t.Context()); err != nil {
		t.Fatalf("alert pass: %v", err)
	}
}

// recorderRow returns the service's row of the recorder.
func (m *micE2E) recorderRow(t *testing.T) (sqlite.Recorder, bool) {
	t.Helper()
	rows, err := m.db.Recorders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == micRecorderName {
			return r, true
		}
	}

	return sqlite.Recorder{}, false
}

// checkInUntil makes the recorder check in until its row on the service
// satisfies cond.
func (m *micE2E) checkInUntil(t *testing.T, what string, cond func(sqlite.Recorder) bool) {
	t.Helper()
	waitFor(t, what, func() bool {
		if r, ok := m.recorderRow(t); ok && cond(r) {
			return true
		}
		m.clock.tick(time.Minute)

		return false
	})
}

// wantAlerts checks the alert messages sent so far, by their text.
func (m *micE2E) wantAlerts(t *testing.T, want ...string) {
	t.Helper()
	got := m.alertTexts()
	if len(got) != len(want) {
		t.Fatalf("alerts = %q, want %d: %q", got, len(want), want)
	}
	for i := range want {
		if !strings.Contains(got[i], want[i]) {
			t.Errorf("alert %d = %q, want it to contain %q", i, got[i], want[i])
		}
	}
}

// Scenario 3.1: the recorder was killed mid-recording after 3 s of audio
// were fsynced, a torn frame at the tail. At the next start the spool is
// recovered and the service saves a note with at least that audio.
func TestMicRecoversAKilledRecordingIntoANote(t *testing.T) {
	const seconds = 3
	m := startMicE2EWith(t, micSetup{prepare: func(t *testing.T, dir string) {
		t.Helper()
		sp, err := micrec.NewDirSpool(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		w, err := sp.Begin(micrec.Recording{ID: micRecoveredID, Recorder: micRecorderName, StartedAt: micFirstTap()})
		if err != nil {
			t.Fatal(err)
		}
		for range seconds {
			if _, err = w.Write(micAudioFixture()); err != nil {
				t.Fatal(err)
			}
		}
		if err = w.Close(); err != nil { // fsynced; the process dies without Finish
			t.Fatal(err)
		}
		part, err := os.OpenFile(filepath.Join(dir, "4050.part.wav"), os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = part.Write([]byte{0x7f}); err != nil { // half a frame, torn by the kill
			t.Fatal(err)
		}
		if err = part.Close(); err != nil {
			t.Fatal(err)
		}
	}})

	waitFor(t, "the recovered recording to be saved", func() bool {
		n, err := m.db.CountUnfinished(t.Context())
		notes, _, _ := m.world.snapshot()
		return err == nil && n == 0 && len(notes) == 1
	})

	notes, _, _ := m.world.snapshot()
	attached := m.world.attachedData()
	if len(attached) != 1 {
		t.Fatalf("%d attachments, want one", len(attached))
	}
	want := string(mic.Header(seconds*micAudioBytes)) + strings.Repeat(string(micAudioFixture()), seconds)
	if attached[0] != want {
		t.Errorf("attached audio is %d bytes, want the %d-byte WAV of the fsynced audio", len(attached[0]), len(want))
	}
	if !notes[0].ReceivedAt.Equal(micFirstTap()) || !strings.Contains(notes[0].Content, micSaid) {
		t.Errorf("note = %+v, want it timed at the recording's start with its text", notes[0])
	}
	ups := m.uploads.all()
	if len(ups) != 1 || ups[0].endReason != string(mic.EndInterrupted) || ups[0].path != "/mic/v1/recordings/4050" {
		t.Errorf("uploads = %+v, want recording 4050 ended as interrupted", ups)
	}
	evs := m.tg.log()
	if len(evs) != 1 || evs[0].op != "send" || !strings.HasPrefix(evs[0].text, "✓ saved · 1 task · ") {
		t.Errorf("Telegram calls = %+v, want the outcome as one new message", evs)
	}
	checkSpoolEmpty(t, m)
}

// Scenario 3.2: the receiver disappears mid-recording. The note holds what
// was captured up to there and the recording ends as receiver_lost.
func TestMicDeviceLostMidRecordingEndsTheNoteThere(t *testing.T) {
	lost := make(chan error, 1)
	m := startMicE2EWith(t, micSetup{audio: micLostAudio{lost: lost}})

	start := m.startRecording(t, micFirstTap(), 4101)
	m.clock.set(micFirstTap().Add(5 * time.Second))

	lost <- errors.Join(micrec.ErrDeviceLost, errors.New("card 2 is gone"))
	edit := m.waitForSaved(t, start)

	notes, _, _ := m.world.snapshot()
	attached := m.world.attachedData()
	if len(notes) != 1 || len(attached) != 1 {
		t.Fatalf("%d notes, %d attachments; want one note with its audio", len(notes), len(attached))
	}
	checkSaved(t, edit, notes[0], attached[0])
	if ups := m.uploads.all(); len(ups) != 1 || ups[0].endReason != string(mic.EndReceiverLost) {
		t.Errorf("uploads = %+v, want one ended as receiver_lost", ups)
	}
	checkSpoolEmpty(t, m)
}

// Scenario 3.3 and SC-202: no receiver degrades the recorder's health, and
// after its check-in the service sends one "receiver not connected"; when
// the receiver is back, one "receiver connected".
func TestMicMissingReceiverAlertsOnceAndRecovers(t *testing.T) {
	disc := &micSwitchDiscovery{}
	m := startMicE2EWith(t, micSetup{discovery: disc, noReceiver: true})

	rep := micrec.ComputeHealth(m.rec.Status(), m.clock.Now(), time.Hour)
	if rep.Status != health.StatusDegraded || rep.Receiver != "missing" {
		t.Errorf("health = %+v, want degraded with the receiver missing", rep)
	}

	waitFor(t, "the first check-in", func() bool {
		r, ok := m.recorderRow(t)
		return ok && !r.ReceiverPresent
	})
	m.passAlerts(t)
	m.passAlerts(t)
	m.wantAlerts(t, "receiver not connected")

	disc.present.Store(true)
	m.advanceUntil(t, "the receiver to be found", func() bool { return m.rec.Status().ReceiverPresent })
	if rep := micrec.ComputeHealth(m.rec.Status(), m.clock.Now(), time.Hour); rep.Status != health.StatusOK {
		t.Errorf("health = %+v, want ok with the receiver back", rep)
	}
	m.checkInUntil(t, "a check-in with the receiver", func(r sqlite.Recorder) bool { return r.ReceiverPresent })
	m.passAlerts(t)
	m.passAlerts(t)
	m.wantAlerts(t, "receiver not connected", "receiver connected")
}

// SC-209 (storage): a check-in with storage_full gives one alert, and a tap
// is refused: no recording, no file, no notice.
func TestMicStorageFullAlertsOnceAndRefusesTaps(t *testing.T) {
	m := startMicE2EWith(t, micSetup{spoolMinFree: math.MaxInt64})

	m.checkInUntil(t, "a check-in with storage_full", func(r sqlite.Recorder) bool { return r.StorageFull })
	m.passAlerts(t)
	m.passAlerts(t)
	m.wantAlerts(t, "storage full")

	// The second tap is read only after the first was handled.
	m.tapAt(t, micFirstTap())
	m.tapAt(t, micFirstTap().Add(5*time.Second))

	if m.rec.Status().Recording {
		t.Error("recording although storage is full")
	}
	if entries, err := os.ReadDir(m.spoolDir); err != nil || len(entries) != 0 {
		t.Errorf("spool holds %v (%v), want nothing for a refused tap", entries, err)
	}
	if ups := m.uploads.all(); len(ups) != 0 {
		t.Errorf("uploads = %+v, want none", ups)
	}
	if rep := micrec.ComputeHealth(m.rec.Status(), m.clock.Now(), time.Hour); rep.Storage != "full" ||
		rep.Status != health.StatusDegraded {
		t.Errorf("health = %+v, want degraded with storage full", rep)
	}
	m.wantAlerts(t, "storage full")
	for _, e := range m.tg.log() {
		if e.text == "🎙 recording…" {
			t.Errorf("unexpected start notice %+v", e)
		}
	}
}

// SC-209 (silence): no check-ins for MIC_SILENCE_ALERT give one "not heard
// from since"; when they resume, one "back online".
func TestMicSilenceAlertsOnceAndRecovers(t *testing.T) {
	m := startMicE2E(t)

	m.checkInUntil(t, "the first check-in", func(sqlite.Recorder) bool { return true })
	m.passAlerts(t)
	m.wantAlerts(t)

	m.net.setRule(func(r *http.Request) micFault {
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/mic/v1/recorders/") {
			return faultDown
		}

		return faultNone
	})
	m.clock.tick(time.Minute) // a check-in that never arrives
	m.svc.advance(micSilence - time.Minute)
	m.passAlerts(t)
	m.wantAlerts(t) // not yet silent for MIC_SILENCE_ALERT

	m.svc.advance(2 * time.Minute)
	m.passAlerts(t)
	m.passAlerts(t)
	m.wantAlerts(t, "not heard from since 08:00")

	m.net.setRule(nil)
	m.checkInUntil(t, "a check-in after the silence", func(r sqlite.Recorder) bool {
		return r.LastSeen.Equal(m.svc.Now())
	})
	m.passAlerts(t)
	m.passAlerts(t)
	m.wantAlerts(t, "not heard from since 08:00", "back online")
}
