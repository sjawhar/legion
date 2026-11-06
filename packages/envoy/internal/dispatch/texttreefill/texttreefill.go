// Package texttreefill is the background pass that gives every stored text written before its
// display tree was stored one, and the retention that keeps the text_trees table to the two
// newest display parser versions (texttrees, migration 20261105121325).
//
// Every path that stores a text has stored that text's display tree beside it since INFRA-635
// (api/text_trees.go, docs/ledger.go, architecture/importer.go), so a read of it copies the
// stored bytes instead of reading the markdown again. Two sets of texts have no row even so: the
// ones written before that change - 140,622 of them, holding 5.8 GB of markdown, when it shipped
// - and, after a parser change raises pmdoc.DisplayTreeVersion, every text there is, since a
// stored tree is served only at the version that stored it. A read of one of those still reads
// the markdown and stores what it read, which is the fallback this pass does not touch; what the
// pass adds is that no reader has to be the first to open a text for its tree to exist.
//
// It is a package of its own for the reason texttrees is: the texts it reads back belong to api
// (comments, messages, broadcasts, asks and reviews), to docs (document versions) and to
// architecture (component prose), and no one of those three can import the other two. It reads
// the rows directly, which is also the only way to reach the stored texts no API list answers -
// a broadcast's body and a direct message's are two of them. It imports docs for one constant,
// docs.ReviewOrder, the order every read of artifact_reviews in this module takes its rows in.
package texttreefill

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/texttrees"
)

// fillLease names the lease one pass runs under, and fillPeriod the period one pass serves
// (duty.Exclusive): a pass reads every stored text that has no tree at this build's display
// parser version, so two envoy-dispatch processes filling, or one filling again after a restart,
// inside one aligned hour would be that whole reading twice. The period is also what paces the
// retention, which runs on every tick whether or not the fill has anything left to do.
const (
	fillLease  = "text-tree-fill"
	fillPeriod = time.Hour
)

// fillWorkers is how many of the batch's texts are read for display at once. A reading is the
// markdown parse this pass exists to spare a reader, and it is CPU the process owes its requests
// first: two keeps the pass to two cores of a pod that serves the API with the rest, which is
// also the number of connections its pool holds (store.textTreePoolSize).
const fillWorkers = 2

// fillChunk is how many texts one worker reads and stores in one statement. The chunk is the unit
// the workers balance on: a batch split in two halves would leave one worker holding the half
// that happened to carry the 3 MB document versions, and a chunk per text would be a round trip
// per text. Fifty of them is an insert of a couple of megabytes at the corpus's average length.
const fillChunk = 50

// fillBatch is how many stored texts one batch takes. Each batch costs two scans of the rows that
// hold stored texts (storedTexts), so a small batch pays those scans for every few texts; each
// also holds its texts in memory while they are read, and the corpus averages 41 KB a text, so a
// large one holds hundreds of megabytes. A thousand is about 41 MB of markdown and 141 batches
// over the corpus the change shipped against.
//
// It is a variable only so a test can make a pass take several batches over a handful of texts.
var fillBatch = 1000

// The kinds of row that hold a stored text, which is one per write path that stores a tree today:
// comments and messages (api/comment_create.go, api/messages.go), broadcasts
// (api/broadcasts.go), asks - the question, each option's label and description, the text of the
// answer and the reason of the resolution (api/asks.go, api/ask_transitions.go) - document
// versions (docs/ledger.go), review reasons (api/reviews.go) and component prose
// (architecture/importer.go).
const (
	kindComment   = "comment"
	kindMessage   = "message"
	kindBroadcast = "broadcast"
	kindAsk       = "ask"
	kindVersion   = "version"
	kindReview    = "review"
	kindComponent = "component"
)

