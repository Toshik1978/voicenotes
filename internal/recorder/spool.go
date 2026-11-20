// Package recorder holds the microphone recorder's runtime: the crash-safe
// spool, the recording loop and the hand-off to the service. It must stay
// CGO-free; audio capture lives behind an interface.
package recorder

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Toshik1978/voicenotes/internal/mic"
)

const (
	// syncInterval bounds how much audio a power cut can lose (research R6).
	syncInterval = 2 * time.Second

	extSidecar = ".json"
	extTmp     = ".json.tmp"
	extPart    = ".part.wav"
	extWAV     = ".wav"
	brokenDir  = "broken"

	dirPerm  = 0o700
	filePerm = 0o600

	bytesPerSecond = mic.SampleRate * mic.FrameSize
)

// Recording is a spool entry's sidecar (data-model.md "Spool layout"). EndedAt
// and EndReason are zero until the recording is finished.
type Recording struct {
	StartedAt time.Time     `json:"started_at"`
	EndedAt   time.Time     `json:"ended_at,omitzero"`
	Recorder  string        `json:"recorder"`
	EndReason mic.EndReason `json:"end_reason,omitempty"`
	ID        int64         `json:"id"`
}

// Recovery reports what [DirSpool.Recover] did.
type Recovery struct {
	// Recovered lists the IDs finished with reason interrupted, ascending.
	Recovered []int64
	// Broken lists the spool file names moved to broken/, sorted.
	Broken []string
}

// DirSpool is the recorder's crash-safe spool directory (research R6). Each
// recording is a <id>.json sidecar plus <id>.part.wav while recording, then
// <id>.wav once finished. It is safe for one recorder process; methods are not
// meant to be called concurrently for the same ID.
type DirSpool struct {
	now func() time.Time
	dir string
}

// NewDirSpool opens the spool at dir, creating it (0700) if missing, and
// checks that it is writable. now is the clock used to pace fsyncs; nil means
// time.Now.
func NewDirSpool(dir string, now func() time.Time) (*DirSpool, error) {
	if now == nil {
		now = time.Now
	}

	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("spool dir: create: %w", err)
	}

	probe, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return nil, fmt.Errorf("spool dir: not writable: %w", err)
	}

	name := probe.Name()
	cerr := probe.Close()

	if err := errors.Join(cerr, os.Remove(name)); err != nil {
		return nil, fmt.Errorf("spool dir: probe: %w", err)
	}

	return &DirSpool{dir: dir, now: now}, nil
}

// Begin starts a recording. The sidecar, a placeholder .part.wav and the
// directory are fsynced before the writer is returned, so a crash after the
// first sample always leaves a recoverable pair. The caller must Close the
// writer before Finish.
func (s *DirSpool) Begin(rec Recording) (io.WriteCloser, error) {
	if rec.ID <= 0 {
		return nil, fmt.Errorf("spool: begin: invalid recording ID %d", rec.ID)
	}

	rec.StartedAt = rec.StartedAt.UTC()
	rec.EndedAt, rec.EndReason = time.Time{}, ""

	b, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("spool: begin: encode sidecar: %w", err)
	}

	if err = writeNew(s.path(rec.ID, extSidecar), b); err != nil {
		return nil, fmt.Errorf("spool: begin: sidecar: %w", err)
	}

	f, err := os.OpenFile(s.path(rec.ID, extPart), os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, filePerm)
	if err != nil {
		return nil, fmt.Errorf("spool: begin: audio: %w", err)
	}

	if err := writeSync(f, mic.Header(0)); err != nil {
		return nil, errors.Join(fmt.Errorf("spool: begin: header: %w", err), f.Close())
	}

	if err := syncDir(s.dir); err != nil {
		return nil, errors.Join(fmt.Errorf("spool: begin: %w", err), f.Close())
	}

	return &partWriter{f: f, now: s.now, sync: f.Sync, lastSync: s.now()}, nil
}

// Finish patches the WAV sizes, fsyncs, renames <id>.part.wav to <id>.wav,
// fsyncs the directory and records endedAt and reason in the sidecar. It is
// idempotent: a repeat after the rename only rewrites the sidecar.
func (s *DirSpool) Finish(id int64, endedAt time.Time, reason mic.EndReason) error {
	if _, err := s.finalize(id); err != nil {
		return fmt.Errorf("spool: finish %d: %w", id, err)
	}

	if err := s.markEnded(id, endedAt, reason); err != nil {
		return fmt.Errorf("spool: finish %d: %w", id, err)
	}

	return nil
}

