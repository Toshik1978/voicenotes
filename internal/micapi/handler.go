package micapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/voicenotes/internal/mic"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

const (
	// maxJSONBytes bounds the JSON body of a start notice or check-in.
	maxJSONBytes = 4 << 10
	// ingestTimeout bounds IngestRecording once the file is durable; it is
	// detached from the request so a client gone at the last moment does
	// not abort an insert.
	ingestTimeout = 30 * time.Second
	// startedTimeout bounds the asynchronous start notice.
	startedTimeout = 30 * time.Second
	// discardTimeout bounds the discard notice.
	discardTimeout = 5 * time.Second
	// copyBufSize is the buffer for streaming an upload to disk.
	copyBufSize = 64 << 10
)

// RecordingIngester queues a durable recording file. *pipeline.Ingester is
// the production adapter.
type RecordingIngester interface {
	IngestRecording(
		ctx context.Context,
		meta pipeline.RecordingMeta,
		localPath string,
	) (pipeline.RecordingResult, error)
}

// StartNotices sends and withdraws the start notices. *Starts is the
// production adapter.
type StartNotices interface {
	Started(ctx context.Context, id int64, startedAt time.Time) error
	Discarded(ctx context.Context, id int64) error
}

// RecorderStore records recorder check-ins. *sqlite.DB is the production
// adapter.
type RecorderStore interface {
	UpsertRecorder(ctx context.Context, name string, lastSeen time.Time, receiverPresent, storageFull bool) error
}

// Config is the handler's settings.
type Config struct {
	// Token is MIC_UPLOAD_TOKEN; an empty token rejects every request.
	Token string
	// MaxBytes is MIC_UPLOAD_MAX_BYTES.
	MaxBytes int64
	// FilesDir is DATA_DIR/files, where recordings are stored.
	FilesDir string
}

// Option customises a Handler.
type Option func(*Handler)

// WithClock overrides the clock used for check-in times.
func WithClock(now func() time.Time) Option { return func(h *Handler) { h.now = now } }

// WithLogger sets the logger; the default discards.
func WithLogger(log *zap.Logger) Option { return func(h *Handler) { h.log = log } }

// Handler serves the microphone API under /mic/v1/
// (contracts/mic-http-api.md).
type Handler struct {
	cfg       Config
	ingest    RecordingIngester
	notices   StartNotices
	recorders RecorderStore
	now       func() time.Time
	log       *zap.Logger
	mux       *http.ServeMux
	wg        sync.WaitGroup
}

// NewHandler builds the handler.
func NewHandler(
	cfg Config, ingest RecordingIngester, notices StartNotices, recorders RecorderStore, opts ...Option,
) *Handler {
	h := &Handler{
		cfg: cfg, ingest: ingest, notices: notices, recorders: recorders,
		now: time.Now, log: zap.NewNop(), mux: http.NewServeMux(),
	}
	for _, o := range opts {
		o(h)
	}
	h.mux.HandleFunc("PUT /mic/v1/recordings/{id}", h.upload)
	h.mux.HandleFunc("POST /mic/v1/recordings/{id}/started", h.started)
	h.mux.HandleFunc("DELETE /mic/v1/recordings/{id}/started", h.discarded)
	h.mux.HandleFunc("PUT /mic/v1/recorders/{name}", h.checkIn)

	return h
}

// Wait blocks until every asynchronous start notice has finished.
func (h *Handler) Wait() { h.wg.Wait() }

// ServeHTTP authenticates the request, then routes it.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !authorized(r.Header.Get("Authorization"), h.cfg.Token) {
		h.log.Info("mic request unauthorized",
			zap.String("path", r.URL.Path), zap.Int("status", http.StatusUnauthorized))
		writeJSON(w, http.StatusUnauthorized, statusUnauthorized, "missing or wrong bearer token")

		return
	}
	h.mux.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, code int, status, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(reply{Status: status, Reason: reason}); err != nil {
		return // the client is gone; nothing more to tell it
	}
}

// reply is every JSON response body.
type reply struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

func reject(w http.ResponseWriter, rej *rejection) { writeJSON(w, rej.code, rej.status, rej.reason) }

func unavailable(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, statusUnavailable, "store or disk failing, retry later")
}

// upload stores a finished recording: stream to a temp file while hashing,
// verify length, hash and WAV header, make it durable, then ingest it.
// The server's own read timeout is sized for small requests. An upload may
// take longer over a weak link, so its read deadline is sized from its length
// at no less than minUploadRate, and the write deadline leaves uploadGrace to
// answer after it.
const (
	minUploadRate   = 64 << 10 // bytes per second
	minUploadWindow = 2 * time.Minute
	uploadGrace     = 30 * time.Second
)

