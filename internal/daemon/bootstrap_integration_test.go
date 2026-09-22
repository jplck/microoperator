//go:build integration && linux

package daemon

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jplck/microoperator/internal/state"
)

func TestGoalFirstBootstrapDelegatesProposesAndRecoversBlockedWork(t *testing.T) {
	const constraints = "Only simulated inputs; no live orders."
	const childPrompt = "Design a deterministic fixture simulator."
	const blocker = "Need a qualified build profile, pinned toolchain and human-owned protected checks; no tool executed."
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []state.ChatMessage   `json:"messages"`
			Tools    []state.ModelFunction `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		hasConstraints := false
		lastFunction, lastTool := "", ""
		for _, message := range request.Messages {
			if message.Role == "system" && strings.Contains(message.Content, constraints) {
				hasConstraints = true
			}
			if len(message.ToolCalls) == 1 {
				lastFunction = message.ToolCalls[0].Function.Name
			}
			if message.Role == "tool" {
				lastTool = message.Content
			}
		}
		if !hasConstraints {
			t.Error("root or delegated call lost system constraints")
		}
		if request.Messages[0].Content == childPrompt {
			if len(request.Tools) != 0 {
				t.Error("child received capabilities it was not granted")
			}
			completion(w, "Propose a pure Process function; a design is not an executed simulation.")
			return
		}
		if len(request.Tools) != 12 || !strings.Contains(request.Messages[0].Content, "not a predefined domain workflow") {
			t.Error("system did not receive the general-purpose bootstrap")
		}
		switch lastFunction {
		case "":
			functionCompletion(w, "runtime.capabilities", "inspect", struct{}{})
		case state.WireToolName("runtime.capabilities"):
			var caps struct {
				Build   bool   `json:"generated_build_configured"`
				Blocker string `json:"generated_build_blocker"`
				Approve bool   `json:"exact_artifact_approval_required"`
			}
			if err := json.Unmarshal([]byte(lastTool), &caps); err != nil || caps.Build || caps.Blocker == "" || !caps.Approve {
				t.Errorf("build prerequisites hidden: %s %v", lastTool, err)
			}
			functionCompletion(w, "runtime.artifact.put", "plan", map[string]string{"content": "Plan: deterministic prices, explicit state, protected accounting checks."})
		case state.WireToolName("runtime.artifact.put"):
			var result struct {
				ID string `json:"artifact_id"`
			}
			if err := json.Unmarshal([]byte(lastTool), &result); err != nil || result.ID == "" {
				t.Errorf("artifact not stored: %s %v", lastTool, err)
			}
			functionCompletion(w, "runtime.artifact.get", "read-plan", map[string]string{"artifact_id": result.ID})
		case state.WireToolName("runtime.artifact.get"):
			if !strings.Contains(lastTool, "protected accounting checks") {
				t.Error("durable plan not returned")
			}
			functionCompletion(w, "runtime.agent.propose", "specialist", state.ProposeAgentArgs{
				Name: "simulator-designer", Prompt: childPrompt, Tools: []string{}, TokenBudget: 10000,
			})
		case state.WireToolName("runtime.agent.propose"):
			var child struct {
				ID string `json:"agent_id"`
			}
			if err := json.Unmarshal([]byte(lastTool), &child); err != nil || child.ID == "" {
				t.Errorf("child not proposed: %s %v", lastTool, err)
			}
			functionCompletion(w, "runtime.task.delegate", "delegate", state.DelegateArgs{AgentID: child.ID, Prompt: "Design; do not claim execution."})
		case state.WireToolName("runtime.task.delegate"):
			if !strings.Contains(lastTool, "a design is not an executed simulation") {
				t.Error("delegation result not delivered")
			}
			functionCompletion(w, "runtime.tool.propose", "draft", state.DraftCommand{
				Kind: "executable", Description: "Inert fixture for explicit-state simulation",
				Content:  "package main\nfunc Process(input string) (string, error) { return input, nil }\n",
				Requires: []string{},
			})
		case state.WireToolName("runtime.tool.propose"):
			if !strings.Contains(lastTool, "tool_id") {
				t.Error("missing inert proposal receipt")
			}
			functionCompletion(w, "runtime.task.wait", "blocked", map[string]string{"reason": blocker})
		case state.WireToolName("runtime.task.wait"):
			if request.Messages[len(request.Messages)-1].Content != "Keep the design only." {
				t.Error("waiting task woke without durable user input")
			}
			completion(w, "Design retained; no simulation, trades or tool execution claimed.")
		default:
			t.Errorf("unexpected bootstrap continuation: %s", lastFunction)
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	cfg.Bootstrap = &state.SystemConfig{Limits: state.SystemLimits{MaxAgents: 8, MaxActiveAgents: 1, TokenBudget: 100000}}
	q := cfg.QuotaGroups["account"]
	q.BurstRequests, q.RequestsPerMinute, q.TokensPerMinute = 60, 600, 1000000
	cfg.QuotaGroups["account"] = q
	filename := filepath.Join(t.TempDir(), "daemon.json")
	writeFixtureConfiguration(t, filename, cfg)
	d := startDaemonFixture(t, filename, fixtureControlToken)
	goal := "Build a paper-market experiment from scratch."
	command := state.CreateSystemCommand{Name: "Paper market", Goal: &goal, Constraints: constraints, TokenBudget: 90000}
	record := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "create", command, 201)
	base := "/v1/systems/" + record.ID
	if count.Load() != 0 || record.Configuration.Name != command.Name || record.State != "inactive" {
		t.Fatal("creation ran work or lost the display name")
	}
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/start", "start", state.StartSystemCommand{ExpectedRevision: 1, LifetimeSeconds: 600}, 202)
	waitSystemFor(t, d, record.ID, 45*time.Second, func(s state.SystemRecord) bool {
		return s.Execution != nil && s.Execution.TaskState == "waiting" && count.Load() == 8
	})
	status, data := daemonRequest(t, d, fixtureControlToken, "GET", base+"/tasks", "", nil)
	var tasks struct {
		Tasks []state.TaskRecord `json:"tasks"`
	}
	if err := json.Unmarshal(data, &tasks); err != nil || status != 200 || len(tasks.Tasks) != 2 {
		t.Fatalf("missing root/child tasks: %s %v", data, err)
	}
	waiting := false
	for _, task := range tasks.Tasks {
		if task.Parent == "" {
			waiting = task.State == "waiting" && task.Reason == blocker
		}
	}
	if !waiting {
		t.Fatalf("missing visible prerequisite blocker: %s", data)
	}
	for _, section := range []string{"artifacts", "tools", "learning"} {
		status, data = daemonRequest(t, d, fixtureControlToken, "GET", base+"/"+section, "", nil)
		if status != 200 {
			t.Fatalf("inspect %s: %s", section, data)
		}
		if section == "tools" && (!strings.Contains(string(data), "Inert fixture") || !strings.Contains(string(data), `"state":"draft"`)) {
			t.Fatalf("proposal promoted or lost: %s", data)
		}
		if section == "learning" && strings.Contains(string(data), "evaluation_id") {
			t.Fatalf("missing prerequisites bypassed: %s", data)
		}
	}
	d.stop(t, true)
	restarted := startDaemonFixture(t, filename, fixtureControlToken)
	replay := daemonSystem(t, restarted, fixtureControlToken, "POST", "/v1/systems", "create", command, 201)
	if replay.ID != record.ID || replay.Configuration.Constraints != constraints || count.Load() != 8 {
		t.Fatal("restart changed creation or dispatched waiting work")
	}
	input := map[string]string{"content": "Keep the design only."}
	for i := 0; i < 2; i++ {
		daemonSystem(t, restarted, fixtureControlToken, "POST", base+"/input", "input", input, 202)
	}
	done := waitSystem(t, restarted, record.ID, func(s state.SystemRecord) bool { return s.State == "inactive" })
	if done.Execution.Response != "Design retained; no simulation, trades or tool execution claimed." ||
		count.Load() != 9 || done.UsedTokens != 162 || done.ReservedTokens != 0 {
		t.Fatalf("recovery/accounting failed: %+v calls=%d used=%d", done.Execution, count.Load(), done.UsedTokens)
	}
}