// storedTexts lists every row that holds a stored text as the moment it was written, the kind of
// row it is and the key that reads it back. It never projects the text itself: the batch is
// chosen by time alone and the texts are read back by key afterwards (textsOf), because a sort
// over a projection carrying the markdown would detoast the whole corpus - 5.8 GB of it - on
// every batch.
//
// A row whose text is empty is left out: an empty text has no tree and no row to fill
// (pmdoc.DisplayJSON), so counting it would leave every pass with work it can never finish.
// Components carry no time of their own - an import replaces a project's whole set - so they are
// ordered by the import that wrote them, and their key is a pair.
//
// It orders nothing. wholeBatch is its one use: the whole of a time range, which a batch takes
// as it finds it.
const storedTexts = `
	select created_at, kind, owner, id from (
		select created_at, '` + kindComment + `', '', id::text from comments where body <> ''
		union all
		select created_at, '` + kindMessage + `', '', id::text from messages where body <> ''
		union all
		select created_at, '` + kindBroadcast + `', '', id::text from broadcasts where body <> ''
		union all
		select created_at, '` + kindAsk + `', '', id::text from asks
		union all
		select created_at, '` + kindVersion + `', '', id::text from artifact_versions
		 where markdown is not null and markdown <> ''
		union all
		select created_at, '` + kindReview + `', '', id::text from artifact_reviews
		 where reason is not null and reason <> ''
		union all
		select snapshot.imported_at, '` + kindComponent + `', component.project_key, component.id
		  from components component
		  join architecture_snapshots snapshot on snapshot.id = component.snapshot_id
		 where component.prose <> ''
	) stored (created_at, kind, owner, id)
`

// headOfKind is the newest fillBatch stored texts of one kind written before the cursor, as the
// moment each was written and the key that reads it back. One statement a kind rather than one
// over the union above, for two reasons: a kind's newest-first key is its own - the reviews' is
// docs.ReviewOrder, the order their events committed, because artifact_reviews.created_at is its
// transaction's start and its id is random (api's TestEveryReviewReadOrdersByReviewOrder holds
// every read of that table in the module to it) - and each statement then sorts one table's rows
// rather than a seven-way union's.
//
// What the heads decide is only where to cut. The batch itself is wholeBatch, every stored text
// in the range the cut opens, so a kind whose head is ordered by something other than time
// leaves nothing behind: a row of it the head did not name is still in that range.
var headOfKind = map[string]string{
	kindComment: `select created_at, '', id::text from comments
		 where body <> '' and created_at < $1 order by created_at desc limit $2`,
	kindMessage: `select created_at, '', id::text from messages
		 where body <> '' and created_at < $1 order by created_at desc limit $2`,
	kindBroadcast: `select created_at, '', id::text from broadcasts
		 where body <> '' and created_at < $1 order by created_at desc limit $2`,
	kindAsk: `select created_at, '', id::text from asks
		 where created_at < $1 order by created_at desc limit $2`,
	kindVersion: `select created_at, '', id::text from artifact_versions
		 where markdown is not null and markdown <> '' and created_at < $1
		 order by created_at desc limit $2`,
	kindReview: `select created_at, '', id::text from artifact_reviews
		 where reason is not null and reason <> '' and created_at < $1
		 order by ` + docs.ReviewOrder + ` limit $2`,
	kindComponent: `select snapshot.imported_at, component.project_key, component.id
		  from components component
		  join architecture_snapshots snapshot on snapshot.id = component.snapshot_id
		 where component.prose <> '' and snapshot.imported_at < $1
		 order by snapshot.imported_at desc limit $2`,
}

// wholeBatch is every stored text from the batch's cut moment up to the cursor. It and the heads
// together are how a batch is always whole: the heads say where to cut, and this takes every row
// at or after that moment, the rest of the rows written in the same microsecond as the cut
// included, so the cursor can move past that moment knowing nothing at it was left behind. A
// cursor that moved to a moment it had only half covered would strand the other half, and one
// that stopped short of a moment whose rows fill a whole batch would never move at all.
const wholeBatch = storedTexts + ` where created_at < $1 and created_at >= $2`

// Run fills the display trees of the stored texts that have none at this build's display parser
// version, and keeps the table to the two newest versions, once an aligned hour across every
// process that serves the database and across restarts (duty.Exclusive, fillLease). It runs until
// ctx ends.
//
// Its ticks are duty.RunLeased's, a ticker's of fillPeriod, the first at the moment it starts: a
// deploy that ships a parser change begins storing the new version's trees as soon as a process
// of it boots, because the pass keys on pmdoc.DisplayTreeVersionNow and finds no finished row for
// it. A pass that runs longer than the period - the first one over a filled database does, since
// it ends only when every text is stored - holds the lease for its whole length, and the ticks it
// overran are dropped. leases is required, as it is of every leased loop.
func Run(ctx context.Context, pool *store.Pool, leases duty.Leases) {
	ctx = store.WithTransactionTracking(ctx)
	duty.RunLeased(ctx, leases, fillLease, fillPeriod, duty.TickAtStart, func(ctx context.Context) {
		pass(ctx, pool)
	})
}

