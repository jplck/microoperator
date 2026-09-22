package sandbox

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	nono "github.com/nolabs-ai/nono-go"
)

// configurePlatformSandbox adds the Linux runtime files to the proposed nono
// profile. It only builds the permission description; nono.Apply enforces it later.
//
// A dynamically linked executable needs its loader and shared libraries even when
// its own application data is restricted to inputs, scratch, and output. These
// system directories are read-only grants, not writable workspaces. The loader
// cache maps library names to installed paths; it does not contain API credentials.
//
// These are the paths required by the current Linux diagnostic profile, not a
// general dependency resolver. A missing required path is an error: we must not
// compensate by granting access to a broader directory such as the filesystem root.
func configurePlatformSandbox(caps *nono.CapabilitySet) error {
	for _, path := range []string{"/lib", "/lib64", "/usr/lib"} {
		if err := caps.AllowPath(path, nono.AccessRead); err != nil {
			return fmt.Errorf("grant runtime path %s: %w", path, err)
		}
	}
	if err := caps.AllowFile("/etc/ld.so.cache", nono.AccessRead); err != nil {
		return fmt.Errorf("grant runtime loader cache: %w", err)
	}
	return nil
}

// applyPlatformSandbox installs the extra Linux restriction that nono's
// NetworkBlocked mode does not provide on our checked host: no new sockets at all.
// Unix sockets communicate with local programs, so allowing them would let workers
// reach services without going through the daemon's permission checks.
//
// This is a fixed seccomp filter, not a general sandbox or configurable policy
// engine. Seccomp lets the kernel reject selected system calls before executing
// them. Nono still supplies filesystem confinement and its own network policy.
// Ordinary pipe read/write calls remain available for worker-to-daemon messages.
//
// The caller must already be the fresh sandbox-exec child, locked to one OS thread.
// This installation restricts that thread; it does not retrofit confinement onto
// all existing Go runtime threads. Exec follows it with nono.Apply and exec
// on the same thread. Exec replaces the program and discards the other threads;
// the new worker and its later threads/children inherit the restrictions.
//
// Never call this in the daemon. The restriction cannot be undone, and any setup
// error must abort the launch rather than continue with weaker protection.
func applyPlatformSandbox() error {
	// These values come from the Linux audit/seccomp/prctl interfaces. The current
	// profile is gated to amd64 by Supported. In a seccomp return
	// value, the upper bits select an action and the low bits can carry an errno:
	// "deny" therefore makes a blocked call fail with EPERM ("operation not permitted").
	const (
		auditArchAMD64    = 0xc000003e
		x32SyscallBit     = 0x40000000
		killProcess       = 0x80000000
		allow             = 0x7fff0000
		deny              = 0x00050000 | uint32(syscall.EPERM)
		prSetNoNewPrivs   = 38
		seccompModeFilter = 2
	)
	// Classic BPF is a small instruction language interpreted by the kernel.
	// LD|W|ABS loads a 32-bit field from seccomp_data: architecture at byte offset
	// 4, syscall number at offset 0. A conditional jump's Jt/Jf says how many
	// following instructions to skip when its comparison is true/false.
	//
	// First require the amd64 syscall ABI. Matching it skips the kill instruction;
	// an unexpected architecture terminates the process because the same syscall
	// number could mean something different under another ABI.
	//
	// x32 needs a separate check: it uses the amd64 architecture identifier but
	// marks syscall numbers with a high bit. Rejecting that entire ABI prevents
	// it from slipping past comparisons with ordinary amd64 syscall numbers.
	filter := []syscall.SockFilter{
		{Code: syscall.BPF_LD | syscall.BPF_W | syscall.BPF_ABS, K: 4}, // seccomp_data.arch
		{Code: syscall.BPF_JMP | syscall.BPF_JEQ | syscall.BPF_K, K: auditArchAMD64, Jt: 1},
		{Code: syscall.BPF_RET | syscall.BPF_K, K: killProcess},
		{Code: syscall.BPF_LD | syscall.BPF_W | syscall.BPF_ABS, K: 0}, // seccomp_data.nr
		{Code: syscall.BPF_JMP | syscall.BPF_JSET | syscall.BPF_K, K: x32SyscallBit, Jf: 1},
		{Code: syscall.BPF_RET | syscall.BPF_K, K: deny},
	}
	// socket/socketpair cover all socket families, including Internet and Unix
	// sockets. Blocking only TCP would leave the local-service bypass open.
	//
	// The remaining amd64 numbers cover alternate ways to obtain or use handles:
	// 425/426/427 are io_uring_setup/enter/register. io_uring can perform socket
	// operations without a normal socket syscall, so this pipe-only worker does
	// not get that interface. 438 is pidfd_getfd, which can duplicate a descriptor
	// from another process; denying it prevents importing an existing socket.
	//
	// Each pair below means "if this is the syscall, return EPERM; otherwise skip
	// that return and compare the next number". Inherited descriptors must still
	// be handled separately by closeOnExecDescriptors before exec.
	for _, number := range []uint32{syscall.SYS_SOCKET, syscall.SYS_SOCKETPAIR, 425, 426, 427, 438,
		syscall.SYS_UNSHARE, 308, syscall.SYS_MOUNT, syscall.SYS_UMOUNT2, syscall.SYS_PIVOT_ROOT,
		syscall.SYS_SETUID, syscall.SYS_SETGID, syscall.SYS_SETREUID, syscall.SYS_SETREGID, syscall.SYS_SETRESUID, syscall.SYS_SETRESGID, syscall.SYS_SETFSUID, syscall.SYS_SETFSGID,
		syscall.SYS_SETXATTR, syscall.SYS_LSETXATTR, syscall.SYS_FSETXATTR,
		428, 429, 430, 431, 432, 442,
	} {
		filter = append(filter,
			syscall.SockFilter{Code: syscall.BPF_JMP | syscall.BPF_JEQ | syscall.BPF_K, K: number, Jf: 1},
			syscall.SockFilter{Code: syscall.BPF_RET | syscall.BPF_K, K: deny},
		)
	}
	// Namespace init must not clear its parent-death signal, directly or by
	// changing credentials. Other prctl operations (including nono setup) remain
	// available. seccomp_data.args[0] starts at offset 16.
	filter = append(filter,
		syscall.SockFilter{Code: syscall.BPF_JMP | syscall.BPF_JEQ | syscall.BPF_K, K: syscall.SYS_PRCTL, Jf: 3},
		syscall.SockFilter{Code: syscall.BPF_LD | syscall.BPF_W | syscall.BPF_ABS, K: 16},
		syscall.SockFilter{Code: syscall.BPF_JMP | syscall.BPF_JEQ | syscall.BPF_K, K: 1, Jf: 1},
		syscall.SockFilter{Code: syscall.BPF_RET | syscall.BPF_K, K: deny},
	)
	// ALLOW means only that THIS filter has no objection. Other seccomp filters,
	// Landlock rules installed by nono, and ordinary OS permissions still apply.
	// Filters stack; a later filter cannot restore access denied by this one.
	filter = append(filter, syscall.SockFilter{Code: syscall.BPF_RET | syscall.BPF_K, K: allow})
	program := syscall.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	// no_new_privs prevents gaining privileges through a later exec (for example,
	// from a set-user-ID executable). It also lets an unprivileged process install
	// a seccomp filter. It is inherited and cannot be turned off.
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("set no_new_privs: %w", errno)
	}
	// prctl expects a native pointer to a SockFprog, which points to the BPF
	// instruction array. unsafe is limited to passing that address across the
	// syscall boundary. The kernel validates and copies the program during this
	// call; it does not retain a Go pointer. KeepAlive makes the array's lifetime
	// explicit until the syscall has finished reading it.
	_, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, syscall.PR_SET_SECCOMP, seccompModeFilter, uintptr(unsafe.Pointer(&program)), 0, 0, 0)
	runtime.KeepAlive(filter)
	if errno != 0 {
		return fmt.Errorf("install socket seccomp filter: %w", errno)
	}
	return nil
}
