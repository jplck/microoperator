# Microoperator development instructions

## Start here

- Read [spec.md](../spec.md) for the implementation contract and relevant sections
  of [plan.md](../plan.md) for rationale. The spec takes precedence over the plan.
- Inspect the actual code and build configuration before changing anything. Do not
  assume planned components already exist. Planning/documentation tasks must not
  bootstrap implementation.
- Implement only the requested slice. Update affected specifications when an
  intentional behavior or architecture change makes them inaccurate.

## Keep it small

- Prefer Go's standard library, existing helpers, and native OS facilities.
- Avoid speculative interfaces, factories, configuration, packages, and dependencies.
  Keep the explicitly required message-transport boundary replaceable.
- Make surgical changes; preserve unrelated work. Explain only non-obvious choices.
- Mark deliberate shortcuts with a short `// ponytail:` comment naming the limit
  and when to replace it. Never use simplification to remove safety or validation.

## Architecture invariants

- One Go daemon hosts independent systems, each with its own operator and agents.
  Use `system_id` throughout system-owned state. Derive identity from authenticated
  sessions; never trust caller-supplied identity or scope.
- The daemon owns SQLite, artifacts, credentials, grants, scheduling, and brokers.
  Workers access these through validated, scoped operations, not direct host access.
- Agents may propose new agents, skills, tools, and revisions; they cannot increase
  their own authority. Revisions are immutable and budgets include descendants.
- Use explicit permission checks. Do not introduce containers, a general policy
  engine, an external queue, or a vector service without a concrete requirement.
- The optional UI is a separate client of the daemon, never its execution engine.

## Sandbox and trust boundaries

- Use `github.com/nolabs-ai/nono-go`, not the nono CLI. The runtime requires cgo
  and pinned native libraries; verify binding/core/platform capabilities.
- Apply irreversible restrictions only in a fresh sandbox launcher, on a locked
  OS thread immediately followed by exec. Never call `nono.Apply` in the daemon.
- Unsupported controls or sandbox setup failures must abort launch. No unsandboxed
  fallback. Verify resource limits, thread/descendant confinement, and cleanup.
- Keep credentials and unrestricted networking out of workers. Validate broker
  arguments, artifact paths, and returned data; bound IPC and redact secrets.
- Before privileged delegation, revisit the trust-boundary reminder in the spec:
  a recipient must not lend its full privileges to a caller. Events wake sandboxed
  work; they never execute agent-supplied code inside the daemon.

## Durable work, model calls, and learning

- Commit local state changes and outbound events transactionally. Delivery is at
  least once; deduplicate and reconcile ambiguous effects instead of blindly
  retrying. Bound queues, retries, timers, and event amplification.
- Route every model call through the shared broker. Apply provider quota groups,
  fair admission, concurrency limits, and atomic system/goal budget reservations.
  Streaming and retries consume capacity; restart must not reset allowances.
- Authorize memory retrieval before exposing results, snippets, or artifacts.
  Retrieved content is untrusted data, not instructions or permission grants.
- Learning proposes versioned improvements, not model-weight or daemon changes.
  Generated Go tools build/test in confinement with no secrets/network, a pinned
  toolchain, and `CGO_ENABLED=0`. Protected checks and exact-artifact promotion
  remain outside agent control; executable promotion defaults to human approval.

## Go implementation and verification

- Use explicit types, context cancellation/deadlines, wrapped errors, and clear
  ownership of goroutines, processes, descriptors, and transactions. Surface errors;
  do not substitute silent defaults or success-shaped fallbacks.
- Format changed Go files with `gofmt`. Use Go's `testing` package and the smallest
  runnable regression check covering nontrivial changes; avoid a new test framework.
- Once a Go module exists, run targeted `go test` and `go vet` for affected packages.
  Use `go test -race` for concurrency changes on a supported cgo-enabled platform.
  Follow existing project commands when present; never invent passing results.
- Use fake model endpoints, temporary databases, and controlled clocks where needed.
  Tests must not spend real API credits or run generated code outside its sandbox.
  Run irreversible sandbox checks in disposable subprocesses, never the test runner.
- Documentation-only changes need no Go build or new tooling. Report unsupported
  platform checks and unverified assumptions honestly rather than claiming isolation.
