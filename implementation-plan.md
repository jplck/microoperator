# Microoperator: implementation plan

[spec.md](spec.md) defines the contract and explains the architecture.
This document tracks implementation order and milestone acceptance.
[README.md](README.md) is authoritative for current commands and known limitations.

Implement small vertical slices; retire completed milestone demos from the runtime.
Do not scaffold every future package, table, or interface at once.

## Sequence and status

| Milestone | Status | Depends on | Result |
| --- | --- | --- | --- |
| 0. Worker-launch foundation | Implemented, not fully qualified | None | Sandbox launcher, framing, supervision, and real subprocess checks retained |
| 1. Configuration and daemon state | Implemented, inactive systems only | 0 | Validated configuration and persistent system lifecycle |
| 2. Model broker and one operator | Implemented; extended by 3-4 | 1 | Tracked, rate-limited model turns |
| 3. Scoped tool registry | Implemented, reviewed tools and inert drafts | 1-2 | Granted shared tools/skills and system-local proposal records |
| 4. Durable events and agent teams | Implemented, bounded goal execution | 1-3 | Multiple systems self-organize through governed events |
| 5. Memory and scheduled wakeups | Next | 4 | Scoped retrieval, subscriptions, and persistent timers |
| 6. Detached control UI | Planned | 1-5 | Create, steer, inspect, pause/resume, and stop systems |
| 7. Learning and generated tools | Planned, executable path gated | 3-6; S before untrusted build/run | Evaluated revisions and controlled tool promotion |
| S. Sandbox and delegation qualification | Deferred; not satisfied by milestone 0 | 0, 3-4, and README prerequisites | Permission to enable untrusted execution, not another service |

Milestones 1-6 can be developed and checked using reviewed workers, fixture tools,
and fake providers. This does not authorize untrusted generated code or privileged
autonomous deployment. Do not start the deferred native upgrade as part of ordinary
feature work. Diagnostic fixtures must not become a production bypass for failed
profile checks or an unsandboxed fallback.

## 0. Worker-launch foundation

The phase-0 demo command and placeholder worker have been removed. Retained
infrastructure and coverage:

- [go.mod](go.mod) and `go.sum`: Go module with a pinned nono-go dependency.
- [main.go](main.go): command dispatch, internal sandbox launcher, sanitized
  environment, bounded JSON framing, deadline/cancellation handling, and
  process-group supervision.
- [sandbox_linux.go](sandbox_linux.go) and [sandbox_darwin.go](sandbox_darwin.go):
  platform runtime grants and Linux/amd64's supplemental socket-denying seccomp filter.
- [main_test.go](main_test.go): framing, argument validation, and output bounds.
- [sandbox_integration_test.go](sandbox_integration_test.go): integration-only
  target fixtures and actual subprocess filesystem/connection checks,
  thread/descendant inheritance, descriptor and
  environment isolation, failed setup, deadlines, and cancellation.
- [README.md](README.md): reproducible run/check commands and explicit limits.

