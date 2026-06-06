// Package store handles all persistence for the DND application layer.
// It operates on dnd_* tables and is entirely separate from loom's own persistence.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rhaqim/loom-dnd/domain"
)

// Store holds the database connection and provides all CRUD operations.
type Store struct {
	db *sql.DB
}

// New creates a Store backed by the given database connection.
func New(db *sql.DB) *Store { return &Store{db: db} }

// -----------------------------------------------------------------------
// Topics
// -----------------------------------------------------------------------

func (s *Store) CreateTopic(ctx context.Context, t *domain.Topic) error {
	t.ID = uuid.New()
	t.CreatedAt = time.Now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO dnd_topics (id, slug, name, description, setting, edition, dm_agent_slug, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		t.ID, t.Slug, t.Name, t.Description, t.Setting, t.Edition, t.DMAgentSlug, t.CreatedAt,
	)
	return err
}

func (s *Store) GetTopicBySlug(ctx context.Context, slug string) (*domain.Topic, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, slug, name, description, setting, edition, dm_agent_slug, created_at
		FROM dnd_topics WHERE slug=$1`, slug)
	return scanTopic(row)
}

func (s *Store) GetTopicByID(ctx context.Context, id uuid.UUID) (*domain.Topic, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, slug, name, description, setting, edition, dm_agent_slug, created_at
		FROM dnd_topics WHERE id=$1`, id)
	return scanTopic(row)
}

func (s *Store) ListTopics(ctx context.Context) ([]*domain.Topic, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, slug, name, description, setting, edition, dm_agent_slug, created_at
		FROM dnd_topics ORDER BY created_at DESC`)
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

func (s *Store) UpdateTopicDMAgent(ctx context.Context, topicID uuid.UUID, agentSlug string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE dnd_topics SET dm_agent_slug=$1 WHERE id=$2`, agentSlug, topicID)
	return err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanTopic(row scanner) (*domain.Topic, error) {
	var t domain.Topic
	var id string
	if err := row.Scan(&id, &t.Slug, &t.Name, &t.Description, &t.Setting, &t.Edition, &t.DMAgentSlug, &t.CreatedAt); err != nil {
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
	return &t, nil
}

// -----------------------------------------------------------------------
// Users
// -----------------------------------------------------------------------

func (s *Store) CreateUser(ctx context.Context, u *domain.User) error {
	u.ID = uuid.New()
	u.CreatedAt = time.Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO dnd_users (id, username, created_at) VALUES ($1,$2,$3)`,
		u.ID, u.Username, u.CreatedAt,
	)
	return err
}

func (s *Store) GetUserByUsername(ctx context.Context, username string) (*domain.User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, username, created_at FROM dnd_users WHERE username=$1`, username)
	return scanUser(row)
}

