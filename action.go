package loom

import (
	"context"

	"github.com/google/uuid"
)

// ActionKind discriminates the shape of an Action's payload.
type ActionKind string

const (
	ActionSelect    ActionKind = "select"
	ActionFreeText  ActionKind = "free_text"
	ActionSpatial   ActionKind = "spatial"
	ActionInventory ActionKind = "inventory"
	ActionGesture   ActionKind = "gesture"
	ActionCustom    ActionKind = "custom"
)

// Action is the discriminator's structured human-guidance input for a step.
type Action struct {
	ID       uuid.UUID
	Kind     ActionKind
	Payload  any // typed per Kind: Selection, FreeText, Spatial, etc.
	Metadata map[string]any
}

// Selection is the payload for ActionSelect — choosing one of N presented options.
type Selection struct {
	OptionIndex int
	OptionID    string
}

// FreeText is the payload for ActionFreeText — a user-typed string.
type FreeText struct {
	Body string
}

// Spatial is the payload for ActionSpatial — a direction or coordinate input.
type Spatial struct {
	Direction  string     // "forward", "north", "left", or custom
	Coordinate [3]float64 // x, y, z for 3-D worlds; ignored otherwise
}

// InventoryChoice is the payload for ActionInventory — selecting an item.
type InventoryChoice struct {
	ItemID   string
	ItemName string
}

// Gesture is the payload for ActionGesture — pointer or touch input.
type Gesture struct {
	Kind string  // "click", "tap", "drag"
	X    float64 // start or absolute x
	Y    float64 // start or absolute y
	DX   float64 // delta x (drag only)
	DY   float64 // delta y (drag only)
}

// ActionTemplate describes an action the engine offers back to the platform.
type ActionTemplate struct {
	ID          uuid.UUID
	Kind        ActionKind
	Label       string // player-facing wording
	Detail      string // longer description
	Payload     any    // pre-filled fields
	Constraints any    // e.g. "must be a direction in {N, E, S, W}"
	Ordering    int
}

// RawInput is opaque user input the platform passes to an ActionInterpreter.
type RawInput struct {
	Kind    string
	Payload map[string]any
}

// ActionInterpreter converts raw platform input to a structured Action.
// The platform supplies this; the engine uses it internally during RunStep.
type ActionInterpreter interface {
	Interpret(ctx context.Context, raw RawInput, session *Session) (Action, error)
}
