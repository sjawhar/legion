package docs

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// compareFlow runs flow on both sides from the same stored history and compares what each left.
func compareFlow(t *testing.T, sides [2]*equivalenceSide, document *equivalenceDocument, flow equivalenceFlow, ids flowIDs) {
	defer func() {
		for _, side := range sides {
			side.cleanUp(t, ids)
		}
	}()
	if !inParallel(t, "set up", sides, func(t *testing.T, _ int, side *equivalenceSide) {
		side.setUp(t, document, flow, ids)
	}) {
		t.FailNow()
	}
	if flow.corrupt != nil {
		// The builders write with clients of their own, so the corruption is made on one side
		// and its rows copied to the other.
		if err := flow.corrupt(context.Background(), sides[0].database, ids.artifact); err != nil {
			t.Fatal(err)
		}
		copyHistory(t, sides[0].database, sides[1].database, ids.artifact)
	}
	history := storedHistory(t, sides[0].database, ids.artifact)
	if other := storedHistory(t, sides[1].database, ids.artifact); !slices.EqualFunc(history, other, storedRow.equal) {
		t.Fatalf("the two sides start the flow from different stored histories (%d and %d rows)", len(history), len(other))
	}
	var observed [2]flowObservation
	inParallel(t, "run", sides, func(t *testing.T, index int, side *equivalenceSide) {
		observed[index] = side.run(t, document, flow, ids, history)
	})
	compareObservations(t, document, observed[0], observed[1])
	for index, side := range sides {
		observed[index].checkFlow(t, document, side.name, flow)
	}
}

// inParallel runs run on both sides at once, in subtests named for them, and reports whether both
// passed.
func inParallel(t *testing.T, name string, sides [2]*equivalenceSide, run func(*testing.T, int, *equivalenceSide)) bool {
	t.Helper()
	return t.Run(name, func(t *testing.T) {
		for index, side := range sides {
			t.Run(side.name, func(t *testing.T) {
				t.Parallel()
				run(t, index, side)
			})
		}
	})
}

// flowObservation is everything one side's flow left that the two sides compare.
type flowObservation struct {
	// Err is the error the flow returned, nil for one that committed. The sides compare it as
	// observeError describes it; checkFlow holds it to the flow's refusal with errors.Is.
	Err        error
	Operations []observedOperation
	Sequences  []int
	Write      *observedWrite
	Rows       []rowEffect
	Anchors    []string
	Text       string
	Blocks     string
	Classes    []string
	Replica    string
}

// observedError is a flow's error as the two sides compare it: its message, and each sentinel it
// is, so a side that wrapped a different sentinel under the same message still differs.
type observedError struct {
	Is      []string
	Message string
}

func observeError(err error) *observedError {
	if err == nil {
		return nil
	}
	observed := &observedError{Message: err.Error()}
	for _, sentinel := range []struct {
		name string
		err  error
	}{
		{"ErrDocNewerContent", ErrDocNewerContent},
		{"ErrDocOutsideSchema", ErrDocOutsideSchema},
		{"ErrDocSchema", ErrDocSchema},
		{"ErrDocumentTooLarge", ErrDocumentTooLarge},
		{"websocket.ErrNoChanges", websocket.ErrNoChanges},
	} {
		if errors.Is(err, sentinel.err) {
			observed.Is = append(observed.Is, sentinel.name)
		}
	}
	return observed
}

type observedOperation struct {
	Name   string
	Result string
	Events []string
}

// observedWrite is the transaction's write to the document as its last operation left it, before
// commit: the rendering it keeps for a version and the anchors it refreshed against, the credits
// and the ask bookkeeping its commit hands the room.
type observedWrite struct {
	Cold              bool
	Appended          int
	TreeMarkdown      string
	TreeOffsets       []pmdoc.BlockOffset
	TreeErr           string
	Markdown          string
	Offsets           []pmdoc.BlockOffset
	AnchorsTree       bool
	AnchorsTreeIsTree bool
	Credits           map[string]model.Actor
	CreditedBy        map[string]persistence.Version
	Actor             *model.Actor
	CreditSeq         uint64
	Added             map[string]struct{}
	Renamed           map[string]struct{}
	Carried           map[string]model.Actor
}

func observeWrite(write *liveWrite) *observedWrite {
	if write == nil {
		return nil
	}
	observed := &observedWrite{
		Cold:              write.cold,
		Appended:          len(write.versions),
		Markdown:          write.markdown,
		Offsets:           write.offsets,
		AnchorsTree:       write.anchorsTree != nil,
		AnchorsTreeIsTree: write.anchorsTree != nil && write.anchorsTree == write.tree,
		Credits:           write.credits,
		CreditedBy:        write.creditedBy,
		Actor:             write.actor,
		CreditSeq:         write.creditSeq,
		Added:             write.addedAskBlockIDs,
		Renamed:           write.renamedAskBlockIDs,
		Carried:           write.carriedAskAuthors,
	}
	if write.tree != nil {
		var err error
		if observed.TreeMarkdown, observed.TreeOffsets, err = documentMarkdownWithOffsets(write.tree); err != nil {
			observed.TreeErr = err.Error()
		}
	}
	return observed
}

