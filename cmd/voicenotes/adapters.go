package main

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/config"
	"github.com/Toshik1978/voicenotes/internal/docling"
	"github.com/Toshik1978/voicenotes/internal/enrich"
	"github.com/Toshik1978/voicenotes/internal/health"
	"github.com/Toshik1978/voicenotes/internal/memos"
	"github.com/Toshik1978/voicenotes/internal/mic"
	"github.com/Toshik1978/voicenotes/internal/micapi"
	"github.com/Toshik1978/voicenotes/internal/openai"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
	"github.com/Toshik1978/voicenotes/internal/store/sqlite"
)

// sttShim adapts the OpenAI-compatible transcriber to pipeline.Transcriber,
// which uses its own request and result types so that pipeline imports no
// adapter.
type sttShim struct{ t *openai.Transcriber }

var _ pipeline.Transcriber = sttShim{}

func (s sttShim) Transcribe(
	ctx context.Context, r pipeline.TranscribeRequest,
) (pipeline.TranscribeResult, capture.Outcome) {
	res, out := s.t.Transcribe(ctx, openai.Request{ID: r.ID, Ext: r.Ext, Audio: r.Audio, Language: r.Language})

	return pipeline.TranscribeResult{Text: res.Text, Language: res.Language, NoSpeechProbs: res.NoSpeechProbs}, out
}

// HasSpeech adapts the client's voice activity check to pipeline.VoiceDetector.
func (s sttShim) HasSpeech(ctx context.Context, r pipeline.TranscribeRequest) (bool, capture.Outcome) {
	return s.t.HasSpeech(ctx, openai.Request{ID: r.ID, Ext: r.Ext, Audio: r.Audio, Language: r.Language})
}

// doclingShim adapts the docling client to pipeline.Extractor, whose result
// type keeps pipeline free of the adapter.
type doclingShim struct{ c *docling.Client }

var _ pipeline.Extractor = doclingShim{}

func (d doclingShim) Convert(
	ctx context.Context, name, mime string, data []byte,
) (pipeline.ExtractResult, capture.Outcome) {
	res, out := d.c.Convert(ctx, name, mime, data)

	return pipeline.ExtractResult{Markdown: res.Markdown, TimedOut: res.TimedOut}, out
}

// The vision client already speaks pipeline.Describer: it needs no shim.
var _ pipeline.Describer = (*openai.Vision)(nil)

// notesShim adapts the Memos client to pipeline.NotesWriter.
type notesShim struct{ c *memos.Client }

var _ pipeline.NotesWriter = notesShim{}

func (n notesShim) Create(ctx context.Context, in pipeline.NoteInput) (string, string, capture.Outcome) {
	return n.c.Create(ctx, memos.Note{CaptureID: in.CaptureID, Content: in.Content, ReceivedAt: in.ReceivedAt})
}

func (n notesShim) Attach(ctx context.Context, name, filename, mime string, data []byte) capture.Outcome {
	return n.c.Attach(ctx, name, filename, mime, data)
}

// llmShim adapts the OpenAI-compatible chat client to pipeline.Enricher,
// mapping the enrich messages onto the client's.
type llmShim struct{ c *openai.Chat }

var _ pipeline.Enricher = llmShim{}

func (l llmShim) Complete(ctx context.Context, msgs []enrich.Message) (string, capture.Outcome) {
	out := make([]openai.Message, len(msgs))
	for i, m := range msgs {
		out[i] = openai.Message{Role: m.Role, Content: m.Content}
	}

	return l.c.Complete(ctx, out)
}

// healthQueue adapts the SQLite store to health.Queue.
type healthQueue struct{ db *sqlite.DB }

var _ health.Queue = healthQueue{}

func (q healthQueue) CountUnfinished(ctx context.Context) (int, error) {
	n, err := q.db.CountUnfinished(ctx)
	if err != nil {
		return 0, fmt.Errorf("health: %w", err)
	}

	return n, nil
}

func (q healthQueue) OldestUnfinished(ctx context.Context) (*health.Oldest, error) {
	c, ok, err := q.db.OldestUnfinished(ctx)
	if err != nil {
		return nil, fmt.Errorf("health: %w", err)
	}
	if !ok {
		return nil, nil //nolint:nilnil // nil means an empty queue.
	}

	return &health.Oldest{ReceivedAt: c.ReceivedAt, WaitingOn: string(c.WaitingOn), Misconfig: c.Misconfig}, nil
}

