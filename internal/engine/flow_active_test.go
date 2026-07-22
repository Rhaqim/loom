package engine

// flow_active_test.go — RunTurnBySlug must refuse a flow version whose IsActive
// flag is false. Before this, is_active was stored but never consulted, so a
// retired flow still resolved and executed.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestRunTurnBySlugRespectsIsActive(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "flow_active", map[string]Generator{"g": okGen{}}, PollerConfig{})
	if err := e.Agents().Create(ctx, &Agent{ID: uuid.New(), Slug: "a1", Version: 1, Modal: ModalityText, GeneratorSlug: "g"}); err != nil {
		t.Fatal(err)
	}
	mkFlow := func(slug string, active bool) {
		if err := e.Flows().Create(ctx, &FlowRecord{
			Slug: slug, Version: 1, Category: "t", IsActive: active,
			Agents: []FlowAgentEntry{{AgentSlug: "a1", OutputKey: "Lead"}},
		}); err != nil {
			t.Fatalf("create flow %s: %v", slug, err)
		}
	}
	mkFlow("f-active", true)
	mkFlow("f-inactive", false)

	sess := &Session{PlatformID: "p", State: State{Modality: ModalityText}}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	if _, err := e.RunTurnBySlug(ctx, sess, "", "f-active", 0, TurnRequest{}); err != nil {
		t.Errorf("active flow should run, got err: %v", err)
	}

	// Version 0 resolves the latest ACTIVE version, so a slug whose only
	// version is inactive has nothing serving — that is ErrNotFound rather than
	// "not active", since no particular version was refused.
	_, err := e.RunTurnBySlug(ctx, sess, "", "f-inactive", 0, TurnRequest{})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("inactive flow via version 0 should be not-found, got err: %v", err)
	}

	// Pinning the inactive version explicitly still reports it as inactive:
	// here a specific version WAS named and refused.
	_, err = e.RunTurnBySlug(ctx, sess, "", "f-inactive", 1, TurnRequest{})
	if err == nil || !strings.Contains(err.Error(), "not active") {
		t.Errorf("pinned inactive flow should be refused as inactive, got err: %v", err)
	}
}
