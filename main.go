//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	nono "github.com/nolabs-ai/nono-go"
)

const maxFrame = 64 * 1024

type message struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"run"}
	}
	switch args[0] {
	case "run":
		flags := flag.NewFlagSet("microoperator run", flag.ContinueOnError)
		timeout := flags.Duration("timeout", 5*time.Second, "worker lifetime limit")
		data := flags.String("message", "ping", "text to echo through the confined worker")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *timeout <= 0 {
			return errors.New("run requires a positive timeout and no positional arguments")
		}
		ctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		return demo(ctx, *data)
	case "sandbox-exec":
		if len(args) < 3 {
			return errors.New("internal usage: sandbox-exec WORKSPACE EXECUTABLE [ARGS...]")
		}
		return sandboxExec(args[1], args[2], args[3:])
	case "worker":
		if len(args) != 1 {
			return errors.New("worker accepts no arguments")
		}
		return worker(os.Stdin, os.Stdout)
	default:
		return errors.New("usage: microoperator run [-timeout 5s] [-message ping]")
	}
}

func demo(ctx context.Context, data string) (err error) {
	fmt.Fprintln(os.Stderr, "Diagnostic spike only: confinement is not fully qualified; do not use for untrusted code. See README.md.")
	root, err := os.MkdirTemp("", "microoperator-")
	if err != nil {
		return err
	}
	defer func(path string) { err = errors.Join(err, os.RemoveAll(path)) }(root)
	for _, name := range []string{"inputs", "scratch", "output"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			return err
		}
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	var reply message
	if err := supervise(ctx, executable, root, executable, []string{"worker"},
		func(in io.Writer, out io.Reader) error {
			reader := bufio.NewReaderSize(out, maxFrame+1)
			ready, err := readMessage(reader)
			if err != nil {
				return fmt.Errorf("worker readiness: %w", err)
			}
			if ready.Type != "ready" {
				return fmt.Errorf("unexpected readiness message %q", ready.Type)
			}
			if err := writeMessage(in, message{Type: "ping", Data: data}); err != nil {
				return err
			}
			reply, err = readMessage(reader)
			if err != nil {
				return err
			}
			if reply.Type != "pong" || reply.Data != data {
				return errors.New("worker returned an unexpected reply")
			}
			return nil
		}); err != nil {
		return err
	}
	return writeMessage(os.Stdout, reply)
}

func worker(in io.Reader, out io.Writer) error {
	if err := writeMessage(out, message{Type: "ready"}); err != nil {
		return err
	}
	request, err := readMessage(bufio.NewReaderSize(in, maxFrame+1))
	if err != nil {
		return err
	}
	if request.Type != "ping" {
		return fmt.Errorf("unsupported worker operation %q", request.Type)
	}
	return writeMessage(out, message{Type: "pong", Data: request.Data})
}

