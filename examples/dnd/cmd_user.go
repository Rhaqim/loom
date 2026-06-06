package main

import (
	"context"
	"fmt"

	"github.com/rhaqim/loom-dnd/domain"
	"github.com/rhaqim/loom-dnd/store"
	"github.com/spf13/cobra"
)

func userCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage players",
	}
	cmd.AddCommand(userCreateCmd(), userListCmd())
	return cmd
}

func userCreateCmd() *cobra.Command {
	var username string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new player account",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if username == "" {
				return fmt.Errorf("--username is required")
			}
			conn, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()
			st := store.New(conn)

			u := &domain.User{Username: username}
			if err := st.CreateUser(context.Background(), u); err != nil {
				return fmt.Errorf("create user: %w", err)
			}
			fmt.Printf("Player created: %s (id: %s)\n", u.Username, u.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&username, "username", "", "Player username (required)")
	return cmd
}

func userListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all players",
		RunE: func(cmd *cobra.Command, _ []string) error {
			conn, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()
			st := store.New(conn)

			users, err := st.ListUsers(context.Background())
			if err != nil {
				return err
			}
			if len(users) == 0 {
				fmt.Println("No players yet. Run: loom-dnd user create --username <name>")
				return nil
			}
			fmt.Printf("%-36s  %s\n", "ID", "USERNAME")
			fmt.Println("----------------------------------------------")
			for _, u := range users {
				fmt.Printf("%-36s  %s\n", u.ID, u.Username)
			}
			return nil
		},
	}
}
