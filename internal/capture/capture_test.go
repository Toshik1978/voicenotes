package capture

import (
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name    string
		kind    Kind
		start   Step
		wantErr bool
	}{
		{"audio downloaded", KindAudio, StepExtract, false},
		{"audio not downloaded", KindAudio, StepDownload, false},
		{"files collect", KindFiles, StepCollect, false},
		{"files cannot start at download", KindFiles, StepDownload, true},
		{"files cannot start at extract", KindFiles, StepExtract, true},
		{"audio cannot start at collect", KindAudio, StepCollect, true},
		{"text cannot start at collect", KindText, StepCollect, true},
		{"text", KindText, StepEnrich, false},
		{"text cannot download", KindText, StepDownload, true},
		{"audio cannot start at enrich", KindAudio, StepEnrich, true},
		{"unknown kind", Kind("video"), StepDownload, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(tt.kind, tt.start, at)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if c.Step != tt.start || c.Status != StatusPending || c.Kind != tt.kind ||
				!c.ReceivedAt.Equal(at) || c.Attempts != 0 || !c.NextAttemptAt.Equal(at) {
				t.Errorf("unexpected capture %+v", c)
			}
		})
	}
}

func TestStepNextAndString(t *testing.T) {
	want := []Step{
		StepCollect,
		StepDownload,
		StepExtract,
		StepEnrich,
		StepWriteNote,
		StepFinalizeReply,
		StepCleanup,
		StepDone,
	}
	for i := range len(want) - 1 {
		if got := want[i].next(); got != want[i+1] {
			t.Errorf("%s.next() = %s, want %s", want[i], got, want[i+1])
		}
	}
	if StepDone.next() != StepDone {
		t.Error("done must be terminal")
	}
	if StepWriteNote.String() != "write_note" || StatusWaiting.String() != "waiting" || KindText.String() != "text" ||
		KindFiles.String() != "files" || StepExtract.String() != "extract" || StepCollect.String() != "collect" ||
		WaitingDocling != "docling" {
		t.Error("string forms must match the stored values")
	}
}
