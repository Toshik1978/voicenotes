package sqlite

import (
	"context"
	"database/sql"

	"github.com/Toshik1978/voicenotes/internal/capture"
)

// UserVersion reports PRAGMA user_version for tests.
func (s *DB) UserVersion() int {
	var v int
	_ = s.db.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&v)

	return v
}

// HasTable reports whether the named table exists.
func (s *DB) HasTable(name string) bool {
	var n int
	_ = s.db.QueryRowContext(context.Background(),
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name,
	).Scan(&n)

	return n == 1
}

// Synchronous reports PRAGMA synchronous (2 means FULL).
func (s *DB) Synchronous() int {
	var v int
	_ = s.db.QueryRowContext(context.Background(), "PRAGMA synchronous").Scan(&v)

	return v
}

// CountFiles reports the number of capture_files rows.
func (s *DB) CountFiles() int {
	var n int
	_ = s.db.QueryRowContext(context.Background(), "SELECT count(*) FROM capture_files").Scan(&n)

	return n
}

// OpenAtVersion opens $dataDir/voicenotes.db without the store, applying
// migrations only up to version, to build databases of an older schema.
func OpenAtVersion(dataDir string, version int) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(dataDir))
	if err != nil {
		return nil, err
	}
	if err := migrateTo(context.Background(), db, version); err != nil {
		_ = db.Close()

		return nil, err
	}

	return db, nil
}

// Get reads one capture by ID, files included.
func (s *DB) Get(id int64) (capture.Capture, bool, error) {
	return s.queryOne(context.Background(), "SELECT "+columns+" FROM captures WHERE id=?", id)
}