func readMessage(reader *bufio.Reader) (message, error) {
	var value message
	frame, err := reader.ReadSlice('\n')
	if err != nil {
		return value, fmt.Errorf("read JSON frame (limit %d bytes): %w", maxFrame, err)
	}
	if len(frame) > maxFrame {
		return value, errors.New("JSON frame exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(frame))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("decode JSON frame: %w", err)
	}
	if value.Type == "" {
		return value, errors.New("JSON frame requires a type")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return value, errors.New("JSON frame must contain exactly one object")
	}
	return value, nil
}

func writeMessage(writer io.Writer, value message) error {
	frame, err := json.Marshal(value)
	if err != nil {
		return err
	}
	frame = append(frame, '\n')
	if len(frame) > maxFrame {
		return errors.New("JSON frame exceeds size limit")
	}
	n, err := writer.Write(frame)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	return err
}

// supportedSandboxPlatform limits launch to platforms with an implemented
// diagnostic profile. It is separate from nono.IsSupported: a kernel can support
// nono while still lacking controls our worker profile needs.
//
// Linux is currently amd64-only because the supplemental seccomp filter checks
// that syscall ABI and uses its syscall numbers. Enabling another architecture
// requires adapting and verifying that filter, not just changing this boolean.
// A true result permits the diagnostic spike, not arbitrary untrusted agent code.
func supportedSandboxPlatform() bool {
	// ponytail: qualify additional Linux architectures with real denial tests before enabling them.
	return runtime.GOOS == "darwin" || (runtime.GOOS == "linux" && runtime.GOARCH == "amd64")
}

// sandboxExec turns a fresh launcher process into a confined worker or tool.
// The supervisor starts this child; the daemon must never sandbox itself, since
// it still needs its database, credentials, and approved network access.
//
// Setup first validates paths and builds the nono capability set without applying
// restrictions. It then arranges descriptor closure, locks the calling goroutine
// to its OS thread, installs the platform filter, applies nono, and execs target.
// Exec replaces the launcher program in the same process; it does not start an
// unconstrained worker beside it. Only the replacement program runs worker logic.
//
// Ordering matters: native restrictions attach to OS execution state, not to a Go
// goroutine's identity. Applying them on one thread and executing the worker on
// another could lose confinement. No failure path is allowed to continue to exec.
func sandboxExec(root, target string, args []string) error {
	if !supportedSandboxPlatform() || !nono.IsSupported() {
		return errors.New("this sandbox spike requires supported macOS or Linux/amd64 confinement")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	if !filepath.IsAbs(root) || !filepath.IsAbs(target) {
		return errors.New("workspace and executable must be absolute paths")
	}
	caps := nono.New()
	defer caps.Close()
	if err := caps.AllowFile(target, nono.AccessRead); err != nil {
		return fmt.Errorf("grant executable: %w", err)
	}
	for _, name := range []string{"inputs", "scratch", "output"} {
		path := filepath.Join(root, name)
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", name, err)
		}
		if resolved != path {
			return fmt.Errorf("workspace directory %s must not be a symlink", name)
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("workspace entry %s is not a directory", name)
		}
		access := nono.AccessReadWrite
		if name == "inputs" {
			access = nono.AccessRead
		}
		if err := caps.AllowPath(path, access); err != nil {
			return fmt.Errorf("grant %s: %w", name, err)
		}
	}
	if err := configurePlatformSandbox(caps); err != nil {
		return err
	}
	for _, path := range []string{"/dev/urandom", "/dev/random"} {
		if err := caps.AllowFile(path, nono.AccessRead); err != nil {
			return fmt.Errorf("grant runtime file %s: %w", path, err)
		}
	}
	if err := caps.AllowFile("/dev/null", nono.AccessReadWrite); err != nil {
		return err
	}
	if err := caps.SetNetworkMode(nono.NetworkBlocked); err != nil {
		return err
	}
	if err := closeOnExecDescriptors(); err != nil {
		return err
	}
	runtime.LockOSThread()
	// Deliberately do not defer UnlockOSThread. Successful exec never returns;
	// failure returns to main only to report the error and exit this child.
	// No worker code may run between installing restrictions and exec.
	if err := applyPlatformSandbox(); err != nil {
		return fmt.Errorf("apply platform sandbox: %w", err)
	}
	if err := nono.Apply(caps); err != nil {
		return fmt.Errorf("apply nono sandbox: %w", err)
	}
	if err := syscall.Exec(target, append([]string{target}, args...), workerEnv(root)); err != nil {
		return fmt.Errorf("exec confined worker: %w", err)
	}
	return nil
}

// closeOnExecDescriptors prevents handles opened before confinement from becoming
// a second route out of the worker. Blocking new socket creation is insufficient
// if a worker inherits an already-connected socket and can simply read/write it.
//
// Descriptors 0, 1, and 2 are stdin, stdout, and stderr. The supervisor chooses the
// worker's communication pipes; this function additionally rejects sockets in
// those reserved slots rather than silently preserving a connection.
//
// Other descriptors are marked FD_CLOEXEC: the kernel closes them when exec
// succeeds. We do not close them immediately, because the still-running launcher
// and Go runtime may need some of them during setup. This helper must run before
// nono.Apply, while the launcher can still inspect its descriptor directory.
func closeOnExecDescriptors() error {
	for fd := 0; fd < 3; fd++ {
		var stat syscall.Stat_t
		if err := syscall.Fstat(fd, &stat); err != nil {
			return fmt.Errorf("inspect standard descriptor %d: %w", fd, err)
		}
		if stat.Mode&syscall.S_IFMT == syscall.S_IFSOCK {
			return fmt.Errorf("standard descriptor %d must not be a socket", fd)
		}
	}
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return fmt.Errorf("enumerate inherited descriptors: %w", err)
	}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			return fmt.Errorf("invalid descriptor name: %w", err)
		}
		if fd < 3 {
			continue
		}
		// The directory listing is a snapshot. A descriptor, including the one
		// used to enumerate /dev/fd, may already have closed by this point.
		// EBADF means there is nothing left to inherit; other failures abort.
		_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFD, syscall.FD_CLOEXEC)
		if errno != 0 && errno != syscall.EBADF {
			return fmt.Errorf("mark descriptor %d close-on-exec: %w", fd, errno)
		}
	}
	return nil
}

func workerEnv(root string) []string {
	return []string{
		"HOME=" + filepath.Join(root, "scratch"),
		"TMPDIR=" + filepath.Join(root, "scratch"),
		"LANG=C",
		"TZ=UTC",
	}
}

type boundedStderr struct {
	bytes.Buffer
}

func (b *boundedStderr) Write(p []byte) (int, error) {
	if len(p) > maxFrame-b.Len() {
		n, _ := b.Buffer.Write(p[:maxFrame-b.Len()])
		return n, errors.New("worker stderr exceeds size limit")
	}
	return b.Buffer.Write(p)
}

func killGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func supervise(ctx context.Context, launcher, root, target string, args []string,
	communicate func(io.Writer, io.Reader) error,
) (err error) {
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("worker supervision requires a deadline")
	}
	inRead, inWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	defer inRead.Close()
	defer inWrite.Close()
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	defer outRead.Close()
	defer outWrite.Close()
	var stderr boundedStderr
	cmd := exec.CommandContext(ctx, launcher, append([]string{"sandbox-exec", root, target}, args...)...)
	cmd.Dir, cmd.Env = root, workerEnv(root)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inRead, outWrite, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 250 * time.Millisecond
	cmd.Cancel = func() error {
		if err := killGroup(cmd.Process.Pid); err != nil {
			return err
		}
		return nil
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sandbox launcher: %w", err)
	}
	defer func() { err = errors.Join(err, killGroup(cmd.Process.Pid)) }()
	stopIO := context.AfterFunc(ctx, func() {
		inWrite.Close()
		outRead.Close()
	})
	defer stopIO()
	inRead.Close()
	outWrite.Close()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	communicationErr := communicate(inWrite, outRead)
	inWrite.Close()
	if communicationErr != nil {
		err = killGroup(cmd.Process.Pid)
	}
	waitErr := <-waited
	err = errors.Join(err, communicationErr, waitErr, ctx.Err())
	if err != nil && stderr.Len() != 0 {
		err = fmt.Errorf("%w; worker stderr: %s", err, stderr.String())
	}
	return err
}
