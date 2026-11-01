package capture

import "testing"

func TestLaneOf(t *testing.T) {
	tests := []struct {
		name string
		kind Kind
		step Step
		want Lane
	}{
		{"files at extract is documents", KindFiles, StepExtract, LaneDocuments},
		{"files at collect is main", KindFiles, StepCollect, LaneMain},
		{"files at enrich is main", KindFiles, StepEnrich, LaneMain},
		{"files at done is main", KindFiles, StepDone, LaneMain},
		{"audio at extract is main", KindAudio, StepExtract, LaneMain},
		{"audio at download is main", KindAudio, StepDownload, LaneMain},
		{"text at enrich is main", KindText, StepEnrich, LaneMain},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Capture{Kind: tt.kind, Step: tt.step}
			if got := LaneOf(c); got != tt.want {
				t.Errorf("LaneOf(%s at %s) = %q, want %q", tt.kind, tt.step, got, tt.want)
			}
		})
	}
}
