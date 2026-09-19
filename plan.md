# Microoperator: architecture plan

Status: design only. No runtime implementation.

## 1. The idea

One Go daemon hosts independent agent systems. Each system has a prompt-driven
operator agent that receives the user's goal, proposes a team, and coordinates
the work. Other agents may propose agents,
delegate tasks, create wakeups, remember useful results, and write extensions.
The daemon, not an LLM, decides whether those actions are allowed and executes them.

The operator is the default coordinator, not a mandatory relay for every message.
Agents address one another directly, but every delivery crosses the same trusted
governance boundary. The UI is an optional client of the daemon.

The central distinction is **agents propose; the runtime authorizes and performs**.
Self-organization is a feature inside an approved capability and resource envelope,
not permission to rewrite that envelope.

Systems have separate `system_id` scopes, memory, permissions, and budgets.
They share the daemon and provider quota limits, not automatic access to one
another's data. Creating a system does not launch another daemon.

## 2. Deliberate constraints

- One long-running daemon, one local SQLite database, no containers or external
  queue. Sandboxed worker processes are children, not additional services.
- Project-owned runtime, workers, extensions, and UI server are Go. Prefer embedded
  runtime controls and a small server-rendered UI over separate infrastructure.
- Use `github.com/nolabs-ai/nono-go` as the binding to nono, not its CLI.
  Project code stays Go, but the binding uses cgo and nono's native Rust library;
  this is an explicit exception to a pure-Go dependency tree.
- Agents are versioned data: prompts, model settings, tools, skills, memory access,
  event subscriptions, and limits. An agent does not require its own codebase.
- Agent-generated code runs out of process. Never load generated Go plugins or
  generated code into the daemon.
- Keep agent communication behind one event-delivery boundary. Do not add alternate
  peer protocols or make scheduling, memory, and every function into a plugin system.
- Start as a single-user, single-machine system. Do not claim hostile multi-tenant
  isolation or distributed fault tolerance.

## 3. Ownership and process model

```text
  CLI / optional browser UI
            |
       authenticated API + event stream
            |
  +--------------------- Go daemon --------------------------+
  | User goals / approvals / stop controls                    |
  | Agent/tool records + immutable revisions                  |
  | Supervisor + durable task/mailbox scheduler              |
  | Capability checks + approvals + shared LLM limits         |
  | Governed event router / durable mailboxes                 |
  | Model broker / tool broker / memory access                |
  | SQLite: state, events, deliveries, grants, audit           |
  +------------+-------------------+-------------------------+
               | authenticated, scoped IPC
       +-------+-------+   +-------+-------+
       | nono sandbox |   | nono sandbox |
       | Go operator  |   | Go worker    |  ... on demand
       +---------------+   +---------------+
               |
       generated extension jobs are separately sandboxed
       and launched by the daemon, never loaded into it
```

The diagram shows logical components, not separate services.

**Trusted daemon:** persistence, permission checks, identity, process supervision,
credentials, allowed model/provider access, tool mediation, and approval handling.

**Untrusted workers:** prompt-following agent loops, retrieved content, tool output,
agent-written prompts, and generated programs. The initial operator worker has
more coordination rights, but no special ability to bypass the daemon.

A worker is an activation of an agent, not the agent's durable identity. It receives
a pinned agent revision, task context, and a narrowly scoped capability set. On
completion or suspension it can exit; durable state lives in the daemon.
Initially allow only one active activation per agent to avoid concurrent memory
and conversation mutation. Different agents run concurrently.

Use one Go executable with daemon, sandbox-exec, and worker subcommands. The daemon
starts a fresh sandbox-exec child with a trusted capability profile and a pinned
target executable. That launcher builds a `nono.CapabilitySet`, grants only the
required paths, sets `nono.NetworkBlocked`, and calls `nono.Apply`.

Apply restrictions on a locked OS thread and immediately exec the worker or tool
on that same thread, without unlocking or running agent logic in between. This
avoids treating one restricted Go thread as a sandbox for an already-running
multithreaded worker. Verify inheritance across exec, new threads, and descendants
on each supported platform in phase 0.

`Apply` is irreversible: never call it in the daemon. Reject unsupported profiles
and exit on setup/apply/exec errors; there is no unsandboxed or CLI fallback.
Only after successful confinement may the target receive task content. Reuse this
launch path for agent workers, configured tools, extension builders, and generated
binaries. Microoperator owns supervision and brokering; no nono supervisor is
required.

