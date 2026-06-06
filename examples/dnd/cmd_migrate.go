package main

import (
	"context"
	"fmt"

	"github.com/rhaqim/loom-dnd/db"
	"github.com/rhaqim/loom-dnd/game"
	"github.com/spf13/cobra"
)

func migrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply the loom and DND database schemas",
		RunE: func(cmd *cobra.Command, _ []string) error {
			conn, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()
			ctx := context.Background()

			fmt.Print("Applying loom schema... ")
			if err := game.ApplyLoomSchema(ctx, conn); err != nil {
				return fmt.Errorf("loom schema: %w", err)
			}
			fmt.Println("done.")

			fmt.Print("Applying DND schema...  ")
			if err := db.ApplySchema(ctx, conn); err != nil {
				return fmt.Errorf("dnd schema: %w", err)
			}
			fmt.Println("done.")

			fmt.Println("Schemas applied successfully.")
			return nil
		},
	}
}
