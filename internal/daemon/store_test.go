//go:build linux

package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jplck/microoperator/internal/state"
)

func fixtureStore(t *testing.T, cfg state.Configuration) (*state.Store, string) {
	t.Helper()
	if err := cfg.Validate(fixtureLookup); err != nil {
		t.Fatal(err)
	}
	lock, err := lockStateDirectory(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	})
	store, err := openTestStore(t, context.Background(), filepath.Join(cfg.DataDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	configID, err := store.RememberConfiguration(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return store, configID
}

func assertCount(t *testing.T, store *state.Store, table string, want int) {
	t.Helper()
	var got int
	// Table names here are fixed test literals, never request-supplied identifiers.
	if err := testDB(t, store).QueryRow("SELECT count(*) FROM " + table).Scan(&got); err != nil || got != want {
		t.Fatalf("%s rows = %d, %v; want %d", table, got, err, want)
	}
}
