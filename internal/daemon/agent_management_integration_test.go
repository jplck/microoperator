//go:build integration && linux

package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jplck/microoperator/internal/state"
)

func TestOperatorManagesAgentModelsAndLifecycle(t *testing.T) {
	var childCalls atomic.Int64
	childProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model    string                `json:"model"`
			Messages []state.ChatMessage   `json:"messages"`
			Tools    []state.ModelFunction `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) < 2 {
			t.Errorf("child request: %+v, %v", request, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		childCalls.Add(1)
		if r.Header.Get("Authorization") != "" || len(request.Tools) != 0 {
			t.Error("child inherited another provider's credentials or tools")
		}
		switch request.Model {
		case "fast-model":
			if request.Messages[0].Content != "first role" {
				t.Error("initial child revision not selected")
			}
			completion(w, "fast result")
		case "deep-model":
			if request.Messages[0].Content != "revised role" {
				t.Error("future task did not select revised prompt")
			}
			completion(w, "deep result")
		default:
			t.Errorf("unselected child model: %s", request.Model)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(childProvider.Close)
	var childID atomic.Value
	childID.Store("")
	cfg, rootCalls := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []state.ChatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		lastFunction, lastTool := "", ""
		for _, message := range request.Messages {
			if len(message.ToolCalls) == 1 {
				lastFunction = message.ToolCalls[0].Function.Name
			}
			if message.Role == "tool" {
				lastTool = message.Content
			}
		}
		switch lastFunction {
		case "":
			functionCompletion(w, "runtime.model.list", "models", struct{}{})
		case state.WireToolName("runtime.model.list"):
			if !strings.Contains(lastTool, `"name":"fast"`) || !strings.Contains(lastTool, `"name":"deep"`) ||
				strings.Contains(lastTool, childProvider.URL) {
				t.Errorf("model discovery missing aliases or exposed endpoint: %s", lastTool)
			}
			functionCompletion(w, "runtime.agent.propose", "create", state.ProposeAgentArgs{
				Name: "specialist", Prompt: "first role", Model: "fast", Tools: []string{}, TokenBudget: 30000,
			})
		case state.WireToolName("runtime.agent.propose"), state.WireToolName("runtime.agent.revise"):
			var agent struct {
				ID string `json:"agent_id"`
			}
			if err := json.Unmarshal([]byte(lastTool), &agent); err != nil || agent.ID == "" {
				t.Errorf("agent receipt: %s, %v", lastTool, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			childID.Store(agent.ID)
			functionCompletion(w, "runtime.task.delegate", "delegate", state.DelegateArgs{AgentID: agent.ID, Prompt: "Return your scoped result."})
		case state.WireToolName("runtime.task.delegate"):
			switch {
			case strings.Contains(lastTool, "fast result"):
				functionCompletion(w, "runtime.agent.revise", "revise", state.ReviseAgentArgs{
					AgentID: childID.Load().(string), ExpectedRevision: 1, Prompt: "revised role", Model: "deep", Tools: []string{},
				})
			case strings.Contains(lastTool, "deep result"):
				functionCompletion(w, "runtime.agent.retire", "retire", state.RetireAgentArgs{
					AgentID: childID.Load().(string), ExpectedRevision: 2,
				})
			default:
				t.Errorf("delegation did not return selected model result: %s", lastTool)
				w.WriteHeader(http.StatusBadRequest)
			}
		case state.WireToolName("runtime.agent.retire"):
			functionCompletion(w, "runtime.agent.list", "inspect", struct{}{})
		case state.WireToolName("runtime.agent.list"):
			if !strings.Contains(lastTool, `"state":"stopped"`) || !strings.Contains(lastTool, `"revision":2`) {
				t.Errorf("retirement or revision missing: %s", lastTool)
			}
			completion(w, "Team reorganized within its grants.")
		default:
			t.Errorf("unexpected operator decision: %s", lastFunction)
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	cfg.Providers["children"] = state.ProviderConfig{Adapter: "ollama", BaseURL: childProvider.URL + "/v1"}
	cfg.Models["fast"] = state.ModelConfig{Provider: "children", Model: "fast-model", QuotaGroups: []string{"account"}, MaxOutputTokens: 1024}
	cfg.Models["deep"] = state.ModelConfig{Provider: "children", Model: "deep-model", QuotaGroups: []string{"account"}, MaxOutputTokens: 1024}
	def := *cfg.Bootstrap
	def.Tools = []string{"runtime.model.list", "runtime.agent.propose", "runtime.agent.revise", "runtime.agent.retire", "runtime.agent.list", "runtime.task.delegate"}
	def.Operator.Tools = append([]string{}, def.Tools...)
	def.Operator.Prompt = "operator"
	def.Limits.MaxActiveAgents = 1
	cfg.Bootstrap = &def
	q := cfg.QuotaGroups["account"]
	q.BurstRequests, q.RequestsPerMinute, q.TokensPerMinute = 60, 600, 1000000
	cfg.QuotaGroups["account"] = q
	filename := filepath.Join(t.TempDir(), "daemon.json")
	writeFixtureConfiguration(t, filename, cfg)
	d := startDaemonFixture(t, filename, fixtureControlToken)
	record := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "create",
		state.CreateSystemCommand{Name: "Managed team", Goal: fixtureGoal()}, http.StatusCreated)
	base := "/v1/systems/" + record.ID
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/start", "start", state.StartSystemCommand{ExpectedRevision: 1}, http.StatusAccepted)
	done := waitSystem(t, d, record.ID, func(s state.SystemRecord) bool { return s.State == "inactive" })
	if done.Execution.Response != "Team reorganized within its grants." || rootCalls.Load() != 8 || childCalls.Load() != 2 ||
		done.UsedTokens != 180 || done.ReservedTokens != 0 {
		t.Fatalf("managed team failed: %+v, root=%d child=%d", done, rootCalls.Load(), childCalls.Load())
	}
	status, data := daemonRequest(t, d, fixtureControlToken, "GET", base+"/tasks", "", nil)
	var tasks struct {
		Tasks []state.TaskRecord `json:"tasks"`
	}
	if err := json.Unmarshal(data, &tasks); err != nil || status != http.StatusOK || len(tasks.Tasks) != 3 {
		t.Fatalf("tasks: %s, %v", data, err)
	}
	revisions := map[int64]bool{}
	for _, task := range tasks.Tasks {
		if task.State != "completed" {
			t.Fatalf("unfinished task: %+v", task)
		}
		if task.AgentID == childID.Load().(string) {
			revisions[task.Revision] = true
		}
	}
	if !revisions[1] || !revisions[2] {
		t.Fatalf("historical task pins rewritten: %+v", tasks.Tasks)
	}
	d.stop(t, false)
	restarted := startDaemonFixture(t, filename, fixtureControlToken)
	saved := daemonSystem(t, restarted, fixtureControlToken, "GET", base, "", nil, http.StatusOK)
	if saved.UsedTokens != done.UsedTokens || rootCalls.Load() != 8 || childCalls.Load() != 2 {
		t.Fatal("restart replayed managed work or reset accounting")
	}
}
