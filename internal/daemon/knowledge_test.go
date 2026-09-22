//go:build linux

package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jplck/microoperator/internal/state"
)

func knowledgeConfiguration(t *testing.T) state.Configuration {
	cfg := fixtureConfiguration(t)
	def := cfg.Systems["research"]
	def.Tools = []string{"runtime.memory.put", "runtime.memory.search", "runtime.schedule.create", "runtime.schedule.cancel", "runtime.events.subscribe", "runtime.events.unsubscribe", "runtime.task.wait", "runtime.agent.propose"}
	def.Operator.Tools = append([]string{}, def.Tools...)
	cfg.Systems["research"] = def
	q := cfg.QuotaGroups["account"]
	q.BurstRequests = 20
	q.TokensPerMinute = 1000000
	cfg.QuotaGroups["account"] = q
	return cfg
}

func knowledgeCall(t *testing.T, engine *executionEngine, id, key string) state.ExecutionRecord {
	t.Helper()
	prompt := "knowledge goal"
	s, err := engine.store.CreateSystem(context.Background(), localAdministrator, "create-"+key, state.CreateSystemCommand{Launch: "research", Goal: &prompt}, engine.cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	s, err = engine.store.StartSystem(context.Background(), localAdministrator, "start-"+key, s.ID, engine.owner, state.StartSystemCommand{ExpectedRevision: 1, LifetimeSeconds: 3600}, engine.cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return *s.Execution
}

func memoryResult(t *testing.T, value any) []state.MemoryEntry {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Untrusted bool                `json:"untrusted_data"`
		Entries   []state.MemoryEntry `json:"entries"`
		Next      string              `json:"next"`
	}
	if err := json.Unmarshal(data, &result); err != nil || !result.Untrusted {
		t.Fatalf("memory was treated as instructions: %s %v", data, err)
	}
	return result.Entries
}
