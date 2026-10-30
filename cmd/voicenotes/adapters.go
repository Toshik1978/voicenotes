package main

import (
	"context"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/memos"
	"github.com/Toshik1978/voicenotes/internal/openai"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
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
