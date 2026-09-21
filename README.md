# Microoperator

A Go daemon with validated configuration, SQLite-backed inactive systems, and an
authenticated local control API, plus the diagnostic nono-go worker-launch spike.
The model broker, agent execution loop, active tool registry, scheduler, and UI
are not implemented yet.

**Not ready for untrusted agent code.** Hard resource limits and cleanup after
supervisor death remain unqualified. The macOS profile also retains resolver IPC.
On Linux/amd64, the launcher supplements nono-go with a socket-denying seccomp
filter; this closes the observed Unix-socket gap, not the entire qualification gate.

## Run

Requires macOS or Linux/amd64, Go 1.24+, cgo, and a C compiler. Linux also requires
working Landlock and seccomp, `/lib`, `/lib64`, `/usr/lib`, and `/etc/ld.so.cache`.
Diagnostic profiles have been checked on macOS/arm64 and Linux/amd64 WSL2; other
Linux architectures are rejected until separately verified.

### Daemon

Milestone 1 creates, inspects, and revises **inactive** systems. It never starts
workers or calls a model provider. Create a user-owned `microoperator.json`:

```json
{
  "schema_version": 1,
  "data_dir": "./state",
  "providers": {
    "primary": {
      "adapter": "openai-chat-completions",
      "base_url": "https://llm.example.invalid/v1",
      "api_key_env": "MICROOPERATOR_LLM_KEY"
    }
  },
  "models": {
    "default": {
      "provider": "primary",
      "model": "replace-with-model-id",
      "quota_groups": ["account"],
      "max_output_tokens": 1024
    }
  },
  "quota_groups": {
    "account": {
      "requests_per_minute": 60,
      "tokens_per_minute": 60000,
      "burst_requests": 1,
      "max_concurrent": 2,
      "queue_capacity": 100,
      "max_wait_seconds": 30
    }
  },
  "sandbox_profiles": {
    "worker": {
      "read": ["inputs"],
      "read_write": ["scratch", "output"],
      "network": "blocked"
    }
  },
  "tools": {},
  "systems": {
    "research": {
      "tools": [],
      "operator": {
        "prompt": "Research the supplied goal.",
        "model": "default",
        "tools": [],
        "sandbox_profile": "worker"
      },
      "limits": {
        "max_agents": 4,
        "max_active_agents": 2,
        "token_budget": 50000
      }
    }
  }
}
```

```sh
chmod 600 microoperator.json
export MICROOPERATOR_CONTROL_TOKEN="$(openssl rand -hex 32)"
export MICROOPERATOR_LLM_KEY='unused-until-the-model-broker-exists'
CGO_ENABLED=1 go run . daemon --config ./microoperator.json
```

The placeholder provider key suffices for this inactive milestone; no provider
request is made. Use real approved settings before enabling model calls in a later
milestone. Credentials are environment variables, never JSON values or URL parameters.
The control token must contain 32-256 printable, non-space characters.

Readiness is one JSON line with `type: "ready"` and `data` containing the absolute
control-socket path. Invalid configuration or missing declared credentials prevents
readiness. SIGINT/SIGTERM shuts down the server and closes the database.

`data_dir` is relative to the configuration file, not the invoking shell. It must
be a private, user-owned directory (0700); the database, lock, and socket use 0600.
The daemon refuses symlinked configuration/state files, unsafe file permissions,
an unrelated/newer database, or a second daemon using the same directory. After
a crash, it recovers committed SQLite state and removes only the stale socket.
The default local configuration and state directory are ignored by Git.

### Control API

The API speaks HTTP **only over `state/control.sock`**; there is no TCP listener.
Every request requires `Authorization: Bearer ...`. The token authenticates one
local administrator; worker sessions and multi-user roles are not implemented.
Caller-supplied identity/scope fields are rejected, and browser `Origin` requests
are refused until the separate UI is implemented.

From a client shell with the same control token exported:

```sh
curl --unix-socket ./state/control.sock \
  -H "Authorization: Bearer $MICROOPERATOR_CONTROL_TOKEN" \
  http://localhost/v1/launch-configurations

curl --unix-socket ./state/control.sock \
  -H "Authorization: Bearer $MICROOPERATOR_CONTROL_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: research-1' \
  -d '{"launch":"research","goal":"Record this goal without executing it."}' \
  http://localhost/v1/systems

curl --unix-socket ./state/control.sock \
  -H "Authorization: Bearer $MICROOPERATOR_CONTROL_TOKEN" \
  http://localhost/v1/systems
```