Use bounded, framed stdin/stdout IPC so workers need neither a listening
socket nor general network access. Bind identity to the process/session established
by the daemon, not a sender ID supplied in a request. Keep diagnostic output on
stderr, with size limits and secret redaction.

Workers call the model broker instead of receiving provider credentials. Workers
request tools, memory operations, messages, and lifecycle changes through the
broker. Broker calls are structured requests, not arbitrary daemon-side shell
commands.

## 4. Configurable agents, tools, and skills

Bootstrap from user-owned, versioned JSON configuration: the initial operator,
approved model providers, shared LLM limits, shared tool definitions, sandbox profiles, and
capability grants.
Use Go's JSON support rather than adding a configuration language. Secrets stay
outside agent definitions and are resolved only by the trusted broker.
Agent-generated definitions arrive through the API with narrower authority; they
cannot edit bootstrap configuration.

An agent revision contains:

| Field | Purpose |
| --- | --- |
| Identity and lineage | Stable ID; revision; creator; parent; owning user goal |
| Prompt and skills | Instructions plus pinned, read-only skill content |
| Model selection | Approved provider/model and token limits; no raw API keys |
| Tools | Pinned scoped catalog references and argument constraints |
| Memory | Read/write scopes, retention, and allowed shared collections |
| Wakeups | Approved subscriptions, schedules, and permitted event types |
| Capabilities | Agent creation, delegation, code proposals, and other privileges |
| Limits | Lifetime, turns, concurrency, descendants, event volume, and spend |

Creation produces a revision and an authorization record. Editing an agent creates
a new revision; it does not silently change a running activation. Names and prompts
are descriptive, never proof of authority.

Expose a scoped directory of agent capabilities and availability so agents can
choose an existing collaborator before proposing another one. Directory entries
are runtime-owned advertisements, not authorization grants.

The operator can propose a researcher and a builder for a goal. The daemon validates
their definitions, computes capabilities within the caller's delegable grant and
the goal's remaining budget, and starts them only after authorization. A child
cannot acquire a capability by writing it into its own configuration.

All descendants share the goal's aggregate budget and quota. Per-agent limits are
additional caps, not a way to multiply the parent's allowance. Explicit user grants
are the only escalation path.

**Tool configuration** maps stable tool IDs to execution details: executable and
fixed argument template or approved remote integration, input/output schema, effect
class, timeout, filesystem/network requirements, and secret references. Validate
arguments at the boundary; a shell string is not a tool schema.

Start with built-in runtime tools and explicitly configured Go/JSON subprocess
tools. Each subprocess tool runs in an appropriate nono sandbox. Add an MCP adapter
when a needed integration requires it; MCP is a tool protocol, not a replacement
for agent messaging or authorization. Remote tools have external side effects that
the local sandbox cannot undo.

A skill is instruction content plus declared requirements, not a permission grant.
Loading a skill cannot install tools, execute hooks, obtain secrets, or expand
capabilities. Agent-authored skills follow the same revision/promotion path as
agent-authored prompts.

### One registry, scoped ownership

Use "tools" as the catalog umbrella for executables and skills, with an explicit
kind distinguishing callable operations from instruction/context content.
Maintain one daemon registry API over two sources:

| Scope | Source | Availability |
| --- | --- | --- |
| Shared | Trusted built-ins and administrative JSON definitions | Explicit grants to systems, narrowed for each agent/task |
| System-local | Agent/user proposals and revisions in SQLite | Only within the owning system's authorized context |

Entries carry IDs, immutable versions, provenance, descriptions, content/artifact
digests, capability requirements, dependencies, and lifecycle state. Executable
manifests include schemas and execution details. Skills declare required tools;
loading them never automatically installs or grants those dependencies.

System grants select eligible tools; agent/task grants narrow that set. Resolve
and pin exact scoped versions when creating revisions, expose their function
schemas or skill content at activation, and recheck every call in the broker.
Local entries cannot shadow trusted IDs, and agents cannot query other systems'
private catalogs or source artifacts.

Agent-created entries progress from local draft through evaluation and approval
to active versions, with failed/rejected/disabled states visible. Becoming active
does not itself grant execution: explicit system and agent assignment is required.
Generated executable tools use the sandboxed build/promotion path below; no runtime
plugin is loaded into the daemon. Revocation blocks future calls to pinned versions.

Publishing a local tool as shared requires a separate user-controlled action and
review of the content being shared. With the current JSON-owned defaults, export
a reviewed manifest/artifact and explicitly import it into administrative config.
Do not expose private system data or automatically grant the shared tool to any
system. See `spec.md` section 2.6 for the registry and assignment contract.

