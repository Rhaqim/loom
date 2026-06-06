package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/rhaqim/loom-dnd/domain"
	"github.com/rhaqim/loom-dnd/game"
	"github.com/rhaqim/loom-dnd/store"
	"github.com/spf13/cobra"
)

func playCmd() *cobra.Command {
	var username, campaignID string
	cmd := &cobra.Command{
		Use:   "play",
		Short: "Start or resume an interactive D&D session",
		Long: `Enter the world and speak to your AI Dungeon Master.

Special commands during play:
  !sheet      — display your character sheet
  !roll NdS   — roll dice (e.g. !roll 1d20, !roll 2d6)
  !hp N       — set your current HP to N
  !damage N   — take N damage
  !heal N     — heal N HP
  !item add X — add item X to inventory
  !item drop X— drop item X from inventory
  !gold N     — set gold to N
  !quit       — end the session`,
		Example: `  loom-dnd play --username thorin --campaign 4e9f2a...`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if username == "" || campaignID == "" {
				return fmt.Errorf("--username and --campaign are required")
			}

			conn, st, e, err := openEngine(context.Background(), cmd)
			if err != nil {
				return err
			}
			defer conn.Close()
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
			if !campaign.IsActive() {
				return fmt.Errorf("campaign %q is %s", campaign.Name, campaign.Status)
			}

			topic, err := st.GetTopicByID(ctx, campaign.TopicID)
			if err != nil {
				return fmt.Errorf("topic not found: %w", err)
			}

			// Ensure DM agent is seeded in loom for this topic.
			agentSlug, err := e.SeedDMAgent(ctx, topic)
			if err != nil {
				return fmt.Errorf("seed DM agent: %w", err)
			}
			_ = st.UpdateTopicDMAgent(ctx, topic.ID, agentSlug)

			character, err := st.GetCharacter(ctx, user.ID, campaign.ID)
			if err != nil {
				return fmt.Errorf("character not found — run: loom-dnd character create --username %s --campaign %s", username, campaignID)
			}

			sess, err := e.StartSession(ctx, campaign, character, user.ID.String())
			if err != nil {
				return fmt.Errorf("start session: %w", err)
			}

			// Welcome banner.
			clearScreen()
			fmt.Println(banner())
			fmt.Printf("Campaign : %s\n", campaign.Name)
			fmt.Printf("Setting  : %s (%s %s)\n", topic.Name, topic.Setting, topic.Edition)
			fmt.Printf("Generator: %s\n", game.BestGeneratorSlug())
			fmt.Println()
			fmt.Println(character.Sheet())
			fmt.Println("Type your actions in plain English. Special commands start with !")
			fmt.Println("─────────────────────────────────────────────────────────────────")
			fmt.Println()

			isNew := len(sess.History) == 0
			if isNew {
				fmt.Println("[DM is setting the scene...]")
				fmt.Println()
				opening := fmt.Sprintf(
					"Begin the campaign. Describe the opening scene for %s, a %s %s. "+
						"Set the atmosphere, introduce the world, and present the first situation or hook.",
					character.Name, character.Race, character.Class,
				)
				resp, rerr := e.Play(ctx, sess, character, agentSlug, opening)
				if rerr != nil {
					return fmt.Errorf("opening scene: %w", rerr)
				}
				// Non-streaming: print the response.
				if resp != "" {
					fmt.Println(resp)
				}
				fmt.Println()
			} else {
				fmt.Printf("Resuming session (%d turns played)...\n\n", len(sess.History))
			}

			// Persist character on exit.
			defer func() { _ = st.UpdateCharacter(ctx, character) }()

			// REPL.
			scanner := bufio.NewScanner(os.Stdin)
			for {
				if !character.IsAlive() {
					fmt.Println("\n💀 Your character has fallen. The adventure is over.")
					break
				}
				fmt.Printf("\n[%s | HP %d/%d] > ", character.Name, character.CurrentHP, character.MaxHP)
				if !scanner.Scan() {
					break
				}
				input := strings.TrimSpace(scanner.Text())
				if input == "" {
					continue
				}
				if strings.HasPrefix(input, "!") {
					if quit := handleMetaCommand(input, character, st, ctx); quit {
						fmt.Println("\nFarewell, adventurer. Until next time!")
						break
					}
					continue
				}
				fmt.Println()
				resp, rerr := e.Play(ctx, sess, character, agentSlug, input)
				if rerr != nil {
					fmt.Fprintf(os.Stderr, "Error: %v\n", rerr)
					continue
				}
				if resp != "" {
					fmt.Println(resp)
				}
				_ = st.UpdateCharacter(ctx, character)
			}

			if err := scanner.Err(); err != nil {
				fmt.Fprintf(os.Stderr, "Error reading input: %v\n", err)
			}

			return nil
		},
	}
	cmd.Flags().StringVar(&username, "username", "", "Your player username (required)")
	cmd.Flags().StringVar(&campaignID, "campaign", "", "Campaign ID (required) — see: loom-dnd campaign list")
	return cmd
}

