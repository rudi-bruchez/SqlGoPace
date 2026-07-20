package mssql_test

import (
	"errors"
	"fmt"
	"testing"

	mssqldb "github.com/microsoft/go-mssqldb"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

func TestIsFileAllocationError(t *testing.T) {
	alloc := mssqldb.Error{Number: 5240, Message: "Could not adjust the space allocation for file 'X'."}
	if !mssql.IsFileAllocationError(alloc) {
		t.Error("Msg 5240 should be detected")
	}
	if !mssql.IsFileAllocationError(fmt.Errorf("execute ddl: %w", alloc)) {
		t.Error("a wrapped Msg 5240 should still be detected")
	}
	if mssql.IsFileAllocationError(mssqldb.Error{Number: 1205}) {
		t.Error("a different SQL error (deadlock) must not match")
	}
	if mssql.IsFileAllocationError(errors.New("plain error")) {
		t.Error("a non-mssql error must not match")
	}
}
