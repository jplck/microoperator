# microorchestrator Implementation Plan

Status: draft v0.1

`SPEC.md` defines what the product is. This file defines the shortest sequence
that can prove and implement it. If this plan changes product behavior or scope,
update `SPEC.md` in the same change.

## 1. Implementation stance

- Build vertical slices, not framework layers.
- Keep one daemon until measurements require another service.
- Use existing binaries through stable APIs before importing large SDKs.
- Prove Firecracker, OCI, vsock, and policy enforcement before building the UI.
- Each phase ends with one executable acceptance check.
- Do not implement deferred features while an earlier phase remains unproven.

## 2. Proposed stack

These are implementation choices, not permanent product contracts:

| Area | Choice | Reason |
|---|---|---|
| Host daemon and guest harness | Rust, one toolchain | Static Linux binaries and memory-safe low-level runtime code |
| HTTP API | Rust HTTP crate selected in Phase 1 | Do not add a server dependency before the API phase |
| Storage | SQLite | Single-host durability with no database service |
| UI | server-rendered HTML, CSS, ES modules | No frontend build system until needed |
| Isolation | Firecracker + jailer v1.16.1 | Required pinned microVM boundary |
| Guest channel | AF_VSOCK | Avoid a guest NIC and make mediation unavoidable |
| Model gateway | host gateway (OpenAI-compatible) | One proxy; name routing and policy live in core |
| Model adapter | optional, e.g. LiteLLM, inside a plugin | Only for non-OpenAI-native providers |
| Local model runner | llama.cpp server | Small standalone local inference runtime |
| Governance | ACS-compatible snapshots/verdicts + OPA/Rego | Open, deterministic, fail-closed policy |
| Agent protocol | A2A and OpenAI Responses API | Portable agent interoperability |
| Tool protocol | MCP | Portable tool discovery and invocation |
| Plugins | JSON-RPC 2.0 over stdio | Language-neutral and process-isolated |
| Telemetry | OpenTelemetry | Open traces, metrics, and logs |

Avoid an in-process plugin ABI, gRPC, message broker, Redis, PostgreSQL, frontend
framework, and dependency-injection framework in v0.1.

### 2.1 Extension rule

Before adding a plugin capability:

1. use the existing protocol extension point when MCP, A2A, OCI,
   OpenAI-compatible APIs, or OpenTelemetry already covers it;
2. otherwise keep vendor translation out of process;
3. define a new JSON-RPC capability only for two concrete implementations or
   an explicit `SPEC.md` requirement.

v0.1 implements only `environment`, `model-provider`, and
`identity-provider`. Add `policy-engine`, `annotator`, `secret-provider`, and
`approval-channel` capabilities only when their second implementation is
scheduled. Plugins may translate or enrich data; isolation, lifecycle,
authorization, audit, and transaction ownership remain in the daemon.

## 3. Repository shape

Create directories only when their phase begins:

```text
src/                      daemon entrypoint and trusted guest binaries
plugins/                  first-party plugin executables
policies/                 default Rego bundle and tests
web/                      HTML, CSS, and ES modules
api/                      OpenAPI and plugin JSON Schemas
test/                     end-to-end host checks
```

No `pkg/`, generated client SDK, or generic framework package until an external
consumer exists.

## 4. Phase 0 - feasibility gates

Estimated effort: 5-8 engineer-days.

### 4.1 Host prerequisite check

Build a small command that reports:

- architecture and Linux version;
- `/dev/kvm` access;
- cgroup v2;
- Firecracker/jailer versions and hashes;
- vsock support;
- at least 10 GiB free on the working filesystem for the feasibility assets.

Check: command exits non-zero with a useful reason on an incompatible host.

### 4.2 Jailed hello-world microVM

- Use Firecracker CI kernel 6.1.155 and Ubuntu 24.04 guest filesystem with
  repository-pinned SHA-256 hashes for the feasibility spike.
- Build the trusted feasibility guest helper in Rust, matching the host daemon.
- Boot a pinned kernel and read-only base rootfs through the jailer.
- Start an unprivileged guest process.
- Apply CPU, memory, PID, file-descriptor, disk, and runtime limits.
- Stop and clean exact resources.

Check: guest prints a nonce over vsock and cannot alter the base disk.

### 4.3 Bidirectional vsock proxy

- Use a 4 KiB request ceiling for the feasibility protocol.
- Host sends a request to a loopback HTTP service in the guest.
- Guest sends a request to a host test service.
- Route guest outbound HTTP through a loopback forward proxy that crosses vsock
  to the host gateway; confirm proxy-ignoring clients still reach no network.
- Verify streaming, cancellation, timeout, and bounded message sizes.
- Boot without a network interface.
- Bind workload identity to the dedicated host-side vsock Unix socket and
  Firecracker instance handle; treat CID as routing metadata only.
