package mic

import "slices"

// SoundCard is one ALSA card as seen in sysfs.
type SoundCard struct {
	Number     int
	HasCapture bool   // a pcmC<Number>D*c node exists
	USBParent  string // sysfs path of the owning USB device; empty if not USB
}

// InputDevice is one /dev/input/eventN as seen in sysfs.
type InputDevice struct {
	Path      string // /dev/input/eventN
	KeyBitmap string // contents of capabilities/key
	USBParent string // sysfs path of the owning USB device; empty if not USB
}

// Snapshot is the discovery input: every card and input device found.
type Snapshot struct {
	Cards  []SoundCard
	Inputs []InputDevice
}

// Receiver identifies a matched capture card and its button device.
type Receiver struct {
	Card      int
	InputPath string
}

// MatchReceiver returns the receiver on the lexicographically smallest USB
// parent path that has both a capture PCM and an input with KEY_VOLUMEUP.
// Within a parent the lowest card number and then the smallest input path
// win, so the result does not depend on the order of the snapshot. Devices
// with no USB parent never match.
func MatchReceiver(s Snapshot) (Receiver, bool) {
	cards := captureCards(s.Cards)
	inputs := buttonInputs(s.Inputs)

	var parents []string
	for p := range cards {
		if _, ok := inputs[p]; ok {
			parents = append(parents, p)
		}
	}

	if len(parents) == 0 {
		return Receiver{}, false
	}

	slices.Sort(parents)

	return Receiver{Card: cards[parents[0]], InputPath: inputs[parents[0]]}, true
}

// captureCards maps each USB parent to its lowest capture card number.
func captureCards(cards []SoundCard) map[string]int {
	out := map[string]int{}

	for _, c := range cards {
		if c.USBParent == "" || !c.HasCapture {
			continue
		}

		if n, ok := out[c.USBParent]; !ok || c.Number < n {
			out[c.USBParent] = c.Number
		}
	}

	return out
}

// buttonInputs maps each USB parent to its smallest input path that has
// KEY_VOLUMEUP.
func buttonInputs(inputs []InputDevice) map[string]string {
	out := map[string]string{}

	for _, in := range inputs {
		if in.USBParent == "" || !HasKey(in.KeyBitmap, KeyVolumeUp) {
			continue
		}

		if p, ok := out[in.USBParent]; !ok || in.Path < p {
			out[in.USBParent] = in.Path
		}
	}

	return out
}
