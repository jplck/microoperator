package state

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	fixtureProviderEnv    = "MICROOPERATOR_TEST_PROVIDER_KEY"
	fixtureProviderSecret = "fixture-provider-secret-not-real"
	fixtureControlToken   = "fixture-control-token-not-real-0123456789"
)

func fixtureStore(t *testing.T, cfg Configuration) (*Store, string) {
	t.Helper()
	if err := cfg.Validate(fixtureLookup); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), filepath.Join(cfg.DataDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	id, err := store.RememberConfiguration(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return store, id
}

type fixtureWorkflow struct {
	workflow
	broker *admission
}

func fixtureTeamEngine(t *testing.T, cfg Configuration) (*fixtureWorkflow, string) {
	t.Helper()
	store, id := fixtureStore(t, cfg)
	return &fixtureWorkflow{workflow: workflow{store: store, cfg: cfg, owner: "fixture-owner", active: map[string]string{}}, broker: &admission{store: store, cfg: cfg, now: time.Now}}, id
}

func (f *fixtureWorkflow) invokeTool(ctx context.Context, e ExecutionRecord, a ModelToolCall) (string, error) {
	plan, err := f.store.PrepareTool(ctx, f.cfg, f.owner, e, a)
	if err == nil && plan.Dispatch {
		panic("storage fixture must not execute subprocess tools")
	}
	return plan.Result, err
}

func testDB(t *testing.T, store *Store) *sql.DB {
	t.Helper()
	return store.db
}