// rowEffect is one doc_updates row the flow's transaction appended: the class it is stored with,
// and the document it leaves when applied, with the rows before it, to the stored document the
// flow started from.
type rowEffect struct {
	ContentChanged bool
	Marks          string
	Markdown       string
	DocumentToken  string
	BlockTokens    map[string]string
	TreeErr        string
	Inserted       []string
	Deleted        []string
}

// run is flow on the side, from history, the stored document both sides start from: in a joined
// transaction, committed when the flow succeeds, and what it left before and after the commit.
func (side *equivalenceSide) run(t *testing.T, document *equivalenceDocument, flow equivalenceFlow, ids flowIDs, history []storedRow) flowObservation {
	t.Helper()
	ctx := context.Background()
	var observed flowObservation
	if side.classes == nil {
		t.Fatal("no room of the side was loaded with the class recorder (recordClasses), which a flow that starts from the browser edit installs")
	}
	side.classes.mu.Lock()
	classesFrom := len(side.classes.classes)
	side.classes.mu.Unlock()

	tx, err := side.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the flow's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := side.service.Join(ctx, tx)
	defer ledger.Discard()
	r := &flowRun{t: t, side: side, document: document, ids: ids, ctx: joined, tx: tx, ledger: ledger, observed: &observed}
	flowErr := flow.run(r)
	observed.Err = flowErr
	write := joinedLiveWrite(joined, ids.artifact)
	observed.Write = observeWrite(write)
	// A refused flow rolls back, so the rows its transaction appended are not stored; how many it
	// appended before the refusal is the write's own (observedWrite.Appended).
	var versions []int64
	var client crdt.ClientID
	if write != nil && flowErr == nil {
		for _, version := range write.versions {
			versions = append(versions, int64(version))
		}
		client = write.clientID
	}
	if flowErr == nil {
		if err := ledger.Commit(ctx); err != nil {
			t.Fatalf("commit the flow: %v", err)
		}
	} else {
		ledger.Discard()
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("roll the refused flow back: %v", err)
		}
	}
	resident := side.service.srv.GetDoc(ids.artifact) != nil
	if resident {
		waitForLandedAppends(t, side.service, ids.artifact)
	}

	observed.Rows = side.rowEffects(t, document, ids.artifact, versions, client, history)
	observed.Anchors = side.anchors(t, ids)
	if markdown, token, err := side.service.TextWithToken(ctx, ids.artifact); err != nil {
		observed.Text = "error: " + err.Error()
	} else {
		observed.Text = token + "\n" + markdown
	}
	if blocks, err := side.service.Blocks(ctx, ids.artifact); err != nil {
		observed.Blocks = "error: " + err.Error()
	} else {
		observed.Blocks = encodeObserved(blocks)
	}

	side.classes.mu.Lock()
	recorded := slices.Clone(side.classes.classes[classesFrom:])
	side.classes.mu.Unlock()
	tagged := 0
	for _, class := range recorded {
		origin := fmt.Sprintf("%T", class.origin)
		switch published := class.origin.(type) {
		case *liveWriteOrigin:
			origin = "a published write"
			if published.marginOnly {
				tagged++
			}
		case string:
			origin = published
		}
		observed.Classes = append(observed.Classes, fmt.Sprintf("%s: recorded=%t contentChanged=%t durable=%t credited=%t",
			origin, class.found, class.class.contentChanged, class.class.durable, class.class.credit != nil))
	}
	switch {
	case side.margin && flowErr == nil && tagged != r.marginWrites:
		t.Errorf("the room was published %d margin writes tagged margin-only, want the %d this transaction made on a fork warm-built from it", tagged, r.marginWrites)
	case !side.margin && tagged != 0:
		t.Errorf("the room was published %d updates tagged margin-only by the whole-document write, which tags none", tagged)
	}

	if resident {
		kept, fresh := replicaRendering(t, side.service, ids.artifact)
		if kept != fresh {
			t.Errorf("the room's replica keeps a rendering of %s, not the room's own %s", document.shown(kept), document.shown(fresh))
		}
		observed.Replica = kept
	}
	return observed
}

