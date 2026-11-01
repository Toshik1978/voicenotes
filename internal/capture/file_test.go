package capture

import "testing"

func TestDoclingTimeout(t *testing.T) {
	tests := []struct {
		name         string
		before       int
		wantTimeouts int
		wantClass    Class
		wantState    FileState
		wantFailure  string
		wantWaiting  WaitingOn
	}{
		{"first timeout is temporary", 0, 1, ClassTemporary, FilePending, "", WaitingDocling},
		{"second timeout is temporary", 1, 2, ClassTemporary, FilePending, "", WaitingDocling},
		{"third timeout is permanent", 2, 3, ClassPermanent, FileFailed, "couldn't read in time", ""},
		{"later timeouts stay permanent", 3, 4, ClassPermanent, FileFailed, "couldn't read in time", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &File{Timeouts: tt.before, State: FilePending}
			got := DoclingTimeout(f)
			if got.Class != tt.wantClass {
				t.Errorf("class = %v, want %v", got.Class, tt.wantClass)
			}
			if got.WaitingOn != tt.wantWaiting {
				t.Errorf("waiting = %q, want %q", got.WaitingOn, tt.wantWaiting)
			}
			if f.Timeouts != tt.wantTimeouts {
				t.Errorf("timeouts = %d, want %d", f.Timeouts, tt.wantTimeouts)
			}
			if f.State != tt.wantState {
				t.Errorf("state = %q, want %q", f.State, tt.wantState)
			}
			if f.Failure != tt.wantFailure {
				t.Errorf("failure = %q, want %q", f.Failure, tt.wantFailure)
			}
		})
	}
}
