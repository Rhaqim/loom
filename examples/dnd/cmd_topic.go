package main

import (
	"context"
	"fmt"

	"github.com/rhaqim/loom-dnd/domain"
	"github.com/rhaqim/loom-dnd/game"
	"github.com/rhaqim/loom-dnd/store"
	"github.com/spf13/cobra"
)

func topicCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "topic",
		Short: "Manage D&D topics (settings/universes)",
	}
	cmd.AddCommand(topicCreateCmd(), topicListCmd())
	return cmd
}

func topicCreateCmd() *cobra.Command {
	var name, setting, edition, description string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new D&D topic/setting",
		Example: `  loom-dnd topic create --name "Forgotten Realms" --setting high-fantasy --edition 5e
  loom-dnd topic create --name "Ravenloft" --setting horror --edition 5e
  loom-dnd topic create --name "Eberron" --setting steampunk --edition 5e`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			conn, st, e, err := openEngine(context.Background(), cmd)
			if err != nil {
				return err
			}
			defer conn.Close()
			ctx := context.Background()

			t := &domain.Topic{
				Slug:        store.Slugify(name),
				Name:        name,
				Description: description,
				Setting:     setting,
				Edition:     edition,
			}
			if err := st.CreateTopic(ctx, t); err != nil {
				return fmt.Errorf("create topic: %w", err)
			}

			// Seed the DM agent for this topic in loom.
			agentSlug, err := e.SeedDMAgent(ctx, t)
			if err != nil {
				return fmt.Errorf("seed DM agent: %w", err)
			}
			if err := st.UpdateTopicDMAgent(ctx, t.ID, agentSlug); err != nil {
				return fmt.Errorf("save agent slug: %w", err)
			}

			fmt.Printf("Topic created: %s (slug: %s)\n", t.Name, t.Slug)
			fmt.Printf("  Setting : %s\n", t.Setting)
			fmt.Printf("  Edition : %s\n", t.Edition)
			fmt.Printf("  DM agent: %s (generator: %s)\n", agentSlug, game.BestGeneratorSlug())
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Topic name (required)")
	cmd.Flags().StringVar(&setting, "setting", "high-fantasy", "Setting style: high-fantasy | dark-fantasy | steampunk | horror | seafaring")
	cmd.Flags().StringVar(&edition, "edition", "5e", "Rules edition: 5e | 3.5e | pathfinder | osr")
	cmd.Flags().StringVar(&description, "description", "", "Short description of this world")
	return cmd
}

func topicListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all D&D topics",
		RunE: func(cmd *cobra.Command, _ []string) error {
			conn, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()
			st := store.New(conn)

			topics, err := st.ListTopics(context.Background())
			if err != nil {
				return err
			}
			if len(topics) == 0 {
				fmt.Println("No topics yet. Run: loom-dnd topic create --name \"...\"")
				return nil
			}
			fmt.Printf("%-20s  %-15s  %-6s  %s\n", "SLUG", "SETTING", "EDITION", "NAME")
			fmt.Println("------------------------------------------------------------------")
			for _, t := range topics {
				fmt.Printf("%-20s  %-15s  %-6s  %s\n", t.Slug, t.Setting, t.Edition, t.Name)
			}
			return nil
		},
	}
}