// Discard deletes every spool file of a recording (one below the minimum
// length). Missing files are not an error.
func (s *DirSpool) Discard(id int64) error {
	if err := s.removeFiles(id, extPart, extWAV, extSidecar); err != nil {
		return fmt.Errorf("spool: discard %d: %w", id, err)
	}

	return nil
}

// Finished returns the finished recordings, oldest StartedAt first (ties by
// ID). A recording still in progress is not listed.
func (s *DirSpool) Finished() ([]Recording, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("spool: list: %w", err)
	}

	var out []Recording

	for _, e := range entries {
		id, ext, ok := parseName(e.Name())
		if !ok || ext != extWAV || e.IsDir() {
			continue
		}

		rec, err := s.readSidecar(id)
		if err != nil {
			return nil, fmt.Errorf("spool: list: %w", err)
		}

		if rec.EndReason != "" {
			out = append(out, rec)
		}
	}

	slices.SortFunc(out, func(a, b Recording) int {
		if c := a.StartedAt.Compare(b.StartedAt); c != 0 {
			return c
		}

		return cmp.Compare(a.ID, b.ID)
	})

	return out, nil
}

// Open opens a finished recording's WAV for upload and returns its size. The
// reader seeks, so the caller can hash it and rewind.
func (s *DirSpool) Open(id int64) (io.ReadSeekCloser, int64, error) {
	f, err := os.Open(s.path(id, extWAV))
	if err != nil {
		return nil, 0, fmt.Errorf("spool: open %d: %w", id, err)
	}

	fi, err := f.Stat()
	if err != nil {
		return nil, 0, errors.Join(fmt.Errorf("spool: open %d: %w", id, err), f.Close())
	}

	return f, fi.Size(), nil
}

// Remove deletes a handed-over recording. Missing files are not an error.
func (s *DirSpool) Remove(id int64) error {
	if err := s.removeFiles(id, extWAV, extSidecar); err != nil {
		return fmt.Errorf("spool: remove %d: %w", id, err)
	}

	return nil
}

// Recover runs at startup. Each <id>.part.wav with a readable sidecar is
// repaired and finished with reason interrupted, its end time derived from
// the audio length; a finished WAV whose sidecar was not yet updated is
// completed the same way. Anything unmatched or unrecoverable moves to
// broken/ and is never silently deleted. Unrelated files are left alone.
func (s *DirSpool) Recover() (Recovery, error) {
	var res Recovery

	groups, errs, err := s.scan(&res)
	if err != nil {
		return Recovery{}, fmt.Errorf("spool: recover: %w", err)
	}

	for _, id := range slices.Sorted(maps.Keys(groups)) {
		recovered, err := s.recoverOne(&res, id, groups[id])
		errs = append(errs, err)

		if recovered {
			res.Recovered = append(res.Recovered, id)
		}
	}

	slices.Sort(res.Broken)

	if err := errors.Join(errs...); err != nil {
		return res, fmt.Errorf("spool: recover: %w", err)
	}

	return res, nil
}

// FreeBytes returns the bytes available to an unprivileged user on the
// spool's filesystem.
func (s *DirSpool) FreeBytes() (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.dir, &st); err != nil {
		return 0, fmt.Errorf("spool: statfs: %w", err)
	}

	free := st.Bavail * uint64(st.Bsize) //nolint:gosec // block size is positive

	return int64(min(free, math.MaxInt64)), nil
}

// scan groups the spool files by recording ID. Files with a spool suffix but
// no valid ID, and torn sidecar updates, go straight to broken/.
func (s *DirSpool) scan(res *Recovery) (map[int64]*group, []error, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, nil, fmt.Errorf("list: %w", err)
	}

	var errs []error

	groups := map[int64]*group{}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !hasSpoolExt(name) {
			continue
		}

		id, ext, ok := parseName(name)
		if !ok || ext == extTmp {
			errs = append(errs, s.toBroken(res, name))

			continue
		}

		if groups[id] == nil {
			groups[id] = &group{}
		}

		groups[id].set(ext)
	}

	return groups, errs, nil
}

