//go:build integration && linux

package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jplck/microoperator/internal/state"
)

func functionCompletion(w http.ResponseWriter, name, id string, args any) {
	arguments, _ := json.Marshal(args)
	data, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": nil,
			"tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]string{"name": state.WireToolName(name), "arguments": string(arguments)}}}}, "finish_reason": "tool_calls"}},
		"usage": map[string]int{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func TestPausedInputSurvivesRealDaemonRestart(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []state.ChatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		hasInput := 0
		for _, message := range request.Messages {
			if message.Content == "durable follow-up" {
				hasInput++
			}
		}
		if hasInput == 0 {
			entered <- struct{}{}
			select {
			case <-release:
				completion(w, "first turn")
			case <-r.Context().Done():
			}
		} else {
			if hasInput != 1 {
				t.Error("input was delivered more than once")
			}
			completion(w, "follow-up handled")
		}
	})
	q := cfg.QuotaGroups["account"]
	q.BurstRequests = 2
	cfg.QuotaGroups["account"] = q
	filename := filepath.Join(t.TempDir(), "daemon.json")
	writeFixtureConfiguration(t, filename, cfg)
	d := startDaemonFixture(t, filename, fixtureControlToken)
	record := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "create", map[string]string{"name": "research", "goal": "initial goal"}, 201)
	daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/start", "start", state.StartSystemCommand{ExpectedRevision: 1}, 202)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("initial model call not dispatched")
	}
	daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/pause", "pause", struct{}{}, 202)
	input := map[string]string{"content": "durable follow-up"}
	daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/input", "input", input, 202)
	close(release)
	waitSystem(t, d, record.ID, func(s state.SystemRecord) bool { return s.State == "paused" && s.Execution.State == "awaiting_worker" })
	d.stop(t, true)
	restarted := startDaemonFixture(t, filename, fixtureControlToken)
	paused := daemonSystem(t, restarted, fixtureControlToken, "GET", "/v1/systems/"+record.ID, "", nil, 200)
	if paused.State != "paused" || count.Load() != 1 {
		t.Fatal("restart resumed paused work")
	}
	daemonSystem(t, restarted, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/input", "input", input, 202)
	daemonSystem(t, restarted, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/resume", "resume", struct{}{}, 202)
	done := waitSystem(t, restarted, record.ID, func(s state.SystemRecord) bool { return s.State == "inactive" })
	if done.Execution.Response != "follow-up handled" || count.Load() != 2 || done.UsedTokens != 36 {
		t.Fatalf("accepted input lost/replayed: %+v count=%d", done.Execution, count.Load())
	}
	_, data := daemonRequest(t, restarted, fixtureControlToken, "GET", "/v1/systems/"+record.ID+"/events", "", nil)
	var events struct {
		Events []state.EventView `json:"events"`
	}
	if err := json.Unmarshal(data, &events); err != nil {
		t.Fatal(err)
	}
	inputs := 0
	for _, event := range events.Events {
		if event.Type == "user.input" {
			inputs++
			if event.State != "acked" {
				t.Fatalf("input has no delivery receipt: %+v", event)
			}
		}
	}
	if inputs != 1 {
		t.Fatalf("input deduplication failed: %d", inputs)
	}
}

func TestSharedToolAndSkillRequireSystemGrant(t *testing.T) {
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []state.ChatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		last := request.Messages[len(request.Messages)-1]
		if last.Role == "tool" {
			completion(w, "granted tool finished")
			return
		}
		if request.Messages[0].Content == "denied" {
			for _, message := range request.Messages {
				if message.Content == "read-only skill" {
					t.Error("ungranted skill exposed")
				}
			}
		} else if len(request.Messages) < 3 || request.Messages[1].Content != "read-only skill" {
			t.Error("granted skill not supplied")
		}
		functionCompletion(w, "runtime.text.analyze", "analyze", state.TextArguments{Text: "reviewed tool", Save: true})
	})
	cfg.Tools["shared.instructions"] = state.ToolConfig{Kind: "skill", Version: 1, Description: "Pinned instructions", Content: "read-only skill", RequiresTools: []string{"runtime.text.analyze"}}
	granted := *cfg.Bootstrap
	granted.Tools = []string{"runtime.text.analyze", "shared.instructions"}
	granted.Operator.Tools = granted.Tools
	cfg.Bootstrap = &granted
	denied := granted
	denied.Tools = []string{}
	denied.Operator.Tools = []string{}
	denied.Operator.Prompt = "denied"
	q := cfg.QuotaGroups["account"]
	q.BurstRequests = 10
	cfg.QuotaGroups["account"] = q
	filename := filepath.Join(t.TempDir(), "daemon.json")
	writeFixtureConfiguration(t, filename, cfg)
	d := startDaemonFixture(t, filename, fixtureControlToken)
	for _, name := range []string{"research", "denied"} {
		record := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "create-"+name, map[string]string{"name": name, "goal": "analyze"}, 201)
		if name == "denied" {
			denied.Name = name
			record = daemonSystem(t, d, fixtureControlToken, "PUT", "/v1/systems/"+record.ID+"/configuration", "deny",
				state.ReviseSystemCommand{ExpectedRevision: 1, Configuration: &denied}, 200)
		}
		daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/start", "start-"+name, state.StartSystemCommand{ExpectedRevision: record.Revision}, 202)
		done := waitSystem(t, d, record.ID, func(s state.SystemRecord) bool { return s.State == "inactive" })
		_, data := daemonRequest(t, d, fixtureControlToken, "GET", "/v1/systems/"+record.ID+"/artifacts", "", nil)
		var artifacts struct {
			Artifacts []json.RawMessage `json:"artifacts"`
		}
		if err := json.Unmarshal(data, &artifacts); err != nil {
			t.Fatal(err)
		}
		if name == "research" {
			if done.Execution.Response != "granted tool finished" || len(artifacts.Artifacts) != 1 {
				t.Fatalf("granted execution failed: %+v %s", done.Execution, data)
			}
		} else if done.Execution.TaskState != "failed" || len(artifacts.Artifacts) != 0 {
			t.Fatalf("ungranted execution accepted: %+v %s", done.Execution, data)
		}
	}
	if count.Load() != 3 {
		t.Fatalf("unexpected dispatches: %d", count.Load())
	}
	_, data := daemonRequest(t, d, fixtureControlToken, "GET", "/v1/tools/runtime.text.analyze", "", nil)
	var manifest state.RegistryEntry
	if err := json.Unmarshal(data, &manifest); err != nil || len(manifest.ExecutableDigest) != 64 {
		t.Fatalf("missing executable fingerprint: %s %v", data, err)
	}
}

