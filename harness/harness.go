// Package harness provides a test harness for Loom procgen pipelines.
//
// A TestPlan defines a scripted session, a VariantMatrix of providers/prompts/
// parameters to cross-product, and a set of Assertions that are evaluated
// against every generated Result. The harness runs all variants in parallel and
// returns a TestReport with per-variant pass/fail outcomes.
//
// Example:
//
//	plan := &harness.TestPlan{
//	    Name: "chat-v1",
//	    Session: harness.SessionScript{
//	        PlatformID: "test-user",
//	        Steps: []harness.ScriptedStep{
//	            {AgentSlug: "assistant", ActionPayload: "Begin"},
//	        },
//	    },
//	    Variants: harness.VariantMatrix{
//	        Providers: []string{"openai-gpt4o", "anthropic-claude"},
//	    },
//	    Assertions: []harness.Assertion{
//	        harness.MinLength(100),
//	        harness.NoKeyword("ERROR"),
//	    },
//	}
//	report, err := harness.Run(ctx, engine, plan)
package harness

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	loom "github.com/rhaqim/loom"
)

// -----------------------------------------------------------------------
// Plan types
// -----------------------------------------------------------------------

// TestPlan describes a complete test scenario.
type TestPlan struct {
	// Name is a human-readable identifier for the plan.
	Name string `yaml:"name" json:"name"`
	// Session defines the scripted session to run for each variant.
	Session SessionScript `yaml:"session" json:"session"`
	// Variants is the cross-product of providers/prompts/params to test.
	Variants VariantMatrix `yaml:"variants" json:"variants"`
	// Assertions are checked against every step Result.
	Assertions []Assertion `yaml:"-" json:"-"`
}

// SessionScript describes the session lifecycle for a test.
type SessionScript struct {
	// PlatformID is the user/tenant identifier.
	PlatformID string `yaml:"platform_id" json:"platform_id"`
	// Tags are applied to the test session so it can be GC'd by the test-branch rule.
	Tags []string `yaml:"tags" json:"tags"`
	// Steps are the ordered generation steps to run.
	Steps []ScriptedStep `yaml:"steps" json:"steps"`
}

// ScriptedStep is a single step in the test script.
type ScriptedStep struct {
	// AgentSlug is the agent to invoke.
	AgentSlug string `yaml:"agent_slug" json:"agent_slug"`
	// AgentVersion pins a specific agent version (0 = latest).
	AgentVersion int `yaml:"agent_version,omitempty" json:"agent_version,omitempty"`
	// ActionPayload is serialised as a free-text action if non-empty.
	ActionPayload string `yaml:"action_payload,omitempty" json:"action_payload,omitempty"`
}

// VariantMatrix is the cross-product of configuration axes to test.
// Each combination of (Provider × SystemPrompt × Param) produces one variant.
type VariantMatrix struct {
	// Providers are generator slugs to substitute into agent.GeneratorSlug.
	// Empty means "use the agent's default generator".
	Providers []string `yaml:"providers" json:"providers"`
	// SystemPrompts override the agent's system prompt per variant (optional).
	SystemPrompts []loom.PromptRef `yaml:"system_prompts" json:"system_prompts"`
	// Params overrides the agent's GenerateParams per variant (optional).
	Params []loom.GenerateParams `yaml:"params" json:"params"`
}

// -----------------------------------------------------------------------
// Assertions
// -----------------------------------------------------------------------

// Assertion is a function that checks a Result and returns an error if it fails.
type Assertion func(step *loom.Step, result loom.Result) error

// MinLength asserts the text content of a TextResult has at least n characters.
func MinLength(n int) Assertion {
	return func(_ *loom.Step, result loom.Result) error {
		tr, ok := result.(*loom.TextResult)
		if !ok {
			return nil // only applicable to text
		}
		if len(tr.Content) < n {
			return fmt.Errorf("min_length: got %d chars, want >= %d", len(tr.Content), n)
		}
		return nil
	}
}

// MaxLength asserts the text content of a TextResult has at most n characters.
func MaxLength(n int) Assertion {
	return func(_ *loom.Step, result loom.Result) error {
		tr, ok := result.(*loom.TextResult)
		if !ok {
			return nil
		}
		if len(tr.Content) > n {
			return fmt.Errorf("max_length: got %d chars, want <= %d", len(tr.Content), n)
		}
		return nil
	}
}

// NoKeyword fails if the text content contains any of the given keywords (case-insensitive).
func NoKeyword(keywords ...string) Assertion {
	return func(_ *loom.Step, result loom.Result) error {
		tr, ok := result.(*loom.TextResult)
		if !ok {
			return nil
		}
		lower := strings.ToLower(tr.Content)
		for _, kw := range keywords {
			if strings.Contains(lower, strings.ToLower(kw)) {
				return fmt.Errorf("no_keyword: found prohibited keyword %q", kw)
			}
		}
		return nil
	}
}

// HasStatus asserts the result has the expected status.
func HasStatus(want loom.ResultStatus) Assertion {
	return func(_ *loom.Step, result loom.Result) error {
		if result.Status() != want {
			return fmt.Errorf("has_status: got %q, want %q", result.Status(), want)
		}
		return nil
	}
}

// Custom wraps a user-defined assertion function.
func Custom(name string, fn func(step *loom.Step, result loom.Result) error) Assertion {
	return func(step *loom.Step, result loom.Result) error {
		if err := fn(step, result); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	}
}

// -----------------------------------------------------------------------
// Report types
// -----------------------------------------------------------------------

