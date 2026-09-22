//go:build integration && linux

package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jplck/microoperator/internal/state"
)

func TestContinuousAssistantInputRestartAndStop(t *testing.T) {
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []state.ChatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) == 0 {
			t.Errorf("continuous request: %+v %v", request, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		last := request.Messages[len(request.Messages)-1]
		switch {
		case last.Content == "Keep helping":
			functionCompletion(w, "runtime.agent.propose", "specialist", state.ProposeAgentArgs{
				Name: "researcher", Prompt: "Research narrowly", Tools: []string{}, TokenBudget: 20000,
			})
		case last.Content == "Wait for me":
			functionCompletion(w, "runtime.task.wait", "wait", map[string]string{"reason": "Ready for more input"})
		case last.Role == "tool":
			completion(w, "Team ready")
		default:
			completion(w, "Handled: "+last.Content)
		}
	})
	def := *cfg.Bootstrap
	def.Tools = []string{"runtime.agent.propose", "runtime.task.wait"}
	def.Operator.Tools = def.Tools
	cfg.Bootstrap = &def
	q := cfg.QuotaGroups["account"]
	q.BurstRequests, q.RequestsPerMinute, q.TokensPerMinute = 60, 600, 1000000
	cfg.QuotaGroups["account"] = q
	filename := filepath.Join(t.TempDir(), "daemon.json")
	writeFixtureConfiguration(t, filename, cfg)
	d := startDaemonFixture(t, filename, fixtureControlToken)
	s := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "create",
		map[string]string{"name": "Assistant", "goal": "Keep helping"}, http.StatusCreated)
	base := "/v1/systems/" + s.ID
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/start", "start",
		state.StartSystemCommand{ExpectedRevision: 1, Continuous: true, LifetimeSeconds: 60}, http.StatusAccepted)
	idle := waitSystem(t, d, s.ID, func(s state.SystemRecord) bool { return s.Idle })
	goalID := idle.Execution.GoalID
	if idle.State != "running" || !idle.Continuous || idle.Execution.Response != "Team ready" ||
		idle.UsedTokens != 36 || idle.ReservedTokens != 0 {
		t.Fatalf("final response closed the assistant: %+v", idle)
	}
	status, agents := daemonRequest(t, d, fixtureControlToken, "GET", base+"/agents", "", nil)
	if status != http.StatusOK || !strings.Contains(string(agents), `"name":"researcher"`) ||
		strings.Contains(string(agents), `"state":"stopped"`) {
		t.Fatalf("idle team not retained: %s", agents)
	}
	for i := 0; i < 9; i++ {
		input := fmt.Sprintf("Follow-up %d", i)
		daemonSystem(t, d, fixtureControlToken, "POST", base+"/input", fmt.Sprintf("input-%d", i),
			state.AddInputCommand{Content: input}, http.StatusAccepted)
		idle = waitSystem(t, d, s.ID, func(s state.SystemRecord) bool {
			return s.Idle && s.Execution.Response == "Handled: "+input
		})
		if idle.Execution.GoalID != goalID || idle.State != "running" {
			t.Fatal("follow-up replaced or completed continuous goal")
		}
	}
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/input", "wait",
		state.AddInputCommand{Content: "Wait for me"}, http.StatusAccepted)
	waitSystem(t, d, s.ID, func(s state.SystemRecord) bool { return s.Idle })
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/pause", "pause", struct{}{}, http.StatusAccepted)
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/input", "paused-input",
		state.AddInputCommand{Content: "After restart"}, http.StatusAccepted)
	before := count.Load()
	d.stop(t, true)
	d = startDaemonFixture(t, filename, fixtureControlToken)
	paused := daemonSystem(t, d, fixtureControlToken, "GET", base, "", nil, http.StatusOK)
	if paused.State != "paused" || count.Load() != before || paused.Execution.GoalID != goalID {
		t.Fatal("restart lost or activated paused continuous work")
	}
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/input", "paused-input",
		state.AddInputCommand{Content: "After restart"}, http.StatusAccepted)
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/resume", "resume", struct{}{}, http.StatusAccepted)
	idle = waitSystem(t, d, s.ID, func(s state.SystemRecord) bool {
		return s.Idle && s.Execution.Response == "Handled: After restart"
	})
	if count.Load() != before+1 || idle.UsedTokens != count.Load()*18 || idle.ReservedTokens != 0 {
		t.Fatalf("input replay or accounting reset: %+v calls=%d", idle, count.Load())
	}
	other := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "other",
		map[string]string{"name": "Other", "goal": "Independent assistant"}, http.StatusCreated)
	daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+other.ID+"/start", "other-start",
		state.StartSystemCommand{ExpectedRevision: 1, Continuous: true}, http.StatusAccepted)
	waitSystem(t, d, other.ID, func(s state.SystemRecord) bool { return s.Idle })
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/stop", "stop", struct{}{}, http.StatusAccepted)
	waitSystem(t, d, s.ID, func(s state.SystemRecord) bool { return s.State == "stopped" })
	before = count.Load()
	d.stop(t, false)
	d = startDaemonFixture(t, filename, fixtureControlToken)
	stopped := daemonSystem(t, d, fixtureControlToken, "GET", base, "", nil, http.StatusOK)
	other = daemonSystem(t, d, fixtureControlToken, "GET", "/v1/systems/"+other.ID, "", nil, http.StatusOK)
	if stopped.State != "stopped" || !other.Idle || other.State != "running" || count.Load() != before {
		t.Fatal("idle stop/recovery restarted work or affected another system")
	}
}

func TestContinuousTimerOutlivesCompletedTask(t *testing.T) {
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) {
		completion(w, "Ready")
	})
	filename := filepath.Join(t.TempDir(), "daemon.json")
	writeFixtureConfiguration(t, filename, cfg)
	d := startDaemonFixture(t, filename, fixtureControlToken)
	s := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "create",
		map[string]string{"name": "Timer", "goal": "Keep watching"}, http.StatusCreated)
	base := "/v1/systems/" + s.ID
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/start", "start",
		state.StartSystemCommand{ExpectedRevision: 1, Continuous: true, LifetimeSeconds: 5}, http.StatusAccepted)
	idle := waitSystem(t, d, s.ID, func(s state.SystemRecord) bool { return s.Idle })
	first := *idle.Execution
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/pause", "pause", struct{}{}, http.StatusAccepted)
	due := time.UnixMilli(first.Deadline).Add(time.Second)
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/schedules", "timer",
		state.ScheduleCommand{At: due.Format(time.RFC3339Nano), Content: "Check again", Budget: 1}, http.StatusCreated)
	d.stop(t, true)
	d = startDaemonFixture(t, filename, fixtureControlToken)
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/resume", "resume", struct{}{}, http.StatusAccepted)
	idle = waitSystemFor(t, d, s.ID, 12*time.Second, func(s state.SystemRecord) bool {
		return s.Idle && s.Execution.TaskID != first.TaskID
	})
	if idle.State != "running" || idle.Execution.GoalID != first.GoalID || count.Load() != 2 ||
		idle.Execution.CreatedAt < due.UnixMilli() || idle.Execution.Deadline <= first.Deadline ||
		idle.UsedTokens != 36 || idle.ReservedTokens != 0 {
		t.Fatalf("durable continuous timer: %+v calls=%d", idle, count.Load())
	}
}
