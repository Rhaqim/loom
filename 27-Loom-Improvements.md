# 27 — Loom Improvement Proposals

**Audience:** loom maintainers, who do **not** have access to the application that surfaced these findings. Every proposal below is written to stand on its own, cites only loom's own source (module `v0.0.0-20260722143750-4eb06ba9cfd8`), and is framed as a **general-purpose** capability.

**Framing — loom is a library, not this app's backend.** loom is embedded by multiple applications; none of these proposals should assume any one domain. Where a concrete consumer is described, it's as *a representative multi-tenant, per-request-resolving embedder* — the common shape loom already targets (owner-scoped agents, versioned prompts, a read-through cache on the RunStep hot path). If a proposal only makes sense for one app, it doesn't belong here; each below is written to justify itself for any such embedder. These are proposals, not prescriptions.

---

## 1. The configured cache does not cover the reads the hot path makes

This is a concrete gap, not a design opinion, and it's the highest-value item.

### 1.1 What loom caches today

`Cache` is documented as a read-through cache for immutable objects, keyed by `owner:slug:version` or by row UUID (`internal/engine/cache.go:14-16`, `cacheKeyOwned`). Those reads are cached correctly:

- `Agents().Get(owner, slug, version)` with a concrete version → cached (`service_agent.go:25-34`)
- `Prompts()` by UUID → cached (`service_step.go:421,450`)
- `Agents().GetByID` → cached (`service_agent.go:43`)

### 1.2 What it does not cache — the resolutions every embedder actually calls

A caller that wants "the current published agent/prompt/flow" — i.e. anyone running the latest version rather than pinning a historical one — calls the **latest/active** resolvers. All three hit the database on every call, cache untouched:

| Resolver | Behaviour | Evidence |
|---|---|---|
| `Agents().Latest(owner, slug)` (also `Get` with `version == 0`) | newest version, raw DB | `service_agent.go:19-21, 37-39` |
| `Prompts().Latest(owner, slug)` | newest version, raw DB | `service_prompt.go:32-33` |
| `Flows().LatestActive(owner, slug)` | active version, raw DB | `flow_store.go:152`, `sqlQueryFlowLatestActive:271` |

Running "at latest" is the default posture for most embedders (you publish a new version and expect it to take effect). So in the common case, **the cache sits in front of a door nobody uses while the hot path goes straight to the DB.** A representative per-turn workload that resolves one active flow plus several latest agents does N uncached round-trips per request with a warm cache configured.

### 1.3 Why it was left out, and the fix

"Latest/active" is a **mutable pointer** — `Create`/`SetActive` change what it resolves to — so it can't share the immutable `owner:slug:version` key without going stale. The fix is the standard mutable-pointer cache: **cache the pointer, evict on write.** loom's `Cache` interface already exposes `Delete` (`cache.go:31`) for exactly this and is otherwise unused by the latest path.

**Proposed shape (backend-agnostic, matches the existing interface):**

1. Cache latest/active resolutions under mutable keys, e.g. `loom:agent-latest:<prefix>:<owner>:<slug>`, `loom:prompt-latest:…`, `loom:flow-active:…`. Store either the resolved version number (then reuse the existing immutable cache for the record) or the record itself.
2. **Invalidate on every write** to that pointer:
   - `Agents().Create` / `Prompts().Create` → `Delete` the corresponding `-latest` key.
   - `Flows().SetActive` (`flow_store.go:156`) → `Delete` the `flow-active` key.
3. Use a **short, separate TTL** for the latest keys (seconds–minutes) as a self-healing backstop against a missed eviction — the immutable `defaultCacheTTL` of 24h (`cache.go:37`) is correct for version-pinned records but wrong for a mutable pointer.
4. **Keep owner in every latest key.** `cacheKeyOwned` already documents why (`cache.go:50-54`): slugs are unique only within an owner, so an ownerless key would serve one tenant's active record to another — the cache silently defeating loom's own SQL owner filter.

**Why this generalizes:** it needs no knowledge of any embedder's domain. It closes the gap between "loom accepts a cache" and "loom's cache covers the calls loom's own runtime makes," for every consumer, with the interface loom already ships.

---

## 2. Proposed capabilities (motivated by a general multi-tenant embedder)

These are additive. Each states loom's current model, the general gap, a proposed abstraction, and why it stays domain-neutral. Order is by value.

### 2.1 Multi-level (hierarchical) scope resolution

**Loom today:** an agent/prompt/flow is addressed by `owner` + `slug`; resolution is two-level — a specific `owner`, falling back to the global `""` owner (`aliases.go:74-77`). Slugs are unique within an owner.

