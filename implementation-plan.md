# Microoperator: implementation plan

[spec.md](spec.md) defines the contract and explains the architecture.
This document tracks implementation order and milestone acceptance.
[README.md](README.md) is authoritative for current commands and known limitations.

Implement small vertical slices; retire completed milestone demos from the runtime.
Do not scaffold every future package, table, or interface at once.

## Sequence and status

| Milestone | Status | Depends on | Result |
| --- | --- | --- | --- |
| 0. Worker-launch foundation | Implemented; qualification scoped by S | None | Sandbox launcher, framing, supervision, and real subprocess checks retained |
| 1. Configuration and daemon state | Implemented, inactive systems only | 0 | Validated configuration and persistent system lifecycle |
| 2. Model broker and one operator | Implemented; extended by 3-4 | 1 | Tracked, rate-limited model turns |
| 3. Scoped tool registry | Implemented; local promotion added by 7 | 1-2 | Granted shared tools/skills and system-local proposal records |
| 4. Durable events and agent teams | Implemented, bounded goal execution | 1-3 | Multiple systems self-organize through governed events |
| 5. Memory and scheduled wakeups | Implemented, bounded by goal lifetime | 4 | Scoped retrieval, subscriptions, and persistent timers |
| 6. Detached control UI | Implemented, loopback authenticated client | 1-5 | Create, steer, inspect, pause/resume, and stop systems |
| 7. Learning and generated tools | Implemented, system-local; generated path requires S | 3-6; S before untrusted build/run | Protected evidence, exact approval, explicit assignment and rollback |
| S. Sandbox and delegation qualification | Linux resource profile qualified on the checked host; macOS remains blocked | 0, 3-4, and README prerequisites | Required isolation profile for generated work, not another service |

Milestones 1-6 can be developed and checked using reviewed workers, fixture tools,
and fake providers. This does not authorize untrusted generated code or privileged
autonomous deployment. Do not start the deferred native upgrade as part of ordinary
feature work. Diagnostic fixtures must not become a production bypass for failed
profile checks or an unsandboxed fallback.

