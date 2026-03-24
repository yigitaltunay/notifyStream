package db

import "errors"

// ErrNotImplemented is returned until the repository is wired to Postgres.
var ErrNotImplemented = errors.New("db: not implemented")
