//go:build integration && (darwin || linux)

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type daemonFixture struct {
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	done    chan struct{}
	waitErr error
	stderr  bytes.Buffer
	socket  string
	client  *http.Client
}

func startDaemonFixture(t *testing.T, filename, token string) *daemonFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	fixture := &daemonFixture{cancel: cancel, done: make(chan struct{})}
	fixture.cmd = exec.CommandContext(ctx, microoperatorBinary, "daemon", "--config", filename)
	fixture.cmd.Env = []string{"LANG=C", "TZ=UTC", controlTokenEnv + "=" + token, fixtureProviderEnv + "=" + fixtureProviderSecret}
	fixture.cmd.Stderr = &fixture.stderr
	fixture.cmd.WaitDelay = time.Second
	stdout, err := fixture.cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := fixture.cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	go func() {
		fixture.waitErr = fixture.cmd.Wait()
		close(fixture.done)
	}()
	t.Cleanup(func() {
		select {
		case <-fixture.done:
			fixture.cancel()
		default:
			fixture.stop(t, false)
		}
	})
	type readyResult struct {
		message message
		err     error
	}
	ready := make(chan readyResult, 1)
	go func() {
		value, err := readMessage(bufio.NewReaderSize(stdout, maxFrame+1))
		ready <- readyResult{value, err}
	}()
	select {
	case result := <-ready:
		if result.err != nil || result.message.Type != "ready" {
			fixture.cancel()
			<-fixture.done
			t.Fatalf("daemon readiness: %+v, %v; %s", result.message, result.err, fixture.stderr.String())
		}
		fixture.socket = result.message.Data
	case <-fixture.done:
		t.Fatalf("daemon exited before readiness: %v; %s", fixture.waitErr, fixture.stderr.String())
	case <-ctx.Done():
		<-fixture.done
		t.Fatalf("daemon startup timed out: %s", fixture.stderr.String())
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", fixture.socket)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	fixture.client = &http.Client{Transport: transport, Timeout: 3 * time.Second}
	status, _ := daemonRequest(t, fixture, token, "GET", "/v1/health", "", nil)
	if status != http.StatusOK {
		t.Fatalf("ready daemon not responsive: %d", status)
	}
	return fixture
}

func (fixture *daemonFixture) stop(t *testing.T, abrupt bool) {
	t.Helper()
	var signal os.Signal = syscall.SIGTERM
	if abrupt {
		signal = syscall.SIGKILL
	}
	if err := fixture.cmd.Process.Signal(signal); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatal(err)
	}
	select {
	case <-fixture.done:
	case <-time.After(7 * time.Second):
		fixture.cmd.Process.Kill()
		<-fixture.done
		t.Fatal("daemon did not shut down within its bound")
	}
	fixture.cancel()
	if !abrupt && fixture.waitErr != nil {
		t.Fatalf("daemon shutdown: %v; %s", fixture.waitErr, fixture.stderr.String())
	}
	if strings.Contains(fixture.stderr.String(), fixtureProviderSecret) ||
		strings.Contains(fixture.stderr.String(), fixtureControlToken) {
		t.Fatal("credential leaked to daemon stderr")
	}
}

func daemonRequest(t *testing.T, fixture *daemonFixture, token, method, path, key string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(fixtureJSON(t, body))
	}
	request, err := http.NewRequest(method, "http://localhost"+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response, err := fixture.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 2*maxConfigBytes))
	if err := errors.Join(readErr, response.Body.Close()); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(fixtureProviderSecret)) || bytes.Contains(data, []byte(token)) {
		t.Fatal("credential leaked to control response")
	}
	return response.StatusCode, data
}

