// Package migration converts a creator's block prompt (block.txt) into a
// structured World + Constitution, using the migrate agent through loom. It
// mirrors the Conexus topic→world pipeline.
package migration

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/rhaqim/conexus-loom/domain"
	"github.com/rhaqim/conexus-loom/narrative"
	"github.com/rhaqim/conexus-loom/ports"
	loom "github.com/rhaqim/loom"
)

// Service runs the migrate agent.
type Service struct {
	Loom    *loom.Engine
	Version int // migrate prompt/agent version
	Log     ports.Logger
}

// Migrate converts blockText into a World + Constitution.
func (s *Service) Migrate(ctx context.Context, blockText string) (domain.World, domain.Constitution, error) {
	// A short-lived session so the one-shot migration step has somewhere to live
	// (its cost is still tracked by loom under the "migration" platform id).
	sess := &loom.Session{PlatformID: "migration", State: loom.State{Modality: loom.ModalityText}}
	if err := s.Loom.Sessions().Create(ctx, sess); err != nil {
		return domain.World{}, domain.Constitution{}, err
	}
	step, err := s.Loom.RunStep(ctx, sess, loom.StepRequest{
		AgentSlug:    narrative.AgentMigrate,
		AgentVersion: s.Version,
		Inputs:       map[string]any{"block": blockText},
	})
	if err != nil {
		return domain.World{}, domain.Constitution{}, fmt.Errorf("migrate step: %w", err)
	}
	res, err := narrative.ParseMigration(narrative.JSONOf(step.Result))
	if err != nil {
		return domain.World{}, domain.Constitution{}, fmt.Errorf("parse migration output: %w", err)
	}
	if res.World.ID == uuid.Nil {
		res.World.ID = uuid.New()
	}
	if s.Log != nil {
		s.Log.Info("migrated block to world", "name", res.World.Name, "characters", len(res.World.Characters))
	}
	return res.World, res.Constitution, nil
}
