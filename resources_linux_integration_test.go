//go:build integration && linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func platformResourceProbe(mode, value string) (string, error) {
	switch mode {
	case "resource-memory":
		data := make([]byte, 256<<20)
		for i := 0; i < len(data); i += 4096 {
			data[i] = 1
		}
		runtime.KeepAlive(data)
		return "", errors.New("memory cap was not enforced")
	case "resource-cpu":
		var children []*exec.Cmd
		defer func() {
			for _, child := range children {
				child.Process.Kill()
				child.Wait()
			}
		}()
		if value != "leaf" {
			executable, err := os.Executable()
			if err != nil {
				return "", err
			}
			for i := 0; i < 2; i++ {
				child := exec.Command(executable, "-sandbox-probe=resource-cpu", "-sandbox-value=leaf")
				child.Env = os.Environ()
				child.Stdin = strings.NewReader("{\"type\":\"probe\"}\n")
				if err := child.Start(); err != nil {
					return "", err
				}
				children = append(children, child)
			}
		}
		deadline := time.Now().Add(1500 * time.Millisecond)
		sum := 0
		for time.Now().Before(deadline) {
			sum++
		}
		return strconv.Itoa(sum), nil
	case "resource-shm":
		id, err := strconv.Atoi(value)
		if err != nil {
			return "", err
		}
		address, _, errno := syscall.Syscall(syscall.SYS_SHMAT, uintptr(id), 0, 010000)
		if errno == syscall.EINVAL || errno == syscall.EACCES {
			return "isolated", nil
		}
		if errno == 0 {
			syscall.Syscall(syscall.SYS_SHMDT, address, 0, 0)
		}
		return "", fmt.Errorf("host IPC isolation failed: %v", errno)
	case "resource-pids":
		executable, err := os.Executable()
		if err != nil {
			return "", err
		}
		var children []*exec.Cmd
		defer func() {
			for _, child := range children {
				child.Process.Kill()
				child.Wait()
			}
		}()
		for i := 0; i < 128; i++ {
			child := exec.Command(executable, "-sandbox-probe=sleep")
			child.Env = os.Environ()
			if err := child.Start(); err != nil {
				return "fork bounded", nil
			}
			children = append(children, child)
		}
		return "thread creation bounded", nil
	case "resource-escape":
		executable, err := os.Executable()
		if err != nil {
			return "", err
		}
		child := exec.Command(executable, "-sandbox-probe=sleep")
		child.Env = os.Environ()
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			return "", err
		}
		group, err := syscall.Getpgid(child.Process.Pid)
		if err != nil || group != child.Process.Pid {
			return "", fmt.Errorf("escape fixture did not establish a separate process group: %v", err)
		}
		if err := writeMessage(os.Stdout, message{Type: "escaped", Data: strconv.Itoa(child.Process.Pid)}); err != nil {
			return "", err
		}
		return "", child.Wait()
	case "resource-supervisor":
		var args struct{ Root, Launcher, Target string }
		if err := json.Unmarshal([]byte(value), &args); err != nil {
			return "", err
		}
		group, err := prepareResourceRoot()
		if err != nil {
			return "", err
		}
		profile := resourceTestProfile(group)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err = supervise(ctx, args.Launcher, args.Root, args.Target, []string{"-sandbox-probe=resource-escape"}, func(in io.Writer, out io.Reader) error {
			reply, err := exchange(in, out)
			if err != nil {
				return err
			}
			if reply.Type != "escaped" {
				return fmt.Errorf("escape fixture failed: %+v", reply)
			}
			path, err := activeResourceGroup(group)
			if err != nil {
				return err
			}
			if err := writeMessage(os.Stdout, message{Type: "resource-group", Data: path}); err != nil {
				return err
			}
			_, err = io.Copy(io.Discard, out)
			return err
		}, profile)
		return "", err
	case "resource-pid":
		return strconv.Itoa(os.Getpid()), nil
	case "resource-disk":
		block := make([]byte, 512<<10)
		total := 0
		for i := 0; i < 32; i++ {
			dir := "output"
			if i%2 == 0 {
				dir = "scratch"
			}
			file, err := os.Create(filepath.Join(dir, strconv.Itoa(i)))
			if err != nil {
				return "", err
			}
			n, err := file.Write(block)
			closeErr := file.Close()
			if closeErr != nil {
				return "", closeErr
			}
			total += n
			if errors.Is(err, syscall.ENOSPC) {
				return strconv.Itoa(total), nil
			}
			if err != nil {
				return "", err
			}
		}
		return "", errors.New("workspace exceeded its hard cap")
	case "resource-seal":
		if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, 1, 0, 0, 0, 0, 0); errno != syscall.EPERM {
			return "", fmt.Errorf("parent-death signal not sealed: %v", errno)
		}
		if err := syscall.Unshare(syscall.CLONE_NEWUSER); err != syscall.EPERM {
			return "", fmt.Errorf("namespace restriction not sealed: %v", err)
		}
		return "sealed", nil
	default:
		return "", fmt.Errorf("unknown test probe %q", mode)
	}
}

