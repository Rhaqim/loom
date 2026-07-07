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
	"strings"

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
	root.PersistentFlags().String("dsn", "", "PostgreSQL connection string. WARNING: visible to other users via 'ps'/'/proc'; prefer LOOM_DSN or --dsn-file")
	root.PersistentFlags().String("dsn-file", "", "path to a file containing the PostgreSQL connection string (keeps credentials out of the process arguments)")

	root.AddCommand(migrateCmd(), seedCmd(), testCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// openDB opens a database connection from flags / env. The DSN (which contains
// credentials) is resolved in order of decreasing safety: a --dsn-file, then the
// LOOM_DSN env var, then the --dsn flag. The flag is the least safe because the
// connection string — password included — is visible to any local user via
// 'ps'/'/proc/<pid>/cmdline', so its use emits a warning.
func openDB(cmd *cobra.Command) (*sql.DB, error) {
	var dsn string
	if dsnFile, _ := cmd.Flags().GetString("dsn-file"); dsnFile != "" {
		b, err := os.ReadFile(dsnFile)
		if err != nil {
			return nil, fmt.Errorf("read --dsn-file: %w", err)
		}
		dsn = strings.TrimSpace(string(b))
	}
	if dsn == "" {
		dsn = os.Getenv("LOOM_DSN")
	}
	if dsn == "" {
		if flagDSN, _ := cmd.Flags().GetString("dsn"); flagDSN != "" {
			fmt.Fprintln(os.Stderr, "warning: --dsn exposes database credentials in the process arguments (visible via 'ps'); prefer LOOM_DSN or --dsn-file")
			dsn = flagDSN
		}
	}
	if dsn == "" {
		return nil, fmt.Errorf("database DSN is required: set LOOM_DSN, --dsn-file, or --dsn")
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