- Prove a second VM cannot use the first VM's host-side channel.

Check: guest reaches the allowed host service, has no internet route, and a
second VM cannot impersonate it.

### 4.4 OCI artifact spike

- Use the upstream `a2aproject/a2a-samples` Python hello-world agent at commit
  `6603ba3f2c31a7ef33e70b9d8b5b5f8be42ac9a3`.
Compare only two concrete paths:

1. Materialize an OCI image into a read-only rootfs/block device.
2. Boot a minimal guest containing `runc` and mount the workload bundle as a
   separate read-only disk.

Choose the shorter reliable path. Record boot latency, disk amplification,
cleanup behavior, and required privileged operations. Do not build a general
artifact abstraction during the spike.

Check: run one existing A2A sample agent image without modifying its contents.

### 4.5 Governance spike

- Pin the feasibility evaluator to OPA v1.20.1.
- Build one ACS-compatible snapshot for a model request.
- Evaluate one Rego bundle.
- Exercise allow, deny, and timeout/fail-closed behavior.
- Confirm policy digest and trace ID appear in the decision event.

Check: denied traffic never reaches the fake upstream.

### Exit

Stop the project if OCI execution, vsock streaming, or unavoidable policy
mediation cannot be made reliable without adding a container orchestrator.

## 5. Phase 1 - one governed agent and model

Estimated effort: 8-12 engineer-days.

### 5.1 Minimal daemon

- Single process with lifecycle context and bounded worker pool.
- SQLite migrations embedded in the binary.
- Tables only for agents, models, edges, policies, operations, and events.
- Exact data directory ownership and locking.
- Loopback HTTP API with bootstrap-token-to-session authentication.

### 5.2 Protocol governance middleware

- Implement one middleware pipeline for source identity, edge lookup, ACS
  snapshot construction, policy evaluation, verdict handling, and audit.
- Keep A2A, MCP, and model extraction in thin typed adapters.
- Make governed route registration the only route-registration API available to
  protocol handlers.
- Exercise allow and deny with a fake upstream before adding real protocols.

### 5.3 Agent lifecycle

- Create an agent from an OCI reference.
- Resolve and store its digest.
- Materialize workload disk.
- Allocate jailer identity, cgroup, socket paths, and vsock CID.
- Start, health-check, stop, and delete.
- Reconcile after daemon restart.

### 5.4 Guest harness

- Trusted PID 1/supervisor.
- Run workload unprivileged.
- Inject standard endpoint variables.
- Inject `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` for the loopback egress proxy.
- Proxy Responses/A2A ingress, model egress, and allow-listed HTTP egress over
  vsock.
- Forward bounded logs and health.

### 5.5 Model path

- Route model requests directly through the host gateway (no separate proxy).
- Register one existing OpenAI-compatible remote endpoint.
- Route an agent request through pre-model policy to the gateway.
- Return streaming output through post-model policy when configured.

### 5.6 Minimal UI

- Host status.
- Agent create/list/detail/start/stop/delete.
- Model register/list/health.
- Operation progress through SSE.
- Policy decision list.

Check: create an agent in the UI, call a remote model, deny the next request by
changing policy, and prove the upstream did not receive it.

## 6. Phase 2 - local model installation

Estimated effort: 5-8 engineer-days.

### 6.1 Plugin protocol

- Define one JSON-RPC 2.0 protocol version and manifest schema.
- Implement process supervision, deadlines, output bounds, capability grants,
  and opaque secret handles.
- Implement a fake host and
  `microorchestrator plugin check <executable>` conformance command.
- Publish minimal fixtures for initialization, health, success, protocol error,
  timeout, oversized output, and malformed output.

Only implement the `model-provider` capability in this phase.
Do not add a language SDK; add one only after two maintained plugins in that
language duplicate the same protocol plumbing.

### 6.2 Hugging Face and llama.cpp plugin

- Resolve an explicit repository and revision.
- Surface license and size before operator approval.
- Download resumably into content-addressed storage.
- Verify expected files and hashes.
- Start a pinned llama.cpp server with calibrated CPU/GPU settings.
- Register the stable model name with the host gateway.
- Remove only unreferenced artifacts.

Check: install, run, stop, restart, and remove one small permissively licensed
model from the UI.

## 7. Phase 3 - MCP tools

Estimated effort: 5-8 engineer-days.

- Register a remote MCP server.
- Install one local OCI MCP server as an isolated workload.
- Discover and display tools.
- Create agent-to-tool edges.
- Route MCP calls over vsock through pre/post tool policy.
- Enforce argument size, timeout, approval, and result labels.
- Prevent direct workload access to the MCP server.

Check: allow a read tool, deny a write tool, then escalate it for approval.

