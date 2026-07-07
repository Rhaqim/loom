# Loom — Code Organization & Scalability Plan

## Why this exists

The root package (`package loom`) currently holds **40 files** (21 source + 19
tests), including two god-files — `engine.go` (~60 KB) and `persist.go` (~38 KB).
There are ~225 exported symbols (the public API) and ~194 unexported ones (pure
machinery) living side by side. In a single Go package the exported/unexported
split is the *only* encapsulation boundary, so today there is no wall between the
API applications depend on and the internals we want the freedom to change — and
that we may want to protect if this becomes enterprise tech.

This document is the plan to fix that **without breaking downstream consumers**.

## The core constraint (Go-specific)

In Go, **folder = package**. "Move files into folders" literally means "split one
package into several," which breaks two things:

1. **Consumers** who write `loom.Engine`, `loom.Session`, `loom.New(...)`.
2. **Internal cross-references** — every `s.e.*` service call and every shared
   unexported helper.

Two facts about the current code make a clean reorganization feasible:

- **Hub-and-spoke coupling.** Ten `xxxService` structs each embed `e *Engine` and
  call back into `s.e.db`, `s.e.cache`, `s.e.prompts`, … We do **not** try to
  break this apart — the whole engine moves as one package, so the hub-and-spoke
  stays intact and internal.
- **Favourable dependency directions.** `generator/*` and `harness` import root
  `loom`; but `judge`, `gc`, and `schema` do **not**, and the engine does **not**
  import `generator` or `harness`. The god-package sits near the top of the import
  graph — imported by extension packages, not importing them back — so making it
  internal behind a facade introduces **no import cycle**.

## The compatibility linchpin: type aliases

`type Session = engine.Session` is the *identical* type, not a wrapper. So
`res.(*loom.TextResult)`, `loom.Config{DB: db}`, and interface satisfaction all
keep working across a package boundary. This is what lets the implementation move
into `internal/` while `loom.X` stays valid for every consumer.

```go
// loom/loom.go — the facade (Phase 1)
package loom

import "github.com/rhaqim/loom/internal/engine"

type (
    Engine      = engine.Engine        // methods, fields, type assertions all carry over
    Config      = engine.Config
    Session     = engine.Session
    StepRequest = engine.StepRequest
    Result      = engine.Result        // interface: user implementations still satisfy it
    // …one line per public type
)

var (
    New           = engine.New         // funcs re-exported as vars
    NewTextResult = engine.NewTextResult
    ErrSkip       = engine.ErrSkip     // error identity preserved (errors.Is works)
    ErrNotFound   = engine.ErrNotFound
)

const (
    ModalityText   = engine.ModalityText   // consts re-declared
    DialectSQLite  = engine.DialectSQLite
    ActionFreeText = engine.ActionFreeText
)
```

## Target architecture

```
loom/                        ← PUBLIC FACADE (package loom): aliases + re-exports only
  loom.go, results.go, ...   ← type Session = engine.Session; var New = engine.New; const ...
  doc.go                     ← the documented public surface
  loom_ext_test.go           ← BLACK-BOX test (package loom_test): the consumer contract guard
  internal/                  ← ENTERPRISE WALL: unimportable by any other module
    engine/                  ← the hub: Engine, all services, RunStep/RunTurn, hooks, poller, template
    core/    (Phase 2)       ← domain types + interfaces (Session, Agent, Result, Generator…)
    store/   (Phase 2)       ← all SQL (persist.go, flow_store.go), imports core only
  generator/  judge/  gc/  schema/  harness/   ← stay public (extension points & tools)
```

`internal/` is the mechanism the enterprise requirement needs: anything under it
**cannot be imported by another module**, so customers/competitors can only touch
the curated facade.

## Phased rollout

Each phase is independently shippable and leaves the tree green.

### Phase 0 — Readability. Zero package changes, zero risk. (this change)

Stay in `package loom`; only split and rename files. Because it is all one
package, the compiler guarantees correctness — it is pure `git mv` + cut/paste.

- Break `engine.go` →
  `engine.go` (Config, Engine, New, accessors), `service_agent.go`,
  `service_prompt.go`, `service_session.go`, `service_step.go`,
  `service_cost.go`, `service_budget.go`, `poller.go`, `template.go`,
  `query.go` (the `queryAgent`/`insertAgent`… thin wrappers).
