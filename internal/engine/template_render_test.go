package engine

import (
	"strings"
	"testing"
)

func renderTestSession() *Session {
	return &Session{
		PlatformID: "user-1",
		State:      State{Vars: map[string]any{"scene": "a tavern"}},
		Metadata:   map[string]any{"api_key": "SECRET"},
		History:    []Step{{}},
	}
}

func TestRenderTemplate_RejectsTemplateInvocation(t *testing.T) {
	// Self-recursive invocation would otherwise stack-overflow the process.
	body := `{{define "x"}}{{template "x" .}}{{end}}{{template "x" .}}`
	if _, err := renderTemplate(body, renderTestSession(), &Action{}, nil, nil, nil); err == nil {
		t.Fatal("expected nested template invocation to be rejected")
	}

	// Self-reference by the root template name is also blocked.
	if _, err := renderTemplate(`{{template "user" .}}`, renderTestSession(), &Action{}, nil, nil, nil); err == nil {
		t.Fatal("expected root self-invocation to be rejected")
	}
}

func TestRenderTemplate_RejectsOversizeBody(t *testing.T) {
	big := strings.Repeat("a", maxTemplateBodyBytes+1)
	if _, err := renderTemplate(big, renderTestSession(), &Action{}, nil, nil, nil); err == nil {
		t.Fatal("expected oversize template body to be rejected")
	}
}

func TestRenderTemplate_DoesNotExposeMetadataOrHistory(t *testing.T) {
	// Metadata/History are not on the exposed view, so a template referencing
	// them errors — but crucially the secret must never appear in the output.
	// (The field is simply absent from the render context.)
	out, _ := renderTemplate(`[{{.Session.Metadata.api_key}}]`, renderTestSession(), &Action{}, nil, nil, nil)
	if strings.Contains(out, "SECRET") {
		t.Fatalf("template leaked Session.Metadata: %q", out)
	}

	// Safe fields still resolve.
	out, err := renderTemplate(`{{.Vars.scene}}|{{.Session.PlatformID}}`, renderTestSession(), &Action{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if out != "a tavern|user-1" {
		t.Fatalf("unexpected render: %q", out)
	}
}
