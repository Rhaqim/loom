# Security Audit — Tracking Issue

Security review of the `loom` library and example apps. Findings are grouped by
severity. Most library findings are **conditional** on the trust model: harmless
in strictly first-party use, but High/Critical once an embedder wires *untrusted*
input (multi-tenant prompts, client-supplied overrides, hosted test-plan
execution) into the affected surface. `flow_store.go` scopes records by tenant
`owner`, so multi-tenant use is an intended scenario.

Status legend: ⬜ open · ✅ fixed · 🔷 won't-fix / accepted risk

---

## Critical

- ✅ **C1 — storyapi ships a usable default signing secret → full auth bypass.**
  `examples/storyapi/main.go` fell back to the hardcoded `change-me-in-production`
  HMAC secret and only logged a warning. Deployed without `TOKEN_SECRET`, the key
  is public (in source) and any user's token can be forged. **Fix:** refuse to
  boot when the secret is empty/default or shorter than 32 bytes.

## High

- ✅ **H1 — Generator `base_url` override → SSRF + API-key exfiltration.**
  `generator/openai` and `generator/anthropic` applied a per-request
  `Overrides["base_url"]` while still attaching the server's real API key.
  **Fix:** ignore `base_url` unless the generator opts in via an explicit
  host allowlist; require `https`; reject on mismatch.

- ✅ **H2 — Arbitrary local file read via `PromptRef.File` (LFI → exfiltration).**
  `engine.go resolveSystemPrompt` did `os.ReadFile(ref.File)` with no
  confinement; contents become the system prompt (sent to the LLM, persisted).
  **Fix:** file loading is disabled by default and gated behind
  `Config.PromptFileRoot`; paths are confined to that root (clean + prefix +
  symlink-resolved).

- ✅ **H3 — Whole `Session` exposed to the user template.** `renderTemplate`
  handed the raw `*Session` (incl. `Metadata`, `PlatformID`, full `History`) to
  the template, enabling `{{.Session.Metadata.api_key}}`-style exfiltration from
  a tenant-authored template. **Fix:** expose a minimal whitelisted view; drop
  `Metadata`/`PlatformID`/raw session from the render context.

- ✅ **H4 — storyapi: no HTTP server timeouts (Slowloris).**
  `http.ListenAndServe` used a zero-value server. **Fix:** explicit
  `http.Server` with `ReadHeaderTimeout`/`ReadTimeout`/`IdleTimeout`.

- ✅ **H5 — storyapi: unbounded request body + no auth rate limiting.**
  `decodeBody` had no `MaxBytesReader`; `/login` & `/register` had no throttle.
  **Fix:** `MaxBytesReader` + field caps in `decodeBody`; per-IP rate limiter on
  auth routes.

## Medium

- ✅ **M1 — GC over-broad hard-delete (unconditional data-loss bug).**
  `gc/worker.go` matched tier-4 hard delete with `tags LIKE '%test:true%'`, a
  substring match that also matches `latest:true`, `contest:true`, etc. **Fix:**
  JSON array-membership match (`@>` on Postgres, `json_each` on SQLite).

- ✅ **M2 — Template DoS.** `text/template` recursion (`{{define}}`/`{{template}}`)
  causes an unrecoverable stack overflow; the lead render panic wasn't recovered;
  no output-size cap. **Fix:** `recover` around render, output-size limit, body
  size cap.

- ✅ **M3 — Hook-bus data race.** `hook.go` had no mutex; registering hooks while
  steps run races the slice. **Fix:** `sync.RWMutex` guarding register/run.

- ⬜ **M4 — Harness unbounded fan-out.** `harness.Run` launches one goroutine per
  expanded variant with no semaphore. **Fix:** bound concurrency, cap variants.

- ⬜ **M5 — Generator `http://` base_url + unbounded response reads.** (Partly
  covered by H1's https enforcement.) Add `io.LimitReader` on response bodies.

- ⬜ **M6 — storyapi info leaks / session hygiene.** Raw `err.Error()` to clients;
  user enumeration (register-409 + login timing); no token revocation/logout.

- ⬜ **M7 — DB creds in `--dsn` argv** (visible via `ps`). Prefer env / `--dsn-file`.

## Low / hardening

- ⬜ Unvalidated table `prefix` interpolated into DDL/SQL (SQLi *if* prefix ever
  untrusted) — add `^[A-Za-z0-9_]+$` guard.
- ⬜ Budget enforcement fail-open + TOCTOU (cost overshoot under concurrency).
- ⬜ Wildcard CORS; missing security headers; default DSN with embedded creds;
  no max password length (bcrypt truncates at 72 bytes); unbounded topic
  creation; YAML loaded with no size cap; unescaped async task handle in poll
  URL; upstream error bodies echoed into errors.

## Verified clean

- No SQL injection from request data (all `$N` bind params).
- Object-level authz on stories is correct (no IDOR).
- bcrypt passwords; HMAC tokens with constant-time compare, verify-before-decode;
  UUIDv4 IDs; `PasswordHash` is `json:"-"`.
- `https` generator defaults, client timeouts set, no `InsecureSkipVerify`.
- Safe result deserialization (comma-ok assertions).
