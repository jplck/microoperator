//go:build integration && (darwin || linux)

package daemon

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
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jplck/microoperator/internal/protocol"
	"github.com/jplck/microoperator/internal/sandbox"
	nono "github.com/nolabs-ai/nono-go"
)

var (
	probeMode           = flag.String("sandbox-probe", "", "internal confined test operation")
	probeValue          = flag.String("sandbox-value", "", "internal test fixture")
	microoperatorBinary string
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
	if !sandbox.Supported() || !nono.IsSupported() {
		fmt.Fprintln(os.Stderr, "integration preflight: supported macOS or Linux/amd64 confinement required")
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
	microoperatorBinary = filepath.Join(root, "microoperator")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	buildArgs := []string{"build", "-o", microoperatorBinary}
	raceEnabled := false
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "-race" && setting.Value == "true" {
				raceEnabled = true
				buildArgs = append(buildArgs, "-race")
			}
		}
	}
	buildArgs = append(buildArgs, filepath.Join("..", ".."))
	cmd := exec.CommandContext(ctx, "go", buildArgs...)
	if output, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build runtime: %v\n%s", err, output)
		return 1
	}
	fmt.Printf("sandbox integration: %s/%s, nono FFI reports %s, application race detector: %t\n", runtime.GOOS, runtime.GOARCH, nono.Version(), raceEnabled)
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

func exchange(in io.Writer, out io.Reader) (protocol.Message, error) {
	reader := bufio.NewReaderSize(out, protocol.MaxFrame+1)
	ready, err := protocol.ReadMessage(reader)
	if err != nil {
		return protocol.Message{}, fmt.Errorf("readiness: %w", err)
	}
	if ready.Type != "ready" {
		return protocol.Message{}, fmt.Errorf("unexpected readiness %+v", ready)
	}
	if err := protocol.WriteMessage(in, protocol.Message{Type: "probe"}); err != nil {
		return protocol.Message{}, err
	}
	return protocol.ReadMessage(reader)
}

