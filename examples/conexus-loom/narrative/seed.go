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

	loom "github.com/rhaqim/loom"
)

// Agent slugs. Each is a loom agent backed by a versioned prompt pair.
const (
	AgentAuthor   = "author"
	AgentLogician = "logician"
	AgentSensory  = "sensory"
	AgentMigrate  = "migrate"
)

// generator slugs the engine registers (text models). Sensory/video would use
// image/video generators in production; here every agent is text-driven.
const (
	GenText = "text"
)

var promptFileRe = regexp.MustCompile(`^([a-z]+)-(sys|user)\.v(\d+)\.txt$`)

// Seed loads every prompt file in dir (named "<agent>-<sys|user>.v<N>.txt") into
// loom as a versioned Prompt, then creates one agent version per (agent, N) that
// has both a sys and user prompt. It returns, per agent slug, the sorted list of
// available versions so the CLI can offer them.
func Seed(ctx context.Context, e *loom.Engine, dir string) (map[string][]int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read prompts dir %q: %w", dir, err)
	}

	// agent -> version -> kind -> promptID
	type key struct {
		agent string
		ver   int
	}
	pids := map[key]map[loom.PromptKind]string{}

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
		// A response format lives WITH its system prompt: drop a sibling
		// "<agent>-schema.v<N>.json" file and it is attached here and stored in
		// the DB alongside the prompt (loom applies it when the agent runs).
		var rf *loom.ResponseFormat
		if kind == loom.PromptKindSystem {
			rf = loadResponseFormat(dir, agent, ver)
		}
		slug := fmt.Sprintf("%s-%s", agent, part)
		p, err := seedPrompt(ctx, e, slug, ver, kind, string(body), rf)
		if err != nil {
			return nil, err
		}
		k := key{agent, ver}
		if pids[k] == nil {
			pids[k] = map[loom.PromptKind]string{}
		}
		pids[k][kind] = p.ID.String()
	}

	versions := map[string]map[int]bool{}
	for k, kinds := range pids {
		if kinds[loom.PromptKindSystem] == "" || kinds[loom.PromptKindUserTemplate] == "" {
			continue // need both halves to form an agent
		}
		if err := seedAgent(ctx, e, k.agent, k.ver, loom.ModalityText, GenText); err != nil {
			return nil, err
		}
		if versions[k.agent] == nil {
			versions[k.agent] = map[int]bool{}
		}
		versions[k.agent][k.ver] = true
	}

	out := map[string][]int{}
	for agent, vs := range versions {
		for v := range vs {
			out[agent] = append(out[agent], v)
		}
		sort.Ints(out[agent])
	}
	return out, nil
}

func seedPrompt(ctx context.Context, e *loom.Engine, slug string, ver int, kind loom.PromptKind, body string, rf *loom.ResponseFormat) (*loom.Prompt, error) {
	if existing, err := e.Prompts().Get(ctx, slug, ver); err == nil {
		// Reseed if the body changed (lets you edit a file and re-run the same
		// version during iteration without manual cleanup).
		if existing.Body == body {
			return existing, nil
		}
	}
	p := &loom.Prompt{Slug: slug, Version: ver, Kind: kind, Category: "conexus", Body: body, ResponseFormat: rf}
	if err := e.Prompts().Create(ctx, p); err != nil {
		// Already exists with different body but same version: surface clearly.
		return nil, fmt.Errorf("seed prompt %s v%d: %w (bump the version to change a shipped prompt)", slug, ver, err)
	}
	return p, nil
}

// jsonAgents emit JSON. With no schema file they get an empty response_format
// (portable "json_object" mode); with a schema file they get strict json_schema.
var jsonAgents = map[string]bool{AgentLogician: true, AgentSensory: true, AgentMigrate: true}

// loadResponseFormat returns the response format for an agent's system prompt:
// the schema in "<agent>-schema.v<N>.json" if present, else json_object mode for
// JSON agents, else nil (free-form, e.g. the Author).
func loadResponseFormat(dir, agent string, ver int) *loom.ResponseFormat {
	path := filepath.Join(dir, fmt.Sprintf("%s-schema.v%d.json", agent, ver))
	if b, err := os.ReadFile(path); err == nil {
		if rf, err := loom.ResponseFormatJSON(string(b), true); err == nil {
			return rf
		}
	}
	if jsonAgents[agent] {
		return &loom.ResponseFormat{} // json_object
	}
	return nil
}

func seedAgent(ctx context.Context, e *loom.Engine, slug string, ver int, modal loom.Modality, gen string) error {
	if _, err := e.Agents().Get(ctx, slug, ver); err == nil {
		return nil
	}
	sys, err := e.Prompts().Get(ctx, prefixSlug(slug, "sys"), ver)
	if err != nil {
		return err
	}
	user, err := e.Prompts().Get(ctx, prefixSlug(slug, "user"), ver)
	if err != nil {
		return err
	}
	// The agent leaves ResponseFormat nil; loom falls back to the one stored on
	// the system prompt (seeded above), so the schema travels with the prompt.
	return e.Agents().Create(ctx, &loom.Agent{
		Slug: slug, Version: ver, Category: "conexus", Modal: modal,
		GeneratorSlug:  gen,
		SystemPromptID: sys.ID,
		UserTemplateID: user.ID,
	})
}

func prefixSlug(agent, part string) string { return agent + "-" + part }

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
