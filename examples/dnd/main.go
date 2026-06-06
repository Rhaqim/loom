// Command loom-dnd is a solo D&D experience powered by the loom procgen engine.
//
// Usage:
//
//	export DND_DSN="postgres://loom:loom@localhost:5432/loom?sslmode=disable"
//	loom-dnd migrate
//	loom-dnd topic create --name "Forgotten Realms" --setting high-fantasy --edition 5e
//	loom-dnd user create --username thorin
//	loom-dnd campaign create --topic forgotten-realms --name "The Lost Mine of Phandelver"
//	loom-dnd character create --username thorin --campaign <id>
//	loom-dnd play --username thorin --campaign <id>
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	_ "github.com/lib/pq"
	"github.com/rhaqim/loom-dnd/db"
	"github.com/rhaqim/loom-dnd/game"
	"github.com/rhaqim/loom-dnd/store"
	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "loom-dnd",
		Short: "A solo D&D experience powered by the loom procgen engine",
		Long: `loom-dnd lets you learn and play Dungeons & Dragons solo with an AI Dungeon Master.

Set DND_DSN to your PostgreSQL connection string:
  export DND_DSN="postgres://loom:loom@localhost:5432/loom?sslmode=disable"

Then run:
  loom-dnd migrate                        # set up the database
  loom-dnd topic create --name "..."      # create a D&D setting
  loom-dnd user create --username <name>  # create your player account
  loom-dnd campaign create ...            # start a new campaign
  loom-dnd character create ...           # create your character
  loom-dnd play ...                       # start playing!`,
	}

	root.PersistentFlags().String("dsn", "", "PostgreSQL DSN (overrides DND_DSN env var)")

	root.AddCommand(
		migrateCmd(),
		topicCmd(),
		userCmd(),
		campaignCmd(),
		characterCmd(),
		playCmd(),
	)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// -----------------------------------------------------------------------
// shared helpers used by all commands
// -----------------------------------------------------------------------

func openDB(cmd *cobra.Command) (*sql.DB, error) {
	dsn, _ := cmd.Flags().GetString("dsn")
	if dsn == "" {
		dsn = os.Getenv("DND_DSN")
	}
	if dsn == "" {
		return nil, fmt.Errorf("DND_DSN not set — use --dsn or export DND_DSN=<postgres-url>")
	}
	return db.Open(dsn)
}

func openEngine(ctx context.Context, cmd *cobra.Command) (*sql.DB, *store.Store, *game.Engine, error) {
	conn, err := openDB(cmd)
	if err != nil {
		return nil, nil, nil, err
	}
	st := store.New(conn)
	e, err := game.New(ctx, conn, st)
	if err != nil {
		conn.Close()
		return nil, nil, nil, fmt.Errorf("game engine: %w", err)
	}
	return conn, st, e, nil
}