// uploadWindow is how long reading an upload of length bytes may take.
func uploadWindow(length int64) time.Duration {
	return max(minUploadWindow, time.Duration(length/minUploadRate)*time.Second+uploadGrace)
}

// extendDeadlines replaces the server's read and write deadlines for this
// request. A writer without deadline support keeps the server's.
func extendDeadlines(w http.ResponseWriter, length int64, log *zap.Logger) {
	rc := http.NewResponseController(w)
	read := time.Now().Add(uploadWindow(length))
	for _, err := range []error{rc.SetReadDeadline(read), rc.SetWriteDeadline(read.Add(uploadGrace))} {
		if err != nil && !errors.Is(err, http.ErrNotSupported) {
			log.Warn("extending the upload deadline failed", zap.Error(err))
		}
	}
}

func (h *Handler) upload(w http.ResponseWriter, r *http.Request) {
	id, rej := parseRecordingID(r.PathValue("id"))
	if rej != nil {
		reject(w, rej)

		return
	}
	up, rej := parseUpload(r.Header, r.ContentLength, h.cfg.MaxBytes)
	if rej != nil {
		reject(w, rej)

		return
	}
	log := h.log.With(zap.Int64("recording", id), zap.String("recorder", up.recorder))
	extendDeadlines(w, up.length, log)

	name, err := h.receive(w, r, id, up)
	if errors.As(err, &rej) {
		log.Warn("recording upload rejected", zap.Int("status", rej.code), zap.String("reason", rej.reason))
		reject(w, rej)

		return
	}
	if err != nil {
		log.Error("storing a recording upload failed", zap.Error(err))
		unavailable(w)

		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), ingestTimeout)
	defer cancel()
	meta := pipeline.RecordingMeta{ID: id, StartedAt: up.startedAt, EndReason: up.endReason, Recorder: up.recorder}
	res, err := h.ingest.IngestRecording(ctx, meta, name)
	if err != nil {
		log.Error("ingesting a recording failed", zap.Error(err))
		h.remove(log, name)
		unavailable(w)

		return
	}
	if res == pipeline.Duplicate {
		log.Info("recording already stored")
		h.remove(log, name)
		writeJSON(w, http.StatusOK, statusDuplicate, "")

		return
	}
	log.Info("recording stored", zap.Int64("bytes", up.length))
	writeJSON(w, http.StatusCreated, statusStored, "")
}

// receive streams the body into the files directory and returns the durable
// file's name. A client fault is a *rejection, anything else a disk fault;
// either way nothing is left behind.
func (h *Handler) receive(w http.ResponseWriter, r *http.Request, id int64, up upload) (string, error) {
	if err := os.MkdirAll(h.cfg.FilesDir, 0o750); err != nil {
		return "", fmt.Errorf("micapi: create files dir: %w", err)
	}
	f, err := os.CreateTemp(h.cfg.FilesDir, ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("micapi: create temp file: %w", err)
	}
	tmp := f.Name()
	if err = fill(f, http.MaxBytesReader(w, r.Body, h.cfg.MaxBytes), up); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)

		return "", err
	}
	name := recordingName(id)
	final := filepath.Join(h.cfg.FilesDir, name)
	if err = os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)

		return "", fmt.Errorf("micapi: rename recording: %w", err)
	}
	if err = syncDir(h.cfg.FilesDir); err != nil {
		_ = os.Remove(final)

		return "", err
	}

	return name, nil
}

// fill writes the verified body into f, checks its WAV header, then syncs
// and closes f. The caller closes f on error.
func fill(f *os.File, body io.Reader, up upload) error {
	if err := verifyBody(f, body, up); err != nil {
		return err
	}
	if err := checkWAV(f); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("micapi: sync recording: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("micapi: close recording: %w", err)
	}

	return nil
}

// verifyBody copies body into f, hashing it, and checks the length and hash
// against the headers.
func verifyBody(f *os.File, body io.Reader, up upload) error {
	hash := sha256.New()
	n, rerr, werr := copyBody(io.MultiWriter(f, hash), body)
	if werr != nil {
		return fmt.Errorf("micapi: write recording: %w", werr)
	}
	if mbe := (*http.MaxBytesError)(nil); errors.As(rerr, &mbe) {
		return &rejection{http.StatusRequestEntityTooLarge, statusTooLarge, "recording exceeds the upload limit"}
	}
	if rerr != nil {
		return &rejection{http.StatusBadRequest, statusCorrupt, "body ended early"}
	}
	if n != up.length {
		return &rejection{http.StatusBadRequest, statusCorrupt, "body length does not match Content-Length"}
	}
	if hex.EncodeToString(hash.Sum(nil)) != up.sha256 {
		return &rejection{http.StatusBadRequest, statusCorrupt, "body does not match " + headerSHA256}
	}

	return nil
}

