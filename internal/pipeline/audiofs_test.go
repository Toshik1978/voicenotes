package pipeline_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

func TestDirAudio(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "audio")
	a := pipeline.NewDirAudio(dir)

	if ok, err := a.Exists("1.ogg"); err != nil || ok {
		t.Fatalf("exists before write: %v %v", ok, err)
	}
	if err := a.Write("1.ogg", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := a.Write("1.ogg", []byte("y")); err != nil {
		t.Fatal(err)
	}
	got, err := a.Read("1.ogg")
	if err != nil || string(got) != "y" {
		t.Fatalf("read %q %v", got, err)
	}
	if ok, _ := a.Exists("1.ogg"); !ok {
		t.Error("should exist")
	}
	if err := a.Remove("1.ogg"); err != nil {
		t.Fatal(err)
	}
	if err := a.Remove("1.ogg"); err != nil {
		t.Errorf("remove missing: %v", err)
	}
	if _, err := a.Read("1.ogg"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("read missing: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("leftovers: %v", entries)
	}
	for _, bad := range []string{"../x", "a/b", ""} {
		if err := a.Write(bad, nil); err == nil {
			t.Errorf("write %q should fail", bad)
		}
	}
}