// recoverOne settles one ID's files and reports whether it was finished with
// reason interrupted.
func (s *DirSpool) recoverOne(res *Recovery, id int64, g *group) (bool, error) {
	rec, ok, err := s.pair(res, id, g)
	if !ok || err != nil {
		return false, err
	}

	if g.wav && rec.EndReason != "" {
		return false, nil
	}

	data, err := s.finalize(id)
	if _, bad := errors.AsType[*mic.FormatError](err); bad {
		return false, s.brokenAll(res, id, g)
	}

	if err != nil {
		return false, err
	}

	ended := rec.StartedAt.Add(time.Duration(data) * time.Second / bytesPerSecond)
	if err := s.markEnded(id, ended, mic.EndInterrupted); err != nil {
		return false, err
	}

	return true, nil
}

// pair checks that id has a readable sidecar and exactly one audio half. An
// unmatched or unreadable entry moves to broken/ and pair reports !ok. When
// both halves exist the finished one wins and the .part.wav moves aside.
func (s *DirSpool) pair(res *Recovery, id int64, g *group) (Recording, bool, error) {
	if !g.sidecar || (!g.part && !g.wav) {
		return Recording{}, false, s.brokenAll(res, id, g)
	}

	rec, err := s.readSidecar(id)
	if err != nil {
		return Recording{}, false, s.brokenAll(res, id, g)
	}

	if g.part && g.wav {
		if err := s.toBroken(res, idName(id)+extPart); err != nil {
			return Recording{}, false, err
		}

		g.part = false
	}

	return rec, true, nil
}

// finalize makes <id>.wav final: it repairs and renames <id>.part.wav, or,
// when only <id>.wav exists, reads its data size. It returns the data bytes.
func (s *DirSpool) finalize(id int64) (int64, error) {
	part := s.path(id, extPart)

	f, err := os.OpenFile(part, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		fi, serr := os.Stat(s.path(id, extWAV))
		if serr != nil {
			return 0, fmt.Errorf("no audio: %w", serr)
		}

		return max(fi.Size()-mic.HeaderSize, 0), nil
	}

	if err != nil {
		return 0, fmt.Errorf("open audio: %w", err)
	}

	r, rerr := repair(f)
	if err := errors.Join(rerr, f.Close()); err != nil {
		return 0, err
	}

	if err := os.Rename(part, s.path(id, extWAV)); err != nil {
		return 0, fmt.Errorf("rename audio: %w", err)
	}

	if err := syncDir(s.dir); err != nil {
		return 0, err
	}

	return r.DataBytes, nil
}

// markEnded rewrites the sidecar with the end time and reason, atomically.
func (s *DirSpool) markEnded(id int64, endedAt time.Time, reason mic.EndReason) error {
	rec, err := s.readSidecar(id)
	if err != nil {
		return err
	}

	rec.EndedAt, rec.EndReason = endedAt.UTC(), reason

	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode sidecar: %w", err)
	}

	tmp := s.path(id, extTmp)
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) { // our own torn update
		return fmt.Errorf("clear sidecar update: %w", err)
	}

	if err := writeNew(tmp, b); err != nil {
		return fmt.Errorf("write sidecar: %w", err)
	}

	if err := os.Rename(tmp, s.path(id, extSidecar)); err != nil {
		return fmt.Errorf("replace sidecar: %w", err)
	}

	return syncDir(s.dir)
}

func (s *DirSpool) readSidecar(id int64) (Recording, error) {
	b, err := os.ReadFile(s.path(id, extSidecar))
	if err != nil {
		return Recording{}, fmt.Errorf("read sidecar: %w", err)
	}

	var rec Recording
	if err := json.Unmarshal(b, &rec); err != nil {
		return Recording{}, fmt.Errorf("decode sidecar %d: %w", id, err)
	}

	if rec.ID != id {
		return Recording{}, fmt.Errorf("sidecar %d holds ID %d", id, rec.ID)
	}

	return rec, nil
}

func (s *DirSpool) removeFiles(id int64, exts ...string) error {
	for _, ext := range exts {
		if err := os.Remove(s.path(id, ext)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("delete: %w", err)
		}
	}

	return syncDir(s.dir)
}

// brokenAll moves every present file of id to broken/.
func (s *DirSpool) brokenAll(res *Recovery, id int64, g *group) error {
	exts := g.exts()
	errs := make([]error, 0, len(exts))

	for _, ext := range exts {
		errs = append(errs, s.toBroken(res, idName(id)+ext))
	}

	return errors.Join(errs...)
}

