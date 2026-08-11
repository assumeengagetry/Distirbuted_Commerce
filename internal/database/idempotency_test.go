package database

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestCommitOutcomeIsKnown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "success", want: true},
		{name: "explicit rollback", err: pgx.ErrTxCommitRollback, want: true},
		{name: "constraint", err: &pgconn.PgError{Code: "23514", Severity: "ERROR"}, want: true},
		{name: "resolution unknown", err: &pgconn.PgError{Code: "08007", Severity: "ERROR"}, want: false},
		{name: "statement unknown", err: &pgconn.PgError{Code: "40003", Severity: "ERROR"}, want: false},
		{name: "connection exception", err: &pgconn.PgError{Code: "08006", Severity: "ERROR"}, want: false},
		{name: "fatal", err: &pgconn.PgError{Code: "57P01", Severity: "FATAL"}, want: false},
		{name: "generic", err: errors.New("connection lost"), want: false},
		{name: "safe before send", err: safeBeforeSendError{}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := commitOutcomeIsKnown(test.err); got != test.want {
				t.Fatalf("commitOutcomeIsKnown(%v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}

type safeBeforeSendError struct{}

func (safeBeforeSendError) Error() string     { return "safe before send" }
func (safeBeforeSendError) SafeToRetry() bool { return true }
