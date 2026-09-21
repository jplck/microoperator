//go:build integration && linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGeneratedPromotionAndExactArtifactExecution(t *testing.T) {
	if resourceTestUnit(t) {
		return
	}
	group, err := prepareResourceRoot()
	if err != nil {
		t.Fatal(err)
	}
	var generatedID atomic.Value
	generatedID.Store("")
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []chatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		last := request.Messages[len(request.Messages)-1]
		if last.Content == "wait for review" {
			functionCompletion(w, "runtime.task.wait", "wait", map[string]string{"reason": "human review"})
			return
		}
		if last.Role == "tool" {
			if last.Content != `{"text":"HELLO"}` {
				t.Errorf("unexpected generated result: %s", last.Content)
			}
			completion(w, "approved binary reused")
		} else {
			functionCompletion(w, generatedID.Load().(string), "generated", map[string]string{"text": "hello"})
		}
	})
	root, err := filepath.EvalSymlinks(runtime.GOROOT())
	if err != nil {
		t.Fatal(err)
	}
	digest, err := toolchainDigest(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Learning = &learningConfig{ToolchainRoot: root, ToolchainDigest: digest}
	profile := cfg.SandboxProfiles["worker"]
	profile.Resources = &resourceLimits{MemoryBytes: 1 << 30, WorkspaceBytes: 512 << 20, Processes: 256, CPUPercent: 200}
	cfg.SandboxProfiles["worker"] = profile
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
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/start", "start", startSystemCommand{ExpectedRevision: 1, LifetimeSeconds: 3600}, 202)
	waiting := waitSystem(t, d, system.ID, func(s systemRecord) bool { return s.Execution.TaskState == "waiting" })
	check := daemonSystem(t, d, fixtureControlToken, "POST", base+"/learning/checks", "checks", map[string]any{"cases": []protectedCase{{Input: "hello", Expected: "HELLO"}}}, 201)
	draft := daemonSystem(t, d, fixtureControlToken, "POST", base+"/tools/drafts", "draft", draftCommand{Kind: "executable", Description: "Uppercase JSON text", Content: `package main; import "strings"; func Process(text string)(string,error){return strings.ToUpper(text),nil}`, Requires: []string{}}, 201)
	toolID := learningResult(t, draft, "tool_id")
	generatedID.Store(toolID)
	job := daemonSystem(t, d, fixtureControlToken, "POST", base+"/learning/evaluate", "evaluate", evaluateCommand{ToolID: toolID, Version: 1, CheckID: learningResult(t, check, "check_id"), TaskID: waiting.Execution.TaskID}, 202)
	evaluation := waitEvaluation(t, d, system.ID, learningResult(t, job, "evaluation_id"))
	if evaluation.State != "pending_approval" || evaluation.Artifact == "" || count.Load() != 1 {
		t.Fatalf("confined evaluation: %+v calls=%d", evaluation, count.Load())
	}
	approved := daemonSystem(t, d, fixtureControlToken, "POST", base+"/learning/"+evaluation.ID+"/approve", "approve", map[string]any{"digest": evaluation.Digest, "expires_seconds": 3600, "task_uses": 4}, 200)
	var pin toolPin
	if err := json.Unmarshal(approved.CommandResult, &pin); err != nil {
		t.Fatal(err)
	}
	pending := daemonSystem(t, d, fixtureControlToken, "POST", base+"/tools/drafts", "cancel-draft", draftCommand{Kind: "executable", Description: "Cancellation fixture", Content: `package main; func Process(text string)(string,error){for{}}`, Requires: []string{}}, 201)
	cancelJob := daemonSystem(t, d, fixtureControlToken, "POST", base+"/learning/evaluate", "cancel-evaluation", evaluateCommand{ToolID: learningResult(t, pending, "tool_id"), Version: 1, CheckID: learningResult(t, check, "check_id"), TaskID: waiting.Execution.TaskID}, 202)
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		active, err := activeResourceGroup(group)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if active != "" {
			data, err := os.ReadFile(filepath.Join(active, "cgroup.events"))
			if err == nil && strings.Contains(string(data), "populated 1") {
				break
			}
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
		select {
		case <-deadline.C:
			t.Fatal("confined builder did not become active")
		case <-tick.C:
		}
	}
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/goals/"+waiting.Execution.GoalID+"/stop", "stop", struct{}{}, 202)
	waitSystem(t, d, system.ID, func(s systemRecord) bool { return s.State == "stopped" })
	canceled := waitEvaluation(t, d, system.ID, learningResult(t, cancelJob, "evaluation_id"))
	if canceled.State != "failed" {
		t.Fatalf("canceled build became promotable: %+v", canceled)
	}
	def.Tools = []string{pin.Name}
	def.Operator.Tools = def.Tools
	revised := daemonSystem(t, d, fixtureControlToken, "PUT", base+"/configuration", "assign", reviseSystemCommand{ExpectedRevision: 1, Configuration: &def, LocalTools: []toolPin{pin}}, 200)
	goal := "run the approved generated tool"
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/start", "reuse", startSystemCommand{ExpectedRevision: revised.Revision, Goal: &goal}, 202)
	result := waitSystem(t, d, system.ID, func(s systemRecord) bool { return s.State == "inactive" })
	if result.Execution.Response != "approved binary reused" || count.Load() != 3 {
		t.Fatalf("generated reuse failed: %+v calls=%d", result.Execution, count.Load())
	}
	binary, err := readGeneratedArtifact(cfg.DataDir, system.ID, evaluation.Artifact)
	if err != nil {
		t.Fatal(err)
	}
	path, err := generatedArtifactPath(cfg.DataDir, system.ID, evaluation.Artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered binary"), 0600); err != nil {
		t.Fatal(err)
	}
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/start", "tampered", startSystemCommand{ExpectedRevision: revised.Revision, Goal: &goal}, 202)
	result = waitSystem(t, d, system.ID, func(s systemRecord) bool { return s.State == "inactive" })
	if result.Execution.TaskState != "failed" || count.Load() != 4 {
		t.Fatalf("modified binary was not refused: %+v calls=%d", result.Execution, count.Load())
	}
	if err := os.WriteFile(path, binary, 0600); err != nil {
		t.Fatal(err)
	}
	daemonSystem(t, d, fixtureControlToken, "POST", base+"/tools/revoke", "revoke", map[string]any{"name": pin.Name, "version": pin.Version}, 200)
	if status, _ := daemonRequest(t, d, fixtureControlToken, "POST", base+"/start", "revoked", startSystemCommand{ExpectedRevision: revised.Revision, Goal: &goal}); status != 400 {
		t.Fatal("revoked generated version remained executable")
	}
}

