// Package store handles all persistence for the story-API application layer.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rhaqim/story-api/domain"
)

// Store provides all CRUD operations for story-API tables.
type Store struct{ db *sql.DB }

func New(db *sql.DB) *Store { return &Store{db: db} }

// -----------------------------------------------------------------------
// Users
// -----------------------------------------------------------------------

func (s *Store) CreateUser(ctx context.Context, u *domain.User) error {
	u.ID = uuid.New()
	u.CreatedAt = time.Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO story_users (id, username, password_hash, created_at)
		 VALUES ($1,$2,$3,$4)`,
		u.ID, u.Username, u.PasswordHash, u.CreatedAt,
	)
	return err
}

func (s *Store) GetUserByUsername(ctx context.Context, username string) (*domain.User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at FROM story_users WHERE username=$1`, username)
	return scanUser(row)
}

func (s *Store) GetUserByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at FROM story_users WHERE id=$1`, id)
	return scanUser(row)
}

func scanUser(row scanner) (*domain.User, error) {
	var u domain.User
	var id string
	if err := row.Scan(&id, &u.Username, &u.PasswordHash, &u.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("user not found")
		}
		return nil, err
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return nil, err
	}
	u.ID = parsed
	return &u, nil
}

// -----------------------------------------------------------------------
// Topics
// -----------------------------------------------------------------------

func (s *Store) CreateTopic(ctx context.Context, t *domain.Topic) error {
	t.ID = uuid.New()
	t.CreatedAt = time.Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO story_topics (id, slug, name, description, genre, dm_agent_slug, created_by, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		t.ID, t.Slug, t.Name, t.Description, t.Genre, t.DMAgentSlug, nullUUID(t.CreatedBy), t.CreatedAt,
	)
	return err
}

func (s *Store) GetTopicBySlug(ctx context.Context, slug string) (*domain.Topic, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, slug, name, description, genre, dm_agent_slug, created_by, created_at
		 FROM story_topics WHERE slug=$1`, slug)
	return scanTopic(row)
}

func (s *Store) GetTopicByID(ctx context.Context, id uuid.UUID) (*domain.Topic, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, slug, name, description, genre, dm_agent_slug, created_by, created_at
		 FROM story_topics WHERE id=$1`, id)
	return scanTopic(row)
}

func (s *Store) ListTopics(ctx context.Context) ([]*domain.Topic, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, slug, name, description, genre, dm_agent_slug, created_by, created_at
		 FROM story_topics ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var topics []*domain.Topic
	for rows.Next() {
		t, err := scanTopic(rows)
		if err != nil {
			return nil, err
		}
		topics = append(topics, t)
	}
	return topics, rows.Err()
}

func (s *Store) UpdateTopicAgentSlug(ctx context.Context, id uuid.UUID, slug string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE story_topics SET dm_agent_slug=$1 WHERE id=$2`, slug, id)
	return err
}

func scanTopic(row scanner) (*domain.Topic, error) {
	var t domain.Topic
	var id string
	var createdBy sql.NullString
	if err := row.Scan(&id, &t.Slug, &t.Name, &t.Description, &t.Genre, &t.DMAgentSlug, &createdBy, &t.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("topic not found")
		}
		return nil, err
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return nil, err
	}
	t.ID = parsed
	if createdBy.Valid {
		uid, _ := uuid.Parse(createdBy.String)
		t.CreatedBy = &uid
	}
	return &t, nil
}

// -----------------------------------------------------------------------
// Stories
// -----------------------------------------------------------------------

func (s *Store) CreateStory(ctx context.Context, st *domain.Story) error {
	st.ID = uuid.New()
	now := time.Now()
	st.CreatedAt = now
	st.UpdatedAt = now
	if st.Status == "" {
		st.Status = "active"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO story_sessions
		 (id, user_id, topic_id, loom_session_id, name, status, turn_count, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		st.ID, st.UserID, st.TopicID, st.LoomSessionID,
		st.Name, st.Status, st.TurnCount, st.CreatedAt, st.UpdatedAt,
	)
	return err
}

func (s *Store) GetStoryByID(ctx context.Context, id uuid.UUID) (*domain.Story, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, topic_id, loom_session_id, name, status, turn_count, created_at, updated_at
		 FROM story_sessions WHERE id=$1`, id)
	return scanStory(row)
}

func (s *Store) ListStoriesByUser(ctx context.Context, userID uuid.UUID) ([]*domain.Story, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, topic_id, loom_session_id, name, status, turn_count, created_at, updated_at
		 FROM story_sessions WHERE user_id=$1 ORDER BY updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stories []*domain.Story
	for rows.Next() {
		st, err := scanStory(rows)
		if err != nil {
			return nil, err
		}
		stories = append(stories, st)
	}
	return stories, rows.Err()
}

func (s *Store) UpdateStory(ctx context.Context, st *domain.Story) error {
	st.UpdatedAt = time.Now()
	_, err := s.db.ExecContext(ctx,
		`UPDATE story_sessions
		 SET loom_session_id=$1, status=$2, turn_count=$3, updated_at=$4
		 WHERE id=$5`,
		st.LoomSessionID, st.Status, st.TurnCount, st.UpdatedAt, st.ID,
	)
	return err
}

func (s *Store) AbandonStory(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE story_sessions SET status='abandoned', updated_at=now() WHERE id=$1`, id)
	return err
}

func scanStory(row scanner) (*domain.Story, error) {
	var st domain.Story
	var id, userID, topicID string
	if err := row.Scan(
		&id, &userID, &topicID, &st.LoomSessionID,
		&st.Name, &st.Status, &st.TurnCount, &st.CreatedAt, &st.UpdatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("story not found")
		}
		return nil, err
	}
	var err error
	st.ID, err = uuid.Parse(id)
	if err != nil {
		return nil, err
	}
	st.UserID, err = uuid.Parse(userID)
	if err != nil {
		return nil, err
	}
	st.TopicID, err = uuid.Parse(topicID)
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

type scanner interface{ Scan(dest ...any) error }

func nullUUID(id *uuid.UUID) sql.NullString {
	if id == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: id.String(), Valid: true}
}

// Slugify converts a name to a URL-safe slug.
func Slugify(name string) string {
	s := strings.ToLower(name)
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, s)
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return strings.Trim(s, "-")
}
