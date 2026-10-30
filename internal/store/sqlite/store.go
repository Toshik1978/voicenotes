// Package sqlite is the durable capture queue backed by a local SQLite file.
// It is a thin I/O adapter: all scheduling decisions live in
// internal/capture and internal/pipeline.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
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
	// Insert stores c, sets c.ID, and returns ErrDuplicate if the identity
	// already exists.
	Insert(ctx context.Context, c *capture.Capture) error
	// NextDue enforces strict FIFO: it considers only the unfinished
	// capture with the lowest ID (the head). If the head is due
	// (NextAttemptAt not after now) it is returned with OK true. If the head
	// is not due, later captures wait too: OK is false and WakeAt is the
	// head's NextAttemptAt (UTC) so the worker can sleep until then. With an
	// empty queue OK is false and WakeAt is the zero time.
	NextDue(ctx context.Context, now time.Time) (capture.Due, error)
	// Update persists every mutable field of c, returning ErrNotFound if
	// c.ID does not exist. A zero ReplyMessageID keeps the stored one: a
	// worker copy read before ingest recorded the reply must not erase it.
	Update(ctx context.Context, c capture.Capture) error
	// SetReplyMessageID records the reply to edit, touching no other
	// column, so it cannot race the worker's step updates. A reply already
	// recorded is kept. It returns ErrNotFound if id does not exist.
	SetReplyMessageID(ctx context.Context, id, msgID int64) error
	// OldestUnfinished returns the unfinished capture with the lowest ID,
	// due or not; ok is false when the queue is empty.
	OldestUnfinished(ctx context.Context) (c capture.Capture, ok bool, err error)
	// PurgeDone deletes done captures last updated before olderThan and
	// returns how many were removed.
	PurgeDone(ctx context.Context, olderThan time.Time) (int64, error)
	// Close releases the database.
	Close() error
}

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DB is the SQLite-backed implementation of Store.
type DB struct {
	db *sql.DB
}

var _ Store = (*DB)(nil)

// Open opens (creating if needed) $dataDir/voicenotes.db, applies pending
// migrations and returns the store. The dataDir must exist.
func Open(dataDir string) (*DB, error) {
	// Pragmas are set per connection through the DSN. A single connection
	// matches the single-writer service and avoids SQLITE_BUSY.
	q := "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, DBFile)+"?"+q)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}
	db.SetMaxOpenConns(1)

	if err := migrate(context.Background(), db); err != nil {
		_ = db.Close()

		return nil, err
	}

	return &DB{db: db}, nil
}