// toBroken moves name into broken/, never overwriting: a clash gets a numeric
// suffix.
func (s *DirSpool) toBroken(res *Recovery, name string) error {
	bdir := filepath.Join(s.dir, brokenDir)
	if err := os.MkdirAll(bdir, dirPerm); err != nil {
		return fmt.Errorf("create broken dir: %w", err)
	}

	dst := filepath.Join(bdir, name)
	for i := 1; ; i++ {
		_, err := os.Lstat(dst)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}

		if err != nil { // only an existing name means "try the next suffix"
			return fmt.Errorf("check broken/%s: %w", filepath.Base(dst), err)
		}

		dst = filepath.Join(bdir, name+"."+strconv.Itoa(i))
	}

	if err := os.Rename(filepath.Join(s.dir, name), dst); err != nil {
		return fmt.Errorf("move %s to broken: %w", name, err)
	}

	res.Broken = append(res.Broken, name)

	return errors.Join(syncDir(bdir), syncDir(s.dir))
}

func (s *DirSpool) path(id int64, ext string) string {
	return filepath.Join(s.dir, idName(id)+ext)
}

// partWriter appends frames to a .part.wav, fsyncing at most every
// syncInterval and on Close.
type partWriter struct {
	lastSync time.Time
	f        *os.File
	now      func() time.Time
	sync     func() error
}

// Write appends p and fsyncs when syncInterval has passed since the last one.
func (w *partWriter) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	if err != nil {
		return n, fmt.Errorf("spool: write: %w", err)
	}

	if t := w.now(); t.Sub(w.lastSync) >= syncInterval {
		if err := w.sync(); err != nil {
			return n, fmt.Errorf("spool: sync: %w", err)
		}

		w.lastSync = t
	}

	return n, nil
}

// Close fsyncs and closes the file.
func (w *partWriter) Close() error {
	serr := w.sync()
	cerr := w.f.Close()

	if err := errors.Join(serr, cerr); err != nil {
		return fmt.Errorf("spool: close: %w", err)
	}

	return nil
}

// group records which spool files exist for one ID.
type group struct {
	sidecar, part, wav bool
}

func (g *group) set(ext string) {
	switch ext {
	case extSidecar:
		g.sidecar = true
	case extPart:
		g.part = true
	case extWAV:
		g.wav = true
	}
}

func (g *group) exts() []string {
	var out []string

	for _, e := range []struct {
		ok  bool
		ext string
	}{{g.sidecar, extSidecar}, {g.part, extPart}, {g.wav, extWAV}} {
		if e.ok {
			out = append(out, e.ext)
		}
	}

	return out
}

// repair applies mic.Repair to an open .part.wav and fsyncs it.
func repair(f *os.File) (mic.Repaired, error) {
	fi, err := f.Stat()
	if err != nil {
		return mic.Repaired{}, fmt.Errorf("stat audio: %w", err)
	}

	r, err := mic.Repair(fi.Size())
	if err != nil {
		return mic.Repaired{}, fmt.Errorf("repair audio: %w", err)
	}

	if err := f.Truncate(r.Length); err != nil {
		return mic.Repaired{}, fmt.Errorf("truncate audio: %w", err)
	}

	if _, err := f.WriteAt(r.Header, 0); err != nil {
		return mic.Repaired{}, fmt.Errorf("patch header: %w", err)
	}

	if err := f.Sync(); err != nil {
		return mic.Repaired{}, fmt.Errorf("sync audio: %w", err)
	}

	return r, nil
}

func idName(id int64) string { return strconv.FormatInt(id, 10) }

// spoolExts lists the spool suffixes, longest first so parseName matches
// ".part.wav" before ".wav" and ".json.tmp" before ".json".
func spoolExts() []string { return []string{extPart, extTmp, extSidecar, extWAV} }

func hasSpoolExt(name string) bool {
	return slices.ContainsFunc(spoolExts(), func(ext string) bool { return strings.HasSuffix(name, ext) })
}

// parseName splits a spool file name into its recording ID and suffix.
func parseName(name string) (int64, string, bool) {
	for _, ext := range spoolExts() {
		base, ok := strings.CutSuffix(name, ext)
		if !ok {
			continue
		}

		id, err := strconv.ParseInt(base, 10, 64)
		if err != nil || id <= 0 || idName(id) != base {
			return 0, ext, false
		}

		return id, ext, true
	}

	return 0, "", false
}

// writeNew creates path exclusively, writes b and fsyncs it.
func writeNew(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}

	return errors.Join(writeSync(f, b), f.Close())
}

func writeSync(f *os.File, b []byte) error {
	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync: %w", err)
	}

	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir: %w", err)
	}

	serr := d.Sync()

	if err := errors.Join(serr, d.Close()); err != nil {
		return fmt.Errorf("sync dir: %w", err)
	}

	return nil
}
