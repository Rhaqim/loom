// loom-cli is the command-line interface for the Loom procgen engine.
//
// Commands:
//
//	migrate   Apply the Loom database schema
//	seed      Seed agents, prompts, and generators from a YAML file
//	test      Run a YAML test plan against the engine
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	_ "github.com/lib/pq"
	"github.com/spf13/cobra"

	loom "github.com/rhaqim/loom"
	"github.com/rhaqim/loom/generator/echo"
	"github.com/rhaqim/loom/schema"
)

func main() {
	root := &cobra.Command{
		Use:   "loom-cli",
		Short: "Loom procgen engine CLI",
	}
	root.PersistentFlags().String("dsn", "", "PostgreSQL connection string (overrides LOOM_DSN env)")

	root.AddCommand(migrateCmd(), seedCmd(), testCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// openDB opens a database connection from flags / env.
func openDB(cmd *cobra.Command) (*sql.DB, error) {
	dsn, _ := cmd.Flags().GetString("dsn")
	if dsn == "" {
		dsn = os.Getenv("LOOM_DSN")
	}
	if dsn == "" {
		return nil, fmt.Errorf("database DSN is required: set --dsn or LOOM_DSN")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return db, nil
}

// -----------------------------------------------------------------------
// migrate
// -----------------------------------------------------------------------

func migrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply Loom schema to the database",
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer db.Close()

			prefix, _ := cmd.Flags().GetString("prefix")
			loader := schema.NewLoader(schema.DialectPostgres, prefix)
			if err := loader.Apply(context.Background(), db); err != nil {
				return fmt.Errorf("migrate: %w", err)
			}
			fmt.Println("Schema applied successfully.")
			return nil
		},
	}
	cmd.Flags().String("prefix", "loom_", "Table name prefix")
	return cmd
}

// -----------------------------------------------------------------------
// seed
// -----------------------------------------------------------------------

func seedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "seed <file.yaml>",
		Short: "Seed agents, prompts, and generators from a YAML file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer db.Close()

			prefix, _ := cmd.Flags().GetString("prefix")
			e, err := loom.New(loom.Config{
				DB:           db,
				Dialect:      loom.DialectPostgres,
				SchemaPrefix: prefix,
				Generators:   cliGenerators(),
			})
			if err != nil {
				return fmt.Errorf("engine: %w", err)
			}

			sf, err := loadSeedFile(args[0])
			if err != nil {
				return err
			}
			return runSeed(context.Background(), e, sf)
		},
	}
	cmd.Flags().String("prefix", "loom_", "Table name prefix")
	return cmd
}

// -----------------------------------------------------------------------
// test
// -----------------------------------------------------------------------

func testCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "test <plan.yaml>",
		Short: "Run a YAML test plan against the engine",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer db.Close()

			prefix, _ := cmd.Flags().GetString("prefix")
			e, err := loom.New(loom.Config{
				DB:           db,
				Dialect:      loom.DialectPostgres,
				SchemaPrefix: prefix,
				Generators:   cliGenerators(),
			})
			if err != nil {
				return fmt.Errorf("engine: %w", err)
			}

			plan, err := loadTestPlan(args[0])
			if err != nil {
				return err
			}

			report, err := runTestPlan(context.Background(), e, plan)
			if err != nil {
				return err
			}

			printReport(report)
			if !report.Passed {
				os.Exit(1)
			}
			return nil
		},
	}
	cmd.Flags().String("prefix", "loom_", "Table name prefix")
	return cmd
}

// cliGenerators returns the generators available to the CLI.
// The echo generator is always included so test plans can run without real API keys.
// Real generators (openai, anthropic) are activated when their API-key env vars are set.
func cliGenerators() map[string]loom.Generator {
	gens := map[string]loom.Generator{
		"echo": echo.New(""),
	}
	return gens
}
