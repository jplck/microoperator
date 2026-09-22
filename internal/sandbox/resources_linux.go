package sandbox

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/jplck/microoperator/internal/protocol"
	"github.com/jplck/microoperator/internal/state"
)

const cgroupFilesystem = 0x63677270

// The user service must delegate cpu, memory and pids. Move only this daemon
// into a leaf before enabling controllers: cgroup v2 forbids internal processes
// in a domain that distributes resources to child groups.
func PrepareResourceRoot() (string, error) {
	if os.Geteuid() == 0 {
		return "", errors.New("resource confinement requires a non-root delegated user service")
	}
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(strings.TrimPrefix(string(data), "0::"))
	if !strings.HasPrefix(string(data), "0::") || !filepath.IsAbs(path) || strings.Contains(path, "\n") {
		return "", errors.New("unified cgroup v2 membership required")
	}
	if filepath.Base(path) == "supervisor" {
		path = filepath.Dir(path)
	}
	if !strings.HasSuffix(path, ".service") && !strings.HasSuffix(path, ".scope") {
		return "", errors.New("start within a delegated systemd user service or scope")
	}
	root := filepath.Join("/sys/fs/cgroup", path)
	file, err := os.Open(root)
	if err != nil {
		return "", err
	}
	var filesystem syscall.Statfs_t
	err = syscall.Fstatfs(int(file.Fd()), &filesystem)
	info, statErr := file.Stat()
	err = errors.Join(err, statErr, file.Close())
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || filesystem.Type != cgroupFilesystem || stat.Uid != uint32(os.Geteuid()) {
		return "", errors.New("cgroup must be a user-owned delegated cgroup v2 domain")
	}
	controllers, err := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
	if err != nil {
		return "", err
	}
	for _, name := range []string{"cpu", "memory", "pids"} {
		found := false
		for _, available := range strings.Fields(string(controllers)) {
			if name == available {
				found = true
			}
		}
		if !found {
			return "", fmt.Errorf("required delegated controller %s unavailable", name)
		}
	}
	leaf := filepath.Join(root, "supervisor")
	if err := os.Mkdir(leaf, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(leaf, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		return "", fmt.Errorf("move supervisor to resource leaf: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte("+cpu +memory +pids"), 0600); err != nil {
		return "", fmt.Errorf("enable delegated controllers: %w", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		parts := strings.Split(entry.Name(), "_")
		if !entry.IsDir() || len(parts) != 3 || parts[0] != "worker" || len(parts[2]) != 32 {
			continue
		}
		pid, err := strconv.Atoi(parts[1])
		if err != nil || pid < 1 {
			continue
		}
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			continue
		}
		group := filepath.Join(root, entry.Name())
		events, err := os.ReadFile(filepath.Join(group, "cgroup.events"))
		if err != nil {
			return "", err
		}
		if strings.Contains(string(events), "populated 0") {
			if err := os.Remove(group); err != nil {
				return "", fmt.Errorf("remove abandoned empty resource group: %w", err)
			}
		}
	}
	return root, nil
}

// CLONE_INTO_CGROUP attaches the child before any launcher instruction runs.
// The PID namespace makes setsid/double-fork irrelevant to descendant cleanup.
// Killing its init kills the entire namespace; the sealed parent-death signal
// also does that if the daemon dies before it can issue cgroup.kill.
func prepareResourceProcess(cmd *exec.Cmd, profile state.SandboxConfig) (func() error, error) {
	if profile.Resources == nil {
		return func() error { return nil }, nil
	}
	if profile.ResourceRoot == "" {
		return nil, errors.New("resource profile has no delegated cgroup root")
	}
	id, err := state.NewID("worker_" + strconv.Itoa(os.Getpid()) + "_")
	if err != nil {
		return nil, err
	}
	group := filepath.Join(profile.ResourceRoot, id)
	if err := os.Mkdir(group, 0700); err != nil {
		return nil, err
	}
	remove := func(cause error) (func() error, error) { return nil, errors.Join(cause, os.Remove(group)) }
	limits := profile.Resources
	for name, value := range map[string]string{
		"memory.max":      strconv.FormatInt(limits.MemoryBytes, 10),
		"memory.swap.max": "0", "memory.oom.group": "1",
		"pids.max": strconv.FormatInt(limits.Processes, 10),
		"cpu.max":  fmt.Sprintf("%d 100000", limits.CPUPercent*1000),
	} {
		if err := os.WriteFile(filepath.Join(group, name), []byte(value), 0600); err != nil {
			return remove(fmt.Errorf("set %s: %w", name, err))
		}
	}
	fd, err := os.Open(group)
	if err != nil {
		return remove(err)
	}
	cmd.SysProcAttr.Cloneflags = syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC | syscall.CLONE_NEWNET
	cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Geteuid(), Size: 1}}
	cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getegid(), Size: 1}}
	cmd.SysProcAttr.GidMappingsEnableSetgroups = false
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = int(fd.Fd())
	cleanup := sync.OnceValue(func() error {
		killErr := os.WriteFile(filepath.Join(group, "cgroup.kill"), []byte("1"), 0600)
		deadline := time.Now().Add(5 * time.Second)
		for killErr == nil {
			data, err := os.ReadFile(filepath.Join(group, "cgroup.events"))
			if err != nil {
				killErr = err
				break
			}
			if strings.Contains(string(data), "populated 0") {
				break
			}
			if time.Now().After(deadline) {
				killErr = errors.New("resource group did not empty after cgroup.kill")
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		closeErr := fd.Close()
		if killErr != nil {
			return errors.Join(killErr, closeErr)
		}
		return errors.Join(closeErr, os.Remove(group))
	})
	return cleanup, nil
}

// Mount only in the new launcher's private namespace, before applying nono.
// One tmpfs bounds aggregate scratch/output bytes and inodes; imported inputs
// are a read-only bind mount, not a writable route to the original workspace.
func prepareResourceWorkspace(root string, profile state.SandboxConfig) error {
	if profile.Resources == nil {
		return nil
	}
	if os.Getpid() != 1 {
		return errors.New("resource launcher must be PID-namespace init")
	}
	// Go's fork/exec Pdeathsig check compares getppid with a host PID, which
	// cannot work across a PID namespace. Set it here, on the locked exec thread.
	// The pipe handshake below closes the pre-install parent-death race: a dead
	// supervisor cannot acknowledge setup, so no workload can be executed.
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, 1, uintptr(syscall.SIGKILL), 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("set parent-death protection: %w", errno)
	}
	var deathSignal int32
	_, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, 2, uintptr(unsafe.Pointer(&deathSignal)), 0, 0, 0, 0)
	if errno != 0 || deathSignal != int32(syscall.SIGKILL) {
		return errors.New("resource launcher requires SIGKILL parent-death protection")
	}
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}
	input, err := os.Open(filepath.Join(root, "inputs"))
	if err != nil {
		return err
	}
	defer input.Close()
	options := fmt.Sprintf("size=%d,nr_inodes=4096,mode=0700", profile.Resources.WorkspaceBytes)
	if err := syscall.Mount("tmpfs", root, "tmpfs", syscall.MS_NODEV|syscall.MS_NOSUID, options); err != nil {
		return fmt.Errorf("mount bounded workspace: %w", err)
	}
	for _, path := range []string{"inputs", "scratch", "output"} {
		if err := os.Mkdir(filepath.Join(root, path), 0700); err != nil {
			return err
		}
	}
	if err := syscall.Mount(fmt.Sprintf("/proc/self/fd/%d", input.Fd()), filepath.Join(root, "inputs"), "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return err
	}
	if err := syscall.Mount("", filepath.Join(root, "inputs"), "", syscall.MS_REMOUNT|syscall.MS_BIND|syscall.MS_RDONLY|syscall.MS_NODEV|syscall.MS_NOSUID, ""); err != nil {
		return err
	}
	if err := syscall.Chdir(root); err != nil {
		return err
	}
	for _, paths := range [][]string{profile.Read, profile.ReadWrite} {
		for _, path := range paths {
			if !strings.HasPrefix(path, "inputs") {
				if err := os.MkdirAll(filepath.Join(root, path), 0700); err != nil {
					return err
				}
			}
		}
	}
	if err := protocol.WriteMessage(os.Stdout, protocol.Message{Type: "sandbox.workspace"}); err != nil {
		return err
	}
	ack, err := protocol.ReadMessage(bufio.NewReaderSize(os.Stdin, protocol.MaxFrame+1))
	if err != nil {
		return err
	}
	if ack != (protocol.Message{Type: "sandbox.workspace.accepted"}) {
		return errors.New("workspace descriptor was not accepted")
	}
	return nil
}

func openResourceWorkspace(pid int, root string) (*os.Root, error) {
	return os.OpenRoot(filepath.Join("/proc", strconv.Itoa(pid), "root", root))
}