// TestReport contains the aggregate outcome of a test run.
type TestReport struct {
	PlanName   string
	StartedAt  time.Time
	FinishedAt time.Time
	Variants   []*VariantReport
}

// Passed returns true if all variants passed.
func (r *TestReport) Passed() bool {
	for _, v := range r.Variants {
		if !v.Passed {
			return false
		}
	}
	return true
}

// VariantReport holds the outcome for a single cross-product variant.
type VariantReport struct {
	Index    int
	Provider string
	Passed   bool
	Steps    []*StepReport
	Duration time.Duration
	Err      error
}

// StepReport holds assertion results for a single step within a variant.
type StepReport struct {
	StepIndex  int
	AgentSlug  string
	ResultID   string
	Failures   []string
	DurationMs int
}

// -----------------------------------------------------------------------
// Runner
// -----------------------------------------------------------------------

// Run executes the TestPlan against all variants and returns a TestReport.
// Each variant runs its own ephemeral session.
func Run(ctx context.Context, e *loom.Engine, plan *TestPlan) (*TestReport, error) {
	report := &TestReport{
		PlanName:  plan.Name,
		StartedAt: time.Now(),
	}

	variants := expandVariants(plan.Variants)
	if len(variants) == 0 {
		variants = []variantConfig{{}} // single default variant
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	variantReports := make([]*VariantReport, len(variants))

	for i, v := range variants {
		wg.Add(1)
		go func(idx int, vc variantConfig) {
			defer wg.Done()
			vr := runVariant(ctx, e, plan, vc, idx)
			mu.Lock()
			variantReports[idx] = vr
			mu.Unlock()
		}(i, v)
	}

	wg.Wait()
	report.Variants = variantReports
	report.FinishedAt = time.Now()
	return report, nil
}

// variantConfig is one expansion of the VariantMatrix.
type variantConfig struct {
	Provider     string
	SystemPrompt *loom.PromptRef
	Params       *loom.GenerateParams
}

func expandVariants(m VariantMatrix) []variantConfig {
	var out []variantConfig
	providers := m.Providers
	if len(providers) == 0 {
		providers = []string{""}
	}
	prompts := m.SystemPrompts
	if len(prompts) == 0 {
		prompts = []loom.PromptRef{{}} // one no-op prompt slot
	}
	params := m.Params
	if len(params) == 0 {
		params = []loom.GenerateParams{{}} // one no-op param slot
	}
	for _, p := range providers {
		for pi := range prompts {
			for qi := range params {
				vc := variantConfig{Provider: p}
				if prompts[pi].Slug != "" || prompts[pi].File != "" {
					vc.SystemPrompt = &prompts[pi]
				}
				// Only override params when the Params axis was actually given;
				// otherwise leave nil so the agent's own params stand (an empty
				// GenerateParams override would zero temperature/max_tokens/etc.).
				if len(m.Params) > 0 {
					vc.Params = &params[qi]
				}
				out = append(out, vc)
			}
		}
	}
	return out
}

func runVariant(ctx context.Context, e *loom.Engine, plan *TestPlan, vc variantConfig, idx int) *VariantReport {
	start := time.Now()
	vr := &VariantReport{Index: idx, Provider: vc.Provider}

	// Create a test session tagged so GC can clean it up.
	tags := append([]string{"test:true"}, plan.Session.Tags...)
	sess := &loom.Session{
		PlatformID: plan.Session.PlatformID,
		Tags:       tags,
		State: loom.State{
			Modality: loom.ModalityText,
			Vars:     map[string]any{},
		},
	}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		vr.Err = fmt.Errorf("create session: %w", err)
		vr.Duration = time.Since(start)
		return vr
	}

	passed := true
	for si, scriptStep := range plan.Session.Steps {
		stepStart := time.Now()
		req := loom.StepRequest{
			AgentSlug:    scriptStep.AgentSlug,
			AgentVersion: scriptStep.AgentVersion,
		}
		// Apply the variant's overrides so each cross-product cell actually runs
		// a different configuration. Without this the matrix was cosmetic — every
		// variant ran the identical agent under a different label.
		if vc.Provider != "" {
			req.GeneratorOverride = vc.Provider
		}
		if vc.Params != nil {
			req.ParamOverride = vc.Params
		}
		if vc.SystemPrompt != nil {
			req.SystemPromptOverride = vc.SystemPrompt
		}
		if scriptStep.ActionPayload != "" {
			req.Action = &loom.Action{
				Kind:    loom.ActionFreeText,
				Payload: map[string]any{"text": scriptStep.ActionPayload},
			}
		}

		step, err := e.RunStep(ctx, sess, req)
		sr := &StepReport{
			StepIndex:  si,
			AgentSlug:  scriptStep.AgentSlug,
			DurationMs: int(time.Since(stepStart).Milliseconds()),
		}
		if err != nil {
			sr.Failures = append(sr.Failures, fmt.Sprintf("run_step: %v", err))
			passed = false
			vr.Steps = append(vr.Steps, sr)
			continue
		}

		sr.ResultID = step.Result.ResultID().String()
		// Run all assertions.
		for _, assert := range plan.Assertions {
			if aerr := assert(step, step.Result); aerr != nil {
				sr.Failures = append(sr.Failures, aerr.Error())
				passed = false
			}
		}
		vr.Steps = append(vr.Steps, sr)
	}

	vr.Passed = passed
	vr.Duration = time.Since(start)
	return vr
}