// rowEffects applies the rows at versions, in order, to the stored document history leaves, and
// records what each leaves. client is the flow's transaction's own client id, named "txn" so the
// two sides' compare.
func (side *equivalenceSide) rowEffects(t *testing.T, document *equivalenceDocument, artifactID string, versions []int64, client crdt.ClientID, history []storedRow) []rowEffect {
	t.Helper()
	if len(versions) == 0 {
		return nil
	}
	rows, err := side.database.Pool.Query(context.Background(), `
		select version, update, content_changed from doc_updates where artifact_id = $1 and version = any($2) order by version
	`, artifactID, versions)
	if err != nil {
		t.Fatalf("read the flow's rows: %v", err)
	}
	appended, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedRow, error) {
		var stored storedRow
		return stored, row.Scan(&stored.version, &stored.update, &stored.contentChanged)
	})
	if err != nil {
		t.Fatalf("read the flow's rows: %v", err)
	}
	if len(appended) != len(versions) {
		t.Fatalf("the transaction appended %d rows, the store holds %d of them", len(versions), len(appended))
	}
	doc := newDocumentCopy()
	for _, row := range history {
		if err := crdt.ApplyUpdateV1(doc, row.update, nil); err != nil {
			t.Fatalf("decode the stored history: %v", err)
		}
	}
	effects := make([]rowEffect, 0, len(appended))
	for _, row := range appended {
		ids, err := crdt.ContentIDsFromUpdateV1(row.update)
		if err != nil {
			t.Fatalf("decode the row at version %d: %v", row.version, err)
		}
		if err := crdt.ApplyUpdateV1(doc, row.update, nil); err != nil {
			t.Fatalf("apply the row at version %d: %v", row.version, err)
		}
		effect := rowEffect{
			ContentChanged: row.contentChanged,
			Inserted:       idRanges(ids.Inserts, client),
			Deleted:        idRanges(ids.Deletes, client),
		}
		marks, err := doc.GetMap(marksMapName).ToJSON()
		if err != nil {
			t.Fatalf("encode the marks map: %s", document.shown(err.Error()))
		}
		effect.Marks = string(marks)
		if err := func() error {
			tree, err := treeOf(doc)
			if err != nil {
				return err
			}
			if effect.Markdown, err = documentMarkdown(tree); err != nil {
				return err
			}
			effect.DocumentToken, effect.BlockTokens, err = encodedTokens(tree)
			return err
		}(); err != nil {
			effect.TreeErr = err.Error()
		}
		effects = append(effects, effect)
	}
	return effects
}

// idRanges is set's id ranges, client's named "txn".
func idRanges(set *crdt.IDSet, client crdt.ClientID) []string {
	var ranges []string
	for _, owner := range set.Clients() {
		name := strconv.FormatUint(uint64(owner), 10)
		if owner == client {
			name = "txn"
		}
		for _, run := range set.Ranges(owner) {
			ranges = append(ranges, fmt.Sprintf("%s:%d+%d", name, run.Clock, run.Len))
		}
	}
	slices.Sort(ranges)
	return ranges
}

// anchors is each of the flow's comment rows with its anchor and whether it is resolved.
func (side *equivalenceSide) anchors(t *testing.T, ids flowIDs) []string {
	t.Helper()
	rows, err := side.database.Pool.Query(context.Background(), `
		select id::text || ' resolved=' || resolved::text || ' ' || coalesce(anchor::text, 'unanchored')
		from comments where issue_key = $1 order by id
	`, ids.issue)
	if err != nil {
		t.Fatalf("read the flow's comments: %v", err)
	}
	anchors, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the flow's comments: %v", err)
	}
	return anchors
}

// encodeObserved is value as the sides compare it: itself for a string, its JSON otherwise.
func encodeObserved(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "unencodable: " + err.Error()
	}
	return string(encoded)
}

