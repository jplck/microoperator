# Copilot instructions - microorchestrator

## Sources of truth

- Read `SPEC.md` before changing behavior, architecture, protocols, resource
  shapes, security boundaries, dependencies, or scope.
- `SPEC.md` defines **what** the product is. `PLAN.md` defines **how and when**
  it is implemented.
- Keep them synchronized in the same change. A changed requirement updates
  `SPEC.md`; a changed implementation sequence or technology choice updates
  `PLAN.md`; a change affecting both updates both.
- If implementation and spec disagree, stop and resolve the spec explicitly.
  Do not silently make code the new design.

## Product invariants

1. No Kubernetes.
2. One host daemon and SQLite until measured requirements justify more.
3. Each agent runs in a jailed Firecracker microVM.
4. Agent artifacts remain unmodified OCI images.
5. Guest workloads have no general network interface in v0.1; governed traffic
   crosses the harness, vsock, and host gateway.
6. A2A is the agent protocol, MCP is the tool protocol, OpenAI-compatible APIs
   are the model protocol, and OpenTelemetry is the telemetry protocol.
7. The host gateway is the single OpenAI-compatible model endpoint; LiteLLM is
   an optional per-plugin adapter, and neither runs local model weights.
8. Every observable A2A, MCP, and model interaction is evaluated immediately
   before execution and fails closed.
9. Private in-process functions cannot be governed without an agent adapter;
   never claim otherwise. Governed tools use MCP.
10. Vendor-specific code and types live only in out-of-process plugins. The core
    must build and run with all vendor plugins disabled.
11. Every A2A, MCP, and model route is registered through the shared host
    governance middleware. Protocol handlers may not bypass it.
12. External identity uses out-of-process `identity-provider` plugins. Plugins
    validate and exchange credentials; the core normalizes principals and OPA
    alone authorizes interactions.
13. Prefer native extension points: MCP for tools, A2A for agents, OCI for
    artifacts, OpenAI-compatible APIs for models, and OTel for telemetry.
    Plugins translate or enrich; they never replace isolation, lifecycle,
    authorization, audit, or transaction ownership.
14. Plugin authors receive versioned JSON Schemas, fixtures, a fake host, and a
    conformance command. Schemas are authoritative; language SDKs require two
    maintained plugins with demonstrated duplicate plumbing.
15. Every accepted design decision is appended to the `PLAN.md` decision log
    (newest first) in the same change that edits `SPEC.md` or `PLAN.md`.

## Ponytail rules

- Use the shortest correct path: operating system, standard library, existing
  open-source component, then minimal custom code.
- Do not add an interface with one foreseeable implementation, a factory for
  one product, speculative configuration, generic repository layers, or
  scaffolding for later.
- Plugin, policy, and protocol boundaries are justified abstractions. New ones
  require a concrete second implementation or an explicit `SPEC.md` decision.
- Do not add UI plugins, arbitrary lifecycle hooks, storage-engine plugins, or
  configurable middleware chains.
- Prefer deletion over addition and one process over distributed components.
- Do not add a frontend framework, message broker, cache, external database,
  service mesh, or dependency-injection framework without measured need and a
  spec update.
- Mark a deliberate shortcut with `ponytail:` and state its ceiling and upgrade
  trigger.
- Non-trivial logic leaves one smallest runnable check. Do not build broad test
  matrices for trivial code.

## Security and governance

- Never weaken microVM isolation, jailer use, digest pinning, default-deny
  communication, policy fail-closed behavior, secret handling, input bounds, or
  auditability to reduce code.
- Policy snapshots use host-verified identity and metadata. Workload-supplied
  caller identity, delegation depth, labels, and limits are untrusted.
- Never return provider credentials from an identity plugin to a guest or
  another plugin. Credentials go only to the host protocol gateway and must be
  audience-scoped.
- OPA is stateless. Counters, budgets, approvals, and collective windows belong
  to the daemon and are passed into policy snapshots.
- Do not log credentials, tokens, full prompts, files, or tool payloads by
  default.
- A network proxy cannot observe private in-process tool calls. Require MCP or a
  voluntary runtime adapter.

## Implementation workflow

1. Locate the relevant `SPEC.md` requirement and `PLAN.md` phase.
2. Use `.github/skills/spec-sync/SKILL.md` for architecture or scope changes.
3. Implement only the current requirement; do not pre-build later phases.
4. Use Rust for the host daemon and trusted guest harness. Browser UI code may
   use native HTML, CSS, and JavaScript and does not need to match the systems
   language.
5. Reuse open standards and upstream APIs rather than wrapping them.
6. Run the smallest existing check that proves success and the relevant failure
   path.
7. Confirm documentation, schemas, and behavior still agree before completion.
8. Document every host dependency, permission change, pinned version, checksum
   verification, and install command in `README.md` in the same change.

Until implementation files exist, do not invent build commands. Add only
commands that have been run successfully.
