// Package sqlite is the durable capture queue backed by a local SQLite file.
// It is a thin I/O adapter: all scheduling decisions live in
// internal/capture and internal/pipeline.
package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/Toshik1978/voicenotes/internal/capture"
)

// DBFile is the database file name inside the data directory.
const DBFile = "voicenotes.db"

// Sentinel errors.
var (
	// ErrDuplicate means a capture with the same (source, chat, message)
	// identity already exists. It reports Duplicate() true, which is how
	// pipeline.Ingest recognises it without importing this package.
	ErrDuplicate error = duplicateError{}
	// ErrNotFound means the capture to update does not exist.
	ErrNotFound = errors.New("sqlite: capture not found")
)

// duplicateError is the type of ErrDuplicate.
type duplicateError struct{}

func (duplicateError) Error() string { return "sqlite: duplicate capture" }

// Duplicate marks the error as a duplicate insert.
func (duplicateError) Duplicate() bool { return true }

// Store is the capture queue used by ingest, the worker and health.
// Optional columns map to the zero value of the field: NULL reads back as
// "" or 0, and zero values are written as NULL. Timestamps are stored as
// Unix nanoseconds and read back in UTC.
type Store interface {
	// Insert stores c and its files in one transaction, sets c.ID and each
	// file's ID and CaptureID, and returns ErrDuplicate if the capture's or
	// any file's identity already exists (nothing is stored then).
	Insert(ctx context.Context, c *capture.Capture) error
	// AppendFile adds one incoming file item to the chat's open batch, or
	// starts a new one, in a single BEGIN IMMEDIATE transaction (research
	// R3). item is a new files capture at collect holding that one file.
	// capture.JoinBatch decides join or new at the wall-clock now with quiet
	// and sets the batch deadline. The result holds the stored batch,
	// whether it was created, and whether its deadline moved later, so the
	// caller wakes the worker only then. It returns
	// ErrDuplicate if the item's identity was already stored.
	AppendFile(
		ctx context.Context,
		item capture.Capture,
		quiet time.Duration,
	) (capture.AppendResult, error)
	// JoinOpenBatch is AppendFile that never starts a batch: it adds item
	// only when capture.JoinBatch joins it to the chat's open batch, and
	// otherwise stores nothing and reports joined false. A file over the
	// Telegram download limit uses it, since alone it is rejected.
	JoinOpenBatch(
		ctx context.Context,
		item capture.Capture,
		quiet time.Duration,
	) (res capture.AppendResult, joined bool, err error)
	// NextDue is due-first within a lane (FR-114a): it returns, with OK
	// true, the lowest-ID unfinished capture of the lane whose NextAttemptAt
	// is not after now; a capture that is not due never blocks later ones.
	// When none is due, OK is false and WakeAt is the lane's earliest
	// NextAttemptAt (UTC) so the worker can sleep until then; with an empty
	// lane WakeAt is the zero time. Lane membership matches capture.LaneOf.
	NextDue(ctx context.Context, now time.Time, lane capture.Lane) (capture.Due, error)
	// Update persists every mutable field of c and of its files in one
	// transaction, returning ErrNotFound if c.ID does not exist. Files are
	// inserted only at ingest: each must already have a row, and its
	// identity, kind and position are never changed. A zero ReplyMessageID keeps the stored one: a
	// worker copy read before ingest recorded the reply must not erase it.
	Update(ctx context.Context, c capture.Capture) error
	// AdvanceCollect is Update for the collect step's result next, as a
	// compare-and-set against read, the capture the worker ran collect on:
	// it writes only while the row is still at collect with read's
	// next_attempt_at and file count. It returns false, writing nothing,
	// when a batch join landed since read (or the row is gone), so the
	// worker re-reads instead of dropping the joined file's caption and
	// deadline.
	AdvanceCollect(ctx context.Context, read, next capture.Capture) (bool, error)
	// SetReplyMessageID records the reply to edit, touching no other
	// column, so it cannot race the worker's step updates. A reply already
	// recorded is kept. It returns ErrNotFound if id does not exist.
	SetReplyMessageID(ctx context.Context, id, msgID int64) error
	// OldestUnfinished returns the unfinished capture with the lowest ID,
	// due or not; ok is false when the queue is empty.
	OldestUnfinished(ctx context.Context) (c capture.Capture, ok bool, err error)
	// PurgeDone deletes done captures last updated before olderThan, with
	// their files, and returns how many captures were removed.
	PurgeDone(ctx context.Context, olderThan time.Time) (int64, error)
	// Close releases the database.
	Close() error
}

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DB is the SQLite-backed implementation of Store.
type DB struct {
	db *sql.DB
	// now is the wall clock batching decides by.
	now func() time.Time
}