**Persistence refactor:** Complete. [`internal/state`](internal/state) owns all
production SQL, migrations, scoped artifacts, and typed durable workflows.
Runtime/API/network/process code stays outside that package. Commands, admission,
settlement, mailbox claims, timers, recovery, and learning retain their atomic
boundaries; external effects use typed preparation/completion. SQLite schema 5 and
JSON schema 1 are unchanged. Component checks, integration-tagged vet, and the full
race-enabled real-process suite passed on Linux/amd64 WSL2. Boundary and JSON
compatibility checks guard the extraction; macOS was not rerun. Other databases
remain future implementations, not an added adapter or generic repository layer.

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
in [README.md](README.md#required-profile-for-generated-execution).

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

[config.go](config.go), [internal/state/store.go](internal/state/store.go), and
[daemon.go](daemon.go) implement
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
[broker.go](broker.go) performs provider calls through the durable admission and
settlement operations in [internal/state/broker.go](internal/state/broker.go).
These share quota groups across aliases, rotate admission across systems, and
atomically reserve goal/system allowances. Request buckets,
rolling token windows, concurrency, queue limits, cooldowns, explicit 429 retries,
usage uncertainty, and storage-failure denial are enforced.
[internal/state/execution_store.go](internal/state/execution_store.go) migrates existing state, retains call
history/receipts, and rejects unrecoverable work visibly rather than replaying
ambiguous effects. Only authenticated starts launch work; stop remains system-scoped.

Component and real daemon/worker scenarios cover fake-provider responses, streaming,
shared admission, throttling, cancellation, command replay, and graceful/abrupt
restart. Unknown outcomes retain reservations; neither restart nor new goals refill
lifetime budgets. Linux/amd64 WSL2 is the checked host; macOS has not been rerun.
See [README.md](README.md#run-one-operator-goal) for commands, fixed bounds, usage
estimates, and remaining qualification limits. Later milestones extend this
original single-turn slice. Currency accounting remains unsupported; milestone 7
adds generated execution only on the qualified profile.

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

[internal/state/registry.go](internal/state/registry.go),
[internal/state/tools.go](internal/state/tools.go), and
[runtime_api.go](runtime_api.go) supply one scoped catalog, pinned function schemas and
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

[internal/state/team_store.go](internal/state/team_store.go) migrates SQLite to schema 3, preserving prior
receipts, calls, attempts, and reservations. [team.go](team.go) schedules activations
using durable claims in [internal/state/team.go](internal/state/team.go);
[internal/state/team_ops.go](internal/state/team_ops.go) commits proposals,
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
These team checks alone do not qualify generated code or resource enforcement;
the resource-confined Linux profile is covered separately by gate S.

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

**Implemented**

Schema 4 adds revisioned scoped notes, approval-controlled system facts, durable
occurrences, schedules and subscriptions. Authorization precedes search/pagination;
retrieval is explicitly untrusted data. The pinned five-field cron parser has
documented/tested DST behavior and missed-run coalescing. Only an authenticated
start may authorize a goal lifetime up to 24 hours; all existing budgets remain.
Real daemon/worker checks recover memory and paused timers across restart.
Component checks cover unauthorized reads, shared approval, expiry/deletion,
deduplication, grant revocation and trigger bounds. Logical memory retention does
not promise erasure of separately retained task/command history or external backups.

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

**Implemented**

`ui --socket ... --listen 127.0.0.1:8080` runs a separate Go HTTP client/server.
It requires distinct browser/daemon credentials, validates Host/Origin and CSRF,
escapes content, bounds inert text uploads and preserves command keys on retry.
System/team controls, memory, schedules, scoped catalogs, learning evidence and
approvals use only the daemon API. Real UI/daemon processes control two systems;
repeated creation deduplicates and daemon work completes after UI shutdown.

## S. Qualification gate before untrusted execution

The requested Linux enforcement work is implemented in `resources_linux.go`,
the launcher/supervisor, and supplemental seccomp rules. It uses delegated cgroups
and private namespaces directly, not containers or a native-library upgrade.
The optional profile enforces aggregate activation CPU/memory/PID and workspace
limits, sealed parent-death cleanup, cancellation of escaped process groups,
descriptor-based artifact access, and scoped abandoned-workspace cleanup.
Reviewed profiles without `resources` are not upgraded implicitly.

Require real denial, cleanup, revocation, and delegated-authority checks on every
claimed supported host profile. Diagnostic success or a support flag is not enough.
Keep unsupported profiles and unqualified executable paths disabled.

`TestResourceQualification` runs in an automatically collected delegated user
unit and asserts actual kernel throttling/OOM/PID-denial counters, combined
scratch/output exhaustion, sealed namespace controls, escaped descendants,
abrupt supervisor/daemon death, and artifact/accounting recovery. Missing required
host support fails the integration run. macOS resource enforcement and its
resolver allowance remain blocked; no macOS qualification is claimed.
Milestone 7 must require this qualified Linux resource profile for build/run,
while preserving exact-artifact approval and the phase-4 delegation restrictions.
`TestGeneratedBuildQualification` compiles a stdlib-only Go candidate in confinement
and checks actual generated-program filesystem/network/environment denials.
Changing the toolchain pin fails closed. Ordinary reviewed profiles are not an
alternative generated-code path.

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

**Implemented**

Schema 5 adds immutable protected suites, user feedback, evaluation provenance,
exact approvals and durable task-use allowances. Skill/prompt-fragment comparisons
run through the ordinary model broker with creation/delegation ancestor budgets.
Agents with the explicit evaluation grant wait/resume on durable mailbox events;
they cannot edit protected inputs, approve their own proposals, or gain authority
through a successful check.

Generated source implements a fixed JSON text transformation ABI. Builds use the
complete pinned local Go tree, cgo disabled, no downloads/secrets, isolated caches,
and the qualified resource profile. Protected black-box checks execute fresh
confined binaries. Approval binds immutable evidence, source, binary, build/runtime
identity and capabilities; assignment and rollback are separate revision commands.
Large binary artifacts remain outside SQLite. Shared executable publication is
not supported; local imports into another system require fresh evaluation/approval.

Real-process scenarios cover proposal/evaluation/resume, private skill reuse after
restart, explicit version selection and rollback without historical rewrites,
failed candidates, approval-use limits, scoped generated execution, modified
binaries, revocation and cancellation of an active confined builder. Component
checks cover protected-case/evidence immutability, input-tampering denial, atomic
budget rejection, expiry, stale configuration approvals and ambiguous-build recovery.

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
