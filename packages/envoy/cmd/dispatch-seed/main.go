// Command dispatch-seed fills an empty, migrated Dispatch database with the large, deterministic
// data set INFRA-325's measurements are taken on: ten times production's row counts at --scale 1,
// and that shape times --scale at any other. Every key, uuid and timestamp comes from one fixed
// base time and one fixed random seed, and the block ids come from a fixed generator
// (pmdoc.SetBlockIDGenerator), so two runs at one scale write the same rows: a measurement taken
// before a change and one taken after it read the same data.
//
// It writes the rows and then calls the server's own indexers over them (refs.IndexBlocks for a
// document's blocks, refs.IndexItem for a comment's or a message's), and settles the state those
// writes leave owed, so a Dispatch server started on the seeded database rewrites nothing. A
// server that rewrote it would make the two sides of a before-and-after measurement two different
// data sets.
//
// Usage:
//
//	go run ./cmd/dispatch-seed --database-url <url> [--scale <f>] [--no-migrate]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run parses the command's arguments, seeds the database they name and reports what it wrote on
// stdout. Every refusal is written to stderr and answers a non-zero status, so a caller script
// reads the status and a person reads the reason.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("dispatch-seed", flag.ContinueOnError)
	flags.SetOutput(stderr)
	databaseURL := flags.String("database-url", "", "the Postgres URL of the database to fill (required)")
	scale := flags.Float64("scale", 1, "multiplier on every row count; 1 is ten times production, 0.01 is what the CI test runs")
	noMigrate := flags.Bool("no-migrate", false, "do not migrate the database; use it as somebody else migrated it")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "dispatch-seed: unexpected argument %q; the command takes flags alone\n", flags.Arg(0))
		return 2
	}
	if strings.TrimSpace(*databaseURL) == "" {
		fmt.Fprintln(stderr, "dispatch-seed: --database-url is required")
		return 2
	}
	if !(*scale > 0) || *scale > 1000 {
		fmt.Fprintf(stderr, "dispatch-seed: --scale must be above 0 and at most 1000, not %v\n", *scale)
		return 2
	}
	if err := Seed(ctx, Options{DatabaseURL: *databaseURL, Scale: *scale, Migrate: !*noMigrate, Out: stdout}); err != nil {
		fmt.Fprintf(stderr, "dispatch-seed: %v\n", err)
		return 1
	}
	return 0
}

// Options are one run of the seed: the database it fills, how much of the full data set it writes
// (Scale), whether it brings the database to the current schema itself, and where it reports.
type Options struct {
	DatabaseURL string
	Scale       float64
	Migrate     bool
	Out         io.Writer
}

// Seed fills the database Options names and reports every row count, distribution and measurement
// subject it wrote. It migrates the database with store.Migrate, the way cmd/dispatch's own
// subcommands do (openMigrated), rather than by booting a server, whose loops would write to the
// data set it is building. --no-migrate leaves that to the caller, which is what the two-commit
// measurement needs: the "before" side migrates from the release before this one and seeds with
// this tool.
//
// The refusal comes before the migration, so a --database-url that names a database in use by
// mistake is refused with that database as it was found. Migrating it first would apply this
// release's own schema change - this change's ten migrations, 20261101182419 to 20261101182522,
// which no release has run yet - to somebody else's database, and a refusal afterwards does not
// take that back.
func Seed(ctx context.Context, options Options) error {
	out := options.Out
	if out == nil {
		out = io.Discard
	}
	database, err := store.Open(ctx, options.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Pool.Close()
	if err := refuseFilledDatabase(ctx, database); err != nil {
		return err
	}
	if options.Migrate {
		if err := database.Migrate(ctx); err != nil {
			return fmt.Errorf("migrate database: %w", err)
		}
	}
	_, err = seed(ctx, database, options.Scale, out)
	return err
}

// seed fills an open, migrated, empty database and answers the run, which holds the subjects the
// report printed. The tests seed the database storetest gave them through it, which is the same
// path Seed takes once it has opened and migrated one.
func seed(ctx context.Context, database *store.Store, scale float64, out io.Writer) (*seedRun, error) {
	sizes := newSizes(scale)
	fmt.Fprintf(out, "dispatch-seed: scale %g, random seed %d, base time %s, server URL %s\n",
		sizes.Scale, randomSeed, baseTime.Format(time.RFC3339), serverURL)
	run := &seedRun{database: database, sizes: sizes, out: out, started: time.Now()}
	if err := run.do(ctx); err != nil {
		return nil, err
	}
	return run, nil
}

// seedTables are the tables the refusal reads: the two every other row of the data set hangs off,
// and the two the seed writes first, so both a database somebody else is using and a half-written
// run of this command hold rows in one of them.
var seedTables = []string{"projects", "issues"}

// refuseFilledDatabase refuses a database that already holds Dispatch data. The seed writes fixed
// keys, so a second run would fail halfway through on a duplicate key and leave a half-written
// data set; and a database somebody else filled is not the data set a measurement compares
// against. It names what it found, so the caller knows which database it pointed at.
//
// It runs before this command writes anything, the migration included, so the database it reads
// may carry no Dispatch schema at all: a table that is not there holds no row, and an empty
// database and one carrying an empty schema both pass it. Each table's presence is asked of
// to_regclass first because a count naming a table that does not exist fails the whole statement
// when Postgres parses it, before any case or coalesce around it decides anything.
func refuseFilledDatabase(ctx context.Context, database *store.Store) error {
	found := make([]string, 0, len(seedTables))
	for _, table := range seedTables {
		var present bool
		if err := database.Pool.QueryRow(ctx, `select to_regclass($1) is not null`, table).Scan(&present); err != nil {
			return fmt.Errorf("look for the %s table: %w", table, err)
		}
		if !present {
			continue
		}
		var rows int
		if err := database.Pool.QueryRow(ctx,
			`select count(*) from `+pgx.Identifier{table}.Sanitize()).Scan(&rows); err != nil {
			return fmt.Errorf("count the rows already in %s: %w", table, err)
		}
		if rows > 0 {
			found = append(found, fmt.Sprintf("%d %s rows", rows, table))
		}
	}
	if len(found) == 0 {
		return nil
	}
	return errors.New("the database already holds data (" + strings.Join(found, " and ") +
		"); dispatch-seed fills an empty database, so drop it and create it again")
}
