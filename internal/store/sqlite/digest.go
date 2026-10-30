package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/Toshik1978/voicenotes/internal/digest"
)

// LastDigestRun returns the run with the latest local date; ok is false when
// no digest has been recorded yet.
func (s *DB) LastDigestRun(ctx context.Context) (r digest.Run, ok bool, err error) {
	var (
		status string
		sentAt int64
	)
	row := s.db.QueryRowContext(ctx,
		`SELECT local_date, status, sent_at, note_count FROM digest_runs
		 ORDER BY local_date DESC LIMIT 1`)
	switch err = row.Scan(&r.LocalDate, &status, &sentAt, &r.NoteCount); {
	case err == nil:
		r.Status, r.SentAt = digest.RunStatus(status), fromTS(sentAt)

		return r, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return digest.Run{}, false, nil
	default:
		return digest.Run{}, false, fmt.Errorf("sqlite: last digest run: %w", err)
	}
}

// LastSentDigestDate returns the local date of the latest day whose digest
// was actually sent; empty and skipped days do not count. ok is false when no
// digest has been sent yet.
func (s *DB) LastSentDigestDate(ctx context.Context) (date string, ok bool, err error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT local_date FROM digest_runs WHERE status = ?
		 ORDER BY local_date DESC LIMIT 1`, string(digest.StatusSent))
	switch err = row.Scan(&date); {
	case err == nil:
		return date, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	default:
		return "", false, fmt.Errorf("sqlite: last sent digest: %w", err)
	}
}

// RecordDigestRun stores the outcome of a local day. It is idempotent: a day
// already recorded keeps its first row (FR-027, at most one per day).
func (s *DB) RecordDigestRun(ctx context.Context, r digest.Run) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO digest_runs (local_date, status, sent_at, note_count)
		 VALUES (?, ?, ?, ?)`,
		r.LocalDate, string(r.Status), ts(r.SentAt), r.NoteCount)
	if err != nil {
		return fmt.Errorf("sqlite: record digest run: %w", err)
	}

	return nil
}

// SeenNotes returns every note already listed by a digest and still in the
// inbox, keyed by Memos name with the local date it was first shown.
func (s *DB) SeenNotes(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT note_name, first_shown FROM digest_seen`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: seen notes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	seen := map[string]string{}
	for rows.Next() {
		var name, first string
		if err := rows.Scan(&name, &first); err != nil {
			return nil, fmt.Errorf("sqlite: scan seen note: %w", err)
		}
		seen[name] = first
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: seen notes: %w", err)
	}

	return seen, nil
}

// MarkSeen records the notes a digest listed (keeping an existing first_shown)
// and deletes rows for notes no longer in inbox, in one transaction.
func (s *DB) MarkSeen(ctx context.Context, listed []string, localDate string, inbox []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin mark seen: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, name := range listed {
		_, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO digest_seen (note_name, first_shown) VALUES (?, ?)`,
			name, localDate)
		if err != nil {
			return fmt.Errorf("sqlite: insert seen note: %w", err)
		}
	}

	if err := pruneSeen(ctx, tx, inbox); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit mark seen: %w", err)
	}

	return nil
}

// pruneSeen deletes the seen rows whose note is not in inbox.
func pruneSeen(ctx context.Context, tx *sql.Tx, inbox []string) error {
	names, err := seenNames(ctx, tx)
	if err != nil {
		return err
	}

	for _, name := range names {
		if slices.Contains(inbox, name) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM digest_seen WHERE note_name = ?`, name); err != nil {
			return fmt.Errorf("sqlite: delete seen note: %w", err)
		}
	}

	return nil
}

func seenNames(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT note_name FROM digest_seen`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list seen notes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("sqlite: scan seen note: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list seen notes: %w", err)
	}

	return names, nil
}
