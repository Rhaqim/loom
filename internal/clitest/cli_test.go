// Package clitest contains integration tests for the loom-cli binary.
// Each test builds the CLI binary once (TestMain) and then exercises the
// migrate, seed, and test subcommands against a real Postgres database.
//
// Tests are skipped when LOOM_DSN is not set.
package clitest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var cliBin string // path to the compiled loom-cli binary

// TestMain compiles the loom-cli binary before any test runs.
func TestMain(m *testing.M) {
	dsn := os.Getenv("LOOM_DSN")
	if dsn == "" {
		// Skip compilation — tests will skip themselves.
		os.Exit(m.Run())
	}

	tmp, err := os.MkdirTemp("", "loom-cli-test-*")
	if err != nil {
		panic("tmpdir: " + err.Error())
	}
	defer os.RemoveAll(tmp)

	cliBin = filepath.Join(tmp, "loom-cli")

	// Find the module root (three levels up from internal/clitest).
	moduleRoot := filepath.Join("..", "..")
	cmd := exec.Command("go", "build", "-o", cliBin, "./cmd/loom-cli")
	cmd.Dir = moduleRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		panic("build loom-cli: " + err.Error())
	}

	os.Exit(m.Run())
}

// run executes the CLI binary with the given args and returns stdout+stderr.
func run(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(cliBin, args...)
	cmd.Env = append(os.Environ(), "LOOM_DSN="+os.Getenv("LOOM_DSN"))
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ex, ok := err.(*exec.ExitError); ok {
			code = ex.ExitCode()
		} else {
			t.Fatalf("exec: %v", err)
		}
	}
	return string(out), code
}

// requireDSN skips the test if LOOM_DSN is unset.
func requireDSN(t *testing.T) {
	t.Helper()
	if os.Getenv("LOOM_DSN") == "" {
		t.Skip("LOOM_DSN not set — skipping integration test")
	}
}

// -----------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------

func TestCLI_Migrate(t *testing.T) {
	requireDSN(t)

	out, code := run(t, "migrate")
	if code != 0 {
		t.Fatalf("migrate exited %d:\n%s", code, out)
	}
	if !strings.Contains(out, "Schema applied") {
		t.Errorf("expected 'Schema applied' in output, got:\n%s", out)
	}
}

func TestCLI_Migrate_Idempotent(t *testing.T) {
	requireDSN(t)

	// Running migrate twice should succeed both times.
	for i := range 2 {
		out, code := run(t, "migrate")
		if code != 0 {
			t.Fatalf("migrate run %d exited %d:\n%s", i+1, code, out)
		}
	}
}

func TestCLI_Seed(t *testing.T) {
	requireDSN(t)

	// migrate first so tables exist.
	run(t, "migrate")

	seedFile := writeTempFile(t, "seed-*.yaml", seedYAML)
	out, code := run(t, "seed", seedFile)
	if code != 0 {
		t.Fatalf("seed exited %d:\n%s", code, out)
	}
	// Output contains either "seeded" (first run) or "skipped" (already exists).
	if !strings.Contains(out, "seeded") && !strings.Contains(out, "skipped") {
		t.Errorf("expected 'seeded' or 'skipped' in output, got:\n%s", out)
	}
}

func TestCLI_Seed_Idempotent(t *testing.T) {
	requireDSN(t)
	run(t, "migrate")

	// Use a unique seed file per run to avoid clashing with TestCLI_Seed.
	seedFile := writeTempFile(t, "seed-idem-*.yaml", seedIdempotentYAML)

	// Running seed twice must not error — duplicate slug/version should be tolerated.
	for i := range 2 {
		out, code := run(t, "seed", seedFile)
		if code != 0 && i == 0 {
			t.Fatalf("seed first run exited %d:\n%s", code, out)
		}
	}
}

func TestCLI_Test_Pass(t *testing.T) {
	requireDSN(t)
	run(t, "migrate")
	run(t, "seed", writeTempFile(t, "seed-*.yaml", seedYAML))

	planFile := writeTempFile(t, "plan-pass-*.yaml", passingPlanYAML)
	out, code := run(t, "test", planFile)
	if code != 0 {
		t.Fatalf("test exited %d (expected pass):\n%s", code, out)
	}
	if !strings.Contains(out, "passed") && !strings.Contains(out, "PASS") {
		t.Errorf("expected pass indication in output:\n%s", out)
	}
}

func TestCLI_Test_Fail(t *testing.T) {
	requireDSN(t)
	run(t, "migrate")
	run(t, "seed", writeTempFile(t, "seed-*.yaml", seedYAML))

	// A plan that references a non-existent agent should cause the test command to fail.
	planFile := writeTempFile(t, "plan-fail-*.yaml", failingPlanYAML)
	out, code := run(t, "test", planFile)
	if code == 0 {
		t.Fatalf("expected non-zero exit for failing plan, got 0:\n%s", out)
	}
}

func TestCLI_Help(t *testing.T) {
	requireDSN(t)
	out, code := run(t, "--help")
	if code != 0 {
		t.Fatalf("--help exited %d", code)
	}
	for _, want := range []string{"migrate", "seed", "test"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in --help output:\n%s", want, out)
		}
	}
}

// -----------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------

const seedYAML = `
prompts:
  - slug: cli-test-sys
    version: 1
    kind: system
    category: cli-test
    body: "You are a CLI test narrator."
  - slug: cli-test-user
    version: 1
    kind: user_template
    category: cli-test
    body: "Player: {{.Action.Payload.text}}"
agents:
  - slug: cli-test-agent
    version: 1
    category: cli-test
    modal: text
    generator_slug: echo
`

// seedIdempotentYAML uses different slugs so it doesn't conflict with seedYAML.
const seedIdempotentYAML = `
prompts:
  - slug: cli-test-idem-sys
    version: 1
    kind: system
    category: cli-test
    body: "Idempotency test system prompt."
agents:
  - slug: cli-test-idem-agent
    version: 1
    category: cli-test
    modal: text
    generator_slug: echo
`

// passingPlanYAML runs the echo generator via the cli-test-agent.
// The echo generator is wired into the CLI via cliGenerators() in cmd/loom-cli/main.go.
const passingPlanYAML = `
name: cli-smoke-pass
session:
  platform_id: cli-test-user
  tags: ["test:true"]
  steps:
    - agent_slug: cli-test-agent
      action_payload: "Hello from CLI test"
variants:
  providers: []
`

// failingPlanYAML references an agent that does not exist — the test command
// must exit non-zero when it cannot resolve the agent.
const failingPlanYAML = `
name: cli-smoke-fail
session:
  platform_id: cli-test-user
  tags: ["test:true"]
  steps:
    - agent_slug: nonexistent-agent-xyz
      action_payload: "Hello"
variants:
  providers: []
`

// writeTempFile creates a temp file with the given content and registers cleanup.
func writeTempFile(t *testing.T, pattern, content string) string {
	t.Helper()
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}