// migrate applies migrations/NNN_*.sql in order, each in one transaction,
// tracking progress in PRAGMA user_version. It is forward-only.
func migrate(ctx context.Context, db *sql.DB) error {
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
		if version <= current {
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
file_id, file_size, mime_type, audio_path, text, language, enriched_json,
llm_skipped, failure, note_name, note_url, task_count, reply_message_id,
step, status, waiting_on, attempts, next_attempt_at, updated_at,
misconfig, last_error, queue_reply_edit, minimal_note, send_new_reply,
enrich_started_at`

func (s *DB) Insert(ctx context.Context, c *capture.Capture) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO captures (
source, source_chat_id, source_message_id, kind, received_at,
file_id, file_size, mime_type, audio_path, text, language, enriched_json,
llm_skipped, failure, note_name, note_url, task_count, reply_message_id,
step, status, waiting_on, attempts, next_attempt_at, updated_at,
misconfig, last_error, queue_reply_edit, minimal_note, send_new_reply,
enrich_started_at)
VALUES (?,?,?,?,?, ?,?,?,?,?,?,?, ?,?,?,?,?,?, ?,?,?,?,?,?, ?,?,?,?,?, ?)`,
		c.Source, c.SourceChatID, c.SourceMessageID, string(c.Kind), ts(c.ReceivedAt),
		nullStr(c.FileID), nullInt(c.FileSize), nullStr(c.MimeType), nullStr(c.AudioPath),
		nullStr(c.Text), nullStr(c.Language), nullStr(c.EnrichedJSON),
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
	c.ID = id

	return nil
}

func (s *DB) Update(ctx context.Context, c capture.Capture) error {
	res, err := s.db.ExecContext(ctx, `UPDATE captures SET
kind=?, received_at=?, file_id=?, file_size=?, mime_type=?, audio_path=?, text=?,
language=?, enriched_json=?, llm_skipped=?, failure=?, note_name=?, note_url=?,
task_count=?, reply_message_id=COALESCE(?, reply_message_id), step=?, status=?, waiting_on=?, attempts=?,
next_attempt_at=?, updated_at=?, misconfig=?, last_error=?, queue_reply_edit=?,
minimal_note=?, send_new_reply=?, enrich_started_at=?
WHERE id=?`,
		string(c.Kind), ts(c.ReceivedAt), nullStr(c.FileID), nullInt(c.FileSize),
		nullStr(c.MimeType), nullStr(c.AudioPath), nullStr(c.Text),
		nullStr(c.Language), nullStr(c.EnrichedJSON), nullStr(c.LLMSkipped),
		nullStr(c.Failure), nullStr(c.NoteName), nullStr(c.NoteURL),
		c.TaskCount, nullInt(c.ReplyMessageID), string(c.Step), string(c.Status),
		nullStr(string(c.WaitingOn)), c.Attempts,
		ts(c.NextAttemptAt), ts(c.UpdatedAt), c.Misconfig, nullStr(c.LastError),
		c.QueueReplyEdit, c.MinimalNote, c.SendNewReply, nullTS(c.EnrichStartedAt),
		c.ID)
	if err != nil {
		return fmt.Errorf("sqlite: update capture %d: %w", c.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: update capture %d: %w", c.ID, err)
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

func (s *DB) NextDue(ctx context.Context, now time.Time) (capture.Due, error) {
	head, ok, err := s.OldestUnfinished(ctx)
	if err != nil || !ok {
		return capture.Due{}, err
	}
	if head.NextAttemptAt.After(now) {
		return capture.Due{WakeAt: head.NextAttemptAt}, nil
	}

	return capture.Due{Capture: head, OK: true}, nil
}

func (s *DB) OldestUnfinished(ctx context.Context) (capture.Capture, bool, error) {
	return s.queryOne(ctx, "SELECT "+columns+
		" FROM captures WHERE status != 'done' ORDER BY id LIMIT 1")
}

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

func (s *DB) queryOne(ctx context.Context, query string, args ...any) (capture.Capture, bool, error) {
	var (
		c                                       capture.Capture
		kind, step, status                      string
		received, next, updated                 int64
		enrichStarted                           sql.NullInt64
		fileID, mime, audio, text, lang, enrich sql.NullString
		skipped, failure, noteName, noteURL     sql.NullString
		waiting, lastErr                        sql.NullString
		fileSize, replyID                       sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, query, args...).Scan(
		&c.ID, &c.Source, &c.SourceChatID, &c.SourceMessageID, &kind, &received,
		&fileID, &fileSize, &mime, &audio, &text, &lang, &enrich,
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
	c.FileID, c.MimeType, c.AudioPath = fileID.String, mime.String, audio.String
	c.Text, c.Language, c.EnrichedJSON = text.String, lang.String, enrich.String
	c.LLMSkipped, c.Failure = skipped.String, failure.String
	c.NoteName, c.NoteURL = noteName.String, noteURL.String
	c.WaitingOn, c.LastError = capture.WaitingOn(waiting.String), lastErr.String
	c.FileSize, c.ReplyMessageID = fileSize.Int64, replyID.Int64
	if enrichStarted.Valid {
		c.EnrichStartedAt = fromTS(enrichStarted.Int64)
	}

	return c, true, nil
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
