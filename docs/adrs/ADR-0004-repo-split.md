# ADR-0004 — Split the repo: extract `overseer` as a standalone tool with a published SDK

**Status:** Accepted
**Date:** 2026-04-25
**Phase:** 1.6 (executes after Phase 1.5 closes via [W10](../../workstreams/10-cleanup.md))

> ADR-1 / ADR-2 / ADR-3 are inline in [workstreams/01-engine-node-interpreter.md](../../workstreams/01-engine-node-interpreter.md), [workstreams/02-hcl-config-split.md](../../workstreams/02-hcl-config-split.md), and [workstreams/03-standalone-cli.md](../../workstreams/03-standalone-cli.md). ADR-4 is the first decision record to land as a standalone file under `docs/adrs/`; future ADRs follow this convention.

## Context

The current monorepo holds four interconnected modules:

| Module | Role |
|---|---|
| [workflow/](../../workflow/) | HCL parser + FSM compiler + `cty` eval context; no external dependents |
| [overseer/](../../overseer/) | FSM executor, plugin host, CLI (`compile`/`plan`/`apply`/`run`), local + Castle sinks, reattach |
| [castle/](../../castle/) | OverseerService + CastleService gRPC servers, SQLite store, event hub, auth |
| [parapet/](../../parapet/) | React/Redux dashboard UI; Connect-Web client to Castle |

Phase 1.5 work has surfaced a long-running ambiguity: are we shipping **one platform** (Overlord-the-monolith) or **two products** — `overseer`, a local FSM workflow runner with a plugin protocol (Terraform-shaped), and `overlord` (castle + parapet), a control plane that orchestrates many overseers (Terragrunt / Spacelift / Terraform Cloud shaped)?

Several signals point to the latter:

- [W03](../../workstreams/03-standalone-cli.md) shipped: `overseer compile | plan | apply` runs end-to-end without Castle.
- [castle/](../../castle/) has zero Go imports from [overseer/](../../overseer/); coupling is RPC-only via [proto/overlord/v1/](../../proto/overlord/v1/).
- [workflow/](../../workflow/) has zero project-internal dependencies (only hashicorp/hcl + zclconf/go-cty).
- The Terraform analogy is already documented: [PLAN.md §1.4](../../PLAN.md) references Terraform's go-plugin design; ADR-3 is described as "Terraform-style local-first CLI" in [W03](../../workstreams/03-standalone-cli.md#L13).

The trigger for *acting* on the ambiguity now is **concurrency on the codebase**. The `overseer` side is moving faster than the `castle`+`parapet` side; in a single repo, parallel agent contributions across the boundary serialise on each other unnecessarily, even though the boundary is already a clean RPC seam in the code. A split:

- Lets each side ship on its own release cadence.
- Lets external projects adopt `overseer` without taking on the orchestrator.
- Lets Overlord's own development use `overseer` workflows to drive itself (dogfooding) — the same way external consumers will.

## Decision

After Phase 1.5 closes (post-[W10](../../workstreams/10-cleanup.md)), Phase 1.6 splits the repo:

1. **`overseer` extracts to its own repo** (e.g. `github.com/brokenbots/overseer`) carrying:
   - [workflow/](../../workflow/), [overseer/](../../overseer/)
   - [proto/overlord/v1/adapter_plugin.proto](../../proto/overlord/v1/adapter_plugin.proto) (southbound to plugins)
   - [proto/overlord/v1/overseer.proto](../../proto/overlord/v1/overseer.proto) (northbound to orchestrators)
   - [proto/overlord/v1/events.proto](../../proto/overlord/v1/events.proto) (overseer-emitted payloads)
   - [shared/events/](../../shared/events/) (envelope helpers — folded into the SDK)
   - [docs/workflow.md](../workflow.md) (authored in [W09](../../workstreams/09-demo-new-docs.md)), [docs/plugins.md](../plugins.md)
   - Standalone-mode examples under [examples/](../../examples/) (sorted by `# mode:` header added in W09/W10)