// checkWAV parses the WAV header at the start of f.
func checkWAV(f *os.File) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("micapi: rewind recording: %w", err)
	}
	_, err := mic.Parse(f)
	if errors.Is(err, mic.ErrUnsupportedFormat) {
		return &rejection{
			http.StatusUnsupportedMediaType, statusUnsupported, "audio must be PCM 16-bit mono 16 kHz WAV",
		}
	}
	if err != nil {
		return fmt.Errorf("micapi: read recording header: %w", err)
	}

	return nil
}

// copyBody copies src to dst, keeping read and write errors apart: a read
// error is the client's, a write error the disk's. Both are returned as is
// for the caller to classify.
func copyBody(dst io.Writer, src io.Reader) (n int64, readErr, writeErr error) {
	buf := make([]byte, copyBufSize)
	for {
		m, err := src.Read(buf)
		if m > 0 {
			if _, werr := dst.Write(buf[:m]); werr != nil {
				return n, nil, werr //nolint:wrapcheck // classified and wrapped by the caller
			}
			n += int64(m)
		}
		if errors.Is(err, io.EOF) {
			return n, nil, nil
		}
		if err != nil {
			return n, err, nil //nolint:wrapcheck // classified by the caller
		}
	}
}

// recordingName is "mic-<id>-<random>.wav". The random part keeps a
// re-upload of a stored recording from replacing the stored file.
func recordingName(id int64) string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on supported platforms

	return fmt.Sprintf("mic-%d-%s.wav", id, hex.EncodeToString(b[:]))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("micapi: sync files dir: %w", err)
	}
	serr := d.Sync()
	if err = errors.Join(serr, d.Close()); err != nil {
		return fmt.Errorf("micapi: sync files dir: %w", err)
	}

	return nil
}

func (h *Handler) remove(log *zap.Logger, name string) {
	if err := os.Remove(filepath.Join(h.cfg.FilesDir, name)); err != nil { //nolint:gosec // name is recordingName's
		log.Warn("removing an unneeded recording file failed", zap.Error(err))
	}
}

// readJSONBody reads a small JSON body.
func readJSONBody(w http.ResponseWriter, r *http.Request) ([]byte, *rejection) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBytes))
	if err != nil {
		return nil, badRequest("body is unreadable or too large")
	}

	return body, nil
}

// started answers 202 at once and sends the start notice in the background.
func (h *Handler) started(w http.ResponseWriter, r *http.Request) {
	id, rej := parseRecordingID(r.PathValue("id"))
	if rej != nil {
		reject(w, rej)

		return
	}
	body, rej := readJSONBody(w, r)
	if rej != nil {
		reject(w, rej)

		return
	}
	at, _, rej := parseStarted(body)
	if rej != nil {
		reject(w, rej)

		return
	}

	detached := context.WithoutCancel(r.Context())
	h.wg.Go(func() {
		ctx, cancel := context.WithTimeout(detached, startedTimeout)
		defer cancel()
		if err := h.notices.Started(ctx, id, at); err != nil {
			h.log.Warn("start notice failed", zap.Int64("recording", id), zap.Error(err))
		}
	})
	writeJSON(w, http.StatusAccepted, statusAccepted, "")
}

// discarded withdraws the start notice and answers 204 whatever happens.
func (h *Handler) discarded(w http.ResponseWriter, r *http.Request) {
	id, rej := parseRecordingID(r.PathValue("id"))
	if rej != nil {
		reject(w, rej)

		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), discardTimeout)
	defer cancel()
	if err := h.notices.Discarded(ctx, id); err != nil {
		h.log.Warn("discard notice failed", zap.Int64("recording", id), zap.Error(err))
	}
	w.WriteHeader(http.StatusNoContent)
}

// checkIn records a recorder's check-in at the server's time.
func (h *Handler) checkIn(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validRecorderName(name) {
		reject(w, badRequest("recorder name must match [A-Za-z0-9._-]{1,64}"))

		return
	}
	body, rej := readJSONBody(w, r)
	if rej != nil {
		reject(w, rej)

		return
	}
	c, rej := parseCheckIn(body)
	if rej != nil {
		reject(w, rej)

		return
	}
	if err := h.recorders.UpsertRecorder(r.Context(), name, h.now(), c.receiverPresent, c.storageFull); err != nil {
		h.log.Error("recording a check-in failed", zap.String("recorder", name), zap.Error(err))
		unavailable(w)

		return
	}
	w.WriteHeader(http.StatusNoContent)
}
