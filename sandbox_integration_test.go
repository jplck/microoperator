//go:build integration && (darwin || linux)

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	nono "github.com/nolabs-ai/nono-go"
)

var (
	probeMode   = flag.String("spike-probe", "", "internal confined test operation")
	probeValue  = flag.String("spike-value", "", "internal test fixture")
	spikeBinary string
)

func TestMain(m *testing.M) { os.Exit(integrationMain(m)) }

func integrationMain(m *testing.M) int {
	flag.Parse()
	if *probeMode != "" {
		if err := probe(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if runtime.GOOS != "darwin" || !nono.IsSupported() {
		fmt.Fprintln(os.Stderr, "integration preflight: this spike requires macOS confinement")
		return 1
	}
	root, err := os.MkdirTemp("", "microoperator-build-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(root)
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	spikeBinary = filepath.Join(root, "microoperator")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", spikeBinary, ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build spike: %v\n%s", err, output)
		return 1
	}
	fmt.Printf("sandbox integration: %s/%s, nono FFI reports %s\n", runtime.GOOS, runtime.GOARCH, nono.Version())
	return m.Run()
}

func workspace(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "mo-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	for _, name := range []string{"inputs", "scratch", "output"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func exchange(in io.Writer, out io.Reader) (message, error) {
	reader := bufio.NewReaderSize(out, maxFrame+1)
	ready, err := readMessage(reader)
	if err != nil {
		return message{}, fmt.Errorf("readiness: %w", err)
	}
	if ready.Type != "ready" {
		return message{}, fmt.Errorf("unexpected readiness %+v", ready)
	}
	if err := writeMessage(in, message{Type: "ping", Data: "hello"}); err != nil {
		return message{}, err
	}
	return readMessage(reader)
}

func confinedProbe(t *testing.T, root, mode, value string) message {
	t.Helper()
	target, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var reply message
	err = supervise(ctx, spikeBinary, root, target,
		[]string{"-spike-probe=" + mode, "-spike-value=" + value},
		func(in io.Writer, out io.Reader) error {
			var err error
			reply, err = exchange(in, out)
			return err
		})
	if err != nil {
		t.Fatal(err)
	}
	return reply
}

func TestSandboxBoundary(t *testing.T) {
	root := workspace(t)
	input := filepath.Join(root, "inputs", "note")
	outside := filepath.Join(t.TempDir(), "outside")
	for _, path := range []string{input, outside} {
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MICROOPERATOR_TEST_SECRET", "must-not-reach-worker")
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	unixPath := filepath.Join(root, "scratch", "s")
	unix, err := net.Listen("unix", unixPath)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close()
	link := filepath.Join(root, "scratch", "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, mode, value, wantType, wantData string
	}{
		{"read inputs", "read", input, "result", "fixture"},
		{"write scratch", "write", filepath.Join(root, "scratch", "written"), "result", "written"},
		{"write output", "write", filepath.Join(root, "output", "written"), "result", "written"},
		{"deny input write", "write", input, "denied", ""},
		{"deny outside read", "read", outside, "denied", ""},
		{"deny outside write", "write", outside, "denied", ""},
		{"deny symlink escape", "read", link, "denied", ""},
		{"deny tcp", "tcp", tcp.Addr().String(), "denied", ""},
		{"deny udp", "udp", udp.LocalAddr().String(), "denied", ""},
		{"deny unix", "unix", unixPath, "denied", ""},
		// Characterize the pinned core's resolver exception; this is not strict network isolation.
		{"known resolver socket allowance", "socket", "", "result", "created"},
		{"new threads inherit", "threads", outside, "denied", ""},
		{"descendant inherits", "descendant", outside, "denied", ""},
		{"clean environment", "env", "MICROOPERATOR_TEST_SECRET", "result", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply := confinedProbe(t, root, tc.mode, tc.value)
			if reply.Type != tc.wantType || (tc.wantType == "result" && reply.Data != tc.wantData) {
				t.Fatalf("reply = %+v; want %s %q", reply, tc.wantType, tc.wantData)
			}
			if tc.mode == "write" && tc.wantType == "result" {
				data, err := os.ReadFile(tc.value)
				if err != nil || string(data) != tc.wantData {
					t.Fatalf("allowed write missing: %q, %v", data, err)
				}
			}
		})
	}
	for _, path := range []string{input, outside} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "fixture" {
			t.Fatalf("protected fixture changed: %s: %q, %v", path, data, err)
		}
	}
}