func resourceTestProfile(group string) sandboxConfig {
	return sandboxConfig{Read: []string{"inputs"}, ReadWrite: []string{"scratch", "output"}, Network: "blocked",
		Resources: &resourceLimits{MemoryBytes: 256 << 20, WorkspaceBytes: 8 << 20, Processes: 128, CPUPercent: 100}, resourceRoot: group}
}

func activeResourceGroup(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	found := ""
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "worker_") {
			if found != "" {
				return "", errors.New("more than one fixture resource group")
			}
			found = filepath.Join(root, entry.Name())
		}
	}
	if found == "" {
		return "", fmt.Errorf("fixture resource group not found: %w", os.ErrNotExist)
	}
	return found, nil
}

func resourceCounter(path, key string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == key {
			return strconv.ParseInt(fields[1], 10, 64)
		}
	}
	return 0, fmt.Errorf("counter %s missing from %s", key, path)
}

func resourceTestUnit(t *testing.T) bool {
	t.Helper()
	if os.Getenv("MICROOPERATOR_RESOURCE_TEST") != "1" {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "systemd-run", "--user", "--wait", "--pipe", "--collect",
			"--property=Delegate=cpu memory pids", "--property=RuntimeMaxSec=170", "--property=WorkingDirectory="+cwd,
			"--setenv=PATH="+os.Getenv("PATH"), "--setenv=MICROOPERATOR_RESOURCE_TEST=1",
			binary, "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.timeout=160s", "-test.v")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("required resource qualification failed: %v\n%s", err, output)
		} else {
			t.Log(string(output))
		}
		return true
	}
	return false
}