// handleMetaCommand processes ! commands. Returns true if the session should quit.
func handleMetaCommand(input string, ch *domain.Character, st *store.Store, ctx context.Context) (quit bool) {
	parts := strings.Fields(input)
	switch strings.ToLower(parts[0]) {
	case "!quit", "!exit", "!q":
		return true

	case "!sheet":
		fmt.Println(ch.Sheet())

	case "!roll":
		if len(parts) < 2 {
			fmt.Println("Usage: !roll NdS  (e.g. !roll 1d20, !roll 2d6)")
			return false
		}
		result, desc, err := game.RollExpr(parts[1])
		if err != nil {
			fmt.Println(err)
			return false
		}
		fmt.Printf("🎲 %s → %d  %s\n", parts[1], result, desc)

	case "!hp":
		if len(parts) < 2 {
			fmt.Printf("Current HP: %d/%d\n", ch.CurrentHP, ch.MaxHP)
			return false
		}
		if n, err := strconv.Atoi(parts[1]); err == nil && n >= 0 {
			ch.CurrentHP = clamp(n, 0, ch.MaxHP)
			fmt.Printf("HP set to %d/%d\n", ch.CurrentHP, ch.MaxHP)
		}

	case "!damage":
		if len(parts) >= 2 {
			if n, err := strconv.Atoi(parts[1]); err == nil && n >= 0 {
				ch.CurrentHP = clamp(ch.CurrentHP-n, 0, ch.MaxHP)
				fmt.Printf("💥 Took %d damage. HP: %d/%d\n", n, ch.CurrentHP, ch.MaxHP)
				if !ch.IsAlive() {
					fmt.Println("💀 You have fallen!")
				}
			}
		}

	case "!heal":
		if len(parts) >= 2 {
			if n, err := strconv.Atoi(parts[1]); err == nil && n >= 0 {
				ch.CurrentHP = clamp(ch.CurrentHP+n, 0, ch.MaxHP)
				fmt.Printf("💚 Healed %d. HP: %d/%d\n", n, ch.CurrentHP, ch.MaxHP)
			}
		}

	case "!gold":
		if len(parts) < 2 {
			fmt.Printf("Gold: %d gp\n", ch.Gold)
			return false
		}
		if n, err := strconv.Atoi(parts[1]); err == nil {
			ch.Gold = clamp(n, 0, 1<<30)
			fmt.Printf("💰 Gold: %d gp\n", ch.Gold)
		}

	case "!item":
		if len(parts) < 3 {
			fmt.Println("Usage: !item add <item>  |  !item drop <item>")
			return false
		}
		item := strings.Join(parts[2:], " ")
		switch strings.ToLower(parts[1]) {
		case "add":
			ch.Inventory = append(ch.Inventory, item)
			fmt.Printf("📦 Added %q to inventory\n", item)
		case "drop", "remove":
			for i, v := range ch.Inventory {
				if strings.EqualFold(v, item) {
					ch.Inventory = append(ch.Inventory[:i], ch.Inventory[i+1:]...)
					fmt.Printf("🗑  Removed %q\n", item)
					return false
				}
			}
			fmt.Printf("Item %q not found\n", item)
		}

	case "!help":
		fmt.Print(`Commands:
  !sheet        — character sheet
  !roll NdS     — roll dice
  !hp N         — set HP
  !damage N     — take damage
  !heal N       — restore HP
  !gold N       — set gold
  !item add X   — add to inventory
  !item drop X  — drop from inventory
  !quit         — end session
`)

	default:
		fmt.Printf("Unknown command %q. Type !help for a list.\n", parts[0])
	}
	return false
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clearScreen() { fmt.Print("\033[H\033[2J") }

func banner() string {
	return ` ╔══════════════════════════════════════════════╗
 ║                                              ║
 ║   ⚔   loom-dnd  —  AI Dungeon Master  ⚔      ║
 ║         powered by the loom engine           ║
 ║                                              ║
 ╚══════════════════════════════════════════════╝
`
}
