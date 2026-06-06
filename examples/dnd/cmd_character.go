package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/rhaqim/loom-dnd/domain"
	"github.com/rhaqim/loom-dnd/game"
	"github.com/rhaqim/loom-dnd/store"
	"github.com/spf13/cobra"
)

func characterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "character",
		Short: "Manage player characters",
	}
	cmd.AddCommand(characterCreateCmd(), characterSheetCmd())
	return cmd
}

func characterCreateCmd() *cobra.Command {
	var username, campaignID, name, race, class, background string
	var rollStats bool

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new character in a campaign",
		Example: `  # Roll random ability scores (4d6 drop lowest):
  loom-dnd character create --campaign <id> --username thorin \
      --name "Thorin Ironforge" --race Dwarf --class Fighter --roll-stats

  # Use standard array (15,14,13,12,10,8):
  loom-dnd character create --campaign <id> --username thorin \
      --name "Aria Swiftwind" --race Elf --class Rogue`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if username == "" || campaignID == "" || name == "" || race == "" || class == "" {
				return fmt.Errorf("--username, --campaign, --name, --race, and --class are required")
			}
			conn, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()
			st := store.New(conn)
			ctx := context.Background()

			user, err := st.GetUserByUsername(ctx, username)
			if err != nil {
				return fmt.Errorf("user %q not found (run: loom-dnd user create --username %s)", username, username)
			}

			campID, err := uuid.Parse(campaignID)
			if err != nil {
				return fmt.Errorf("invalid campaign ID: %w", err)
			}
			campaign, err := st.GetCampaignByID(ctx, campID)
			if err != nil {
				return fmt.Errorf("campaign not found: %w", err)
			}

			// Build ability scores.
			var scores [6]int
			if rollStats {
				scores = game.RollStats()
				fmt.Printf("Rolled ability scores (4d6 drop lowest): %v\n", scores)
			} else {
				// Standard array: 15, 14, 13, 12, 10, 8
				scores = [6]int{15, 14, 13, 12, 10, 8}
				fmt.Printf("Using standard array: %v\n", scores)
			}

			if background == "" {
				background = "Folk Hero"
			}

			// Assign scores in order: STR DEX CON INT WIS CHA.
			str, dex, con, intel, wis, cha := scores[0], scores[1], scores[2], scores[3], scores[4], scores[5]
			conMod := domain.Modifier(con)
			dexMod := domain.Modifier(dex)
			maxHP := game.BaseHPForClass(class, conMod)
			ac := game.StartingACForClass(class, dexMod)

			ch := &domain.Character{
				UserID:       user.ID,
				CampaignID:   campaign.ID,
				Name:         name,
				Race:         race,
				Class:        class,
				Background:   background,
				Level:        1,
				MaxHP:        maxHP,
				CurrentHP:    maxHP,
				ArmorClass:   ac,
				Strength:     str,
				Dexterity:    dex,
				Constitution: con,
				Intelligence: intel,
				Wisdom:       wis,
				Charisma:     cha,
				Gold:         10,
				Inventory:    startingInventory(class),
				SpellSlots:   map[string]int{},
				Conditions:   []string{},
			}
			if err := st.CreateCharacter(ctx, ch); err != nil {
				return fmt.Errorf("create character: %w", err)
			}

			fmt.Println()
			fmt.Println(ch.Sheet())
			fmt.Println("Character created! Start your adventure:")
			fmt.Printf("  loom-dnd play --username %s --campaign %s\n", username, campaign.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&username, "username", "", "Player username (required)")
	cmd.Flags().StringVar(&campaignID, "campaign", "", "Campaign ID (required) — see: loom-dnd campaign list")
	cmd.Flags().StringVar(&name, "name", "", "Character name (required)")
	cmd.Flags().StringVar(&race, "race", "", "Race: Human | Elf | Dwarf | Halfling | Gnome | Half-Orc | Tiefling | Dragonborn")
	cmd.Flags().StringVar(&class, "class", "", "Class: Fighter | Wizard | Rogue | Cleric | Bard | Ranger | Paladin | Barbarian | Druid | Monk | Warlock | Sorcerer")
	cmd.Flags().StringVar(&background, "background", "Folk Hero", "Background: Folk Hero | Noble | Soldier | Criminal | Sage | Acolyte | etc.")
	cmd.Flags().BoolVar(&rollStats, "roll-stats", false, "Roll ability scores (4d6 drop lowest) instead of using standard array")
	return cmd
}

func characterSheetCmd() *cobra.Command {
	var username, campaignID string
	cmd := &cobra.Command{
		Use:   "sheet",
		Short: "Display a character sheet",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if username == "" || campaignID == "" {
				return fmt.Errorf("--username and --campaign are required")
			}
			conn, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()
			st := store.New(conn)
			ctx := context.Background()

			user, err := st.GetUserByUsername(ctx, username)
			if err != nil {
				return fmt.Errorf("user %q not found", username)
			}
			campID, err := uuid.Parse(campaignID)
			if err != nil {
				return fmt.Errorf("invalid campaign ID: %w", err)
			}
			ch, err := st.GetCharacter(ctx, user.ID, campID)
			if err != nil {
				return fmt.Errorf("character not found: %w", err)
			}
			fmt.Println(ch.Sheet())
			return nil
		},
	}
	cmd.Flags().StringVar(&username, "username", "", "Player username")
	cmd.Flags().StringVar(&campaignID, "campaign", "", "Campaign ID")
	return cmd
}

// startingInventory returns a sensible default starting kit for a class.
func startingInventory(class string) []string {
	base := []string{"Backpack", "Bedroll", "5x Rations", "Torch x2", "Waterskin"}
	switch strings.ToLower(class) {
	case "fighter", "paladin":
		return append(base, "Longsword", "Shield", "Chainmail")
	case "rogue":
		return append(base, "Shortsword", "Dagger x2", "Thieves' Tools", "Leather Armor")
	case "wizard", "sorcerer":
		return append(base, "Arcane Focus", "Spellbook", "Dagger", "Robes")
	case "cleric":
		return append(base, "Mace", "Shield", "Holy Symbol", "Scale Mail")
	case "ranger":
		return append(base, "Longbow", "20x Arrows", "Shortsword", "Leather Armor")
	case "barbarian":
		return append(base, "Greataxe", "2x Handaxe", "Hide Armor")
	case "bard":
		return append(base, "Rapier", "Lute", "Leather Armor", "Diplomat's Pack")
	case "druid":
		return append(base, "Quarterstaff", "Druidic Focus", "Leather Armor", "Herbalism Kit")
	case "monk":
		return append(base, "Shortsword", "10x Darts", "Explorer's Pack")
	case "warlock":
		return append(base, "Arcane Focus", "Light Crossbow", "20x Bolts", "Leather Armor")
	default:
		return append(base, "Dagger")
	}
}
