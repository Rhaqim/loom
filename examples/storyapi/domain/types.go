package domain

import (
	"time"

	"github.com/google/uuid"
)

// User is a registered player.
type User struct {
	ID           uuid.UUID `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}

// Topic is a story genre / setting.
type Topic struct {
	ID          uuid.UUID  `json:"id"`
	Slug        string     `json:"slug"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Genre       string     `json:"genre"`
	DMAgentSlug string     `json:"-"`
	CreatedBy   *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// Story is an ongoing narrative for one user within one topic.
type Story struct {
	ID            uuid.UUID `json:"id"`
	UserID        uuid.UUID `json:"user_id"`
	TopicID       uuid.UUID `json:"topic_id"`
	LoomSessionID string    `json:"-"`
	Name          string    `json:"name"`
	Status        string    `json:"status"`
	TurnCount     int       `json:"turn_count"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// IsActive reports whether the story is still running.
func (s *Story) IsActive() bool { return s.Status == "active" }

// Turn is one player→narrator exchange within a story.
// It is reconstructed from the loom session history on demand.
type Turn struct {
	Index       int       `json:"index"`
	PlayerInput string    `json:"player_input"`
	DMResponse  string    `json:"dm_response"`
	DurationMs  int       `json:"duration_ms"`
	CreatedAt   time.Time `json:"created_at"`
}
