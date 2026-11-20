package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
)

// StartReply is the "recording..." message sent when a recording started,
// kept until the recording's upload takes it as the capture's reply.
type StartReply struct {
	RecordingID int64
	ChatID      int64
	MessageID   int64
	CreatedAt   time.Time
}

// Recorder is the last known state of one microphone recorder. Alert is the
// outstanding alert name (ok, missing_receiver, storage_full, silent).
type Recorder struct {
	Name            string
	LastSeen        time.Time
	ReceiverPresent bool
	StorageFull     bool
	Alert           string
}

// InsertRecording is Insert for a microphone recording. In the same
// transaction it takes (selects and deletes) the start reply of recording
// c.SourceMessageID and, if there is one, sets c.ReplyMessageID to it, both
// stored and on c. A duplicate recording stores nothing and leaves the start
// reply in place.
func (s *DB) InsertRecording(ctx context.Context, c *capture.Capture) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: insert recording: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stored := *c
	reply, found, err := takeStartReply(ctx, tx, c.SourceMessageID)
	if err != nil {
		return err
	}
	if found {
		stored.ReplyMessageID = reply.MessageID
	}
	if err := insertCapture(ctx, tx, &stored); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: insert recording: %w", err)
	}
	*c = stored

	return nil
}

// PutStartReply records the start reply of a recording. A repeat for the
// same recording keeps the first.
func (s *DB) PutStartReply(ctx context.Context, recordingID, chatID, messageID int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO mic_replies (recording_id, chat_id, message_id, created_at)
VALUES (?,?,?,?) ON CONFLICT (recording_id) DO NOTHING`,
		recordingID, chatID, messageID, ts(s.now()))
	if err != nil {
		return fmt.Errorf("sqlite: put start reply of recording %d: %w", recordingID, err)
	}

	return nil
}

// TakeStartReply selects and deletes the start reply of a recording; ok is
// false when there is none.
func (s *DB) TakeStartReply( //nolint:revive // the four results are the specified signature
	ctx context.Context, recordingID int64,
) (chatID, messageID int64, ok bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, false, fmt.Errorf("sqlite: take start reply of recording %d: %w", recordingID, err)
	}
	defer func() { _ = tx.Rollback() }()

	r, ok, err := takeStartReply(ctx, tx, recordingID)
	if err != nil {
		return 0, 0, false, err
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, false, fmt.Errorf("sqlite: take start reply of recording %d: %w", recordingID, err)
	}

	return r.ChatID, r.MessageID, ok, nil
}

// takeStartReply selects and deletes a start reply inside tx.
func takeStartReply(ctx context.Context, tx *sql.Tx, recordingID int64) (StartReply, bool, error) {
	r := StartReply{RecordingID: recordingID}
	err := tx.QueryRowContext(ctx, "SELECT chat_id, message_id FROM mic_replies WHERE recording_id = ?",
		recordingID).Scan(&r.ChatID, &r.MessageID)
	if errors.Is(err, sql.ErrNoRows) {
		return StartReply{}, false, nil
	}
	if err != nil {
		return StartReply{}, false, fmt.Errorf("sqlite: read start reply of recording %d: %w", recordingID, err)
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM mic_replies WHERE recording_id = ?", recordingID); err != nil {
		return StartReply{}, false, fmt.Errorf("sqlite: delete start reply of recording %d: %w", recordingID, err)
	}

	return r, true, nil
}

// StaleStartReplies returns the start replies created before olderThan,
// oldest first. It deletes nothing.
func (s *DB) StaleStartReplies(ctx context.Context, olderThan time.Time) ([]StartReply, error) {
	// created_at has TEXT affinity: compare it as the integer it holds.
	rows, err := s.db.QueryContext(ctx, `SELECT recording_id, chat_id, message_id, CAST(created_at AS INTEGER)
FROM mic_replies WHERE CAST(created_at AS INTEGER) < ? ORDER BY CAST(created_at AS INTEGER), recording_id`,
		ts(olderThan))
	if err != nil {
		return nil, fmt.Errorf("sqlite: query stale start replies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []StartReply
	for rows.Next() {
		var r StartReply
		var created int64
		if err = rows.Scan(&r.RecordingID, &r.ChatID, &r.MessageID, &created); err != nil {
			return nil, fmt.Errorf("sqlite: scan stale start reply: %w", err)
		}
		r.CreatedAt = fromTS(created)
		out = append(out, r)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: query stale start replies: %w", err)
	}

	return out, nil
}

// UpsertRecorder records a check-in of recorder name. It never changes the
// outstanding alert of an existing recorder; a new one starts at "ok".
func (s *DB) UpsertRecorder(
	ctx context.Context, name string, lastSeen time.Time, receiverPresent, storageFull bool,
) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO mic_recorders (name, last_seen, receiver_present, storage_full)
VALUES (?,?,?,?) ON CONFLICT (name) DO UPDATE SET
last_seen = excluded.last_seen, receiver_present = excluded.receiver_present,
storage_full = excluded.storage_full`,
		name, ts(lastSeen), receiverPresent, storageFull)
	if err != nil {
		return fmt.Errorf("sqlite: upsert recorder: %w", err)
	}

	return nil
}

// Recorders returns every recorder, ordered by name.
func (s *DB) Recorders(ctx context.Context) ([]Recorder, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, CAST(last_seen AS INTEGER), receiver_present,
storage_full, alert FROM mic_recorders ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: query recorders: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Recorder
	for rows.Next() {
		var r Recorder
		var seen int64
		if err = rows.Scan(&r.Name, &seen, &r.ReceiverPresent, &r.StorageFull, &r.Alert); err != nil {
			return nil, fmt.Errorf("sqlite: scan recorder: %w", err)
		}
		r.LastSeen = fromTS(seen)
		out = append(out, r)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: query recorders: %w", err)
	}

	return out, nil
}

// SetRecorderAlert stores the outstanding alert of recorder name, returning
// ErrNotFound if there is no such recorder.
func (s *DB) SetRecorderAlert(ctx context.Context, name, alert string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE mic_recorders SET alert = ? WHERE name = ?", alert, name)
	if err != nil {
		return fmt.Errorf("sqlite: set alert of recorder: %w", err)
	}
	if err := oneRow(res); err != nil {
		return fmt.Errorf("sqlite: set alert of recorder: %w", err)
	}

	return nil
}

// HasStartReply reports whether a start reply is stored for the recording.
func (s *DB) HasStartReply(ctx context.Context, recordingID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mic_replies WHERE recording_id = ?`, recordingID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("sqlite: look up start reply of recording %d: %w", recordingID, err)
	}

	return n > 0, nil
}

// HasRecording reports whether a microphone capture with the recording ID
// is stored for the notify chat.
func (s *DB) HasRecording(ctx context.Context, chatID, recordingID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM captures
WHERE source = ? AND source_chat_id = ? AND source_message_id = ?`,
		capture.SourceMic, chatID, recordingID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("sqlite: look up recording %d: %w", recordingID, err)
	}

	return n > 0, nil
}
