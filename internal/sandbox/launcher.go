//go:build darwin || linux

package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/jplck/microoperator/internal/protocol"
	"github.com/jplck/microoperator/internal/state"
	nono "github.com/nolabs-ai/nono-go"
)

// Supported limits launch to platforms with an implemented
// sandbox profile. It is separate from nono.IsSupported: a kernel can support
// nono while still lacking controls our worker profile needs.
//
// Linux is currently amd64-only because the supplemental seccomp filter checks
// that syscall ABI and uses its syscall numbers. Enabling another architecture
// requires adapting and verifying that filter, not just changing this boolean.
// A true result reports implemented support, not permission to run untrusted code.
func Supported() bool {
	// ponytail: qualify additional Linux architectures with real denial tests before enabling them.
	return runtime.GOOS == "darwin" || (runtime.GOOS == "linux" && runtime.GOARCH == "amd64")
}

// Exec turns a fresh launcher process into a confined worker or tool.
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
func Exec(root, target string, args []string, profiles ...state.SandboxConfig) error {
	profile := state.SandboxConfig{Read: []string{"inputs"}, ReadWrite: []string{"scratch", "output"}, Network: "blocked"}
	if len(profiles) > 1 {
		return errors.New("only one sandbox profile is allowed")
	}
	if len(profiles) == 1 {
		profile = profiles[0]
	}
	if err := state.ValidateSandbox("sandbox_profile", profile); err != nil {
		return err
	}
	if !Supported() || !nono.IsSupported() {
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
	if profile.Toolchain != "" {
		if profile.Resources == nil || target != filepath.Join(profile.Toolchain, "bin", "go") {
			return errors.New("toolchain grant is restricted to the confined Go builder")
		}
		if err := caps.AllowPath(profile.Toolchain, nono.AccessRead); err != nil {
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
	env := Environment(root)
	if profile.Toolchain != "" {
		env = append(env, buildEnvironment(root, profile.Toolchain)...)
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

func Environment(root string) []string {
	return []string{
		"HOME=" + filepath.Join(root, "scratch"),
		"TMPDIR=" + filepath.Join(root, "scratch"),
		"LANG=C",
		"TZ=UTC",
	}
}

type BoundedStderr struct {
	bytes.Buffer
}

func (b *BoundedStderr) Write(p []byte) (int, error) {
	if len(p) > protocol.MaxFrame-b.Len() {
		n, _ := b.Buffer.Write(p[:protocol.MaxFrame-b.Len()])
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

func Supervise(ctx context.Context, launcher, root, target string, args []string,
	communicate func(io.Writer, io.Reader) error,
	profiles ...state.SandboxConfig,
) (err error) {
	return SuperviseWorkspace(ctx, launcher, root, target, args, communicate, nil, profiles...)
}

func SuperviseWorkspace(ctx context.Context, launcher, root, target string, args []string,
	communicate func(io.Writer, io.Reader) error, inspect func(*os.Root) error, profiles ...state.SandboxConfig,
) (err error) {
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("worker supervision requires a deadline")
	}
	commandArgs := append([]string{"sandbox-exec", root, target}, args...)
	if len(profiles) > 1 {
		return errors.New("only one sandbox profile is allowed")
	}
	if len(profiles) == 1 {
		if err := state.ValidateSandbox("sandbox_profile", profiles[0]); err != nil {
			return err
		}
		data, err := json.Marshal(profiles[0])
		if err != nil {
			return err
		}
		if len(data) > protocol.MaxFrame {
			return errors.New("sandbox profile exceeds launch limit")
		}
		commandArgs = append([]string{"sandbox-exec-profile", root, target, string(data)}, args...)
		if profiles[0].Toolchain != "" {
			commandArgs = append([]string{"sandbox-build-profile", root, target, string(data), profiles[0].Toolchain}, args...)
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
	var stderr BoundedStderr
	cmd := exec.CommandContext(ctx, launcher, commandArgs...)
	cmd.Dir, cmd.Env = root, Environment(root)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inRead, outWrite, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	profile := state.SandboxConfig{}
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
	reader := bufio.NewReaderSize(outRead, protocol.MaxFrame+1)
	var workspace *os.Root
	var communicationErr error
	if profile.Resources != nil {
		var setup protocol.Message
		setup, communicationErr = protocol.ReadMessage(reader)
		if communicationErr == nil && setup != (protocol.Message{Type: "sandbox.workspace"}) {
			communicationErr = errors.New("missing resource workspace bootstrap")
		}
		if communicationErr == nil {
			workspace, communicationErr = openResourceWorkspace(cmd.Process.Pid, root)
		}
		if communicationErr == nil {
			communicationErr = protocol.WriteMessage(inWrite, protocol.Message{Type: "sandbox.workspace.accepted"})
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