// pass is one tick: the retention first, then the fill. The retention runs on every tick, the
// ticks after the fill has finished included, because it is a deploy of another build and not
// this pass's own work that leaves a version behind.
func pass(ctx context.Context, pool *store.Pool) {
	fill, err := pool.TextTrees()
	if err != nil {
		slog.Error("dispatch: display tree fill", "error", err)
		return
	}
	version := pmdoc.DisplayTreeVersionNow()
	// A retention that fails is logged and the fill runs anyway: the delete is one statement an
	// hour over rows no running build reads, and holding back the fill for it would leave the
	// readers it is for paying a markdown read apiece until the next tick.
	trees, cursors, err := retain(ctx, fill, version)
	switch {
	case err != nil && ctx.Err() == nil:
		slog.Error("dispatch: display tree retention", "error", err)
	case trees > 0 || cursors > 0:
		slog.Info("dispatch: display tree retention dropped the versions before the two newest",
			"trees", trees, "cursors", cursors, "version", version)
	}
	started := time.Now()
	done, err := fillVersion(ctx, fill, version)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("dispatch: display tree fill", "error", err, "texts", done.texts, "batches", done.batches)
		}
		return
	}
	if done.texts == 0 {
		return
	}
	slog.Info("dispatch: display tree fill", "version", version, "texts", done.texts,
		"batches", done.batches, "finished", done.finished, "seconds", time.Since(started).Seconds())
}

// filled is what one call of fillVersion did: the texts it read back and stored the trees of, the
// batches it took them in, and whether it reached the end of the corpus rather than being cut
// short by ctx. texts counts the texts the pass took, not the rows it wrote: a text whose row
// another process wrote meanwhile is one this pass took and the insert dropped.
type filled struct {
	texts    int
	batches  int
	finished bool
}

// fillVersion stores the display tree of every stored text that has none at version, newest
// first, until it runs out of texts or ctx ends.
//
// Newest first is the order a reader is most likely to ask in: the comment written an hour ago is
// read today, and the one written two years ago is read when somebody opens that issue. It is
// also what makes the cursor one timestamp: text_tree_rebuild.through is the moment the pass has
// reached, every stored text written at or after it being stored already, so a process that
// starts again after a restart, a deploy or a shutdown takes the next batch below that moment
// instead of reading the whole corpus again.
//
// A text written while the pass runs is above the cursor and no batch of this pass takes it: the
// write that stored it stored its tree, and a write by a process of the build before this one -
// which a rolling deploy runs beside it - is stored by the first read of it, exactly as it was
// before this pass existed.
func fillVersion(ctx context.Context, fill *store.Pool, version int) (filled, error) {
	through, finished, err := readCursor(ctx, fill, version)
	if err != nil {
		return filled{}, err
	}
	if finished {
		return filled{finished: true}, nil
	}
	table := texttrees.New(fill)
	var done filled
	for {
		if ctx.Err() != nil {
			return done, nil
		}
		batch, err := fillOneBatch(ctx, fill, table, version, through)
		done.texts += batch.texts
		if err != nil {
			return done, err
		}
		done.batches++
		if batch.last {
			done.finished = true
			return done, nil
		}
		through = batch.through
	}
}

// batched is what one batch did: how many texts it took, the moment its cursor now stands at,
// and whether it was the last batch there is to take.
type batched struct {
	texts   int
	through time.Time
	last    bool
}

