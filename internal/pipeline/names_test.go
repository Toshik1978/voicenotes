package pipeline_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

func TestSanitizeFileName(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain", "invoice-2026-10.pdf", "invoice-2026-10.pdf"},
		{"control characters removed", "in\x00vo\x1fice\n.pdf", "invoice.pdf"},
		{"directories dropped", "../../etc/passwd.txt", "passwd.txt"},
		{"backslash directories dropped", `C:\docs\a.docx`, "a.docx"},
		{"only dots", "..", ""},
		{"empty", "", ""},
		{"only control", "\x00\x01", ""},
		{"unicode kept", "счёт.pdf", "счёт.pdf"},
		{"hostile extension dropped", "a.\x00\x01", "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pipeline.SanitizeFileName(tt.in); got != tt.want {
				t.Errorf("pipeline.SanitizeFileName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSanitizeFileNameLimit(t *testing.T) {
	got := pipeline.SanitizeFileName(strings.Repeat("я", 80) + ".pdf") // 160 bytes of base
	if len(got) > 100 || !strings.HasSuffix(got, ".pdf") {
		t.Errorf("len %d, name %q", len(got), got)
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Errorf("cut inside a rune: %q", got)
	}
}

func TestSanitizeExt(t *testing.T) {
	tests := []struct{ in, want string }{
		{".pdf", ".pdf"},
		{"", ""},
		{".p\x00df", ".pdf"},
		{".a/b", ".ab"},
		{"." + strings.Repeat("x", 40), ""},
	}
	for _, tt := range tests {
		if got := pipeline.SanitizeExt(tt.in); got != tt.want {
			t.Errorf("pipeline.SanitizeExt(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestAttachmentNames(t *testing.T) {
	recv := time.Date(2026, 1, 2, 23, 30, 0, 0, time.UTC)
	tokyo := time.FixedZone("JST", 9*3600)
	files := []capture.File{
		{Kind: capture.FileAudio, LocalPath: "tg-1-2-3.mp3"},
		{Kind: capture.FileImage, LocalPath: "5-1.jpg"},
		{Kind: capture.FileDocument, FileName: "invoice.pdf", LocalPath: "5-2.pdf"},
		{Kind: capture.FileDocument, FileName: "invoice.pdf", LocalPath: "5-3.pdf"},
		{Kind: capture.FileDocument, FileName: "invoice.pdf", LocalPath: "5-4.pdf"},
		{Kind: capture.FileDocument, FileName: "", LocalPath: "5-5.txt"},
		{Kind: capture.FileVideo, LocalPath: "5-6.mov"},
		{Kind: capture.FileVideo, LocalPath: ""},
	}
	want := []string{
		"voice-2026-01-03.mp3",
		"photo-2026-01-03-2.jpg",
		"invoice.pdf",
		"invoice (2).pdf",
		"invoice (3).pdf",
		"file-2026-01-03-6.txt",
		"video-2026-01-03-7.mov",
		"video-2026-01-03-8.mp4",
	}
	got := pipeline.AttachmentNames(recv, tokyo, files)
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("name %d = %q, want %q", i, got[i], want[i])
		}
	}
	again := pipeline.AttachmentNames(recv, tokyo, files)
	for i := range got {
		if again[i] != got[i] {
			t.Errorf("not deterministic at %d: %q vs %q", i, again[i], got[i])
		}
	}
}

func TestAttachmentNamesAudioDefaults(t *testing.T) {
	recv := time.Date(2026, 1, 2, 23, 30, 0, 0, time.UTC)
	got := pipeline.AttachmentNames(recv, nil, []capture.File{{Kind: capture.FileAudio, LocalPath: "5"}})
	if got[0] != "voice-2026-01-02.ogg" {
		t.Errorf("got %q", got[0])
	}
}

func TestAttachmentNamesDuplicateOfSuffixedName(t *testing.T) {
	files := []capture.File{
		{Kind: capture.FileDocument, FileName: "a (2).pdf"},
		{Kind: capture.FileDocument, FileName: "a.pdf"},
		{Kind: capture.FileDocument, FileName: "a.pdf"},
	}
	got := pipeline.AttachmentNames(time.Now(), time.UTC, files)
	seen := map[string]bool{}
	for _, n := range got {
		if seen[n] {
			t.Fatalf("duplicate %q in %q", n, got)
		}
		seen[n] = true
	}
}
