package telegram_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
	"github.com/Toshik1978/voicenotes/internal/telegram"
)

const (
	owner    int64 = 111111111
	stranger int64 = 222222222
	chat     int64 = -1001234567890
	secret         = "remember the dentist on thursday"
)

func date() time.Time { return time.Date(2026, 10, 30, 7, 15, 0, 0, time.UTC) }

// events is the shared, ordered log of every side effect across the fakes,
// so tests can assert that ⏳ is sent only after the row is committed.
type events struct {
	mu  sync.Mutex
	log []string
}

func (e *events) addf(format string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, fmt.Sprintf(format, args...))
}

func (e *events) get() []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	return slices.Clone(e.log)
}

type sent struct {
	chat, replyTo int64
	text          string
}

type fakeReplier struct {
	ev      *events
	sendErr error
	sent    []sent
}

func (f *fakeReplier) Send(_ context.Context, chatID, replyTo int64, text string) (int64, error) {
	f.ev.addf("send %q", text)
	if f.sendErr != nil {
		return 0, f.sendErr
	}
	f.sent = append(f.sent, sent{chatID, replyTo, text})

	return 9000 + int64(len(f.sent)), nil
}

func (f *fakeReplier) Edit(_ context.Context, _, msgID int64, text string) (int64, error) {
	f.ev.addf("edit %d %q", msgID, text)

	return msgID, nil
}

type fakeDownloader struct {
	ev   *events
	data []byte
	err  error
}

func (f *fakeDownloader) Download(_ context.Context, fileID string) ([]byte, error) {
	f.ev.addf("download %s", fileID)
	if f.err != nil {
		return nil, f.err
	}

	return f.data, nil
}

type fakeIngester struct {
	ev       *events
	err      error
	audioDir string
	got      []capture.Capture
	// withAck records whether each ingest carried an acknowledgement.
	withAck []bool
	// fileAtInsert records whether the audio file was on disk at insert.
	fileAtInsert []bool
}

// Ingest mirrors pipeline.Ingester: insert, then the acknowledgement, then
// the worker wake, which is logged so tests see the ⏳ precede it.
func (f *fakeIngester) Ingest(ctx context.Context, c capture.Capture, ack pipeline.Ack) (capture.Capture, error) {
	f.ev.addf("ingest %s", c.Step)
	f.got = append(f.got, c)
	f.withAck = append(f.withAck, ack != nil)
	if c.AudioPath != "" {
		_, err := os.Stat(filepath.Join(f.audioDir, c.AudioPath))
		f.fileAtInsert = append(f.fileAtInsert, err == nil)
	}
	if f.err != nil {
		return capture.Capture{}, f.err
	}
	c.ID = 7
	if ack != nil {
		ack(ctx, c)
	}
	f.ev.addf("wake")

	return c, nil
}

type fakeReplies struct {
	ev  *events
	err error
}

func (f *fakeReplies) SetReplyMessageID(_ context.Context, id, msgID int64) error {
	f.ev.addf("reply %d=%d", id, msgID)

	return f.err
}

type rig struct {
	ev   *events
	rep  *fakeReplier
	dl   *fakeDownloader
	in   *fakeIngester
	rs   *fakeReplies
	dir  string
	logs *observer.ObservedLogs
	h    *telegram.Handler
}

