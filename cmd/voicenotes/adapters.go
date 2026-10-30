package main

import (
	"context"
	"fmt"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/enrich"
	"github.com/Toshik1978/voicenotes/internal/health"
	"github.com/Toshik1978/voicenotes/internal/memos"
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

	return pipeline.TranscribeResult{Text: res.Text, Language: res.Language}, out
}

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
