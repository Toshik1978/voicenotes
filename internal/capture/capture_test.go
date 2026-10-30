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
		{"audio downloaded", KindAudio, StepTranscribe, false},
		{"audio not downloaded", KindAudio, StepDownload, false},
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
	want := []Step{StepDownload, StepTranscribe, StepEnrich, StepWriteNote, StepFinalizeReply, StepCleanup, StepDone}
	for i := range len(want) - 1 {
		if got := want[i].next(); got != want[i+1] {
			t.Errorf("%s.next() = %s, want %s", want[i], got, want[i+1])
		}
	}
	if StepDone.next() != StepDone {
		t.Error("done must be terminal")
	}
	if StepWriteNote.String() != "write_note" || StatusWaiting.String() != "waiting" || KindText.String() != "text" {
		t.Error("string forms must match the stored values")
	}
}
