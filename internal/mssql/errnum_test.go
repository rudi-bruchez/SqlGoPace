package mssql

import (
	"errors"
	"fmt"
	"testing"

	mssqldb "github.com/microsoft/go-mssqldb"
)

// fatalErr mirrors mssqldb.ServerError, whose field is unexported: a fixed message
// that hides the server's own error, reachable only through Unwrap.
type fatalErr struct{ inner mssqldb.Error }

func (e fatalErr) Error() string { return "SQL Server had internal error" }
func (e fatalErr) Unwrap() error { return e.inner }

func TestWithErrorNumber(t *testing.T) {
	full := mssqldb.Error{Number: 1105, Class: 17, State: 2, Message: "Could not allocate space for object 'dbo.T'."}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			"a fatal error's hidden number and text are surfaced",
			fatalErr{full},
			"SQL Server had internal error: Msg 1105, Level 17, State 2: Could not allocate space for object 'dbo.T'.",
		},
		{
			"an ordinary error gains its number, not a second copy of its text",
			full,
			"mssql: Could not allocate space for object 'dbo.T'. (Msg 1105, Level 17, State 2)",
		},
		{
			"a non-server error is untouched",
			errors.New("driver: bad connection"),
			"driver: bad connection",
		},
		{
			"a fatal error with nothing behind it is untouched",
			fatalErr{},
			"SQL Server had internal error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withErrorNumber(tt.err)
			if got.Error() != tt.want {
				t.Errorf("withErrorNumber() = %q, want %q", got.Error(), tt.want)
			}
		})
	}
}

// The number must survive the "execute ddl:" wrap and stay reachable by errors.As,
// which is how a caller tells 1105 from 1205 from 9002.
func TestWithErrorNumberKeepsTheDriverErrorReachable(t *testing.T) {
	err := fmt.Errorf("execute ddl: %w", withErrorNumber(fatalErr{mssqldb.Error{Number: 9002, Message: "log full"}}))
	var me mssqldb.Error
	if !errors.As(err, &me) || me.Number != 9002 {
		t.Fatalf("errors.As lost the driver error: %v", err)
	}
}
