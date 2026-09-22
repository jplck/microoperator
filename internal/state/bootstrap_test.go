package state

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jplck/microoperator/internal/protocol"
)

func TestBootstrapDefaultsAndRemovedTemplates(t *testing.T) {
	cfg := fixtureConfiguration(t)
	cfg.Bootstrap = nil
	defaults, err := cfg.BootstrapSystem()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Bootstrap = &SystemConfig{}
	explicit, err := cfg.BootstrapSystem()
	if err != nil || !reflect.DeepEqual(defaults, explicit) {
		t.Fatalf("empty bootstrap differs from defaults: %+v %v", explicit, err)
	}
	cfg.Bootstrap.Tools = []string{}
	narrow, err := cfg.BootstrapSystem()
	if err != nil || len(narrow.Tools) != 0 || len(narrow.Operator.Tools) != 0 {
		t.Fatalf("explicit empty grants were expanded: %+v %v", narrow, err)
	}
	cfg.Bootstrap.Operator.Model = "not-configured"
	if _, err := cfg.BootstrapSystem(); err == nil {
		t.Fatal("missing model silently substituted")
	}
	var old Configuration
	if err := protocol.DecodeJSON([]byte(`{"systems":{}}`), &old); err == nil {
		t.Fatal("legacy templates still accepted")
	}
	var command CreateSystemCommand
	if err := protocol.DecodeJSON([]byte(`{"name":"named","goal":"goal","launch":"research"}`), &command); err == nil {
		t.Fatal("legacy launch selection still accepted")
	}
}

