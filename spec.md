# Microoperator: technical specification

Design contract; no implementation yet. See [plan.md](plan.md) for rationale and
dependency research. "Must" denotes an implementation requirement.

## 1. Runtime and isolation

One Go daemon hosts multiple independent agent systems on one machine. Each system
has its own operator agent, team, goals, memory, and budgets. The daemon owns
execution; operators propose and coordinate work without administrative authority.

```text
CLI / optional Go UI -> daemon API
                         |
              supervisor + durable scheduler
              permission checks + model/tool brokers
              SQLite + artifact storage
                         |
              nono-go sandboxed worker processes
```

Every system-owned record and artifact must carry `system_id`. The daemon derives
caller identity and scope from authenticated sessions, not request-supplied IDs.
Cross-system access and messaging are denied unless explicitly granted.

Project code is Go. Sandboxing uses `github.com/nolabs-ai/nono-go`, requiring cgo,
a C toolchain, and pinned native nono libraries. No containers or nono CLI are
required. Systems share a daemon failure boundary; this is not hostile
multi-tenant isolation.

## 2. Configuration and entities

### 2.1. Ownership and startup

The daemon accepts `--config <path>` pointing to a user-owned JSON file, conventionally
`microoperator.json`. Require `schema_version`; reject unsupported versions and
unknown fields instead of guessing their meaning.

| Data | Source of truth | How it changes |
| --- | --- | --- |
| Providers, model aliases, quota groups, tool definitions, sandbox profiles, launch configurations | Administrative JSON | User edits file and restarts daemon; no hot reload in v1 |
| Created systems, agent revisions, grants, tasks, usage, memory, approvals | SQLite | Authenticated runtime commands, including UI actions |
| Credentials | Named environment variables in the daemon process | User supplies them outside JSON; never forward to workers or return through the API |

JSON `systems` entries are **named launch configurations**, not running instances.
The UI lists them and creates a system by copying the selected configuration into
SQLite with a new runtime-assigned `system_id`. Creation does not imply starting;
the UI may offer a combined create-and-start action. File declarations never
autostart work or overwrite existing systems on restart.

Existing systems retain their persisted configuration and budgets. After a daemon
restart, only previously running systems are eligible for recovery/resumption;
paused/stopped systems remain so. Revalidate current administrative permissions
before resuming. Removed providers/profiles or incompatible configuration block
affected work with a visible reason, never an implicit substitute.

### 2.2. Example configuration