func TestGoalStopCancelsWaitingTeamButNotOtherSystem(t *testing.T) {
	childEntered := make(chan struct{}, 1)
	cfg, _ := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []state.ChatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		switch request.Messages[0].Content {
		case "child":
			childEntered <- struct{}{}
			<-r.Context().Done()
		case "independent":
			completion(w, "other system finished")
		default:
			last := request.Messages[len(request.Messages)-1]
			if last.Role != "tool" {
				functionCompletion(w, "runtime.agent.propose", "propose", state.ProposeAgentArgs{Name: "child", Prompt: "child", Tools: []string{}, TokenBudget: 10000})
				return
			}
			var child struct {
				ID string `json:"agent_id"`
			}
			if err := json.Unmarshal([]byte(last.Content), &child); err != nil {
				t.Error(err)
				return
			}
			functionCompletion(w, "runtime.task.delegate", "delegate", state.DelegateArgs{AgentID: child.ID, Prompt: "wait"})
		}
	})
	def := *cfg.Bootstrap
	def.Tools = []string{"runtime.agent.propose", "runtime.task.delegate"}
	def.Operator.Tools = def.Tools
	def.Operator.Prompt = "operator"
	def.Limits.MaxActiveAgents = 1
	cfg.Bootstrap = &def
	other := def
	other.Operator.Prompt = "independent"
	q := cfg.QuotaGroups["account"]
	q.BurstRequests = 10
	q.MaxConcurrent = 2
	cfg.QuotaGroups["account"] = q
	filename := filepath.Join(t.TempDir(), "daemon.json")
	writeFixtureConfiguration(t, filename, cfg)
	d := startDaemonFixture(t, filename, fixtureControlToken)
	record := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "team", map[string]string{"name": "research", "goal": "delegate"}, 201)
	started := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/start", "start", state.StartSystemCommand{ExpectedRevision: 1}, 202)
	select {
	case <-childEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("child not dispatched")
	}
	second := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "other", map[string]string{"name": "independent", "goal": "finish"}, 201)
	other.Name = "independent"
	second = daemonSystem(t, d, fixtureControlToken, "PUT", "/v1/systems/"+second.ID+"/configuration", "other-config",
		state.ReviseSystemCommand{ExpectedRevision: 1, Configuration: &other}, 200)
	daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+second.ID+"/start", "other-start", state.StartSystemCommand{ExpectedRevision: second.Revision}, 202)
	stopping := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/goals/"+started.Execution.GoalID+"/stop", "stop", struct{}{}, 202)
	if stopping.State != "stopping" {
		t.Fatalf("goal stop did not wait for cleanup: %s", stopping.State)
	}
	waitSystem(t, d, record.ID, func(s state.SystemRecord) bool { return s.State == "stopped" })
	done := waitSystem(t, d, second.ID, func(s state.SystemRecord) bool { return s.State == "inactive" })
	if done.Execution.Response != "other system finished" {
		t.Fatal("goal stop crossed system scope")
	}
	_, data := daemonRequest(t, d, fixtureControlToken, "GET", "/v1/systems/"+record.ID+"/tasks", "", nil)
	var tasks struct {
		Tasks []state.TaskRecord `json:"tasks"`
	}
	if err := json.Unmarshal(data, &tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks.Tasks) != 2 {
		t.Fatalf("task history lost: %s", data)
	}
	for _, task := range tasks.Tasks {
		if task.State != "canceled" {
			t.Fatalf("uncanceled descendant: %+v", task)
		}
	}
	status, _ := daemonRequest(t, d, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/input", "late-input", map[string]string{"content": "too late"})
	if status != 409 {
		t.Fatalf("stopped system accepted input: %d", status)
	}
}