## 8. Phase 4 - A2A systems and graph UI

Estimated effort: 7-10 engineer-days.

### 8.1 Registry

- Publish local A2A cards.
- Import explicitly trusted external cards.
- List candidates without granting access.
- Validate card version and endpoint changes.

### 8.2 Runtime path

- Create directional agent-to-agent edges.
- Route A2A send and stream operations:

  ```text
  Agent A -> harness/vsock -> host gateway -> vsock/harness -> Agent B
  ```

- Preserve trace and task context.
- Attach host-verified caller and delegation chain.
- Enforce maximum depth, deadlines, and monotonic scope narrowing.
- Reject calls without an active edge even when the target exists.

The control API updates routes but is not invoked by each message. The data path
is logically direct and physically mediated.

### 8.3 Graph UI

- Show agents, tools, and models as nodes.
- Create or remove a directional edge.
- Select allowed skills/operations and policy parameters.
- Show live health and recent allow/deny results.

Start with accessible HTML lists and forms. Add a graph visualization library
only if operator testing shows the lists are inadequate.

Check: create a two-worker system from the UI, stream one A2A task, remove its
edge, and observe the next call denied.

## 9. Phase 5 - external environment plugins

Estimated effort: 5-10 engineer-days per environment.

Implement the `environment` capability only with operations required by the
first environment:

- list and inspect external resources;
- map external IDs to core resources;
- import compatible definitions;
- report unsupported features explicitly.

Implement the Foundry plugin without importing its SDK or resource shapes into
the daemon.

Check: disable the plugin and prove the core and local runtime still work.

### 9.1 Identity-provider plugins

Estimated effort: 10-16 engineer-days after the local identity path works.

1. Define one JSON-RPC `identity-provider` capability for token validation,
   principal mapping, and audience-scoped token acquisition/exchange.
2. Keep authorization and delegation limits in the core policy middleware.
3. Implement Keycloak OIDC and RFC 8693 token exchange first.
4. Implement Entra Agent ID provisioning and token flows through the Microsoft
   Entra ID Auth SDK sidecar.
5. Add optional SPIFFE JWT-SVID federation to both providers after a
   Firecracker/SPIRE attestation spike proves the binding.

Check: the same core principal and policy decision result from equivalent
Keycloak and Entra identities; disabling both leaves local agent identity
working.

## 10. Phase 6 - governance depth and v0.1 release

Estimated effort: 8-12 engineer-days.

Add in this order:

1. policy hierarchy and narrowing composition;
2. evaluate-only rollout and policy tests;
3. persistent human approvals;
4. information-flow labels across A2A, MCP, and model calls;
5. per-agent and per-edge token/cost budgets;
6. mesh-wide sliding-window limits;
7. kill switch and bounded restart suppression;
8. optional content annotators.

Stateful controls live in the daemon and enter the policy snapshot. Do not make
OPA hold counters. This phase completes the governance requirements in
`SPEC.md` and is required for v0.1; additional policy families remain
demand-driven. The initial annotator and approval path use their direct HTTP
and UI boundaries; do not create deferred plugin capabilities yet.

Check: one trace proves label propagation and a mesh-wide limit across two
agents.

## 11. GitHub Copilot spec-driven workflow

### 11.1 Repository instructions

`.github/copilot-instructions.md` is always active and defines:

- `SPEC.md` as the authority for product behavior;
- `PLAN.md` as the implementation sequence;
- same-change synchronization;
- standards, security boundaries, and Ponytail constraints;
- required validation before completion.

### 11.2 Project skill

`.github/skills/spec-sync/SKILL.md` is useful because Agent Skills are an open
standard supported by Copilot cloud agent, code review, CLI, app, VS Code, and
JetBrains. The skill provides one repeatable architecture-change workflow
without bloating global instructions.

Use it when a task changes scope, architecture, protocols, resources, or
dependencies. Do not invoke it for typo fixes or implementation that already
matches the spec.

### 11.3 Path-specific instructions

Add `.github/instructions/*.instructions.md` only after code exists and two
areas genuinely need conflicting build or style rules. Repository-wide
instructions are enough now.

### 11.4 Custom agents and MCP

- A read-only architecture-review custom agent may be useful once implementation
  spans multiple subsystems. It is premature in the documentation-only repo.
- Do not add a project MCP server. Existing file, GitHub, and test tools cover
  spec-driven development.
- Use Copilot plan mode for implementation phases, but keep durable decisions
  in `SPEC.md` and `PLAN.md`, not chat history.

## 12. Validation gates

Every implementation change must run the smallest relevant checks:

- formatting and static analysis;
- unit tests for changed decision logic;
- one protocol conformance test for changed boundaries;
- one failure-path test for policy/security changes;
- spec/plan synchronization check.