func TestLaunchFailureDoesNotRunWorker(t *testing.T) {
	for _, mode := range []string{"missing inputs", "symlinked inputs", "nested launch"} {
		t.Run(mode, func(t *testing.T) {
			root := workspace(t)
			args := []string{"worker"}
			want := "resolve inputs"
			switch mode {
			case "missing inputs":
				if err := os.Remove(filepath.Join(root, "inputs")); err != nil {
					t.Fatal(err)
				}
			case "symlinked inputs":
				if err := os.Remove(filepath.Join(root, "inputs")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), filepath.Join(root, "inputs")); err != nil {
					t.Fatal(err)
				}
				want = "must not be a symlink"
			case "nested launch":
				args = []string{"sandbox-exec", root, spikeBinary, "worker"}
				want = "enumerate inherited descriptors"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var reply message
			err := supervise(ctx, spikeBinary, root, spikeBinary, args,
				func(in io.Writer, out io.Reader) error {
					var err error
					reply, err = exchange(in, out)
					return err
				})
			if err == nil || !strings.Contains(err.Error(), want) || reply.Type != "" {
				t.Fatalf("fail-closed launch: reply=%+v, error=%v", reply, err)
			}
		})
	}
}

func TestInheritedDescriptorIsClosed(t *testing.T) {
	root := workspace(t)
	secret, err := os.CreateTemp(t.TempDir(), "fixture-")
	if err != nil {
		t.Fatal(err)
	}
	defer secret.Close()
	if _, err := secret.WriteString("descriptor fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := secret.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	target, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, spikeBinary, "sandbox-exec", root, target, "-spike-probe=fd")
	cmd.Env, cmd.Dir = workerEnv(root), root
	cmd.ExtraFiles = []*os.File{secret}
	cmd.Stdin = strings.NewReader("{\"type\":\"ping\"}\n")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReaderSize(bytes.NewReader(output), maxFrame+1)
	if _, err := readMessage(reader); err != nil {
		t.Fatal(err)
	}
	reply, err := readMessage(reader)
	if err != nil || reply != (message{Type: "result", Data: "not inherited"}) {
		t.Fatalf("descriptor isolation: %+v, %v", reply, err)
	}
}

func TestWorkerDeadlineAndCancellation(t *testing.T) {
	t.Run("real worker ping", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := supervise(ctx, spikeBinary, workspace(t), spikeBinary, []string{"worker"},
			func(in io.Writer, out io.Reader) error {
				reply, err := exchange(in, out)
				if err == nil && reply != (message{Type: "pong", Data: "hello"}) {
					err = fmt.Errorf("unexpected reply %+v", reply)
				}
				return err
			})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		readySeen := false
		err := supervise(ctx, spikeBinary, workspace(t), spikeBinary, []string{"worker"},
			func(_ io.Writer, out io.Reader) error {
				reader := bufio.NewReaderSize(out, maxFrame+1)
				ready, err := readMessage(reader)
				readySeen = err == nil && ready.Type == "ready"
				if err != nil {
					return err
				}
				_, err = readMessage(reader)
				return err
			})
		if !readySeen || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline did not terminate ready worker: ready=%v, err=%v", readySeen, err)
		}
	})
	t.Run("cancel process group", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		target, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		var pids []int
		err = supervise(ctx, spikeBinary, workspace(t), target, []string{"-spike-probe=tree"},
			func(in io.Writer, out io.Reader) error {
				reply, err := exchange(in, out)
				if err != nil {
					return err
				}
				if reply.Type != "children" {
					return fmt.Errorf("unexpected tree reply %+v", reply)
				}
				for _, field := range strings.Split(reply.Data, ",") {
					pid, err := strconv.Atoi(field)
					if err != nil || pid <= 0 {
						return fmt.Errorf("invalid child PID %q", field)
					}
					pids = append(pids, pid)
				}
				cancel()
				_, err = io.Copy(io.Discard, out)
				return err
			})
		if !errors.Is(err, context.Canceled) || len(pids) != 2 {
			t.Fatalf("cancel result: pids=%v, err=%v", pids, err)
		}
		for _, pid := range pids {
			deadline := time.Now().Add(2 * time.Second)
			for {
				err := syscall.Kill(pid, 0)
				if errors.Is(err, syscall.ESRCH) {
					break
				}
				if time.Now().After(deadline) {
					syscall.Kill(pid, syscall.SIGKILL)
					t.Fatalf("worker process %d survived cancellation: %v", pid, err)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	})
}

func probe() error {
	if *probeMode == "sleep" {
		time.Sleep(time.Hour)
		return nil
	}
	if err := writeMessage(os.Stdout, message{Type: "ready"}); err != nil {
		return err
	}
	if _, err := readMessage(bufio.NewReaderSize(os.Stdin, maxFrame+1)); err != nil {
		return err
	}
	if *probeMode == "tree" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		child := exec.Command(executable, "-spike-probe=sleep")
		child.Env, child.Stderr = os.Environ(), os.Stderr
		if err := child.Start(); err != nil {
			return err
		}
		if err := writeMessage(os.Stdout, message{
			Type: "children", Data: fmt.Sprintf("%d,%d", os.Getpid(), child.Process.Pid),
		}); err != nil {
			child.Process.Kill()
			child.Wait()
			return err
		}
		return child.Wait()
	}
	data, err := probeOperation(*probeMode, *probeValue)
	reply := message{Type: "result", Data: data}
	if err != nil {
		reply.Type, reply.Data = "error", err.Error()
		if errors.Is(err, os.ErrPermission) {
			reply.Type = "denied"
		}
	}
	return writeMessage(os.Stdout, reply)
}

