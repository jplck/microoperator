//go:build linux

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/jplck/microoperator/internal/state"
)

func teamConfiguration(t *testing.T) state.Configuration {
	cfg := fixtureConfiguration(t)
	def := cfg.Systems["research"]
	def.Tools = []string{"runtime.agent.propose", "runtime.agent.list", "runtime.task.delegate", "runtime.task.progress", "runtime.tool.propose", "runtime.text.analyze"}
	def.Operator.Tools = append([]string{}, def.Tools...)
	cfg.Systems["research"] = def
	q := cfg.QuotaGroups["account"]
	q.BurstRequests, q.RequestsPerMinute, q.TokensPerMinute = 60, 600, 1000000
	cfg.QuotaGroups["account"] = q
	return cfg
}

func fixtureTeamEngine(t *testing.T, cfg state.Configuration) (*executionEngine, string) {
	t.Helper()
	b, _, id := fixtureBroker(t, cfg)
	return &executionEngine{ctx: context.Background(), owner: "fixture-owner", store: b.store, cfg: cfg, broker: b, active: map[string]activation{}, logger: log.New(io.Discard, "", 0)}, id
}

func claimAction(t *testing.T, engine *executionEngine, name string, args any) state.ExecutionRecord {
	t.Helper()
	_, e, found, err := engine.claim(context.Background(), time.Now())
	if err != nil || !found {
		t.Fatalf("claim: %v %v", found, err)
	}
	if err := engine.broker.queue(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	admitted := admitCall(t, engine.broker, e, true)
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	action := state.ModelToolCall{ID: "action", Type: "function"}
	action.Function.Name, action.Function.Arguments = state.WireToolName(name), string(data)
	if err := engine.broker.settle(context.Background(), admitted, state.ProviderResult{Known: true, Input: 11, Output: 7, Actions: []state.ModelToolCall{action}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	record, err := engine.store.GetSystem(context.Background(), localAdministrator, e.SystemID)
	if err != nil {
		t.Fatal(err)
	}
	current := *record.Execution
	current.EventID = e.EventID
	return current
}

func invokeAction(t *testing.T, engine *executionEngine, e state.ExecutionRecord) string {
	t.Helper()
	result, err := engine.invokeTool(context.Background(), e, e.Actions[0])
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func finishAction(t *testing.T, engine *executionEngine, e state.ExecutionRecord) {
	t.Helper()
	if err := engine.finishDelivery(context.Background(), e, nil, false); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryScopeDraftsAndRevocation(t *testing.T) {
	cfg := teamConfiguration(t)
	engine, id := fixtureTeamEngine(t, cfg)
	first := fixtureCall(t, engine.broker, id, "first", false)
	second, err := engine.store.CreateSystem(context.Background(), localAdministrator, "second", state.CreateSystemCommand{Launch: "research"}, cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	handler := newControlHandler(engine.store, cfg, id, fixtureControlToken, log.New(io.Discard, "", 0), engine)
	draft := `{"kind":"executable","description":"Inert source","content":"package main\nfunc main() {}","requires_tools":[]}`
	response := controlRequest(handler, "POST", "/v1/systems/"+first.SystemID+"/tools/drafts", "draft", draft, fixtureControlToken)
	record := decodeSystemResponse(t, response, 201)
	var result struct {
		ID    string `json:"tool_id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(record.CommandResult, &result); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.ID, "local.") || result.State != "draft" {
		t.Fatal("proposal acquired authority or selected its own ID")
	}
	assertCount(t, engine.store, "tool_drafts", 1)
	replayed := decodeSystemResponse(t, controlRequest(handler, "POST", "/v1/systems/"+first.SystemID+"/tools/drafts", "draft", draft, fixtureControlToken), 201)
	if string(replayed.CommandResult) != string(record.CommandResult) {
		t.Fatal("proposal replay created a new draft")
	}
	for _, path := range []string{"/v1/tools/" + result.ID, "/v1/systems/" + second.ID + "/tools/" + result.ID} {
		if got := controlRequest(handler, "GET", path, "", "", fixtureControlToken); got.Code != 404 {
			t.Fatalf("draft leaked: %s", got.Body.String())
		}
	}
	for i, body := range []string{
		`{"tool_id":"runtime.text.analyze","kind":"skill","description":"shadow","content":"no"}`,
		`{"kind":"skill","description":"missing dependency","content":"x","requires_tools":["shared.missing"]}`,
	} {
		if got := controlRequest(handler, "POST", "/v1/systems/"+first.SystemID+"/tools/drafts", fmt.Sprintf("invalid-%d", i), body, fixtureControlToken); got.Code != 400 {
			t.Fatalf("invalid draft: %s", got.Body.String())
		}
	}
	if got := controlRequest(handler, "POST", "/v1/systems/"+first.SystemID+"/tools/"+result.ID+"/state", "activate", `{"version":1,"state":"active"}`, fixtureControlToken); got.Code != 400 {
		t.Fatal("inert source was promoted")
	}
	e := claimAction(t, engine, "runtime.agent.propose", state.ProposeAgentArgs{Name: "child", Prompt: "work", Tools: []string{"runtime.text.analyze"}, TokenBudget: 10000})
	if got := controlRequest(handler, "POST", "/v1/systems/"+first.SystemID+"/tools/revoke", "revoke", `{"name":"runtime.agent.propose","version":1}`, fixtureControlToken); got.Code != 200 {
		t.Fatalf("revocation: %s", got.Body.String())
	}
	if _, err := engine.invokeTool(context.Background(), e, e.Actions[0]); err == nil {
		t.Fatal("revoked pinned version executed")
	}
	assertCount(t, engine.store, "tool_calls", 0)
	if _, err := testDB(t, engine.store).Exec(`UPDATE tool_drafts SET content='replaced'`); err == nil {
		t.Fatal("draft content was mutable")
	}
}

func TestDelegationCannotBorrowRecipientAuthority(t *testing.T) {
	cfg := teamConfiguration(t)
	engine, id := fixtureTeamEngine(t, cfg)
	root := fixtureCall(t, engine.broker, id, "root", false)
	first := claimAction(t, engine, "runtime.agent.propose", state.ProposeAgentArgs{Name: "privileged", Prompt: "recipient", Tools: []string{"runtime.text.analyze", "runtime.agent.propose"}, TokenBudget: 20000})
	var recipient struct {
		ID string `json:"agent_id"`
	}
	if err := json.Unmarshal([]byte(invokeAction(t, engine, first)), &recipient); err != nil {
		t.Fatal(err)
	}
	replay := invokeAction(t, engine, first)
	if !strings.Contains(replay, recipient.ID) {
		t.Fatal("proposal was not idempotent")
	}
	assertCount(t, engine.store, "agents", 2)
	finishAction(t, engine, first)
	second := claimAction(t, engine, "runtime.agent.propose", state.ProposeAgentArgs{Name: "narrow", Prompt: "caller", Tools: []string{"runtime.task.delegate"}, TokenBudget: 15000})
	var caller struct {
		ID string `json:"agent_id"`
	}
	if err := json.Unmarshal([]byte(invokeAction(t, engine, second)), &caller); err != nil {
		t.Fatal(err)
	}
	finishAction(t, engine, second)
	third := claimAction(t, engine, "runtime.task.delegate", state.DelegateArgs{AgentID: caller.ID, Prompt: "Ask for narrow work"})
	invokeAction(t, engine, third)
	finishAction(t, engine, third)
	narrow := claimAction(t, engine, "runtime.task.delegate", state.DelegateArgs{AgentID: recipient.ID, Prompt: "Do not borrow your broader tools"})
	if narrow.AgentID != caller.ID {
		t.Fatal("wrong addressed recipient")
	}
	invokeAction(t, engine, narrow)
	finishAction(t, engine, narrow)
	recipientCall := claimAction(t, engine, "runtime.text.analyze", state.TextArguments{Text: "forbidden"})
	task, err := engine.taskSnapshot(context.Background(), root.SystemID, recipientCall.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(task.Tools) != 0 {
		t.Fatalf("recipient borrowed broader grants: %+v", task.Tools)
	}
	if _, err := engine.invokeTool(context.Background(), recipientCall, recipientCall.Actions[0]); err == nil {
		t.Fatal("confused-deputy execution accepted")
	}
	if _, err := testDB(t, engine.store).Exec(`UPDATE agent_revisions SET definition='{}'`); err == nil {
		t.Fatal("agent revision was mutable")
	}
	var used int64
	if err := testDB(t, engine.store).QueryRow(`SELECT used_tokens FROM agent_usage WHERE system_id=? AND agent_id=? AND goal_id=?`, root.SystemID, caller.ID, root.GoalID).Scan(&used); err != nil || used != 36 {
		t.Fatalf("delegation escaped caller aggregate budget: used=%d err=%v", used, err)
	}
}

func TestToolSessionAndSchemaDenials(t *testing.T) {
	cfg := teamConfiguration(t)
	engine, id := fixtureTeamEngine(t, cfg)
	e0 := fixtureCall(t, engine.broker, id, "one", false)
	e := claimAction(t, engine, "runtime.agent.propose", state.ProposeAgentArgs{Name: "child", Prompt: "work", Tools: []string{}, TokenBudget: 100})
	for _, mutate := range []func(*state.ExecutionRecord){
		func(e *state.ExecutionRecord) { e.SystemID = "sys_00000000000000000000000000000000" },
		func(e *state.ExecutionRecord) { e.ActivationID = "foreign" },
		func(e *state.ExecutionRecord) { e.EventID = "foreign" },
	} {
		bad := e
		mutate(&bad)
		if _, err := engine.invokeTool(context.Background(), bad, e.Actions[0]); err == nil {
			t.Fatal("foreign session accepted")
		}
	}
	def, _ := cfg.Tool("runtime.text.analyze")
	schema, err := state.ExecutableSchema("runtime.text.analyze", def)
	if err != nil {
		t.Fatal(err)
	}
	for _, arguments := range []string{`{}`, `null`, `{"text":null}`, `{"text":42}`, `{"text":"x","path":"/etc/passwd"}`, `{"text":"x","text":"y"}`} {
		if err := state.ValidateArguments(schema.Function, arguments); err == nil {
			t.Fatalf("invalid arguments accepted: %s", arguments)
		}
	}
	if _, err := testDB(t, engine.store).Exec(`INSERT INTO call_groups(call_id,system_id,group_name) VALUES(?,'foreign','bad')`, e0.CallID); err == nil {
		t.Fatal("call groups lost system scoping")
	}
}

func TestScopedPauseResumeStopAndDurableInput(t *testing.T) {
	for _, scope := range []string{"system", "goal", "agent"} {
		t.Run(scope, func(t *testing.T) {
			cfg := fixtureConfiguration(t)
			engine, id := fixtureTeamEngine(t, cfg)
			e := fixtureCall(t, engine.broker, id, scope, false)
			handler := newControlHandler(engine.store, cfg, id, fixtureControlToken, log.New(io.Discard, "", 0), engine)
			path := "/v1/systems/" + e.SystemID
			if scope == "goal" {
				path += "/goals/" + e.GoalID
			}
			if scope == "agent" {
				path += "/agents/" + e.AgentID
			}
			if response := controlRequest(handler, "POST", path+"/pause", "pause", `{}`, fixtureControlToken); response.Code != 202 {
				t.Fatalf("pause: %s", response.Body.String())
			}
			for i := 0; i < 2; i++ {
				response := controlRequest(handler, "POST", "/v1/systems/"+e.SystemID+"/input", "input", `{"content":"persisted follow-up"}`, fixtureControlToken)
				if response.Code != 202 {
					t.Fatalf("input: %s", response.Body.String())
				}
			}
			if _, _, found, err := engine.claim(context.Background(), time.Now()); err != nil || found {
				t.Fatalf("paused work claimed: %v %v", found, err)
			}
			if err := engine.store.RecoverExecutions(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, _, found, err := engine.claim(context.Background(), time.Now()); err != nil || found {
				t.Fatalf("restart resumed paused work: %v %v", found, err)
			}
			if response := controlRequest(handler, "POST", path+"/resume", "resume", `{}`, fixtureControlToken); response.Code != 202 {
				t.Fatalf("resume: %s", response.Body.String())
			}
			_, claimed, found, err := engine.claim(context.Background(), time.Now())
			if err != nil || !found {
				t.Fatalf("resume claim: %v %v", found, err)
			}
			var request struct {
				Messages []state.ChatMessage `json:"messages"`
			}
			if err := json.Unmarshal(claimed.Request, &request); err != nil {
				t.Fatal(err)
			}
			seen := 0
			for _, message := range request.Messages {
				if message.Content == "persisted follow-up" {
					seen++
				}
			}
			if seen != 1 {
				t.Fatalf("accepted input lost or duplicated: %d", seen)
			}
			if response := controlRequest(handler, "POST", path+"/stop", "stop", `{}`, fixtureControlToken); response.Code != 202 {
				t.Fatalf("stop: %s", response.Body.String())
			}
			if err := engine.finishDelivery(context.Background(), claimed, context.Canceled, true); err != nil {
				t.Fatal(err)
			}
			task, err := engine.taskSnapshot(context.Background(), e.SystemID, e.TaskID)
			if err != nil || task.State != "canceled" {
				t.Fatalf("stopped task: %+v %v", task, err)
			}
			if _, _, found, err := engine.claim(context.Background(), time.Now()); err != nil || found {
				t.Fatalf("stopped work resumed: %v %v", found, err)
			}
			assertCount(t, engine.store, "model_attempts", 0)
		})
	}
}

func TestRecoveryReusesCompletedModelAndToolReceipt(t *testing.T) {
	cfg := teamConfiguration(t)
	engine, id := fixtureTeamEngine(t, cfg)
	fixtureCall(t, engine.broker, id, "recover", false)
	e := claimAction(t, engine, "runtime.agent.propose", state.ProposeAgentArgs{Name: "child", Prompt: "reviewed prompt", Tools: []string{}, TokenBudget: 1000})
	result := invokeAction(t, engine, e)
	if err := engine.store.RecoverExecutions(context.Background()); err != nil {
		t.Fatal(err)
	}
	engine.owner = "recovered-owner"
	_, recovered, found, err := engine.claim(context.Background(), time.Now())
	if err != nil || !found || recovered.CallID != e.CallID {
		t.Fatalf("recovery discarded completed call: %+v %v", recovered, err)
	}
	if _, err := engine.invokeTool(context.Background(), e, e.Actions[0]); err == nil {
		t.Fatal("superseded process session retained authority")
	}
	cached, err := engine.broker.call(context.Background(), recovered)
	if err != nil || cached.State != "completed" {
		t.Fatalf("cached call: %+v %v", cached, err)
	}
	if replay := invokeAction(t, engine, recovered); replay != result {
		t.Fatalf("tool receipt replay changed result: %s", replay)
	}
	assertCount(t, engine.store, "agents", 2)
	assertCount(t, engine.store, "tool_calls", 1)
	assertCount(t, engine.store, "model_attempts", 1)
	finishAction(t, engine, recovered)
	finishAction(t, engine, recovered)
	assertCount(t, engine.store, "model_calls", 2)
}

func TestWaitingDelegationAndAppliedResultSurviveRecovery(t *testing.T) {
	cfg := teamConfiguration(t)
	engine, id := fixtureTeamEngine(t, cfg)
	root := fixtureCall(t, engine.broker, id, "waiting", false)
	propose := claimAction(t, engine, "runtime.agent.propose", state.ProposeAgentArgs{Name: "child", Prompt: "work", Tools: []string{}, TokenBudget: 10000})
	var child struct {
		ID string `json:"agent_id"`
	}
	if err := json.Unmarshal([]byte(invokeAction(t, engine, propose)), &child); err != nil {
		t.Fatal(err)
	}
	finishAction(t, engine, propose)
	delegate := claimAction(t, engine, "runtime.task.delegate", state.DelegateArgs{AgentID: child.ID, Prompt: "child work"})
	invokeAction(t, engine, delegate)
	// Crash after delegation commits, before the waiting worker acknowledges.
	if err := engine.store.RecoverExecutions(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, e, found, err := engine.claim(context.Background(), time.Now())
	if err != nil || !found || e.AgentID != child.ID {
		t.Fatalf("child was lost: %+v %v", e, err)
	}
	if err := engine.broker.queue(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	dispatched := admitCall(t, engine.broker, e, true)
	if err := engine.broker.settle(context.Background(), dispatched, state.ProviderResult{Known: true, Input: 11, Output: 7, Text: "durable child result"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	finishAction(t, engine, e)
	_, parent, found, err := engine.claim(context.Background(), time.Now())
	if err != nil || !found || parent.AgentID != root.AgentID {
		t.Fatalf("parent not woken: %+v %v", parent, err)
	}
	if err := engine.store.RecoverExecutions(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, again, found, err := engine.claim(context.Background(), time.Now())
	if err != nil || !found || again.CallID != parent.CallID {
		t.Fatalf("result applied twice: %+v %v", again, err)
	}
	var request struct {
		Messages []state.ChatMessage `json:"messages"`
	}
	if err := json.Unmarshal(again.Request, &request); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, message := range request.Messages {
		if message.Role == "tool" && strings.Contains(message.Content, "durable child result") {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("child result missing/duplicated: %d", seen)
	}
	assertCount(t, engine.store, "agents", 2)
	assertCount(t, engine.store, "tasks", 2)
	assertCount(t, engine.store, "model_calls", 4)
	assertCount(t, engine.store, "model_attempts", 3)
}

func TestUnknownToolOutcomeCannotReplay(t *testing.T) {
	cfg := teamConfiguration(t)
	engine, id := fixtureTeamEngine(t, cfg)
	fixtureCall(t, engine.broker, id, "unknown", false)
	e := claimAction(t, engine, "runtime.text.analyze", state.TextArguments{Text: "never redispatch"})
	hash, _, err := state.JsonDigest(e.Actions[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testDB(t, engine.store).Exec(`INSERT INTO tool_calls(system_id,call_id,tool_call_id,task_id,name,version,arguments_hash,state) VALUES(?,?,?,?,?,1,?,'running')`, e.SystemID, e.CallID, e.Actions[0].ID, e.TaskID, "runtime.text.analyze", hash); err != nil {
		t.Fatal(err)
	}
	if err := engine.store.RecoverExecutions(context.Background()); err != nil {
		t.Fatal(err)
	}
	task, err := engine.taskSnapshot(context.Background(), e.SystemID, e.TaskID)
	if err != nil || task.State != "failed" {
		t.Fatalf("unknown effect recovered as success: %+v %v", task, err)
	}
	if _, _, found, err := engine.claim(context.Background(), time.Now()); err != nil || found {
		t.Fatalf("unknown effect was retried: %v %v", found, err)
	}
	if _, err := engine.invokeTool(context.Background(), e, e.Actions[0]); err == nil {
		t.Fatal("unknown receipt replayed")
	}
	assertCount(t, engine.store, "model_attempts", 1)
	assertCount(t, engine.store, "artifacts", 0)
}

func TestAgentAssignmentPinsAndDirectory(t *testing.T) {
	cfg := teamConfiguration(t)
	engine, id := fixtureTeamEngine(t, cfg)
	root := fixtureCall(t, engine.broker, id, "assignment", false)
	propose := claimAction(t, engine, "runtime.agent.propose", state.ProposeAgentArgs{Name: "child", Prompt: "work", Tools: []string{"runtime.text.analyze"}, TokenBudget: 10000})
	var child struct {
		ID string `json:"agent_id"`
	}
	if err := json.Unmarshal([]byte(invokeAction(t, engine, propose)), &child); err != nil {
		t.Fatal(err)
	}
	finishAction(t, engine, propose)
	directory := claimAction(t, engine, "runtime.agent.list", struct{}{})
	result := invokeAction(t, engine, directory)
	if !strings.Contains(result, `"available":true`) || !strings.Contains(result, "runtime.text.analyze") {
		t.Fatalf("directory lacks availability/capabilities: %s", result)
	}
	finishAction(t, engine, directory)
	delegate := claimAction(t, engine, "runtime.task.delegate", state.DelegateArgs{AgentID: child.ID, Prompt: "pinned work"})
	var task struct {
		ID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(invokeAction(t, engine, delegate)), &task); err != nil {
		t.Fatal(err)
	}
	handler := newControlHandler(engine.store, cfg, id, fixtureControlToken, engine.logger, engine)
	path := "/v1/systems/" + root.SystemID + "/agents/" + child.ID + "/tools"
	if response := controlRequest(handler, "PUT", path, "assign", `{"expected_revision":1,"tools":[]}`, fixtureControlToken); response.Code != 200 {
		t.Fatalf("assign: %s", response.Body.String())
	}
	if response := controlRequest(handler, "PUT", path, "stale", `{"expected_revision":1,"tools":[]}`, fixtureControlToken); response.Code != 409 {
		t.Fatalf("stale assignment: %s", response.Body.String())
	}
	if response := controlRequest(handler, "PUT", path, "ungranted", `{"expected_revision":2,"tools":[{"name":"runtime.text.analyze","version":999,"digest":"fake"}]}`, fixtureControlToken); response.Code != 400 {
		t.Fatalf("forged pin: %s", response.Body.String())
	}
	pinned, err := engine.taskSnapshot(context.Background(), root.SystemID, task.ID)
	if err != nil || pinned.Revision != 1 || len(pinned.Tools) != 1 {
		t.Fatalf("assignment rewrote waiting task: %+v %v", pinned, err)
	}
}

func TestDelayedRevocationAndNarrowToolProfile(t *testing.T) {
	for _, revoke := range []bool{true, false} {
		t.Run(fmt.Sprint(revoke), func(t *testing.T) {
			cfg := teamConfiguration(t)
			if !revoke {
				profile := cfg.SandboxProfiles["worker"]
				profile.ReadWrite = []string{"scratch"}
				cfg.SandboxProfiles["worker"] = profile
			}
			engine, id := fixtureTeamEngine(t, cfg)
			root := fixtureCall(t, engine.broker, id, "denial", false)
			if revoke {
				if _, err := testDB(t, engine.store).Exec(`INSERT INTO tool_revocations(system_id,name,version) VALUES(?,'runtime.text.analyze',1)`, root.SystemID); err != nil {
					t.Fatal(err)
				}
				if _, _, found, err := engine.claim(context.Background(), time.Now()); err != nil || found {
					t.Fatalf("revoked delayed work ran: %v %v", found, err)
				}
				assertCount(t, engine.store, "model_attempts", 0)
			} else {
				e := claimAction(t, engine, "runtime.text.analyze", state.TextArguments{Text: "denied", Save: true})
				if _, err := engine.invokeTool(context.Background(), e, e.Actions[0]); err == nil {
					t.Fatal("tool borrowed broader workspace grant")
				}
				assertCount(t, engine.store, "tool_calls", 0)
			}
		})
	}
}

func TestFullMailboxDoesNotPreventStop(t *testing.T) {
	cfg := fixtureConfiguration(t)
	engine, id := fixtureTeamEngine(t, cfg)
	root := fixtureCall(t, engine.broker, id, "full", false)
	handler := newControlHandler(engine.store, cfg, id, fixtureControlToken, engine.logger, engine)
	rejected := false
	for i := 0; i < 65; i++ {
		response := controlRequest(handler, "POST", "/v1/systems/"+root.SystemID+"/input", fmt.Sprintf("input-%d", i), `{"content":"bounded"}`, fixtureControlToken)
		if response.Code == 400 {
			rejected = true
			break
		}
		if response.Code != 202 {
			t.Fatalf("input: %s", response.Body.String())
		}
	}
	if !rejected {
		t.Fatal("recipient queue is unbounded")
	}
	response := controlRequest(handler, "POST", "/v1/systems/"+root.SystemID+"/stop", "stop", `{}`, fixtureControlToken)
	if response.Code != 202 {
		t.Fatalf("full mailbox blocked stop: %s", response.Body.String())
	}
	var pending int
	if err := testDB(t, engine.store).QueryRow(`SELECT count(*) FROM mailboxes WHERE state IN ('pending','leased')`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("stopped input lost without receipt: %d %v", pending, err)
	}
	assertCount(t, engine.store, "model_attempts", 0)
}
