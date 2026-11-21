package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/mic"
	"github.com/Toshik1978/voicenotes/internal/openai"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
	"github.com/Toshik1978/voicenotes/internal/store/sqlite"
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

func TestAlertStoreAdaptsRecorders(t *testing.T) {
	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	seen := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	if err = db.UpsertRecorder(t.Context(), "bed", seen, true, false); err != nil {
		t.Fatal(err)
	}
	st := alertStore{db: db}
	both := mic.NewAlertSet(mic.AlertMissingReceiver, mic.AlertSilent)
	if err = st.SetRecorderAlert(t.Context(), "bed", both); err != nil {
		t.Fatal(err)
	}
	got, err := st.RecorderStatuses(t.Context())
	if err != nil || len(got) != 1 {
		t.Fatalf("got=%v err=%v", got, err)
	}
	want := mic.RecorderStatus{Name: "bed", LastSeen: seen, ReceiverPresent: true, Outstanding: both}
	if !got[0].LastSeen.Equal(seen) {
		t.Fatalf("last seen = %v", got[0].LastSeen)
	}
	got[0].LastSeen = want.LastSeen
	if got[0] != want {
		t.Fatalf("got %+v want %+v", got[0], want)
	}
	if rows, _ := db.Recorders(t.Context()); rows[0].Alert != "missing_receiver,silent" {
		t.Fatalf("stored alert = %q", rows[0].Alert)
	}
}

func TestAlertStoreRejectsUnknownStoredAlert(t *testing.T) {
	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = db.UpsertRecorder(t.Context(), "bed", time.Now(), true, false); err != nil {
		t.Fatal(err)
	}
	if err = db.SetRecorderAlert(t.Context(), "bed", "bogus"); err != nil {
		t.Fatal(err)
	}
	if _, err = (alertStore{db: db}).RecorderStatuses(t.Context()); err == nil {
		t.Fatal("want an error for an unknown stored alert")
	}
}