var _ Store = (*DB)(nil)

// Open opens (creating if needed) $dataDir/voicenotes.db, applies pending
// migrations and returns the store. The dataDir must exist.
func Open(dataDir string) (*DB, error) {
	// Pragmas are set per connection through the DSN. A single connection
	// matches the single-writer service and avoids SQLITE_BUSY.
	db, err := sql.Open("sqlite", dsn(dataDir))
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}
	db.SetMaxOpenConns(1)

	if err := migrateTo(context.Background(), db, math.MaxInt); err != nil {
		_ = db.Close()

		return nil, err
	}

	return &DB{db: db, now: time.Now}, nil
}

// dsn is the connection string for $dataDir/voicenotes.db. Pragmas are set
// per connection; foreign_keys makes PurgeDone cascade to capture_files.
func dsn(dataDir string) string {
	q := "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(FULL)"

	return "file:" + filepath.Join(dataDir, DBFile) + "?" + q
}

// migrateTo applies migrations/NNN_*.sql up to version target in order, each
// in one transaction, tracking progress in PRAGMA user_version. It is
// forward-only.
func migrateTo(ctx context.Context, db *sql.DB, target int) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("sqlite: list migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)

	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("sqlite: read user_version: %w", err)
	}

	for _, name := range names {
		num, _, _ := strings.Cut(name, "_")
		version, err := strconv.Atoi(num)
		if err != nil {
			return fmt.Errorf("sqlite: bad migration name %q: %w", name, err)
		}
		if version <= current || version > target {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("sqlite: read migration %s: %w", name, err)
		}
		if err := applyMigration(ctx, db, version, string(body)); err != nil {
			return fmt.Errorf("sqlite: apply migration %s: %w", name, err)
		}
	}

	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, version int, body string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err //nolint:wrapcheck // wrapped by the caller
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, body); err != nil {
		return err //nolint:wrapcheck // wrapped by the caller
	}
	// PRAGMA does not accept bound parameters; version is an int we parsed.
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(version)); err != nil {
		return err //nolint:wrapcheck // wrapped by the caller
	}

	return tx.Commit() //nolint:wrapcheck // wrapped by the caller
}

const columns = `id, source, source_chat_id, source_message_id, kind, received_at,
album_id, text, language, enriched_json,
llm_skipped, failure, note_name, note_url, task_count, reply_message_id,
step, status, waiting_on, attempts, next_attempt_at, updated_at,
misconfig, last_error, queue_reply_edit, minimal_note, send_new_reply,
enrich_started_at`

const fileColumns = `id, capture_id, position, source, source_chat_id, source_message_id,
kind, file_id, mime_type, file_name, file_size, local_path, state, text,
text_source, language, failure, timeouts, attached, attach_failure,
vision_attempts, vision_started_at`

// Lane predicates over captures; they must agree with capture.LaneOf.
const (
	documentsLane = "(kind = 'files' AND step = 'extract')"
	mainLane      = "NOT " + documentsLane
)

// updateCapture writes every mutable column of a capture.
const updateCapture = `UPDATE captures SET
kind=?, received_at=?, album_id=?, text=?,
language=?, enriched_json=?, llm_skipped=?, failure=?, note_name=?, note_url=?,
task_count=?, reply_message_id=COALESCE(?, reply_message_id), step=?, status=?, waiting_on=?, attempts=?,
next_attempt_at=?, updated_at=?, misconfig=?, last_error=?, queue_reply_edit=?,
minimal_note=?, send_new_reply=?, enrich_started_at=?
WHERE id=?`

// advanceCollect is updateCapture guarded for AdvanceCollect: the row is
// still at collect with the deadline and file count the worker read.
const advanceCollect = updateCapture + " AND step = 'collect' AND next_attempt_at = ?" +
	" AND (SELECT count(*) FROM capture_files WHERE capture_id = captures.id) = ?"

// errBatchChanged means AdvanceCollect's guard matched no row.
var errBatchChanged = errors.New("batch changed since read")

