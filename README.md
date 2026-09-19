# Microoperator

A diagnostic Go/nono-go worker-launch spike. The multi-agent runtime, LLM broker,
storage, tool registry, event scheduler, and UI are not implemented yet.

**Not ready for untrusted agent code.** The pinned binding's native macOS profile
retains resolver IPC even in `NetworkBlocked` mode. The spike demonstrates specific
filesystem and connection restrictions; it does not satisfy the target runtime's
strict network-isolation requirement.

## Run

Requires macOS, Go 1.24+, cgo, and a C compiler. Checked on macOS/arm64.

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
Frames and captured stderr are capped at 64 KiB. Deadline or cancellation kills
the worker process group; the parent waits and removes its temporary workspace.

`sandbox-exec` and `worker` are internal modes, not general-purpose user commands.
The launcher applies nono-go on a locked OS thread, then execs the target. Errors
abort launch; there is no unsandboxed fallback. Linux launch is explicitly rejected
until its confinement profile is implemented and verified.

## Checks

```sh
CGO_ENABLED=1 go test ./...
CGO_ENABLED=1 go vet ./...
CGO_ENABLED=1 go test -race ./...
CGO_ENABLED=1 go test -tags=integration -count=1 ./...
```

Default tests cover framing, validation, and output bounds. Integration tests build
the real executable and use disposable confined subprocesses and fixture files.
They cover filesystem denials, symlink escapes, TCP/UDP/ordinary Unix-connection
denials, thread/descendant inheritance, environment and descriptor isolation,
failed launch, deadlines, and process-group cancellation. No model API is called.

The integration suite also **characterizes the remaining Unix-stream socket
allowance**. It does not contact the real system resolver. Passing this diagnostic
suite is not approval of the strict v1 sandbox profile.

## Before enabling agent execution

- Resolve strict DNS blocking before enabling untrusted code; upgrade work is
  deferred for now. Extra platform deny rules in the current core do not override
  its later resolver allowance.
- Establish hard CPU, memory, disk, and process-count limits. A deadline is not a
  substitute for these controls.
- Verify containment of descendants that leave their process group and cleanup
  after abrupt supervisor death. Current cancellation checks cover descendants
  that remain in the supervised group.

### Deferred binding upgrade

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
steps, [spec.md](spec.md) for the target contract, and [plan.md](plan.md) for the
architecture.