Reusing `research-1` with the same command returns its saved result. A different
key, such as `research-2`, creates a separate instance from the same launch
configuration. Reusing a key with a different command returns 409.

To revise an instance, set `SYSTEM_ID` to its returned `system_id`. Submit the full
future configuration and the revision number you inspected:

```sh
curl --unix-socket ./state/control.sock \
  -H "Authorization: Bearer $MICROOPERATOR_CONTROL_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: revise-research-1' \
  -X PUT \
  -d '{
    "expected_revision": 1,
    "configuration": {
      "tools": [],
      "operator": {
        "prompt": "A revised research prompt.",
        "model": "default",
        "tools": [],
        "sandbox_profile": "worker"
      },
      "limits": {"max_agents": 4, "max_active_agents": 2, "token_budget": 50000}
    }
  }' \
  "http://localhost/v1/systems/$SYSTEM_ID/configuration"
```

| Route | Result |
| --- | --- |
| `GET /v1/health` | Readiness and `execution_enabled: false` |
| `GET /v1/launch-configurations` | Named templates, not running instances |
| `POST /v1/systems` | New runtime-assigned system/operator IDs and optional pending initial goal; returns 201 |
| `GET /v1/systems` | Up to 20 owned systems; follow `?after=<next>` when `next` is present |
| `GET /v1/systems/<system_id>` | Saved configuration, pinned grants, usage/remaining tokens, state, and any blocking reason |
| `PUT /v1/systems/<system_id>/configuration` | New immutable revision; stale `expected_revision` returns 409 |

Mutations require an `Idempotency-Key` of 1-128 letters/digits, dots, colons,
underscores, or hyphens, starting with a letter or digit. The state change, audit
record, and replay receipt commit in one SQLite transaction. Keys survive restarts
and control-token rotation. A replay represents the original command result;
use GET for the latest revision/usage. Administrative compatibility is rechecked
when displaying results. Storage failures return a generic 500 and do not commit
partial mutations; details stay in daemon diagnostics.

Administrative JSON is never rewritten by the API and has no hot reload. Existing
instances retain their saved revisions, grants, initial goal, and consumed budget
across file changes/restarts. Changed/removed referenced definitions produce a
`blocked_reason`, not an automatic substitution; an explicit revision can select
currently permitted definitions. Only named templates seed new instances.

Configuration is limited to 1 MiB and command bodies to 64 KiB. Unknown, duplicate,
or incorrectly cased JSON keys are rejected. References and grant subsets are
checked; profile paths must stay under `inputs`, `scratch`, or `output`, with writes
only to the latter two. Names use lowercase letters, digits, dots, underscores,
and hyphens, beginning with a letter, up to 64 characters.

Quota values must be positive: requests/minute up to 1,000,000, tokens/minute up to
1,000,000,000, concurrency up to 1,024, queue capacity up to 100,000, and wait up to
3,600 seconds. Burst cannot exceed requests/minute, and a model's output cap must
fit every referenced token quota. Agent counts satisfy
`1 <= max_active_agents <= max_agents <= 1024`; token budgets are positive and cannot
be revised below consumed usage. Prompt/goal/skill content is capped at 32 KiB.

This slice recognizes only `openai-chat-completions` provider configuration and
shared `skill` definitions. Skill versions/dependencies are validated and pinned
as metadata; there is no active registry or execution path. Executable tools and
the future `runtime.agent.propose` / `runtime.task.delegate` operations are rejected
rather than installed or stubbed. Profile validation and native support diagnostics
are not permission to run untrusted code. No start, model-call, or tool-call route exists.

### Worker-launch diagnostic

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

Default tests cover framing, strict configuration, real temporary SQLite
transactions/migrations, authorization, immutable revisions, pinned grants,
duplicate/concurrent commands, rollback, cancellation, and output bounds.
Integration tests build the real executable and use disposable processes and fixtures.
Daemon checks create two independent systems, revise one, restart gracefully and
after a crash, replay commands, rotate the control token, and revalidate removed
definitions. They assert zero requests to a fake provider and no implicit workers.
The daemon acceptance checks ran on Linux/amd64 WSL2; macOS was not rerun.
Sandbox checks cover filesystem denials, symlink escapes, TCP/UDP/ordinary Unix-connection
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
The nono-go dependency was not changed for the Linux socket fix. Its Linux-amd64 archive records
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