## 5. How agents speak: addressed tasks and durable events

Use a small durable event system as the runtime's communication substrate.
Task delegation, replies, progress, wakeups, and external triggers all enter through
the same authorization and delivery path. A scheduler is a producer of events,
not a second way of invoking an agent.

### Communication primitives

| Primitive | Meaning |
| --- | --- |
| Delegate | Ask a named agent to perform work; return a task ID immediately |
| Reply / progress | Correlate a message, artifact, or status with an existing task |
| Publish | Emit a typed event to an explicitly authorized audience/subscription |
| Schedule | Register a future or recurring event, within the caller's grant |
| Subscribe | Request a filtered wakeup subscription within authorized scopes |

Default to addressed delivery. Subscriptions are explicit; there is no ambient
global broadcast every agent can read. A recipient can reject a task or request
more input. An agent may propose a different team, but acceptance and spawning are
separate governed actions.

Tasks are coordinated by addressed events carrying task IDs, status updates, and
artifact references. The same mechanism carries timers, tool completions, and
memory-change notifications.

Persist an explicit task state: queued, running, waiting for input/result/approval,
completed, failed, canceled, or rejected. Worker exit is not task completion.
Waiting states retain their wakeup condition and deadline; terminal states cannot
be silently reopened by a late or duplicate message.

Each event has:

- A runtime-assigned ID, type, schema version, timestamp, and expiration.
- An authenticated source; authorized destination or subscription scope.
- Root goal, task, correlation, and causation IDs.
- A bounded payload or an access-controlled artifact reference.
- Data classification, authorization reference, and trace metadata.

The daemon stamps trusted fields. Agent-provided labels are claims to validate;
they cannot declassify data or impersonate another sender.

### Direct, but governed

```text
agent A -> send request -> permission checks -> durable acceptance
        -> router -> permission recheck -> agent B's mailbox
```

"Direct" means A addresses B without the operator interpreting or rewriting the
message. It does not mean an unobserved worker-to-worker socket. Arbitrary peer
networking would defeat the governance requirement and is denied.

The router delivers through local SQLite-backed mailboxes and sandboxed worker IPC.
This is the only agent-to-agent communication mechanism in v1. Keep authorization,
durable acceptance, and delivery in that path; there are no per-agent network
servers or external agent-protocol adapters.

> **Reminder for later:** Review the router/broker/eventing trust boundary before
> enabling privileged delegation. Prevent confused-deputy bypasses: bind sender
> identity to the session, constrain delegated work to its task grant, and authorize
> returned data. Wakeups must never execute agent code inside the daemon. Add denial
> checks demonstrating these boundaries; nono alone does not enforce them.

### Delivery guarantees

- Acceptance means the event and intended deliveries are durably recorded in one
  SQLite transaction. It does not mean the work has completed.
- Delivery is **at least once**. Use event IDs and per-recipient delivery records
  for deduplication; task completion and resulting outbound events commit together.
- External side effects need tool-specific idempotency keys or reconciliation.
  After an ambiguous timeout, record an unknown outcome rather than blindly
  retrying a non-idempotent action. Exactly-once side effects are not promised.
- One mailbox has a defined enqueue order. There is no global completion ordering.
- Enforce bounded queues, payload limits, retry backoff, attempt limits, and a
  visible dead-letter state. No unbounded retry loops or silent drops.

Delegation is asynchronous. A worker can persist a waiting condition and exit,
freeing its execution slot. A child result wakes the parent. Avoid synchronous
waits that consume all available worker slots or deadlock parent/child teams.
Record continuation state, pending call IDs, and completed tool results so a crash
does not automatically replay already performed side effects.

## 6. Eventing and wakeups

Persist timers and subscriptions in SQLite. The daemon runs a timer loop and claims
ready mailbox work; it does not create one goroutine or OS cron entry per agent.

Initial trigger types:

1. Addressed messages and task status changes.
2. One-shot timers and recurring cron schedules.
3. Authorized subscriptions to typed internal events, including tool completion
   and memory-change notifications.

An authenticated webhook adapter can be added when an actual external source is
needed. The UI and CLI submit through the same API as other trusted producers.

An agent-created schedule records its owner, target, event template, time zone,
expiry, creator grant, and remaining trigger budget. Cron uses an explicit IANA
time zone, default UTC, and a documented five-field grammar. Define DST behavior
and pin the parser's tested semantics. Use a maintained Go cron parser rather than
writing one.