// errFileNotInserted means Update was given a file without a row: files are
// inserted at ingest, never by Update.
var errFileNotInserted = errors.New("file was never inserted")

// Insert stores c and its files in one transaction, setting c.ID and each
// file's ID and CaptureID.
func (s *DB) Insert(ctx context.Context, c *capture.Capture) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: insert capture: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := insertCapture(ctx, tx, c); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: insert capture: %w", err)
	}

	return nil
}

// AppendFile stores item inside BEGIN IMMEDIATE, so the write lock is taken
// before the open batch is read and concurrent appends serialise (research
// R3). capture.JoinBatch, applied at the wall-clock now once the lock is
// held (not item.ReceivedAt, the sender's clock), decides: a join adds
// the file at the next position, updates the batch text and moves
// next_attempt_at only when the deadline moved later; otherwise item becomes
// a new capture due at the decision's deadline. JoinBatch is the single
// source of truth for that deadline; item.NextAttemptAt is ignored.
func (s *DB) AppendFile(
	ctx context.Context, item capture.Capture, quiet time.Duration,
) (capture.AppendResult, error) {
	res, _, err := s.appendTx(ctx, item, quiet, false)

	return res, err
}

// JoinOpenBatch is AppendFile restricted to a join; see Store.
func (s *DB) JoinOpenBatch(
	ctx context.Context, item capture.Capture, quiet time.Duration,
) (capture.AppendResult, bool, error) {
	return s.appendTx(ctx, item, quiet, true)
}

// PostponeOpenBatches pushes the deadline of every files capture still at
// collect to the wall-clock now plus quiet, where it is earlier, and returns
// how many it moved. Run before the worker starts, it lets parts of an album
// that Telegram redelivers after a restart join their half batch
// (quickstart §3).
func (s *DB) PostponeOpenBatches(ctx context.Context, quiet time.Duration) (int64, error) {
	deadline := ts(s.now().Add(quiet))
	res, err := s.db.ExecContext(ctx, "UPDATE captures SET next_attempt_at = ?"+
		" WHERE kind = 'files' AND step = 'collect' AND status != 'done' AND next_attempt_at < ?",
		deadline, deadline)
	if err != nil {
		return 0, fmt.Errorf("sqlite: postpone open batches: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite: postpone open batches: %w", err)
	}

	return n, nil
}

// appendFile is AppendFile's body inside the open transaction. It reports
// whether item was stored: always, unless joinOnly and it would start a
// batch.
func appendFile(
	ctx context.Context, conn *sql.Conn, item capture.Capture, now time.Time, quiet time.Duration,
	joinOnly bool,
) (capture.AppendResult, bool, error) {
	openBatch, err := queryOpenBatch(ctx, conn, item)
	if err != nil {
		return capture.AppendResult{}, false, err
	}

	d := capture.JoinBatch(openBatch, capture.BatchItem{AlbumID: item.AlbumID, Caption: item.Text},
		now, quiet)
	if !d.Join && joinOnly {
		return capture.AppendResult{}, false, nil
	}
	if !d.Join {
		item.NextAttemptAt, item.Text = d.NextAttemptAt, d.Text
		item.Files = slices.Clone(item.Files)
		item.Files[0].Position = 0
		if insErr := insertCapture(ctx, conn, &item); insErr != nil {
			return capture.AppendResult{}, false, insErr
		}

		return capture.AppendResult{Capture: item, Created: true, DeadlineMoved: true}, true, nil
	}

	open := *openBatch
	f := item.Files[0]
	f.CaptureID, f.Position = open.ID, d.Position
	if insErr := insertFile(ctx, conn, &f); insErr != nil {
		return capture.AppendResult{}, false, insErr
	}
	open.Text = d.Text
	if d.DeadlineMoved {
		open.NextAttemptAt = d.NextAttemptAt
	}
	_, err = conn.ExecContext(ctx, "UPDATE captures SET text=?, next_attempt_at=? WHERE id=?",
		nullStr(open.Text), ts(open.NextAttemptAt), open.ID)
	if err != nil {
		return capture.AppendResult{}, false, fmt.Errorf("sqlite: join batch %d: %w", open.ID, err)
	}
	open.Files = append(open.Files, f)

	return capture.AppendResult{Capture: open, DeadlineMoved: d.DeadlineMoved}, true, nil
}