2. **`overlord` (castle + parapet)** stays in the current repo, consuming the published overseer SDK. It keeps:
   - [castle/](../../castle/), [parapet/](../../parapet/)
   - [proto/overlord/v1/castle.proto](../../proto/overlord/v1/castle.proto) (parapet ↔ castle; no overseer consumer)
   - Orchestrator-required examples (those using `wait { signal }`, `approval`, etc.)

3. **Two SDK Go modules publish from the overseer repo:**
   - **`overseer-sdk`** (northbound) — generated Connect/gRPC stubs for `OverseerService`, `Envelope` + payload types, `SchemaVersion`, `TypeString`, `IsTerminal`. Castle imports this and implements the server side against it. Any other orchestrator does the same.
   - **`overseer-plugin-sdk`** (southbound) — generated stubs for `AdapterPluginService` plus reusable host-side helpers extracted from [overseer/internal/plugin/](../../overseer/internal/plugin/). Plugin authors `go get` this.

4. **Pre-1.0 versioning** (`v0.x`) on both Go SDKs so the shape can settle.

5. **Parapet TS bindings** for overseer-owned protos move to a published TS package (e.g. `@overseer/proto-ts`); `castle.ts` continues to be generated locally in the overlord repo.

The detailed Phase 1.6 task list is in [PLAN.md §1.6](../../PLAN.md). The per-workstream split-readiness constraints that W05–W10 honour during Phase 1.5 execution are documented in each workstream's `## Split readiness (Phase 1.6 prep)` section.

## Consequences

### Positive

- **Concurrency unblocked.** Two repos = two independent agent work streams; no more cross-boundary serialisation.
- **Clear product narrative.** New users see two products, not one: `overseer` (the executor) and `overlord` (the orchestrator). The standalone-overseer story stops being undersold by the directory layout.
- **SDK becomes a real interface.** Forces the team to ask "what does *any* orchestrator need from overseer?" rather than "whatever castle reaches into."
- **Independent release cadence.** Overseer's workflow-language work doesn't block castle UX work and vice versa.
- **Third-party adoption credible.** A standalone `brew install overseer` story works once it's a separate repo. Bundling it with a control plane that has open security work (per [tech_evaluations/TECH_EVALUATION-20260425-01.md](../../tech_evaluations/TECH_EVALUATION-20260425-01.md)) hurt that pitch.
- **Plugin-author DX improves.** Authors `go get` a small SDK module rather than cloning a repo called "overlord" to hunt for `adapter_plugin.proto`.
- **Dogfooding loop.** Overlord can use overseer workflows to drive its own development, sharing the integration burden with external consumers.

### Negative

- **Proto coordination becomes a multi-repo dance.** Each protocol change is two PRs in dependency order: overseer-side (definitions + SDK bump), then overlord-side (consume new SDK). The "permanent field number" rules in `events.proto` already enforce wire stability, but the cadence slows.
- **Doubled CI/release infrastructure.** Two GitHub Actions setups, two release pipelines, two changelog conventions, two Docker image flows. One-time setup plus recurring coordination.
- **Compose / dev orchestration gets more complex.** [compose.local.yml](../../compose.local.yml) needs to either pull a tagged `overseer` Docker image from a registry or document a side-by-side checkout dev story.
- **SDK design is real work.** A poorly shaped SDK is harder to fix than a poorly shaped internal package because external consumers exist. Pre-1.0 versioning mitigates but doesn't eliminate the cost.
- **Test coverage gaps become public.** Uneven coverage (per [tech_evaluations/TECH_EVALUATION-20260425-01.md](../../tech_evaluations/TECH_EVALUATION-20260425-01.md)) is internal debt today; once `overseer` is its own public repo, every gap is visible to plugin authors and integration partners.
- **Standalone overseer security posture is unfinished.** Adapter plugins run as subprocesses with no sandbox. Trusted-environment-only docs are required at minimum; sandboxing is future work.
- **Naming + identity churn.** Module path migration (`github.com/brokenbots/overlord/overseer/...` → `github.com/brokenbots/overseer/...`), broken links, doc cross-link refresh.

### Risk-mitigated by Phase 1.5 split-readiness work

