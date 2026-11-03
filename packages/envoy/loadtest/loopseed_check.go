package main

// loadgen loop-seed-check: what a seeded Dispatch database holds of a plan, against what the plan
// expected. It reads Postgres only - the seed's own rows, found through loadtest.loop_ids, which
// loop-seed-write wrote as it created them - prints one line per check and exits non-zero when any
// line fails.
//
// Every expected number comes from the plan file, never from a constant typed again here: a plan
// of another seed or another source file expects its own totals, and a check that held copies of
// them would pass against the wrong database.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/jackc/pgx/v5"
)

// loopCheckTolerance is the tolerance a byte or row total is judged at: the seed's sizes are what
// the plan drew, but a deployment's own rendering of a document and a refusal the run recorded
// both move a total by a fraction of a percent, and a seed that is 1% light is still the seed
// (acceptance criterion A2 of dispatch://INFRA-988). A count the plan fixes exactly - the issues,
// the review pages, the review indexes, the part documents - is judged exactly.
const loopCheckTolerance = 0.01

// loopFound is what the database holds of one plan's seed.
type loopFound struct {
	Issues          int
	CommentRows     int
	ThreadBytes     int64
	Parts           int
	ReviewPages     int
	ReviewPageBytes int64
	ReviewIndexes   int
	// OverCap is how many of the seed's documents are past a cap one document holds: markdown
	// over loopMarkdownCap, or a comment margin over loopMarginCap. A document past both is one.
	OverCap int
}

// loopQuerier is what readLoopFound reads over: a pool or a connection, as readReplayIDs takes
// either (replay_check.go:357).
type loopQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// readLoopFound counts the seed of plan planID in the database: its issues, comment rows and
// bodies, its documents by kind, and the documents past a cap. Every count is of rows the id map
// names, so another seed in the same database counts for nothing here.
func readLoopFound(ctx context.Context, q loopQuerier, planID string) (loopFound, error) {
	var f loopFound
	// The issues of the seed that the database still holds, by the keys the id map recorded.
	if err := q.QueryRow(ctx, `
		select count(*) from issues i
		join loadtest.loop_ids m on m.local_id = i.key
		where m.plan = $1 and m.kind = 'issue'
	`, planID).Scan(&f.Issues); err != nil {
		return f, fmt.Errorf("count the seed's issues: %w", err)
	}
	// The thread roots and replies together: the comment rows the seed wrote, and the bytes their
	// bodies hold, which is the thread text the plan counted.
	if err := q.QueryRow(ctx, `
		select count(*), coalesce(sum(octet_length(c.body)), 0) from comments c
		join loadtest.loop_ids m on m.local_id = c.id::text
		where m.plan = $1 and m.kind in ('thread', 'reply')
	`, planID).Scan(&f.CommentRows, &f.ThreadBytes); err != nil {
		return f, fmt.Errorf("count the seed's comment rows: %w", err)
	}
	// Each kind of document the seed wrote, and the markdown the review pages hold at their
	// latest version.
	if err := q.QueryRow(ctx, `
		select
		  count(*) filter (where m.kind = 'part'),
		  count(*) filter (where m.kind = 'page'),
		  count(*) filter (where m.kind = 'index'),
		  coalesce(sum(octet_length(v.markdown)) filter (where m.kind = 'page'), 0)
		from loadtest.loop_ids m
		join artifacts a on a.id::text = m.local_id
		left join lateral (
		  select markdown from artifact_versions av
		  where av.artifact_id = a.id order by av.number desc limit 1
		) v on true
		where m.plan = $1 and m.kind in ('part', 'page', 'index')
	`, planID).Scan(&f.Parts, &f.ReviewPages, &f.ReviewIndexes, &f.ReviewPageBytes); err != nil {
		return f, fmt.Errorf("count the seed's documents: %w", err)
	}
	// The documents past a cap: a latest markdown over loopMarkdownCap, or a margin - every
	// comment record anchored on the document, each record a root and its replies - over
	// loopMarginCap (internal/dispatch/docs/growth.go:149-160). A margin is measured here in the
	// bytes of the bodies the seed wrote, which are ASCII.
	if err := q.QueryRow(ctx, `
		with docs as (
		  select a.id from loadtest.loop_ids m join artifacts a on a.id::text = m.local_id
		  where m.plan = $1 and m.kind in ('part', 'page', 'index')
		), heavy_markdown as (
		  select d.id from docs d
		  where (
		    select octet_length(av.markdown) from artifact_versions av
		    where av.artifact_id = d.id order by av.number desc limit 1
		  ) > $2
		), heavy_margin as (
		  select (root.anchor->>'artifact_id')::uuid as id
		  from comments c
		  join comments root on root.id = coalesce(c.reply_to, c.id)
		  where root.anchor->>'artifact_id' in (select id::text from docs)
		  group by 1 having sum(octet_length(c.body)) > $3
		)
		select count(*) from (
		  select id from heavy_markdown union select id from heavy_margin
		) past
	`, planID, loopMarkdownCap, loopMarginCap).Scan(&f.OverCap); err != nil {
		return f, fmt.Errorf("count the seed's documents past a cap: %w", err)
	}
	return f, nil
}

