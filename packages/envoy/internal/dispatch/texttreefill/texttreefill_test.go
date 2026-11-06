package texttreefill

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/texttrees"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// openFill is a migrated database of this test's own and the pool the pass works through.
func openFill(t *testing.T) (*store.Store, *store.Pool) {
	t.Helper()
	database := storetest.Open(t)
	fill, err := database.Pool.TextTrees()
	if err != nil {
		t.Fatalf("open the display tree fill's pool: %v", err)
	}
	return database, fill
}

// seed runs each statement against the database, failing the test on the first that will not run.
// Each is sent on its own with no arguments, which is how a statement holding several of them
// reaches Postgres whole.
func seed(t *testing.T, database *store.Store, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
}

// storedTreeOf is the tree the table holds for text in the latest display mode, and whether it
// holds one at all.
func storedTreeOf(t *testing.T, fill *store.Pool, text string) ([]byte, bool) {
	t.Helper()
	stored, err := texttrees.New(fill).Read(context.Background(), pmdoc.DisplayLatest, [][32]byte{texttrees.Sum(text)})
	if err != nil {
		t.Fatalf("read the stored tree of %.40q: %v", text, err)
	}
	tree, held := stored[texttrees.Sum(text)]
	return tree, held
}

// withFillBatch stands the pass at a batch size of its own for one test, so a handful of seeded
// texts takes the several batches a corpus takes.
func withFillBatch(t *testing.T, size int) {
	t.Helper()
	was := fillBatch
	fillBatch = size
	t.Cleanup(func() { fillBatch = was })
}

// The stored texts of one of every kind of row that holds one, each distinct so that each has a
// row of its own in the table.
const (
	commentBody       = "The *staging* run is green; see [the log](https://example.test/log)."
	messageBody       = "**Hold** the deploy until the migration lands."
	broadcastBody     = "# Maintenance\n\nThe database is read-only for ten minutes.\n"
	askQuestion       = "Ship the ~~draft~~ release today?"
	askOptionLabel    = "Ship it"
	askOptionDetail   = "Roll the release out to **every** pod."
	askAnswerText     = "Ship it, but hold the `--force` flag."
	withdrawnQuestion = "Should the old index be dropped in the same deploy?"
	resolutionReason  = "Answered in [the thread](https://example.test/thread)."
	versionMarkdown   = "## The plan\n\n1. Migrate the table.\n2. Deploy the readers.\n"
	reviewReason      = "The migration needs a `down` of its own."
	componentProse    = "The **front door** terminates TLS and routes by host."
)

// seedOneOfEveryKind writes one row of every kind of row that holds a stored text, by direct SQL
// and not through a write path, which is what a text written before this table existed is.
func seedOneOfEveryKind(t *testing.T, database *store.Store) []string {
	t.Helper()
	const artifactID = "11111111-1111-4111-8111-111111111111"
	const author = `'{"kind":"user","id":"alice"}'`
	seed(t, database,
		`insert into projects (key, name) values ('TEST', 'Test')`,
		`insert into issues (key, project_key, number, title, created_by, rank)
			values ('TEST-1', 'TEST', 1, 'Test issue', `+author+`, 'U')`,
		`insert into comments (issue_key, author, body) values ('TEST-1', `+author+`, '`+commentBody+`')`,
		`insert into messages (issue_key, author, body) values ('TEST-1', `+author+`, '`+messageBody+`')`,
		`insert into broadcasts (author, body, delivery) values (`+author+`, '`+broadcastBody+`', 'all')`,
		`insert into asks (issue_key, author, question, options, state, answer) values (
			'TEST-1', `+author+`, '`+askQuestion+`',
			'[{"id":"o1","label":"`+askOptionLabel+`","description":"`+askOptionDetail+`"}]',
			'answered',
			'{"user":"alice","selected":["o1"],"text":"`+askAnswerText+`","at":"2026-11-01T00:00:00Z"}')`,
		`insert into asks (issue_key, author, question, state, resolution) values (
			'TEST-1', `+author+`, '`+withdrawnQuestion+`', 'resolved',
			'{"kind":"withdrawn","reason":"`+resolutionReason+`","actor":{"kind":"user","id":"alice"},"at":"2026-11-01T00:00:00Z"}')`,
		`insert into artifacts (id, issue_key, project_key, slug, name, kind, created_by)
			values ('`+artifactID+`', 'TEST-1', 'TEST', 'plan', 'Plan', 'doc', `+author+`)`,
		`insert into artifact_versions (artifact_id, number, markdown)
			values ('`+artifactID+`', 1, '`+versionMarkdown+`')`,
		`insert into artifact_reviews (artifact_id, version, state, actor, reason)
			values ('`+artifactID+`', 1, 'changes_requested', `+author+`, '`+reviewReason+`')`,
		`insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
			values ('TEST', 'acme/repo', 'main', 1, `+author+`)`,
		`insert into architecture_snapshots (project_key, commit, files)
			values ('TEST', 'deadbeef', '{}')`,
		`insert into components (project_key, id, title, prose, snapshot_id, source_id)
			select 'TEST', 'front-door', 'Front door', '`+componentProse+`', snapshot.id, source.id
			  from architecture_snapshots snapshot, architecture_sources source
			 where snapshot.project_key = 'TEST' and source.project_key = 'TEST'`,
	)
	return []string{
		commentBody, messageBody, broadcastBody,
		askQuestion, askOptionLabel, askOptionDetail, askAnswerText,
		withdrawnQuestion, resolutionReason,
		versionMarkdown, reviewReason, componentProse,
	}
}

