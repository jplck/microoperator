---
name: spec-sync
description: Keep microorchestrator's SPEC.md, PLAN.md, and implementation aligned. Use for changes to scope, architecture, protocols, resources, security boundaries, dependencies, or implementation phases.
license: MIT
---

# Spec synchronization

1. Read the relevant sections of `SPEC.md`, `PLAN.md`, and
   `.github/copilot-instructions.md`.
2. Classify the request:
   - product behavior or constraint: update `SPEC.md`;
   - implementation sequence or technology: update `PLAN.md`;
   - durable repository rule: update Copilot instructions;
   - more than one category: update every affected file in the same change.
3. Search for conflicting statements by resource and protocol name.
4. Apply the smallest coherent edit. Do not duplicate the same detail across
   files: the spec owns what, the plan owns how, and instructions own workflow
   invariants.
5. Check these fixed boundaries:
   - no Kubernetes;
   - Firecracker microVM per agent;
   - unmodified OCI workload;
   - no guest NIC in v0.1;
   - A2A, MCP, OpenAI-compatible APIs, OPA/Rego, and OTel;
   - all network-visible interactions are governed and fail closed;
   - A2A, MCP, and model routes use the shared host governance middleware;
   - vendor behavior stays in out-of-process plugins;
   - external identity plugins validate and exchange credentials while the
     core normalizes principals and authorizes;
   - prefer MCP, A2A, OCI, OpenAI-compatible APIs, and OTel over new plugin
     capabilities;
   - plugins translate or enrich and cannot replace isolation, lifecycle,
     authorization, audit, or transaction ownership;
   - plugin integration uses the schema, fake host, fixtures, and conformance
     command before adding any language SDK;
   - the host gateway is the single OpenAI-compatible model endpoint; any model
     adapter (e.g. LiteLLM) is optional and per-plugin, and neither executes
     local model weights;
   - guest outbound HTTP is default-deny and crosses the harness egress proxy
     to the host gateway.
6. Reject claims that a sidecar can inspect private in-process function calls.
7. Append accepted design decisions to the `PLAN.md` decision log (newest
   first) in the same change that edits SPEC.md or PLAN.md.
8. Report any unresolved contradiction instead of inventing behavior.