On daemon restart, recompute due schedules from persisted state. Default missed-run
behavior is one coalesced catch-up event, not replaying every missed minute. Use a
unique schedule/occurrence key to prevent double firing. Timers have deadlines;
in-process waits use monotonic time where available, while persisted schedules
necessarily use wall time.

Check authorization both when a schedule/subscription is created and when it
fires. Revoked agents and expired grants cannot keep running through old timers.
Only the user or an explicitly privileged operator can authorize standing
background work beyond a goal's lifetime.

Prevent event-driven runaway behavior with per-goal and per-agent rate limits,
bounded fan-out, causation depth, maximum autonomous activations, and queue
backpressure. Creating agents, subscriptions, or new event IDs does not reset these
limits. Memory events need origin tracking and coalescing to avoid feedback loops.

## 7. Runtime controls and shared LLM rate limits

No general policy middleware or external decision service for now. Keep ordinary
Go checks for capability grants, scoped data access, approval requirements, and
resource limits at the existing runtime operation boundaries. Unknown identities,
missing grants, invalid requests, and failed checks deny the operation.

### Put LLM admission in the model broker

All LLM calls pass through one daemon-owned model broker, including calls from
the operator, child agents, evaluation runs, and learning workflows. Workers never
receive provider credentials or a direct network route that bypasses this broker.

```text
worker -> model broker -> bounded queue -> shared limits + budget reservation
       -> provider API, or configured external gateway -> provider API
```

Start with rate limiting **inside the runtime**: it already owns all calls and
knows which goal pays for them. No extra service is needed. The broker controls
admission before sending a request, not merely how quickly it retries a rejection.

| Configured control | Scope and behavior |
| --- | --- |
| Requests per minute and allowed burst | Shared provider/account quota group; pace requests and count every dispatched attempt |
| Tokens per minute | Shared quota group with provider-specific accounting; reserve estimated input plus bounded output before dispatch |
| Concurrent requests | Cap in-flight calls; streaming calls occupy a slot until closed |
| Queue length and maximum wait | Bound pending work; expiration/cancellation returns an explicit result |
| Goal/agent token and spend budgets | Separate lifetime or configured-period caps; descendants share their root goal's allowance |

Define quota groups in user-owned provider configuration. Map credentials and
models to the actual shared account/project/model quota; a new agent, API key, or
model alias must not accidentally create a fresh allowance. Apply every relevant
group when a provider has both account-wide and model-specific limits.
Validate rates, burst sizes, output caps, and queue bounds at startup.

Use one shared limiter state per quota group, not a limiter per worker. Admit only
when all applicable rate, concurrency, and budget checks pass. Reserve budgets
atomically in SQLite before dispatch; concurrent agents cannot independently spend
the same remaining tokens. Give roots a fair queue, for example round-robin by
goal with FIFO within each goal, so spawning more agents does not buy more priority.
Waiting for capacity must not occupy an in-flight provider slot.

Token estimation is provider-specific and is not proof of the provider's exact
count. Cap requested output, use conservative estimates and configurable headroom,
and reject a request that cannot fit the configured limit instead of queuing it
forever. Reconcile spend and task token reservations with reported usage.
Rate-limit accounting follows the provider's rules: do not automatically refund
rate capacity just because actual output is smaller than its reserved allowance.
Unknown usage remains conservatively charged/reserved until reconciled, not zero.

Keep spend budgets distinct from rate limits. A request may fit the current
minute's allowance but exceed the goal's remaining budget. Currency estimates need
configured model pricing; unsupported usage/pricing must be surfaced rather than
presented as an exact hard cost guarantee.

### Backpressure, retries, and recovery

On provider throttling, honor valid `Retry-After` and supported quota-reset signals
for the affected quota group. Without a usable signal, use bounded exponential
backoff with jitter. Repeated failures do not create an unbounded retry loop.
Every retry re-enters admission, consumes any applicable provider allowance, and
stays within the original call's deadline and goal budget.

Have one retry owner, initially the broker; disable overlapping SDK/gateway retries
where possible and account for any unavoidable upstream retries. A disconnected
stream or ambiguous timeout may already have incurred usage. Record that outcome;
do not silently replay it as if no call happened or duplicate emitted tool calls.
Cancellation before dispatch releases reservations; cancellation after dispatch
does not imply the provider stopped or billed nothing.

Persist pending call IDs, budget reservations, dispatch/usage records, and active
cooldowns. On restart, reconcile in-flight outcomes and restore conservative rate
state; do not reset to a full burst or reset a goal's spend. Reject or expire work
that cannot be recovered safely, with an observable reason.

