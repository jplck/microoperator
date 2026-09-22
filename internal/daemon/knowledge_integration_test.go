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

func TestAgentMemorySurvivesRealRestart(t *testing.T) {
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []state.ChatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		goal := request.Messages[1].Content
		last := request.Messages[len(request.Messages)-1]
		switch {
		case goal == "store" && last.Role == "user":
			functionCompletion(w, "runtime.memory.put", "remember", state.MemoryCommand{Scope: "agent", Content: "blue-tag durable fact", Evidence: "fixture observation", Confidence: 90, Artifacts: []string{}, RetentionSeconds: 3600})
		case goal == "store":
			completion(w, "stored")
		case goal == "recall" && last.Role == "user":
			functionCompletion(w, "runtime.memory.search", "recall", map[string]string{"scope": "agent", "query": "blue-tag"})
		case goal == "recall":
			if !strings.Contains(last.Content, "blue-tag durable fact") || !strings.Contains(last.Content, `"untrusted_data":true`) {
				t.Errorf("memory missing or promoted to authority: %s", last.Content)
			}
			completion(w, "recalled")
		default:
			t.Errorf("unexpected memory conversation: %+v", request.Messages)
			w.WriteHeader(400)
		}
	})
	def := *cfg.Bootstrap
	def.Tools = []string{"runtime.memory.put", "runtime.memory.search"}
	def.Operator.Tools = def.Tools
	cfg.Bootstrap = &def
	q := cfg.QuotaGroups["account"]
	q.BurstRequests = 10
	q.TokensPerMinute = 1000000
	cfg.QuotaGroups["account"] = q
	filename := filepath.Join(t.TempDir(), "daemon.json")
	writeFixtureConfiguration(t, filename, cfg)
	d := startDaemonFixture(t, filename, fixtureControlToken)
	record := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "create", map[string]string{"name": "research", "goal": "store"}, 201)
	daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/start", "start", state.StartSystemCommand{ExpectedRevision: 1}, 202)
	done := waitSystem(t, d, record.ID, func(s state.SystemRecord) bool { return s.State == "inactive" })
	if done.Execution.Response != "stored" {
		d.stop(t, false)
		t.Fatalf("store failed: %+v %s", done.Execution, d.stderr.String())
	}
	d.stop(t, true)
	restarted := startDaemonFixture(t, filename, fixtureControlToken)
	goal := "recall"
	daemonSystem(t, restarted, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/start", "recall", state.StartSystemCommand{ExpectedRevision: 1, Goal: &goal}, 202)
	done = waitSystem(t, restarted, record.ID, func(s state.SystemRecord) bool { return s.State == "inactive" })
	if done.Execution.Response != "recalled" || done.UsedTokens != 72 || count.Load() != 4 {
		t.Fatalf("memory replay/accounting: %+v calls=%d", done.Execution, count.Load())
	}
}

func TestPausedTimerSurvivesRealRestart(t *testing.T) {
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []state.ChatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		seen := 0
		for _, message := range request.Messages {
			if strings.Contains(message.Content, "timer-payload") {
				seen++
			}
		}
		if seen == 0 {
			functionCompletion(w, "runtime.task.wait", "wait", map[string]string{"reason": "waiting for scheduled context"})
			return
		}
		if seen != 1 {
			t.Errorf("timer data applied %d times", seen)
		}
		completion(w, "timer handled")
	})
	def := *cfg.Bootstrap
	def.Tools = []string{"runtime.task.wait"}
	def.Operator.Tools = def.Tools
	cfg.Bootstrap = &def
	q := cfg.QuotaGroups["account"]
	q.BurstRequests = 5
	cfg.QuotaGroups["account"] = q
	filename := filepath.Join(t.TempDir(), "daemon.json")
	writeFixtureConfiguration(t, filename, cfg)
	d := startDaemonFixture(t, filename, fixtureControlToken)
	record := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "create", map[string]string{"name": "research", "goal": "wait"}, 201)
	daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/start", "start", state.StartSystemCommand{ExpectedRevision: 1, LifetimeSeconds: 3600}, 202)
	waitSystem(t, d, record.ID, func(s state.SystemRecord) bool { return s.Execution.TaskState == "waiting" })
	daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/pause", "pause", struct{}{}, 202)
	due := time.Now().Add(250 * time.Millisecond)
	command := state.ScheduleCommand{At: due.Format(time.RFC3339Nano), Content: "timer-payload", Budget: 1}
	daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/schedules", "timer", command, 201)
	d.stop(t, true)
	restarted := startDaemonFixture(t, filename, fixtureControlToken)
	if delay := time.Until(due); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		<-timer.C
	}
	paused := daemonSystem(t, restarted, fixtureControlToken, "GET", "/v1/systems/"+record.ID, "", nil, 200)
	if paused.State != "paused" || count.Load() != 1 {
		t.Fatal("restart or due timer resumed paused work")
	}
	daemonSystem(t, restarted, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/schedules", "timer", command, 201)
	daemonSystem(t, restarted, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/resume", "resume", struct{}{}, 202)
	done := waitSystem(t, restarted, record.ID, func(s state.SystemRecord) bool { return s.State == "inactive" })
	if done.Execution.Response != "timer handled" || count.Load() != 2 || done.UsedTokens != 36 {
		restarted.stop(t, false)
		t.Fatalf("durable timer failed: %+v calls=%d %s", done.Execution, count.Load(), restarted.stderr.String())
	}
	_, data := daemonRequest(t, restarted, fixtureControlToken, "GET", "/v1/systems/"+record.ID+"/events", "", nil)
	var events struct {
		Events []state.EventView `json:"events"`
	}
	if err := json.Unmarshal(data, &events); err != nil {
		t.Fatal(err)
	}
	fired := 0
	for _, event := range events.Events {
		if event.Type == "schedule.fired" {
			fired++
			if event.State != "acked" || event.CreatedAt < due.UnixMilli() {
				t.Fatalf("invalid timer receipt: %+v", event)
			}
		}
	}
	if fired != 1 {
		t.Fatalf("missing or duplicated occurrence: %d", fired)
	}
	restarted.stop(t, true)
	again := startDaemonFixture(t, filename, fixtureControlToken)
	saved := daemonSystem(t, again, fixtureControlToken, "GET", "/v1/systems/"+record.ID, "", nil, 200)
	if saved.State != "inactive" || count.Load() != 2 {
		t.Fatal("completed timer reactivated on restart")
	}
}
