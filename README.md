# Microoperator

A diagnostic Go/nono-go worker-launch spike. The multi-agent runtime, LLM broker,
storage, tool registry, event scheduler, and UI are not implemented yet.

**Not ready for untrusted agent code.** Hard resource limits and cleanup after
supervisor death remain unqualified. The macOS profile also retains resolver IPC.
On Linux/amd64, the launcher supplements nono-go with a socket-denying seccomp
filter; this closes the observed Unix-socket gap, not the entire qualification gate.

## Run

Requires macOS or Linux/amd64, Go 1.24+, cgo, and a C compiler. Linux also requires
working Landlock and seccomp, `/lib`, `/lib64`, `/usr/lib`, and `/etc/ld.so.cache`.
Diagnostic profiles have been checked on macOS/arm64 and Linux/amd64 WSL2; other
Linux architectures are rejected until separately verified.

```sh
CGO_ENABLED=1 go run . run -message hello -timeout 5s
```

The parent prints a diagnostic warning to stderr, creates a temporary private
workspace, starts a confined worker, waits for readiness, and exchanges one JSON
ping/pong over inherited pipes. Successful stdout:

```json
{"type":"pong","data":"hello"}
```

The worker gets read-only `inputs`, writable `scratch`/`output`, the executable,
and required system library/device access. Its environment contains only private
`HOME`/`TMPDIR`, `LANG`, and `TZ`; unrelated descriptors are marked close-on-exec.
Sockets are rejected on standard descriptors too, so inherited connections cannot
replace the approved pipes.
Frames and captured stderr are capped at 64 KiB. Deadline or cancellation kills
the worker process group; the parent waits and removes its temporary workspace.

`sandbox-exec` and `worker` are internal modes, not general-purpose user commands.
The launcher locks its OS thread, installs any platform restrictions, applies
nono-go, and immediately execs the target on that thread. Errors abort launch;
there is no unsandboxed fallback. On Linux/amd64, the extra seccomp filter denies
`socket`, `socketpair`, `io_uring_setup/enter/register`, and `pidfd_getfd`; it also
rejects alternate syscall ABIs. It cannot be loosened by a worker and is inherited
across exec, new threads, and descendants. The parent's networking is unchanged.

## Checks

```sh
CGO_ENABLED=1 go test ./...
CGO_ENABLED=1 go vet ./...
CGO_ENABLED=1 go test -race ./...
CGO_ENABLED=1 go test -tags=integration -count=1 ./...
CGO_ENABLED=1 go test -race -tags=integration -count=1 ./...
```

Default tests cover framing, validation, and output bounds. Integration tests build
the real executable and use disposable confined subprocesses and fixture files.
They cover filesystem denials, symlink escapes, TCP/UDP/ordinary Unix-connection
denials, thread/descendant inheritance, environment and descriptor isolation,
failed launch, deadlines, and process-group cancellation. No model API is called.

Linux checks additionally cover pathname/abstract Unix sockets, socket pairs,
`io_uring`, `pidfd_getfd`, x32 syscalls, socket-filter inheritance, inherited socket
descriptors, and rejection of socket-backed stdin before worker readiness.
On macOS the suite still **characterizes the remaining Unix-stream socket
allowance**. No check contacts the real system resolver. Passing this diagnostic
suite is not approval of the full v1 sandbox profile.

## Before enabling agent execution

- Resolve strict macOS network/IPC isolation before enabling untrusted code there.
  Extra platform deny rules in the current core do not override its later resolver
  allowance; binding upgrade work remains deferred.
- Establish hard CPU, memory, disk, and process-count limits. A deadline is not a
  substitute for these controls.
- Verify containment of descendants that leave their process group and cleanup
  after abrupt supervisor death. Current cancellation checks cover descendants
  that remain in the supervised group.

### Linux/WSL2 recheck

Checked 21 September 2026 on Linux/amd64, WSL2 kernel
`6.6.114.1-microsoft-standard-WSL2`, Go 1.24.0, GCC 13.3.0, and `CGO_ENABLED=1`.
Dependencies were not changed. The pinned Linux-amd64 archive records the same
native core commit `1d1c88c9f98f0a1f3ff79cff1509713aaec7cdb0` (0.65.1).

| Check | Observed result |
| --- | --- |
| Default tests and `go vet` | Passed on this host |
| Application integration suite, including race detection | Passed with the Linux launcher and supplemental seccomp filter |
| Diagnostic launch | Returned `{"type":"pong","data":"hello"}` over the existing pipes |
| Binding support diagnostics | `IsSupported()` is true; `SupportInfo()` reports Landlock V3, with filesystem controls but no Landlock TCP or signal/abstract-socket scoping |
| Filesystem fixtures through the launcher | Granted reads/writes succeeded; protected reads/writes and symlink escapes denied |
| IPv4 TCP/UDP and pathname/abstract Unix sockets through the launcher | Denied, including Unix listeners outside the workspace |
| Alternate socket/descriptor paths | Socket pairs, `io_uring` calls, `pidfd_getfd`, and x32 calls denied; inherited sockets unavailable |
| Inheritance and cancellation | Filesystem and socket restrictions held in new threads/descendants; supervised process-group cancellation passed |
| Host resource controls | cgroup v2 exposes CPU, memory, and PID controllers; delegation and workload-limit enforcement were not verified |

Before the fix, a native-only probe using `NetworkBlocked` could deliver a fixed
payload to pathname and abstract Unix-socket fixtures outside its granted workspace.
The [pinned Linux implementation](https://github.com/nolabs-ai/nono/blob/1d1c88c9f98f0a1f3ff79cff1509713aaec7cdb0/crates/nono/src/sandbox/linux.rs)
uses a seccomp network-block fallback on Landlock V3 that still permits Unix sockets.
The application now installs its own narrower filter before nono-go applies the
filesystem/network profile. It needs no binding fork, native rebuild, or new dependency.

The integration checks use the actual launcher, fresh exec'd workers, temporary
files, and local fixture sockets with positive controls. No real resolver or
external provider is contacted. This verifies the listed Linux boundaries, not
hard resource limits, process-group escape containment, or cleanup after supervisor
death. macOS behavior was not rerun on this Linux host.

### Deferred macOS binding upgrade

As checked on 19 September 2026, our binding pin already matches upstream `main`
(`9ba65a11c842`). The latest tagged release, `v0.21.0`, is older; there is no newer
Go version to bump to that fixes this limitation.

Keep the current dependency for now: no local fork or native rebuild. Recheck
upstream when revisiting this issue. The eventual fix needs a newer bundled native
core and binding support for disabling DNS, followed by real denial checks.
Installing a newer nono CLI would not update the binding's statically linked core.

The binding is pinned in `go.mod`; its macOS/arm64 archive records native commit
`1d1c88c9f98f0a1f3ff79cff1509713aaec7cdb0` (core 0.65.1).
`nono.Version()` reports the FFI version (`0.1.0` here), not that native core version.
The newer upstream core has a DNS-blocking option that this binding does not expose.

See [implementation-plan.md](implementation-plan.md) for milestone status and next
steps, and [spec.md](spec.md) for the target contract, architecture rationale, and
dependency research.
