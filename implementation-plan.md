# Microoperator: implementation plan

[spec.md](spec.md) defines the contract and explains the architecture.
This document tracks implementation order and milestone acceptance.
[README.md](README.md) is authoritative for current commands and known limitations.

Implement small vertical slices, retaining the existing diagnostic command.
Do not scaffold every future package, table, or interface at once.

## Sequence and status

| Milestone | Status | Depends on | Result |
| --- | --- | --- | --- |
| 0. Worker-launch spike | Implemented, diagnostic only | None | Confined ping/pong, supervision, and real subprocess checks |
| 1. Configuration and daemon state | Implemented, inactive systems only | 0 | Validated configuration and persistent system lifecycle |
| 2. Model broker and one operator | Next | 1 | One prompt produces a tracked, rate-limited response |
| 3. Scoped tool registry | Planned | 1-2 | Granted shared tools/skills and system-local proposal records |
| 4. Durable events and agent teams | Planned | 1-3 | Multiple systems self-organize through governed events |
| 5. Memory and scheduled wakeups | Planned | 4 | Scoped retrieval, subscriptions, and persistent timers |
| 6. Detached control UI | Planned | 1-5 | Create, steer, inspect, pause/resume, and stop systems |
| 7. Learning and generated tools | Planned, executable path gated | 3-6; S before untrusted build/run | Evaluated revisions and controlled tool promotion |
| S. Sandbox and delegation qualification | Deferred; not satisfied by milestone 0 | 0, 3-4, and README prerequisites | Permission to enable untrusted execution, not another service |

Milestones 1-6 can be developed and checked using reviewed workers, fixture tools,
and fake providers. This does not authorize untrusted generated code or privileged
autonomous deployment. Do not start the deferred native upgrade as part of ordinary
feature work. Diagnostic fixtures must not become a production bypass for failed
profile checks or an unsandboxed fallback.

## 0. Existing worker-launch spike

Already present:

- [go.mod](go.mod) and `go.sum`: Go module with a pinned nono-go dependency.
- [main.go](main.go): diagnostic parent, sandbox launcher, and fixed worker;
  private workspace, sanitized environment, bounded JSON IPC, readiness handshake,
  deadline/cancellation handling, and process-group cleanup.
- [sandbox_linux.go](sandbox_linux.go) and [sandbox_darwin.go](sandbox_darwin.go):
  platform runtime grants and Linux/amd64's supplemental socket-denying seccomp filter.
- [main_test.go](main_test.go): framing, argument validation, and output bounds.
- [sandbox_integration_test.go](sandbox_integration_test.go): actual subprocess
  filesystem/connection checks, thread/descendant inheritance, descriptor and
  environment isolation, failed setup, deadlines, and cancellation.
- [README.md](README.md): reproducible run/check commands and explicit limits.