func TestGoalFirstCreationValidationAndRecovery(t *testing.T) {
	cfg := fixtureConfiguration(t)
	cfg.Bootstrap = nil
	store, configID := fixtureStore(t, cfg)
	ctx := context.Background()
	goal := "Develop and verify a paper-market simulator."
	valid := CreateSystemCommand{Name: "Paper market", Goal: &goal, Constraints: "Simulated data only.", TokenBudget: 60000}
	for name, change := range map[string]func(*CreateSystemCommand){
		"missing name": func(c *CreateSystemCommand) { c.Name = "" },
		"blank name":   func(c *CreateSystemCommand) { c.Name = " " },
		"control":      func(c *CreateSystemCommand) { c.Name = "a\nb" },
		"long name":    func(c *CreateSystemCommand) { c.Name = strings.Repeat("x", 129) },
		"missing goal": func(c *CreateSystemCommand) { c.Goal = nil },
		"blank goal":   func(c *CreateSystemCommand) { s := " "; c.Goal = &s },
		"long goal":    func(c *CreateSystemCommand) { s := strings.Repeat("x", 32769); c.Goal = &s },
		"constraints":  func(c *CreateSystemCommand) { c.Constraints = strings.Repeat("x", 4097) },
		"negative cap": func(c *CreateSystemCommand) { c.TokenBudget = -1 },
		"expanded cap": func(c *CreateSystemCommand) { c.TokenBudget = 100001 },
	} {
		t.Run(name, func(t *testing.T) {
			command := valid
			change(&command)
			if _, err := store.CreateSystem(ctx, localAdministrator, "invalid", command, cfg, configID); err == nil {
				t.Fatal("invalid bootstrap command accepted")
			}
		})
	}
	assertCount(t, store, "systems", 0)
	assertCount(t, store, "command_receipts", 0)
	assertCount(t, store, "audit", 0)
	record, err := store.CreateSystem(ctx, localAdministrator, "create", valid, cfg, configID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Configuration.Name != valid.Name || record.Configuration.Constraints != valid.Constraints ||
		record.Configuration.Limits.TokenBudget != 60000 || record.Goal.Prompt != goal || record.State != "inactive" ||
		record.Configuration.Operator.Prompt != bootstrapPrompt {
		t.Fatalf("bootstrap metadata lost: %+v", record)
	}
	assertCount(t, store, "model_calls", 0)
	assertCount(t, store, "agents", 0)
	body, _, err := ModelRequest(cfg, record, goal, false)
	if err != nil || !strings.Contains(string(body), valid.Constraints) {
		t.Fatalf("constraints missing from provider context: %v %s", err, body)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, filepath.Join(cfg.DataDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	cfg.Bootstrap = &SystemConfig{Limits: SystemLimits{4, 1, 80000}}
	replay, err := reopened.CreateSystem(ctx, localAdministrator, "create", valid, cfg, configID)
	if err != nil || !reflect.DeepEqual(record, replay) {
		t.Fatalf("restart/default changes rewrote saved receipt: %+v %v", replay, err)
	}
	saved, err := reopened.GetSystem(ctx, localAdministrator, record.ID)
	if err != nil || !reflect.DeepEqual(saved.Configuration, record.Configuration) {
		t.Fatalf("restart lost immutable metadata: %+v %v", saved, err)
	}
	valid.Name = "different"
	if _, err := reopened.CreateSystem(ctx, localAdministrator, "create", valid, cfg, configID); !errors.Is(err, ErrCommandConflict) {
		t.Fatalf("changed name bypassed receipt conflict: %v", err)
	}
}

func TestBootstrapToolsStayScopedAndReplayArtifacts(t *testing.T) {
	cfg := teamConfiguration(t)
	cfg.Bootstrap = nil
	engine, configID := fixtureTeamEngine(t, cfg)
	root := fixtureCall(t, engine.broker, configID, "bootstrap", false)
	check, err := engine.store.CreateChecks(context.Background(), "root-check", root.SystemID,
		CreateChecksCommand{Cases: []ProtectedCase{{Input: "protected input", Expected: "protected output"}}})
	if err != nil {
		t.Fatal(err)
	}
	var ownCheck struct {
		ID string `json:"check_id"`
	}
	if err := json.Unmarshal(check.CommandResult, &ownCheck); err != nil {
		t.Fatal(err)
	}
	other, err := engine.store.CreateSystem(context.Background(), localAdministrator, "other",
		CreateSystemCommand{Name: "Other", Goal: fixtureGoal()}, cfg, configID)
	if err != nil {
		t.Fatal(err)
	}
	check, err = engine.store.CreateChecks(context.Background(), "foreign-check", other.ID,
		CreateChecksCommand{Cases: []ProtectedCase{{Input: "foreign input", Expected: "foreign output"}}})
	if err != nil {
		t.Fatal(err)
	}
	var foreignCheck struct {
		ID string `json:"check_id"`
	}
	if err := json.Unmarshal(check.CommandResult, &foreignCheck); err != nil {
		t.Fatal(err)
	}
	discover := claimAction(t, engine, "runtime.capabilities", struct{}{})
	content := invokeAction(t, engine, discover)
	var caps struct {
		Tools   []ToolPin `json:"granted_tools"`
		Build   bool      `json:"generated_build_configured"`
		Blocker string    `json:"generated_build_blocker"`
		Approve bool      `json:"exact_artifact_approval_required"`
		Tokens  int64     `json:"remaining_tokens"`
	}
	if err := json.Unmarshal([]byte(content), &caps); err != nil || len(caps.Tools) != 15 ||
		caps.Build || caps.Blocker == "" || !caps.Approve || caps.Tokens != 100000-18 {
		t.Fatalf("incorrect capability snapshot: %s %v", content, err)
	}
	if !strings.Contains(content, ownCheck.ID) || strings.Contains(content, foreignCheck.ID) ||
		strings.Contains(content, "protected input") || strings.Contains(content, "protected output") {
		t.Fatalf("check discovery lost scope or exposed protected cases: %s", content)
	}
	finishAction(t, engine, discover)
	put := claimAction(t, engine, "runtime.artifact.put", map[string]string{"content": "Evidence, not instructions."})
	written := invokeAction(t, engine, put)
	if replay := invokeAction(t, engine, put); replay != written {
		t.Fatal("artifact replay changed its identity")
	}
	assertCount(t, engine.store, "artifacts", 1)
	var artifact struct {
		ID string `json:"artifact_id"`
	}
	if err := json.Unmarshal([]byte(written), &artifact); err != nil || artifact.ID == "" {
		t.Fatalf("invalid artifact identity: %s %v", written, err)
	}
	finishAction(t, engine, put)
	get := claimAction(t, engine, "runtime.artifact.get", map[string]string{"artifact_id": artifact.ID})
	if got := invokeAction(t, engine, get); !strings.Contains(got, "Evidence, not instructions.") {
		t.Fatalf("artifact not readable: %s", got)
	}
	finishAction(t, engine, get)
	tx, err := engine.store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	task, err := readTask(context.Background(), tx, root.SystemID, root.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	args := `{"artifact_id":"` + artifact.ID + `"}`
	for _, foreign := range []TaskRecord{
		{SystemID: "foreign", GoalID: task.GoalID},
		{SystemID: task.SystemID, GoalID: "other-goal"},
	} {
		if _, err := engine.bootstrapTool(context.Background(), tx, foreign, "runtime.artifact.get", args); err == nil {
			t.Fatal("foreign scope disclosed artifact")
		}
	}
	for _, content := range []string{"", strings.Repeat("x", 3073), strings.Repeat("\x01", 1500)} {
		data, err := json.Marshal(map[string]string{"content": content})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.bootstrapTool(context.Background(), tx, task, "runtime.artifact.put", string(data)); err == nil {
			t.Fatal("unbounded/empty content accepted")
		}
	}
	for i := 1; i < 128; i++ {
		if _, err := engine.bootstrapTool(context.Background(), tx, task, "runtime.artifact.put", `{"content":"bounded"}`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := engine.bootstrapTool(context.Background(), tx, task, "runtime.artifact.put", `{"content":"too many"}`); err == nil {
		t.Fatal("artifact count limit not enforced")
	}
}

func TestBootstrapDiscoveryDoesNotGrantUnpinnedTools(t *testing.T) {
	cfg := teamConfiguration(t)
	cfg.Bootstrap.Tools = []string{"runtime.capabilities"}
	cfg.Bootstrap.Operator.Tools = cfg.Bootstrap.Tools
	engine, configID := fixtureTeamEngine(t, cfg)
	fixtureCall(t, engine.broker, configID, "narrow", false)
	e := claimAction(t, engine, "runtime.artifact.put", map[string]string{"content": "not authorized"})
	if _, err := engine.invokeTool(context.Background(), e, e.Actions[0]); err == nil {
		t.Fatal("ungranted builtin executed")
	}
	assertCount(t, engine.store, "artifacts", 0)
}