This is the intended v1 shape, not an implemented loader. Replace the intentionally
invalid endpoint/model below; example quotas are not provider guarantees.

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
      "quota_groups": ["primary-account"],
      "max_output_tokens": 1024
    }
  },
  "quota_groups": {
    "primary-account": {
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
  "systems": {
    "research": {
      "operator": {
        "prompt": "Coordinate tasks and delegate within the granted capabilities.",
        "model": "default",
        "tools": ["runtime.agent.propose", "runtime.task.delegate"],
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

Model aliases reference registered provider adapters and all applicable quota
groups. The first planned provider adapter is OpenAI-compatible Chat Completions;
a gateway may supply that API. Changing `base_url` does not change the protocol.
Additional adapters must be explicitly implemented, not inferred from a URL.

`runtime.agent.propose` and `runtime.task.delegate` are built-in broker operations,
not executables or installed dependencies. Additional tools require an administrative
registry entry with execution details, schemas, effect class, timeout, capabilities,
and credential references. Listing a tool name does not install or approve it.

`max_agents` counts live agents, including the operator; `max_active_agents` caps
simultaneous activations in that system. `token_budget` is its lifetime aggregate
allowance across goals and descendants. Goal/agent caps may narrow it; creating
agents or stopping/starting a system cannot replenish it.

Resolve `data_dir` relative to the configuration file, not the caller's working
directory. Resolve sandbox paths relative to each activation's private workspace.
Do not perform shell expansion or interpolation in paths/prompts. Validate registry
references, required credentials, bounds, and profile support before accepting work.
Malformed configuration prevents readiness; errors name the field without secrets.

### 2.3. Runtime grants versus nono permissions

| Layer | Meaning | Enforcement |
| --- | --- | --- |
| Runtime grant | Allowed models/tools, delegation, memory scopes, messaging, schedules, and budgets | Daemon checks authenticated requests before admission and execution |
| Sandbox profile | OS resources reachable by a worker or tool process | Launcher converts validated paths/network mode into nono-go capabilities |

The daemon materializes grants from validated system/agent settings. An allowed
tool remains subject to its own argument/resource restrictions and the task grant.
Agent requests cannot select an unapproved profile or acquire missing capabilities.
Do not treat a broad sandbox profile as permission to invoke more broker operations.

For v1, profile paths are confined to `inputs`, `scratch`, and `output`, or their
descendants, within the activation workspace. `inputs` is read-only; writable
grants may cover only scratch/output. Canonicalize and enforce containment without
symlink/path-traversal races. Do not expose daemon state, credentials, configuration,
other workers' workspaces, or writable ancestors of protected files.

Map `read` to `nono.AccessRead`, `read_write` to `nono.AccessReadWrite`, and
`network: "blocked"` to `nono.NetworkBlocked`. The launcher adds only the trusted
executable and platform runtime access needed to start the target, and exposes
that effective profile for inspection. Close unrelated inherited descriptors.
`blocked` is the only worker network mode in v1; no blanket filesystem/network
allowance or automatic permission expansion on failure.

A network-blocked worker may still request an approved HTTP tool or model call:
the broker performs that operation against configured destinations. The worker
gets neither direct connectivity nor credentials. Tool execution uses its own
validated, scoped profile; generated tools do not inherit daemon authority.

OS grants are not live toggles. Profile changes require a new confined process.
Revoking runtime grants blocks queued/future broker actions immediately; cancel
affected work when continued execution would violate the new grant.

### 2.4. UI configuration and unresolved alternatives

The UI may create systems from launch configurations, edit prompts and permitted
model/tool/profile selections, and set limits within the authenticated user's
authority. Persist changes as revisions in SQLite. Selecting a model or profile
never edits its administrative definition. Show effective permissions, remaining
budgets, and disabled/unsupported options before starting.

Two choices could not be confirmed with the user. These are conservative working
defaults for v1, **not user-confirmed requirements**:

| Choice | Working default | Alternative requiring a decision |
| --- | --- | --- |
| Administrative settings | JSON owns providers, tools, and profiles; UI manages systems only | JSON bootstrap followed by authenticated UI editing of administrative settings in SQLite |
| Host filesystem grants | Private worker directories only; import selected files as scoped inputs | Explicit approved read-only host directories, or explicit read/write host directories |

Do not implement both alternatives as configuration modes preemptively. Before
expanding either boundary, ask the user to choose and update the ownership,
permission, and acceptance rules. File import does not grant access to its source
directory and cannot bypass protected-path or artifact authorization checks.

### 2.5. Durable entities

| Entity | Required information |
| --- | --- |
| System | ID, operator, lifecycle state, grants, aggregate limits |
| Goal | System, user prompt, owner, outcome, aggregate budget |
| Agent revision | Identity/lineage, prompt, model, pinned skills/tools, memory scopes, wakeups, grants, limits |
| Task | System/goal/agent, pinned revision, status, continuation, pending/completed call IDs |
| Event | ID/type/version, system, authenticated source, destination, goal/task, correlation/causation, timestamp/expiry, bounded payload or artifact reference |
| Tool revision | Executable or approved integration, schemas, effect class, capabilities, timeout, artifact digest |

Revisions are immutable. New tasks select the active revision; running/waiting tasks
remain pinned unless explicitly migrated. Revocations still apply to pinned work.
Skills are read-only instructions, never permission grants. Agent creation is a
validated runtime request; child capabilities cannot exceed the creator's
delegable grant. Descendants share goal and system budgets.

## 3. Worker lifecycle and brokers

Use one executable with `daemon`, `sandbox-exec`, and `worker` modes. The daemon
launches a fresh sandbox-exec child with a trusted profile and pinned executable.
The child applies nono-go on a locked OS thread and immediately execs the target
on that thread. Never call irreversible `nono.Apply` in the daemon.

Workers receive only scoped read-only inputs, private scratch/output, sanitized
environment, and bounded framed JSON over stdin/stdout. No provider credentials,
database access, or unrestricted network. Release task content only after
confinement; any setup/apply/exec failure aborts launch.

The binding/native-core combination must demonstrably enforce the requested
profile, including exec/thread/descendant inheritance and non-TCP network denial.
CLI protections cannot be assumed present in the binding. Unsupported profiles
must remain disabled. Resource caps and process-tree cleanup require separately
verified host enforcement.

Allow one active activation per agent; different agents and systems run concurrently.
Workers request model calls, tools, delegation, memory, messages, subscriptions,
timers, and agent proposals through the daemon's brokers. Each request has a
correlation ID, operation, and validated arguments; failures return explicit errors.
No broker operation exposes arbitrary host shell execution or unrestricted URLs.

Task states are `queued`, `running`, `waiting`, and terminal `completed`, `failed`,
`canceled`, or `rejected`. Waiting records a reason, wakeup condition, and deadline;
an authorized wakeup returns the task to the queue for admission.
Delegation is asynchronous: persist continuation and release the worker slot.
Worker exit alone is not task completion; terminal tasks cannot be silently reopened.

## 4. Messaging and wakeups

Agents address peers directly through the daemon's authorized router, not through
the operator and not through unmanaged peer sockets. Check permissions on
acceptance and again before delayed delivery/execution.

> **Reminder for later:** Review the router/broker/eventing trust boundary before
> enabling privileged delegation. Prevent confused-deputy bypasses: bind sender
> identity to the session, constrain delegated work to its task grant, and authorize
> returned data. Wakeups must never execute agent code inside the daemon. Add denial
> checks demonstrating these boundaries; nono alone does not enforce them.

Commit events and intended mailbox deliveries atomically in SQLite. Local delivery
is at least once: deduplicate by event/recipient and correlate resulting actions.
Bound payloads, queues, retries, and fan-out; expose expired/dead-letter deliveries.
External effects require idempotency or reconciliation. Never blindly retry an
action whose outcome is unknown.

The same path handles addressed messages, task results, subscribed events, and
agent-created one-shot/cron timers. Persist schedules and subscriptions; validate
their owner, target, expiry, grants, and trigger budget at creation and firing.
Use five-field cron with explicit time zone, default UTC; specify/test DST behavior.
After downtime, coalesce missed occurrences into one catch-up event and deduplicate
by schedule/occurrence. Standing work beyond a goal's lifetime needs explicit grants.

Enforce system/goal event budgets, causation depth, and activation limits to prevent
feedback loops. New agents and event IDs must not reset allowances.

Keep delivery replaceable. A later A2A adapter maps Agent Cards, messages, tasks,
status, and artifacts using the official Go SDK. Internal timers and memory events
are not A2A operations. Adapters preserve authorization and durable state; remote
streaming/notifications do not inherit local delivery guarantees.

## 5. LLM admission and permissions

All model calls, including evaluation and learning, pass through the shared broker.
Quota groups reflect actual provider/account/model sharing, not individual agents.
Enforce request/token rates, burst, concurrency, queue bounds, and wait deadlines.
Schedule fairly across systems, then goals; additional agents do not buy priority.

Reserve goal/system budgets atomically before dispatch. Estimate input tokens,
cap output, reconcile reported usage, and retain conservative reservations for
unknown outcomes. Spend budgets are separate from rate limits; token/cost
estimates must not be represented as exact provider accounting.

Streaming occupies a concurrency slot until closed. Honor supported throttling
signals and `Retry-After`; otherwise use bounded backoff with jitter. Every retry
re-enters admission within the original deadline/budget. The broker owns retries;
avoid stacked SDK/gateway retries. Persist usage, reservations, and cooldowns so
restart does not reset allowances.

An existing external gateway can be a configured provider endpoint when multiple
applications share quotas. Keep local fairness and budgets; never bypass a failed
gateway automatically.

Use explicit Go permission and approval checks, not a general policy engine.
Approvals bind the exact action, artifact/agent/grant revisions, approver, expiry,
and permitted use count. Recheck grants before resumption. Agents cannot approve
their own escalation, edit audit state, or turn memory into authority.

## 6. Memory and self-learning

The daemon owns SQLite and scoped immutable artifacts. Workers use memory APIs;
authorization must precede search results, snippets, counts, and artifact access.
Start with structured queries and SQLite text search, not a vector service.

| Scope | Contents | Default access |
| --- | --- | --- |
| Task | Conversation, tool receipts, checkpoints | Task participants with grants |
| Agent | Reusable notes and observations | Owning agent |
| System | Reviewed facts, procedures, skills | Granted agents in that system |

Entries carry provenance, author, evidence, version, classification, and retention.
Retrieve relevant entries within context limits; treat retrieved content as
untrusted data. Cross-system sharing requires explicit collection access or
authorized export. Retention/deletion must cover artifacts, indexes, and backups.

Learning is `outcome -> proposal -> evaluation -> promotion -> reuse`, not model
weight updates. Task events or bounded schedules may wake an improvement agent.
User feedback and protected checks supply evidence; self-reported success does not.
All learning consumes ordinary system/goal budgets.

Scoped notes may be admitted within existing grants. Prompt/skill/tool changes
produce candidate revisions evaluated against the current baseline. Agents cannot
rewrite protected acceptance checks. Executable promotion defaults to human approval;
only the approved revision is selected for future work, with rollback available.

Generated extensions are Go subprocess tools, never daemon plugins. Quarantine
source; build/test through the sandbox launcher with a pinned toolchain, isolated
cache, no secrets/network, and `CGO_ENABLED=0`. Initially allow standard library
only: no automatic dependency/toolchain downloads or `go generate`. Promotion
binds source, build settings, test evidence, binary digest, and capabilities.
Failures remain quarantined and cannot trigger permission expansion.

## 7. Control API, UI, and recovery

Expose an authenticated versioned API for systems, goals, agents, tasks, grants,
limits/usage, messages, memory, schedules, artifacts, revisions, and approvals.
Provide scoped snapshot queries and a resumable event stream with event IDs.
Commands include create/start/stop system, submit goal, send input, approve/reject,
revise, revoke, pause/resume, and cancel. Retried UI commands use idempotency keys
so a double click or reconnect cannot create duplicate systems, runs, or inputs.

The UI is an optional, separate Go client/server with server-rendered pages, but
when connected it must support active control, not just visualization:

| UI action | Required behavior |
| --- | --- |
| Create and start | Choose an operator configuration, enter an initial goal/context, set allowed capabilities and budgets, and start a new system through the daemon API |
| Inspect | List systems and show their state, team graph, tasks, messages, artifacts, schedules, queues, budgets, and failures |
| Add information | Send follow-up instructions, corrections, answers, or scoped attachments to the system operator; optionally address an authorized task/agent |
| Pause / resume | Stop admitting new activations without losing state; resume eligible work after current grant/budget checks |
| Stop | Cancel that system's pending work/wakeups, revoke execution grants, and terminate its workers without stopping other systems |
| Steer | Approve/reject requests and learning proposals, revise future agent configurations, and adjust authorized grants/budgets |

The UI is not an administrative profile/provider editor in v1; configuration
ownership and available choices follow section 2. Start/steer commands cannot use
prompt text or uploaded files to grant additional permissions.

Additional information is a durable, attributed `user.input` event, not a silent
conversation rewrite or a permission grant. Consume it at the next safe agent
turn; it cannot retroactively alter an in-flight external action. Paused systems
queue input for resume. Stopped systems reject input until explicitly started.
Show input receipt/delivery status and rejection reasons.

Pause allows already-running work to finish; stop requests cancellation, then
forces termination after a bounded grace period. Show `stopping` until cleanup
finishes, not `stopped` merely because the command was accepted. Stopping preserves
memory, artifacts, and history; deletion is separate. Starting a stopped system
creates a new goal/run under current grants, without replaying canceled work,
reactivating old timers, or resetting consumed system budgets.

The UI owns no runtime state. Use restricted local sockets or authenticated
loopback access, CSRF protection, and escaped output. Bound and authorize attachment
ingestion. Display authorization, budget, queue, and provider errors explicitly.
Agent/goal controls remain scoped; daemon-wide controls must be clearly distinguished.
On restart, reconcile leases, orphan processes, pending calls, and ambiguous effects
before resuming. UI failure must not interrupt work or implicitly approve an action.

Persist authorization and state-changing audit records before dispatch; redact
secrets. Storage/accounting failures block new privileged work, while emergency
stop remains available. Correlate system/goal/task/event/activation/call records.
Local audit is not tamper-proof against the host administrator.

## 8. Initial acceptance criteria

- Configuration rejects invalid fields/references, missing credentials, escaping
  paths, and unsupported profiles without partial activation. Inspection shows
  effective grants but never secrets.
- Creating two instances from one launch configuration yields separate IDs/state;
  daemon restart neither duplicates instances nor overwrites UI revisions/budgets.
  Profile changes take effect only in newly confined processes.
- Two systems run concurrently without unauthorized memory/message/artifact access.
- From the UI, create/start a system, send additional context, pause/resume it, and
  stop it without affecting another system. Input survives pause/restart; retried
  commands do not duplicate work, and starting again does not replay canceled tasks.
- nono-go confines workers/tools across exec, threads, and descendants; unsupported
  controls fail closed and cancellation leaves no ungoverned worker processes.
- Shared LLM limits hold under concurrent calls, streaming, retries, and restart;
  queued work cannot bypass revoked grants or exhausted budgets.
- Restart preserves tasks, memory, and timers; duplicate events do not silently
  repeat non-idempotent effects.
- A generated tool is quarantined, evaluated, explicitly promoted, and pinned;
  changing its content invalidates approval, and rollback restores the old revision.
- The daemon remains operational without the UI. No containers, external queue,
  generic policy engine, or online model training are required.
