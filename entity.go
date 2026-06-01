package loom

import (
	"github.com/google/uuid"
)

// EntityAnnotation marks a named entity reference in generated prose.
type EntityAnnotation struct {
	EntityID   string // platform-supplied ID (character_id, location_id, …)
	EntityKind string // "character", "location", "time", "item", or custom
	Surface    string // the substring as it appears in the prose
	StartByte  int    // byte offset in the result content
	EndByte    int
	Confidence float64        // 0..1 — useful when matching is fuzzy
	Metadata   map[string]any // platform-attached: bible link, image, etc.
}

// EntityEntry describes a named entity the annotator should look for.
type EntityEntry struct {
	ID    string   // matches EntityAnnotation.EntityID
	Kind  string   // matches EntityAnnotation.EntityKind
	Names []string // name variants and aliases
}

// EntityRegistry is the list of entities known at annotation time.
type EntityRegistry struct {
	Entries []EntityEntry
	// SessionID is provided for context; annotators may use it to query history.
	SessionID uuid.UUID
}
