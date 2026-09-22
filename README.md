# Microoperator

A Linux/amd64 Go daemon with validated configuration, SQLite-backed systems, an
authenticated local control API, and a shared model broker. Reviewed, nono-go-confined workers
run bounded tool-using turns and delegate to agents through durable mailboxes.
Scoped tools/skills, durable teams, memory, schedules, a detached browser UI, and
protected learning evaluations are implemented.

**Generated Go requires explicit approval and the resource-confined Linux/amd64
profile.** The default reviewed-code profile cannot build or run generated tools.
Other operating systems are not supported.

## Run

Requires Linux/amd64, Go 1.24+, cgo, and a C compiler. The host also requires
working Landlock and seccomp, `/lib`, `/lib64`, `/usr/lib`, and `/etc/ld.so.cache`.
Sandbox profiles have been exercised on Linux/amd64 WSL2; other Linux
architectures are rejected until separately verified. The pinned `nono-go`
binding and its Linux native library remain required for sandbox enforcement.

### Quick start with local defaults

[microoperator.example.json](microoperator.example.json) is a complete configuration
with no placeholders or provider credentials. It uses Ollama at
`http://127.0.0.1:11434/v1`, `qwen3.8:27b` (Q4_K_M, about 18 GB), one active model
call, and a 100,000-token system budget. Each new system starts with a general-purpose
operator, scoped memory/artifacts, and bounded delegation/proposal capabilities.
Generated-code execution still requires additional setup and human approval.

Install Ollama 0.32.12 or newer first. If its service is not running, leave `ollama serve` running
in another terminal. Then, from the repository root:

```sh
ollama pull qwen3.8:27b
cp -n microoperator.example.json microoperator.json
chmod 600 microoperator.json
export MICROOPERATOR_CONTROL_TOKEN="$(openssl rand -hex 32)"
printf 'Control token for the UI shell: %s\n' "$MICROOPERATOR_CONTROL_TOKEN"
CGO_ENABLED=1 go run . daemon --config ./microoperator.json
```

