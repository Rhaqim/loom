// Package narrative wires the Conexus agents onto loom: it seeds versioned
// prompts from files, builds the per-version agents, defines the turn Flow, and
// parses/validates the agents' output. The agents and prompts are the only thing
// that changes between prompt experiments — bump a version and replay.
package narrative

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	loom "github.com/rhaqim/loom"
)

// Agent slugs. Each is a loom agent backed by a versioned prompt pair.
const (
	AgentAuthor   = "author"
	AgentLogician = "logician"
	AgentSensory  = "sensory"
	AgentTitle    = "title"
	AgentMigrate  = "migrate"
)

// generator slugs the engine registers (text models). Sensory/video would use
// image/video generators in production; here every agent is text-driven.
const (
	GenText = "text"
)

var (
	promptFileRe = regexp.MustCompile(`^([a-z]+)-(sys|user)\.v(\d+)\.txt$`)
	schemaFileRe = regexp.MustCompile(`^([a-z]+)-schema\.v(\d+)\.json$`)
)

// components holds an agent's independently-versioned parts. Each part is a
// reference to a stored record, so composing an agent reuses unchanged parts.
type components struct {
	sys    map[int]uuid.UUID // version -> system-prompt id
	user   map[int]uuid.UUID // version -> user-template id
	schema map[int]uuid.UUID // version -> response-format record id
}

// Seed loads prompt files ("<agent>-<sys|user>.v<N>.txt") and optional response
// format files ("<agent>-schema.v<N>.json") from dir. The system prompt, user
// template, and response format are versioned INDEPENDENTLY; an agent at version
// N is composed from the highest-≤N version of each part. So dropping, say,
// author-sys.v3.txt alone bumps only the system prompt — the user template and
// schema at their existing versions are reused (referenced, not duplicated).
// Returns, per agent, the sorted list of composed versions.
func Seed(ctx context.Context, e *loom.Engine, dir string) (map[string][]int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read prompts dir %q: %w", dir, err)
	}

	comps := map[string]*components{}
	get := func(agent string) *components {
		if comps[agent] == nil {
			comps[agent] = &components{sys: map[int]uuid.UUID{}, user: map[int]uuid.UUID{}, schema: map[int]uuid.UUID{}}
		}
		return comps[agent]
	}

	// Pass 1: prompt files → versioned Prompt records.
	for _, ent := range entries {
		m := promptFileRe.FindStringSubmatch(ent.Name())
		if m == nil {
			continue
		}
		agent, part, verStr := m[1], m[2], m[3]
		ver, _ := strconv.Atoi(verStr)
		body, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			return nil, err
		}
		kind := loom.PromptKindSystem
		if part == "user" {
			kind = loom.PromptKindUserTemplate
		}
		p, err := seedPrompt(ctx, e, fmt.Sprintf("%s-%s", agent, part), ver, kind, string(body))
		if err != nil {
			return nil, err
		}
		c := get(agent)
		if part == "sys" {
			c.sys[ver] = p.ID
		} else {
			c.user[ver] = p.ID
		}
	}

	// Pass 2: schema files → reusable, versioned response-format RECORDS. Each
	// agent references one by ID, so a new system-prompt version reuses the same
	// response format (stored once) instead of copying it.
	for _, ent := range entries {
		m := schemaFileRe.FindStringSubmatch(ent.Name())
		if m == nil {
			continue
		}
		agent, verStr := m[1], m[2]
		ver, _ := strconv.Atoi(verStr)
		raw, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			return nil, err
		}
		id, err := seedResponseFormat(ctx, e, agent, ver, string(raw))
		if err != nil {
			return nil, err
		}
		get(agent).schema[ver] = id
	}

	// Compose one agent version per distinct component version, resolving each
	// part to the highest version ≤ N.
	out := map[string][]int{}
	for agent, c := range comps {
		for _, n := range unionVersions(c.sys, c.user, c.schema) {
			sysID, okS := highestLE(c.sys, n)
			userID, okU := highestLE(c.user, n)
			if !okS || !okU {
				continue // need both halves to form an agent
			}
			rfID, rfInline := composeResponseFormat(agent, c.schema, n)
			if err := seedAgent(ctx, e, agent, n, sysID, userID, rfID, rfInline); err != nil {
				return nil, err
			}
			out[agent] = append(out[agent], n)
		}
		sort.Ints(out[agent])
	}
	return out, nil
}

