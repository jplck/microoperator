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
	ID   string `json:"id,omitempty"`
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
	const usage = "usage: microoperator daemon --config PATH | ui --socket PATH [--listen 127.0.0.1:8080] | toolchain-digest ABSOLUTE_GOROOT"
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "toolchain-digest":
		if len(args) != 2 {
			return errors.New("usage: microoperator toolchain-digest ABSOLUTE_GOROOT")
		}
		digest, err := toolchainDigest(ctx, args[1])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(os.Stdout, digest)
		return err
	case "ui":
		flags := flag.NewFlagSet("microoperator ui", flag.ContinueOnError)
		socket := flags.String("socket", "", "daemon Unix socket")
		listen := flags.String("listen", "127.0.0.1:8080", "loopback HTTP address")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *socket == "" {
			return errors.New("ui requires --socket PATH and no positional arguments")
		}
		return runUI(ctx, *socket, *listen, os.Stdout, os.Stderr)
	case "daemon":
		flags := flag.NewFlagSet("microoperator daemon", flag.ContinueOnError)
		config := flags.String("config", "", "user-owned JSON configuration")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *config == "" {
			return errors.New("daemon requires --config PATH and no positional arguments")
		}
		return runDaemon(ctx, *config, os.Stdout, os.Stderr)
	case "sandbox-exec":
		if len(args) < 3 {
			return errors.New("internal usage: sandbox-exec WORKSPACE EXECUTABLE [ARGS...]")
		}
		return sandboxExec(args[1], args[2], args[3:])
	case "sandbox-exec-profile", "sandbox-build-profile":
		if len(args) < 4 || len(args[3]) > maxFrame {
			return errors.New("internal usage: sandbox-exec-profile WORKSPACE EXECUTABLE PROFILE [ARGS...]")
		}
		var profile sandboxConfig
		if err := decodeJSON([]byte(args[3]), &profile); err != nil {
			return err
		}
		if args[0] == "sandbox-build-profile" {
			if len(args) < 5 || profile.Resources == nil {
				return errors.New("builder requires a resource profile and pinned toolchain")
			}
			profile.toolchain = args[4]
			args = append(args[:4:4], args[5:]...)
		}
		return sandboxExec(args[1], args[2], args[4:], profile)
	case "worker":
		if len(args) != 2 || args[1] != "operator" {
			return errors.New(usage)
		}
		return operatorWorker(os.Stdin, os.Stdout)
	case "tool":
		if len(args) != 2 || args[1] != "text-analyze" {
			return errors.New(usage)
		}
		return reviewedTextTool(os.Stdin, os.Stdout)
	default:
		return errors.New(usage)
	}
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
	if err := decodeJSON(frame, &value); err != nil {
		return value, fmt.Errorf("decode JSON frame: %w", err)
	}
	if value.Type == "" {
		return value, errors.New("JSON frame requires a type")
	}
	return value, nil
}