`cp -n` preserves an existing `microoperator.json`; if one already exists, merge
the sample settings into it deliberately. The private copy is necessary because
the daemon rejects group/world-readable configuration files. Start the
[detached UI](#detached-browser-ui) with the same control token, enter a system
name and goal, then explicitly start it. Nothing starts a goal automatically.
Use [Ollama and Azure](#ollama-and-azure) to change models or add your Azure resource;
Azure is not a default because its endpoint and deployment are account-specific.

If you run `./microoperator` instead of `go run .`, rebuild it after source updates.
An older executable can reject newer configuration fields such as `bootstrap`:

```sh
CGO_ENABLED=1 go build -o ./microoperator .
./microoperator daemon --config ./microoperator.json
```

### Daemon

The daemon supports explicit goals and OpenAI-compatible Chat Completions.
Loading configuration and creating systems still do not start workers or make
provider calls. The sample above is the local default. For an API-key-based
provider instead, create a user-owned `microoperator.json`:

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
      "max_output_tokens": 1024,
      "input_headroom_percent": 20
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
  "bootstrap": {
    "operator": {
      "model": "default",
      "sandbox_profile": "worker"
    },
    "limits": {
      "max_agents": 8,
      "max_active_agents": 2,
      "token_budget": 100000
    }
  }
}
```

```sh
chmod 600 microoperator.json
export MICROOPERATOR_CONTROL_TOKEN="$(openssl rand -hex 32)"
export MICROOPERATOR_LLM_KEY='replace-with-your-provider-key'
CGO_ENABLED=1 go run . daemon --config ./microoperator.json
```

Replace the example endpoint, model ID, and key with approved provider settings
before starting a goal. Starting a goal authorizes a real, potentially billable
provider call. Credentials are environment variables, never JSON values or URL parameters.
The control token must contain 32-256 printable, non-space characters.

Readiness is one JSON line with `type: "ready"` and `data` containing the absolute
control-socket path. Invalid configuration or missing declared API-key credentials
prevents readiness. Azure credentials are acquired on admitted calls, not startup.
SIGINT/SIGTERM cancels active operators, shuts down the server, and closes
the database. Interrupted calls keep conservative accounting.

`data_dir` is relative to the configuration file, not the invoking shell. It must
be a private, user-owned directory (0700); the database, lock, and socket use 0600.
The daemon refuses symlinked configuration/state files, unsafe file permissions,
an unrelated/newer database, or a second daemon using the same directory. After
a crash, it recovers committed SQLite state and removes only the stale socket.
The default local configuration and state directory are ignored by Git.

### Ollama and Azure

Three explicit adapters share the same broker, quotas, budgets, response validation,
and tool-call handling:

| Adapter | Endpoint | Authentication |
| --- | --- | --- |
| `openai-chat-completions` | OpenAI-compatible base URL | Required `api_key_env`, sent as a Bearer token |
| `ollama` | Loopback URL ending in `/v1`, normally `http://127.0.0.1:11434/v1` | None; omit `api_key_env` |
| `azure-openai` | HTTPS URL ending in `/openai/v1` | Azure SDK `DefaultAzureCredential`; omit `api_key_env` |

For both local Ollama and Azure models, replace the example's `providers`,
`models`, and `quota_groups` sections with the following. Keep the other sections.
The bootstrap operator uses `default`, which now selects Ollama:

```json
{
  "providers": {
    "local": {
      "adapter": "ollama",
      "base_url": "http://127.0.0.1:11434/v1",
      "timeout_seconds": 600
    },
    "azure": {
      "adapter": "azure-openai",
      "base_url": "https://YOUR-RESOURCE.openai.azure.com/openai/v1"
    }
  },
  "models": {
    "default": {
      "provider": "local",
      "model": "qwen3:8b",
      "quota_groups": ["local"],
      "max_output_tokens": 1024
    },
    "azure": {
      "provider": "azure",
      "model": "YOUR-DEPLOYMENT-NAME",
      "quota_groups": ["azure"],
      "max_output_tokens": 1024
    }
  },
  "quota_groups": {
    "local": {
      "requests_per_minute": 60,
      "tokens_per_minute": 60000,
      "burst_requests": 1,
      "max_concurrent": 1,
      "queue_capacity": 100,
      "max_wait_seconds": 30
    },
    "azure": {
      "requests_per_minute": 60,
      "tokens_per_minute": 60000,
      "burst_requests": 1,
      "max_concurrent": 2,
      "queue_capacity": 100,
      "max_wait_seconds": 30
    }
  }
}
```

These quota values are examples, not Azure quota guarantees. Keep aliases sharing
an Azure deployment/account in the same applicable quota groups. Local inference
still consumes configured token budgets.

Each provider accepts `timeout_seconds`: 1-3600 seconds, with omitted/zero meaning
60 seconds. This bounds credential acquisition and the entire request, including
response headers, body, and streaming. The Ollama sample allows 600 seconds for
large local models, including loading and inference; this is not a speed guarantee.
The original goal deadline can end a request sooner. Changing this administrative
setting requires restarting the daemon and creating or explicitly revising a system
to bind the new definitions. It does not replay or refund earlier timed-out calls.

Install Ollama and start its server with `ollama serve` in a separate terminal,
unless it is already running as a service. Pull a local model with
`ollama pull qwen3:8b`, or substitute another installed model name in configuration.
No dummy API key is needed. Choose a local model, not an Ollama cloud-model tag,
if inference must stay on this machine.

For Azure, deploy a Chat Completions-compatible model and use its **deployment
name**, not its catalog model ID. Both `RESOURCE.openai.azure.com/openai/v1` and
`RESOURCE.services.ai.azure.com/openai/v1` HTTPS endpoints work. The old
deployment URLs with `api-version` are not this adapter's protocol.
Your identity needs the **Cognitive Services OpenAI User** role on the resource.
In the daemon's Linux environment:

```sh
az login
# Optional when more than one subscription is available:
az account set --subscription YOUR-SUBSCRIPTION-ID
# Optional: use exactly the identity logged in through az login.
export AZURE_TOKEN_CREDENTIALS=AzureCLICredential
```

Without that selector, the SDK's normal `DefaultAzureCredential` chain applies,
including environment/workload/managed identity credentials before developer
credentials. This is the local-development login path; constrain credentials
explicitly for unattended deployments. Azure CLI must be installed and available
on the daemon's `PATH` when using CLI authentication.

Set `systems.research.operator.model` to `azure` to select Azure instead of Ollama.
Rebuild the executable and restart the daemon after editing configuration. Create
a new system, or explicitly revise an inactive existing system to bind the changed
definitions. The daemon/UI commands and their control/UI tokens are unchanged;
`MICROOPERATOR_LLM_KEY` is unnecessary when only these two providers are configured.

Azure requests use `max_completion_tokens`; the other adapters use `max_tokens`.
The selected model must support requested tools/streaming and return valid usage.
Azure access tokens are requested per admitted attempt for
`https://ai.azure.com/.default`, within the provider deadline, and are never written
to configuration, SQLite, artifacts, or worker IPC. Authentication failures before
HTTP dispatch fail visibly and release goal/system reservations without a model
call; rate admission remains conservative. Errors from the SDK/CLI are sanitized.
There is no fallback to a different provider or unauthenticated Azure request.

Protocol references: [Ollama OpenAI compatibility](https://docs.ollama.com/api/openai-compatibility)
and [Azure OpenAI v1](https://learn.microsoft.com/azure/foundry/openai/api-version-lifecycle).

### Control API

The API speaks HTTP **only over `state/control.sock`**; there is no TCP listener.
Every request requires `Authorization: Bearer ...`. The token authenticates one
local administrator; workers instead use daemon-established private pipes, not this
token. Multi-user roles are not implemented.
Caller-supplied identity/scope fields and direct browser `Origin` requests are
rejected. The separate UI authenticates browser requests and calls this socket
server-side; the browser never receives the daemon credential.

From a client shell with the same control token exported:

```sh
curl --unix-socket ./state/control.sock \
  -H "Authorization: Bearer $MICROOPERATOR_CONTROL_TOKEN" \
  http://localhost/v1/system-defaults

curl --unix-socket ./state/control.sock \
  -H "Authorization: Bearer $MICROOPERATOR_CONTROL_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: research-1' \
  -d '{"name":"Research","goal":"Record this goal without executing it."}' \
  http://localhost/v1/systems

curl --unix-socket ./state/control.sock \
  -H "Authorization: Bearer $MICROOPERATOR_CONTROL_TOKEN" \
  http://localhost/v1/systems
```

Reusing `research-1` with the same command returns its saved result. A different
key, such as `research-2`, creates a separate named system. Reusing a key with a
different command returns 409. Names need not be unique; system IDs are identities.

Creation requires `name` and `goal`; optional `constraints` guide the operator,
and `token_budget` may reduce, never exceed, the configured default (0 uses it).
Names are 1-128 UTF-8 bytes with no control characters or surrounding whitespace.
Constraints are at most 4096 bytes and persist in every agent's model context.
They are guidance, not enforceable grants: network, tools, budgets and approval
rules are enforced separately by the daemon.

`bootstrap` is optional administrative startup defaults, not a domain template.
Without it, the model/profile aliases are `default`/`worker`, agent limits are
8 total/2 active, and the system budget is 100,000 tokens. The generic operator
discovers capabilities, plans, delegates narrowly, proposes missing tools and
requires evidence before claiming success. Administrators can override bootstrap
operator settings and limits or supply explicit tool lists; omitted lists use the
generic capabilities, while `[]` grants no tools. Supplied limits must be complete.
No domain prompt, simulator or trading strategy is required to create a system.
It must request human input when a needed capability cannot yet be used.

**Configuration/API change:** the old `systems` map, `launch` creation field and
`/v1/launch-configurations` endpoint have been removed. Replace the map in your
private config with the sample's optional `bootstrap` settings, then restart.
The application does not erase existing on-disk runtime data.

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
      "name": "Research",
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
| `GET /v1/health` | Readiness, execution availability, generated-build availability, and eligible `generated_profiles`; approval/assignment remain mandatory |
| `GET /v1/system-defaults` | Effective general-purpose bootstrap configuration and limits |
| `POST /v1/systems` | Required name + goal, optional constraints/reduced budget; new IDs and a pending goal, returns 201 |
| `GET /v1/systems` | Up to 20 owned systems; follow `?after=<next>` when `next` is present |
| `GET /v1/systems/<system_id>` | Saved configuration, pinned grants, usage/remaining tokens, state, and any blocking reason |
| `GET /v1/systems/<system_id>/activity` | Read-only snapshot of the current/latest goal: up to 32 agents and the 20 most recent calls and event deliveries; scoped and newest-first |
| `PUT /v1/systems/<system_id>/configuration` | New immutable revision; stale `expected_revision` returns 409 |
| `POST /v1/systems/<system_id>/start` | Start one goal under its pinned revision; returns 202 |
| `POST /v1/systems/<system_id>/stop` | Cancel only that system; returns 202, with `stopping` until cleanup finishes |
| `GET /v1/systems/<system_id>/model-calls` | Up to 20 durable calls/results, including older unknown outcomes; numeric `after` cursor |
| `GET /v1/model-broker` | Shared quota utilization, queue/in-flight counts, attempts, throttles, and accounting availability |

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
currently permitted definitions. New systems use the administrative bootstrap defaults.

Configuration is limited to 1 MiB and command bodies to 64 KiB. Unknown, duplicate,
or incorrectly cased JSON keys are rejected. References and grant subsets are
checked; profile paths must stay under `inputs`, `scratch`, or `output`, with writes
only to the latter two. Configuration identifiers use lowercase letters, digits, dots, underscores,
and hyphens, beginning with a letter, up to 64 characters.

Quota values must be positive: requests/minute up to 1,000,000, tokens/minute up to
1,000,000,000, concurrency up to 1,024, queue capacity up to 100,000, and wait up to
3,600 seconds. Burst cannot exceed requests/minute, and a model's output cap must
fit every referenced token quota. Agent counts satisfy
`1 <= max_active_agents <= max_agents <= 1024`; token budgets are positive and cannot
be revised below consumed plus reserved usage. Stop an active system before revising
its grants or configuration. Prompt/goal/skill content is capped at 32 KiB.

Supported provider adapters are listed [above](#ollama-and-azure). Administrative
`shared.*` definitions are skills; reviewed `runtime.*` executables are built into the daemon's
registry. Pinned skills become system-message instructions, never permission grants.
Arbitrary executable paths cannot be installed through configuration. Generated
source must pass the separate protected evaluation, approval, and assignment path.
Profile validation and native support diagnostics are not permission to run
untrusted code. Model/tool calls use authenticated worker pipes, not a public
arbitrary-prompt/provider or arbitrary-tool-invocation endpoint.

### Run one operator goal

Set `SYSTEM_ID` to an instance's returned `system_id`. Starting its still-pending
initial goal requires the revision you inspected:

```sh
curl --unix-socket ./state/control.sock \
  -H "Authorization: Bearer $MICROOPERATOR_CONTROL_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: start-research-1' \
  -d '{"expected_revision":1,"stream":true}' \
  "http://localhost/v1/systems/$SYSTEM_ID/start"

curl --unix-socket ./state/control.sock \
  -H "Authorization: Bearer $MICROOPERATOR_CONTROL_TOKEN" \
  "http://localhost/v1/systems/$SYSTEM_ID"

curl --unix-socket ./state/control.sock \
  -H "Authorization: Bearer $MICROOPERATOR_CONTROL_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: stop-research-1' \
  -d '{}' \
  "http://localhost/v1/systems/$SYSTEM_ID/stop"
```

Inspect `execution.response`, `execution.task_state`, `execution.state`,
`execution.reason`, and the token fields. Provider completion and worker/task
completion are separate: a worker exit alone never means success. `stream` defaults
to false; true consumes provider SSE with terminal usage, buffers the bounded result,
and retains its concurrency slot until the stream closes. This is not yet a UI stream.

After completion or cancellation, use a new idempotency key and a new `goal`:
`{"expected_revision":1,"goal":"Explain the next topic.","token_budget":5000}`.
The optional goal cap cannot expand the remaining system allowance. Restarting a
system after stop never replays its previous goal or replenishes lifetime budgets. Replaying
an old start/stop command cannot start or cancel a later activation.

There is one active goal per system and one active activation per agent, at most
64 across the daemon, subject to each system's `max_active_agents`. Task data crosses private pipes only after
sandbox readiness. Scope comes from that activation, not worker-supplied identities.

### Scoped tools and agent teams

The generic bootstrap already enables scoped delegation. Use a model that supports
function calls. To customize the allowed set, provide **both** `bootstrap.tools`
and `bootstrap.operator.tools` (or explicitly revise a stopped system), for example:

```json
[
  "runtime.agent.list",
  "runtime.agent.propose",
  "runtime.task.delegate",
  "runtime.task.progress",
  "runtime.tool.propose",
  "runtime.text.analyze"
]
```

Do not copy these entries into the top-level administrative `tools` map. Restart
after editing configuration, then create a new instance or explicitly revise a
stopped instance. Start with `stream:false` (the default): executable tool calls
are currently non-streaming, with exactly one function action per model turn.
Plain model text is never treated as code or as a function request.

| Tool | Behavior |
| --- | --- |
| `runtime.capabilities` | Current task's exact tool pins, model/profile, remaining ancestral budget/turns, goal deadline, protected check IDs (not cases), and generated-build blockers; grants nothing |
| `runtime.artifact.put` / `runtime.artifact.get` | Immutable inert text/JSON, up to 3072 UTF-8 bytes and a bounded encoded result; reads restricted to the current system and goal; writes stop at 128 goal artifacts |
| `runtime.agent.list` | Same-goal collaborators, availability, model/profile, and capability summaries; five per page with `after`/`next`. Shows up to 16 tool names plus the full `tool_count`; the administrator's agent view has full pins |
| `runtime.agent.propose` | Creates a child with a name, prompt, subset of the current task's tools, and token cap; inherits model/profile; does not launch it |
| `runtime.task.delegate` | Returns a task ID and persists a waiting continuation; the child runs through a mailbox, then its result wakes the parent |
| `runtime.task.progress` | Records a bounded progress event for the parent; does not create another model turn by itself |
| `runtime.tool.propose` | Stores a private, inert skill or Go-source draft with creator/goal/task provenance; never builds or activates it |
| `runtime.text.analyze` | Reviewed sandboxed subprocess accepting `text` (up to 4096 bytes) and optional `save_artifact`; returns rune/word counts and an authorized artifact ID |

For example, set the operator prompt to “Inspect the directory, propose a narrowly
granted research assistant if needed, delegate text analysis to it, and summarize
its result.” Model decisions remain model-dependent; the daemon validates each
proposed action instead of interpreting arbitrary plans.

Registry visibility is not a grant. Task pins include the version and definition
digest; invocation and delayed delivery recheck availability and revocation.
Delegation intersects caller task and recipient grants, disallows a different
model/profile, and charges creation/delegation ancestors as well as the common
goal and system. A waiting parent releases its execution slot, so a parent and child
can run with `max_active_agents:1`.

The text tool has fixed arguments and no shell, arbitrary file path, or network
option. Its manifest exposes schemas, limits, and the runtime executable's SHA-256.
Rebuilding that executable changes the tool pin: restart and explicitly revise
affected stopped systems rather than silently using a new binary. Subprocesses
use the task's selected profile; saving a report requires write access to `output`.
Validated reports are at most 4096 bytes and stored with scope/digest in SQLite.
This small-report path is not a general upload/artifact store.

| Route | Result |
| --- | --- |
| `GET /v1/tools[/<tool_id>]` | Shared registry; no local drafts |
| `GET /v1/systems/<system_id>/tools[/<tool_id>]` | Shared definitions/revocations and this system's private drafts |
| `POST /v1/systems/<system_id>/tools/drafts` | `{kind,description,content,requires_tools}`; returns `command_result.tool_id`, always inert |
| `POST /v1/systems/<system_id>/tools/<tool_id>/state` | `{version,state}`; only `rejected` or `disabled`, never activation/promotion |
| `POST /v1/systems/<system_id>/tools/revoke` | `{name,version}`; blocks subsequent calls/deliveries and cancels affected activations |
| `PUT /v1/systems/<system_id>/agents/<agent_id>/tools` | `{expected_revision,tools:[{name,version,digest}]}`; exact system-granted pins in a new child revision |
| `GET /v1/systems/<system_id>/agents` | Agent definitions, full pins, revisions, state, and availability |
| `GET /v1/systems/<system_id>/tasks` | Pinned tasks, continuation/wait state, outcomes, and denial reasons |
| `GET /v1/systems/<system_id>/events` | Trusted envelopes, payloads, delivery status, retries, leases, and dead-letter reasons |
| `GET /v1/systems/<system_id>/tool-calls` | Durable tool receipts/results, correlated with calls and tasks |
| `GET /v1/systems/<system_id>/artifacts[/<artifact_id>]` | Scoped report metadata, or an individual validated report |

Agent/task/event/tool-call/artifact lists return up to 20 entries; follow `next`
using `?after=<next>`. Events use numeric sequence cursors; other lists use IDs.
Use system configuration revisions for operator/system assignment. Child assignment
does not rewrite running/waiting task pins; revocation still takes immediate effect.
All mutations use the existing authenticated, transactional idempotency mechanism.
Draft IDs are runtime-assigned `local.*` identifiers; they cannot shadow `shared.*`
or `runtime.*`. Draft content is immutable; at most 64 drafts per system, with
8192-byte content and 2048-byte descriptions. Missing dependencies are rejected;
skills never auto-install or auto-grant them. To propose another immutable version,
include `tool_id` and `expected_version`; there are at most 32 versions per ID and
64 total draft revisions per system. The learning workflow below owns promotion.

### Follow-up input and scoped controls

`POST /v1/systems/<system_id>/input` accepts
`{"content":"Additional context","agent_id":"optional-addressed-agent-id"}`.
Omit `agent_id` to address the operator's current task. The 202 response contains
`command_result.event_id`; inspect events for delivery status. Input is limited to
4096 bytes and is durable context, not authority. It is consumed at the next safe
turn; a waiting parent's input follows its child's correlated result.

Post `{}` with a new idempotency key to:

- `/v1/systems/<system_id>/pause` or `/resume` (system `/stop` remains available).
- `/v1/systems/<system_id>/goals/<goal_id>/pause`, `/resume`, or `/stop`.
- `/v1/systems/<system_id>/agents/<agent_id>/pause`, `/resume`, or `/stop`.

Pause lets already claimed work finish and prevents subsequent activations.
Paused input survives restart; resume does not reactivate stopped work. Stop
cancels the addressed task subtree, preserves history, and never targets another
system. Stopping a root/goal waits in `stopping` until leased workers finish cleanup.
A completed goal becomes inactive even if its final turn finished during a pause.
Stopped systems reject input; start a new goal explicitly.

Fixed bounds are eight model turns per task and per agent/goal, creation depth
eight, 64 tasks per goal, 256 events per goal, 4096 lifetime events per system,
64 outstanding deliveries per recipient, 32 source events per agent/minute,
8192-byte event payloads, and causation depth 16. Terminal-reply capacity is reserved;
if a result cannot be delivered, its waiting continuation fails explicitly.
Source IDs, scope, classification, and correlation are daemon-stamped. Neither
new agents nor restarts reset allowances.

All turns share the original goal deadline. The default is the shortest quota
wait plus three configured request timeouts for the initial operator's provider
(60 seconds each when omitted). An authenticated start may set
`lifetime_seconds` to 1-86400 for scheduled work; agents cannot extend it.
**Pause does not extend this lifetime**; expired work/input receives a visible
terminal/dead-letter outcome. Longer lifetimes do not reset turn, event, or token
budgets, and schedules never start a new goal implicitly.

### Memory and wakeups

Grant these reviewed tools explicitly, like the team tools:
`runtime.memory.put`, `runtime.memory.search`, `runtime.schedule.create`,
`runtime.schedule.cancel`, `runtime.events.subscribe`, `runtime.events.unsubscribe`,
and `runtime.task.wait`.

Memory has `task`, `agent`, and `system` scopes. Worker ownership comes from its
authenticated task, not supplied IDs. Writes carry content, evidence, confidence
(0-100), artifact references, and retention (1 second to 365 days). Task/agent
notes are private to their owner; worker-proposed system notes require human
approval. Workers cannot overwrite an existing shared fact. Searches authorize
before matching or pagination and return `untrusted_data:true`, never instructions
or grants. Bounds are 512 memory IDs/system, 32 revisions/ID, 4096-byte content,
1024-byte evidence, eight artifact references, and ten results/page within 8 KiB.

Expiry/deletion removes stored memory revisions and their artifact references;
tombstones prevent resurrection. Original task artifacts, conversations, model
requests, and command receipts retain their independent history. There is no
managed backup or secure-erasure facility: deleting a note does not promise
physical deletion of historical copies or administrator-made backups.

Schedules accept an RFC3339 `at` or a five-field `cron` with an explicit IANA
`timezone` (UTC by default; `Local`, seconds fields, and `@every` are rejected).
The pinned `robfig/cron/v3` parser skips nonexistent spring-forward times and
fires both repeated fall-back times as distinct UTC occurrences. Missed runs
coalesce into one catch-up event. Occurrence, cursor, trigger allowance, and
mailbox delivery commit together. Each schedule/subscription allows 1-8 triggers;
one-shot schedules require one. There are 128 IDs of each kind per system.

Subscriptions support authorized `memory.changed` and same-goal `task.completed`
notifications, with references rather than leaked contents. Self-memory
notifications are suppressed. Paused work retains pending wakeups; expiry, stop,
revocation, event limits, and goal deadlines prevent unauthorized dispatch.
`runtime.task.wait` persists the continuation and releases its worker. Timers and
subscriptions are durable SQLite records, not per-agent goroutines or OS cron jobs.

| System-scoped route | Body / result |
| --- | --- |
| `GET/POST .../memory` | Search, or write `{scope,content,evidence,confidence,retention_seconds,artifacts?}`; updates also supply `memory_id,expected_revision` |
| `POST .../memory/<id>/approve` or `/delete` | `{expected_revision}`; explicit review or logical deletion |
| `GET/POST .../schedules` | Inspect or create `{agent_id?,at?,cron?,timezone?,content,trigger_budget}` |
| `POST .../schedules/<id>/cancel` | `{}`; preserve history |
| `GET/POST .../subscriptions` | Inspect or create `{agent_id?,type,scope?,trigger_budget}` |
| `POST .../subscriptions/<id>/cancel` | `{}`; preserve history |

Here `...` means `/v1/systems/<system_id>`. Administrative memory search supports
`scope`, `owner_id`, `query`, `after`, and `include_pending`; workers cannot select
another owner. Trigger listings are cursor-paginated.

### Detached browser UI

Build the binary, leave the daemon running, and start the separate UI process:

```sh
export MICROOPERATOR_UI_TOKEN="$(openssl rand -hex 32)"
./microoperator ui --socket "$PWD/state/control.sock" --listen 127.0.0.1:8080
```

The UI process also needs the daemon's `MICROOPERATOR_CONTROL_TOKEN`, but not its
provider credentials. The two tokens must differ. Open the printed URL and use
HTTP Basic username `operator` and the UI token as password. Numeric loopback
binding, browser authentication, Host/Origin checks, CSRF tokens, escaped HTML,
and a restrictive CSP are enforced; do not publish this HTTP listener remotely.
The `same-origin` referrer policy preserves native form POST origins without
sending referrers to other origins; `Origin: null` remains rejected.

The UI uses status badges, budget summaries and tables for systems, agents, tasks,
events, tools, memory, schedules, quotas and learning. Raw response JSON and
advanced command editors are hidden inside **Properties**; errors and blockers
remain visible without opening it. Primary start/input/pause/resume/stop actions
do not require editing JSON. A form retry preserves its command key.
Optional `?refresh=5` refreshes inspection views without resetting command forms.
Closing the UI does not stop work, approve proposals, or change daemon state.
The creation form takes **Name + Goal**, with optional constraints and a smaller
budget. It shows the default model, sandbox and team/token limits; templates are
not needed. Names and constraints are stored in immutable configuration revisions.
Creation remains inactive until an explicit start.

**Run a saved initial goal:** open the system from the Systems table, then click
**Start system**. Leave the replacement goal in **Start options** blank; the daemon
uses the already-saved goal, not a duplicate. After that goal has been used, the
form becomes **Start new goal**. A paused system instead offers **Resume system**.

**Watch the team:** click **Watch activity** from the dashboard or system overview.
The Activity view shows the operator and agent creation hierarchy, current task
assignments, waiting reasons, model admission/calls, requested/completed tools,
token usage and a recent-event timeline. It reflects durable recorded work, not
hidden model reasoning, and refreshes every five seconds. **Pause live updates**
(`?refresh=off`) keeps Properties open for inspection. The view has no editable
forms; changing focus or closing it does not control execution.
Each snapshot is a scoped, read-only transaction for the current/latest goal.
At most 32 agents and 20 recent calls/event deliveries are displayed; truncation
is explicit, and the ordinary paginated histories remain available.
Restart both daemon and UI after rebuilding to load the new activity endpoint.

Autonomy is bounded, not unattended authority expansion. The operator can store
plans/results, delegate, draft tools and request protected evaluation. Missing
toolchains, resource-confined profiles or human-owned checks are visible blockers;
exact-artifact approval and assignment remain separate human actions. Waiting
releases the worker but does not extend the eight-turn task cap or goal lifetime.
For a paper-trading goal, simulated fills/accounting must come from actual approved
execution, not invented model output. No live-market access is granted by this setup.

Text attachments are bounded to 3072 UTF-8 bytes, with a display name but no host
path. They become inert scoped JSON artifacts and attributed `user.input` events
in one transaction (`POST .../attachments`, `{agent_id?,name,content}`).
Uploads never install code or grant filesystem access. Protected evaluation
inputs cannot be amended through input, attachment, or scheduling commands.

### Governed learning and generated tools

The lifecycle is proposal, protected evaluation, human approval, then explicit
assignment. Prompt improvements are versioned skill fragments; they do not mutate
the daemon or silently replace an agent's prompt. Outcomes remain in task/call
history, and authenticated user feedback records successes as well as failures.

Create immutable protected cases through `POST .../learning/checks`:
`{"cases":[{"input":"hello","expected":"HELLO"}]}`. There are one or two exact-output
cases per suite, up to 64 suites/system. Inputs are limited to 1024 bytes and
expected outputs to 4096 bytes. Candidate-authored tests cannot alter these checks.

Submit a draft, then `POST .../learning/evaluate` with
`{tool_id,version,check_id,task_id,baseline?}`. The originating task must still be
live. An optional baseline is an exact originating task pin. Skill evaluations
compare the fixed current prompt/skills with the candidate in ordinary reviewed
workers through the shared model broker, quotas, ancestor/system/goal budgets,
and execution slots. They do not execute model-selected tools. Every candidate
case must pass; baseline failures and successes both remain visible.
There are at most four evaluations per goal, within the existing agent/task caps.

An agent granted `runtime.learning.evaluate` may request the same workflow using
`{tool_id,version,check_id,baseline_name?}`. Identity is derived from its session.
It waits without occupying a worker; a durable result notification resumes it.
This grants neither permission to edit protected checks nor authority to approve.
Human-requested evaluations stay visible for review without automatically waking
the originating agent.

Generated candidates are bounded Go source in `package main`, implementing:

```go
func Process(text string) (string, error)
```

The runtime supplies fixed JSON framing (`{"text":"..."}`), validates bounded
responses, and provides no broker session. `requires_tools` must be empty.
Standard-library-only compilation runs in a resource-confined subprocess with
`CGO_ENABLED=0`, an isolated cache, `GOTOOLCHAIN=local`, and downloads disabled.
There is no shell, `go generate`, installation hook, or daemon plugin. Protected
black-box tests run the candidate in fresh sandboxes; expected results are
compared outside candidate control. A first executable uses text identity as its
fixed baseline, or an explicitly selected approved generated tool.

To enable builds on the qualified Linux host, use the resource profile below
with at least a practical build allowance such as 1 GiB RAM, 512 MiB workspace,
256 processes/threads, and 200 CPU percent. Pin the complete canonical local Go
tree (including compiler, linker, and sources):

```sh
./microoperator toolchain-digest "$(realpath "$(go env GOROOT)")"
```

Add top-level administrative configuration, substituting that path and digest:

```json
"learning": {
  "toolchain_root": "/usr/local/go",
  "toolchain_digest": "<64-character SHA-256 tree digest>"
}
```

The toolchain must not overlap daemon state; startup and each build verify its
identity. Only builders receive this read-only host grant; generated programs do
not. Builds are bounded to 120 seconds, executions to ten seconds, and binaries
to 32 MiB. Approved binaries are private digest-addressed files under
`data_dir/generated/<system_id>/`, outside SQLite. Removing `learning` disables
new builds, not already approved execution on a qualified profile.

Inspect `GET .../learning` for evidence and its digest. Approve only the exact
result with `POST .../learning/<evaluation_id>/approve`,
`{digest,expires_seconds,task_uses}`. Approval lifetime is 1-86400 seconds and its
allowance is 1-64 distinct tasks. The first model dispatch reserves a task use
transactionally; continuations/retries do not reset it, and failed tasks do not
refund it. New-key replay, changed evidence/configuration, disabled drafts, and
failed candidates cannot renew or obtain approval. A same-key retry merely
returns the original receipt.

Approval returns an exact `{name,version,digest}` pin but assigns nothing.
Stop the system, include the local name in the desired system/operator tools,
and supply `local_tools:[<pin>]` alongside the normal configuration revision.
Child assignment then uses exact system-granted pins. Rollback selects an eligible
previous pin through another revision; historical tasks are not rewritten.
Expiry, disable, revocation, missing grants, changed binaries, and changed
confinement implementations block subsequent use.

`GET .../learning/checks` and `/feedback` are paginated. Record feedback with
`POST .../learning/feedback`, `{task_id,rating:"success"|"failure",content}`.
The UI exposes these commands and evidence. Local publication never happens
automatically: reviewed skill content may be explicitly imported as administrative
`shared.*` configuration. Shared generated executables are not supported.

### Model accounting and recovery

All aliases use their configured shared quota groups. Request rate is a persisted
token bucket (`requests_per_minute / 60` refill per second, capped by
`burst_requests`). Token rate reserves estimated input plus the output cap in a
rolling 60-second dispatch window; smaller completions do not refund rate capacity.
Concurrency and queue limits apply to every group. Admission rotates across systems;
each currently has one goal, so creating more agents cannot buy priority.

Requests are limited to 64 KiB after JSON encoding. Input is conservatively estimated
as serialized request bytes plus `input_headroom_percent` (default 20 when omitted
or zero, maximum 1000); this is not an exact model tokenizer. Responses are limited
to 1 MiB on the wire, 32 KiB of text, and the 64 KiB encoded worker frame.
The adapter sends `max_tokens`, requests usage for streams, forbids redirects and
environment proxies, and never bypasses a configured gateway.

Agent/ancestor/goal/system budgets reserve atomically before dispatch and reconcile validated
provider usage. `used_tokens` reports known usage; `reserved_tokens` includes
in-flight and unresolved calls. Available budget subtracts both. Pricing is not
configured, so currency accounting is explicitly unsupported, not reported as zero.
The configured limits cover this daemon, not unrelated applications using the account.

Only explicit HTTP 429 rejection is retried, at most three attempts. Supported
`Retry-After` seconds/dates establish a persisted shared cooldown; absent/invalid
headers use bounded jittered backoff. Each attempt consumes rate capacity and
re-enters admission. Queue waits respect each group's deadline; the overall
activation deadline is the shortest group wait plus three configured provider-call
allowances (60 seconds each by default). No timeout, disconnected stream, missing
usage, or ambiguous provider failure is automatically replayed. Disable hidden
gateway retries where possible; the daemon cannot observe or guarantee accounting
for those external attempts.

SQLite migrates version-1/2/3/4 state transactionally to version 5; JSON stays at
`schema_version:1`. On restart, eligible queued work resumes, including safe
explicit-429 retries, while paused work remains paused. Completed model/tool
receipts are reused, not redispatched; committed delegation and accepted input
survive a worker's missing acknowledgement. Pre-dispatch worker failures receive
at most three delivery attempts with 2/4-second backoff. Leases and event/recipient
deduplication prevent simultaneous activations and duplicate input application.

Ambiguous dispatched model/subprocess outcomes become `unknown` and fail the
affected task, preserving reservations, rate state, cooldowns, and receipts.
There is no blind side-effect retry or automatic refund/reconciliation endpoint.
New failed event deliveries show the sanitized model failure reason when available;
provider deadline expiration and cancellation are reported separately, including
when a response or stream was interrupted. Other worker failures still direct the
administrator to daemon diagnostics rather than exposing raw process errors.
Legacy version-2 unfinished goals have no continuation/mailbox and remain
unrecoverable rather than being synthesized into new work. New goals require explicit starts.
Storage/accounting failures disable new dispatch; emergency stop remains available.

### Sandbox launch infrastructure

The phase-0 `run` demo and placeholder `worker` command have been removed.
Running without arguments prints daemon usage instead of executing a demo.
The real internal `worker operator` mode performs one correlated model turn,
relays at most one structured function action for daemon validation, acknowledges
the result/continuation, and exits. It never interprets model text as code.
Simulated workloads remain confined to integration-test fixtures.

The launcher applies the selected profile's workspace read/read-write directories
without expanding narrower grants, plus the target executable and required system
library/device access. The target environment
contains only private `HOME`/`TMPDIR`, `LANG`, and `TZ`; unrelated descriptors are
marked close-on-exec.
Sockets are rejected on standard descriptors too, so inherited connections cannot
replace the approved pipes.
Frames and captured stderr are capped at 64 KiB. Deadline or cancellation kills
the supervised process group, and the supervisor waits for its direct child.
The daemon removes activation workspaces after normal completion/cancellation.
The resource-confined Linux path additionally kills the whole PID namespace if
the daemon dies. Its private tmpfs disappears after the last process/descriptor
closes. Startup, under the exclusive state-directory lock, removes abandoned
runtime workspaces; persistent artifacts and unrelated files are left intact.

`sandbox-exec`, `sandbox-exec-profile`, `sandbox-build-profile`, `worker operator`, and `tool text-analyze` are internal modes,
not general-purpose user commands.
The launcher locks its OS thread, installs any platform restrictions, applies
nono-go, and immediately execs the target on that thread. Errors abort launch;
there is no unsandboxed fallback. On Linux/amd64, the extra seccomp filter denies
`socket`, `socketpair`, `io_uring_setup/enter/register`, and `pidfd_getfd`; it also
rejects alternate syscall ABIs. It cannot be loosened by a worker and is inherited
across exec, new threads, and descendants. The parent's networking is unchanged.

## Project layout

The root [`main.go`](main.go) handles CLI dispatch and signals. The implementation
is split into internal packages, not separate services:

| Package | Responsibility |
| --- | --- |
| [`internal/daemon`](internal/daemon) | Daemon lifecycle, authenticated control API, scheduling, model admission coordination, and task execution |
| [`internal/state`](internal/state) | SQLite, migrations, persisted types, scoped artifacts, and atomic durable workflows |
| [`internal/provider`](internal/provider) | Bounded model HTTP/SSE transport, response validation, and credential redaction |
| [`internal/sandbox`](internal/sandbox) | Fresh-process confinement, supervision, resource limits, and confined generated builds/runs |
| [`internal/worker`](internal/worker) | Reviewed operator and text-tool worker entry points |
| [`internal/ui`](internal/ui) | Detached browser UI and its authenticated Unix-socket client |
| [`internal/protocol`](internal/protocol) | Shared bounded JSON framing, strict decoding, and API token format validation |

Unit tests stay with their packages. Cross-boundary daemon/worker/UI/sandbox
integration tests stay together under `internal/daemon`; their harness builds the
real executable from the repository root. Root tests cover CLI arguments and
package/persistence boundaries. All existing `go run .`, `go build -o microoperator .`,
and `go test ./...` commands remain valid.

## Persistence architecture

[`internal/state`](internal/state) contains the SQLite implementation, migrations,
shared persisted types, scoped artifacts, and typed workflow operations such as
`SearchMemory`, `AdmitModelCall`, `Claim`, and `FinishDelivery`. HTTP handlers,
the model broker, and the execution engine no longer issue SQL or access database
handles. Tool and generated-evaluation preparation/completion are separate from
actual subprocess execution.

Transactions still include their associated audit records, events, receipts, and
budget reservations. SQLite remains the only backend, with the same single
connection, WAL/durability settings, schema 5, and JSON schema 1. No ORM, generic
repository framework, or extra dependency is introduced. A future backend would
implement the same workflows and qualify its migration, locking, and transaction
semantics; it would not be a driver-only replacement.

Storage-internal tests live alongside the implementation. Runtime/API tests call
the public workflows and may inspect their own temporary database through a
separate test-only connection. An architecture check rejects production SQL
imports outside storage and database types in its public contract. It also guards
package dependencies: the UI cannot embed the daemon, and state/protocol code
cannot depend on execution or transport implementations.

## Checks

```sh
CGO_ENABLED=1 go test ./...
CGO_ENABLED=1 go vet ./...
CGO_ENABLED=1 go test -race ./...
CGO_ENABLED=1 go test -tags=integration -count=1 ./...
CGO_ENABLED=1 go test -race -tags=integration -count=1 ./...
```

Linux integration checks now also require a running systemd user manager with
`cpu`, `memory`, and `pids` delegation and working user/mount/PID namespaces.
`TestResourceQualification` creates and collects its own temporary user unit;
missing support fails the requested integration run instead of silently skipping.

Default tests cover framing, strict configuration, real temporary SQLite
transactions/migrations, authorization, immutable revisions, pinned grants,
duplicate/concurrent commands, rollback, cancellation, and output bounds.
Broker checks use real temporary SQLite and fake HTTP providers with controlled
rate clocks: shared aliases/groups, fairness, reservations, queue/budget denial,
clock rollback, retries/cooldowns, streams, credential redaction, and schema recovery.
Integration tests build the real executable and use disposable processes and fixtures.
Sandbox denial probes use the integration-test binary; operator scenarios use the
actual production worker. Test-probe flags and operations are absent from normal builds. CLI
regressions ensure retired demo commands cannot silently return.
Daemon checks create two independent systems, revise one, restart gracefully and
after a crash, replay commands, rotate the control token, and revalidate removed
definitions. Inactive-system scenarios still assert zero provider calls and no
implicit workers. Operator scenarios assert fake-provider responses/usage, shared
limits, streaming cancellation, targeted stop, idempotency, and abrupt recovery.
Ollama and Azure scenarios use local HTTP/TLS fixtures and the real
`DefaultAzureCredential` CLI path with a fixture `az`, never a personal login.
They cover token rotation/expiry, keyless responses, streaming, redaction,
pre-dispatch authentication failure, and persisted usage across daemon restarts.
Team scenarios run two operators and children with one active slot per system,
native text subprocesses and scoped artifacts. Coverage includes tool/skill denial,
confused-deputy prevention, immutable assignments/drafts, revocation, delivery
deduplication/retries/limits, unknown effects, waiting-parent recovery, paused input
across a real daemon restart, and stopping a waiting team without affecting another
system. Version-1/2 migrations preserve prior receipts and accounting.
Memory and timer scenarios cover scope, retention, DST/coalescing and real restart.
UI scenarios use an actual detached process and prove work continues after it exits.
Learning scenarios cover agent-requested protected evaluations, budgeted model
comparisons, approval replay/expiry, restart, explicit rollback, and unchanged task
history. Linux generated-tool scenarios compile and execute through actual
resource-confined launchers, reject changed artifacts/toolchains and cancel an
active builder. No generated candidate is executed in the test runner.
The daemon acceptance checks ran on Linux/amd64 WSL2.
Sandbox checks cover filesystem denials, symlink escapes, TCP/UDP/ordinary Unix-connection
denials, thread/descendant inheritance, environment and descriptor isolation,
failed launch, deadlines, and process-group cancellation. No real model API is called.

Additional checks cover pathname/abstract Unix sockets, socket pairs,
`io_uring`, `pidfd_getfd`, x32 syscalls, socket-filter inheritance, inherited socket
descriptors, and rejection of socket-backed stdin before worker readiness.
No check contacts the real system resolver. Passing this diagnostic suite is not
approval of the full v1 sandbox profile.

## Required profile for generated execution

Generated build/evaluation/promotion must use the resource-confined Linux profile,
not the reviewed-code path, and preserve exact-artifact approval and narrowed
broker authority. Passing evaluation alone does not approve or assign a tool.

### Resource-confined Linux profile

For Linux/amd64, add this object to the selected sandbox profile:

```json
"resources": {
  "memory_bytes": 268435456,
  "workspace_bytes": 8388608,
  "processes": 128,
  "cpu_percent": 100
}
```

Build first, then run the actual daemon in its own delegated user service (not
`go run`, whose parent build process would occupy the controller's domain):

```sh
CGO_ENABLED=1 go build -o microoperator .
systemd-run --user --wait --pipe --collect \
  --property='Delegate=cpu memory pids' \
  --property="WorkingDirectory=$PWD" \
  --setenv=MICROOPERATOR_CONTROL_TOKEN --setenv=MICROOPERATOR_LLM_KEY \
  "$PWD/microoperator" daemon --config "$PWD/microoperator.json"
```

Pass each configured credential environment variable by name. This does not change
global systemd settings or require sudo. Unavailable delegation, namespaces,
controllers, mounts, or sandbox setup aborts the requested launch; there is no
fallback to the ordinary profile.

Each activation's cgroup v2 limits cover its threads and descendants:
`memory.max` with swap disabled and group OOM killing, `pids.max` (threads count),
and `cpu.max` using a 100 ms period. `cpu_percent:100` means one CPU's aggregate
runtime per period, **not** a lifetime CPU-seconds budget. A single private tmpfs
bounds combined scratch/output storage and 4096 inodes; imported inputs are
read-only. Bounds are memory 64 MiB-16 GiB, workspace 1 MiB-1 GiB (no larger than
memory), 32-4096 processes/threads, and 1-1000 CPU percent. Daemon-brokered tool
processes have their own bounded activation; these are not a daemon-wide RAM cap.

The child enters its cgroup atomically and gets private user/mount/PID/IPC/network namespaces.
Namespace init carries a sealed SIGKILL parent-death signal; its creating daemon
thread remains pinned until reap. Descendants cannot escape cleanup by calling
`setsid`, double-forking, clearing that signal, or changing credentials. Cancellation
uses `cgroup.kill` and waits for an empty group; restart removes abandoned empty
groups belonging to dead supervisors, never another live supervisor's group.
Artifact ingestion uses a retained workspace descriptor, after execution/cleanup,
rather than confusing the private mount with the host's underlying directory.

Real Linux checks cover namespace identity, sealed controls, aggregate disk
exhaustion, kernel CPU throttling, memory OOM, process/thread refusal, an escaped
process group, abrupt supervisor/daemon death, and durable artifact/accounting
recovery. This qualification applies to the explicit resource profile on the
checked host, not to nil-resource profiles or a support flag alone.

### Linux/WSL2 recheck

Checked 21 September 2026 on Linux/amd64, WSL2 kernel
`6.6.114.1-microsoft-standard-WSL2`, Go 1.24.0, GCC 13.3.0, and `CGO_ENABLED=1`.
The nono-go dependency was not changed for the Linux socket fix. Its Linux-amd64 archive records
native core commit `1d1c88c9f98f0a1f3ff79cff1509713aaec7cdb0` (0.65.1).
`nono.Version()` reports the FFI version (`0.1.0` here), not that native core version.

| Check | Observed result |
| --- | --- |
| Default tests and `go vet` | Passed on this host |
| Application integration suite, including race detection | Passed with the Linux launcher and supplemental seccomp filter |
| Confined pipe communication | Integration-only target exchanged bounded messages through the retained launcher |
| Binding support diagnostics | `IsSupported()` is true; `SupportInfo()` reports Landlock V3, with filesystem controls but no Landlock TCP or signal/abstract-socket scoping |
| Filesystem fixtures through the launcher | Granted reads/writes succeeded; protected reads/writes and symlink escapes denied |
| IPv4 TCP/UDP and pathname/abstract Unix sockets through the launcher | Denied, including Unix listeners outside the workspace |
| Alternate socket/descriptor paths | Socket pairs, `io_uring` calls, `pidfd_getfd`, and x32 calls denied; inherited sockets unavailable |
| Inheritance and cancellation | Filesystem and socket restrictions held in new threads/descendants; supervised process-group cancellation passed |
| Resource-confined profile | Delegated cgroup v2 CPU/memory/PID enforcement, bounded private tmpfs, PID-namespace cleanup and abrupt-death recovery passed on this host |
| Generated tools | Confined pinned-toolchain build, protected tests, exact approval/assignment/reuse, modified-binary denial and active-builder cancellation passed |

Before the fix, a native-only probe using `NetworkBlocked` could deliver a fixed
payload to pathname and abstract Unix-socket fixtures outside its granted workspace.
The [pinned Linux implementation](https://github.com/nolabs-ai/nono/blob/1d1c88c9f98f0a1f3ff79cff1509713aaec7cdb0/crates/nono/src/sandbox/linux.rs)
uses a seccomp network-block fallback on Landlock V3 that still permits Unix sockets.
The application now installs its own narrower filter before nono-go applies the
filesystem/network profile. It needs no binding fork, native rebuild, or new dependency.

The integration checks use the actual launcher, fresh exec'd workers, temporary
files, and local fixture sockets with positive controls. No real resolver or
external provider is contacted. This verifies the listed Linux boundaries, not
all profiles indiscriminately. The additional resource qualification above covers
hard limits, process-group escape containment, and cleanup after supervisor death
only when that profile is explicitly configured.

See [implementation-plan.md](implementation-plan.md) for milestone status and next
steps, and [spec.md](spec.md) for the target contract, architecture rationale, and
dependency research.
