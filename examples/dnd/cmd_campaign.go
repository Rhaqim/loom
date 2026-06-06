package main

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/rhaqim/loom-dnd/domain"
	"github.com/rhaqim/loom-dnd/store"
	"github.com/spf13/cobra"
)

func campaignCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "campaign",
		Short: "Manage campaigns",
	}
	cmd.AddCommand(campaignCreateCmd(), campaignListCmd())
	return cmd
}

func campaignCreateCmd() *cobra.Command {
	var topicSlug, name, description string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new campaign within a topic",
		Example: `  loom-dnd campaign create --topic forgotten-realms --name "The Lost Mine"
  loom-dnd campaign create --topic ravenloft --name "Curse of Strahd" --description "..."`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if topicSlug == "" || name == "" {
				return fmt.Errorf("--topic and --name are required")
			}
			conn, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()
			st := store.New(conn)
			ctx := context.Background()

			topic, err := st.GetTopicBySlug(ctx, topicSlug)
			if err != nil {
				return fmt.Errorf("topic %q not found (run: loom-dnd topic list)", topicSlug)
			}

			c := &domain.Campaign{
				TopicID:     topic.ID,
				Name:        name,
				Description: description,
			}
			if err := st.CreateCampaign(ctx, c); err != nil {
				return fmt.Errorf("create campaign: %w", err)
			}

			fmt.Printf("Campaign created: %s\n", c.Name)
			fmt.Printf("  ID    : %s\n", c.ID)
			fmt.Printf("  Topic : %s (%s)\n", topic.Name, topic.Setting)
			fmt.Printf("  Status: %s\n", c.Status)
			fmt.Println()
			fmt.Printf("Next step: create your character\n")
			fmt.Printf("  loom-dnd character create --campaign %s --username <your-username>\n", c.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&topicSlug, "topic", "", "Topic slug (required) — see: loom-dnd topic list")
	cmd.Flags().StringVar(&name, "name", "", "Campaign name (required)")
	cmd.Flags().StringVar(&description, "description", "", "Campaign description")
	return cmd
}

func campaignListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all campaigns",
		RunE: func(cmd *cobra.Command, _ []string) error {
			conn, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()
			st := store.New(conn)
			ctx := context.Background()

			campaigns, err := st.ListCampaigns(ctx)
			if err != nil {
				return err
			}
			if len(campaigns) == 0 {
				fmt.Println("No campaigns yet. Run: loom-dnd campaign create --topic <slug> --name \"...\"")
				return nil
			}

			// Pre-fetch topics for display.
			topicCache := map[uuid.UUID]string{}
			topics, _ := st.ListTopics(ctx)
			for _, t := range topics {
				topicCache[t.ID] = t.Name
			}

			fmt.Printf("%-36s  %-8s  %-20s  %s\n", "ID", "STATUS", "TOPIC", "NAME")
			fmt.Println("----------------------------------------------------------------------")
			for _, c := range campaigns {
				topicName := topicCache[c.TopicID]
				fmt.Printf("%-36s  %-8s  %-20s  %s\n", c.ID, c.Status, topicName, c.Name)
			}
			return nil
		},
	}
}