// Every kind of row that holds a stored text is one the pass reaches, the two the API's census
// cannot see among them: a broadcast's body and a direct message's. Each text's stored tree is
// the bytes a reader is served, so a read of it after the pass copies them and parses nothing.
func TestTheFillStoresTheTreeOfEveryKindOfStoredText(t *testing.T) {
	ctx := context.Background()
	database, fill := openFill(t)
	texts := seedOneOfEveryKind(t, database)

	done, err := fillVersion(ctx, fill, pmdoc.DisplayTreeVersionNow())
	if err != nil {
		t.Fatalf("fill the seeded texts: %v", err)
	}
	if !done.finished {
		t.Errorf("the pass did not finish; it took %d texts in %d batches", done.texts, done.batches)
	}
	if done.texts != len(texts) {
		t.Errorf("the pass took %d texts, want the %d seeded", done.texts, len(texts))
	}
	for _, text := range texts {
		tree, held := storedTreeOf(t, fill, text)
		if !held {
			t.Errorf("no stored tree of %q", text)
			continue
		}
		if want := pmdoc.DisplayJSON(text, pmdoc.DisplayLatest); !bytes.Equal(tree, want) {
			t.Errorf("the stored tree of %q is\n\t%s\nwant\n\t%s", text, tree, want)
		}
	}
}