// queryOpenBatch reads the chat's newest files capture at collect with its
// files, or nil when there is none.
func queryOpenBatch(ctx context.Context, conn *sql.Conn, item capture.Capture) (*capture.Capture, error) {
	open, ok, err := scanCapture(conn.QueryRowContext(ctx, "SELECT "+columns+
		" FROM captures WHERE kind = 'files' AND step = 'collect' AND status != 'done'"+
		" AND source = ? AND source_chat_id = ? ORDER BY id DESC LIMIT 1",
		item.Source, item.SourceChatID))
	if err != nil || !ok {
		return nil, err
	}
	if open.Files, err = queryFiles(ctx, conn, open.ID); err != nil {
		return nil, err
	}

	return &open, nil
}

// rollback ends conn's open transaction. A failed ROLLBACK may leave the
// transaction open, so the connection is then discarded rather than
// returned to the pool.
func rollback(ctx context.Context, conn *sql.Conn) error {
	// Not the request context: a cancelled one must still roll back.
	_, err := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
	if err == nil {
		return nil
	}
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })

	return fmt.Errorf("sqlite: append file: rollback: %w", err)
}

// execer is the part of *sql.Tx and *sql.Conn that inserts need.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// insertCapture writes c and its files with q, setting c.ID and each file's
// ID and CaptureID; c is left unchanged on error.
func insertCapture(ctx context.Context, q execer, c *capture.Capture) error {
	res, err := q.ExecContext(ctx, `INSERT INTO captures (
source, source_chat_id, source_message_id, kind, received_at,
album_id, text, language, enriched_json,
llm_skipped, failure, note_name, note_url, task_count, reply_message_id,
step, status, waiting_on, attempts, next_attempt_at, updated_at,
misconfig, last_error, queue_reply_edit, minimal_note, send_new_reply,
enrich_started_at)
VALUES (?,?,?,?,?, ?,?,?,?, ?,?,?,?,?,?, ?,?,?,?,?,?, ?,?,?,?,?, ?)`,
		c.Source, c.SourceChatID, c.SourceMessageID, string(c.Kind), ts(c.ReceivedAt),
		nullStr(c.AlbumID), nullStr(c.Text), nullStr(c.Language), nullStr(c.EnrichedJSON),
		nullStr(c.LLMSkipped), nullStr(c.Failure), nullStr(c.NoteName), nullStr(c.NoteURL),
		c.TaskCount, nullInt(c.ReplyMessageID),
		string(c.Step), string(c.Status), nullStr(string(c.WaitingOn)), c.Attempts,
		ts(c.NextAttemptAt), ts(c.UpdatedAt),
		c.Misconfig, nullStr(c.LastError), c.QueueReplyEdit, c.MinimalNote, c.SendNewReply,
		nullTS(c.EnrichStartedAt))
	if err != nil {
		if isUnique(err) {
			return ErrDuplicate
		}

		return fmt.Errorf("sqlite: insert capture: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("sqlite: insert capture id: %w", err)
	}

	// A clone, so copies of c sharing the slice never see half-assigned IDs.
	files := slices.Clone(c.Files)
	for i := range files {
		files[i].CaptureID = id
		if err := insertFile(ctx, q, &files[i]); err != nil {
			return err
		}
	}
	c.ID, c.Files = id, files

	return nil
}

func insertFile(ctx context.Context, tx execer, f *capture.File) error {
	res, err := tx.ExecContext(ctx, `INSERT INTO capture_files (
capture_id, position, source, source_chat_id, source_message_id,
kind, file_id, mime_type, file_name, file_size, local_path, state, text,
text_source, language, failure, timeouts, attached, attach_failure,
vision_attempts, vision_started_at)
VALUES (?,?,?,?,?, ?,?,?,?,?,?,?,?, ?,?,?,?,?,?, ?,?)`,
		f.CaptureID, f.Position, f.Source, f.SourceChatID, f.SourceMessageID,
		string(f.Kind), nullStr(f.FileID), nullStr(f.MimeType), nullStr(f.FileName),
		nullInt(f.FileSize), nullStr(f.LocalPath), string(f.State), nullStr(f.Text),
		nullStr(string(f.TextSource)), nullStr(f.Language), nullStr(f.Failure),
		f.Timeouts, f.Attached, nullStr(f.AttachFailure),
		f.VisionAttempts, nullTS(f.VisionStartedAt))
	if err != nil {
		if isUnique(err) {
			return ErrDuplicate
		}

		return fmt.Errorf("sqlite: insert file of capture %d: %w", f.CaptureID, err)
	}
	if f.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("sqlite: insert file id of capture %d: %w", f.CaptureID, err)
	}

	return nil
}

