package api

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	// issueListTimingLimit and issueListTimingOffset are the page the paged listing is measured
	// on: the first page of twenty, which is what the agent command line asks for and the size
	// the push-down was written for.
	issueListTimingLimit  = 20
	issueListTimingOffset = 0
	// relatedTermsRefreshRounds and relatedTermsRefreshWrites are protocol 3's eleven rounds and
	// the hundred items each one is preceded by: twenty each of title edits, document versions,
	// comments, asks and messages.
	relatedTermsRefreshRounds = 11
	relatedTermsRefreshWrites = 20
)

// TestStatementTimings measures this package's changed statements on the seeded measurement
// database (benchtime), which DISPATCH_BENCH_DATABASE_URL names. CI never sets that variable, so
// CI never runs a measurement; search_bench_test.go and related_recall_test.go are gated the
// same way.
//
// Which protocol each statement is under, and why:
//
//   - Protocol 1 pairs, whose gain is SQL text alone and whose two sides therefore run on one
//     schema: the issue list unpaged and paged, the project list, agentLastActivity and the
//     issue's children read. None of them reaches an index or a table this change adds - the
//     listing and the project list add no migration, agentLastActivity reads
//     events_session_actor_created_at_idx, which migration 0026 built, and the children read
//     walks issues_parent_key - so main's text and this branch's are both measurable here, and
//     the pair is the committed regression check.
//
//   - Protocol 2, head text alone: listIssueAsksQueryAll, listIssueRootMessagesQuery and
//     listArtifactAsksQueryOpen, whose texts this change does not edit at all - their gain is
//     asks_issue_key (0159) and messages_issue_roots (0160), and for the artifact list the drop
//     of asks_open_artifact (0161), whose before side needs that index present. And the five
//     statements of search's document arm - searchQuery, searchQueryMeaning,
//     relatedKeywordQuery, relatedMeaningQuery and relatedProbeSQL - whose head text reads
//     document_search (0162), a table that must be absent for the before side and present for
//     the after. One database cannot be both, so each of these is measured here as its head
//     side, with its plan, and seam 7's two-commit run takes the before side by running this
//     same measurement against a database migrated from a checkout of main.
//
// Every pair's main text is an oracle this branch already holds - issue_list_equality_test.go,
// agent_activity_test.go and issue_children_test.go - read from there rather than copied a
// second time: two copies of one statement drift apart, and those oracles are what the
// package's equality tests are held to.
func TestStatementTimings(t *testing.T) {
	runner := benchtime.Open(t, os.Getenv(benchtime.DatabaseURLEnv))
	subjects := runner.Subjects()

	// The eight filters a listing request carries (listIssues' arguments), narrowed to the
	// project and nothing else: the list a dashboard opens.
	filters := []any{subjects.Project, "", "", nil, []string{}, false, []int16{}, false}
	runner.Pair(t, "the issue list, unpaged",
		benchtime.Statement{SQL: mainListIssuesQuery, Args: filters},
		benchtime.Statement{SQL: listIssuesQuery, Args: filters})
	// The paged read's before side is main's one text: main answered a paged request by reading
	// every matching issue, resolving all of them and cutting the page in Go, so the push-down
	// is measured against the read it replaced rather than against itself.
	runner.Pair(t, "the issue list, the first page of 20",
		benchtime.Statement{SQL: mainListIssuesQuery, Args: filters},
		benchtime.Statement{SQL: listIssuesPageQuery,
			Args: slices.Concat(filters, []any{issueListTimingLimit, issueListTimingOffset})})

	runner.Pair(t, "the project list",
		benchtime.Statement{SQL: mainProjectList},
		benchtime.Statement{SQL: projectListQuery})

	runner.Pair(t, "agentLastActivity",
		benchtime.Statement{SQL: mainAgentLastActivityQuery, Args: []any{subjects.Sessions}},
		benchtime.Statement{SQL: agentLastActivityQuery, Args: []any{subjects.Sessions}})

	runner.Pair(t, "the issue's children",
		benchtime.Statement{SQL: mainIssueChildrenQuery, Args: []any{subjects.Issue}},
		benchtime.Statement{SQL: issueChildrenQuery, Args: []any{subjects.Issue}})

	runner.Head(t, "listIssueAsksQueryAll",
		benchtime.Statement{SQL: listIssueAsksQueryAll, Args: []any{subjects.Issue}})
	runner.Head(t, "listIssueRootMessagesQuery",
		benchtime.Statement{SQL: listIssueRootMessagesQuery, Args: []any{subjects.Issue}})
	runner.Head(t, "listArtifactAsksQueryOpen",
		benchtime.Statement{SQL: listArtifactAsksQueryOpen, Args: []any{subjects.AnchoredDocument}})

	// Search is measured on the middling word: a word in a handful of items ranks almost
	// nothing, and the commonest word is a query nobody runs. The related-item check is measured
	// on all three at once, as its picker hands its stages the text's rarest words together.
	word := subjects.MediumWord
	searchArguments := []any{word, subjects.Project, searchDefaultLimit, firstTerm(word), headlineOptions,
		0, contracts.SearchKindDepth, searchFusionK}
	meaning := []any{subjects.QueryVector, searchMeaningFloor, subjects.EmbeddingModel}
	lexemes := []string{subjects.RareWord, subjects.MediumWord, subjects.CommonWord}
	// The weights are an idf order rather than a measured idf: the rank they decide costs the
	// same whatever the numbers are, and the check's own idf moves with the project's statistics.
	weights := []float64{2.5, 1.5, 0.5}
	quoted := make([]string, 0, len(lexemes))
	for _, lexeme := range lexemes {
		quoted = append(quoted, "'"+lexeme+"'")
	}
	relatedArguments := []any{strings.Join(quoted, " | "), subjects.Project, suggestionSearchDepth,
		subjects.RareWord, headlineOptions, 0, contracts.SearchKindDepth, searchFusionK}

	// The five statements of search's document arm are protocol 2: their head text reads
	// document_search and their before text reads the lateral over artifact_versions, and one
	// database cannot hold both. documentArmSide takes whichever the database it is pointed at
	// can run, and names the side it read, so the two-commit run is this one test against two
	// databases rather than a second harness.
	side, label := documentArmSide(t, runner)
	runner.Head(t, "searchQuery, keywords only"+label,
		benchtime.Statement{SQL: side.searchQuery, Args: searchArguments})
	runner.Head(t, "searchQueryMeaning"+label,
		benchtime.Statement{SQL: side.searchQueryMeaning, Args: slices.Concat(searchArguments, meaning)})
	runner.Head(t, "relatedKeywordQuery"+label,
		benchtime.Statement{SQL: side.relatedKeywordQuery, Args: slices.Concat(relatedArguments, []any{lexemes, weights})})
	runner.Head(t, "relatedMeaningQuery"+label, benchtime.Statement{
		SQL:  side.relatedMeaningQuery,
		Args: slices.Concat(relatedArguments, meaning, []any{lexemes, weights}),
		// The check raises hnsw.ef_search for its own transaction, so a measurement without it
		// would be of a statement production never runs (relatedEFSearch).
		Setup: []string{relatedEFSearch},
	})
	runner.Head(t, "relatedProbeSQL"+label,
		benchtime.Statement{SQL: side.relatedProbeSQL, Args: []any{subjects.Project, lexemes, "", (*string)(nil)}})
}

