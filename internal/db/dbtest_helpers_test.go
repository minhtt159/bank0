package db

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// sqlstate returns the Postgres SQLSTATE of err, or "" if err is nil or not a
// *pgconn.PgError. DB functions RAISE typed SQLSTATEs (28P01, 28000, 42501,
// check_violation=23514, unique_violation=23505, restrict_violation=23001,
// bare RAISE EXCEPTION=P0001); tests assert on the code, not the message text.
func sqlstate(err error) string {
	if pg, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pg.Code
	}
	return ""
}
