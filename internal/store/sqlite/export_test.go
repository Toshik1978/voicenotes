package sqlite

import "context"

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