// fillOneBatch takes the newest batch of stored texts written before through, stores the display
// tree of each, and only then moves the cursor, so a process that stops anywhere inside the batch
// takes that same batch again rather than leaving a text behind it with no tree. Writing a row
// twice costs the reading and nothing else: the insert keeps the row already stored.
func fillOneBatch(ctx context.Context, fill *store.Pool, table texttrees.Table, version int, through time.Time) (batched, error) {
	texts, cut, last, err := nextBatch(ctx, fill, through)
	if err != nil {
		return batched{through: through}, err
	}
	if err := storeTexts(ctx, table, texts); err != nil {
		return batched{through: through}, err
	}
	done := batched{texts: len(texts), through: cut, last: last}
	if last {
		if _, err := fill.Exec(ctx,
			`update text_tree_rebuild set done = true where version = $1`, version); err != nil {
			return done, fmt.Errorf("record the display tree fill of version %d as finished: %w", version, err)
		}
		return done, nil
	}
	if _, err := fill.Exec(ctx,
		`update text_tree_rebuild set through = $2 where version = $1`, version, cut); err != nil {
		return done, fmt.Errorf("move the display tree fill's cursor to %s: %w", cut, err)
	}
	return done, nil
}

// readCursor answers how far the pass for version has reached and whether it has finished,
// writing the row at the first pass of a version. A new row starts at the moment it is written,
// not at the oldest text: every text written from then on has its tree stored by the write that
// stored it, so the pass has nothing to do above that moment and everything below it.
//
// It takes no lock on the row. The lease is what keeps two processes out of one pass
// (duty.Exclusive, fillLease), and a second process reading this row has already lost it.
func readCursor(ctx context.Context, fill *store.Pool, version int) (through time.Time, done bool, err error) {
	if _, err := fill.Exec(ctx, `
		insert into text_tree_rebuild (version, through) values ($1, now()) on conflict do nothing
	`, version); err != nil {
		return time.Time{}, false, fmt.Errorf("open the display tree fill of version %d: %w", version, err)
	}
	if err := fill.QueryRow(ctx,
		`select through, done from text_tree_rebuild where version = $1`, version,
	).Scan(&through, &done); err != nil {
		return time.Time{}, false, fmt.Errorf("read the display tree fill's cursor at version %d: %w", version, err)
	}
	return through, done, nil
}

// nextBatch reads back the texts of the newest whole batch of stored texts written before
// through, the moment to move the cursor to, and whether this is the last batch there is.
func nextBatch(ctx context.Context, fill *store.Pool, through time.Time) (texts []string, cut time.Time, last bool, err error) {
	head, err := headCandidates(ctx, fill, through)
	if err != nil {
		return nil, time.Time{}, false, err
	}
	if len(head) < fillBatch {
		// Every head was shorter than a batch, so between them they hold every stored text left
		// and there is nothing below them to come back for.
		texts, err := textsOf(ctx, fill, head)
		return texts, through, true, err
	}
	cut = head[fillBatch-1].at
	whole, err := readCandidates(ctx, fill, "", wholeBatch, through, cut)
	if err != nil {
		return nil, time.Time{}, false, err
	}
	texts, err = textsOf(ctx, fill, whole)
	return texts, cut, false, err
}

// headCandidates is every kind's head (headOfKind) under one cursor, newest first across the
// kinds. It is read for its fillBatch-th row alone, the moment to cut the batch at: a cut taken
// from the merged heads is a moment the batch below it is about a batch's worth of rows, and it
// is always below the cursor, which is what moves the pass on.
//
// A kind whose head is ordered by something other than time - the reviews' is - can name a row
// older than that cut and leave a newer one out. Neither matters here: the rows the batch takes
// are wholeBatch's, every stored text in the range the cut opens.
func headCandidates(ctx context.Context, fill *store.Pool, through time.Time) ([]candidate, error) {
	var head []candidate
	for kind, query := range headOfKind {
		found, err := readCandidates(ctx, fill, kind, query, through, fillBatch)
		if err != nil {
			return nil, err
		}
		head = append(head, found...)
	}
	slices.SortFunc(head, func(left, right candidate) int { return right.at.Compare(left.at) })
	return head, nil
}

// candidate is one row that holds a stored text: when it was written, the kind of row it is and
// the key that reads it back - the row's id, and for a component the project it belongs to too.
type candidate struct {
	at    time.Time
	kind  string
	owner string
	id    string
}