// documentArmStatements are the five statements whose document arm this change moved onto
// document_search, in the one spelling the database under measurement can run.
type documentArmStatements struct {
	searchQuery         string
	searchQueryMeaning  string
	relatedKeywordQuery string
	relatedMeaningQuery string
	relatedProbeSQL     string
}

// documentArmSide answers the five statements this database can run, and the label that names
// the side they are. A database migrated from a checkout of main has no document_search, so it
// can run only main's text, which is protocol 2's before side; the head's schema runs this
// branch's. The table is read from the catalogue rather than from a caught error, so a statement
// that fails for its own reason still fails the run.
func documentArmSide(t *testing.T, runner *benchtime.Runner) (documentArmStatements, string) {
	t.Helper()
	var present bool
	if err := runner.Conn().QueryRow(context.Background(), `
		select exists (
			select 1 from information_schema.tables
			where table_schema = current_schema() and table_name = 'document_search'
		)
	`).Scan(&present); err != nil {
		t.Fatalf("read whether this database holds document_search: %v", err)
	}
	if !present {
		return documentArmStatements{
			searchQuery:         mainSearchQuery,
			searchQueryMeaning:  mainSearchQueryMeaning,
			relatedKeywordQuery: mainRelatedKeywordQuery,
			relatedMeaningQuery: mainRelatedMeaningQuery,
			relatedProbeSQL:     mainRelatedProbeSQL,
		}, " (main's text, the before side)"
	}
	return documentArmStatements{
		searchQuery:         searchQuery,
		searchQueryMeaning:  searchQueryMeaning,
		relatedKeywordQuery: relatedKeywordQuery,
		relatedMeaningQuery: relatedMeaningQuery,
		relatedProbeSQL:     relatedProbeSQL,
	}, " (this branch's text, the after side)"
}

