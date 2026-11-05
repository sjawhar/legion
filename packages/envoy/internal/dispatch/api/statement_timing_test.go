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
//   - Protocol 1 pairs, whose two sides one schema runs: the issue list unpaged and paged, the
//     project list, agentLastActivity, the issue's children, and the five statements of
//     search's document arm - searchQuery, searchQueryMeaning, relatedKeywordQuery,
//     relatedMeaningQuery and relatedProbeSQL. The listing and the project list add no
//     migration, agentLastActivity reads events_session_actor_created_at_idx, which migration
//     0026 built, the children read walks issues_parent_key, and the document arm's older text
//     reads artifact_versions, a table this release's schema still holds; so on a database at
//     this release's schema both texts of each are measurable, and the pair is the committed
//     regression check. The two related statements are gated on the document arm they share
//     and reported whole beside it, for the reason given where that pair is measured.
//
//   - Protocol 2, one side alone: listIssueAsksQueryAll, listIssueRootMessagesQuery and
//     listArtifactAsksQueryOpen, whose texts this release does not edit at all - their gain is
//     asks_issue_key (20261101182426) and messages_issue_roots (20261101182433), and for the
//     artifact list the drop of asks_open_artifact (20261101182440), whose before side needs
//     that index present. No one schema holds both sides of those, so each is measured alone,
//     with its plan: on a database at this release's schema that reading is the after side, and
//     on one migrated from the release before it the same three readings are the before side,
//     which is what the after side is compared with. That older database also has no
//     document_search (20261101182447), so this release's text of the five document-arm
//     statements cannot run on it: there each of the five is measured as its before text alone,
//     under the name it carries here - a pair's name for the three gated whole, and a Report's
//     name for the two related statements that are gated on the arm they share.
//
// Every before text is the text the release before this change runs, which this package already
// holds as an oracle - issue_list_equality_test.go, agent_activity_test.go,
// issue_children_test.go and document_search_oracle_test.go - read from there rather than
// copied a second time: two copies of one statement drift apart, and those oracles are what the
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
	// The paged read's before side is the one text the release before this change had: it
	// answered a paged request by reading every matching issue, resolving all of them and
	// cutting the page in Go, so the push-down is measured against the read it replaced rather
	// than against itself.
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
		benchtime.Statement{SQL: issueChildrenStatement, Args: []any{subjects.Issue}})

	// Which side these three readings are is the schema's answer, not the statement's: their
	// text is the same on both databases, and what differs is whether the indexes a migration
	// of this release adds are there. The same answer chooses the document arm's branch below,
	// so it is read once.
	thisReleasesSchema := databaseHoldsDocumentSearch(t, runner)
	oneSide := runner.After
	if !thisReleasesSchema {
		oneSide = runner.Before
	}
	oneSide(t, "listIssueAsksQueryAll",
		benchtime.Statement{SQL: listIssueAsksQueryAll, Args: []any{subjects.Issue}})
	oneSide(t, "listIssueRootMessagesQuery",
		benchtime.Statement{SQL: listIssueRootMessagesQuery, Args: []any{subjects.Issue}})
	oneSide(t, "listArtifactAsksQueryOpen",
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
	lexemeQuery := strings.Join(quoted, " | ")
	relatedArguments := []any{lexemeQuery, subjects.Project, suggestionSearchDepth,
		subjects.RareWord, headlineOptions, 0, contracts.SearchKindDepth, searchFusionK}

	// The check raises hnsw.ef_search for its own transaction, so a measurement of its meaning
	// query without it would be of a statement production never runs (relatedEFSearch).
	meaningSetup := []string{relatedEFSearch}
	searchMeaningArguments := slices.Concat(searchArguments, meaning)
	relatedKeywordArguments := slices.Concat(relatedArguments, []any{lexemes, weights})
	relatedMeaningArguments := slices.Concat(relatedArguments, meaning, []any{lexemes, weights})
	// The probe is measured on a subject of its own, subjects.ProbeLexemes, the project's twenty
	// rarest words rather than the three the other statements are measured on. Twenty is the
	// list a question of a paragraph or two hands the picker, and rare is what it hands it: of
	// the text's words the picker probes live only those its statistics do not hold, the noise
	// and the too-common already dropped (pickRelatedTerms). On three words, and on the
	// project's commonest, every arm matches its first row at once, both texts read tens of
	// milliseconds whatever their plans are, and a committed "lower after" there decides
	// nothing. On twenty rare words the document arm is the statement's whole cost, which is
	// where the two texts differ: six runs of the pair on the 15 GB database spanned 8.8 s to
	// 10.4 s before and 19 ms to 31 ms after.
	//
	// Every write that runs the check excludes its own owner (relatedExclusion), so the probe is
	// measured with the subject issue excluded: with nothing excluded each arm's exists stops at
	// the first row it meets, which is not the statement production runs.
	probeArguments := []any{subjects.Project, subjects.ProbeLexemes, subjects.Issue, (*string)(nil)}

	// The five statements of search's document arm are measured as pairs on a database at this
	// release's schema: this release's text of the arm reads document_search, the release
	// before's reads a lateral over artifact_versions, and that schema holds both, so the two
	// texts run alternately on one connection and the runner fails a pair whose after median is
	// not below its before. A database migrated from the release before holds no
	// document_search (migration 20261101182447), so this release's text cannot run there at
	// all: each before text is measured alone instead, under the name it carries here, and the
	// reading to compare it with is this schema's own reading of the same before text, which
	// every pair below logs beside its after text. The two-commit run is therefore this one
	// test against two databases rather than a second harness.
	if !thisReleasesSchema {
		runner.Before(t, "searchQuery, keywords only",
			benchtime.Statement{SQL: mainSearchQuery, Args: searchArguments})
		runner.Before(t, "searchQueryMeaning",
			benchtime.Statement{SQL: mainSearchQueryMeaning, Args: searchMeaningArguments})
		runner.Before(t, "relatedKeywordQuery, the whole statement",
			benchtime.Statement{SQL: mainRelatedKeywordQuery, Args: relatedKeywordArguments})
		runner.Before(t, "relatedMeaningQuery, the whole statement", benchtime.Statement{
			SQL:   mainRelatedMeaningQuery,
			Args:  relatedMeaningArguments,
			Setup: meaningSetup,
		})
		runner.Before(t, "relatedProbeSQL",
			benchtime.Statement{SQL: mainRelatedProbeSQL, Args: probeArguments})
		return
	}

	runner.Pair(t, "searchQuery, keywords only",
		benchtime.Statement{SQL: mainSearchQuery, Args: searchArguments},
		benchtime.Statement{SQL: searchQuery, Args: searchArguments})
	runner.Pair(t, "searchQueryMeaning",
		benchtime.Statement{SQL: mainSearchQueryMeaning, Args: searchMeaningArguments},
		benchtime.Statement{SQL: searchQueryMeaning, Args: searchMeaningArguments})
	// relatedKeywordQuery and relatedMeaningQuery are gated on the document arm they share
	// rather than on either whole statement. The arm is the piece whose text differs, and it is
	// a fraction of a second of a statement of about ten, whose other four arms, fusion, page
	// and snippet are the same text on both sides: what the arm saves sits inside the run-to-run
	// spread of that unchanged remainder, and readings of the whole statement have come out both
	// ways round - on this database the after text read 10.9 s against the before text's 10.7 s
	// while its document arm read 160 ms against 287 ms. A pair over the whole statement would
	// therefore pass or fail on which way the machine drifted and would gate nothing, so what is
	// gated is the arm itself, both texts measured with the same subject on this database in the
	// smallest statement that runs one.
	//
	// The two statements are built from the same keyword legs, so that arm is one text and one
	// reading, measured once under a name that says whose it is: two gates over one text would
	// double the run and could disagree under the very spread above. Each whole statement is
	// reported beside it, both texts and no bound, which is the end-to-end number a reader of
	// the gate asks for next.
	documentLegArguments := []any{lexemeQuery, subjects.Project, lexemes, weights}
	runner.Pair(t, "the document arm relatedKeywordQuery and relatedMeaningQuery share",
		benchtime.Statement{
			SQL:  documentLegStatement(mainRelatedDocumentKeywordLeg, mainRelatedTermWeights),
			Args: documentLegArguments,
		},
		benchtime.Statement{
			SQL:  documentLegStatement(relatedDocumentKeywordLeg, relatedTermWeights),
			Args: documentLegArguments,
		})
	runner.Report(t, "relatedKeywordQuery, the whole statement",
		benchtime.Statement{SQL: mainRelatedKeywordQuery, Args: relatedKeywordArguments},
		benchtime.Statement{SQL: relatedKeywordQuery, Args: relatedKeywordArguments})
	runner.Report(t, "relatedMeaningQuery, the whole statement",
		benchtime.Statement{SQL: mainRelatedMeaningQuery, Args: relatedMeaningArguments, Setup: meaningSetup},
		benchtime.Statement{SQL: relatedMeaningQuery, Args: relatedMeaningArguments, Setup: meaningSetup})

	runner.Pair(t, "relatedProbeSQL",
		benchtime.Statement{SQL: mainRelatedProbeSQL, Args: probeArguments},
		benchtime.Statement{SQL: relatedProbeSQL, Args: probeArguments})
}