Before a release:

- clean-host install;
- Firecracker production-host checklist;
- OCI digest and license inventory;
- plugin conformance;
- A2A, MCP, OpenAI, OpenAPI, and OTel compatibility;
- policy bypass attempt from an untrusted agent;
- restart/orphan reconciliation;
- dependency and license audit.

## 13. Risks

| Risk | Response |
|---|---|
| Firecracker requires Linux/KVM and privileged host setup | Make it explicit and fail prerequisites early |
| OCI is not a native Firecracker workload format | Prove one materialization path before core design |
| Guest harness can be confused with agent-integrated governance | Document observable network boundary honestly |
| Sidecar/proxy cannot see private function calls | Require MCP or voluntary runtime adapter |
| An optional model adapter is mistaken for a core requirement | Route OpenAI-native backends directly; add an adapter only per provider plugin |
| Policy evaluation blocks all traffic when unavailable | Local evaluation, bounded latency, fail closed |
| Stateful limits are incorrectly placed in OPA | Keep counters and approvals in SQLite/daemon |
| Plugin API becomes a speculative framework | Add methods only for shipping plugins |
| Foundry concepts leak into core | Enforce protocol conformance tests and dependency boundaries |
| AGT/ACS changes before GA | Pin versions and keep its adapter behind the policy contract |
| Monolithic daemon mixes control and data concerns | Separate modules and queues, not processes, until measured |

## 14. Rough effort

One experienced engineer, excluding product design and external certification:

| Milestone | Cumulative estimate |
|---|---:|
| Feasibility decision | 1-2 weeks |
| One governed agent + remote model + minimal UI | 3-5 weeks |
| Local model installation | 4-7 weeks |
| MCP management | 5-9 weeks |
| A2A systems and graph UI | 7-11 weeks |
| First external environment plugin | 8-14 weeks |
| Governance depth and release hardening | 11-18 weeks |

The largest uncertainty is safe OCI execution inside Firecracker, not the
operator API or UI.

## 15. Decision log

Newest first. One row per accepted design decision that changed the docs.
Append here in the same change that edits SPEC.md or PLAN.md.

| Date | Decision | Why |
|---|---|---|
| 2026-08-29 | Defer the Rust HTTP crate choice until Phase 1 | Phase 0 has no HTTP API, so selecting and adding a server dependency now would be speculative |
| 2026-08-29 | Select direct OCI rootfs materialization over in-guest `runc` | The measured path booted an unmodified A2A agent in 2.7 seconds; `runc` adds runtime and namespace plumbing inside an already isolated microVM |
| 2026-08-29 | Use the pinned upstream A2A Python hello-world sample for the OCI spike | It is a small credential-free standard agent and keeps workload language independent from the Rust host |
| 2026-08-29 | Bound Phase 0 vsock test requests to 4 KiB | Small fixed messages are enough to prove streaming, cancellation, timeout, and isolation before protocol framing exists |
| 2026-08-29 | Pin the Phase 0 governance evaluator to OPA v1.20.1 | Makes the ACS/Rego feasibility result reproducible with an official checksum-verified binary |
| 2026-08-29 | Replace Go with Rust for the host daemon and trusted guest harness; browser UI remains HTML/CSS/JavaScript | Keeps low-level host and guest code on one memory-safe toolchain without imposing the rule on native browser code |
| 2026-08-29 | Use Go for both the host and trusted guest harness; do not add C or Rust | One toolchain is enough for static Linux binaries and vsock syscalls |
| 2026-08-29 | Pin the Phase 0 guest spike to Firecracker CI Linux 6.1.155 and Ubuntu 24.04 assets by SHA-256 | Uses known Firecracker-compatible inputs without creating a guest-image build system |
| 2026-08-29 | Pin Phase 0 Firecracker and jailer to v1.16.1 and document host setup in `README.md` | Reproducible KVM access, binary installation, and checksum verification must precede VM spikes |
| 2026-08-29 | Start Phase 0 with `microorchestrator host-check` and require 10 GiB free working storage | Proves host incompatibility early without scaffolding later runtime phases; 10 GiB leaves room for the first pinned kernel, rootfs, and OCI spike |
| 2026-08-29 | Add a default-deny HTTP(S) egress proxy in the harness (SPEC §8.5) | Keeps the zero-code contract honest: outbound URLs become policy decisions, not silent failures |
| 2026-08-29 | Host gateway is the single OpenAI-compatible endpoint; LiteLLM demoted to an optional per-plugin adapter | v0.1 backends are OpenAI-native, so a second proxy was redundant; removes a process and config-reload machinery |
| 2026-08-29 | Standardize component name on "host gateway"; reserve "governance middleware" for the §8.7 pipeline | Terminology was inconsistent and conflated component with pipeline |