// TestRelatedTermsRefreshTiming is protocol 3, the refresh's own, and it is a Go test rather
// than a line of seam 7's script because the pass it times is Go: refreshSearchTermStats is
// unexported in this package, and nothing outside it can run one pass of this build.
//
// The refresh has no statement pair. Main's is a per-project ts_stat rebuild and this build's is
// a drain whose time follows the queue depth, so an empty queue would make it trivially faster.
// The protocol is therefore the wall time of one whole pass before (main's full rebuild of every
// project, runMainSearchTermStatsRebuild), the wall time of one whole pass after with no change,
// and eleven rounds each preceded by a hundred item writes outside the timed part - twenty each
// of title edits, document versions, comments, asks and messages - with the median of the eleven
// reported. Each timed pass after must be below the pass before.
//
// It writes 1,100 items to the database it measures and leaves them there, which is what the
// protocol asks for: run it after the statement timings, and re-seed before measuring a row
// count or a checksum. The settling pass between main's rebuild and the first timed pass is
// untimed on purpose: main's rebuild moves refreshed_at and writes no snapshot, so the pass
// after it counts every project in full by this build's own rule, and timing that would measure
// a first-use full build rather than the steady state.
func TestRelatedTermsRefreshTiming(t *testing.T) {
	runner := benchtime.Open(t, os.Getenv(benchtime.DatabaseURLEnv))
	project := runner.Subjects().Project
	database := openRefreshTimingStore(t)
	projects := refreshTimingProjects(t, database)

	before := mainRefreshPass(t, database, projects)
	t.Logf("one whole pass, main's full rebuild, %d projects: %v", len(projects), before)

	settling, settlingNote := refreshTimingPass(t, database, projects)
	t.Logf("the settling pass after main's rebuild, untimed by the protocol: %v (%s)", settling, settlingNote)

	noChange, noChangeNote := refreshTimingPass(t, database, projects)
	t.Logf("one whole pass, this build with no change: %v (%s)", noChange, noChangeNote)

	rounds := make([]time.Duration, 0, relatedTermsRefreshRounds)
	for round := range relatedTermsRefreshRounds {
		writeRefreshTimingRound(t, database, project, round)
		took, note := refreshTimingPass(t, database, projects)
		t.Logf("round %d, after %d writes to %s: %v (%s)",
			round+1, 5*relatedTermsRefreshWrites, project, took, note)
		rounds = append(rounds, took)
	}
	sorted := slices.Clone(rounds)
	slices.Sort(sorted)
	median := sorted[len(sorted)/2]
	t.Logf("the median of %d rounds of a hundred writes: %v; every round: %v",
		len(rounds), median, rounds)

	if noChange >= before {
		t.Errorf("a pass with no change took %v, not below main's whole pass at %v", noChange, before)
	}
	for round, took := range rounds {
		if took >= before {
			t.Errorf("round %d took %v, not below main's whole pass at %v", round+1, took, before)
		}
	}
}

