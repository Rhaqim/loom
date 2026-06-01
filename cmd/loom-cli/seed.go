package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	loom "github.com/rhaqim/loom"
	"github.com/rhaqim/loom/harness"
)

// -----------------------------------------------------------------------
// Seed file format
// -----------------------------------------------------------------------

type seedFile struct {
	Prompts []seedPrompt `yaml:"prompts"`
	Agents  []seedAgent  `yaml:"agents"`
}

type seedPrompt struct {
	Slug     string `yaml:"slug"`
	Version  int    `yaml:"version"`
	Kind     string `yaml:"kind"`
	Category string `yaml:"category"`
	Body     string `yaml:"body"`
	Notes    string `yaml:"notes"`
}

type seedAgent struct {
	Slug          string `yaml:"slug"`
	Version       int    `yaml:"version"`
	Category      string `yaml:"category"`
	Modal         string `yaml:"modal"`
	GeneratorSlug string `yaml:"generator_slug"`
	SystemPrompt  string `yaml:"system_prompt_slug"` // slug reference
	UserTemplate  string `yaml:"user_template_slug"` // slug reference
}

func loadSeedFile(path string) (*seedFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read seed file: %w", err)
	}
	var sf seedFile
	if err := yaml.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("parse seed file: %w", err)
	}
	return &sf, nil
}

func runSeed(ctx context.Context, e *loom.Engine, sf *seedFile) error {
	for _, sp := range sf.Prompts {
		p := &loom.Prompt{
			Slug:     sp.Slug,
			Version:  sp.Version,
			Kind:     loom.PromptKind(sp.Kind),
			Category: sp.Category,
			Body:     sp.Body,
			Notes:    sp.Notes,
		}
		if err := e.Prompts().Create(ctx, p); err != nil {
			if isDuplicateErr(err) {
				fmt.Printf("  skipped prompt %s@%d (already exists)\n", sp.Slug, sp.Version)
				continue
			}
			return fmt.Errorf("seed prompt %q: %w", sp.Slug, err)
		}
		fmt.Printf("  seeded prompt %s@%d\n", sp.Slug, sp.Version)
	}
	for _, sa := range sf.Agents {
		a := &loom.Agent{
			Slug:          sa.Slug,
			Version:       sa.Version,
			Category:      sa.Category,
			Modal:         loom.Modality(sa.Modal),
			GeneratorSlug: sa.GeneratorSlug,
		}
		if err := e.Agents().Create(ctx, a); err != nil {
			if isDuplicateErr(err) {
				fmt.Printf("  skipped agent %s@%d (already exists)\n", sa.Slug, sa.Version)
				continue
			}
			return fmt.Errorf("seed agent %q: %w", sa.Slug, err)
		}
		fmt.Printf("  seeded agent %s@%d\n", sa.Slug, sa.Version)
	}
	return nil
}

// isDuplicateErr returns true for PostgreSQL unique-constraint violation errors.
func isDuplicateErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "duplicate key") || strings.Contains(msg, "UNIQUE constraint failed")
}

// -----------------------------------------------------------------------
// Test plan loading
// -----------------------------------------------------------------------

type yamlTestPlan struct {
	Name     string                `yaml:"name"`
	Session  harness.SessionScript `yaml:"session"`
	Variants struct {
		Providers     []string         `yaml:"providers"`
		SystemPrompts []loom.PromptRef `yaml:"system_prompts"`
	} `yaml:"variants"`
}

func loadTestPlan(path string) (*harness.TestPlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read test plan: %w", err)
	}
	var yp yamlTestPlan
	if err := yaml.Unmarshal(data, &yp); err != nil {
		return nil, fmt.Errorf("parse test plan: %w", err)
	}
	plan := &harness.TestPlan{
		Name:    yp.Name,
		Session: yp.Session,
		Variants: harness.VariantMatrix{
			Providers:     yp.Variants.Providers,
			SystemPrompts: yp.Variants.SystemPrompts,
		},
		// Built-in assertion: all results must be ready.
		Assertions: []harness.Assertion{
			harness.HasStatus(loom.ResultStatusReady),
		},
	}
	return plan, nil
}