- Break `persist.go` →
  `store_agent.go`, `store_prompt.go`, `store_responseformat.go`,
  `store_session.go`, `store_flow.go`, `store_budget.go`, `store_cost.go`,
  `store_helpers.go`.
- Adopt a filename convention so the API is greppable:
  - `api_*.go` — the public contract types (Agent, Prompt, Session, Step,
    Result, Action, Flow, Generator, Config surface, errors).
  - `service_*.go` — the registry/service implementations.
  - `store_*.go` — SQL persistence.
  - everything else — implementation helpers.

Nothing external changes; nothing can break.

### Phase 1 — Erect the wall (the encapsulation step)

- `git mv *.go internal/engine/` and rename `package loom` → `package engine`.
  Still one cohesive package, so it compiles essentially untouched (hub-and-spoke
  stays inside `engine`). Tests that touch unexported fields move with the code,
  so they keep compiling.
- Generate the root facade (`loom.go` etc.): one alias per public type, `var`
  re-export per public func, `const` per public constant, `var` per public error.
- `generator/*` and `harness` need **no changes** — they import the facade, which
  now re-exports everything.
- External API is byte-for-byte compatible. This is where the inner workings
  become protected.

### Phase 2 — Deeper layering (optional, incremental, later)

Peel `internal/core` (domain types + interfaces) and `internal/store` (SQL) out
of `internal/engine`, one subsystem at a time, enforcing `core ← store ← engine`.
Buys store-level testability and prevents the god-package from re-forming. Do this
only if/when desired; Phase 1 already delivers the encapsulation.

## Risks & mitigations

- **Facade drift** (a new public type added without a re-export): add a black-box
  `package loom_test` that imports `loom` as a real consumer, plus a small CI test
  that asserts every exported identifier in `internal/engine` has a facade
  counterpart. Self-policing.
- **Godoc indirection**: aliased types render as `= engine.Session`. Acceptable;
  Phase 2's `core` package with doc comments at the alias site mitigates it.
- **One-time churn**: Phase 1 is a large mechanical diff. Do it on a dedicated
  branch, keep the full `-race` suite green at each step, and land Phase 0 first so
  the god-files are already split before they move.

## Recommendation & sequencing

1. **Phase 0 now** — biggest readability win for the least risk; makes Phase 1
   trivial.
2. **Phase 1** as a single focused PR when ready to commit to the facade — this is
   the one that delivers enterprise encapsulation.
3. **Phase 2** opportunistically.

## Status

- [x] Phase 0 — split god-files, apply naming convention
- [x] Phase 1 — move to `internal/engine` + root facade
- [ ] Phase 2 — extract `internal/core` and `internal/store`

### Phase 1 — as-built notes

- All 58 root `*.go` files moved to `internal/engine/`, `package loom` →
  `package engine`. The engine remained a single cohesive package, so it compiled
  essentially untouched; hub-and-spoke coupling stays internal.
- Root facade is **generated**: `internal/tools/facadegen` scans the exported
  top-level declarations of `internal/engine` and emits `aliases.go`
  (98 type aliases, 31 const re-exports, 35 func/var re-exports). Regenerate with
  `go generate ./...` (directive in `doc.go`).
- `facade_test.go` is the **drift guard**: it fails if any exported
  `internal/engine` symbol is missing from the facade. Verified it both passes on
  the current tree and fails when a new un-re-exported symbol is introduced.
- Zero changes needed in `generator/*`, `harness`, `cmd/loom-cli`, or the three
  example modules — they import the facade and it re-exports everything they use.
  No import cycles (the engine imports `judge`/`gc`/`schema`, none of which import
  the facade; `generator`/`harness` import the facade, which the engine does not
  import back).
- Encapsulation confirmed: example modules (separate modules) cannot import
  `internal/engine`; Go's `internal/` rule enforces this at build time.
- Full `-race` suite and all example-module builds are green.

**Known follow-up (Phase 2 candidates):** `CacheTTL` is re-exported as a copied
value; it is never reassigned anywhere today, but if a settable global is ever
required it should move behind a setter or Config field (a facade copy cannot
propagate writes across the package boundary). A few files under `internal/engine`
(`hook.go`, `cache.go`, `bus.go`, `responseformat.go`, `flow.go`, `schemahook.go`)
still mix public types with implementation; splitting them into `internal/core`
(types/interfaces) and `internal/store` (SQL) is the Phase 2 work.