func probeOperation(mode, value string) (string, error) {
	switch mode {
	case "read":
		data, err := os.ReadFile(value)
		return string(data), err
	case "write":
		return "written", os.WriteFile(value, []byte("written"), 0600)
	case "env":
		return os.Getenv(value), nil
	case "tcp", "udp", "unix":
		conn, err := net.DialTimeout(mode, value, 500*time.Millisecond)
		if err != nil {
			return "", err
		}
		return "connected", conn.Close()
	case "socket":
		fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
		if err != nil {
			return "", err
		}
		return "created", syscall.Close(fd)
	case "threads":
		start := make(chan struct{})
		results := make(chan error, 4)
		var ready sync.WaitGroup
		ready.Add(4)
		for i := 0; i < 4; i++ {
			go func() {
				runtime.LockOSThread()
				defer runtime.UnlockOSThread()
				ready.Done()
				<-start
				_, err := os.ReadFile(value)
				results <- err
			}()
		}
		ready.Wait()
		close(start)
		for i := 0; i < 4; i++ {
			if err := <-results; !errors.Is(err, os.ErrPermission) {
				return "thread access was not denied", err
			}
		}
		return "", os.ErrPermission
	case "descendant":
		executable, err := os.Executable()
		if err != nil {
			return "", err
		}
		cmd := exec.Command(executable, "-spike-probe=read", "-spike-value="+value)
		cmd.Stdin = strings.NewReader("{\"type\":\"ping\"}\n")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		output, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("descendant: %w: %s", err, stderr.String())
		}
		reader := bufio.NewReaderSize(bytes.NewReader(output), maxFrame+1)
		if _, err := readMessage(reader); err != nil {
			return "", err
		}
		reply, err := readMessage(reader)
		if err != nil {
			return "", err
		}
		if reply.Type == "denied" {
			return "", os.ErrPermission
		}
		return fmt.Sprintf("descendant was not denied: %+v", reply), nil
	case "fd":
		var stat syscall.Stat_t
		err := syscall.Fstat(3, &stat)
		if errors.Is(err, syscall.EBADF) || (err == nil && stat.Mode&syscall.S_IFMT != syscall.S_IFREG) {
			return "not inherited", nil
		}
		if err != nil {
			return "", err
		}
		return "inherited regular file descriptor", nil
	default:
		return "", fmt.Errorf("unknown test probe %q", mode)
	}
}