func seedPrompt(ctx context.Context, e *loom.Engine, slug string, ver int, kind loom.PromptKind, body string) (*loom.Prompt, error) {
	if existing, err := e.Prompts().Get(ctx, "", slug, ver); err == nil && existing.Body == body {
		return existing, nil // unchanged → reuse (lets you re-run the same version)
	}
	p := &loom.Prompt{Slug: slug, Version: ver, Kind: kind, Category: "conexus", Body: body}
	if err := e.Prompts().Create(ctx, p); err != nil {
		return nil, fmt.Errorf("seed prompt %s v%d: %w (bump the version to change a shipped prompt)", slug, ver, err)
	}
	return p, nil
}

// jsonAgents emit JSON. With no schema file they get an inline empty
// response_format (portable "json_object" mode); with a schema file they
// reference a shared, versioned response-format record.
var jsonAgents = map[string]bool{AgentLogician: true, AgentSensory: true, AgentMigrate: true}

// seedResponseFormat creates (idempotently) a reusable response-format record
// "<agent>-schema" at version ver and returns its id.
func seedResponseFormat(ctx context.Context, e *loom.Engine, agent string, ver int, raw string) (uuid.UUID, error) {
	slug := agent + "-schema"
	if existing, err := e.ResponseFormats().Get(ctx, "", slug, ver); err == nil {
		return existing.ID, nil
	}
	rf, err := loom.ResponseFormatJSON(raw, true)
	if err != nil {
		return uuid.Nil, fmt.Errorf("response format %s v%d: %w", slug, ver, err)
	}
	rec := &loom.ResponseFormatRecord{Slug: slug, Version: ver, Schema: rf.Schema, StrictMode: rf.StrictMode}
	if err := e.ResponseFormats().Create(ctx, rec); err != nil {
		return uuid.Nil, err
	}
	return rec.ID, nil
}

// composeResponseFormat resolves an agent's response format at version n: a
// reference to the highest-≤n schema record if any (shared, not copied), else an
// inline json_object for JSON agents, else nothing.
func composeResponseFormat(agent string, schema map[int]uuid.UUID, n int) (*uuid.UUID, *loom.ResponseFormat) {
	if id, ok := highestLE(schema, n); ok {
		return &id, nil
	}
	if jsonAgents[agent] {
		return nil, &loom.ResponseFormat{} // json_object
	}
	return nil, nil
}

// seedAgent creates an agent that composes specific system/user prompt versions
// and a response format — each an independent reference.
func seedAgent(ctx context.Context, e *loom.Engine, slug string, ver int, sysID, userID uuid.UUID, rfID *uuid.UUID, rfInline *loom.ResponseFormat) error {
	if _, err := e.Agents().Get(ctx, "", slug, ver); err == nil {
		return nil
	}
	return e.Agents().Create(ctx, &loom.Agent{
		Slug: slug, Version: ver, Category: "conexus", Modal: loom.ModalityText,
		GeneratorSlug:    GenText,
		SystemPromptID:   sysID,
		UserTemplateID:   userID,
		ResponseFormatID: rfID,
		ResponseFormat:   rfInline,
	})
}

// highestLE returns the value at the highest key ≤ target.
func highestLE[V any](m map[int]V, target int) (V, bool) {
	best := 0
	var bv V
	found := false
	for v, val := range m {
		if v <= target && v > best {
			best, bv, found = v, val, true
		}
	}
	return bv, found
}

// unionVersions returns the sorted set of all versions across the components.
func unionVersions(sys, user, schema map[int]uuid.UUID) []int {
	set := map[int]bool{}
	for v := range sys {
		set[v] = true
	}
	for v := range user {
		set[v] = true
	}
	for v := range schema {
		set[v] = true
	}
	out := make([]int, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

// LatestVersion returns the highest available version for an agent, or 1.
func LatestVersion(versions map[string][]int, agent string) int {
	vs := versions[agent]
	if len(vs) == 0 {
		return 1
	}
	return vs[len(vs)-1]
}

// available is a tiny helper for CLI display.
func Available(versions map[string][]int) string {
	var b strings.Builder
	agents := make([]string, 0, len(versions))
	for a := range versions {
		agents = append(agents, a)
	}
	sort.Strings(agents)
	for _, a := range agents {
		fmt.Fprintf(&b, "%s%v ", a, versions[a])
	}
	return strings.TrimSpace(b.String())
}