// documentLegStatement is the smallest statement that runs one keyword document arm and nothing
// else: the check's own q, the term weights the arm's rank reads, the arm as a CTE of its own,
// and a count over it. Both sides of the measurement are built by this one function, from the
// arm and the term weights of the release that arm belongs to, so the two readings differ in
// the arm and in nothing else.
//
// q is materialized, as the two statements this arm is taken from materialize it by reading it
// from each of their five arms. Inlined, which is what Postgres does to a query CTE one arm
// reads, the tsquery becomes a constant the planner can take the arm's selectivity from, and
// the arm is then run under a plan neither statement runs.
//
// A count asks for no rank, so the reading is of the rows the arm finds and of the joins that
// decide them, which is what this change moved; the rank over those rows is one expression,
// the same on both sides. The term weights are still each side's own, since the arm's text
// reads them.
//
// The parameters are numbered for this statement rather than for the check's, whose $3 and $5
// to $8 only the parts left out here read: Postgres cannot parse a statement whose parameters
// have a gap, having no type for the one nothing reads. q.term is a literal for the same
// reason, no arm reading it.
//
// Parameters: $1 the tsquery, $2 the project, $3 the chosen lexemes, $4 their idf.
func documentLegStatement(documentLeg string, termWeights func(lexemes int) string) string {
	return `
with q as materialized (select $1::tsquery as tsq, ''::text as term, ''::text as own_key),
` + termWeights(3) + `document_arm as (
` + documentLeg + `
)
select count(*) from document_arm`
}