Six W05–W10 workstreams now ship under split-readiness constraints (see each workstream's `## Split readiness (Phase 1.6 prep)` section):

- New event payloads and the new `OverseerService.Resume` RPC are designed as SDK contract surface from day one — no castle-isms.
- `applyRunStatus` cases for new payloads stay castle-side; engine code emits semantic events only.
- The W07 iteration cursor JSON stays opaque to castle (castle stores the W04 variable-scope blob verbatim).
- `parapet/src/gen/` is treated as fully generated; helpers live in sibling `parapet/src/api/`.
- Examples gain `# mode: standalone` / `# mode: orchestrator-required` headers in W09 so the W10 audit and the Phase 1.6 sort are mechanical.
- W10 lands a six-item audit checklist proving no castle imports under `overseer/internal/`, no overseer imports under `castle/internal/`, no castle-specific helpers in `shared/events/`, and orchestrator-neutral comments on every overseer-owned RPC and event.

The intent: at Phase 1.5 archive time, the actual Phase 1.6 work is directory moves + SDK package publishing + module-path rewrites — estimated <1 week of focused work given the clean starting state. The split-readiness constraints exist to protect that estimate.

## Alternatives considered

**Status quo (no split).** Keep the monorepo and rely on social conventions for the boundary. Rejected because the concurrency cost is real today and the social boundary doesn't scale to multiple parallel agent contributions. The technical work to enforce the boundary in code (the SDK shape) is a forcing function the monorepo doesn't provide.

**Split now (mid-Phase 1.5).** Rejected because W05–W08 each touch both sides of the boundary; coordinating five workstreams across two repos under one contributor amplifies coordination cost without delivering split benefits any sooner. Phase 1.5's exit demo (W09) and cleanup (W10) need a single-repo home. Better to land Phase 1.5 cleanly and execute the split as Phase 1.6.

**Three-repo split** (separate `workflow` lib, `overseer` executor, `overlord` orchestrator). Rejected because [workflow/](../../workflow/) has no consumers other than overseer; a third repo adds coordination cost without unlocking parallel work. If a future "policy linter" or "graph visualiser" needs the workflow library standalone, the workflow Go module inside the overseer repo can be promoted to its own module path within the same repo (`github.com/brokenbots/overseer/workflow`) without a third repo.

**Shared proto repo as neutral ground.** Rejected because there's no neutral party — overseer owns both wire protocols (southbound to plugins, northbound to orchestrators); castle is one consumer. Putting protos in a third repo with neither owner accountable for evolution creates drift risk. Overseer-as-owner is the right model.

## References

- [PLAN.md §1.6](../../PLAN.md) — Phase 1.6 task breakdown
- Per-workstream prep: [W05](../../workstreams/05-wait-approval.md), [W06](../../workstreams/06-branch.md), [W07](../../workstreams/07-for-each.md), [W08](../../workstreams/08-parapet-ui.md), [W09](../../workstreams/09-demo-new-docs.md), [W10](../../workstreams/10-cleanup.md) — see the `## Split readiness (Phase 1.6 prep)` section in each
- Long-form decision report (private planning notes): `~/.claude/plans/we-need-to-make-purring-milner.md`
- Boundary code today: [overseer/internal/transport/castle/client.go](../../overseer/internal/transport/castle/client.go), [overseer/internal/run/sink.go](../../overseer/internal/run/sink.go), [overseer/internal/run/local_sink.go](../../overseer/internal/run/local_sink.go), [overseer/internal/engine/engine.go](../../overseer/internal/engine/engine.go)
- Wire contract: [proto/overlord/v1/overseer.proto](../../proto/overlord/v1/overseer.proto), [proto/overlord/v1/events.proto](../../proto/overlord/v1/events.proto), [proto/overlord/v1/adapter_plugin.proto](../../proto/overlord/v1/adapter_plugin.proto)
- Established intent: [workstreams/03-standalone-cli.md](../../workstreams/03-standalone-cli.md), [tech_evaluations/TECH_EVALUATION-20260425-01.md](../../tech_evaluations/TECH_EVALUATION-20260425-01.md), [arch_reviews/v1.2-postreview.md](../../arch_reviews/v1.2-postreview.md)