// readCandidates runs one batch query and drains it, so no cursor of this pool is open while the
// batch's texts are read back and stored. A query of one kind (headOfKind) is named by that
// kind and answers the moment and the key alone; the query over every kind at once (wholeBatch)
// is named by the empty kind and answers each row's own.
func readCandidates(ctx context.Context, fill *store.Pool, kind, query string, args ...any) ([]candidate, error) {
	rows, err := fill.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list the stored texts to fill: %w", err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		found := candidate{kind: kind}
		if kind == "" {
			return found, row.Scan(&found.at, &found.kind, &found.owner, &found.id)
		}
		return found, row.Scan(&found.at, &found.owner, &found.id)
	})
	if err != nil {
		return nil, fmt.Errorf("list the stored texts to fill: %w", err)
	}
	return found, nil
}

// textsOf reads back every stored text the candidates name, one query a kind: the batch is chosen
// by time over every kind at once, and read back by primary key a kind at a time.
func textsOf(ctx context.Context, fill *store.Pool, found []candidate) ([]string, error) {
	ids := map[string][]string{}
	owners := map[string][]string{}
	for _, one := range found {
		ids[one.kind] = append(ids[one.kind], one.id)
		owners[one.kind] = append(owners[one.kind], one.owner)
	}
	texts := make([]string, 0, len(found))
	for kind, keys := range ids {
		read, err := textsOfKind(ctx, fill, kind, owners[kind], keys)
		if err != nil {
			return nil, err
		}
		texts = append(texts, read...)
	}
	return texts, nil
}

// textsOfKind reads back the stored texts of one kind of row by key. A row a delete took between
// the batch's listing and this read is simply absent, and so is the text it held.
func textsOfKind(ctx context.Context, fill *store.Pool, kind string, owners, ids []string) ([]string, error) {
	switch kind {
	case kindComment:
		return textColumn(ctx, fill, kind, `select body from comments where id = any($1::uuid[])`, ids)
	case kindMessage:
		return textColumn(ctx, fill, kind, `select body from messages where id = any($1::uuid[])`, ids)
	case kindBroadcast:
		return textColumn(ctx, fill, kind, `select body from broadcasts where id = any($1::uuid[])`, ids)
	case kindVersion:
		return textColumn(ctx, fill, kind, `select markdown from artifact_versions where id = any($1::uuid[])`, ids)
	case kindReview:
		return textColumn(ctx, fill, kind, `select reason from artifact_reviews where id = any($1::uuid[])`, ids)
	case kindComponent:
		return textColumn(ctx, fill, kind, `
			select prose from components
			 where (project_key, id) in (select * from unnest($1::text[], $2::text[]))
		`, owners, ids)
	case kindAsk:
		return askTexts(ctx, fill, ids)
	}
	return nil, fmt.Errorf("read the stored texts of %q: no such kind of row", kind)
}

// textColumn reads one text column of one kind of row.
func textColumn(ctx context.Context, fill *store.Pool, kind, query string, args ...any) ([]string, error) {
	rows, err := fill.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read the stored texts of %q: %w", kind, err)
	}
	texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the stored texts of %q: %w", kind, err)
	}
	return texts, nil
}