// relatedDocumentKeywordLeg is the document arm of the related-item check's keyword legs alone:
// searchDocumentKeywordLeg under relatedCoordinationRank, the rank relatedKeywordQuery and
// relatedMeaningQuery rank their arms by, which makes it the after side of that arm's pair. It
// is built here rather than beside those two statements because this measurement is its only
// reader, and a server would otherwise build it at start for nothing.
var relatedDocumentKeywordLeg = searchDocumentKeywordLeg(relatedCoordinationRank)

// databaseHoldsDocumentSearch answers whether the database under measurement holds
// document_search (migration 20261101182447), which is what decides how the five document-arm
// statements are measured: a database migrated from the release before this change runs only
// that release's text of them, and a database at this release's schema runs both texts. The
// table is read from the catalogue rather than from a caught error, so a statement that fails
// for its own reason still fails the run.
func databaseHoldsDocumentSearch(t *testing.T, runner *benchtime.Runner) bool {
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
	return present
}

// TestRelatedTermsRefreshTiming is protocol 3, the refresh's own, and it is a Go test rather
// than a measurement script because the pass it times is Go: refreshSearchTermStats is
// unexported in this package, and nothing outside it can run one pass of this build.
//
// The refresh has no statement pair. The released refresh is a per-project ts_stat rebuild and
// this build's is a drain whose time follows the queue depth, so an empty queue would make it
// trivially faster. The protocol is therefore the wall time of one whole pass before (the
// released full rebuild of every project, runMainSearchTermStatsRebuild), the wall time of one
// whole pass after with no change, and eleven rounds each preceded by a hundred item writes
// outside the timed part - twenty each of title edits, document versions, comments, asks and
// messages - with the median of the eleven reported. Each timed pass after must be below the
// pass before.
//
// It writes 1,100 items to the database it measures and leaves them there, which is what the
// protocol asks for: run it after the statement timings, and re-seed before measuring a row
// count or a checksum. The settling pass between the released rebuild and the first timed pass
// is untimed on purpose: that rebuild moves refreshed_at and writes no snapshot, so the pass
// after it counts every project in full by this build's own rule, and timing that would measure
// a first-use full build rather than the steady state.
func TestRelatedTermsRefreshTiming(t *testing.T) {
	runner := benchtime.Open(t, os.Getenv(benchtime.DatabaseURLEnv))
	project := runner.Subjects().Project
	database := openRefreshTimingStore(t)
	projects := refreshTimingProjects(t, database)

	before := mainRefreshPass(t, database, projects)
	t.Logf("one whole pass, the released full rebuild, %d projects: %v", len(projects), before)

	settling, settlingNote := refreshTimingPass(t, database, projects)
	t.Logf("the settling pass after the released rebuild, untimed by the protocol: %v (%s)", settling, settlingNote)

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
		t.Errorf("a pass with no change took %v, not below the released whole pass at %v", noChange, before)
	}
	for round, took := range rounds {
		if took >= before {
			t.Errorf("round %d took %v, not below the released whole pass at %v", round+1, took, before)
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
