//go:build integration && (darwin || linux)

package daemon

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jplck/microoperator/internal/state"
)

func TestAgentProposesAndWaitsForProtectedEvaluation(t *testing.T) {
	var protectedID atomic.Value
	protectedID.Store("")
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []state.ChatMessage   `json:"messages"`
			Tools    []state.ModelFunction `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		last := request.Messages[len(request.Messages)-1]
		switch {
		case last.Content == "Improve":
			functionCompletion(w, "runtime.tool.propose", "propose", state.DraftCommand{Kind: "skill", Description: "Proposed learning", Content: "Use the approved answer", Requires: []string{}})
		case last.Role == "tool":
			var result struct {
				ToolID string `json:"tool_id"`
			}
			if err := json.Unmarshal([]byte(last.Content), &result); err != nil || result.ToolID == "" {
				t.Errorf("proposal result: %s %v", last.Content, err)
				return
			}
			functionCompletion(w, "runtime.learning.evaluate", "evaluate", map[string]any{"tool_id": result.ToolID, "version": 1, "check_id": protectedID.Load().(string)})
		case last.Content == "protected":
			if strings.Contains(request.Messages[0].Content, "approved answer") {
				completion(w, "correct")
			} else {
				completion(w, "baseline")
			}
		case strings.HasPrefix(last.Content, "Protected evaluation "):
			if !strings.Contains(last.Content, "pending_approval") {
				t.Errorf("evaluation did not pass: %s", last.Content)
			}
			if len(request.Tools) != 2 {
				t.Error("evaluation silently changed agent grants")
			}
			completion(w, "Ready for human approval")
		default:
			t.Errorf("unexpected learning continuation: %+v", request.Messages)
		}
	})
	def := cfg.Systems["research"]
	def.Tools = []string{"runtime.tool.propose", "runtime.learning.evaluate"}
	def.Operator.Tools = def.Tools
	def.Limits.MaxActiveAgents = 1
	cfg.Systems["research"] = def
	q := cfg.QuotaGroups["account"]
	q.BurstRequests = 20
	cfg.QuotaGroups["account"] = q
	filename := filepath.Join(t.TempDir(), "daemon.json")
	writeFixtureConfiguration(t, filename, cfg)
	d := startDaemonFixture(t, filename, fixtureControlToken)
	system := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "create", map[string]string{"launch": "research"}, 201)
	base := "/v1/systems/" + system.ID
	check := daemonSystem(t, d, fixtureControlToken, "POST", base+"/learning/checks", "checks", map[string]any{"cases": []state.ProtectedCase{{Input: "protected", Expected: "correct"}}}, 201)
	protectedID.Store(learningResult(t, check, "check_id"))
	goal := "Improve"
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/start", "start", state.StartSystemCommand{ExpectedRevision: 1, Goal: &goal}, 202)
	result := waitSystem(t, d, system.ID, func(s state.SystemRecord) bool { return s.State == "inactive" })
	if result.Execution.Response != "Ready for human approval" || count.Load() != 5 {
		t.Fatalf("durable learning failed: %+v calls=%d", result.Execution, count.Load())
	}
	status, data := daemonRequest(t, d, fixtureControlToken, "GET", base+"/learning", "", nil)
	if status != 200 {
		t.Fatal(string(data))
	}
	var evaluations struct {
		Items []evaluationView `json:"items"`
	}
	if err := json.Unmarshal(data, &evaluations); err != nil {
		t.Fatal(err)
	}
	if len(evaluations.Items) != 1 || evaluations.Items[0].State != "pending_approval" {
		t.Fatalf("agent approved or lost its proposal: %s", data)
	}
}

type evaluationView struct {
	ID       string                   `json:"evaluation_id"`
	State    string                   `json:"state"`
	Digest   string                   `json:"digest"`
	Reason   string                   `json:"reason"`
	Evidence []state.LearningEvidence `json:"evidence"`
	Artifact string                   `json:"artifact_digest"`
}

func waitEvaluation(t *testing.T, d *daemonFixture, systemID, id string) evaluationView {
	t.Helper()
	timeout := time.NewTimer(135 * time.Second)
	defer timeout.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		status, data := daemonRequest(t, d, fixtureControlToken, "GET", "/v1/systems/"+systemID+"/learning", "", nil)
		if status != 200 {
			t.Fatalf("evaluations: %d %s", status, data)
		}
		var view struct {
			Items []evaluationView `json:"items"`
		}
		if err := json.Unmarshal(data, &view); err != nil {
			t.Fatal(err)
		}
		for _, item := range view.Items {
			if item.ID == id && item.State != "queued" && item.State != "evaluating" {
				return item
			}
		}
		select {
		case <-timeout.C:
			t.Fatalf("evaluation did not finish: %s", data)
		case <-tick.C:
		}
	}
}

func learningResult(t *testing.T, record state.SystemRecord, key string) string {
	t.Helper()
	var result map[string]json.RawMessage
	if err := json.Unmarshal(record.CommandResult, &result); err != nil {
		t.Fatal(err)
	}
	var value string
	if err := json.Unmarshal(result[key], &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestProtectedSkillLearningApprovalReuseAndLimits(t *testing.T) {
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []state.ChatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		last := request.Messages[len(request.Messages)-1].Content
		if last == "wait for review" {
			functionCompletion(w, "runtime.task.wait", "wait", map[string]string{"reason": "human review"})
			return
		}
		improved := false
		for _, message := range request.Messages {
			if message.Role == "system" && strings.Contains(message.Content, "approved answer") {
				improved = true
			}
		}
		if improved {
			completion(w, "correct")
		} else {
			completion(w, "baseline")
		}
	})
	def := cfg.Systems["research"]
	def.Tools = []string{"runtime.task.wait"}
	def.Operator.Tools = def.Tools
	def.Limits.MaxActiveAgents = 1
	cfg.Systems["research"] = def
	q := cfg.QuotaGroups["account"]
	q.BurstRequests = 20
	cfg.QuotaGroups["account"] = q
	filename := filepath.Join(t.TempDir(), "daemon.json")
	writeFixtureConfiguration(t, filename, cfg)
	d := startDaemonFixture(t, filename, fixtureControlToken)
	system := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "create", map[string]string{"launch": "research", "goal": "wait for review"}, 201)
	base := "/v1/systems/" + system.ID
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/start", "start", state.StartSystemCommand{ExpectedRevision: 1, LifetimeSeconds: 3600}, 202)
	waiting := waitSystem(t, d, system.ID, func(s state.SystemRecord) bool { return s.Execution.TaskState == "waiting" })
	check := daemonSystem(t, d, fixtureControlToken, "POST", base+"/learning/checks", "checks", map[string]any{"cases": []state.ProtectedCase{{Input: "protected input", Expected: "correct"}}}, 201)
	checkID := learningResult(t, check, "check_id")
	draft := daemonSystem(t, d, fixtureControlToken, "POST", base+"/tools/drafts", "draft", state.DraftCommand{Kind: "skill", Description: "Learned prompt fragment", Content: "Use the approved answer", Requires: []string{}}, 201)
	toolID := learningResult(t, draft, "tool_id")
	evaluate := state.EvaluateCommand{ToolID: toolID, Version: 1, CheckID: checkID, TaskID: waiting.Execution.TaskID}
	job := daemonSystem(t, d, fixtureControlToken, "POST", base+"/learning/evaluate", "evaluate", evaluate, 202)
	id := learningResult(t, job, "evaluation_id")
	replayed := daemonSystem(t, d, fixtureControlToken, "POST", base+"/learning/evaluate", "evaluate", evaluate, 202)
	if learningResult(t, replayed, "evaluation_id") != id {
		t.Fatal("evaluation replay created new work")
	}
	evaluation := waitEvaluation(t, d, system.ID, id)
	if evaluation.State != "pending_approval" || len(evaluation.Evidence) != 1 || evaluation.Evidence[0].BaselinePassed || !evaluation.Evidence[0].CandidatePassed {
		t.Fatalf("protected evaluation: %+v", evaluation)
	}
	if count.Load() != 3 {
		t.Fatalf("expected root and two budgeted model calls, got %d", count.Load())
	}
	status, data := daemonRequest(t, d, fixtureControlToken, "GET", base+"/model-calls", "", nil)
	if status != 200 {
		t.Fatal(string(data))
	}
	var calls struct {
		Calls []state.ExecutionRecord `json:"calls"`
	}
	if err := json.Unmarshal(data, &calls); err != nil {
		t.Fatal(err)
	}
	if len(calls.Calls) != 3 {
		t.Fatal("evaluation bypassed shared broker history")
	}
	approval := map[string]any{"digest": strings.Repeat("0", 64), "expires_seconds": 3600, "task_uses": 1}
	if status, _ := daemonRequest(t, d, fixtureControlToken, "POST", base+"/learning/"+id+"/approve", "wrong", approval); status != 400 {
		t.Fatal("stale evidence approved")
	}
	approval["digest"] = evaluation.Digest
	approved := daemonSystem(t, d, fixtureControlToken, "POST", base+"/learning/"+id+"/approve", "approve", approval, 200)
	var pin state.ToolPin
	if err := json.Unmarshal(approved.CommandResult, &pin); err != nil {
		t.Fatal(err)
	}
	if status, _ := daemonRequest(t, d, fixtureControlToken, "POST", base+"/learning/"+id+"/approve", "approve-again", approval); status != 400 {
		t.Fatal("approval use allowance reset by replay")
	}
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/learning/feedback", "feedback", map[string]string{"task_id": waiting.Execution.TaskID, "rating": "failure", "content": "Baseline did not produce the protected answer"}, 201)
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/tools/drafts", "next-version", state.DraftCommand{ToolID: toolID, ExpectedVersion: 1, Kind: "skill", Description: "Next approved revision", Content: "Use the approved answer, revised", Requires: []string{}}, 201)
	evaluate.Version = 2
	nextJob := daemonSystem(t, d, fixtureControlToken, "POST", base+"/learning/evaluate", "next-evaluation", evaluate, 202)
	nextEvaluation := waitEvaluation(t, d, system.ID, learningResult(t, nextJob, "evaluation_id"))
	if nextEvaluation.State != "pending_approval" {
		t.Fatalf("next version: %+v", nextEvaluation)
	}
	nextApproval := daemonSystem(t, d, fixtureControlToken, "POST", base+"/learning/"+nextEvaluation.ID+"/approve", "next-approve", map[string]any{"digest": nextEvaluation.Digest, "expires_seconds": 3600, "task_uses": 1}, 200)
	var nextPin state.ToolPin
	if err := json.Unmarshal(nextApproval.CommandResult, &nextPin); err != nil {
		t.Fatal(err)
	}
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/tools/drafts", "bad-version", state.DraftCommand{ToolID: toolID, ExpectedVersion: 2, Kind: "skill", Description: "Rejected revision", Content: "An incorrect answer", Requires: []string{}}, 201)
	evaluate.Version = 3
	bad := daemonSystem(t, d, fixtureControlToken, "POST", base+"/learning/evaluate", "bad-evaluation", evaluate, 202)
	badEvaluation := waitEvaluation(t, d, system.ID, learningResult(t, bad, "evaluation_id"))
	if badEvaluation.State != "failed" {
		t.Fatalf("failed candidate not quarantined: %+v", badEvaluation)
	}
	if status, _ := daemonRequest(t, d, fixtureControlToken, "POST", base+"/learning/"+badEvaluation.ID+"/approve", "approve-bad", map[string]any{"digest": badEvaluation.Digest, "expires_seconds": 3600, "task_uses": 1}); status != 400 {
		t.Fatal("failed candidate approved")
	}
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/stop", "stop", struct{}{}, 202)
	waitSystem(t, d, system.ID, func(s state.SystemRecord) bool { return s.State == "stopped" })
	def.Tools = []string{pin.Name}
	def.Operator.Tools = def.Tools
	revision := state.ReviseSystemCommand{ExpectedRevision: 1, Configuration: &def, LocalTools: []state.ToolPin{nextPin}}
	system = daemonSystem(t, d, fixtureControlToken, "PUT", base+"/configuration", "assign", revision, 200)
	d.stop(t, false)
	d = startDaemonFixture(t, filename, fixtureControlToken)
	other := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "other", map[string]string{"launch": "research"}, 201)
	if status, _ := daemonRequest(t, d, fixtureControlToken, "PUT", "/v1/systems/"+other.ID+"/configuration", "cross", revision); status != 400 {
		t.Fatal("private learning artifact crossed systems")
	}
	goal := "reuse"
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/start", "reuse", state.StartSystemCommand{ExpectedRevision: system.Revision, Goal: &goal}, 202)
	result := waitSystem(t, d, system.ID, func(s state.SystemRecord) bool { return s.State == "inactive" })
	if result.Execution.Response != "correct" || count.Load() != 8 {
		t.Fatalf("approved skill not used: %+v %d", result.Execution, count.Load())
	}
	priorTask := result.Execution.TaskID
	system = daemonSystem(t, d, fixtureControlToken, "PUT", base+"/configuration", "rollback", state.ReviseSystemCommand{ExpectedRevision: system.Revision, Configuration: &def, LocalTools: []state.ToolPin{pin}}, 200)
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/start", "rolled-back", state.StartSystemCommand{ExpectedRevision: system.Revision, Goal: &goal}, 202)
	result = waitSystem(t, d, system.ID, func(s state.SystemRecord) bool { return s.State == "inactive" })
	if result.Execution.Response != "correct" || count.Load() != 9 {
		t.Fatalf("eligible rollback failed: %+v calls=%d", result.Execution, count.Load())
	}
	status, data = daemonRequest(t, d, fixtureControlToken, "GET", base+"/tasks", "", nil)
	if status != 200 {
		t.Fatal(string(data))
	}
	var tasks struct {
		Tasks []state.TaskRecord `json:"tasks"`
	}
	if err := json.Unmarshal(data, &tasks); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, task := range tasks.Tasks {
		if task.ID == priorTask {
			found = true
			if len(task.Tools) != 1 || task.Tools[0] != nextPin {
				t.Fatal("rollback rewrote historical task pins")
			}
		}
	}
	if !found {
		t.Fatal("historical task missing after rollback")
	}
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/start", "exhausted", state.StartSystemCommand{ExpectedRevision: system.Revision, Goal: &goal}, 202)
	result = waitSystem(t, d, system.ID, func(s state.SystemRecord) bool { return s.State == "inactive" })
	if count.Load() != 9 || !strings.Contains(result.Execution.Reason, "task-use") {
		t.Fatalf("approval-use limit bypass: %+v calls=%d", result.Execution, count.Load())
	}
}