Expose queue wait, in-flight calls, quota-group utilization, tokens/cost, throttles,
retry counts, and budget exhaustion in the API/UI. Rate waiting, exhausted budget,
and a failed provider call are different visible states.

### Optional external gateway

If a gateway already exists, configure its approved base URL and credential
reference as the broker's provider endpoint; retain the required model API
semantics. Do not build a gateway or add a gateway abstraction just for this.

A gateway becomes useful when multiple runtimes or other applications share the
same provider quota. It can enforce aggregate upstream limits; the runtime still
owns per-goal budgets, fair admission, cancellation, and backpressure. Without that
shared enforcement, local limits cannot account for other clients' usage.
If both layers limit traffic, document their scopes and retry ownership to avoid
stacked waits and retries. A gateway outage never triggers an automatic direct
provider bypass.

### Permissions, approval, and revocation remain

Removing policy middleware does not remove sandboxing or permission checks.
Check agent creation, tool use, messaging, memory, schedules, subscriptions,
extension promotion, and administrative operations against explicit grants.

Approval suspends an operation in durable state. Bind it to the authenticated
approver, exact request, target, agent/grant revision, expiry, and one-time use
where appropriate. Material changes invalidate it. Recheck current grants before
resuming delayed work, including queued model calls.

Only the user/admin can increase grants or budgets. Agents may propose changes
but cannot activate them or edit authorization/audit state. Revocation stops new
actions and terminates running work where required; it cannot undo completed
external effects or retract data already disclosed.

## 8. Sandboxing and host safety

Sandbox the initial operator and every other agent activation, not only generated
code. Giving an agent a prompt-level rule is not sandboxing it.

Each activation receives a private working directory, pinned read-only inputs, an
explicit writable scratch/output area, a sanitized environment, and bounded IPC.
Do not expose the daemon's database, credentials, control configuration, or other agents'
workspaces. Artifacts leave the sandbox only through validated, scoped ingestion;
canonicalize paths and reject symlink/path-traversal escapes at this boundary.

Workers have no general outbound network access by default. The daemon's broker
performs authorized provider and remote tool requests, enforcing endpoint allowlists,
redirect handling, request limits, and credential scope. Do not expose an arbitrary
URL-fetch or arbitrary host-shell escape through an otherwise narrow broker.

Use nono for its verified filesystem/network/process restrictions. Keep OS resource
enforcement explicit and separate from claims about sandboxing: execution deadlines,
output caps, bounded concurrency, process-tree cleanup, and any supported CPU,
memory, process-count, and disk controls.

### nono-go integration and constraints

