package mic

import "testing"

func TestMatchReceiver(t *testing.T) {
	consumer := readFixture(t, "key_consumer_control.txt")
	keyboard := readFixture(t, "key_keyboard.txt")

	const (
		usbA = "/sys/devices/pci0000:00/0000:00:14.0/usb1/1-1"
		usbB = "/sys/devices/pci0000:00/0000:00:14.0/usb1/1-2"
		usbC = "/sys/devices/pci0000:00/0000:00:14.0/usb1/1-3"
	)

	tests := []struct {
		name string
		snap Snapshot
		want Receiver
		ok   bool
	}{
		{"empty", Snapshot{}, Receiver{}, false},
		{
			"card and button on one USB device",
			Snapshot{
				Cards:  []SoundCard{{Number: 2, HasCapture: true, USBParent: usbA}},
				Inputs: []InputDevice{{Path: "/dev/input/event7", KeyBitmap: consumer, USBParent: usbA}},
			},
			Receiver{Card: 2, InputPath: "/dev/input/event7"},
			true,
		},
		{
			"no capture PCM",
			Snapshot{
				Cards:  []SoundCard{{Number: 2, HasCapture: false, USBParent: usbA}},
				Inputs: []InputDevice{{Path: "/dev/input/event7", KeyBitmap: consumer, USBParent: usbA}},
			},
			Receiver{},
			false,
		},
		{
			"input without bit 115",
			Snapshot{
				Cards:  []SoundCard{{Number: 2, HasCapture: true, USBParent: usbA}},
				Inputs: []InputDevice{{Path: "/dev/input/event7", KeyBitmap: keyboard, USBParent: usbA}},
			},
			Receiver{},
			false,
		},
		{
			"different USB parents",
			Snapshot{
				Cards:  []SoundCard{{Number: 2, HasCapture: true, USBParent: usbA}},
				Inputs: []InputDevice{{Path: "/dev/input/event7", KeyBitmap: consumer, USBParent: usbB}},
			},
			Receiver{},
			false,
		},
		{
			"built-in card without USB parent is ignored",
			Snapshot{
				Cards:  []SoundCard{{Number: 0, HasCapture: true, USBParent: ""}},
				Inputs: []InputDevice{{Path: "/dev/input/event3", KeyBitmap: consumer, USBParent: ""}},
			},
			Receiver{},
			false,
		},
		{
			"laptop keyboard volume key does not pair with a USB mic",
			Snapshot{
				Cards: []SoundCard{
					{Number: 0, HasCapture: true, USBParent: ""},
					{Number: 3, HasCapture: true, USBParent: usbB},
				},
				Inputs: []InputDevice{{Path: "/dev/input/event3", KeyBitmap: consumer, USBParent: ""}},
			},
			Receiver{},
			false,
		},
		{
			"smallest parent path wins regardless of order",
			Snapshot{
				Cards: []SoundCard{
					{Number: 5, HasCapture: true, USBParent: usbC},
					{Number: 4, HasCapture: true, USBParent: usbB},
				},
				Inputs: []InputDevice{
					{Path: "/dev/input/event9", KeyBitmap: consumer, USBParent: usbC},
					{Path: "/dev/input/event8", KeyBitmap: consumer, USBParent: usbB},
				},
			},
			Receiver{Card: 4, InputPath: "/dev/input/event8"},
			true,
		},
		{
			"smaller parent incomplete, next complete one is chosen",
			Snapshot{
				Cards: []SoundCard{
					{Number: 1, HasCapture: true, USBParent: usbA},
					{Number: 4, HasCapture: true, USBParent: usbB},
				},
				Inputs: []InputDevice{
					{Path: "/dev/input/event1", KeyBitmap: keyboard, USBParent: usbA},
					{Path: "/dev/input/event8", KeyBitmap: consumer, USBParent: usbB},
				},
			},
			Receiver{Card: 4, InputPath: "/dev/input/event8"},
			true,
		},
		{
			"several inputs and cards on one parent: lowest card, then path",
			Snapshot{
				Cards: []SoundCard{
					{Number: 6, HasCapture: true, USBParent: usbA},
					{Number: 3, HasCapture: true, USBParent: usbA},
					{Number: 2, HasCapture: false, USBParent: usbA},
				},
				Inputs: []InputDevice{
					{Path: "/dev/input/event12", KeyBitmap: consumer, USBParent: usbA},
					{Path: "/dev/input/event11", KeyBitmap: consumer, USBParent: usbA},
					{Path: "/dev/input/event10", KeyBitmap: keyboard, USBParent: usbA},
				},
			},
			Receiver{Card: 3, InputPath: "/dev/input/event11"},
			true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := MatchReceiver(tc.snap)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("MatchReceiver = (%+v, %v), want (%+v, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}