func newRig(t *testing.T) *rig {
	t.Helper()

	ev := &events{}
	dir := filepath.Join(t.TempDir(), "audio")
	core, logs := observer.New(zapcore.DebugLevel)
	r := &rig{
		ev:   ev,
		rep:  &fakeReplier{ev: ev},
		dl:   &fakeDownloader{ev: ev, data: []byte("OggS-fake-opus")},
		in:   &fakeIngester{ev: ev, audioDir: dir},
		rs:   &fakeReplies{ev: ev},
		dir:  dir,
		logs: logs,
	}
	h, err := telegram.NewHandler(telegram.HandlerConfig{
		AllowedUsers: []int64{owner},
		AudioDir:     dir,
		Replier:      r.rep,
		Downloader:   r.dl,
		Ingester:     r.in,
		Replies:      r.rs,
		Log:          zap.New(core),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.h = h

	return r
}

// handle runs the handler and checks that no log line leaks the text.
func (r *rig) handle(t *testing.T, m telegram.Message) {
	t.Helper()

	r.h.Handle(context.Background(), m)
	all := r.logs.All()
	for i := range all {
		line := all[i].Message + fmt.Sprint(all[i].ContextMap())
		if strings.Contains(line, secret) || strings.Contains(line, "FAKE-TOKEN") {
			t.Fatalf("log leaks content: %s", line)
		}
	}
}

func (r *rig) audioFiles(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	return names
}

func textMsg(from int64, text string) telegram.Message {
	return telegram.Message{ChatID: chat, ID: 42, SenderID: from, Date: date(), Text: text}
}

func voiceMsg(size int64) telegram.Message {
	return telegram.Message{
		ChatID: chat, ID: 43, SenderID: owner, Date: date(),
		Audio: &telegram.Audio{FileID: "FAKE-FILE-ID", MimeType: "audio/ogg", Size: size, Voice: true},
	}
}

func wantEvents(t *testing.T, r *rig, want ...string) {
	t.Helper()

	if got := r.ev.get(); !slices.Equal(got, want) {
		t.Fatalf("events\n got %q\nwant %q", got, want)
	}
}

func TestNewHandlerValidates(t *testing.T) {
	ev := &events{}
	ok := telegram.HandlerConfig{
		AudioDir:   filepath.Join(t.TempDir(), "a", "audio"),
		Replier:    &fakeReplier{ev: ev},
		Downloader: &fakeDownloader{ev: ev},
		Ingester:   &fakeIngester{ev: ev},
		Replies:    &fakeReplies{ev: ev},
	}
	if _, err := telegram.NewHandler(ok); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if fi, err := os.Stat(ok.AudioDir); err != nil || !fi.IsDir() {
		t.Fatalf("audio dir not created: %v", err)
	}

	for name, mut := range map[string]func(*telegram.HandlerConfig){
		"audio dir":  func(c *telegram.HandlerConfig) { c.AudioDir = "" },
		"replier":    func(c *telegram.HandlerConfig) { c.Replier = nil },
		"downloader": func(c *telegram.HandlerConfig) { c.Downloader = nil },
		"ingester":   func(c *telegram.HandlerConfig) { c.Ingester = nil },
		"replies":    func(c *telegram.HandlerConfig) { c.Replies = nil },
	} {
		c := ok
		mut(&c)
		if _, err := telegram.NewHandler(c); err == nil {
			t.Errorf("%s: want error", name)
		}
	}

	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	bad := ok
	bad.AudioDir = filepath.Join(file, "audio")
	if _, err := telegram.NewHandler(bad); err == nil {
		t.Error("uncreatable audio dir: want error")
	}
}

func TestStrangerIsIgnored(t *testing.T) {
	r := newRig(t)

	r.handle(t, textMsg(stranger, secret))
	m := voiceMsg(1000)
	m.SenderID = stranger
	r.handle(t, m)

	wantEvents(t, r)
}

func TestTextCommitsThenAcks(t *testing.T) {
	r := newRig(t)

	r.handle(t, textMsg(owner, secret))

	wantEvents(t, r, `ingest enrich`, `send "⏳"`, `reply 7=9001`, `wake`)
	c := r.in.got[0]
	if c.Kind != capture.KindText || c.Text != secret || c.Source != telegram.Source ||
		c.SourceChatID != chat || c.SourceMessageID != 42 || !c.ReceivedAt.Equal(date()) {
		t.Fatalf("capture = %+v", c)
	}
	if s := r.rep.sent[0]; s.chat != chat || s.replyTo != 42 {
		t.Fatalf("⏳ must reply to the capture, got %+v", s)
	}
}

func TestTextDuplicateGetsNoReply(t *testing.T) {
	r := newRig(t)
	r.in.err = pipeline.ErrDuplicate

	r.handle(t, textMsg(owner, secret))

	wantEvents(t, r, `ingest enrich`)
}

const notSaved = `send "⚠ not saved — please send it again."`

func TestTextStoreErrorAsksToResend(t *testing.T) {
	r := newRig(t)
	r.in.err = errors.New("fake: disk full")

	r.handle(t, textMsg(owner, secret))

	wantEvents(t, r, `ingest enrich`, notSaved)
	if s := r.rep.sent[0]; s.replyTo != 42 {
		t.Fatalf("reply = %+v", s)
	}
	if r.logs.FilterLevelExact(zapcore.ErrorLevel).Len() == 0 {
		t.Fatal("lost capture must be logged as an error")
	}
}

func TestAckFailureKeepsRow(t *testing.T) {
	r := newRig(t)
	r.rep.sendErr = errors.New("fake: telegram down")

	r.handle(t, textMsg(owner, secret))

	wantEvents(t, r, `ingest enrich`, `send "⏳"`, `wake`)
}

func TestReplyRecordFailureIsLogged(t *testing.T) {
	r := newRig(t)
	r.rs.err = errors.New("fake: store busy")

	r.handle(t, textMsg(owner, secret))

	wantEvents(t, r, `ingest enrich`, `send "⏳"`, `reply 7=9001`, `wake`)
	if r.logs.FilterMessageSnippet("reply").Len() == 0 {
		t.Fatal("failed reply record must be logged")
	}
}

func TestVoiceDownloadsWritesCommitsThenAcks(t *testing.T) {
	r := newRig(t)

	r.handle(t, voiceMsg(1000))

	wantEvents(t, r, `download FAKE-FILE-ID`, `ingest transcribe`, `send "⏳"`, `reply 7=9001`, `wake`)
	c := r.in.got[0]
	if c.Kind != capture.KindAudio || c.FileID != "FAKE-FILE-ID" || c.MimeType != "audio/ogg" ||
		c.FileSize != 1000 || c.SourceMessageID != 43 || c.Source != telegram.Source {
		t.Fatalf("capture = %+v", c)
	}
	if !r.in.fileAtInsert[0] {
		t.Fatal("audio must be on disk before the row is inserted")
	}
	if filepath.Base(c.AudioPath) != c.AudioPath ||
		!strings.HasPrefix(c.AudioPath, fmt.Sprintf("tg-%d-43-", chat)) || !strings.HasSuffix(c.AudioPath, ".ogg") {
		t.Fatalf("audio path %q: want relative tg-<chat>-<msg>-*.ogg", c.AudioPath)
	}
	data, err := os.ReadFile(filepath.Join(r.dir, c.AudioPath))
	if err != nil || !bytes.Equal(data, r.dl.data) {
		t.Fatalf("audio file: %q, %v", data, err)
	}
}

func TestAudioFileExtensionAndSize(t *testing.T) {
	r := newRig(t)
	m := voiceMsg(0)
	m.Audio.Voice, m.Audio.MimeType = false, "audio/mpeg"

	r.handle(t, m)

	c := r.in.got[0]
	if !strings.HasSuffix(c.AudioPath, ".mp3") {
		t.Fatalf("audio/mpeg path %q: want .mp3", c.AudioPath)
	}
	if c.FileSize != int64(len(r.dl.data)) {
		t.Fatalf("unknown size must fall back to the bytes read, got %d", c.FileSize)
	}
}

func TestVoiceDuplicateRemovesFile(t *testing.T) {
	r := newRig(t)
	r.in.err = pipeline.ErrDuplicate

	r.handle(t, voiceMsg(1000))

	wantEvents(t, r, `download FAKE-FILE-ID`, `ingest transcribe`)
	if files := r.audioFiles(t); len(files) != 0 {
		t.Fatalf("duplicate left files %v", files)
	}
}

func TestVoiceStoreErrorRemovesFileAndAsksToResend(t *testing.T) {
	r := newRig(t)
	r.in.err = errors.New("fake: disk full")

	r.handle(t, voiceMsg(1000))

	wantEvents(t, r, `download FAKE-FILE-ID`, `ingest transcribe`, notSaved)
	if files := r.audioFiles(t); len(files) != 0 {
		t.Fatalf("failed insert left files %v", files)
	}
}

func TestVoiceDownloadFailureCommitsDownloadStep(t *testing.T) {
	r := newRig(t)
	r.dl.err = errors.New("fake: file api down")

	r.handle(t, voiceMsg(1000))

	wantEvents(t, r, `download FAKE-FILE-ID`, `ingest download`, `wake`)
	if r.in.withAck[0] {
		t.Fatal("no ⏳ before the audio is on disk: the download step sends it")
	}
	c := r.in.got[0]
	if c.FileID != "FAKE-FILE-ID" || c.AudioPath != "" || c.Step != capture.StepDownload {
		t.Fatalf("capture = %+v", c)
	}
}

func TestVoiceWriteFailureCommitsDownloadStep(t *testing.T) {
	r := newRig(t)
	if err := os.Chmod(r.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(r.dir, 0o700) })
	if f, err := os.CreateTemp(r.dir, "probe"); err == nil {
		_ = f.Close()
		t.Skip("directory permissions are not enforced (running as root?)")
	}

	r.handle(t, voiceMsg(1000))

	wantEvents(t, r, `download FAKE-FILE-ID`, `ingest download`, `wake`)
	if c := r.in.got[0]; c.AudioPath != "" {
		t.Fatalf("capture = %+v", c)
	}
}

func TestTooLargeIsRejected(t *testing.T) {
	r := newRig(t)

	r.handle(t, voiceMsg(telegram.MaxFileSize))
	r.ev.log = nil
	r.rep.sent = nil
	r.handle(t, voiceMsg(telegram.MaxFileSize+1))

	wantEvents(t, r, `send "⚠ too large for Telegram bots (max 20 MB) — not saved."`)
	if s := r.rep.sent[0]; s.replyTo != 43 {
		t.Fatalf("reply = %+v", s)
	}
}

func TestCommandsAndUnsupported(t *testing.T) {
	const help = "VoiceNotes: send a voice or text message to save it as a note."
	const unsupported = "VoiceNotes takes voice or text only."

	for _, tc := range []struct {
		text string
		want string
	}{
		{"/start", help},
		{"/help", help},
		{"/help@VoiceNotesBot", help},
		{"/start payload", help},
		{"", unsupported},
		{"   ", unsupported},
	} {
		r := newRig(t)

		r.handle(t, textMsg(owner, tc.text))

		wantEvents(t, r, fmt.Sprintf("send %q", tc.want))
		if s := r.rep.sent[0]; s.replyTo != 42 {
			t.Fatalf("%q: reply = %+v", tc.text, s)
		}
	}
}

func TestTooLargeDiscoveredAtDownloadIsRejected(t *testing.T) {
	r := newRig(t)
	r.dl.err = fmt.Errorf("wrapped: %w", telegram.ErrTooLarge)

	r.handle(t, voiceMsg(0))

	wantEvents(t, r, `download FAKE-FILE-ID`, `send "⚠ too large for Telegram bots (max 20 MB) — not saved."`)
}

func TestUndownloadedStoreErrorAsksToResend(t *testing.T) {
	r := newRig(t)
	r.dl.err = errors.New("fake: file api down")
	r.in.err = errors.New("fake: disk full")

	r.handle(t, voiceMsg(1000))

	wantEvents(t, r, `download FAKE-FILE-ID`, `ingest download`, notSaved)
}