The project is now [nolabs-ai/nono](https://github.com/nolabs-ai/nono), formerly
`lukehinds/nono`. Integrate through [nono-go](https://github.com/nolabs-ai/nono-go),
which binds its native library. It uses Linux Landlock and macOS Seatbelt; it is
not a VM and does not require containers.

- Build the runtime with `CGO_ENABLED=1` and a C toolchain. The binding supplies
  native libraries for Linux/macOS on amd64/arm64. Pin the binding and verify the
  bundled-library provenance for each target. Generated extensions remain pure Go
  by default; their separate `CGO_ENABLED=0` build does not include this binding.
- Normal builds use the bundled libraries without Docker. If rebuilding the native
  core is necessary, use a native Rust toolchain on the target host; do not adopt
  the upstream helper's Docker-based Linux build path.
- Use `IsSupported` and `SupportInfo` for initial diagnostics, then verify the exact
  requested controls. A support flag or `QueryContext` permission preview is not
  proof that a running workload is confined.
- Linux support is capability-dependent, not just a minimum-kernel check. Landlock
  filesystem support starts at kernel 5.13; additional network and process-scoping
  features require newer ABIs and matching binding/native-core support.
- macOS support uses `sandbox_init()`, documented by nono as a private Apple API.
  Treat macOS updates as compatibility events and rerun boundary checks.
- Set `NetworkBlocked` explicitly and test actual network access, including
  non-TCP paths and inherited descriptors. The upstream CLI documents additional
  Linux seccomp protections; do not assume the binding includes those protections
  or that Landlock alone blocks every network path.
- The inspected binding's Linux-amd64 library pins nono core 0.65.1, older than the
  researched CLI v0.78.0. CLI feature claims are not binding guarantees. Pin and
  inspect the native core, not merely the Go API version.
- The binding's `Apply` is an irreversible self-sandbox operation, not a remote
  process manager or per-goroutine boundary. Use the isolated launch path above;
  do not grant broader rights or silently continue if a profile cannot be applied.

Hard aggregate CPU/memory/process/disk limits need an explicit, tested host
mechanism, for example delegated Linux cgroup limits where available, plus storage
limits. They are not implied by nono confinement or Go contexts. On a host that
cannot enforce a required limit, disable that workload profile; do not describe
best-effort monitoring as a hard resource guarantee.

If the OS and pinned nono-go/native-core combination cannot enforce a required
capability profile, refuse to start that workload. Updating the binding/native core
is a prerequisite, not a reason to bypass it. Report unsupported profiles in
daemon health and the UI. Pick and test a supported host profile before enabling
autonomous generated code.

The threat boundary is an agent/code process against the host resources it was
not granted. It does not protect against a compromised host administrator or a
kernel exploit. Local controls cannot enforce what a remote model or external tool
service does after receiving authorized data.

## 9. Memory and learning

Use SQLite plus a local artifact directory first. The daemon owns both; workers
use governed APIs. Use `database/sql` with a maintained pure-Go SQLite driver
such as `modernc.org/sqlite` to avoid adding another native-language dependency.
Do not add a vector database, memory service, or embedding pipeline initially.

Keep three logical kinds of memory:

| Memory | Contents | Write rules |
| --- | --- | --- |
| Task state | Conversation, pending work, tool receipts, checkpoints | Owning task/agent |
| Agent memory | Notes and reusable observations | Owning agent, versioned |
| Shared knowledge | Reviewed facts, procedures, versioned skill references | Scoped, approval-controlled promotion |

Entries include provenance, author, goal/task references, timestamps, classification,
confidence/evidence, version, and expiry. Treat retrieved text as untrusted data,
not higher-priority instructions. Imported content cannot grant capabilities.

Start retrieval with scope filters, structured keys, and SQLite text search.
Authorization precedes disclosure, including search snippets, artifact content,
and counts. Add embeddings only if retrieval quality demonstrates the need.
Define retention, deletion, and backup behavior, including how deleted content
ages out of backups and indexes. Audit should avoid copying full sensitive payloads.

"Self-learning" initially means a recorded improvement loop, not online changes to
model weights:

```text
run -> observed outcome -> proposed memory/prompt/skill/code revision
    -> sandboxed evaluation -> authorized promotion -> new pinned revision
```

Store failures as well as successes. Compare candidates with a fixed baseline and
bounded evaluation cases. Agents may propose tests, but cannot edit the protected
acceptance checks used to approve their own capability expansion.

Low-risk, scoped notes can be admitted automatically within existing grants. Executable code,
shared instructions, tool changes, and increased permissions require explicit
promotion; default executable promotion to human approval. Rollback changes the
active revision, not historical task records. Learning never mutates the daemon
binary or silently edits permission and limit configuration.

## 10. Agent-generated Go extensions

An extension is a versioned Go subprocess tool with a narrow JSON input/output
contract. Its manifest declares schemas, required capabilities, resource limits,
and its source/build artifact digest. There is no dynamic library loaded into the
daemon and no plugin installation hook running with daemon privileges.

Promotion pipeline:

1. The agent submits source and a manifest into a quarantined artifact namespace.
2. The daemon validates size, allowed inputs, and the proposed capability profile.
3. A nono-isolated builder compiles and checks it with a pinned Go toolchain.
4. A fresh sandbox runs protected acceptance checks with bounded test inputs.
5. Runtime checks and, by default, user approval admit the exact artifact and capabilities.
6. The daemon registers an immutable tool version; an agent revision may select it.

The builder has no secrets or outbound network. Start with standard-library-only
extensions, `CGO_ENABLED=0`, a pinned local toolchain, and an isolated build cache.
Do not run `go generate`, arbitrary install scripts, automatic toolchain downloads,
or agent-requested dependency fetches. Dependency expansion is a separate approval
and supply-chain step.

Tests execute arbitrary code too and must remain sandboxed. Successful tests are
evidence, not a security proof. Validation failure quarantines the candidate and
reports a reason; it does not trigger an automatic permission expansion.

Source, toolchain identity, build settings, test evidence, and the exact promoted
binary stay linked. Rebuilding or modifying it requires a new revision/promotion.
Generated tools have no broker access by default; any needed access uses a separate,
scoped session and the same permission checks. Their child processes cannot acquire broader rights.

## 11. Persistence, recovery, and operational controls

One SQLite database stores agent revisions, tasks, mailbox deliveries, events,
schedules/subscriptions, grants/approvals, limit configuration, model-call usage
and reservations, memory metadata, extension promotions, and audit records.
Large immutable artifacts live outside the database and are referenced by digest
and access scope.

Use transactions for local state transitions and outbound event recording. A
claimed activation has a lease and owner; on restart, reconcile abandoned work
before resuming it. Track spawned process trees and ensure daemon failure does not
leave ungoverned orphan workers consuming resources. Do not simply retry a task
whose external action outcome is unknown.

Persist authorizations and state-changing audit entries before dispatch. Storage
failure, full disk, or failed authorization/accounting blocks new privileged work
and surfaces an error. Safe stop/cancel controls must remain available even when
accounting or audit writes fail; report emergency termination through the remaining
diagnostic channel.

Provide per-agent, per-goal, per-system, and global pause/cancel controls. Pause stops new
activations; cancel additionally revokes grants, cancels pending wakeups, and stops
running workers. Distinguish graceful cancellation from forced termination.

Logs and traces correlate system -> goal -> task -> event -> decision -> activation -> tool.
The initial audit trail is durable and append-only through the application, not
tamper-proof against a host administrator. Signed/off-host audit export is a later
requirement only if the deployment needs independent evidence.

## 12. Optional, detached UI

The daemon exposes a versioned control/query API and a resumable event stream.
Keep API resources aligned with runtime concepts: systems, goals, agents, tasks, messages,
grants/limits, model-call usage, approvals, schedules, memory, artifacts, and tool revisions.
Commands pass the same authorization checks regardless of CLI or UI origin.

The first UI is a separate Go process serving server-rendered HTML with forms
and periodic refresh. It connects to the API; it owns no scheduler, permission state,
or orchestration logic. No JavaScript application or frontend build toolchain is
required. The API's event stream remains available for later clients.

The UI is a control surface, not just a monitor. Required interactions:

| Action | Behavior |
| --- | --- |
| Create and start a system | Select the operator configuration, supply an initial goal/context, set permitted capabilities/budgets, and start through the API |
| Inspect systems | Show system states, team graphs, task/message timelines, artifacts, schedules, worker status, budgets, LLM queues/throttling, and failures |
| Add information | Send instructions, corrections, answers, and scoped attachments to the operator, or an explicitly selected authorized task/agent |
| Pause / resume | Stop new activations while retaining state; resume eligible work under current grants and budgets |
| Stop a system | Cancel pending tasks/wakeups, revoke execution grants, and stop workers in that system only |
| Steer work | Approve/reject requests and learning proposals, edit future agent revisions, and adjust authorized grants/budgets |
| Manage tools | Browse shared catalog entries, grant/revoke system access, inspect local drafts/results, approve/reject, assign versions, and roll back |

Provide a global **Tool catalog** for shared definitions and a system-specific
**Tools** page. Separate granted shared tools from locally created tools, including
drafts, failed evaluations, pending approvals, active and disabled versions.
Show kind, creator/provenance, source or skill content, schemas/dependencies,
requested permissions, evaluation evidence, assignments, and usage. Local entries
remain in their system context, not globally available merely because the registry
is central. The UI manages grants and local lifecycle; shared definitions retain
their administrative JSON authority, with explicit export/import for publication.

Persist follow-up information as an attributed `user.input` event. Deliver it at
the next safe agent turn rather than rewriting hidden conversation state or
pretending it can undo a running external action. The UI shows accepted/queued,
delivered, or rejected input with reasons. Paused systems retain input until resume;
stopped systems reject input until explicitly started. Attachments use the same
bounded, authorized artifact ingestion as other content.

Pause lets existing work finish. Stop first requests graceful cancellation, then
terminates remaining workers after a bounded grace period; show `stopping` until
cleanup completes. Stop is not deletion: retain memory, artifacts, and history.
Starting a stopped system creates a fresh goal/run under current grants, without
replaying canceled tasks/timers or resetting the system's consumed budget.

Every UI action is an authenticated API command with an audit record. Use command
idempotency keys to handle double clicks and reconnects without duplicate systems,
runs, or input events. Show permission, queue, budget, and provider failures
explicitly. Clearly distinguish system stop from daemon-wide controls.

Default local access: a restricted Unix socket for CLI/local integrations and an
authenticated loopback connection for the browser UI. Do not rely on loopback alone
as authentication. Protect state-changing browser requests against CSRF and render
agent output as escaped text. No unauthenticated network listener by default.

The daemon works headlessly. Disconnecting or crashing the UI does not stop a goal
or approve an action.

## 13. Small implementation sequence

These are implementation gates, not a request to build now.

| Phase | Smallest useful result | Required evidence before advancing |
| --- | --- | --- |
| 0. Boundary spike | nono-go sandbox-exec launcher; scoped broker IPC; explicit permission checks | Pinned native-library provenance, filesystem/network denial, exec/thread/descendant inheritance, required resource caps, capability identity, cancellation, and fail-closed launch verified on the chosen host |
| 1. Single agent + LLM limits | Daemon, SQLite, CLI goal submission, one prompt agent, shared tool registry, rate-limited model broker, one tool | Shared RPM/TPM and concurrency caps, queue bounds, streaming/cancellation, throttling cooldowns, retry accounting, and restart-safe budgets exercised against a controllable fake provider; credentials stay out of workers |
| 2. Small team | Governed agent creation, system/agent tool grants, addressed tasks, durable mailboxes, delegation | Child cannot escalate; two agents exchange artifacts; duplicate delivery and waiting parents behave correctly |
| 3. Wakeups | Timers, cron, subscriptions, pause/cancel, event limits | Restart, missed cron runs, revocation, event storms, and queue limits are exercised |
| 4. Memory + UI | Scoped retrieval and detached UI for system controls, shared tool access, and approvals | Two systems can be controlled independently; follow-up input survives pause/restart; repeated commands do not duplicate work; tool grants and approval replay checks hold; daemon remains usable without UI |
| 5. Generated extensions | System-local tool proposals, Go build/test/promote/assign/rollback, and local Tools view | Shared/local grants and visibility hold; failed candidates stay quarantined; only assigned approved versions run; publication is explicit |

Use Go's standard test runner when implementation begins. Each phase needs a small
set of executable checks for its boundary and failure modes, not a new test framework.
Generated extensions can wait until the sandbox and broker boundary are demonstrated.

## 14. What deliberately waits

No distributed scheduler, container platform, Kubernetes, separate event broker,
vector database, hot-loaded Go plugins, online model fine-tuning, unrestricted
agent networking, automatic dependency installation, or self-modifying control
plane. Add infrastructure only when a measured constraint demands it.

OPA/Rego middleware and a general policy decision API are out of
scope for now. Consider a policy engine only when concrete authorization rules
outgrow explicit runtime checks. Consider an external LLM gateway when multiple
clients need shared upstream limits, not simply because agents run concurrently.

Keep the extension points the experiment actually needs: configurable agent/tool
definitions, governed event delivery through durable mailboxes, and configurable model
endpoints with shared rate limits.

## 15. Research baseline and primary sources

Checked 19 September 2026. This is source-level compatibility research, not a
runtime sandbox test or security certification. Pin actual dependencies and
rerun the phase-0 checks when implementation starts or those dependencies change.

The binding snapshot is nono-go commit
`9ba65a11c842eed3644dcd2fb008a4a3f119f680`; its cited Linux-amd64 library records
native core commit `1d1c88c9f98f0a1f3ff79cff1509713aaec7cdb0` (0.65.1).
Upstream nono v0.78.0 documentation is comparison material, not evidence of binding
feature parity. The binding requires Go 1.24+ and a C toolchain; choose a project
toolchain compatible with all selected dependencies when implementation begins.
Pin the toolchain and native artifacts.

| Topic | Primary source |
| --- | --- |
| Upstream CLI network controls, not binding guarantees | [Networking, v0.78.0](https://github.com/nolabs-ai/nono/blob/v0.78.0/docs/cli/features/networking.mdx) |
| nono Linux/macOS enforcement | [Landlock](https://github.com/nolabs-ai/nono/blob/v0.78.0/docs/cli/internals/landlock.mdx), [Seatbelt](https://github.com/nolabs-ai/nono/blob/v0.78.0/docs/cli/internals/seatbelt.mdx), [security model](https://github.com/nolabs-ai/nono/blob/v0.78.0/docs/cli/internals/security-model.mdx) |
| nono-go API, build requirements, and native version | [Pinned README](https://github.com/nolabs-ai/nono-go/blob/9ba65a11c842eed3644dcd2fb008a4a3f119f680/README.md), [Apply and support API](https://github.com/nolabs-ai/nono-go/blob/9ba65a11c842eed3644dcd2fb008a4a3f119f680/nono.go), [bundled core version](https://github.com/nolabs-ai/nono-go/blob/9ba65a11c842eed3644dcd2fb008a4a3f119f680/internal/clib/linux_amd64/VERSION), [native core manifest](https://github.com/nolabs-ai/nono/blob/1d1c88c9f98f0a1f3ff79cff1509713aaec7cdb0/crates/nono/Cargo.toml) |