func (s *Store) ListUsers(ctx context.Context) ([]*domain.User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, username, created_at FROM dnd_users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []*domain.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func scanUser(row scanner) (*domain.User, error) {
	var u domain.User
	var id string
	if err := row.Scan(&id, &u.Username, &u.CreatedAt); err != nil {
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
// Campaigns
// -----------------------------------------------------------------------

func (s *Store) CreateCampaign(ctx context.Context, c *domain.Campaign) error {
	c.ID = uuid.New()
	c.CreatedAt = time.Now()
	if c.Status == "" {
		c.Status = "active"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO dnd_campaigns (id, topic_id, loom_session_id, name, description, status, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		c.ID, c.TopicID, nullStr(c.LoomSessionID), c.Name, c.Description, c.Status, c.CreatedAt,
	)
	return err
}

func (s *Store) GetCampaignByID(ctx context.Context, id uuid.UUID) (*domain.Campaign, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, topic_id, loom_session_id, name, description, status, created_at
		FROM dnd_campaigns WHERE id=$1`, id)
	return scanCampaign(row)
}

func (s *Store) ListCampaigns(ctx context.Context) ([]*domain.Campaign, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, topic_id, loom_session_id, name, description, status, created_at
		FROM dnd_campaigns ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var campaigns []*domain.Campaign
	for rows.Next() {
		c, err := scanCampaign(rows)
		if err != nil {
			return nil, err
		}
		campaigns = append(campaigns, c)
	}
	return campaigns, rows.Err()
}

func (s *Store) SetCampaignSession(ctx context.Context, campaignID uuid.UUID, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE dnd_campaigns SET loom_session_id=$1 WHERE id=$2`, sessionID, campaignID)
	return err
}

func (s *Store) SetCampaignStatus(ctx context.Context, campaignID uuid.UUID, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE dnd_campaigns SET status=$1 WHERE id=$2`, status, campaignID)
	return err
}

func scanCampaign(row scanner) (*domain.Campaign, error) {
	var c domain.Campaign
	var id, topicID string
	var sessID sql.NullString
	if err := row.Scan(&id, &topicID, &sessID, &c.Name, &c.Description, &c.Status, &c.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("campaign not found")
		}
		return nil, err
	}
	var err error
	c.ID, err = uuid.Parse(id)
	if err != nil {
		return nil, err
	}
	c.TopicID, err = uuid.Parse(topicID)
	if err != nil {
		return nil, err
	}
	if sessID.Valid {
		c.LoomSessionID = sessID.String
	}
	return &c, nil
}

// -----------------------------------------------------------------------
// Characters
// -----------------------------------------------------------------------

func (s *Store) CreateCharacter(ctx context.Context, ch *domain.Character) error {
	ch.ID = uuid.New()
	ch.CreatedAt = time.Now()
	invJSON, _ := json.Marshal(ch.Inventory)
	slotsJSON, _ := json.Marshal(ch.SpellSlots)
	condJSON, _ := json.Marshal(ch.Conditions)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO dnd_characters
			(id, user_id, campaign_id, name, race, class, background, level, experience,
			 max_hp, current_hp, armor_class,
			 strength, dexterity, constitution, intelligence, wisdom, charisma,
			 gold, inventory, spell_slots, conditions, notes, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)`,
		ch.ID, ch.UserID, ch.CampaignID, ch.Name, ch.Race, ch.Class, ch.Background,
		ch.Level, ch.Experience, ch.MaxHP, ch.CurrentHP, ch.ArmorClass,
		ch.Strength, ch.Dexterity, ch.Constitution, ch.Intelligence, ch.Wisdom, ch.Charisma,
		ch.Gold, invJSON, slotsJSON, condJSON, ch.Notes, ch.CreatedAt,
	)
	return err
}

func (s *Store) GetCharacter(ctx context.Context, userID, campaignID uuid.UUID) (*domain.Character, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, campaign_id, name, race, class, background, level, experience,
		       max_hp, current_hp, armor_class,
		       strength, dexterity, constitution, intelligence, wisdom, charisma,
		       gold, inventory, spell_slots, conditions, notes, created_at
		FROM dnd_characters WHERE user_id=$1 AND campaign_id=$2`, userID, campaignID)
	return scanCharacter(row)
}

func (s *Store) GetCharacterByID(ctx context.Context, id uuid.UUID) (*domain.Character, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, campaign_id, name, race, class, background, level, experience,
		       max_hp, current_hp, armor_class,
		       strength, dexterity, constitution, intelligence, wisdom, charisma,
		       gold, inventory, spell_slots, conditions, notes, created_at
		FROM dnd_characters WHERE id=$1`, id)
	return scanCharacter(row)
}

func (s *Store) UpdateCharacter(ctx context.Context, ch *domain.Character) error {
	invJSON, _ := json.Marshal(ch.Inventory)
	slotsJSON, _ := json.Marshal(ch.SpellSlots)
	condJSON, _ := json.Marshal(ch.Conditions)
	_, err := s.db.ExecContext(ctx, `
		UPDATE dnd_characters SET
			current_hp=$1, max_hp=$2, level=$3, experience=$4, gold=$5,
			armor_class=$6, inventory=$7, spell_slots=$8, conditions=$9, notes=$10
		WHERE id=$11`,
		ch.CurrentHP, ch.MaxHP, ch.Level, ch.Experience, ch.Gold,
		ch.ArmorClass, invJSON, slotsJSON, condJSON, ch.Notes, ch.ID,
	)
	return err
}

func scanCharacter(row scanner) (*domain.Character, error) {
	var ch domain.Character
	var id, userID, campaignID string
	var invJSON, slotsJSON, condJSON []byte
	if err := row.Scan(
		&id, &userID, &campaignID, &ch.Name, &ch.Race, &ch.Class, &ch.Background,
		&ch.Level, &ch.Experience, &ch.MaxHP, &ch.CurrentHP, &ch.ArmorClass,
		&ch.Strength, &ch.Dexterity, &ch.Constitution, &ch.Intelligence, &ch.Wisdom, &ch.Charisma,
		&ch.Gold, &invJSON, &slotsJSON, &condJSON, &ch.Notes, &ch.CreatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("character not found")
		}
		return nil, err
	}
	var err error
	ch.ID, err = uuid.Parse(id)
	if err != nil {
		return nil, err
	}
	ch.UserID, err = uuid.Parse(userID)
	if err != nil {
		return nil, err
	}
	ch.CampaignID, err = uuid.Parse(campaignID)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(invJSON, &ch.Inventory)
	_ = json.Unmarshal(slotsJSON, &ch.SpellSlots)
	_ = json.Unmarshal(condJSON, &ch.Conditions)
	if ch.Inventory == nil {
		ch.Inventory = []string{}
	}
	if ch.SpellSlots == nil {
		ch.SpellSlots = map[string]int{}
	}
	if ch.Conditions == nil {
		ch.Conditions = []string{}
	}
	return &ch, nil
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
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
	// collapse repeated dashes
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return strings.Trim(s, "-")
}