// Update persists c and the mutable columns of its files in one
// transaction. A file's identity, kind and position are fixed at ingest; a
// file without a row is an error, not an insert.
func (s *DB) Update(ctx context.Context, c capture.Capture) error {
	return s.update(ctx, c, nil)
}

// AdvanceCollect persists next only while the batch is as read left it; see
// Store. A missing row also reads as changed.
func (s *DB) AdvanceCollect(ctx context.Context, read, next capture.Capture) (bool, error) {
	if read.ID != next.ID {
		return false, fmt.Errorf("sqlite: advance collect: read %d, next %d", read.ID, next.ID)
	}
	err := s.update(ctx, next, &read)
	if errors.Is(err, errBatchChanged) {
		return false, nil
	}

	return err == nil, err
}

func updateFile(ctx context.Context, tx *sql.Tx, captureID int64, f *capture.File) error {
	if f.ID == 0 {
		return fmt.Errorf("sqlite: update capture %d: file %d: %w", captureID, f.Position, errFileNotInserted)
	}
	res, err := tx.ExecContext(ctx, `UPDATE capture_files SET
file_id=?, mime_type=?, file_name=?, file_size=?, local_path=?, state=?, text=?,
text_source=?, language=?, failure=?, timeouts=?, attached=?, attach_failure=?,
vision_attempts=?, vision_started_at=?
WHERE id=? AND capture_id=?`,
		nullStr(f.FileID), nullStr(f.MimeType), nullStr(f.FileName), nullInt(f.FileSize),
		nullStr(f.LocalPath), string(f.State), nullStr(f.Text),
		nullStr(string(f.TextSource)), nullStr(f.Language), nullStr(f.Failure),
		f.Timeouts, f.Attached, nullStr(f.AttachFailure),
		f.VisionAttempts, nullTS(f.VisionStartedAt),
		f.ID, captureID)
	if err != nil {
		return fmt.Errorf("sqlite: update file %d of capture %d: %w", f.ID, captureID, err)
	}
	if err := oneRow(res); err != nil {
		return fmt.Errorf("sqlite: update file %d of capture %d: %w", f.ID, captureID, err)
	}

	return nil
}

// oneRow returns ErrNotFound when res affected no row.
func oneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err //nolint:wrapcheck // wrapped by the caller
	}
	if n == 0 {
		return ErrNotFound
	}

	return nil
}

