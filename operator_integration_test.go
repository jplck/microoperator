//go:build integration && (darwin || linux)

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func waitSystem(t *testing.T, d *daemonFixture, id string, predicate func(systemRecord) bool) systemRecord {
	t.Helper()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	var record systemRecord
	for {
		record = daemonSystem(t, d, fixtureControlToken, "GET", "/v1/systems/"+id, "", nil, http.StatusOK)
		if predicate(record) {
			return record
		}
		select {
		case <-deadline.C:
			t.Fatalf("system did not reach expected state: %+v", record.Execution)
		case <-tick.C:
		}
	}
}

func receiveProvider(t *testing.T, requests <-chan string) string {
	t.Helper()
	select {
	case prompt := <-requests:
		return prompt
	case <-time.After(5 * time.Second):
		t.Fatal("provider did not receive the admitted request")
		return ""
	}
}

func streamCompletion(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": text}, "finish_reason": "stop"}}})
	fmt.Fprintf(w, "data: %s\n\n", chunk)
	io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\ndata: [DONE]\n\n")
}

func TestOperatorModelsStopAndIsolation(t *testing.T) {
	requests := make(chan string, 8)
	releaseAlpha := make(chan struct{})
	releaseGamma := make(chan struct{})
	betaClosed := make(chan struct{})
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []chatMessage `json:"messages"`
			Stream   bool          `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if len(request.Messages) != 3 || request.Messages[1].Content != "Use sources, not guesses." {
			t.Error("pinned skill instructions missing")
		}
		prompt := request.Messages[len(request.Messages)-1].Content
		requests <- prompt
		switch prompt {
		case "alpha":
			select {
			case <-releaseAlpha:
				completion(w, "alpha result")
			case <-r.Context().Done():
			}
		case "beta":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			close(betaClosed)
		case "gamma":
			select {
			case <-releaseGamma:
				streamCompletion(w, "gamma result")
			case <-r.Context().Done():
			}
		default:
			t.Error("unexpected goal")
			w.WriteHeader(400)
		}
	})
	q := cfg.QuotaGroups["account"]
	q.BurstRequests = 4
	cfg.QuotaGroups["account"] = q
	filename := filepath.Join(t.TempDir(), "daemon.json")
	writeFixtureConfiguration(t, filename, cfg)
	d := startDaemonFixture(t, filename, fixtureControlToken)
	create := func(name string) systemRecord {
		return daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "create-"+name,
			map[string]any{"launch": "research", "goal": name}, http.StatusCreated)
	}
	a, b := create("alpha"), create("beta")
	startA := startSystemCommand{ExpectedRevision: 1}
	daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+a.ID+"/start", "start-alpha", startA, http.StatusAccepted)
	daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+b.ID+"/start", "start-beta",
		startSystemCommand{ExpectedRevision: 1, Stream: true}, http.StatusAccepted)
	seen := map[string]bool{receiveProvider(t, requests): true, receiveProvider(t, requests): true}
	if !seen["alpha"] || !seen["beta"] {
		t.Fatalf("cross-system prompt confusion: %v", seen)
	}
	daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+a.ID+"/start", "start-alpha", startA, http.StatusAccepted)
	status, _ := daemonRequest(t, d, fixtureControlToken, "POST", "/v1/systems/"+a.ID+"/start", "duplicate-active", startA)
	if status != http.StatusConflict {
		t.Fatalf("second activation accepted: %d", status)
	}
	status, _ = daemonRequest(t, d, fixtureControlToken, "PUT", "/v1/systems/"+a.ID+"/configuration", "revise-active",
		reviseSystemCommand{ExpectedRevision: 1, Configuration: &a.Configuration})
	if status != http.StatusConflict {
		t.Fatalf("active grant revision accepted: %d", status)
	}
	status, data := daemonRequest(t, d, fixtureControlToken, "GET", "/v1/model-broker", "", nil)
	var metrics struct {
		Quotas []quotaView `json:"quota_groups"`
	}
	if err := json.Unmarshal(data, &metrics); err != nil || status != 200 || len(metrics.Quotas) != 1 || metrics.Quotas[0].InFlight != 2 {
		t.Fatalf("live broker accounting: %s %v", data, err)
	}
	close(releaseAlpha)
	doneA := waitSystem(t, d, a.ID, func(s systemRecord) bool { return s.State == "inactive" })
	if doneA.Execution == nil || doneA.Execution.TaskState != "completed" || doneA.Execution.Response != "alpha result" ||
		doneA.UsedTokens != 18 || doneA.ReservedTokens != 0 {
		t.Fatalf("operator result not committed: %+v", doneA)
	}
	stopping := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+b.ID+"/stop", "stop-beta", struct{}{}, http.StatusAccepted)
	if stopping.State != "stopping" {
		t.Fatalf("stop reported completed cleanup too early: %s", stopping.State)
	}
	stopped := waitSystem(t, d, b.ID, func(s systemRecord) bool { return s.State == "stopped" })
	if stopped.Execution.TaskState != "canceled" || stopped.Execution.State != "unknown" ||
		stopped.ReservedTokens != stopped.Execution.Reservation || stopped.UsedTokens != 0 {
		t.Fatalf("stream cancellation refunded ambiguity: %+v", stopped)
	}
	select {
	case <-betaClosed:
	case <-time.After(time.Second):
		t.Fatal("stream still open after stop")
	}
	if count.Load() != 2 {
		t.Fatalf("duplicate or unintended dispatch: %d", count.Load())
	}
	status, _ = daemonRequest(t, d, fixtureControlToken, "POST", "/v1/systems/"+b.ID+"/start", "no-new-goal", startA)
	if status != http.StatusBadRequest {
		t.Fatalf("canceled goal replayed: %d", status)
	}
	prompt := "gamma"
	daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+b.ID+"/start", "gamma",
		startSystemCommand{ExpectedRevision: 1, Goal: &prompt, Stream: true}, http.StatusAccepted)
	if receiveProvider(t, requests) != "gamma" {
		t.Fatal("new goal did not reach provider")
	}
	daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+b.ID+"/stop", "stop-beta", struct{}{}, http.StatusAccepted)
	close(releaseGamma)
	gamma := waitSystem(t, d, b.ID, func(s systemRecord) bool { return s.State == "inactive" })
	if gamma.Execution.Response != "gamma result" || gamma.UsedTokens != 18 || gamma.ReservedTokens != stopped.ReservedTokens ||
		gamma.Execution.GoalID == stopped.Execution.GoalID {
		t.Fatalf("restart reset lifetime usage or reused goal: %+v", gamma)
	}
	stillA := daemonSystem(t, d, fixtureControlToken, "GET", "/v1/systems/"+a.ID, "", nil, http.StatusOK)
	if stillA.Execution.Response != "alpha result" || stillA.UsedTokens != 18 {
		t.Fatal("stopping another system changed completed work")
	}
	_, history := daemonRequest(t, d, fixtureControlToken, "GET", "/v1/systems/"+b.ID+"/model-calls", "", nil)
	var calls struct {
		Calls []executionRecord `json:"calls"`
	}
	if err := json.Unmarshal(history, &calls); err != nil || len(calls.Calls) != 2 ||
		calls.Calls[0].CallID != stopped.Execution.CallID || calls.Calls[1].CallID != gamma.Execution.CallID {
		t.Fatalf("durable call history: %s %v", history, err)
	}
	d.stop(t, false)
	restarted := startDaemonFixture(t, filename, fixtureControlToken)
	saved := daemonSystem(t, restarted, fixtureControlToken, "GET", "/v1/systems/"+b.ID, "", nil, http.StatusOK)
	if saved.Execution.Response != "gamma result" || saved.ReservedTokens != gamma.ReservedTokens || saved.UsedTokens != 18 || count.Load() != 3 {
		t.Fatal("successful work or reservations changed across graceful restart")
	}
}

func TestOperatorCrashDoesNotReplayDispatch(t *testing.T) {
	requests := make(chan string, 2)
	closed := make(chan struct{})
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"incomplete\"},\"finish_reason\":null}]}\n\n")
		w.(http.Flusher).Flush()
		requests <- "dispatched"
		<-r.Context().Done()
		close(closed)
	})
	q := cfg.QuotaGroups["account"]
	q.RequestsPerMinute = 1
	cfg.QuotaGroups["account"] = q
	filename := filepath.Join(t.TempDir(), "daemon.json")
	writeFixtureConfiguration(t, filename, cfg)
	d := startDaemonFixture(t, filename, fixtureControlToken)
	record := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "create",
		map[string]any{"launch": "research", "goal": "wait for cancellation"}, http.StatusCreated)
	command := startSystemCommand{ExpectedRevision: 1, Stream: true}
	started := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/start", "start", command, http.StatusAccepted)
	receiveProvider(t, requests)
	d.stop(t, true)
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("daemon death did not close provider stream")
	}
	restarted := startDaemonFixture(t, filename, fixtureControlToken)
	saved := daemonSystem(t, restarted, fixtureControlToken, "GET", "/v1/systems/"+record.ID, "", nil, http.StatusOK)
	if saved.State != "stopped" || saved.Execution.State != "unknown" || saved.ReservedTokens != started.Execution.Reservation ||
		saved.Execution.TaskState != "failed" || !strings.Contains(saved.Execution.Reason, "restarted") {
		t.Fatalf("crash outcome not recovered conservatively: %+v", saved)
	}
	daemonSystem(t, restarted, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/start", "start", command, http.StatusAccepted)
	_, data := daemonRequest(t, restarted, fixtureControlToken, "GET", "/v1/model-broker", "", nil)
	var metrics struct {
		Quotas []quotaView `json:"quota_groups"`
	}
	if err := json.Unmarshal(data, &metrics); err != nil {
		t.Fatal(err)
	}
	if len(metrics.Quotas) != 1 || metrics.Quotas[0].RequestCredit >= 1 || metrics.Quotas[0].ReservedRateTokens != saved.ReservedTokens ||
		metrics.Quotas[0].InFlight != 0 || count.Load() != 1 {
		t.Fatalf("restart restored allowance or replayed call: %s", data)
	}
}

func TestOperatorThrottlingRetriesThroughAdmission(t *testing.T) {
	var attempts atomic.Int64
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			return
		}
		completion(w, "after throttle")
	})
	q := cfg.QuotaGroups["account"]
	q.BurstRequests = 2
	cfg.QuotaGroups["account"] = q
	filename := filepath.Join(t.TempDir(), "daemon.json")
	writeFixtureConfiguration(t, filename, cfg)
	d := startDaemonFixture(t, filename, fixtureControlToken)
	record := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "create",
		map[string]any{"launch": "research", "goal": "retry explicit rejection"}, http.StatusCreated)
	daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/start", "start", startSystemCommand{ExpectedRevision: 1}, http.StatusAccepted)
	result := waitSystem(t, d, record.ID, func(s systemRecord) bool { return s.State == "inactive" })
	if result.Execution.State != "completed" || result.Execution.Attempts != 2 || result.UsedTokens != 18 ||
		result.ReservedTokens != 0 || count.Load() != 2 {
		t.Fatalf("throttle retry accounting: %+v", result)
	}
	_, data := daemonRequest(t, d, fixtureControlToken, "GET", "/v1/model-broker", "", nil)
	var metrics struct {
		Quotas []quotaView `json:"quota_groups"`
	}
	if err := json.Unmarshal(data, &metrics); err != nil {
		t.Fatal(err)
	}
	if metrics.Quotas[0].ReservedRateTokens != 2*result.Execution.Reservation {
		t.Fatal("retry refunded provider rate capacity")
	}
}

func TestConfiguredSandboxDoesNotExpandWriteGrants(t *testing.T) {
	root := workspace(t)
	target, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	profile := sandboxConfig{Read: []string{"inputs"}, ReadWrite: []string{"scratch"}, Network: "blocked"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var reply message
	err = supervise(ctx, microoperatorBinary, root, target, []string{"-sandbox-probe=write", "-sandbox-value=" + filepath.Join(root, "output", "denied")},
		func(in io.Writer, out io.Reader) error { var err error; reply, err = exchange(in, out); return err }, profile)
	if err != nil || reply.Type != "denied" {
		t.Fatalf("configured profile expanded filesystem authority: %+v %v", reply, err)
	}
	if _, err := os.Stat(filepath.Join(root, "output", "denied")); !os.IsNotExist(err) {
		t.Fatalf("denied write had a side effect: %v", err)
	}
}