func TestTwoSystemsDelegateThroughDurableMailboxes(t *testing.T) {
	var mu sync.Mutex
	artifacts := map[string]string{}
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []state.ChatMessage   `json:"messages"`
			Tools    []state.ModelFunction `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		role := request.Messages[0].Content
		user := ""
		lastTool := ""
		lastFunction := ""
		for _, message := range request.Messages {
			if message.Role == "user" && user == "" {
				user = message.Content
			}
			if message.Role == "assistant" && len(message.ToolCalls) == 1 {
				lastFunction = message.ToolCalls[0].Function.Name
			}
			if message.Role == "tool" {
				lastTool = message.Content
			}
		}
		switch {
		case role == "operator" && lastTool == "":
			functionCompletion(w, "runtime.agent.propose", "propose", state.ProposeAgentArgs{Name: "researcher", Prompt: "child", Tools: []string{"runtime.text.analyze"}, TokenBudget: 20000})
		case role == "operator" && lastFunction == state.WireToolName("runtime.agent.propose"):
			var result struct {
				ID string `json:"agent_id"`
			}
			if err := json.Unmarshal([]byte(lastTool), &result); err != nil {
				t.Error(err)
				return
			}
			functionCompletion(w, "runtime.task.delegate", "delegate", state.DelegateArgs{AgentID: result.ID, Prompt: "child for " + user})
		case role == "child" && lastTool == "":
			if len(request.Tools) != 1 || request.Tools[0].Function.Name != state.WireToolName("runtime.text.analyze") {
				t.Error("child received broader capabilities")
			}
			functionCompletion(w, "runtime.text.analyze", "analyze", state.TextArguments{Text: user, Save: true})
		case role == "child":
			var result state.TextResult
			if err := json.Unmarshal([]byte(lastTool), &result); err != nil {
				t.Error(err)
				return
			}
			if !strings.HasPrefix(result.Artifact, "artifact_") {
				t.Error("subprocess report was not ingested")
			}
			mu.Lock()
			artifacts[strings.TrimPrefix(user, "child for ")] = result.Artifact
			mu.Unlock()
			completion(w, "child finished "+user)
		case role == "operator" && lastFunction == state.WireToolName("runtime.task.delegate"):
			if !strings.Contains(lastTool, "child finished child for "+user) {
				t.Errorf("wrong scoped child result: %s", lastTool)
			}
			completion(w, "operator finished "+user)
		default:
			t.Errorf("unexpected conversation: %+v", request.Messages)
			w.WriteHeader(400)
		}
	})
	def := *cfg.Bootstrap
	def.Tools = []string{"runtime.agent.propose", "runtime.task.delegate", "runtime.text.analyze"}
	def.Operator.Tools = append([]string{}, def.Tools...)
	def.Operator.Prompt = "operator"
	def.Limits.MaxActiveAgents = 1
	cfg.Bootstrap = &def
	q := cfg.QuotaGroups["account"]
	q.BurstRequests, q.RequestsPerMinute, q.TokensPerMinute = 20, 600, 1000000
	cfg.QuotaGroups["account"] = q
	filename := filepath.Join(t.TempDir(), "daemon.json")
	writeFixtureConfiguration(t, filename, cfg)
	d := startDaemonFixture(t, filename, fixtureControlToken)
	records := map[string]state.SystemRecord{}
	for _, name := range []string{"alpha", "beta"} {
		record := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "create-"+name, map[string]string{"name": "research", "goal": name}, 201)
		records[name] = record
		daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/start", "start-"+name, state.StartSystemCommand{ExpectedRevision: 1}, 202)
	}
	for name, record := range records {
		done := waitSystem(t, d, record.ID, func(s state.SystemRecord) bool { return s.State == "inactive" || s.State == "stopped" })
		if done.Execution.Response != "operator finished "+name || done.UsedTokens != 90 || done.ReservedTokens != 0 {
			_, tasks := daemonRequest(t, d, fixtureControlToken, "GET", "/v1/systems/"+record.ID+"/tasks", "", nil)
			d.stop(t, false)
			t.Fatalf("team failed: %+v; tasks=%s; diagnostics=%s", done.Execution, tasks, d.stderr.String())
		}
		_, data := daemonRequest(t, d, fixtureControlToken, "GET", "/v1/systems/"+record.ID+"/tasks", "", nil)
		var tasks struct {
			Tasks []state.TaskRecord `json:"tasks"`
		}
		if err := json.Unmarshal(data, &tasks); err != nil {
			t.Fatal(err)
		}
		if len(tasks.Tasks) != 2 {
			t.Fatalf("tasks: %s", data)
		}
		for _, task := range tasks.Tasks {
			if task.State != "completed" {
				t.Fatalf("unfinished task: %+v", task)
			}
		}
		_, data = daemonRequest(t, d, fixtureControlToken, "GET", "/v1/systems/"+record.ID+"/events", "", nil)
		var events struct {
			Events []state.EventView `json:"events"`
		}
		if err := json.Unmarshal(data, &events); err != nil {
			t.Fatal(err)
		}
		resultSeen := false
		for _, event := range events.Events {
			if event.SystemID != record.ID || event.Classification != "system-private" {
				t.Fatal("untrusted event envelope")
			}
			if event.Type == "task.result" {
				resultSeen = true
				if event.State != "acked" {
					t.Fatalf("result not acknowledged: %+v", event)
				}
			}
		}
		if !resultSeen {
			t.Fatal("delegation bypassed the mailbox")
		}
		mu.Lock()
		artifact := artifacts[name]
		mu.Unlock()
		status, _ := daemonRequest(t, d, fixtureControlToken, "GET", "/v1/systems/"+record.ID+"/artifacts/"+artifact, "", nil)
		if status != 200 {
			t.Fatalf("scoped artifact unavailable: %d", status)
		}
		other := "alpha"
		if name == other {
			other = "beta"
		}
		status, _ = daemonRequest(t, d, fixtureControlToken, "GET", fmt.Sprintf("/v1/systems/%s/artifacts/%s", records[other].ID, artifact), "", nil)
		if status != 404 {
			t.Fatal("artifact escaped its owning system")
		}
	}
	if count.Load() != 10 {
		t.Fatalf("duplicate or missing model dispatches: %d", count.Load())
	}
}