// seedBroadcasts writes count broadcasts, each a minute older than the one before it, and answers
// them newest first. A broadcast carries no foreign key of its own, so a corpus of them is a
// corpus of stored texts and nothing else.
func seedBroadcasts(t *testing.T, database *store.Store, count int) []string {
	t.Helper()
	bodies := make([]string, count)
	for index := range bodies {
		bodies[index] = fmt.Sprintf("Broadcast **%d** of the seeded corpus.", index)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into broadcasts (author, body, delivery, created_at)
		select '{"kind":"user","id":"alice"}', body, 'all', now() - make_interval(mins => ordinality::int)
		  from unnest($1::text[]) with ordinality as seeded (body, ordinality)
	`, bodies); err != nil {
		t.Fatalf("seed %d broadcasts: %v", count, err)
	}
	return bodies
}

// A pass that stops - a shutdown, a deploy, a pod replaced - starts again at the cursor its last
// batch committed, and takes only the texts below it. Making the cursor's write the thing under
// test is what the negative control turns off: a pass that stores its batch and leaves the cursor
// where it was reads the whole corpus again on the next process.
func TestTheFillResumesFromItsCursorRatherThanStartingAgain(t *testing.T) {
	ctx := context.Background()
	database, fill := openFill(t)
	const seeded = 6
	seedBroadcasts(t, database, seeded)
	withFillBatch(t, 2)
	version := pmdoc.DisplayTreeVersionNow()

	// One batch, as far as a process that is stopped right after its first batch commits gets.
	through, _, err := openCursor(ctx, fill, version)
	if err != nil {
		t.Fatalf("open the cursor: %v", err)
	}
	first, err := fillOneBatch(ctx, fill, texttrees.New(fill), version, through)
	if err != nil {
		t.Fatalf("take the first batch: %v", err)
	}
	if first.texts != 2 || first.last {
		t.Fatalf("the first batch took %d texts and said last=%v, want 2 and false", first.texts, first.last)
	}

	// The process that comes back reads the cursor, not the clock.
	done, err := fillVersion(ctx, fill, version)
	if err != nil {
		t.Fatalf("fill after the restart: %v", err)
	}
	if !done.finished {
		t.Errorf("the pass after the restart did not finish")
	}
	if want := seeded - first.texts; done.texts != want {
		t.Errorf("the pass after the restart took %d texts, want the %d below the cursor: it started again rather than resuming",
			done.texts, want)
	}
}

// A finished pass is finished: the version's row says done, and a later tick takes no batch at
// all. That is what keeps an hourly loop from reading the whole corpus once an hour for the life
// of a build.
func TestTheFillEndsDoneAndTakesNoFurtherBatch(t *testing.T) {
	ctx := context.Background()
	database, fill := openFill(t)
	seedBroadcasts(t, database, 3)
	version := pmdoc.DisplayTreeVersionNow()

	done, err := fillVersion(ctx, fill, version)
	if err != nil {
		t.Fatalf("fill the seeded texts: %v", err)
	}
	if !done.finished || done.texts != 3 {
		t.Fatalf("the pass took %d texts and finished=%v, want 3 and true", done.texts, done.finished)
	}
	var finished bool
	if err := database.Pool.QueryRow(ctx,
		`select done from text_tree_rebuild where version = $1`, version).Scan(&finished); err != nil {
		t.Fatalf("read the cursor row: %v", err)
	}
	if !finished {
		t.Error("the cursor row of the finished version does not say done")
	}

	again, err := fillVersion(ctx, fill, version)
	if err != nil {
		t.Fatalf("run the finished pass again: %v", err)
	}
	if again.batches != 0 || again.texts != 0 {
		t.Errorf("the tick after the pass finished took %d texts in %d batches, want none",
			again.texts, again.batches)
	}
}

// A deploy that raises the display parser version is a deploy whose reads miss every stored tree,
// because a tree is served only at the version that read it. The pass keys on the version this
// build runs at, finds no finished row for it, and stores the new version's trees beside the old
// ones.
func TestAVersionBumpStartsAPassThatStoresTheNewVersionsTrees(t *testing.T) {
	ctx := context.Background()
	database, fill := openFill(t)
	bodies := seedBroadcasts(t, database, 3)
	was := pmdoc.DisplayTreeVersionNow()

	if _, err := fillVersion(ctx, fill, was); err != nil {
		t.Fatalf("fill at this version: %v", err)
	}
	restore := pmdoc.SetDisplayTreeVersionForTest(was + 1)
	defer restore()
	done, err := fillVersion(ctx, fill, pmdoc.DisplayTreeVersionNow())
	if err != nil {
		t.Fatalf("fill at the next version: %v", err)
	}
	if !done.finished || done.texts != len(bodies) {
		t.Fatalf("the pass at the next version took %d texts and finished=%v, want %d and true",
			done.texts, done.finished, len(bodies))
	}
	for _, body := range bodies {
		if _, held := storedTreeOf(t, fill, body); !held {
			t.Errorf("no tree of %q at the new version", body)
		}
	}
	// The version before this one keeps its rows: the pods still running it read them.
	rows := versionCounts(t, database)
	if rows[was] != len(bodies) || rows[was+1] != len(bodies) {
		t.Errorf("the table holds %v, want %d rows at each of versions %d and %d", rows, len(bodies), was, was+1)
	}
}

// versionCounts is how many rows text_trees holds at each display parser version.
func versionCounts(t *testing.T, database *store.Store) map[int]int {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(),
		`select version, count(*)::int from text_trees group by version`)
	if err != nil {
		t.Fatalf("count the stored trees by version: %v", err)
	}
	defer rows.Close()
	counts := map[int]int{}
	for rows.Next() {
		var version, count int
		if err := rows.Scan(&version, &count); err != nil {
			t.Fatalf("count the stored trees by version: %v", err)
		}
		counts[version] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("count the stored trees by version: %v", err)
	}
	return counts
}

// The retention keeps the two newest versions and deletes only what is older, with the cursor of
// every version it drops. Two, because a rolling deploy runs two builds at once: the rows of the
// build beside this one are rows it would otherwise miss on and store again.
func TestTheRetentionKeepsTheTwoNewestVersionsAndTheirCursors(t *testing.T) {
	ctx := context.Background()
	database, fill := openFill(t)
	now := pmdoc.DisplayTreeVersionNow()
	table := texttrees.New(fill)
	for _, version := range []int{now - 2, now - 1, now} {
		restore := pmdoc.SetDisplayTreeVersionForTest(version)
		if err := table.Store(ctx, pmdoc.DisplayLatest, fmt.Sprintf("A text stored at version %d.", version)); err != nil {
			t.Fatalf("store a tree at version %d: %v", version, err)
		}
		if _, err := database.Pool.Exec(ctx,
			`insert into text_tree_rebuild (version, through, done) values ($1, now(), true)`, version); err != nil {
			t.Fatalf("write the cursor of version %d: %v", version, err)
		}
		restore()
	}

	trees, cursors, err := retain(ctx, fill, now)
	if err != nil {
		t.Fatalf("run the retention: %v", err)
	}
	if trees != 1 || cursors != 1 {
		t.Errorf("the retention deleted %d trees and %d cursors, want one of each", trees, cursors)
	}
	counts := versionCounts(t, database)
	if len(counts) != 2 || counts[now] != 1 || counts[now-1] != 1 {
		t.Errorf("the table holds %v, want one row at each of versions %d and %d", counts, now-1, now)
	}
	var left int
	if err := database.Pool.QueryRow(ctx,
		`select count(*)::int from text_tree_rebuild where version = $1`, now-2).Scan(&left); err != nil {
		t.Fatalf("count the dropped version's cursor: %v", err)
	}
	if left != 0 {
		t.Errorf("the cursor of version %d survived the retention", now-2)
	}

	// Nothing is deleted twice, and a second pass of the retention finds nothing to do.
	if trees, cursors, err = retain(ctx, fill, now); err != nil {
		t.Fatalf("run the retention again: %v", err)
	}
	if trees != 0 || cursors != 0 {
		t.Errorf("the second retention deleted %d trees and %d cursors, want none", trees, cursors)
	}
}

// A build whose version is older than the two newest rows in the table keeps its own rows: the
// bound never rises above the version this build runs at. Deleting them would cost this build's
// readers a markdown read apiece and its pass would write every one of them straight back, which
// is a pass that never ends.
func TestTheRetentionNeverDeletesTheVersionThisBuildRunsAt(t *testing.T) {
	ctx := context.Background()
	database, fill := openFill(t)
	now := pmdoc.DisplayTreeVersionNow()
	table := texttrees.New(fill)
	for _, version := range []int{now, now + 1, now + 2} {
		restore := pmdoc.SetDisplayTreeVersionForTest(version)
		if err := table.Store(ctx, pmdoc.DisplayLatest, fmt.Sprintf("A text stored at version %d.", version)); err != nil {
			t.Fatalf("store a tree at version %d: %v", version, err)
		}
		restore()
	}

	trees, _, err := retain(ctx, fill, now)
	if err != nil {
		t.Fatalf("run the retention: %v", err)
	}
	if trees != 0 {
		t.Errorf("the retention deleted %d rows, want none: the two newest versions are above this build's", trees)
	}
	if counts := versionCounts(t, database); counts[now] != 1 {
		t.Errorf("the table holds %v, want the row at this build's version %d", counts, now)
	}
}

// The shape of the corpus this change shipped against: 140,622 stored texts holding 5.8 GB of
// markdown, an average of 41.2 KB a text, most of them short and a few very long - an issue
// comment is a paragraph, a document version is the whole document and every version of it is a
// text of its own. The buckets below are a mix of that average and that skew: per thousand texts,
// 600 of a few hundred bytes, 250 of a few kilobytes, 100 of tens of kilobytes, 42 of hundreds
// and 8 of megabytes, which the generator writes as 42 KB a text, since it fills each bucket's
// size with whole sentences and stops just past it.
var corpusMix = []struct {
	texts int
	size  int
}{
	{texts: 600, size: 300},
	{texts: 250, size: 3 << 10},
	{texts: 100, size: 30 << 10},
	{texts: 42, size: 300 << 10},
	{texts: 8, size: 3 << 20},
}

// The real corpus the pass has to get through, from the census this change was specified against
// (sjawhar/legion#2712, INFRA-635).
const (
	corpusTexts = 140622
	corpusBytes = 5.8e9
	corpusBound = 24 * time.Hour
)

// The pass gets through the whole corpus well inside a day at two workers. What is measured is a
// pass over a seeded corpus of corpusMix's shape - every batch, every reading, every insert the
// real pass makes - and what is extrapolated from it is the time the same pass takes over
// 140,622 texts and 5.8 GB, by text and by byte, plus the candidate scans, which are the one cost
// that grows with the corpus rather than with the batch: each batch reads every kind's head and
// then the rows of its range, so the whole pass makes 2 * 140,622 / fillBatch such passes over
// the rows that hold stored texts.
//
// Seeding a corpus of 5.8 GB to measure the same rate would take an hour of a CI run and a disk
// to put it on; the rate per byte and per text is what the bound needs, and that is measured
// here.
func TestTheFillGetsThroughTheCorpusWellWithinADayAtTwoWorkers(t *testing.T) {
	ctx := context.Background()
	database, fill := openFill(t)
	texts, bytes := seedCorpus(t, database)

	started := time.Now()
	done, err := fillVersion(ctx, fill, pmdoc.DisplayTreeVersionNow())
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("fill the seeded corpus: %v", err)
	}
	if !done.finished || done.texts != texts {
		t.Fatalf("the pass took %d texts in %d batches and finished=%v, want %d and true",
			done.texts, done.batches, done.finished, texts)
	}

	// One round of every kind's head over the seeded corpus, which is the cost a batch pays
	// twice - once to choose its cut and once to take its range - however few texts it takes.
	scanned := time.Now()
	if _, err := headMoments(ctx, fill, time.Now()); err != nil {
		t.Fatalf("time a candidate scan: %v", err)
	}
	scan := time.Since(scanned)

	byText := time.Duration(float64(elapsed) * corpusTexts / float64(texts))
	byByte := time.Duration(float64(elapsed) * corpusBytes / float64(bytes))
	scans := time.Duration(float64(scan)*corpusTexts/float64(texts)) * 2 * (corpusTexts/time.Duration(fillBatch) + 1)
	projected := max(byText, byByte) + scans

	rows, trees, total, relation := tableSize(t, database)
	if rows != texts {
		t.Errorf("the table holds %d trees, want one for each of the %d texts", rows, texts)
	}
	t.Logf("seeded %d texts, %d bytes of markdown; the pass stored them in %s with %d workers in %d batches",
		texts, bytes, elapsed.Round(time.Millisecond), fillWorkers, done.batches)
	t.Logf("measured rate: %.1f texts a second, %.2f MB of markdown a second",
		float64(texts)/elapsed.Seconds(), float64(bytes)/elapsed.Seconds()/(1<<20))
	t.Logf("text_trees after the pass: %d rows holding %d bytes of tree, %d bytes on disk in all, %d bytes of table",
		rows, trees, total, relation)
	t.Logf("extrapolated to %d texts and %.1f GB: %s by text, %s by byte, %s of candidate scans, %s in all",
		corpusTexts, corpusBytes/1e9, byText.Round(time.Second), byByte.Round(time.Second),
		scans.Round(time.Second), projected.Round(time.Second))
	if projected >= corpusBound {
		t.Errorf("the pass projects to %s over the whole corpus, want under %s", projected, corpusBound)
	}
}

// seedCorpus writes one thousand broadcasts of corpusMix's shape, newest first, and answers how
// many texts and how many bytes of markdown it wrote. They are written by direct SQL, which is
// what every text written before this table existed is: no row of text_trees comes with them.
func seedCorpus(t *testing.T, database *store.Store) (texts, written int) {
	t.Helper()
	ctx := context.Background()
	// A fixed seed, so two runs of this test measure the same corpus.
	random := rand.New(rand.NewPCG(1, 2))
	minute := 0
	for _, bucket := range corpusMix {
		for taken := 0; taken < bucket.texts; taken += corpusSeedBatch {
			bodies := make([]string, 0, corpusSeedBatch)
			ages := make([]int32, 0, corpusSeedBatch)
			for range min(corpusSeedBatch, bucket.texts-taken) {
				bodies = append(bodies, markdownOfSize(random, bucket.size))
				minute++
				ages = append(ages, int32(minute))
			}
			if _, err := database.Pool.Exec(ctx, `
				insert into broadcasts (author, body, delivery, created_at)
				select '{"kind":"user","id":"alice"}', body, 'all', now() - make_interval(mins => age)
				  from unnest($1::text[], $2::int[]) as seeded (body, age)
			`, bodies, ages); err != nil {
				t.Fatalf("seed %d texts of %d bytes: %v", len(bodies), bucket.size, err)
			}
			for _, body := range bodies {
				written += len(body)
			}
			texts += len(bodies)
		}
	}
	return texts, written
}

// corpusSeedBatch is how many texts one seeding statement carries: enough that the small buckets
// go in one statement each, few enough that the megabyte bucket does not build a statement of
// hundreds of megabytes.
const corpusSeedBatch = 8

// markdownOfSize is about size bytes of the markdown a stored text holds: paragraphs with the
// marks a comment carries, a heading every few of them, and a list. It is built from a word list
// and numbers that vary rather than one repeated phrase, so the parse it costs is the parse a
// real text costs and the trees it makes compress about as a real text's do.
func markdownOfSize(random *rand.Rand, size int) string {
	words := []string{
		"deploy", "migration", "index", "rolled", "pod", "answer", "release", "commit", "handed",
		"reviewer", "database", "connection", "cursor", "branch", "version", "document", "lease",
		"backfill", "compaction", "excerpt", "snapshot", "artifact", "broadcast", "question",
		"paragraph", "sentence", "statement", "transaction", "checkpoint", "listener", "queue",
		"schedule", "approval", "retention", "parser", "renderer", "attribute", "heading",
		"timestamp", "identifier", "fallback", "threshold", "allocation", "measurement",
	}
	word := func() string { return words[random.IntN(len(words))] }
	var text strings.Builder
	text.Grow(size + 64)
	for paragraph := 0; text.Len() < size; paragraph++ {
		switch paragraph % 7 {
		case 0:
			fmt.Fprintf(&text, "## %s %s %d\n\n", word(), word(), random.IntN(100000))
		case 3:
			for item := range 4 {
				fmt.Fprintf(&text, "%d. The %s of %d is **%s**.\n", item+1, word(), random.IntN(100000), word())
			}
			text.WriteString("\n")
		default:
			for sentence := 0; sentence < 6 && text.Len() < size; sentence++ {
				fmt.Fprintf(&text, "The %s of the %s is `%s-%d`, and [%s](https://example.test/%s/%d) is *%s*. ",
					word(), word(), word(), random.IntN(100000), word(), word(), random.IntN(100000), word())
			}
			text.WriteString("\n\n")
		}
	}
	return text.String()
}

// tableSize is what text_trees holds and what it costs: its rows, the bytes of tree in them
// before Postgres compresses any of it, everything the table owns on disk with its TOAST and its
// index, and the table's own heap.
func tableSize(t *testing.T, database *store.Store) (rows int, trees, total, relation int64) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*)::int, coalesce(sum(octet_length(tree)), 0),
		       pg_total_relation_size('text_trees'), pg_table_size('text_trees')
		  from text_trees
	`).Scan(&rows, &trees, &total, &relation); err != nil {
		t.Fatalf("read the table's size: %v", err)
	}
	return rows, trees, total, relation
}