// askTexts reads back every stored text an ask holds: its question, the label and description of
// each of its options, the text of the answer it carries and the reason it was resolved with,
// which is the list a reader of an ask is shown (api's askReadDisplayTexts). The three jsonb
// columns are decoded here rather than read as columns because that is how the row holds them.
//
// A column this process cannot decode is logged and skipped, and the rest of the ask is stored:
// the pass must not stop on one row, since a pass that fails on it would take the same batch
// again on every tick and never reach the texts below it.
func askTexts(ctx context.Context, fill *store.Pool, ids []string) ([]string, error) {
	rows, err := fill.Query(ctx, `
		select id::text, question, options, answer, resolution from asks where id = any($1::uuid[])
	`, ids)
	if err != nil {
		return nil, fmt.Errorf("read the stored texts of %q: %w", kindAsk, err)
	}
	defer rows.Close()
	var texts []string
	for rows.Next() {
		var id, question string
		var options, answer, resolution []byte
		if err := rows.Scan(&id, &question, &options, &answer, &resolution); err != nil {
			return nil, fmt.Errorf("read the stored texts of %q: %w", kindAsk, err)
		}
		texts = append(texts, question)
		var carried []model.AskOption
		if _, err := decodeAskPart(options, &carried); err != nil {
			slog.Warn("dispatch: display tree fill read an ask's options", "ask", id, "error", err)
		}
		for _, option := range carried {
			texts = append(texts, option.Label, option.Description)
		}
		var given model.AskAnswer
		if _, err := decodeAskPart(answer, &given); err != nil {
			slog.Warn("dispatch: display tree fill read an ask's answer", "ask", id, "error", err)
		} else if given.Text != nil {
			texts = append(texts, *given.Text)
		}
		var resolved model.AskResolution
		if held, err := decodeAskPart(resolution, &resolved); err != nil {
			slog.Warn("dispatch: display tree fill read an ask's resolution", "ask", id, "error", err)
		} else if held {
			texts = append(texts, resolved.Reason)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the stored texts of %q: %w", kindAsk, err)
	}
	return texts, nil
}

// decodeAskPart decodes one of an ask's jsonb columns and reports whether the column held one:
// an unanswered ask's answer and an open ask's resolution are null, and the value it was given
// is left alone for those.
func decodeAskPart(encoded []byte, into any) (bool, error) {
	if len(encoded) == 0 || string(encoded) == "null" {
		return false, nil
	}
	return true, json.Unmarshal(encoded, into)
}

// storeTexts reads the display tree of each of texts and stores it, fillWorkers readings at a
// time, in chunks of fillChunk. The reading is pmdoc.DisplayTreeToStore, which fills no cache of
// this process's: the display cache holds the trees readers were served, and a pass that put its
// own 140,000 readings through it would evict every one of them. What that reading does take
// from the cache is a tree the cache already holds, which costs a reader nothing and saves this
// pass the parse of a text a reader of this process has been shown.
func storeTexts(ctx context.Context, table texttrees.Table, texts []string) error {
	if len(texts) == 0 {
		return nil
	}
	chunks := make(chan []string)
	group, groupCtx := errgroup.WithContext(ctx)
	for range fillWorkers {
		group.Go(func() error {
			for chunk := range chunks {
				if err := table.Store(groupCtx, pmdoc.DisplayLatest, chunk...); err != nil {
					return fmt.Errorf("store a batch of display trees: %w", err)
				}
			}
			return nil
		})
	}
	group.Go(func() error {
		defer close(chunks)
		for start := 0; start < len(texts); start += fillChunk {
			select {
			case chunks <- texts[start:min(start+fillChunk, len(texts))]:
			case <-groupCtx.Done():
				return nil
			}
		}
		return nil
	})
	return group.Wait()
}

// retainQuery deletes every row older than the two newest display parser versions the table
// holds, and the cursor of each version it dropped.
//
// Two versions, not one, because a rolling deploy runs two builds at once and a rollback runs the
// build before this one again. Rows of the version the other build reads are rows it would
// otherwise miss on, and every miss stores the row back (api's readDisplayTrees): deleting them
// would cost those pods a markdown read per text and undo the delete at the same time, and a pass
// of that build would start again and never end while this one kept deleting what it wrote.
// Older versions than those two no running build can ask for, since no deploy rolls back past the
// build before this one.
//
// The bound is never above the version this build runs at: a build two versions behind the newest
// rows in the table would otherwise delete the rows its own pass is writing, which is the endless
// pass again. least ignores a null, so an empty table bounds it at this build's version and
// deletes nothing of it.
const retainQuery = `
	with newest as (
		select distinct version from text_trees order by version desc limit 2
	), oldest as (
		select least(min(version), $1::integer) as version from newest
	), trees as (
		delete from text_trees where version < (select version from oldest) returning 1
	), cursors as (
		delete from text_tree_rebuild where version < (select version from oldest) returning 1
	)
	select (select count(*) from trees), (select count(*) from cursors)
`

// retain runs the retention and answers the trees and the cursors it deleted.
func retain(ctx context.Context, fill *store.Pool, version int) (trees, cursors int64, err error) {
	if err := fill.QueryRow(ctx, retainQuery, version).Scan(&trees, &cursors); err != nil {
		return 0, 0, fmt.Errorf("drop the display trees of the versions before the two newest: %w", err)
	}
	return trees, cursors, nil
}