The launch foundation was previously exercised on macOS/arm64 and Linux/amd64
WSL2 with the supplemental Linux socket filter. Pipe communication, confinement,
descriptor isolation, inheritance, and cancellation are now exercised using
integration-only targets, without shipping a simulated agent. These results cover
the measured boundaries, not the entire target sandbox contract; macOS was not
rerun on the Linux host. See the [README](README.md#linuxwsl2-recheck) for verified scope.
The complete qualification gate remains open; keep its technical blocker details
in [README.md](README.md#before-enabling-untrusted-execution).

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
Milestone 1 validated provider/skill metadata without stubbing execution. Milestone 2
below supplies model calls and pinned skill context; milestones 3-4 add reviewed
tools and delegation without enabling arbitrary executable definitions.

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

**Implemented scope and evidence**

[execution.go](execution.go) supplies reviewed, confined operator activations,
with correlated private-pipe IPC and a durable goal/task/call identity.
[provider.go](provider.go) implements bounded Chat Completions and SSE consumption;
credentials stay in the daemon, redirects/proxies are disabled, and model text is
never interpreted as code or a tool call.
[broker.go](broker.go) shares durable quota groups across aliases, rotates admission
across systems, and atomically reserves goal/system allowances. Request buckets,
rolling token windows, concurrency, queue limits, cooldowns, explicit 429 retries,
usage uncertainty, and storage-failure denial are enforced.
[execution_store.go](execution_store.go) migrates existing state, retains call
history/receipts, and rejects unrecoverable work visibly rather than replaying
ambiguous effects. Only authenticated starts launch work; stop remains system-scoped.

Component and real daemon/worker scenarios cover fake-provider responses, streaming,
shared admission, throttling, cancellation, command replay, and graceful/abrupt
restart. Unknown outcomes retain reservations; neither restart nor new goals refill
lifetime budgets. Linux/amd64 WSL2 is the checked host; macOS has not been rerun.
See [README.md](README.md#run-one-operator-goal) for commands, fixed bounds, usage
estimates, and remaining qualification limits. Milestones 3-4 extend the original
single-turn slice; currency accounting and generated/untrusted execution remain disabled.

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

**Implemented scope and evidence**

[registry.go](registry.go), [runtime_api.go](runtime_api.go), and
[team_ops.go](team_ops.go) supply one scoped catalog, pinned function schemas and
skill context, immutable private drafts/provenance, explicit assignments, revocation,
and receipts. `runtime.text.analyze` is fixed reviewed Go code, not a demo worker
or an interpreter for proposed source. Its real subprocess uses the selected
profile, a binary digest/fixed argument template, strict inputs/results, and
bounded private artifact ingestion. Administrative executables remain unsupported.

Component and Linux/amd64 WSL2 real-process scenarios cover granted versus denied
tools/skills across systems, scoped artifacts/drafts, missing dependencies,
forged/stale assignments, immutable revisions, ID shadowing, revoked delayed work,
and narrower output permissions. The actual runtime binary is fingerprinted in
the catalog and its tool pin. Agent revisions never rewrite existing task grants.
Local drafts remain inert: evaluation, promotion, and generated execution are
milestone 7/gate S, not implied by registration.

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

**Implemented scope and evidence**

[team_store.go](team_store.go) migrates SQLite to schema 3, preserving prior
receipts, calls, attempts, and reservations. [team.go](team.go) claims durable
mailboxes through one scheduler; [team_ops.go](team_ops.go) commits proposals,
delegation/continuations, tool receipts, and terminal replies transactionally.
[controls.go](controls.go) and [runtime_api.go](runtime_api.go) provide scoped
pause/resume/stop, attributed input, and bounded team/task/event/artifact inspection.
One active goal/system and one activation/agent are enforced. A recipient cannot
lend its broader tools, model, profile, or budget to a narrower task.

Two real systems each run an operator and child with one active slot per system,
ten total fake-provider calls, native text tools, and scoped reports. Results wake
waiting parents without holding slots. Further component/real-process coverage
checks duplicate delivery/command receipts, bounded pre-dispatch worker retries,
unknown subprocess outcomes without replay, waiting-parent and applied-result
recovery, paused input across daemon restart, all three control scopes, and stopping
a waiting team without affecting another system. Model/ancestor/system accounting,
event limits, terminal-delivery failure, and populated v1/v2 migration are covered.

Default tests, vet, and race-enabled integration checks pass on Linux/amd64 WSL2,
including race instrumentation in the actual daemon/worker/tool executable.
macOS was not rerun. The [README bounds](README.md#follow-up-input-and-scoped-controls)
are deliberate: eight turns, fixed fan-out/mailbox limits, non-streaming executable
actions, and a goal lifetime that pause does not extend. Safe queued work/receipts
resume; ambiguous effects and legacy non-resumable calls fail visibly.
This does not qualify generated code, hard resource limits, or orphan cleanup.

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
[README.md](README.md#before-enabling-untrusted-execution) when explicitly authorized.
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
- Preserve the macOS and Linux/amd64 sandbox profiles. Add another platform
  only with its own verified profile and integration evidence.
- The configuration-ownership and host-access alternatives in spec section 2.4
  remain unconfirmed. Use the documented conservative defaults; ask before expanding
  those boundaries rather than implementing both choices.
