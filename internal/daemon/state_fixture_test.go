//go:build linux

package daemon

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"sync"
	"testing"

	"github.com/jplck/microoperator/internal/state"
)

var fixtureDatabases sync.Map

// Runtime tests inspect their own temporary database using an independent
// connection. No production storage handle or SQL escape hatch is exposed.
func openTestStore(t *testing.T, ctx context.Context, filename string) (*state.Store, error) {
	t.Helper()
	store, err := state.Open(ctx, filename)
	if err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: filename}).String() + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, errors.Join(err, store.Close())
	}
	db.SetMaxOpenConns(1)
	fixtureDatabases.Store(store, db)
	t.Cleanup(func() {
		fixtureDatabases.Delete(store)
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return store, nil
}

func testDB(t *testing.T, store *state.Store) *sql.DB {
	t.Helper()
	value, ok := fixtureDatabases.Load(store)
	if !ok {
		t.Fatal("store was not opened by this test fixture")
	}
	return value.(*sql.DB)
}