func TestResourceQualification(t *testing.T) {
	if resourceTestUnit(t) {
		return
	}
	group, err := prepareResourceRoot()
	if err != nil {
		t.Fatal(err)
	}
	target, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	profile := resourceTestProfile(group)
	t.Run("private IPC", func(t *testing.T) {
		id, _, errno := syscall.Syscall(syscall.SYS_SHMGET, 0, 4096, 01000|0600)
		if errno != 0 {
			t.Fatal(errno)
		}
		defer syscall.Syscall(syscall.SYS_SHMCTL, id, 0, 0)
		address, _, errno := syscall.Syscall(syscall.SYS_SHMAT, id, 0, 010000)
		if errno != 0 {
			t.Fatal(errno)
		}
		if _, _, errno := syscall.Syscall(syscall.SYS_SHMDT, address, 0, 0); errno != 0 {
			t.Fatal(errno)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var reply message
		err := supervise(ctx, microoperatorBinary, workspace(t), target, []string{"-sandbox-probe=resource-shm", "-sandbox-value=" + strconv.FormatUint(uint64(id), 10)},
			func(in io.Writer, out io.Reader) error { var err error; reply, err = exchange(in, out); return err }, profile)
		if err != nil || reply.Type != "result" || reply.Data != "isolated" {
			t.Fatalf("host shared memory exposed: %+v %v", reply, err)
		}
	})
	for _, tc := range []struct{ mode, want string }{
		{"pipe", "complete"}, {"resource-pid", "1"}, {"resource-seal", "sealed"}, {"resource-disk", strconv.Itoa(8 << 20)},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var reply message
			err := supervise(ctx, microoperatorBinary, workspace(t), target, []string{"-sandbox-probe=" + tc.mode},
				func(in io.Writer, out io.Reader) error { var err error; reply, err = exchange(in, out); return err }, profile)
			if err != nil {
				t.Fatal(err)
			}
			if reply.Type != "result" || reply.Data != tc.want {
				t.Fatalf("resource check: %+v; want %s", reply, tc.want)
			}
		})
	}
	for _, mode := range []string{"memory", "pids", "cpu"} {
		t.Run(mode, func(t *testing.T) {
			profile := resourceTestProfile(group)
			if mode == "memory" {
				profile.Resources.MemoryBytes = 64 << 20
			}
			if mode == "pids" {
				profile.Resources.Processes = 32
			}
			if mode == "cpu" {
				profile.Resources.CPUPercent = 10
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var evidence, usage int64
			start := time.Now()
			err := supervise(ctx, microoperatorBinary, workspace(t), target, []string{"-sandbox-probe=resource-" + mode}, func(in io.Writer, out io.Reader) error {
				path, err := activeResourceGroup(group)
				if err != nil {
					return err
				}
				_, exchangeErr := exchange(in, out)
				file, key := mode+".events", "max"
				if mode == "memory" {
					key = "oom_kill"
				}
				if mode == "cpu" {
					file, key = "cpu.stat", "nr_throttled"
				}
				evidence, err = resourceCounter(filepath.Join(path, file), key)
				if err == nil && mode == "cpu" {
					usage, err = resourceCounter(filepath.Join(path, file), "usage_usec")
				}
				return errors.Join(err, exchangeErr)
			}, profile)
			if mode == "memory" && err == nil {
				t.Fatal("memory exhaustion did not kill the confined process")
			}
			if evidence < 1 {
				t.Fatalf("no enforced %s limit: counter=%d err=%v", mode, evidence, err)
			}
			if mode == "cpu" && err != nil {
				t.Fatal(err)
			}
			if mode == "cpu" && usage > time.Since(start).Microseconds()/10+50000 {
				t.Fatalf("descendants exceeded aggregate 10%% CPU quota: %d usec", usage)
			}
		})
	}
	t.Run("escaped descendant cancellation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		path := ""
		err := supervise(ctx, microoperatorBinary, workspace(t), target, []string{"-sandbox-probe=resource-escape"}, func(in io.Writer, out io.Reader) error {
			reply, err := exchange(in, out)
			if err != nil {
				return err
			}
			if reply.Type != "escaped" {
				return fmt.Errorf("escape fixture: %+v", reply)
			}
			path, err = activeResourceGroup(group)
			if err != nil {
				return err
			}
			procs, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
			if err != nil {
				return err
			}
			if len(strings.Fields(string(procs))) < 2 {
				return errors.New("escaped descendant did not join resource group")
			}
			cancel()
			return context.Canceled
		}, profile)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("escaped descendants/resource group survived: %v", err)
		}
	})
	t.Run("abrupt supervisor death", func(t *testing.T) {
		data, err := json.Marshal(struct{ Root, Launcher, Target string }{workspace(t), microoperatorBinary, target})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, target, "-sandbox-probe=resource-supervisor", "-sandbox-value="+string(data))
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		defer stdin.Close()
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var stderr boundedStderr
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { cmd.Process.Kill(); cmd.Wait() }()
		reader := bufio.NewReaderSize(stdout, maxFrame+1)
		ready, err := readMessage(reader)
		if err != nil || ready.Type != "ready" {
			t.Fatalf("supervisor readiness: %+v %v", ready, err)
		}
		if err := writeMessage(stdin, message{Type: "probe"}); err != nil {
			t.Fatal(err)
		}
		reply, err := readMessage(reader)
		if err != nil || reply.Type != "resource-group" {
			cmd.Process.Kill()
			cmd.Wait()
			t.Fatalf("nested supervisor: %+v %v %s", reply, err, stderr.String())
		}
		if filepath.Dir(reply.Data) != group || !strings.HasPrefix(filepath.Base(reply.Data), "worker_") {
			t.Fatal("fixture returned unscoped group")
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err == nil {
			t.Fatal("supervisor did not die abruptly")
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			populated, err := resourceCounter(filepath.Join(reply.Data, "cgroup.events"), "populated")
			if err != nil {
				t.Fatal(err)
			}
			if populated == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("descendants outlived their supervisor")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, err := prepareResourceRoot(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(reply.Data); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("restart did not remove abandoned empty resource group")
		}
	})
	t.Run("daemon tools and crash recovery", func(t *testing.T) {
		dispatched := make(chan struct{}, 1)
		cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) {
			var request struct {
				Messages []chatMessage `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				return
			}
			for _, message := range request.Messages {
				if message.Role == "user" && message.Content == "crash" {
					dispatched <- struct{}{}
					<-r.Context().Done()
					return
				}
			}
			if request.Messages[len(request.Messages)-1].Role == "tool" {
				completion(w, "resource tool complete")
				return
			}
			functionCompletion(w, "runtime.text.analyze", "analyze", textArguments{Text: "bounded artifact", Save: true})
		})
		cfg.SandboxProfiles["worker"] = profile
		def := cfg.Systems["research"]
		def.Tools = []string{"runtime.text.analyze"}
		def.Operator.Tools = def.Tools
		cfg.Systems["research"] = def
		q := cfg.QuotaGroups["account"]
		q.BurstRequests = 10
		cfg.QuotaGroups["account"] = q
		filename := filepath.Join(t.TempDir(), "daemon.json")
		writeFixtureConfiguration(t, filename, cfg)
		d := startDaemonFixture(t, filename, fixtureControlToken)
		first := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "resource-create", map[string]string{"launch": "research", "goal": "artifact"}, 201)
		daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+first.ID+"/start", "resource-start", startSystemCommand{ExpectedRevision: 1}, 202)
		done := waitSystem(t, d, first.ID, func(s systemRecord) bool { return s.State == "inactive" })
		if done.Execution.Response != "resource tool complete" {
			d.stop(t, false)
			t.Fatalf("qualified tool failed: %+v %s", done.Execution, d.stderr.String())
		}
		crash := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "crash-create", map[string]string{"launch": "research", "goal": "crash"}, 201)
		daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+crash.ID+"/start", "crash-start", startSystemCommand{ExpectedRevision: 1}, 202)
		select {
		case <-dispatched:
		case <-time.After(10 * time.Second):
			t.Fatal("qualified operator was not dispatched")
		}
		path, err := activeResourceGroup(group)
		if err != nil {
			t.Fatal(err)
		}
		d.stop(t, true)
		deadline := time.Now().Add(5 * time.Second)
		for {
			populated, err := resourceCounter(filepath.Join(path, "cgroup.events"), "populated")
			if err != nil {
				t.Fatal(err)
			}
			if populated == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("worker survived real daemon death")
			}
			time.Sleep(10 * time.Millisecond)
		}
		restarted := startDaemonFixture(t, filename, fixtureControlToken)
		saved := daemonSystem(t, restarted, fixtureControlToken, "GET", "/v1/systems/"+crash.ID, "", nil, 200)
		if saved.State != "stopped" || saved.Execution.State != "unknown" || saved.ReservedTokens == 0 || count.Load() != 3 {
			t.Fatalf("crash lost conservative accounting: %+v calls=%d", saved.Execution, count.Load())
		}
		status, data := daemonRequest(t, restarted, fixtureControlToken, "GET", "/v1/systems/"+first.ID+"/artifacts", "", nil)
		var reports struct {
			Artifacts []json.RawMessage `json:"artifacts"`
		}
		if err := json.Unmarshal(data, &reports); err != nil || status != 200 || len(reports.Artifacts) != 1 {
			t.Fatalf("private tmpfs artifact lost: %s %v", data, err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("real daemon restart did not clean resource group")
		}
		workspaces, err := os.ReadDir(cfg.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range workspaces {
			if workspaceName.MatchString(entry.Name()) {
				t.Fatalf("abandoned workspace retained: %s", entry.Name())
			}
		}
	})
	entries, err := os.ReadDir(group)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "worker_") {
			t.Fatalf("resource group leaked: %s", entry.Name())
		}
	}
}
