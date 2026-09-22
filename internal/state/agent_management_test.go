package state

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func agentManagementConfiguration(t *testing.T) Configuration {
	t.Helper()
	cfg := teamConfiguration(t)
	cfg.Providers["secondary"] = ProviderConfig{Adapter: "azure-openai", BaseURL: "https://secondary.example.invalid/openai/v1"}
	cfg.QuotaGroups["secondary"] = cfg.QuotaGroups["account"]
	cfg.Models["alternate"] = ModelConfig{Provider: "secondary", Model: "selected-deployment", QuotaGroups: []string{"secondary"}, MaxOutputTokens: 512}
	cfg.Bootstrap.Models = []string{"alternate", "default"}
	cfg.Bootstrap.Tools = append(cfg.Bootstrap.Tools, "runtime.model.list", "runtime.agent.revise", "runtime.agent.retire")
	cfg.Bootstrap.Operator.Tools = append([]string{}, cfg.Bootstrap.Tools...)
	return cfg
}

// Use real transactions to arrange concurrent tasks without manufacturing model turns.
func managementTool(t *testing.T, engine *fixtureWorkflow, caller ExecutionRecord, name string, args any) (string, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := engine.store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	task, err := readTask(ctx, tx, caller.SystemID, caller.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	if len(caller.Actions) == 0 {
		caller.Actions = []ModelToolCall{{ID: "fixture-delegation"}}
	}
	result, err := engine.builtin(ctx, tx, caller, task, name, string(data))
	if err == nil {
		err = tx.Commit()
	}
	return result, err
}

func managementID(t *testing.T, engine *fixtureWorkflow, caller ExecutionRecord, name string, args any) string {
	t.Helper()
	result, err := managementTool(t, engine, caller, name, args)
	if err != nil {
		t.Fatal(err)
	}
	var ids map[string]json.RawMessage
	if err := json.Unmarshal([]byte(result), &ids); err != nil {
		t.Fatal(err)
	}
	var id string
	key := "agent_id"
	if name == "runtime.task.delegate" {
		key = "task_id"
	}
	if err := json.Unmarshal(ids[key], &id); err != nil || id == "" {
		t.Fatalf("missing %s: %s, %v", key, result, err)
	}
	return id
}

func managementAgent(t *testing.T, engine *fixtureWorkflow, systemID, id string, revision int64) AgentRecord {
	t.Helper()
	tx, err := engine.store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	agent, err := readAgent(context.Background(), tx, systemID, id, revision)
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func managementExecution(t *testing.T, engine *fixtureWorkflow, systemID, taskID string) ExecutionRecord {
	t.Helper()
	tx, err := engine.store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	task, err := readTask(context.Background(), tx, systemID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	call, err := readExecution(context.Background(), tx, systemID, task.CallID)
	if err != nil {
		t.Fatal(err)
	}
	return call
}

func TestManagedLegacyModelDigestAndAllowlistValidation(t *testing.T) {
	cfg := agentManagementConfiguration(t)
	legacy := *cfg.Bootstrap
	legacy.Models = nil
	data, err := json.Marshal(legacy)
	if err != nil || strings.Contains(string(data), `"models"`) {
		t.Fatalf("legacy configuration gained a models field: %s %v", data, err)
	}
	var persisted SystemConfig
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateSystem(persisted); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persisted.AllowedModels(), []string{"default"}) {
		t.Fatalf("legacy system gained configured aliases: %v", persisted.AllowedModels())
	}
	model := cfg.Models[persisted.Operator.Model]
	quotas := make(map[string]QuotaConfig)
	for _, name := range model.QuotaGroups {
		quotas[name] = cfg.QuotaGroups[name]
	}
	tools := make(map[string]ToolConfig)
	for _, name := range persisted.Tools {
		tools[name], _ = cfg.Tool(name)
	}
	want, _, err := JsonDigest(struct {
		Model    ModelConfig            `json:"model"`
		Provider ProviderConfig         `json:"provider"`
		Quotas   map[string]QuotaConfig `json:"quotas"`
		Profile  SandboxConfig          `json:"profile"`
		Tools    map[string]ToolConfig  `json:"tools"`
	}{model, cfg.Providers[model.Provider], quotas, cfg.SandboxProfiles[persisted.Operator.SandboxProfile], tools})
	if err != nil {
		t.Fatal(err)
	}
	grants, err := cfg.GrantsFor(persisted)
	if err != nil || grants.DefinitionsDigest != want {
		t.Fatalf("legacy five-field digest changed: got=%s want=%s err=%v", grants.DefinitionsDigest, want, err)
	}
	alternate := cfg.Models["alternate"]
	alternate.Model = "changed-ungranted-deployment"
	cfg.Models["alternate"] = alternate
	grants, err = cfg.GrantsFor(persisted)
	if err != nil || grants.DefinitionsDigest != want {
		t.Fatalf("legacy digest included an ungranted alias: %+v %v", grants, err)
	}
	for name, models := range map[string][]string{
		"unknown":         {"default", "unknown"},
		"duplicate":       {"default", "default"},
		"empty":           {},
		"missing-initial": {"alternate"},
		"malformed-alias": {"default", "bad alias"},
	} {
		t.Run(name, func(t *testing.T) {
			definition := persisted
			definition.Models = models
			if err := cfg.ValidateSystem(definition); err == nil || !strings.HasPrefix(err.Error(), "models:") {
				t.Fatalf("invalid allowlist was not rejected: %v %v", models, err)
			}
		})
	}
	persisted.Models = []string{"default", "alternate"}
	if err := cfg.ValidateSystem(persisted); err != nil {
		t.Fatalf("valid explicit allowlist rejected: %v", err)
	}
	cfg.Bootstrap = nil
	defaults, err := cfg.BootstrapSystem()
	if err != nil || len(defaults.Tools) != 15 {
		t.Fatalf("default managed tool set: %v %v", defaults.Tools, err)
	}
}

func TestManagedModelSnapshotListingAndDefinitionPins(t *testing.T) {
	cfg := agentManagementConfiguration(t)
	cfg.Bootstrap.Models = nil
	for _, name := range []string{"a", "b", "c", "d"} {
		cfg.Models[name] = cfg.Models["default"]
	}
	engine, configID := fixtureTeamEngine(t, cfg)
	root := fixtureCall(t, engine.broker, configID, "models", false)
	record, err := engine.store.GetSystem(context.Background(), localAdministrator, root.SystemID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "alternate", "b", "c", "d", "default"}
	if !reflect.DeepEqual(record.Configuration.Models, want) {
		t.Fatalf("bootstrap did not snapshot sorted aliases: %v", record.Configuration.Models)
	}
	engine.cfg.Models["foreign"] = cfg.Models["default"]
	var names []string
	after := ""
	for page := 0; page < 2; page++ {
		call := claimAction(t, engine, "runtime.model.list", map[string]string{"after": after})
		raw := invokeAction(t, engine, call)
		var result struct {
			Models []struct {
				Name, Provider, Model string
				MaxOutputTokens       int64 `json:"max_output_tokens"`
			}
			Next string
		}
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Models) != []int{5, 1}[page] || strings.Contains(raw, "https://") || strings.Contains(raw, fixtureProviderEnv) || strings.Contains(raw, fixtureProviderSecret) {
			t.Fatalf("unbounded or sensitive model page: %s", raw)
		}
		for _, model := range result.Models {
			configured := cfg.Models[model.Name]
			if model.Provider != configured.Provider || model.Model != configured.Model || model.MaxOutputTokens != configured.MaxOutputTokens {
				t.Fatalf("incorrect model metadata: %+v", model)
			}
			names = append(names, model.Name)
		}
		after = result.Next
		if page == 0 && after != "d" || page == 1 && after != "" {
			t.Fatalf("incorrect page cursor: %q", after)
		}
		finishAction(t, engine, call)
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("configured aliases expanded saved authority: %v", names)
	}
	inspected, err := engine.cfg.Inspect(record)
	if err != nil || inspected.BlockedReason != "" {
		t.Fatalf("ungranted alias blocked existing system: %s %v", inspected.BlockedReason, err)
	}
	for _, kind := range []string{"model", "provider", "quota"} {
		t.Run(kind, func(t *testing.T) {
			changed := agentManagementConfiguration(t)
			changed.Models = map[string]ModelConfig{}
			for name, model := range cfg.Models {
				changed.Models[name] = model
			}
			switch kind {
			case "model":
				model := changed.Models["alternate"]
				model.Model = "replacement"
				changed.Models["alternate"] = model
			case "provider":
				provider := changed.Providers["secondary"]
				provider.BaseURL = "https://replacement.example.invalid/openai/v1"
				changed.Providers["secondary"] = provider
			case "quota":
				quota := changed.QuotaGroups["secondary"]
				quota.TokensPerMinute++
				changed.QuotaGroups["secondary"] = quota
			}
			got, err := changed.Inspect(record)
			if err != nil || got.BlockedReason == "" {
				t.Fatalf("changed allowed %s was not blocked: %+v %v", kind, got, err)
			}
		})
	}
}

func TestManagedAgentRevisionPinsAndSelectedModel(t *testing.T) {
	engine, configID := fixtureTeamEngine(t, agentManagementConfiguration(t))
	root := fixtureCall(t, engine.broker, configID, "revision", false)
	selected := managementID(t, engine, root, "runtime.agent.propose", ProposeAgentArgs{Name: "selected", Prompt: "selected", Model: "alternate", Tools: []string{"runtime.text.analyze"}, TokenBudget: 10000})
	if got := managementAgent(t, engine, root.SystemID, selected, 0); got.Definition.Model != "alternate" || got.Definition.SandboxProfile != "worker" {
		t.Fatalf("explicit model selection changed the wrong grants: %+v", got)
	}
	child := managementID(t, engine, root, "runtime.agent.propose", ProposeAgentArgs{Name: "child", Prompt: "original prompt", Tools: []string{}, TokenBudget: 20000})
	before := managementAgent(t, engine, root.SystemID, child, 1)
	if before.Definition.Model != "default" || before.Definition.SandboxProfile != "worker" {
		t.Fatalf("child failed to inherit: %+v", before)
	}
	action := claimAction(t, engine, "runtime.agent.revise", ReviseAgentArgs{AgentID: child, ExpectedRevision: 1, Prompt: "revised prompt", Model: "alternate", Tools: []string{}})
	taskID := managementID(t, engine, action, "runtime.task.delegate", DelegateArgs{AgentID: child, Prompt: "old work"})
	result := invokeAction(t, engine, action)
	var revised struct {
		ID       string `json:"agent_id"`
		Revision int64
	}
	if err := json.Unmarshal([]byte(result), &revised); err != nil || revised.ID != child || revised.Revision != 2 {
		t.Fatalf("revision result: %s %v", result, err)
	}
	if replay := invokeAction(t, engine, action); replay != result {
		t.Fatalf("revision replay changed: %s", replay)
	}
	current := managementAgent(t, engine, root.SystemID, child, 0)
	old := managementAgent(t, engine, root.SystemID, child, 1)
	if old.Definition.Prompt != before.Definition.Prompt || old.Definition.Model != "default" || current.Definition.Model != "alternate" || current.Definition.Prompt != "revised prompt" ||
		current.TokenBudget != before.TokenBudget || current.MaxTurns != before.MaxTurns || current.Parent != before.Parent || current.Definition.SandboxProfile != before.Definition.SandboxProfile {
		t.Fatalf("revision mutated history or caps: old=%+v current=%+v", old, current)
	}
	for _, state := range []string{"queued", "running", "waiting"} {
		if _, err := engine.store.db.Exec(`UPDATE tasks SET state=? WHERE task_id=?`, state, taskID); err != nil {
			t.Fatal(err)
		}
		call := managementExecution(t, engine, root.SystemID, taskID)
		task, err := engine.store.Task(context.Background(), root.SystemID, taskID)
		if err != nil || task.Revision != 1 || call.Model != "default" || !strings.Contains(string(call.Request), "original prompt") {
			t.Fatalf("%s task lost old revision: %+v %+v %v", state, task, call, err)
		}
	}
	if _, err := engine.store.db.Exec(`UPDATE tasks SET state='completed' WHERE task_id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	next := managementID(t, engine, action, "runtime.task.delegate", DelegateArgs{AgentID: child, Prompt: "new work"})
	call := managementExecution(t, engine, root.SystemID, next)
	var request map[string]json.RawMessage
	if err := json.Unmarshal(call.Request, &request); err != nil {
		t.Fatal(err)
	}
	var group string
	if err := engine.store.db.QueryRow(`SELECT group_name FROM call_groups WHERE call_id=?`, call.CallID).Scan(&group); err != nil {
		t.Fatal(err)
	}
	if call.Model != "alternate" || string(request["model"]) != `"selected-deployment"` || string(request["max_completion_tokens"]) != "512" || request["max_tokens"] != nil || group != "secondary" || !strings.Contains(string(call.Request), "revised prompt") {
		t.Fatalf("selected model/provider/quota not used: %+v %s group=%s", call, call.Request, group)
	}
	list, err := managementTool(t, engine, action, "runtime.agent.list", struct{}{})
	if err != nil || !strings.Contains(list, `"revision":2`) {
		t.Fatalf("agent list omitted current revision: %s %v", list, err)
	}
	if _, err := engine.store.db.Exec(`UPDATE agent_revisions SET definition='{}' WHERE agent_id=?`, child); err == nil {
		t.Fatal("immutable revision was writable")
	}
	if _, err := managementTool(t, engine, action, "runtime.agent.retire", RetireAgentArgs{AgentID: child, ExpectedRevision: 2}); err != nil {
		t.Fatal(err)
	}
	if replay := invokeAction(t, engine, action); replay != result {
		t.Fatalf("revision receipt failed after retirement: %s", replay)
	}
	if got := managementAgent(t, engine, root.SystemID, child, 0); got.Revision != 2 || got.State != "stopped" {
		t.Fatalf("revision replay resurrected retired agent: %+v", got)
	}
	if _, err := engine.store.db.Exec(`UPDATE tasks SET tools=(SELECT json_group_array(json(value))
	 FROM json_each(tasks.tools) WHERE json_extract(value,'$.name')='runtime.task.delegate') WHERE task_id=?`, root.TaskID); err != nil {
		t.Fatal(err)
	}
	narrow := managementID(t, engine, action, "runtime.task.delegate", DelegateArgs{AgentID: selected, Prompt: "no borrowed tools"})
	if task, err := engine.store.Task(context.Background(), root.SystemID, narrow); err != nil || len(task.Tools) != 0 {
		t.Fatalf("alternate model lent broader tools: %+v %v", task, err)
	}
	selectedAgent := managementAgent(t, engine, root.SystemID, selected, 0)
	selectedAgent.Revision++
	selectedAgent.Definition.SandboxProfile = "different-profile"
	tx, err := engine.store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := insertAgentRevision(context.Background(), tx, selectedAgent); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE agents SET revision=? WHERE agent_id=?`, selectedAgent.Revision, selected); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET state='completed' WHERE task_id=?`, narrow); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := managementTool(t, engine, action, "runtime.task.delegate", DelegateArgs{AgentID: selected, Prompt: "no borrowed sandbox"}); err == nil {
		t.Fatal("alternate model lent a different sandbox")
	}
}

func TestManagedAgentDeniedChangesAreAtomic(t *testing.T) {
	for _, test := range []string{"stale", "self", "foreign", "old-goal", "nonoperator", "task-tools", "parent-tools", "missing-model", "ungranted-model"} {
		t.Run(test, func(t *testing.T) {
			cfg := agentManagementConfiguration(t)
			cfg.Models["ungranted"] = cfg.Models["default"]
			engine, configID := fixtureTeamEngine(t, cfg)
			root := fixtureCall(t, engine.broker, configID, "denied", false)
			child := managementID(t, engine, root, "runtime.agent.propose", ProposeAgentArgs{Name: "child", Prompt: "original", Tools: []string{"runtime.agent.propose", "runtime.agent.revise", "runtime.agent.retire"}, TokenBudget: 20000})
			caller, target := root, child
			args := ReviseAgentArgs{AgentID: target, ExpectedRevision: 1, Prompt: "changed", Model: "default", Tools: []string{}}
			switch test {
			case "stale":
				args.ExpectedRevision = 2
			case "self":
				args.AgentID = root.AgentID
			case "foreign":
				foreign := fixtureCall(t, engine.broker, configID, "foreign", false)
				args.AgentID = foreign.AgentID
			case "old-goal":
				if _, err := engine.store.db.Exec(`UPDATE agents SET goal_id='old-goal' WHERE agent_id=?`, child); err != nil {
					t.Fatal(err)
				}
			case "nonoperator", "parent-tools":
				task := managementID(t, engine, root, "runtime.task.delegate", DelegateArgs{AgentID: child, Prompt: "create descendant"})
				caller = managementExecution(t, engine, root.SystemID, task)
				target = managementID(t, engine, caller, "runtime.agent.propose", ProposeAgentArgs{Name: "descendant", Prompt: "original", Tools: []string{}, TokenBudget: 10000})
				args.AgentID = target
				if test == "parent-tools" {
					caller = root
					if _, err := managementTool(t, engine, caller, "runtime.agent.revise", ReviseAgentArgs{AgentID: child, ExpectedRevision: 1, Prompt: "narrowed parent", Model: "default", Tools: []string{}}); err != nil {
						t.Fatal(err)
					}
					args.Tools = []string{"runtime.agent.propose"}
				}
			case "task-tools":
				if _, err := engine.store.db.Exec(`UPDATE tasks SET tools=(SELECT json_group_array(json(value))
				 FROM json_each(tasks.tools) WHERE json_extract(value,'$.name')='runtime.agent.revise') WHERE task_id=?`, root.TaskID); err != nil {
					t.Fatal(err)
				}
				args.Tools = []string{"runtime.text.analyze"}
			case "missing-model", "ungranted-model":
				args.Model = strings.TrimSuffix(test, "-model")
				if _, err := managementTool(t, engine, root, "runtime.agent.propose", ProposeAgentArgs{Name: "denied", Prompt: "work", Model: args.Model, Tools: []string{}, TokenBudget: 10000}); err == nil {
					t.Fatal("unavailable model accepted for proposal")
				}
			}
			before := managementAgent(t, engine, root.SystemID, target, 0)
			var revisions int
			if err := engine.store.db.QueryRow(`SELECT count(*) FROM agent_revisions`).Scan(&revisions); err != nil {
				t.Fatal(err)
			}
			var err error
			if test == "parent-tools" {
				_, err = managementTool(t, engine, caller, "runtime.agent.revise", args)
			} else {
				action := claimAction(t, engine, "runtime.agent.revise", args)
				_, err = engine.invokeTool(context.Background(), action, action.Actions[0])
			}
			if err == nil {
				t.Fatal("invalid revision accepted")
			}
			if test == "stale" && TaskFailureReason(err) != "expected_revision: agent revision has changed" {
				t.Fatalf("stale revision reason was hidden: %v", err)
			}
			if slices.Contains([]string{"stale", "self", "foreign", "old-goal", "nonoperator"}, test) {
				if _, err := managementTool(t, engine, caller, "runtime.agent.retire", map[string]any{"agent_id": args.AgentID, "expected_revision": args.ExpectedRevision}); err == nil {
					t.Fatal("invalid retirement accepted")
				}
			}
			if after := managementAgent(t, engine, root.SystemID, target, 0); !reflect.DeepEqual(before, after) {
				t.Fatalf("denial mutated agent: before=%+v after=%+v", before, after)
			}
			assertCount(t, engine.store, "agent_revisions", revisions)
			assertCount(t, engine.store, "tool_calls", 0)
		})
	}
}

func TestManagedProtectedEvaluationCannotBeRevisedButCanBeRetired(t *testing.T) {
	ctx := context.Background()
	engine, configID := fixtureTeamEngine(t, agentManagementConfiguration(t))
	root := knowledgeCall(t, engine, configID, "protected-revision")
	check, err := engine.store.CreateChecks(ctx, "checks", root.SystemID, CreateChecksCommand{Cases: []ProtectedCase{{Input: "protected", Expected: "correct"}}})
	if err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]string)
	if err := json.Unmarshal(check.CommandResult, &ids); err != nil {
		t.Fatal(err)
	}
	draft, err := engine.store.ProposeTool(ctx, engine.cfg, "draft", root.SystemID, DraftCommand{Kind: "skill", Description: "Candidate", Content: "Answer correctly"})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(draft.CommandResult, &ids); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.store.EvaluateLearning(ctx, engine.cfg, engine.owner, "evaluate", root.SystemID,
		EvaluateCommand{ToolID: ids["tool_id"], Version: 1, CheckID: ids["check_id"], TaskID: root.TaskID}); err != nil {
		t.Fatal(err)
	}
	var agentID string
	if err := engine.store.db.QueryRow(`SELECT agent_id FROM learning_evaluations WHERE system_id=?`, root.SystemID).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	before := managementAgent(t, engine, root.SystemID, agentID, 0)
	if before.State != "active" || before.Parent != root.AgentID || before.GoalID != root.GoalID {
		t.Fatalf("fixture did not create a live same-goal evaluation child: %+v", before)
	}
	_, err = managementTool(t, engine, root, "runtime.agent.revise", ReviseAgentArgs{
		AgentID: agentID, ExpectedRevision: before.Revision, Prompt: "replace protected baseline", Model: "alternate", Tools: []string{},
	})
	if err == nil || !strings.Contains(err.Error(), "protected evaluation") {
		t.Fatalf("protected evaluation revision was not specifically denied: %v", err)
	}
	if after := managementAgent(t, engine, root.SystemID, agentID, 0); !reflect.DeepEqual(before, after) {
		t.Fatalf("denial changed protected agent: before=%+v after=%+v", before, after)
	}
	assertCount(t, engine.store, "agent_revisions", 3)
	assertCount(t, engine.store, "learning_evaluations", 1)
	raw, err := managementTool(t, engine, root, "runtime.agent.retire", RetireAgentArgs{AgentID: agentID, ExpectedRevision: before.Revision})
	if err != nil {
		t.Fatalf("protected evaluation could not be canceled: %v", err)
	}
	var result ControlResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil || result.Scope != "agent" || result.ID != agentID || result.Action != "retire" || len(result.Calls) != 2 {
		t.Fatalf("evaluation cancellation result: %s %v", raw, err)
	}
	var canceled int
	if err := engine.store.db.QueryRow(`SELECT count(*) FROM tasks WHERE system_id=? AND agent_id=? AND state='canceled' AND control='stopped'`, root.SystemID, agentID).Scan(&canceled); err != nil || canceled != 2 {
		t.Fatalf("protected baseline/candidate work survived retirement: %d %v", canceled, err)
	}
	if after := managementAgent(t, engine, root.SystemID, agentID, 0); after.State != "stopped" || after.Revision != before.Revision {
		t.Fatalf("retirement changed protected revision or left agent live: %+v", after)
	}
}

func TestManagedAgentRetirementCancelsTreesAndReplays(t *testing.T) {
	engine, configID := fixtureTeamEngine(t, agentManagementConfiguration(t))
	root := fixtureCall(t, engine.broker, configID, "retire", false)
	tools := []string{"runtime.agent.propose", "runtime.task.delegate"}
	branch := managementID(t, engine, root, "runtime.agent.propose", ProposeAgentArgs{Name: "branch", Prompt: "branch", Tools: tools, TokenBudget: 20000})
	peer := managementID(t, engine, root, "runtime.agent.propose", ProposeAgentArgs{Name: "peer", Prompt: "peer", Tools: tools, TokenBudget: 20000})
	action := claimAction(t, engine, "runtime.agent.retire", map[string]any{"agent_id": branch, "expected_revision": 1})
	branchTask := managementID(t, engine, action, "runtime.task.delegate", DelegateArgs{AgentID: branch, Prompt: "work"})
	branchCall := managementExecution(t, engine, root.SystemID, branchTask)
	if err := engine.broker.queue(context.Background(), branchCall); err != nil {
		t.Fatal(err)
	}
	admitted := admitCall(t, engine.broker, branchCall, true)
	if err := engine.broker.settle(context.Background(), admitted, ProviderResult{Known: true, Input: 5, Output: 3}, time.Now()); err != nil {
		t.Fatal(err)
	}
	descendant := managementID(t, engine, branchCall, "runtime.agent.propose", ProposeAgentArgs{Name: "descendant", Prompt: "descendant", Tools: []string{}, TokenBudget: 10000})
	peerTask := managementID(t, engine, branchCall, "runtime.task.delegate", DelegateArgs{AgentID: peer, Prompt: "work"})
	peerCall := managementExecution(t, engine, root.SystemID, peerTask)
	descendantTask := managementID(t, engine, peerCall, "runtime.task.delegate", DelegateArgs{AgentID: descendant, Prompt: "work"})
	descendantCall := managementExecution(t, engine, root.SystemID, descendantTask)
	if _, err := managementTool(t, engine, action, "runtime.agent.propose", ProposeAgentArgs{Name: "full", Prompt: "work", Tools: []string{}, TokenBudget: 1000}); err == nil {
		t.Fatal("fixture did not reach agent capacity")
	}
	before, err := engine.store.GetSystem(context.Background(), localAdministrator, root.SystemID)
	if err != nil {
		t.Fatal(err)
	}
	result := invokeAction(t, engine, action)
	var retired ControlResult
	if err := json.Unmarshal([]byte(result), &retired); err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{branchCall.CallID, peerCall.CallID, descendantCall.CallID}
	slices.Sort(wantCalls)
	slices.Sort(retired.Calls)
	if retired.Scope != "agent" || retired.ID != branch || retired.Action != "retire" || !reflect.DeepEqual(retired.Calls, wantCalls) {
		t.Fatalf("incomplete cancellation result: %+v", retired)
	}
	for _, id := range []string{branch, descendant} {
		if a := managementAgent(t, engine, root.SystemID, id, 0); a.State != "stopped" {
			t.Fatalf("creation descendant not retired: %+v", a)
		}
		if _, err := managementTool(t, engine, action, "runtime.task.delegate", DelegateArgs{AgentID: id, Prompt: "must not run"}); err == nil {
			t.Fatal("retired agent accepted delegation")
		}
	}
	for _, id := range []string{branchTask, peerTask, descendantTask} {
		task, err := engine.store.Task(context.Background(), root.SystemID, id)
		if err != nil || task.State != "canceled" || task.Control != "stopped" {
			t.Fatalf("task subtree survived retirement: %+v %v", task, err)
		}
		call := managementExecution(t, engine, root.SystemID, id)
		if id == branchTask && call.State != "completed" || id != branchTask && call.State != "canceled" {
			t.Fatalf("cancellation changed completed history or left queued calls: %+v", call)
		}
	}
	if a := managementAgent(t, engine, root.SystemID, peer, 0); a.State != "active" {
		t.Fatalf("delegation peer was retired instead of only its task: %+v", a)
	}
	if replay := invokeAction(t, engine, action); replay != result {
		t.Fatalf("retirement replay changed after target stopped: %s", replay)
	}
	after, err := engine.store.GetSystem(context.Background(), localAdministrator, root.SystemID)
	if err != nil || after.UsedTokens != before.UsedTokens || after.ReservedTokens != before.ReservedTokens || after.UsedTokens != 26 {
		t.Fatalf("retirement refunded accounting: before=%+v after=%+v %v", before, after, err)
	}
	var used int64
	if err := engine.store.db.QueryRow(`SELECT used_tokens FROM agent_usage WHERE agent_id=?`, branch).Scan(&used); err != nil || used != 8 {
		t.Fatalf("retirement lost child usage: %d %v", used, err)
	}
	assertCount(t, engine.store, "agents", 4)
	assertCount(t, engine.store, "agent_revisions", 4)
	assertCount(t, engine.store, "agent_usage", 4)
	assertCount(t, engine.store, "model_attempts", 2)
	assertCount(t, engine.store, "tool_calls", 1)
	replacement := managementID(t, engine, action, "runtime.agent.propose", ProposeAgentArgs{Name: "replacement", Prompt: "work", Tools: []string{}, TokenBudget: 1000})
	managementID(t, engine, action, "runtime.agent.propose", ProposeAgentArgs{Name: "fill", Prompt: "work", Tools: []string{}, TokenBudget: 1000})
	if _, err := managementTool(t, engine, action, "runtime.agent.retire", RetireAgentArgs{AgentID: replacement, ExpectedRevision: 1}); err != nil {
		t.Fatal(err)
	}
	managementID(t, engine, action, "runtime.agent.propose", ProposeAgentArgs{Name: "reuse-idle-slot", Prompt: "work", Tools: []string{}, TokenBudget: 1000})
}
