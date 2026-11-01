package capture

// Lane is a worker lane: a set of captures one worker loop drains. Each
// capture belongs to exactly one lane at a time, derived from its persisted
// kind and step, so the lanes never touch the same capture concurrently.
type Lane string

// Worker lanes.
const (
	// LaneMain runs every step except document extraction.
	LaneMain Lane = "main"
	// LaneDocuments runs only the extract step of files captures, so a slow
	// document conversion never delays voice or text captures.
	LaneDocuments Lane = "documents"
)

func (l Lane) String() string { return string(l) }

// LaneOf returns the lane that runs the capture's next step.
func LaneOf(c Capture) Lane {
	if c.Kind == KindFiles && c.Step == StepExtract {
		return LaneDocuments
	}

	return LaneMain
}