// compareObservations reports every difference between what the margin write and the
// whole-document write left.
func compareObservations(t *testing.T, document *equivalenceDocument, margin, whole flowObservation) {
	t.Helper()
	differ := func(what string, left, right any) {
		t.Helper()
		got, want := encodeObserved(left), encodeObserved(right)
		if got == want {
			return
		}
		if document.private {
			t.Errorf("%s differs: the margin write left %d bytes (sha256 %x), the whole-document write %d bytes (sha256 %x)",
				what, len(got), sha256.Sum256([]byte(got)), len(want), sha256.Sum256([]byte(want)))
			return
		}
		at := 0
		for at < min(len(got), len(want)) && got[at] == want[at] {
			at++
		}
		excerpt := func(text string) string {
			return strconv.Quote(text[max(0, at-160):min(len(text), at+160)])
		}
		t.Errorf("%s differs at byte %d:\n  margin write:         %s\n  whole-document write: %s", what, at, excerpt(got), excerpt(want))
	}
	differ("the flow's error", observeError(margin.Err), observeError(whole.Err))
	operationNames := func(observed flowObservation) []string {
		var names []string
		for _, operation := range observed.Operations {
			names = append(names, operation.Name)
		}
		return names
	}
	differ("the operations run", operationNames(margin), operationNames(whole))
	for index := range min(len(margin.Operations), len(whole.Operations)) {
		name := fmt.Sprintf("operation %d (%s)", index, margin.Operations[index].Name)
		differ(name+"'s result", margin.Operations[index].Result, whole.Operations[index].Result)
		differ(name+"'s events", margin.Operations[index].Events, whole.Operations[index].Events)
	}
	sortedSequences := func(sequences []int) []int {
		sorted := slices.Clone(sequences)
		slices.Sort(sorted)
		return sorted
	}
	differ("the events' sequence numbers", sortedSequences(margin.Sequences), sortedSequences(whole.Sequences))
	if margin.Write == nil || whole.Write == nil {
		differ("whether the transaction wrote the document", margin.Write != nil, whole.Write != nil)
	} else {
		left, right := *margin.Write, *whole.Write
		differ("the write's fork's coldness", left.Cold, right.Cold)
		differ("the number of updates the write appended", left.Appended, right.Appended)
		differ("the write's tree's rendering", left.TreeMarkdown, right.TreeMarkdown)
		differ("the write's tree's block offsets", left.TreeOffsets, right.TreeOffsets)
		differ("the write's tree's refusal", left.TreeErr, right.TreeErr)
		differ("the write's markdown", left.Markdown, right.Markdown)
		differ("the write's block offsets", left.Offsets, right.Offsets)
		differ("whether the write's anchors were refreshed against a tree", left.AnchorsTree, right.AnchorsTree)
		differ("whether the write's anchors were refreshed against its tree", left.AnchorsTreeIsTree, right.AnchorsTreeIsTree)
		differ("the write's credits", left.Credits, right.Credits)
		differ("the appends that credited them", left.CreditedBy, right.CreditedBy)
		differ("the write's latest content author", left.Actor, right.Actor)
		differ("the write's credit sequence", left.CreditSeq, right.CreditSeq)
		differ("the ask blocks the write added", left.Added, right.Added)
		differ("the ask blocks the write renamed", left.Renamed, right.Renamed)
		differ("the ask authors the write carried", left.Carried, right.Carried)
	}
	differ("the number of rows the transaction appended", len(margin.Rows), len(whole.Rows))
	for index := range min(len(margin.Rows), len(whole.Rows)) {
		left, right := margin.Rows[index], whole.Rows[index]
		row := fmt.Sprintf("appended row %d", index)
		differ(row+"'s content_changed", left.ContentChanged, right.ContentChanged)
		differ(row+"'s marks map", left.Marks, right.Marks)
		differ(row+"'s markdown", left.Markdown, right.Markdown)
		differ(row+"'s document token", left.DocumentToken, right.DocumentToken)
		differ(row+"'s block tokens", left.BlockTokens, right.BlockTokens)
		differ(row+"'s tree refusal", left.TreeErr, right.TreeErr)
		differ(row+"'s inserted ids", left.Inserted, right.Inserted)
		differ(row+"'s delete set", left.Deleted, right.Deleted)
	}
	differ("the comments' anchors", margin.Anchors, whole.Anchors)
	differ("the text and document token after the flow", margin.Text, whole.Text)
	differ("the blocks after the flow", margin.Blocks, whole.Blocks)
	differ("the classes the room recorded", margin.Classes, whole.Classes)
	differ("the rendering the room's replica keeps", margin.Replica, whole.Replica)
}

// checkFlow fails a flow that did not test what it is for: a refusal that did not refuse, a write
// that did not commit, or an anchor refresh that wrote nothing where the browser edit had moved
// anchors for it.
func (observed flowObservation) checkFlow(t *testing.T, document *equivalenceDocument, side string, flow equivalenceFlow) {
	t.Helper()
	switch {
	case flow.wantErr == nil && observed.Err != nil:
		t.Errorf("the %s's flow failed (%s), so it compares no write", side, document.shown(observed.Err.Error()))
	case flow.wantErr != nil && observed.Err == nil:
		t.Errorf("the %s's flow committed, want the refusal %v", side, flow.wantErr)
	case flow.wantErr != nil && !errors.Is(observed.Err, flow.wantErr):
		t.Errorf("the %s's flow ended in %s, want the refusal %v", side, document.shown(observed.Err.Error()), flow.wantErr)
	}
	for _, name := range flow.refreshing {
		refreshed := false
		for _, operation := range observed.Operations {
			if operation.Name != name {
				continue
			}
			for _, event := range operation.Events {
				refreshed = refreshed || strings.Contains(event, `"type":"comment.anchor_refreshed"`)
			}
		}
		if !refreshed {
			t.Errorf("the %s's %s refreshed no anchor, so the flow does not test the refresh", side, name)
		}
	}
}
