package micdev

import (
	"os"
	"testing"

	"github.com/Toshik1978/voicenotes/internal/mic"
)

const (
	pci  = "devices/pci0000:00/0000:00:1f.3"
	usb2 = "devices/pci0000:00/0000:00:14.0/usb1/1-1/1-1.2"
	usb3 = "devices/pci0000:00/0000:00:14.0/usb1/1-1/1-1.3"
)

// fakeReal maps the fixture entries to the device paths the symlinks in a
// real sysfs would resolve to.
func fakeReal(m map[string]string) func(string) (string, error) {
	return func(rel string) (string, error) {
		if p, ok := m[rel]; ok {
			return p, nil
		}

		return "", os.ErrNotExist
	}
}

func fixtureSys(links map[string]string) Sysfs {
	return Sysfs{FS: os.DirFS("testdata/sys"), Real: fakeReal(links)}
}

func fullTree() map[string]string {
	return map[string]string{
		"class/sound/card0":  pci + "/sound/card0",
		"class/sound/card1":  usb2 + "/1-1.2:1.0/sound/card1",
		"class/sound/card2":  usb3 + "/1-1.3:1.0/sound/card2",
		"class/input/event2": pci + "/input/input2/event2",
		"class/input/event3": usb2 + "/1-1.2:1.3/0003:0001:0002.0004/input/input9/event3",
		"class/input/event4": usb3 + "/1-1.3:1.3/0003:0001:0003.0005/input/input10/event4",
	}
}

func TestDiscover(t *testing.T) {
	tests := []struct {
		name string
		real map[string]string
		want mic.Receiver
		ok   bool
	}{
		{
			"receiver matched on the USB device with capture and button",
			fullTree(),
			mic.Receiver{Card: 1, InputPath: "/dev/input/event3"},
			true,
		},
		{
			"nothing resolves, so nothing is USB",
			map[string]string{},
			mic.Receiver{},
			false,
		},
		{
			"only built-in devices",
			map[string]string{
				"class/sound/card1":  pci + "/sound/card1",
				"class/input/event3": pci + "/input/input9/event3",
			},
			mic.Receiver{},
			false,
		},
		{
			"button and capture on different USB devices",
			map[string]string{
				"class/sound/card1":  usb2 + "/1-1.2:1.0/sound/card1",
				"class/input/event3": usb3 + "/1-1.3:1.3/input/input9/event3",
			},
			mic.Receiver{},
			false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := Discover{Sys: fixtureSys(tc.real)}.Discover()
			if err != nil {
				t.Fatal(err)
			}

			if ok != tc.ok || got != tc.want {
				t.Fatalf("got %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestDiscoverMissingSubsystems(t *testing.T) {
	d := Discover{Sys: Sysfs{FS: os.DirFS(t.TempDir()), Real: fakeReal(nil)}}

	if _, ok, err := d.Discover(); ok || err != nil {
		t.Fatalf("ok=%v err=%v; want no receiver and no error", ok, err)
	}
}

func TestUSBAncestor(t *testing.T) {
	tests := []struct{ in, want string }{
		{usb2 + "/1-1.2:1.0/sound/card1", usb2},
		{"devices/pci0000:00/0000:00:14.0/usb1/1-1/1-1.2/1-1.2:1.3/input/input9/event3", usb2},
		{"devices/pci0000:00/0000:00:14.0/usb1/1-1/1-1:1.0/sound/card3", "devices/pci0000:00/0000:00:14.0/usb1/1-1"},
		{"devices/pci0000:00/0000:00:14.0/usb1/usb1:1.0/input/event9", ""},
		{pci + "/sound/card0", ""},
	}

	for _, tc := range tests {
		if got := usbAncestor(tc.in); got != tc.want {
			t.Errorf("usbAncestor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHostSysfsResolvesSymlinks(t *testing.T) {
	root := t.TempDir()
	dev := root + "/devices/usb1/1-1/1-1.2:1.0/sound/card1"

	if err := os.MkdirAll(dev, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(root+"/class/sound", 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink("../../devices/usb1/1-1/1-1.2:1.0/sound/card1", root+"/class/sound/card1"); err != nil {
		t.Fatal(err)
	}

	got, err := hostSysfs(root).Real("class/sound/card1")
	if err != nil {
		t.Fatal(err)
	}

	if want := "devices/usb1/1-1/1-1.2:1.0/sound/card1"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	if _, err := hostSysfs(root).Real("class/sound/card9"); err == nil {
		t.Fatal("a missing entry resolved")
	}
}

func TestFixed(t *testing.T) {
	got, ok, err := Fixed{CaptureDevice: "Example Mic", InputPath: "/dev/input/event5"}.Discover()
	want := mic.Receiver{InputPath: "/dev/input/event5", CaptureDevice: "Example Mic"}

	if err != nil || !ok || got != want {
		t.Fatalf("got %+v, %v, %v; want %+v", got, ok, err, want)
	}
}
