package bttest

import (
	"context"
	"testing"
)

// rowCount returns the number of stored rows in tbl.
func rowCount(t *testing.T, tbl *table) int {
	t.Helper()
	n, err := tbl.rows.count(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// storedRow loads one stored row, or nil.
func storedRow(t *testing.T, tbl *table, key string) *row {
	t.Helper()
	r, err := tbl.rows.load(context.Background(), nil, key)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Stream mocks embed a nil grpc.ServerStream; handlers need a live context.
func (s *MockSampleRowKeysServer) Context() context.Context         { return context.Background() }
func (s *MockReadRowsServer) Context() context.Context              { return context.Background() }
func (s *bigtableTestingMutateRowsServer) Context() context.Context { return context.Background() }