The hello exchange, unit/component checks, vet, and diagnostic integration checks
including race detection previously ran successfully on macOS/arm64. On 21 September
2026 they also passed on Linux/amd64 WSL2 with the supplemental socket filter;
macOS was not rerun on that host. Linux checks cover the observed Unix-socket gap,
descriptor isolation, and filter inheritance through real launcher/worker processes.
These results cover the spike's measured behavior, not the entire target sandbox
contract. See the [README](README.md#linuxwsl2-recheck) for verified scope.
The complete qualification gate remains open; keep its technical blocker details
in [README.md](README.md#before-enabling-agent-execution).

## 1. Configuration, persistence, and daemon lifecycle

**Build**

- Implement the spec's versioned JSON loader, field/reference validation, model and
  tool names, quota definitions, profile selection, and credential references.
  Do not add unsupported adapters or tools merely because the example names them.
- Add SQLite through a maintained Go driver. Start with configuration snapshots,
  systems, revisions/grants, goals, and the minimum audit records needed here.
  Introduce explicit schema migrations as tables are added.
- Add daemon startup/shutdown and a restricted local control API. Persist
  runtime-assigned system IDs independently of named JSON launch configurations.
- Support creating/listing/inspecting inactive systems and updating their future
  configuration through authorized, idempotent commands. Keep administrative JSON
  ownership separate from persisted UI/runtime state.

**Acceptance**

Create two instances from one launch configuration, change one, and restart the
daemon. IDs, configuration, grants, and state remain distinct and unchanged.
Invalid configuration or unavailable required credentials prevents readiness;
error/inspection responses contain no secrets. No worker or model call starts
merely because a configuration is declared.

**Implemented scope and evidence**

[config.go](config.go), [store.go](store.go), and [daemon.go](daemon.go) implement
strict JSON loading, private SQLite state with explicit migrations, immutable
configuration/revision/grant snapshots, pending initial goals, audit records,
durable command receipts, and an authenticated Unix-socket control API.
The OpenAI-compatible provider shape and shared skills are validated as metadata;
model calls, executable tools, and future built-in broker operations are not stubbed.

Unit/component checks, vet, and race-enabled real-process integration checks passed
on Linux/amd64 WSL2. Two instances were created, one revised, and both recovered
unchanged across graceful and abrupt daemon restarts. Checks cover authorization,
credential/configuration startup failures, concurrent/stale revisions, transactional
rollback, immutable snapshots, durable retries, token rotation, and removed
administrative definitions. A fake provider observed zero calls; configuration
declarations and system creation started no workers. macOS was not rerun.
See [README.md](README.md#daemon) for runnable configuration and commands.

## 2. Model broker and a single operator

**Build**

- Extend worker IPC with correlated `model.call` requests and responses. Keep
  provider credentials and HTTP requests in the daemon.
- Implement the initial OpenAI-compatible Chat Completions adapter with configurable
  endpoint, bounded responses, deadlines, and explicit errors.
- Add shared quota groups, bounded/fair admission, concurrency slots, output caps,
  and atomic goal/system budget reservations. Persist usage and cooldowns.
- Add start/stop for one prompt-based operator per system, task outcomes, and
  pending/completed call receipts. Allow multiple systems to share the broker.

**Acceptance**

A prompt reaches a controllable fake provider and produces a persisted response
and usage record. Concurrent systems share provider limits without sharing state.
Exercise streaming, throttling, retries, cancellation, exhausted budgets, and
restart. Unknown call outcomes are recorded, not silently replayed or charged as
zero. Stop terminates the selected worker without affecting another system.

## 3. Central registry and governed tool/skill use

**Build**

- Present trusted built-ins and administrative shared definitions through one
  registry API. Store system-local proposals and lifecycle metadata in SQLite.
- Pin scoped tool versions; implement system grants narrowed by agent/task grants.
  Expose executable schemas and selected skill content at activation.
- Add a small reviewed fixture tool and the Go/JSON subprocess execution path.
  Validate arguments, outputs, capabilities, deadlines, and artifact access.
- Validate skill dependencies without auto-installing or granting them. Add
  listing, inspection, assignment, revocation, and local draft submission.
  Generated code remains inert source at this milestone.

**Acceptance**

One system can use a granted shared tool/skill while another is denied. Local
drafts remain visible only in their owning system's authorized context. Missing
dependencies, stale assignments, ID shadowing, and revoked versions fail explicitly.
Registry presence alone never authorizes execution.

## 4. Durable events and self-organizing teams

**Build**

- Add typed event envelopes, durable mailboxes, task state transitions, leases,
  acknowledgements/deduplication, bounded retries, and dead-letter records.
- Commit state changes and outbound deliveries together. Persist continuations
  so waiting agents release their worker slot.
- Implement agent proposals, narrowed child grants, asynchronous delegation,
  progress/results, and `user.input` delivery. One active activation per agent.
- Complete per-agent/goal/system pause, resume, and stop behavior, plus audit
  correlation and protection against unbounded agent/event fan-out.

**Acceptance**

Two systems each run an operator and a child using real worker IPC and fake model
responses. Delegation results wake the parent without blocking execution slots.
Duplicate delivery, worker failure, daemon restart, grant revocation, and stopped
systems cannot cause escalation, lost accepted input, or blind side-effect replay.
All agent communication stays on the event/mailbox path.

## 5. Memory, subscriptions, and timers

**Build**

- Add task, agent, and shared-system memory APIs with provenance, revisions,
  retention, and scoped artifact references. Start with SQLite text/structured
  retrieval bounded by context limits.
- Add authorized event subscriptions and one-shot/cron schedules. Choose a
  maintained parser and document its five-field/time-zone/DST behavior.
- Persist occurrence IDs, expiry, trigger budgets, and missed-run coalescing.
  Recheck grants at firing and suppress memory/event feedback loops.

**Acceptance**

Agents recover relevant permitted memories after restart; unauthorized searches
leak neither content nor counts. Timers survive restart, coalesce missed runs,
and do not double-fire. Revocation, expiry, queue limits, and system stop prevent
further unauthorized wakeups. Validate retention behavior on temporary data.

## 6. Detached UI for active control

**Build**

- Add a separate Go UI process with server-rendered forms and authenticated daemon
  API access. Do not place scheduler, registry, or permission state in the UI.
- Implement system create/start, additional context and attachments, inspection,
  pause/resume/stop, and approved configuration/grant/budget changes.
- Show team/task/event views, artifacts, usage, queues, failures, and approvals.
  Add the shared Tool catalog and system-specific Tools pages, including drafts.
- Implement command idempotency, CSRF protection, escaped content, bounded uploads,
  and visible accepted/queued/delivered/rejected states.

**Acceptance**

Drive two systems through actual UI/API requests. Repeated submissions do not
duplicate work. Input survives pause and daemon restart; stopping preserves
history and does not affect the other system. Tool grants and local visibility
match registry rules. Closing the UI neither stops work nor approves requests.

## S. Qualification gate before untrusted execution

This gate is not complete and does not schedule the deferred dependency work.
Revisit the outstanding qualification items in
[README.md](README.md#before-enabling-agent-execution) when explicitly authorized.
Also complete the router/broker/eventing trust-boundary review recorded in the spec.

Require real denial, cleanup, revocation, and delegated-authority checks on every
claimed supported host profile. Diagnostic success or a support flag is not enough.
Keep unsupported profiles and unqualified executable paths disabled.

## 7. Learning and generated-tool promotion

**Build in two increments**

1. Record outcomes and user feedback; produce memory/prompt/skill revision proposals.
   Evaluate against protected baselines within normal budgets. Add approval,
   explicit assignment, version selection, and rollback with UI-visible evidence.
2. **Only after gate S:** build and test system-local Go candidates in quarantine,
   bind promotion to the exact artifact and capabilities, and execute only approved,
   assigned versions. Keep shared publication a separate user-controlled export/import.

Before gate S, source proposals may be stored and inspected, but untrusted candidates
must not be compiled or executed. No online model-weight training or daemon mutation.

**Acceptance**

Show a complete proposal/evaluation/promotion/reuse loop. Rejected candidates stay
unavailable; agent-authored tests cannot replace protected checks. Changed binaries,
replayed approvals, and missing grants prevent execution. Rollback selects an
eligible prior revision without rewriting history or granting other systems access.

## Working rules and completion

- Add only the next milestone's schema, code, and configuration. Extract helpers
  when duplication appears, not a framework for hypothetical future integrations.
- Use unit tests for logic, component tests with real temporary storage/handlers,
  and integration tests across actual runtime boundaries with fake external services.
  Follow [spec section 9](spec.md#9-testing-requirements) and the README commands.
- Run targeted checks during development, then the applicable complete suites at
  milestone exit. Update documentation and status only after the observable result
  and relevant failure paths work.
- Preserve the macOS and Linux/amd64 diagnostic profiles. Add another platform
  only with its own verified profile and integration evidence.
- The configuration-ownership and host-access alternatives in spec section 2.4
  remain unconfirmed. Use the documented conservative defaults; ask before expanding
  those boundaries rather than implementing both choices.
