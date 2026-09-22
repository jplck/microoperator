//go:build integration && linux

package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jplck/microoperator/internal/protocol"
	"github.com/jplck/microoperator/internal/state"
	webui "github.com/jplck/microoperator/internal/ui"
)

func TestDetachedUIControlsTwoSystemsAndCanExit(t *testing.T) {
	betaEntered := make(chan struct{}, 1)
	release := make(chan struct{})
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []state.ChatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		last := request.Messages[len(request.Messages)-1]
		if last.Content == "finish beta" {
			betaEntered <- struct{}{}
			select {
			case <-release:
				completion(w, "done-beta")
			case <-r.Context().Done():
			}
			return
		}
		functionCompletion(w, "runtime.task.wait", "wait", map[string]string{"reason": "await user input"})
	})
	def := cfg.Systems["research"]
	def.Tools = []string{"runtime.task.wait"}
	def.Operator.Tools = def.Tools
	cfg.Systems["research"] = def
	q := cfg.QuotaGroups["account"]
	q.BurstRequests = 10
	cfg.QuotaGroups["account"] = q
	filename := filepath.Join(t.TempDir(), "daemon.json")
	writeFixtureConfiguration(t, filename, cfg)
	d := startDaemonFixture(t, filename, fixtureControlToken)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, microoperatorBinary, "ui", "--socket", d.socket, "--listen", "127.0.0.1:0")
	cmd.Env = []string{"LANG=C", "TZ=UTC", protocol.ControlTokenEnv + "=" + fixtureControlToken, webui.TokenEnv + "=" + fixtureUIToken}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	defer func() {
		if !stopped {
			cmd.Process.Kill()
			<-done
		}
	}()
	ready, err := protocol.ReadMessage(bufio.NewReaderSize(stdout, protocol.MaxFrame+1))
	if err != nil || ready.Type != "ready" {
		cmd.Process.Kill()
		<-done
		stopped = true
		t.Fatalf("UI readiness: %+v %v %s", ready, err, stderr.String())
	}
	client := &http.Client{Timeout: 5 * time.Second}
	browse := func(method, path string, form url.Values) (int, string) {
		t.Helper()
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		request, err := http.NewRequest(method, ready.Data+path, body)
		if err != nil {
			t.Fatal(err)
		}
		request.SetBasicAuth("operator", fixtureUIToken)
		if form != nil {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("Origin", ready.Data)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(fixtureControlToken)) || bytes.Contains(data, []byte(fixtureUIToken)) {
			t.Fatal("UI exposed credentials")
		}
		return response.StatusCode, string(data)
	}
	status, page := browse("GET", "/", nil)
	if status != 200 {
		t.Fatal(page)
	}
	csrf := hiddenUIValue(t, page, "csrf")
	post := func(path, key string, body any, want int) {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		status, response := browse("POST", "/command", url.Values{"csrf": {csrf}, "key": {key}, "kind": {"json"}, "path": {path}, "method": {"POST"}, "return": {"/"}, "json": {string(data)}})
		if status != want {
			t.Fatalf("UI %s: %d %s", path, status, response)
		}
	}
	for _, goal := range []string{"alpha", "alpha", "beta"} {
		post("/v1/systems", "create-"+goal, map[string]string{"launch": "research", "goal": goal}, 201)
	}
	_, data := daemonRequest(t, d, fixtureControlToken, "GET", "/v1/systems", "", nil)
	var systems struct {
		Systems []state.SystemRecord `json:"systems"`
	}
	if err := json.Unmarshal(data, &systems); err != nil {
		t.Fatal(err)
	}
	if len(systems.Systems) != 2 {
		t.Fatalf("UI retried creation duplicated a system: %s", data)
	}
	ids := map[string]string{}
	for _, system := range systems.Systems {
		ids[system.Goal.Prompt] = system.ID
		post("/v1/systems/"+system.ID+"/start", "start-"+system.ID, state.StartSystemCommand{ExpectedRevision: 1}, 202)
		waitSystem(t, d, system.ID, func(s state.SystemRecord) bool { return s.Execution.TaskState == "waiting" })
	}
	post("/v1/systems/"+ids["alpha"]+"/tools/drafts", "draft", state.DraftCommand{Kind: "executable", Description: "Inert source", Content: "package main\nfunc main(){}", Requires: []string{}}, 201)
	post("/v1/systems/"+ids["beta"]+"/input", "input", map[string]string{"content": "finish beta"}, 202)
	select {
	case <-betaEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("UI input did not reach the actual worker")
	}
	post("/v1/systems/"+ids["alpha"]+"/stop", "stop", struct{}{}, 202)
	waitSystem(t, d, ids["alpha"], func(s state.SystemRecord) bool { return s.State == "stopped" })
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Fatalf("UI shutdown: %v %s", err, stderr.String())
		}
	case <-time.After(7 * time.Second):
		t.Fatal("UI did not stop")
	}
	close(release)
	result := waitSystem(t, d, ids["beta"], func(s state.SystemRecord) bool { return s.State == "inactive" })
	if result.Execution.Response != "done-beta" || count.Load() != 3 {
		t.Fatalf("UI exit affected daemon work: %+v calls=%d", result.Execution, count.Load())
	}
	_, data = daemonRequest(t, d, fixtureControlToken, "GET", "/v1/systems/"+ids["alpha"]+"/tools", "", nil)
	var tools struct {
		Tools []state.RegistryEntry `json:"tools"`
	}
	if err := json.Unmarshal(data, &tools); err != nil {
		t.Fatal(err)
	}
	drafts := 0
	for _, tool := range tools.Tools {
		if tool.SystemID != "" {
			drafts++
			if tool.State != "draft" {
				t.Fatal("UI exit approved a source proposal")
			}
		}
	}
	if drafts != 1 {
		t.Fatal("draft history disappeared")
	}
}