func confinedProbe(t *testing.T, root, mode, value string) protocol.Message {
	t.Helper()
	target, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var reply protocol.Message
	err = sandbox.Supervise(ctx, microoperatorBinary, root, target,
		[]string{"-sandbox-probe=" + mode, "-sandbox-value=" + value},
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

// TestSandboxBoundary checks actual OS behavior after the real launcher applies
// confinement and execs a fresh probe worker. It does not ask a permission-preview
// API what should happen. Temporary files and local listeners let us check both
// allowed operations and denials without touching private data or real services.
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
	type boundaryCase struct {
		name, mode, value, wantType, wantData string
	}
	cases := []boundaryCase{
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
		{"new threads inherit", "threads", outside, "denied", ""},
		{"descendant inherits", "descendant", outside, "denied", ""},
		{"clean environment", "env", "MICROOPERATOR_TEST_SECRET", "result", ""},
	}
	listeners := []net.Listener{tcp, unix}
	if runtime.GOOS == "linux" {
		outsideUnix, err := net.Listen("unix", filepath.Join(t.TempDir(), "socket"))
		if err != nil {
			t.Fatal(err)
		}
		defer outsideUnix.Close()
		// Linux abstract Unix sockets live in a kernel namespace, not at a file
		// path. A filesystem allowlist alone cannot exclude this communication.
		abstractUnix, err := net.Listen("unix", "@microoperator-"+filepath.Base(root))
		if err != nil {
			t.Fatal(err)
		}
		defer abstractUnix.Close()
		listeners = append(listeners, outsideUnix, abstractUnix)
		cases = append(cases,
			boundaryCase{"deny outside unix", "unix", outsideUnix.Addr().String(), "denied", ""},
			boundaryCase{"deny abstract unix", "unix", abstractUnix.Addr().String(), "denied", ""},
			boundaryCase{"deny socket creation", "socket", "", "denied", ""},
			boundaryCase{"deny socket pairs", "socketpair", "", "denied", ""},
			boundaryCase{"new threads inherit socket filter", "threads-socket", "", "denied", ""},
			boundaryCase{"descendants inherit socket filter", "descendant-socket", "", "denied", ""},
		)
		for name, number := range map[string]int{
			"io_uring setup": 425, "io_uring enter": 426, "io_uring register": 427,
			"pidfd_getfd": 438, "x32 syscall": 0x40000000 | syscall.SYS_GETPID,
		} {
			cases = append(cases, boundaryCase{
				"deny " + name, "syscall", strconv.Itoa(number), "denied", "",
			})
		}
	} else {
		// Characterize the pinned macOS resolver exception, not strict network isolation.
		cases = append(cases, boundaryCase{
			"known resolver socket allowance", "socket", "", "result", "created",
		})
	}
	// A denied connection is only meaningful if the endpoint works. These
	// unsandboxed positive controls establish that the listeners are reachable;
	// worker probes must then return permission errors, not connection refused.
	for _, listener := range listeners {
		conn, err := net.DialTimeout(listener.Addr().Network(), listener.Addr().String(), time.Second)
		if err != nil {
			t.Fatalf("listener positive control: %v", err)
		}
		accepted, err := listener.Accept()
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		if err := errors.Join(conn.Close(), accepted.Close()); err != nil {
			t.Fatal(err)
		}
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
	fixture, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"missing inputs", "symlinked inputs", "nested launch"} {
		t.Run(mode, func(t *testing.T) {
			root := workspace(t)
			target := fixture
			args := []string{"-sandbox-probe=pipe"}
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
				target = microoperatorBinary
				args = []string{"sandbox-exec", root, microoperatorBinary, "daemon", "--config", filepath.Join(root, "inputs", "unused.json")}
				want = "enumerate inherited descriptors"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var reply protocol.Message
			err := sandbox.Supervise(ctx, microoperatorBinary, root, target, args,
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

// TestInheritedDescriptorIsClosed deliberately gives the launcher an open file
// and an already-connected socket. exec.Cmd.ExtraFiles places the selected handle
// at descriptor 3 in the child. The worker must not retain it after the launcher's
// exec, even though no new open/socket call would be needed to use that handle.
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
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	socket := os.NewFile(uintptr(fds[0]), "socket fixture")
	peer := os.NewFile(uintptr(fds[1]), "socket peer")
	defer socket.Close()
	defer peer.Close()
	target, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mode string
		file *os.File
	}{{"fd", secret}, {"socket-fd", socket}} {
		t.Run(tc.mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, microoperatorBinary, "sandbox-exec", root, target, "-sandbox-probe="+tc.mode)
			cmd.Env, cmd.Dir = sandbox.Environment(root), root
			cmd.ExtraFiles = []*os.File{tc.file}
			cmd.Stdin = strings.NewReader("{\"type\":\"probe\"}\n")
			output, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReaderSize(bytes.NewReader(output), protocol.MaxFrame+1)
			if _, err := protocol.ReadMessage(reader); err != nil {
				t.Fatal(err)
			}
			reply, err := protocol.ReadMessage(reader)
			if err != nil || reply != (protocol.Message{Type: "result", Data: "not inherited"}) {
				t.Fatalf("descriptor isolation: %+v, %v", reply, err)
			}
		})
	}
}

// TestSocketStandardInputIsRejected covers the exception to closing descriptors
// above 2: stdin must survive exec for normal IPC, but must not hide a socket.
// Supply a real connected socket as stdin and require launch failure before any
// worker readiness message. Rejecting setup is safer than silently keeping the
// connection or closing stdin and starting a worker with broken communication.
func TestSocketStandardInputIsRejected(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	socket := os.NewFile(uintptr(fds[0]), "socket fixture")
	peer := os.NewFile(uintptr(fds[1]), "socket peer")
	defer socket.Close()
	defer peer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := workspace(t)
	target, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, microoperatorBinary, "sandbox-exec", root, target, "-sandbox-probe=pipe")
	cmd.Env, cmd.Dir, cmd.Stdin = sandbox.Environment(root), root, socket
	output, err := cmd.Output()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || len(output) != 0 ||
		!strings.Contains(string(exitErr.Stderr), "standard descriptor 0 must not be a socket") {
		t.Fatalf("socket stdin was not rejected before readiness: output=%q, err=%v", output, err)
	}
}

func TestSandboxSupervision(t *testing.T) {
	target, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("pipe communication", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := sandbox.Supervise(ctx, microoperatorBinary, workspace(t), target, []string{"-sandbox-probe=pipe"},
			func(in io.Writer, out io.Reader) error {
				reply, err := exchange(in, out)
				if err == nil && reply != (protocol.Message{Type: "result", Data: "complete"}) {
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
		err := sandbox.Supervise(ctx, microoperatorBinary, workspace(t), target, []string{"-sandbox-probe=pipe"},
			func(_ io.Writer, out io.Reader) error {
				reader := bufio.NewReaderSize(out, protocol.MaxFrame+1)
				ready, err := protocol.ReadMessage(reader)
				readySeen = err == nil && ready.Type == "ready"
				if err != nil {
					return err
				}
				_, err = protocol.ReadMessage(reader)
				return err
			})
		if !readySeen || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline did not terminate ready worker: ready=%v, err=%v", readySeen, err)
		}
	})
	t.Run("cancel process group", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var pids []int
		err := sandbox.Supervise(ctx, microoperatorBinary, workspace(t), target, []string{"-sandbox-probe=tree"},
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

// probe is a subprocess fixture compiled only into the integration test binary.
// The production executable provides confinement, not a placeholder agent loop.
func probe() error {
	if *probeMode == "sleep" {
		time.Sleep(time.Hour)
		return nil
	}
	if err := protocol.WriteMessage(os.Stdout, protocol.Message{Type: "ready"}); err != nil {
		return err
	}
	request, err := protocol.ReadMessage(bufio.NewReaderSize(os.Stdin, protocol.MaxFrame+1))
	if err != nil {
		return err
	}
	if request.Type != "probe" {
		return fmt.Errorf("unexpected fixture request %q", request.Type)
	}
	if *probeMode == "tree" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		child := exec.Command(executable, "-sandbox-probe=sleep")
		child.Env, child.Stderr = os.Environ(), os.Stderr
		if err := child.Start(); err != nil {
			return err
		}
		if err := protocol.WriteMessage(os.Stdout, protocol.Message{
			Type: "children", Data: fmt.Sprintf("%d,%d", os.Getpid(), child.Process.Pid),
		}); err != nil {
			child.Process.Kill()
			child.Wait()
			return err
		}
		return child.Wait()
	}
	data, err := probeOperation(*probeMode, *probeValue)
	reply := protocol.Message{Type: "result", Data: data}
	if err != nil {
		reply.Type, reply.Data = "error", err.Error()
		if errors.Is(err, os.ErrPermission) {
			reply.Type = "denied"
		}
	}
	return protocol.WriteMessage(os.Stdout, reply)
}

func probeOperation(mode, value string) (string, error) {
	switch mode {
	case "pipe":
		return "complete", nil
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
	case "socketpair":
		fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
		if err != nil {
			return "", err
		}
		return "created", errors.Join(syscall.Close(fds[0]), syscall.Close(fds[1]))
	case "syscall":
		// Exercise low-level entry points directly, without allocating real
		// io_uring resources or attempting to copy another process's handles.
		// Unexpected errors remain test errors, not successful sandbox denials.
		number, err := strconv.Atoi(value)
		if err != nil {
			return "", err
		}
		_, _, errno := syscall.RawSyscall(uintptr(number), 0, 0, 0)
		if errno != 0 {
			return "", errno
		}
		return "syscall succeeded", nil
	case "threads", "threads-socket":
		operation := "read"
		if mode == "threads-socket" {
			operation = "socket"
		}
		start := make(chan struct{})
		results := make(chan error, 4)
		var ready sync.WaitGroup
		ready.Add(4)
		for i := 0; i < 4; i++ {
			go func() {
				// Hold four OS threads at a barrier so this tests more than
				// several goroutines taking turns on the same confined thread.
				runtime.LockOSThread()
				defer runtime.UnlockOSThread()
				ready.Done()
				<-start
				_, err := probeOperation(operation, value)
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
	case "descendant", "descendant-socket":
		executable, err := os.Executable()
		if err != nil {
			return "", err
		}
		operation := "read"
		if mode == "descendant-socket" {
			operation = "socket"
		}
		cmd := exec.Command(executable, "-sandbox-probe="+operation, "-sandbox-value="+value)
		cmd.Stdin = strings.NewReader("{\"type\":\"probe\"}\n")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		output, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("descendant: %w: %s", err, stderr.String())
		}
		reader := bufio.NewReaderSize(bytes.NewReader(output), protocol.MaxFrame+1)
		if _, err := protocol.ReadMessage(reader); err != nil {
			return "", err
		}
		reply, err := protocol.ReadMessage(reader)
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
	case "socket-fd":
		_, err := syscall.GetsockoptInt(3, syscall.SOL_SOCKET, syscall.SO_TYPE)
		if errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.ENOTSOCK) {
			return "not inherited", nil
		}
		if err != nil {
			return "", err
		}
		return "inherited socket descriptor", nil
	default:
		return platformResourceProbe(mode, value)
	}
}