**The general gap:** many embedders have **more than two levels of specificity**. A representative shape is *organization → team → global*, or *tenant → sub-scope → global*: a narrower scope wants to override a broader one without editing the broader record, and resolution should walk narrow→broad→global, taking the first hit. Two levels forces embedders to either flatten (encode the whole path into one opaque `owner` string and lose the fallback chain) or issue the chain of lookups themselves.

**Proposed abstraction:** let a resolve accept an **ordered list of owners** (most specific first) and return the first that resolves, instead of a single owner with an implicit `→ ""` fallback. loom stays domain-neutral — it never interprets the levels, it just walks the list. `Latest([]string{"tenant:topic", "tenant", ""}, slug)` resolves the chain server-side in one call, which is also cacheable as one keyed result (composes with §1).

**Why this generalizes:** loom already treats `owner` as opaque; this only generalizes *one* owner to an *ordered fallback* of owners. No domain concept enters loom.

**Note:** an embedder can already emulate this client-side by issuing the lookups in order (cheap once §1 lands). Native support is cleaner and cacheable but optional — offered as a convenience, not a necessity.

### 2.2 A managed template-variable registry with producer/consumer semantics

**Loom today:** a step renders its user template against per-call `Inputs` (a key/value map supplied at `RunStep` time). There is no *declared, stored* set of variables an owner defines once and templates reference across turns, and no notion of which agent is expected to **produce** a value another agent **consumes**.

**The general gap:** an embedder that lets non-engineers customize multi-agent flows commonly wants **named variables** that (a) are declared and stored per owner, (b) are referenced by any agent's template, and (c) carry *producer/consumer* wiring — "agent A must emit `X`; agent B's template reads `X`." Today each embedder re-implements this on top of raw `Inputs`, re-deriving the same registry-with-wiring every time.

**Proposed abstraction:** a `Variables` registry alongside `Agents`/`Prompts`/`Flows` — owner-scoped, versioned like the others — holding variable definitions (name, description, optional `produced_by` agent slug). Templates reference them the same way they reference `Inputs`; loom resolves declared variables into the render context and can validate at publish time that every consumed variable has a producer. loom needs no idea what the variables *mean* — it manages declaration, scoping, versioning, and producer/consumer validation as data.

**Why this generalizes:** it's the same shape as loom's existing registries (owner-scoped, versioned, immutable), applied to a fourth object type. The *meaning* of any variable stays entirely with the embedder.

**Priority note:** for any embedder migrating prompt authority into loom's registry, this is the feature most likely to be **silently lost** if it isn't given a home — it has no loom equivalent today, so it vanishes the moment an embedder retires whatever external store currently holds it. Worth deciding early.

### 2.3 Cache invalidation on publish — folds into §1

A read-through cache of mutable pointers is only correct with eviction on write. §1 already specifies this (`Create`/`SetActive` → `Delete`). Called out separately only because it's the half embedders most often discover missing: they add caching, then see stale reads after a publish. Doing §1 is doing this — do not build it twice.

### 2.4 Soft-disable (an "enabled" flag) — minor

**Loom today:** on/off is expressed through active-version presence — a flow with no active version isn't runnable (`Flows().SetActive`).

**The general gap:** some embedders want "keep this configuration but temporarily stop using it and fall back to the default," *without* deleting the active version (which loses the pointer and the intent to restore it).

**Proposed abstraction:** an optional `enabled` boolean on a record; a disabled record resolves as "no active record" (→ the caller's fallback) while retaining its version so it can be re-enabled. Low priority — the active-version model is adequate for most; offered only if the disable-without-delete workflow proves common.

---

## 3. Explicitly *not* a loom change

The pattern "use the registry's stored prompt unless it fails validation, else fall back to a locally-composed default" is **entirely embedder-side**. loom already supports it: pass `SystemPromptOverride` (`api_step.go:119`, `service_step.go:435`) only when you want to override the agent's stored prompt; omit it to use the stored one. The decision — what "fails validation" means, what to fall back to — belongs to the embedder. No loom change is needed; noted here so a reader doesn't go looking for it in loom.

---

## 4. Suggested priority

1. **§1 — latest/active caching + invalidation.** Concrete, self-contained, benefits every embedder, and unblocks running the registry as the per-request source of truth without a DB-read storm. Do this first.
2. **§2.2 — variable registry.** The capability with no loom equivalent and the one most at risk of being lost during a migration; decide its home early even if you build it later.
3. **§2.1 — hierarchical scope.** Emulable client-side; make native only if the extra lookups matter after §1.
4. **§2.4 — soft-disable.** Optional.