func daemonSystem(t *testing.T, fixture *daemonFixture, token, method, path, key string, body any, wantStatus int) systemRecord {
	t.Helper()
	status, data := daemonRequest(t, fixture, token, method, path, key, body)
	if status != wantStatus {
		t.Fatalf("control response %d: %s", status, data)
	}
	var record systemRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestDaemonSystemsSurviveRestartWithoutExecution(t *testing.T) {
	var providerCalls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		http.Error(w, "model dispatch is forbidden in milestone 1", http.StatusInternalServerError)
	}))
	defer provider.Close()
	root := workspace(t)
	cfg := fixtureConfiguration(t)
	cfg.DataDir = filepath.Join(root, "state")
	p := cfg.Providers["primary"]
	p.BaseURL = provider.URL + "/v1"
	cfg.Providers["primary"] = p
	filename := filepath.Join(root, "config.json")
	writeFixtureConfiguration(t, filename, cfg)
	firstDaemon := startDaemonFixture(t, filename, fixtureControlToken)
	for filename, mode := range map[string]os.FileMode{
		cfg.DataDir: 0700, firstDaemon.socket: 0600,
		filepath.Join(cfg.DataDir, "state.db"): 0600, filepath.Join(cfg.DataDir, "daemon.lock"): 0600,
	} {
		info, err := os.Stat(filename)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("private state permissions: %s: %v, %v", filename, info, err)
		}
	}
	status, data := daemonRequest(t, firstDaemon, fixtureControlToken, "GET", "/v1/systems", "", nil)
	if status != http.StatusOK || string(data) != "{\"systems\":[]}\n" {
		t.Fatalf("configuration declarations created systems: %d %s", status, data)
	}
	goal := "Record this goal, but do not execute it."
	create := createSystemCommand{Launch: "research", Goal: &goal}
	first := daemonSystem(t, firstDaemon, fixtureControlToken, "POST", "/v1/systems", "first", create, http.StatusCreated)
	second := daemonSystem(t, firstDaemon, fixtureControlToken, "POST", "/v1/systems", "second", create, http.StatusCreated)
	if first.ID == second.ID || first.OperatorID == second.OperatorID || first.Goal.ID == second.Goal.ID {
		t.Fatal("two instances from one launch configuration share identity")
	}
	next := first.Configuration
	next.Operator.Prompt = "Only the first instance changes."
	next.Limits.TokenBudget = 45000
	update := reviseSystemCommand{ExpectedRevision: 1, Configuration: &next}
	revised := daemonSystem(t, firstDaemon, fixtureControlToken, "PUT", "/v1/systems/"+first.ID+"/configuration",
		"revise-first", update, http.StatusOK)
	if runtime.GOOS == "linux" {
		pid := firstDaemon.cmd.Process.Pid
		children, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
		if err != nil || len(bytes.TrimSpace(children)) != 0 {
			t.Fatalf("inactive daemon spawned child processes: %q, %v", children, err)
		}
	}
	if providerCalls.Load() != 0 {
		t.Fatal("inactive systems dispatched a model call")
	}
	firstDaemon.stop(t, false)

	template := cfg.Systems["research"]
	template.Operator.Prompt = "A changed template must not overwrite either instance."
	template.Limits.TokenBudget = 99999
	cfg.Systems["research"] = template
	writeFixtureConfiguration(t, filename, cfg)
	rotatedToken := fixtureControlToken + "-rotated"
	secondDaemon := startDaemonFixture(t, filename, rotatedToken)
	status, _ = daemonRequest(t, secondDaemon, fixtureControlToken, "GET", "/v1/systems", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatal("old control credential survived token rotation")
	}
	recovered := daemonSystem(t, secondDaemon, rotatedToken, "GET", "/v1/systems/"+first.ID, "", nil, http.StatusOK)
	if !reflect.DeepEqual(recovered, revised) {
		t.Fatalf("first instance changed on restart: %+v", recovered)
	}
	recovered = daemonSystem(t, secondDaemon, rotatedToken, "GET", "/v1/systems/"+second.ID, "", nil, http.StatusOK)
	if !reflect.DeepEqual(recovered, second) {
		t.Fatalf("second instance changed on restart: %+v", recovered)
	}
	replay := daemonSystem(t, secondDaemon, rotatedToken, "POST", "/v1/systems", "first", create, http.StatusCreated)
	if replay.ID != first.ID {
		t.Fatal("restart lost create idempotency")
	}
	replay = daemonSystem(t, secondDaemon, rotatedToken, "PUT", "/v1/systems/"+first.ID+"/configuration",
		"revise-first", update, http.StatusOK)
	if !reflect.DeepEqual(replay, revised) {
		t.Fatal("restart lost revision idempotency")
	}

	// A crash leaves the socket pathname behind, but the OS releases the lock.
	// Recovery must preserve committed state and remove only that stale socket.
	secondDaemon.stop(t, true)
	cfg.Providers = nil
	cfg.Models = nil
	cfg.QuotaGroups = nil
	cfg.SandboxProfiles = nil
	cfg.Tools = nil
	cfg.Systems = nil
	writeFixtureConfiguration(t, filename, cfg)
	thirdDaemon := startDaemonFixture(t, filename, rotatedToken)
	recovered = daemonSystem(t, thirdDaemon, rotatedToken, "GET", "/v1/systems/"+first.ID, "", nil, http.StatusOK)
	if recovered.BlockedReason == "" || recovered.State != "inactive" ||
		!reflect.DeepEqual(recovered.Configuration, revised.Configuration) ||
		!reflect.DeepEqual(recovered.Grants, revised.Grants) {
		t.Fatalf("removed administrative definitions did not preserve/block old revision: %+v", recovered)
	}
	status, data = daemonRequest(t, thirdDaemon, rotatedToken, "GET", "/v1/systems", "", nil)
	var listing struct {
		Systems []systemRecord `json:"systems"`
	}
	if err := json.Unmarshal(data, &listing); err != nil || status != http.StatusOK || len(listing.Systems) != 2 {
		t.Fatalf("recovery duplicated/lost systems: %d %s, %v", status, data, err)
	}
	thirdDaemon.stop(t, false)
	if providerCalls.Load() != 0 {
		t.Fatalf("provider dispatches = %d; want zero", providerCalls.Load())
	}
}

func TestRetiredDemoCommandsAreRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no arguments", nil},
		{"run command", []string{"run"}},
		{"run flags", []string{"run", "-message", "legacy"}},
		{"placeholder worker", []string{"worker"}},
		{"test-only fixture", []string{"-sandbox-probe=pipe"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, microoperatorBinary, tc.args...)
			cmd.Env = workerEnv(workspace(t))
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || stdout.Len() != 0 ||
				stderr.String() != "usage: microoperator daemon --config PATH | ui --socket PATH [--listen 127.0.0.1:8080] | toolchain-digest ABSOLUTE_GOROOT\n" {
				t.Fatalf("retired entry point: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
		})
	}
}

func TestDaemonStartupDenialsAndExclusiveOwnership(t *testing.T) {
	root := workspace(t)
	cfg := fixtureConfiguration(t)
	cfg.DataDir = filepath.Join(root, "state")
	filename := filepath.Join(root, "config.json")
	writeFixtureConfiguration(t, filename, cfg)
	runFailure := func(env []string, want string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, microoperatorBinary, "daemon", "--config", filename)
		cmd.Env = append([]string{"LANG=C", "TZ=UTC"}, env...)
		output, err := cmd.CombinedOutput()
		if err == nil || !bytes.Contains(output, []byte(want)) || bytes.Contains(output, []byte(`"type":"ready"`)) ||
			bytes.Contains(output, []byte(fixtureProviderSecret)) || bytes.Contains(output, []byte(fixtureControlToken)) {
			t.Fatalf("unsafe/missing startup rejection: %v, %s", err, output)
		}
	}
	runFailure([]string{controlTokenEnv + "=" + fixtureControlToken}, "api_key_env")
	runFailure([]string{fixtureProviderEnv + "=" + fixtureProviderSecret}, controlTokenEnv)
	if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
		t.Fatalf("failed validation created state: %v", err)
	}
	cfg.SchemaVersion = 99
	writeFixtureConfiguration(t, filename, cfg)
	runFailure([]string{controlTokenEnv + "=" + fixtureControlToken, fixtureProviderEnv + "=" + fixtureProviderSecret}, "schema_version")
	cfg.SchemaVersion = 1
	profile := cfg.SandboxProfiles["worker"]
	profile.Network = "allow-all"
	cfg.SandboxProfiles["worker"] = profile
	writeFixtureConfiguration(t, filename, cfg)
	runFailure([]string{controlTokenEnv + "=" + fixtureControlToken, fixtureProviderEnv + "=" + fixtureProviderSecret}, "network")
	profile.Network = "blocked"
	cfg.SandboxProfiles["worker"] = profile
	writeFixtureConfiguration(t, filename, cfg)
	daemon := startDaemonFixture(t, filename, fixtureControlToken)
	runFailure([]string{controlTokenEnv + "=" + fixtureControlToken, fixtureProviderEnv + "=" + fixtureProviderSecret}, "lock data_dir")
	status, _ := daemonRequest(t, daemon, fixtureControlToken, "GET", "/v1/health", "", nil)
	if status != http.StatusOK {
		t.Fatal("second daemon displaced the first")
	}
	daemon.stop(t, false)
}