// loopCheckLine is one line of the check: what the plan expected, what the database holds, and the
// tolerance the difference is judged at (0 for a count that must match exactly).
type loopCheckLine struct {
	Name      string
	Expected  int64
	Found     int64
	Tolerance float64
}

// delta is how far the found number is from the expected one, as a fraction. An expected zero is
// met only by a found zero, so a found number beside it is infinitely far off rather than a
// division by zero.
func (l loopCheckLine) delta() float64 {
	if l.Expected == 0 {
		if l.Found == 0 {
			return 0
		}
		return math.Inf(1)
	}
	return float64(l.Found-l.Expected) / float64(l.Expected)
}

func (l loopCheckLine) ok() bool {
	if l.Tolerance == 0 {
		return l.Found == l.Expected
	}
	return math.Abs(l.delta()) <= l.Tolerance
}

// loopCheckLines is every check of a seed, in the order they are printed: what the plan expected
// against what the database holds.
func loopCheckLines(p *loopPlan, f loopFound) []loopCheckLine {
	return []loopCheckLine{
		{Name: "proposal issues", Expected: int64(len(p.Proposals)), Found: int64(f.Issues)},
		{Name: "comment rows", Expected: int64(p.Totals.CommentRows), Found: int64(f.CommentRows), Tolerance: loopCheckTolerance},
		{Name: "thread text bytes", Expected: p.Totals.ThreadBytes, Found: f.ThreadBytes, Tolerance: loopCheckTolerance},
		{Name: "review pages", Expected: int64(p.Totals.ReviewPages), Found: int64(f.ReviewPages)},
		{Name: "review-page bytes", Expected: p.Totals.ReviewPageBytes, Found: f.ReviewPageBytes, Tolerance: loopCheckTolerance},
		{Name: "review indexes", Expected: int64(p.Totals.ReviewIndexes), Found: int64(f.ReviewIndexes)},
		{Name: "part documents", Expected: int64(p.Totals.Parts), Found: int64(f.Parts)},
		{Name: "documents past a cap", Expected: 0, Found: int64(f.OverCap)},
	}
}

// printLoopCheck writes one line per check and then the verdict, and answers how many failed.
// rehearsal is the plan's own: a plan that holds a cut of the loop's proposals says so in the
// verdict, so a reader never takes a rehearsal's pass for the seed the issue's A2 asks about.
func printLoopCheck(w io.Writer, lines []loopCheckLine, rehearsal int) int {
	failed := 0
	for _, l := range lines {
		verdict := "ok"
		if !l.ok() {
			verdict = "FAIL"
			failed++
		}
		fmt.Fprintf(w, "%s: expected %d, found %d — %s (%+.2f%%)\n",
			l.Name, l.Expected, l.Found, verdict, l.delta()*100)
	}
	note := ""
	if rehearsal > 0 {
		note = fmt.Sprintf(" (a rehearsal of %d proposals, not the loop's whole seed)", rehearsal)
	}
	if failed == 0 {
		fmt.Fprintf(w, "loop-seed-check: all %d checks passed%s\n", len(lines), note)
	} else {
		fmt.Fprintf(w, "loop-seed-check: %d of %d checks failed%s\n", failed, len(lines), note)
	}
	return failed
}

// runLoopSeedCheck prints what a seeded database holds against what its plan expected, and exits
// non-zero when any line fails.
func runLoopSeedCheck(args []string) error {
	fs := flag.NewFlagSet("loop-seed-check", flag.ExitOnError)
	plan := fs.String("plan", "", "the plan the seed was written from (loopseed.plan.json)")
	pg := fs.String("pg", "", "direct Postgres URL of the dispatch database the seed was written to")
	_ = fs.Parse(args)

	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *plan == "" || *pg == "" {
		return fmt.Errorf("-plan and -pg are required")
	}
	if _, ok := parsePG(*pg); !ok {
		return fmt.Errorf("-pg %s is not a Postgres URL", redactPG(*pg))
	}
	p, err := readLoopPlan(*plan)
	if err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, *pg)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	found, err := readLoopFound(ctx, conn, loopPlanID(p))
	if err != nil {
		return err
	}
	lines := loopCheckLines(p, found)
	if failed := printLoopCheck(os.Stdout, lines, p.Rehearsal); failed > 0 {
		return fmt.Errorf("%d of %d checks failed", failed, len(lines))
	}
	return nil
}
