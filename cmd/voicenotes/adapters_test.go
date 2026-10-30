package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/openai"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

func TestSTTShimPassesTheExtension(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, fh, err := r.FormFile("file"); err == nil {
			got = fh.Filename
		}
		_, _ = io.WriteString(w, `{"text":"hi"}`)
	}))
	t.Cleanup(srv.Close)
	tr := openai.NewTranscriber(openai.Config{BaseURL: srv.URL, Model: "m", ResponseFormat: "json"}, srv.Client())

	res, out := sttShim{t: tr}.Transcribe(context.Background(),
		pipeline.TranscribeRequest{ID: "3", Ext: ".m4a", Audio: []byte("a")})
	if out.Class != capture.ClassOK || res.Text != "hi" || got != "capture-3.m4a" {
		t.Errorf("out %+v res %+v filename %q", out, res, got)
	}
}