func (s *DB) SetReplyMessageID(ctx context.Context, id, msgID int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE captures SET reply_message_id=COALESCE(reply_message_id, ?) WHERE id=?`, msgID, id)
	if err != nil {
		return fmt.Errorf("sqlite: set reply of capture %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: set reply of capture %d: %w", id, err)
	}
	if n == 0 {
		return ErrNotFound
	}

	return nil
}

func (s *DB) NextDue(ctx context.Context, now time.Time, lane capture.Lane) (capture.Due, error) {
	var inLane string
	switch lane {
	case capture.LaneMain:
		inLane = mainLane
	case capture.LaneDocuments:
		inLane = documentsLane
	default:
		return capture.Due{}, fmt.Errorf("sqlite: unknown lane %q", lane)
	}

	c, ok, err := s.queryOne(ctx, "SELECT "+columns+" FROM captures WHERE status != 'done' AND "+
		inLane+" AND next_attempt_at <= ? ORDER BY id LIMIT 1", ts(now))
	if err != nil || ok {
		return capture.Due{Capture: c, OK: ok}, err
	}
	var wake sql.NullInt64
	err = s.db.QueryRowContext(ctx,
		"SELECT min(next_attempt_at) FROM captures WHERE status != 'done' AND "+inLane).Scan(&wake)
	if err != nil {
		return capture.Due{}, fmt.Errorf("sqlite: next wake in lane %s: %w", lane, err)
	}
	if !wake.Valid {
		return capture.Due{}, nil
	}

	return capture.Due{WakeAt: fromTS(wake.Int64)}, nil
}

func (s *DB) OldestUnfinished(ctx context.Context) (capture.Capture, bool, error) {
	return s.queryOne(ctx, "SELECT "+columns+
		" FROM captures WHERE status != 'done' ORDER BY id LIMIT 1")
}

// CountUnfinished returns the number of captures that are not done.
func (s *DB) CountUnfinished(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM captures WHERE status != 'done'").Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("sqlite: count unfinished: %w", err)
	}

	return n, nil
}

// FilePaths returns the local file names referenced by any capture file,
// finished or not, in capture and position order.
func (s *DB) FilePaths(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT local_path FROM capture_files"+
		" WHERE local_path IS NOT NULL AND local_path != '' ORDER BY capture_id, position")
	if err != nil {
		return nil, fmt.Errorf("sqlite: list file paths: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("sqlite: list file paths: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list file paths: %w", err)
	}

	return out, nil
}

// PurgeDone deletes old done captures; their capture_files rows go with them
// (ON DELETE CASCADE).
func (s *DB) PurgeDone(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM captures WHERE status = 'done' AND updated_at < ?", ts(olderThan))
	if err != nil {
		return 0, fmt.Errorf("sqlite: purge done: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite: purge done: %w", err)
	}

	return n, nil
}

func (s *DB) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("sqlite: close: %w", err)
	}

	return nil
}

// appendTx runs appendFile in its BEGIN IMMEDIATE transaction and reports
// whether item was stored; with joinOnly it is stored only on a join.
func (s *DB) appendTx(
	ctx context.Context, item capture.Capture, quiet time.Duration, joinOnly bool,
) (res capture.AppendResult, stored bool, err error) {
	if len(item.Files) != 1 {
		return capture.AppendResult{}, false, fmt.Errorf("sqlite: append file: item holds %d files, want 1",
			len(item.Files))
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return capture.AppendResult{}, false, fmt.Errorf("sqlite: append file: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return capture.AppendResult{}, false, fmt.Errorf("sqlite: append file: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			err = errors.Join(err, rollback(ctx, conn))
		}
	}()

	res, stored, err = appendFile(ctx, conn, item, s.now(), quiet, joinOnly)
	if err != nil {
		return capture.AppendResult{}, false, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return capture.AppendResult{}, false, fmt.Errorf("sqlite: append file: %w", err)
	}
	committed = true

	return res, stored, nil
}

// update persists c and its files. With read set it is guarded by
// advanceCollect and returns errBatchChanged when the guard matches no row.
func (s *DB) update(ctx context.Context, c capture.Capture, read *capture.Capture) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: update capture %d: %w", c.ID, err)
	}
	defer func() { _ = tx.Rollback() }()

	query, args := updateCapture, []any{
		string(c.Kind), ts(c.ReceivedAt), nullStr(c.AlbumID), nullStr(c.Text),
		nullStr(c.Language), nullStr(c.EnrichedJSON), nullStr(c.LLMSkipped),
		nullStr(c.Failure), nullStr(c.NoteName), nullStr(c.NoteURL),
		c.TaskCount, nullInt(c.ReplyMessageID), string(c.Step), string(c.Status),
		nullStr(string(c.WaitingOn)), c.Attempts,
		ts(c.NextAttemptAt), ts(c.UpdatedAt), c.Misconfig, nullStr(c.LastError),
		c.QueueReplyEdit, c.MinimalNote, c.SendNewReply, nullTS(c.EnrichStartedAt),
		c.ID,
	}
	if read != nil {
		query, args = advanceCollect, append(args, ts(read.NextAttemptAt), len(read.Files))
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("sqlite: update capture %d: %w", c.ID, err)
	}
	if err := oneRow(res); err != nil {
		if read != nil && errors.Is(err, ErrNotFound) {
			return errBatchChanged
		}

		return fmt.Errorf("sqlite: update capture %d: %w", c.ID, err)
	}
	for i := range c.Files {
		if err := updateFile(ctx, tx, c.ID, &c.Files[i]); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: update capture %d: %w", c.ID, err)
	}

	return nil
}

// queryOne reads the first capture query selects, with its files, in one
// transaction so the two never disagree.
func (s *DB) queryOne(ctx context.Context, query string, args ...any) (capture.Capture, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return capture.Capture{}, false, fmt.Errorf("sqlite: query capture: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	c, ok, err := scanCapture(tx.QueryRowContext(ctx, query, args...))
	if err != nil || !ok {
		return capture.Capture{}, false, err
	}
	if c.Files, err = queryFiles(ctx, tx, c.ID); err != nil {
		return capture.Capture{}, false, err
	}

	return c, true, nil
}

func scanCapture(row *sql.Row) (capture.Capture, bool, error) {
	var (
		c                                   capture.Capture
		kind, step, status                  string
		received, next, updated             int64
		enrichStarted, replyID              sql.NullInt64
		album, text, lang, enrich           sql.NullString
		skipped, failure, noteName, noteURL sql.NullString
		waiting, lastErr                    sql.NullString
	)
	err := row.Scan(
		&c.ID, &c.Source, &c.SourceChatID, &c.SourceMessageID, &kind, &received,
		&album, &text, &lang, &enrich,
		&skipped, &failure, &noteName, &noteURL, &c.TaskCount, &replyID,
		&step, &status, &waiting, &c.Attempts, &next, &updated,
		&c.Misconfig, &lastErr, &c.QueueReplyEdit, &c.MinimalNote, &c.SendNewReply,
		&enrichStarted,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return capture.Capture{}, false, nil
	}
	if err != nil {
		return capture.Capture{}, false, fmt.Errorf("sqlite: query capture: %w", err)
	}

	c.Kind, c.Step, c.Status = capture.Kind(kind), capture.Step(step), capture.Status(status)
	c.ReceivedAt, c.NextAttemptAt, c.UpdatedAt = fromTS(received), fromTS(next), fromTS(updated)
	c.AlbumID = album.String
	c.Text, c.Language, c.EnrichedJSON = text.String, lang.String, enrich.String
	c.LLMSkipped, c.Failure = skipped.String, failure.String
	c.NoteName, c.NoteURL = noteName.String, noteURL.String
	c.WaitingOn, c.LastError = capture.WaitingOn(waiting.String), lastErr.String
	c.ReplyMessageID = replyID.Int64
	if enrichStarted.Valid {
		c.EnrichStartedAt = fromTS(enrichStarted.Int64)
	}

	return c, true, nil
}

// querier is the part of *sql.Tx and *sql.Conn that reading files needs.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// queryFiles reads a capture's files in position order; nil when it has none.
func queryFiles(ctx context.Context, tx querier, captureID int64) ([]capture.File, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT "+fileColumns+" FROM capture_files WHERE capture_id = ? ORDER BY position", captureID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: query files of capture %d: %w", captureID, err)
	}
	defer func() { _ = rows.Close() }()

	var files []capture.File
	for rows.Next() {
		var (
			f                                   capture.File
			kind, state                         string
			fileID, mime, name, local, text     sql.NullString
			source, lang, failure, attachFailed sql.NullString
			size, visionStarted                 sql.NullInt64
		)
		err := rows.Scan(
			&f.ID, &f.CaptureID, &f.Position, &f.Source, &f.SourceChatID, &f.SourceMessageID,
			&kind, &fileID, &mime, &name, &size, &local, &state, &text,
			&source, &lang, &failure, &f.Timeouts, &f.Attached, &attachFailed,
			&f.VisionAttempts, &visionStarted,
		)
		if err != nil {
			return nil, fmt.Errorf("sqlite: query files of capture %d: %w", captureID, err)
		}
		f.Kind, f.State = capture.FileKind(kind), capture.FileState(state)
		f.FileID, f.MimeType, f.FileName, f.FileSize = fileID.String, mime.String, name.String, size.Int64
		f.LocalPath, f.Text, f.TextSource = local.String, text.String, capture.TextSource(source.String)
		f.Language, f.Failure, f.AttachFailure = lang.String, failure.String, attachFailed.String
		if visionStarted.Valid {
			f.VisionStartedAt = fromTS(visionStarted.Int64)
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: query files of capture %d: %w", captureID, err)
	}

	return files, nil
}

func ts(t time.Time) int64 { return t.UTC().UnixNano() }

func fromTS(n int64) time.Time { return time.Unix(0, n).UTC() }

func nullTS(t time.Time) any {
	if t.IsZero() {
		return nil
	}

	return ts(t)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}

	return s
}

func nullInt(n int64) any {
	if n == 0 {
		return nil
	}

	return n
}

func isUnique(err error) bool {
	var se *sqlite.Error

	return errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}