func checkFrame(value message) error { return writeMessage(io.Discard, value) }

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
// sandbox profile. It is separate from nono.IsSupported: a kernel can support
// nono while still lacking controls our worker profile needs.
//
// Linux is currently amd64-only because the supplemental seccomp filter checks
// that syscall ABI and uses its syscall numbers. Enabling another architecture
// requires adapting and verifying that filter, not just changing this boolean.
// A true result reports implemented support, not permission to run untrusted code.
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
func sandboxExec(root, target string, args []string, profiles ...sandboxConfig) error {
	profile := sandboxConfig{Read: []string{"inputs"}, ReadWrite: []string{"scratch", "output"}, Network: "blocked"}
	if len(profiles) > 1 {
		return errors.New("only one sandbox profile is allowed")
	}
	if len(profiles) == 1 {
		profile = profiles[0]
	}
	if err := validateSandbox("sandbox_profile", profile); err != nil {
		return err
	}
	if !supportedSandboxPlatform() || !nono.IsSupported() {
		return errors.New("sandbox launch requires supported macOS or Linux/amd64 confinement")
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
	runtime.LockOSThread()
	// Namespace setup and irreversible filters must remain on the exec thread.
	if err := prepareResourceWorkspace(root, profile); err != nil {
		return err
	}
	caps := nono.New()
	defer caps.Close()
	if err := caps.AllowFile(target, nono.AccessRead); err != nil {
		return fmt.Errorf("grant executable: %w", err)
	}
	if profile.toolchain != "" {
		if profile.Resources == nil || target != filepath.Join(profile.toolchain, "bin", "go") {
			return errors.New("toolchain grant is restricted to the confined Go builder")
		}
		if err := caps.AllowPath(profile.toolchain, nono.AccessRead); err != nil {
			return fmt.Errorf("grant pinned toolchain: %w", err)
		}
	}
	for _, grant := range []struct {
		paths  []string
		access nono.AccessMode
	}{
		{profile.Read, nono.AccessRead}, {profile.ReadWrite, nono.AccessReadWrite},
	} {
		for _, name := range grant.paths {
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
			if err := caps.AllowPath(path, grant.access); err != nil {
				return fmt.Errorf("grant %s: %w", name, err)
			}
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
	// Deliberately do not defer UnlockOSThread. Successful exec never returns;
	// failure returns to main only to report the error and exit this child.
	// No worker code may run between installing restrictions and exec.
	if err := applyPlatformSandbox(); err != nil {
		return fmt.Errorf("apply platform sandbox: %w", err)
	}
	if err := nono.Apply(caps); err != nil {
		return fmt.Errorf("apply nono sandbox: %w", err)
	}
	env := workerEnv(root)
	if profile.toolchain != "" {
		env = append(env, buildEnvironment(root, profile.toolchain)...)
	}
	if err := syscall.Exec(target, append([]string{target}, args...), env); err != nil {
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
	profiles ...sandboxConfig,
) (err error) {
	return superviseWorkspace(ctx, launcher, root, target, args, communicate, nil, profiles...)
}

func superviseWorkspace(ctx context.Context, launcher, root, target string, args []string,
	communicate func(io.Writer, io.Reader) error, inspect func(*os.Root) error, profiles ...sandboxConfig,
) (err error) {
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("worker supervision requires a deadline")
	}
	commandArgs := append([]string{"sandbox-exec", root, target}, args...)
	if len(profiles) > 1 {
		return errors.New("only one sandbox profile is allowed")
	}
	if len(profiles) == 1 {
		if err := validateSandbox("sandbox_profile", profiles[0]); err != nil {
			return err
		}
		data, err := json.Marshal(profiles[0])
		if err != nil {
			return err
		}
		if len(data) > maxFrame {
			return errors.New("sandbox profile exceeds launch limit")
		}
		commandArgs = append([]string{"sandbox-exec-profile", root, target, string(data)}, args...)
		if profiles[0].toolchain != "" {
			commandArgs = append([]string{"sandbox-build-profile", root, target, string(data), profiles[0].toolchain}, args...)
		}
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
	cmd := exec.CommandContext(ctx, launcher, commandArgs...)
	cmd.Dir, cmd.Env = root, workerEnv(root)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inRead, outWrite, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	profile := sandboxConfig{}
	if len(profiles) == 1 {
		profile = profiles[0]
	}
	cleanup, err := prepareResourceProcess(cmd, profile)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, cleanup()) }()
	if profile.Resources != nil {
		// PR_SET_PDEATHSIG is tied to the creating OS thread, not its goroutine.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
	}
	cmd.WaitDelay = 250 * time.Millisecond
	terminate := func() error {
		if profile.Resources != nil {
			return cleanup()
		}
		return killGroup(cmd.Process.Pid)
	}
	cmd.Cancel = func() error {
		if err := terminate(); err != nil {
			return err
		}
		return nil
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sandbox launcher: %w", err)
	}
	defer func() { err = errors.Join(err, terminate()) }()
	stopIO := context.AfterFunc(ctx, func() {
		inWrite.Close()
		outRead.Close()
	})
	defer stopIO()
	inRead.Close()
	outWrite.Close()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	reader := bufio.NewReaderSize(outRead, maxFrame+1)
	var workspace *os.Root
	var communicationErr error
	if profile.Resources != nil {
		var setup message
		setup, communicationErr = readMessage(reader)
		if communicationErr == nil && setup != (message{Type: "sandbox.workspace"}) {
			communicationErr = errors.New("missing resource workspace bootstrap")
		}
		if communicationErr == nil {
			workspace, communicationErr = openResourceWorkspace(cmd.Process.Pid, root)
		}
		if communicationErr == nil {
			communicationErr = writeMessage(inWrite, message{Type: "sandbox.workspace.accepted"})
		}
	} else if inspect != nil {
		workspace, communicationErr = os.OpenRoot(root)
	}
	if workspace != nil {
		defer func() { err = errors.Join(err, workspace.Close()) }()
	}
	if communicationErr == nil {
		communicationErr = communicate(inWrite, reader)
	}
	inWrite.Close()
	if communicationErr != nil {
		err = terminate()
	}
	waitErr := <-waited
	err = errors.Join(err, communicationErr, waitErr, ctx.Err())
	err = errors.Join(err, terminate())
	if err == nil && inspect != nil {
		err = inspect(workspace)
	}
	if err != nil && stderr.Len() != 0 {
		err = fmt.Errorf("%w; worker stderr: %s", err, stderr.String())
	}
	return err
}