func (q healthQueue) LastDigestDate(ctx context.Context) (string, error) {
	date, _, err := q.db.LastSentDigestDate(ctx)
	if err != nil {
		return "", fmt.Errorf("health: %w", err)
	}

	return date, nil
}

// healthRecorders adapts the SQLite store to health.Recorders.
type healthRecorders struct{ db *sqlite.DB }

var _ health.Recorders = healthRecorders{}

func (r healthRecorders) RecorderAlerts(ctx context.Context) ([]health.RecorderAlert, error) {
	rows, err := r.db.Recorders(ctx)
	if err != nil {
		return nil, fmt.Errorf("health: %w", err)
	}
	out := make([]health.RecorderAlert, len(rows))
	for i, rec := range rows {
		out[i] = health.RecorderAlert{Name: rec.Name, Alert: rec.Alert, LastSeen: rec.LastSeen}
	}

	return out, nil
}

// startReplies adapts the sqlite start-reply store to
// pipeline.StartReplyStore, which uses its own StartReply type.
type startReplies struct{ db *sqlite.DB }

func (s startReplies) StaleStartReplies(ctx context.Context, olderThan time.Time) ([]pipeline.StartReply, error) {
	rows, err := s.db.StaleStartReplies(ctx, olderThan)
	if err != nil {
		return nil, fmt.Errorf("stale start replies: %w", err)
	}
	out := make([]pipeline.StartReply, len(rows))
	for i, r := range rows {
		out[i] = pipeline.StartReply{RecordingID: r.RecordingID, ChatID: r.ChatID, MessageID: r.MessageID}
	}

	return out, nil
}

//nolint:revive // the four results are the specified signature
func (s startReplies) TakeStartReply(
	ctx context.Context,
	recordingID int64,
) (chatID, messageID int64, ok bool, err error) {
	return s.db.TakeStartReply(ctx, recordingID) //nolint:wrapcheck // pass-through adapter
}

// heldPurger purges done captures and forgets retired recorders through the
// store, and purges held no-speech audio through HeldAudio.
type heldPurger struct {
	*sqlite.DB
	pipeline.HeldAudio
}

// micPurger adds the start-reply janitor to heldPurger.
type micPurger struct {
	heldPurger
	pipeline.StartReplyJanitor
}

// purgerFor returns the purge target of PurgeLoop: the store (done captures
// and recorders not seen for pipeline.RecorderRetention) plus the held audio
// in heldDir (both purged even with the microphone off, so what was left
// before it was disabled still goes) and, with the microphone enabled, the
// janitor that marks stale start messages.
//
//nolint:ireturn // the result is either implementation
func purgerFor(
	cfg config.Config, db *sqlite.DB, replier pipeline.Replier, heldDir string, log *zap.Logger,
) pipeline.Purger {
	held := heldPurger{db, pipeline.HeldAudio{Dir: heldDir, Hold: cfg.MicNoSpeechHold}}
	if cfg.MicUploadToken == "" {
		return held
	}
	janitor := pipeline.StartReplyJanitor{Store: startReplies{db: db}, Replier: replier, Log: log}

	return micPurger{held, janitor}
}

// alertStore adapts the SQLite store to micapi.AlertStore, which speaks mic
// types so that micapi imports no adapter.
type alertStore struct{ db *sqlite.DB }

var _ micapi.AlertStore = alertStore{}

func (a alertStore) RecorderStatuses(ctx context.Context) ([]mic.RecorderStatus, error) {
	rows, err := a.db.Recorders(ctx)
	if err != nil {
		return nil, fmt.Errorf("recorders: %w", err)
	}
	out := make([]mic.RecorderStatus, len(rows))
	for i, r := range rows {
		set, err := mic.ParseAlertSet(r.Alert)
		if err != nil {
			return nil, fmt.Errorf("recorder %q: %w", r.Name, err)
		}
		out[i] = mic.RecorderStatus{
			Name: r.Name, LastSeen: r.LastSeen, ReceiverPresent: r.ReceiverPresent,
			StorageFull: r.StorageFull, Outstanding: set,
		}
	}

	return out, nil
}

func (a alertStore) SetRecorderAlert(ctx context.Context, name string, alerts mic.AlertSet) error {
	if err := a.db.SetRecorderAlert(ctx, name, alerts.String()); err != nil {
		return fmt.Errorf("set recorder alert: %w", err)
	}

	return nil
}