// openRefreshTimingStore opens a pool on the measurement database. The refresh takes a pool, not
// the one connection the statement timings share: a pass runs each project in a transaction of
// its own.
func openRefreshTimingStore(t *testing.T) *store.Store {
	t.Helper()
	database, err := store.Open(context.Background(), os.Getenv(benchtime.DatabaseURLEnv))
	if err != nil {
		t.Fatalf("open the measurement database: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	return database
}

// refreshTimingProjects is every project a whole pass refreshes, in the order
// RefreshEverySearchTermStats takes them.
func refreshTimingProjects(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select key from projects order by key`)
	if err != nil {
		t.Fatalf("read the projects: %v", err)
	}
	projects := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("read the projects: %v", err)
		}
		projects = append(projects, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the projects: %v", err)
	}
	if len(projects) == 0 {
		t.Fatal("the database holds no project, so a refresh pass has nothing to count")
	}
	return projects
}

// refreshTimingPass is one whole pass of this build: its refresh of every project, timed
// together, with how many items it applied and how many projects it had to count in full. The
// note is what says a pass measured the drain and not a rebuild.
func refreshTimingPass(t *testing.T, database *store.Store, projects []string) (time.Duration, string) {
	t.Helper()
	ctx := context.Background()
	started := time.Now()
	changes, full := 0, 0
	for _, project := range projects {
		refreshed, err := refreshSearchTermStats(ctx, database.Pool, project)
		if err != nil {
			t.Fatalf("refresh %s's term statistics: %v", project, err)
		}
		changes += refreshed.Changes
		if refreshed.Mode == searchTermModeFull {
			full++
		}
	}
	took := time.Since(started)
	return took, fmt.Sprintf("%d items applied, %d of %d projects counted in full", changes, full, len(projects))
}

// mainRefreshPass is one whole pass as the release before this one runs it: its four-statement
// rebuild of every project (runMainSearchTermStatsRebuild, the oracle this package already
// holds), timed together.
func mainRefreshPass(t *testing.T, database *store.Store, projects []string) time.Duration {
	t.Helper()
	started := time.Now()
	for _, project := range projects {
		runMainSearchTermStatsRebuild(t, database, project)
	}
	return time.Since(started)
}

// writeRefreshTimingRound writes one round's hundred items: twenty title edits, twenty document
// versions, twenty comments, twenty asks and twenty messages, each row written as SQL so that
// the queue triggers fire and nothing else of a route's work is on the clock. Each round takes
// the next twenty issues and documents of the project, so no row is rewritten and every round
// queues the same number of items.
//
// A write that touched fewer rows than it asked for would make a round's queue shorter than the
// protocol's hundred, so each statement is held to its row count.
func writeRefreshTimingRound(t *testing.T, database *store.Store, project string, round int) {
	t.Helper()
	ctx := context.Background()
	offset := round * relatedTermsRefreshWrites
	for _, write := range []struct{ what, statement string }{
		{"the title edits", `
			update issues set title = title || ' ' || $4::text
			 where key in (select i.key from issues i where i.project_key = $1
			                order by i.key limit $3 offset $2)`},
		{"the document versions", `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			select v.artifact_id, max(v.number) + 1,
			       '# Bench round ' || $4::text || chr(10) || chr(10) ||
			       'The plateau ranges north and the rete needs cleaning.' || chr(10), '[]'
			  from artifact_versions v
			 where v.artifact_id in (select a.id from artifacts a
			                          where a.project_key = $1 and a.kind = 'doc'
			                          order by a.id limit $3 offset $2)
			 group by v.artifact_id`},
		{"the comments", `
			insert into comments (issue_key, author, body)
			select i.key, '{"kind":"user","id":"bench"}'::jsonb,
			       'Bench round ' || $4::text || ': the rete needs cleaning.'
			  from (select key from issues where project_key = $1 order by key limit $3 offset $2) i`},
		{"the asks", `
			insert into asks (issue_key, author, question)
			select i.key, '{"kind":"user","id":"bench"}'::jsonb,
			       'Bench round ' || $4::text || ': which quadrant do we keep?'
			  from (select key from issues where project_key = $1 order by key limit $3 offset $2) i`},
		{"the messages", `
			insert into messages (issue_key, author, body)
			select i.key, '{"kind":"user","id":"bench"}'::jsonb,
			       'Bench round ' || $4::text || ': the survey is booked.'
			  from (select key from issues where project_key = $1 order by key limit $3 offset $2) i`},
	} {
		// The round number is bound as text: every statement writes it into a body, so `$4::text`
		// fixes its type as text and pgx encodes nothing else into that.
		tag, err := database.Pool.Exec(ctx, write.statement,
			project, offset, relatedTermsRefreshWrites, strconv.Itoa(round+1))
		if err != nil {
			t.Fatalf("round %d, write %s: %v", round+1, write.what, err)
		}
		if tag.RowsAffected() != relatedTermsRefreshWrites {
			t.Fatalf("round %d, %s wrote %d rows, want %d: %s has too few issues or documents for %d rounds",
				round+1, write.what, tag.RowsAffected(), relatedTermsRefreshWrites, project, relatedTermsRefreshRounds)
		}
	}
}
