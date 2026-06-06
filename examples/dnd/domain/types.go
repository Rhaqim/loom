package domain

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// -----------------------------------------------------------------------
// Topic
// -----------------------------------------------------------------------

// Topic is a D&D universe or setting (Forgotten Realms, Eberron, Dark Sun, Homebrew…).
type Topic struct {
	ID          uuid.UUID
	Slug        string
	Name        string
	Description string
	Setting     string // "high-fantasy", "dark-fantasy", "steampunk", "horror"
	Edition     string // "5e", "3.5e", "pathfinder", "osr"
	DMAgentSlug string // loom agent slug for this topic's DM
	CreatedAt   time.Time
}

// -----------------------------------------------------------------------
// User
// -----------------------------------------------------------------------

// User is a human player account.
type User struct {
	ID        uuid.UUID
	Username  string
	CreatedAt time.Time
}

// -----------------------------------------------------------------------
// Campaign
// -----------------------------------------------------------------------

// Campaign is an ongoing story arc within a topic.
type Campaign struct {
	ID            uuid.UUID
	TopicID       uuid.UUID
	LoomSessionID string // empty until first play session
	Name          string
	Description   string
	Status        string // "active", "completed", "abandoned"
	CreatedAt     time.Time
}

// IsActive reports whether the campaign is still running.
func (c *Campaign) IsActive() bool { return c.Status == "active" }

// -----------------------------------------------------------------------
// Character
// -----------------------------------------------------------------------

// Character is a player character within a campaign.
type Character struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	CampaignID   uuid.UUID
	Name         string
	Race         string
	Class        string
	Background   string
	Level        int
	Experience   int
	MaxHP        int
	CurrentHP    int
	ArmorClass   int
	Strength     int
	Dexterity    int
	Constitution int
	Intelligence int
	Wisdom       int
	Charisma     int
	Gold         int
	Inventory    []string
	SpellSlots   map[string]int // spell level (as string) → remaining slots
	Conditions   []string       // "poisoned", "paralyzed", etc.
	Notes        string
	CreatedAt    time.Time
}

// IsAlive reports whether the character has HP remaining.
func (c *Character) IsAlive() bool { return c.CurrentHP > 0 }

// Modifier computes the standard D&D ability modifier: (score-10)/2.
func Modifier(score int) int { return (score - 10) / 2 }

// ProficiencyBonus returns the proficiency bonus for a given character level.
func ProficiencyBonus(level int) int {
	switch {
	case level <= 4:
		return 2
	case level <= 8:
		return 3
	case level <= 12:
		return 4
	case level <= 16:
		return 5
	default:
		return 6
	}
}

// Sheet returns a formatted character sheet string.
func (c *Character) Sheet() string {
	var b strings.Builder
	fmt.Fprintf(&b, "╔══════════════════════════════════════════╗\n")
	fmt.Fprintf(&b, "║  %-40s║\n", fmt.Sprintf("%s — %s %s (Lvl %d)", c.Name, c.Race, c.Class, c.Level))
	fmt.Fprintf(&b, "║  %-40s║\n", fmt.Sprintf("Background: %s | XP: %d", c.Background, c.Experience))
	fmt.Fprintf(&b, "╠══════════════════════════════════════════╣\n")
	fmt.Fprintf(&b, "║  HP: %d/%d  AC: %d  Gold: %d gp%s║\n",
		c.CurrentHP, c.MaxHP, c.ArmorClass, c.Gold,
		strings.Repeat(" ", 40-len(fmt.Sprintf("  HP: %d/%d  AC: %d  Gold: %d gp", c.CurrentHP, c.MaxHP, c.ArmorClass, c.Gold))),
	)
	fmt.Fprintf(&b, "╠══════════════════════════════════════════╣\n")
	fmt.Fprintf(&b, "║  STR %2d(%+d)  DEX %2d(%+d)  CON %2d(%+d)   ║\n",
		c.Strength, Modifier(c.Strength),
		c.Dexterity, Modifier(c.Dexterity),
		c.Constitution, Modifier(c.Constitution),
	)
	fmt.Fprintf(&b, "║  INT %2d(%+d)  WIS %2d(%+d)  CHA %2d(%+d)   ║\n",
		c.Intelligence, Modifier(c.Intelligence),
		c.Wisdom, Modifier(c.Wisdom),
		c.Charisma, Modifier(c.Charisma),
	)
	if len(c.Inventory) > 0 {
		fmt.Fprintf(&b, "╠══════════════════════════════════════════╣\n")
		fmt.Fprintf(&b, "║  Inventory: %-29s║\n", strings.Join(c.Inventory, ", "))
	}
	if len(c.Conditions) > 0 {
		fmt.Fprintf(&b, "╠══════════════════════════════════════════╣\n")
		fmt.Fprintf(&b, "║  Conditions: %-28s║\n", strings.Join(c.Conditions, ", "))
	}
	fmt.Fprintf(&b, "╚══════════════════════════════════════════╝\n")
	return b.String()
}

// VarsMap converts the character's current state into a map suitable for
// injecting into a loom session's State.Vars so the DM template can use them.
func (c *Character) VarsMap() map[string]any {
	return map[string]any{
		"character_name":       c.Name,
		"character_race":       c.Race,
		"character_class":      c.Class,
		"character_level":      c.Level,
		"character_background": c.Background,
		"current_hp":           c.CurrentHP,
		"max_hp":               c.MaxHP,
		"armor_class":          c.ArmorClass,
		"gold":                 c.Gold,
		"strength":             c.Strength,
		"dexterity":            c.Dexterity,
		"constitution":         c.Constitution,
		"intelligence":         c.Intelligence,
		"wisdom":               c.Wisdom,
		"charisma":             c.Charisma,
		"inventory":            strings.Join(c.Inventory, ", "),
		"conditions":           strings.Join(c.Conditions, ", "),
		"experience":           c.Experience,
		"proficiency":          ProficiencyBonus(c.Level),
	}
}