func TestGeneratedBuildQualification(t *testing.T) {
	if resourceTestUnit(t) {
		return
	}
	group, err := prepareResourceRoot()
	if err != nil {
		t.Fatal(err)
	}
	profile := resourceTestProfile(group)
	profile.Resources = &resourceLimits{MemoryBytes: 1 << 30, WorkspaceBytes: 512 << 20, Processes: 256, CPUPercent: 200}
	root, err := filepath.EvalSymlinks(runtime.GOROOT())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 140*time.Second)
	defer cancel()
	digest, err := toolchainDigest(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	config := learningConfig{ToolchainRoot: root, ToolchainDigest: digest}
	directory := t.TempDir()
	secret := filepath.Join(t.TempDir(), "denied-fixture")
	if err := os.WriteFile(secret, []byte("not accessible"), 0600); err != nil {
		t.Fatal(err)
	}
	source := `package main
import("strings";"os";"net";"fmt")
func Process(s string)(string,error){
 if _,err:=os.ReadFile(s);err==nil{return "",fmt.Errorf("host file readable")}
 if os.Getenv("CGO_ENABLED")!="" || os.Getenv("MICROOPERATOR_CONTROL_TOKEN")!="" || os.Getenv("GOROOT")!=""{return "",fmt.Errorf("builder or daemon environment leaked")}
 if c,err:=net.Dial("tcp","127.0.0.1:9");err==nil{c.Close();return "",fmt.Errorf("network allowed")}
 return strings.ToUpper(s),nil
}`
	binary, err := buildGenerated(ctx, microoperatorBinary, directory, "sys_0123456789abcdef0123456789abcdef", source, config, profile)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"hello", secret} {
		output, err := runGenerated(ctx, microoperatorBinary, directory, "sys_0123456789abcdef0123456789abcdef", binary, input, profile)
		if err != nil || output != strings.ToUpper(input) {
			t.Fatalf("generated run: %q %v", output, err)
		}
	}
	config.ToolchainDigest = strings.Repeat("0", 64)
	if _, err := buildGenerated(ctx, microoperatorBinary, directory, "sys_0123456789abcdef0123456789abcdef", source, config, profile); err == nil {
		t.Fatal("changed toolchain accepted")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("workspaces retained: %v %v", entries, err)
	}
}
