package docs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	ygws "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The tests here hold a write transaction's reads of its fork (forkTreeCounts) to what reading the
// fork would have answered: a read joined to the transaction - docTree, a version's capture - and
// a margin write take the write's own rendering in place of reading the fork
// (liveWrite.rendering). Every expected value comes from a read of its own, never from the code
// under test: a rendering the write holds is compared, wherever the service hands it on
// (checkRendering), with a fresh read and render of the fork, and what a write answers is compared
// with the document its store holds once it commits, decoded into a document of the test's own
// (persistedProofTree). Each operation also asserts the exact number of fork reads and renderings
// reused it made, so none passes by reading where it means to reuse, or by reusing where it means
// to read.

// forkReads is a reading of the service's forkTreeCounts.
type forkReads struct {
	read, renderingReused uint64
}

func readForkReads(service *Service) forkReads {
	counts := &service.forkTrees
	return forkReads{
		read:            counts.read.Load(),
		renderingReused: counts.renderingReused.Load(),
	}
}

func (r forkReads) minus(base forkReads) forkReads {
	return forkReads{
		read:            r.read - base.read,
		renderingReused: r.renderingReused - base.renderingReused,
	}
}

// renderingChecks is the service's checkRendering: at each call it compares the write's rendering
// with a fresh read and render of its fork (renderingProblem), and counts the calls at each site.
// The hook can run off the test's goroutine, so what it finds is collected and reported by take.
type renderingChecks struct {
	mu       sync.Mutex
	calls    map[string]int
	failures []string
}

func checkRenderings(service *Service) *renderingChecks {
	checks := &renderingChecks{calls: map[string]int{}}
	service.checkRendering = checks.check
	return checks
}

func (c *renderingChecks) check(at string, write *liveWrite) {
	problem := renderingProblem(write)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[at]++
	if problem != "" {
		c.failures = append(c.failures, at+": "+problem)
	}
}

// take returns the calls and failures since the last take.
func (c *renderingChecks) take() (map[string]int, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	calls, failures := c.calls, c.failures
	c.calls, c.failures = map[string]int{}, nil
	return calls, failures
}

// requireNoRenderingFailures fails the test for every rendering the hook found not to be its
// fork's since the last take.
func (c *renderingChecks) requireNoRenderingFailures(t *testing.T) {
	t.Helper()
	_, failures := c.take()
	for _, failure := range failures {
		t.Errorf("a write's rendering is not its fork's at %s", failure)
	}
}

// renderingProblem is what is wrong with write's rendering of its fork, or "" when it describes
// the fork exactly - its tree, block ids included, and the tree's encoding, its markdown and its
// block offsets all as a fresh read and render of the fork make them - or it holds none.
func renderingProblem(write *liveWrite) string {
	if write.tree == nil {
		return ""
	}
	if write.fork == nil {
		return "the write holds a rendering but no fork"
	}
	fresh, err := treeOf(write.fork)
	if err != nil {
		return fmt.Sprintf("read the fork: %v", err)
	}
	if !write.tree.EqualWithBlockIDs(fresh) {
		return "the write's tree is not the fork's"
	}
	held, err := json.Marshal(write.tree)
	if err != nil {
		return fmt.Sprintf("encode the write's tree: %v", err)
	}
	read, err := json.Marshal(fresh)
	if err != nil {
		return fmt.Sprintf("encode the fork's tree: %v", err)
	}
	if !bytes.Equal(held, read) {
		return "the write's tree encodes otherwise than the fork's"
	}
	markdown, offsets, err := documentMarkdownWithOffsets(fresh)
	if err != nil {
		return fmt.Sprintf("render the fork: %v", err)
	}
	if write.markdown != markdown {
		return fmt.Sprintf("the write's markdown (%d bytes) is not the fork's (%d bytes)", len(write.markdown), len(markdown))
	}
	if !slices.Equal(write.offsets, offsets) {
		return fmt.Sprintf("the write's %d block offsets are not the fork's %d", len(write.offsets), len(offsets))
	}
	return ""
}

// forkReadsSection is one section of forkReadsDocument: its number, which every sentence of it
// carries, and whether it holds a callout.
type forkReadsSection struct {
	number  int
	callout bool
}

// forkReadsSections are the sections the cases here quote, each case taking a section of its own
// so no two touch the same text: 0 the room's seeded browser edit; 3 to 10 the anchor placements
// (a list item, a table cell, a callout, and a quote across a paragraph and the list after it),
// warm and cold; 11 to 22 the refusals; 41 to 44 the warm flows and 51 and 52 the cold ones; and
// 61 to 73 the panicked writes (panicked_fork_test.go).
var forkReadsSections = []forkReadsSection{
	{number: 0},
	{number: 3}, {number: 4}, {number: 5, callout: true}, {number: 6}, {number: 8}, {number: 10, callout: true},
	{number: 11}, {number: 12}, {number: 21}, {number: 22},
	{number: 41}, {number: 42}, {number: 43}, {number: 44}, {number: 51}, {number: 52},
	{number: 61}, {number: 62}, {number: 63}, {number: 71}, {number: 72}, {number: 73},
}

// forkReadsAreas are the rows of the document's one table, among them the cells a case anchors in.
var forkReadsAreas = []int{7, 9}

// forkReadsDocument is the few kilobytes the tests here write on: a heading, one table, and one
// section per case, each holding a paragraph, a bullet list, an ordered list, and a callout where
// a case anchors in one. It is renderReuseDocument's shape in the size each case needs, and not
// renderReuseDocument itself, which generates 400 KB: every operation here reads, renders and
// encodes the whole document, each rendering check renders and marshals it twice more, and each
// cold case loads it from the store again, so these tests cost their document's size many times
// over, and the cases are the same on four hundred kilobytes as on seven.
func forkReadsDocument() string {
	var b strings.Builder
	b.WriteString("# Equivalence specification\n\n")
	b.WriteString("| Area | Owner | State |\n| --- | --- | --- |\n")
	for _, area := range forkReadsAreas {
		fmt.Fprintf(&b, "| Area %d | Team %d | open |\n", area, area%4)
	}
	b.WriteString("\n")
	for _, section := range forkReadsSections {
		fmt.Fprintf(&b, "## Section %d\n\n", section.number)
		fmt.Fprintf(&b, "Paragraph %d opens the section with enough words to read like specification prose about the subject at hand.\n\n", section.number)
		fmt.Fprintf(&b, "- Bullet %d alpha names one requirement of the section.\n- Bullet %d beta names another requirement of the section.\n\n", section.number, section.number)
		fmt.Fprintf(&b, "1. Step %d one is taken first.\n2. Step %d two is taken next.\n\n", section.number, section.number)
		if section.callout {
			fmt.Fprintf(&b, ":::callout{#callout-%d kind=\"note\" title=\"\"}\nCallout %d holds a note the readers of the section need.\n:::\n\n", section.number, section.number)
		}
	}
	return b.String()
}

// newForkReadsRoom is a service whose settlement waits an hour, holding forkReadsDocument in a
// room one browser edit has written to, durable.
func newForkReadsRoom(t *testing.T) (*Service, string) {
	t.Helper()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, forkReadsDocument())
	editAsConnectedPeer(t, service, artifactID, 1, model.Actor{Kind: "user", ID: "carol"}, replaceRun("Paragraph 0 opens", "Paragraph 0 first opens"))
	waitForLandedAppends(t, service, artifactID)
	return service, artifactID
}

// forkReadsMode is a write that forks a room (warm), or the store with no room holding the
// document (cold). A cold case closes the room first; its publish loads it again.
type forkReadsMode struct {
	name string
	cold bool
}

var forkReadsModes = []forkReadsMode{{name: "warm"}, {name: "cold", cold: true}}

func (m forkReadsMode) prepare(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	if !m.cold {
		if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
			t.Fatalf("load the document's room: %v", err)
		}
		return
	}
	closeDocumentRoom(t, service, artifactID)
	if service.srv.GetDoc(artifactID) != nil {
		t.Fatal("a room still holds the document")
	}
}

// requireForkedAs fails unless the transaction's write forked as m says.
func (m forkReadsMode) requireForkedAs(t *testing.T, ctx context.Context, artifactID string) *liveWrite {
	t.Helper()
	write := joinedLiveWrite(ctx, artifactID)
	if write == nil {
		t.Fatal("the transaction holds no write")
	}
	if write.cold != m.cold && write.fork != nil {
		t.Fatalf("the write forked cold = %t, want %t", write.cold, m.cold)
	}
	return write
}

// committedAnchor is what the mark id of kind anchors to in tree: the text it covers and the
// lowest identified block holding all of it.
func committedAnchor(t *testing.T, tree *pmdoc.Node, kind MarkKind, id string) Anchored {
	t.Helper()
	r, quote, found := pmdoc.FindMark(tree, string(kind), id)
	if !found {
		t.Fatalf("the committed document holds no %s mark %q", kind, id)
	}
	blockID, err := pmdoc.BlockIDForRange(tree, r)
	if err != nil {
		t.Fatalf("the block holding mark %q: %v", id, err)
	}
	return Anchored{Quote: quote, BlockID: blockID}
}

// requireVersionIsTheCommittedDocument fails unless version number of artifactID records the
// rendering of the document its store holds now, read and rendered afresh.
func requireVersionIsTheCommittedDocument(t *testing.T, service *Service, artifactID string, number int) string {
	t.Helper()
	fresh, err := documentMarkdown(persistedProofTree(t, service.store, artifactID))
	if err != nil {
		t.Fatalf("render the committed document: %v", err)
	}
	if stored := versionMarkdown(t, service.store, artifactID, number); stored != fresh {
		t.Errorf("version %d records %d bytes of markdown, not the committed document's %d bytes", number, len(stored), len(fresh))
	}
	return fresh
}

func requireForkReads(t *testing.T, what string, got, want forkReads) {
	t.Helper()
	if got != want {
		t.Errorf("%s: fork reads %+v, want %+v", what, got, want)
	}
}

// A MarkQuote reads the fork twice - before its write, and once after it, the tree the write is
// weighed and versioned with and its anchor is read off - and a version later in its transaction
// reads it no more. What it answers is what the committed document's mark anchors to: in a list
// item, a table cell or a callout, the nested block holding the quote; across top-level siblings,
// no block. A refusal leaves no rendering its fork disagrees with, and a version after it takes the
// rendering only where the write kept it: a mark the growth weigh refuses drops the fork with its
// rendering, a mark already written is no change and keeps both, and a document outside the
// schema answers its own refusal, which a read of it answers too.
func TestForkReadsAMarkQuoteAnswersItsAnchorAndAVersionAfterItReusesItsRendering(t *testing.T) {
	service, artifactID := newForkReadsRoom(t)
	checks := checkRenderings(service)
	topLevel := map[string]bool{}
	for _, block := range persistedProofTree(t, service.store, artifactID).Children {
		if id, _ := block.Attrs[pmdoc.BlockIDAttr].(string); id != "" {
			topLevel[id] = true
		}
	}
	anchors := map[string][]struct {
		name, quote string
		nested      bool
	}{
		"warm": {
			{"a list item", "Bullet 3 alpha names one requirement", true},
			{"a table cell", "Area 7", true},
			{"a callout", "Callout 5 holds a note", true},
			{"top-level siblings", "subject at hand. Bullet 4 alpha", false},
		},
		"cold": {
			{"a list item", "Bullet 6 alpha names one requirement", true},
			{"a table cell", "Area 9", true},
			{"a callout", "Callout 10 holds a note", true},
			{"top-level siblings", "subject at hand. Bullet 8 alpha", false},
		},
	}
	for _, mode := range forkReadsModes {
		for index, anchor := range anchors[mode.name] {
			t.Run(mode.name+"/"+anchor.name, func(t *testing.T) {
				mode.prepare(t, service, artifactID)
				id := fmt.Sprintf("c-%s-%d", mode.name, index)
				var anchored Anchored
				var version int
				if err := joinedWrite(service, func(ctx context.Context) error {
					base := readForkReads(service)
					var err error
					if anchored, err = service.MarkQuote(ctx, artifactID, MarkSpec{Kind: MarkComment, ID: id, By: reuseAlice}, anchor.quote, nil); err != nil {
						return err
					}
					requireForkReads(t, "MarkQuote", readForkReads(service).minus(base), forkReads{read: 2})
					if problem := renderingProblem(mode.requireForkedAs(t, ctx, artifactID)); problem != "" {
						t.Errorf("after MarkQuote: %s", problem)
					}
					base = readForkReads(service)
					snapshot, err := service.SnapshotVersion(ctx, artifactID, reuseAlice)
					if err != nil {
						return err
					}
					version = snapshot.Version.Number
					requireForkReads(t, "SnapshotVersion after MarkQuote", readForkReads(service).minus(base), forkReads{renderingReused: 1})
					return nil
				}); err != nil {
					t.Fatalf("mark %q: %v", anchor.quote, err)
				}
				checks.requireNoRenderingFailures(t)
				committed := persistedProofTree(t, service.store, artifactID)
				if want := committedAnchor(t, committed, MarkComment, id); anchored != want {
					t.Errorf("MarkQuote answered %+v, the committed mark anchors to %+v", anchored, want)
				}
				if anchored.Quote != anchor.quote {
					t.Errorf("MarkQuote answered the quote %q, want %q", anchored.Quote, anchor.quote)
				}
				switch {
				case anchor.nested && (anchored.BlockID == "" || topLevel[anchored.BlockID]):
					t.Errorf("a quote inside %s anchors to block %q, want the nested block holding it", anchor.name, anchored.BlockID)
				case !anchor.nested && anchored.BlockID != "":
					t.Errorf("a quote across top-level siblings anchors to block %q, want none", anchored.BlockID)
				}
				requireVersionIsTheCommittedDocument(t, service, artifactID, version)
			})
		}
	}

	// Each refusal is the transaction's second MarkQuote, after one that wrote its mark and left
	// the write a rendering, and a version follows it in the same transaction, which commits.
	heavy := model.Actor{Kind: "session", ID: "s" + strings.Repeat("a", maxAnchorBytes)}
	for _, mode := range forkReadsModes {
		for index, refusal := range []struct {
			name string
			mark MarkSpec
			// same marks the first quote again rather than the refusal's own.
			same bool
			// refused judges the refusal's error.
			refused func(t *testing.T, err error)
			reads   forkReads
			// abandoned is a refused write that changed the fork, which is dropped with its
			// rendering; any other keeps both, and the version takes the rendering.
			abandoned bool
		}{
			{
				name: "a mark the growth weigh refuses",
				mark: MarkSpec{Kind: MarkComment, ID: "c-heavy", By: heavy},
				refused: func(t *testing.T, err error) {
					if !errors.Is(err, ErrDocumentTooLarge) {
						t.Errorf("got %v, want the growth refusal (ErrDocumentTooLarge)", err)
					}
				},
				reads:     forkReads{read: 2},
				abandoned: true,
			},
			{
				name: "a mark already written",
				same: true,
				refused: func(t *testing.T, err error) {
					if !errors.Is(err, ygws.ErrNoChanges) {
						t.Errorf("got %v, want websocket.ErrNoChanges", err)
					}
				},
				reads: forkReads{read: 1},
			},
		} {
			t.Run(mode.name+"/"+refusal.name, func(t *testing.T) {
				mode.prepare(t, service, artifactID)
				first := MarkSpec{Kind: MarkComment, ID: fmt.Sprintf("c-%s-first-%d", mode.name, index), By: reuseAlice}
				// Steps 11 and 12 for the warm write, 21 and 22 for the cold one: each quote unique.
				section := 11 + index
				if mode.cold {
					section += 10
				}
				firstQuote := fmt.Sprintf("Step %d one is taken first", section)
				quote, mark := fmt.Sprintf("Step %d two is taken next", section), refusal.mark
				if refusal.same {
					quote, mark = firstQuote, first
				}
				var version int
				if err := joinedWrite(service, func(ctx context.Context) error {
					if _, err := service.MarkQuote(ctx, artifactID, first, firstQuote, nil); err != nil {
						return fmt.Errorf("the first mark: %w", err)
					}
					write := mode.requireForkedAs(t, ctx, artifactID)
					if write.tree == nil {
						t.Fatal("the first mark left the write no rendering")
					}
					base := readForkReads(service)
					anchored, err := service.MarkQuote(ctx, artifactID, mark, quote, nil)
					if err == nil {
						t.Fatalf("the refused mark was taken: %+v", anchored)
					}
					refusal.refused(t, err)
					if anchored != (Anchored{}) {
						t.Errorf("a refused MarkQuote answered %+v, want nothing", anchored)
					}
					requireForkReads(t, "the refused MarkQuote", readForkReads(service).minus(base), refusal.reads)
					if problem := renderingProblem(write); problem != "" {
						t.Errorf("after the refusal: %s", problem)
					}
					if refusal.abandoned != (write.fork == nil) || refusal.abandoned != (write.tree == nil) {
						t.Errorf("after the refusal the write holds a fork = %t and a rendering = %t, want both %t", write.fork != nil, write.tree != nil, !refusal.abandoned)
					}
					base = readForkReads(service)
					snapshot, err := service.SnapshotVersion(ctx, artifactID, reuseAlice)
					if err != nil {
						return err
					}
					version = snapshot.Version.Number
					want := forkReads{renderingReused: 1}
					if refusal.abandoned {
						want = forkReads{read: 1}
					}
					requireForkReads(t, "SnapshotVersion after the refusal", readForkReads(service).minus(base), want)
					return nil
				}); err != nil {
					t.Fatalf("commit the transaction: %v", err)
				}
				checks.requireNoRenderingFailures(t)
				committed := persistedProofTree(t, service.store, artifactID)
				committedAnchor(t, committed, MarkComment, first.ID)
				if _, _, found := pmdoc.FindMark(committed, string(MarkComment), "c-heavy"); found {
					t.Error("the committed document holds the refused mark")
				}
				requireVersionIsTheCommittedDocument(t, service, artifactID, version)
			})
		}
	}

	// A document outside the schema - one the reader refuses, or only the renderer - answers the
	// refusal of the tree the write started from, the one a read of the document answers.
	readerRefused := func(t *testing.T) (string, string) {
		id := createDocument(t, service.store, "")
		seedServiceText(t, service, id, "Readable text.\n")
		writeSchemaInvalidElement(t, service, id)
		return id, "Readable"
	}
	rendererRefused := func(t *testing.T) (string, string) {
		id := createDocument(t, service.store, "")
		if err := AppendRenderOnlySchemaViolationForTest(context.Background(), service.store, id); err != nil {
			t.Fatal(err)
		}
		return id, "Unreadable"
	}
	mark := MarkSpec{Kind: MarkComment, ID: "c-outside", By: reuseAlice}
	for _, mode := range forkReadsModes {
		for _, outside := range []struct {
			name     string
			document func(t *testing.T) (string, string)
			reads    forkReads
			// abandoned is a refused write that changed the fork, which is dropped.
			abandoned bool
		}{
			{"the reader refuses it", readerRefused, forkReads{read: 1}, false},
			{"only the renderer refuses it", rendererRefused, forkReads{read: 2}, true},
		} {
			t.Run(mode.name+"/outside the schema/"+outside.name, func(t *testing.T) {
				document, quote := outside.document(t)
				mode.prepare(t, service, document)
				_, readErr := service.Text(context.Background(), document)
				var readRefusal *OutsideSchemaError
				if !errors.As(readErr, &readRefusal) {
					t.Fatalf("a read of the document answered %v, want an OutsideSchemaError", readErr)
				}
				err := joinedWrite(service, func(ctx context.Context) error {
					base := readForkReads(service)
					_, err := service.MarkQuote(ctx, document, mark, quote, nil)
					requireForkReads(t, "MarkQuote", readForkReads(service).minus(base), outside.reads)
					write := mode.requireForkedAs(t, ctx, document)
					if problem := renderingProblem(write); problem != "" {
						t.Errorf("after the refusal: %s", problem)
					}
					if write.tree != nil || outside.abandoned != (write.fork == nil) {
						t.Errorf("after the refusal the write holds a rendering = %t and a fork = %t, want no rendering and a fork = %t", write.tree != nil, write.fork != nil, !outside.abandoned)
					}
					return err
				})
				var refusal *OutsideSchemaError
				if !errors.As(err, &refusal) || !errors.Is(err, ErrDocOutsideSchema) || err.Error() != readErr.Error() {
					t.Errorf("got %v, want the document's own refusal, as a read answers it: %v", err, readErr)
				}
				checks.requireNoRenderingFailures(t)
			})
		}
	}
}

// forkStep is one operation of a transaction, the fork reads it makes and the sites it hands the
// write's rendering to checkRendering at.
type forkStep struct {
	name  string
	run   func(ctx context.Context) error
	reads forkReads
	sites map[string]int
}

// runForkSteps runs steps as one committed transaction in mode, asserting each step's fork reads
// and checkRendering sites, then that the commit's publish checked the rendering once, and that no
// check found a rendering its fork disagrees with.
func runForkSteps(t *testing.T, service *Service, artifactID string, checks *renderingChecks, mode forkReadsMode, steps ...forkStep) {
	t.Helper()
	mode.prepare(t, service, artifactID)
	checks.take()
	var failures []string
	if err := joinedWrite(service, func(ctx context.Context) error {
		for _, step := range steps {
			base := readForkReads(service)
			if err := step.run(ctx); err != nil {
				return fmt.Errorf("%s: %w", step.name, err)
			}
			requireForkReads(t, step.name, readForkReads(service).minus(base), step.reads)
			calls, found := checks.take()
			failures = append(failures, found...)
			if !maps.Equal(calls, step.sites) {
				t.Errorf("%s: the rendering was checked at %v, want %v", step.name, calls, step.sites)
			}
			if joinedLiveWrite(ctx, artifactID) != nil {
				mode.requireForkedAs(t, ctx, artifactID)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("commit the transaction: %v", err)
	}
	calls, found := checks.take()
	failures = append(failures, found...)
	if want := map[string]int{"publish": 1}; !maps.Equal(calls, want) {
		t.Errorf("the commit checked the rendering at %v, want %v", calls, want)
	}
	for _, failure := range failures {
		t.Errorf("a write's rendering is not its fork's at %s", failure)
	}
}

// Every rendering a write hands on - to a margin write (applyMargin), to a read joined to its
// transaction (docTree), to a version (capture), and at each fork read and its publish - is a
// fresh read and render of its fork, in each transaction the API runs: an anchored comment, a
// suggestion, an edit and its version, an accept, and a transaction a browser's commit moves the
// fork under, after which no rendering is reused. Each version records the document its
// transaction committed.
func TestForkReadsEveryReuseOfAWritesRenderingDescribesItsFork(t *testing.T) {
	service, artifactID := newForkReadsRoom(t)
	checks := checkRenderings(service)
	markQuote := func(kind MarkKind, id, quote string) forkStep {
		return forkStep{
			name: "MarkQuote " + quote,
			run: func(ctx context.Context) error {
				_, err := service.MarkQuote(ctx, artifactID, MarkSpec{Kind: kind, ID: id, By: reuseAlice}, quote, nil)
				return err
			},
			reads: forkReads{read: 2},
			sites: map[string]int{"fork": 1},
		}
	}
	// version is SnapshotVersion, or NamedVersion when named, which records its number in number;
	// reused is whether it takes the write's rendering.
	version := func(number *int, named, reused bool) forkStep {
		step := forkStep{name: "SnapshotVersion", reads: forkReads{read: 1}, sites: map[string]int{"fork": 1}}
		if named {
			step.name = "NamedVersion"
		}
		if reused {
			step.reads, step.sites = forkReads{renderingReused: 1}, map[string]int{"fork": 1, "capture": 1}
		}
		step.run = func(ctx context.Context) error {
			var result VersionResult
			var err error
			if named {
				result, err = service.NamedVersion(ctx, artifactID, "Accepted the suggestion", reuseAlice)
			} else {
				result, err = service.SnapshotVersion(ctx, artifactID, reuseAlice)
			}
			*number = result.Version.Number
			return err
		}
		return step
	}
	// suggestionKind is SuggestionKind, which must answer replace; joined is whether the
	// transaction has a write by then, and reused whether it takes that write's rendering.
	suggestionKind := func(id string, joined, reused bool) forkStep {
		step := forkStep{name: "SuggestionKind", sites: map[string]int{}}
		switch {
		case reused:
			step.reads, step.sites = forkReads{renderingReused: 1}, map[string]int{"fork": 1, "docTree": 1}
		case joined:
			step.reads, step.sites = forkReads{read: 1}, map[string]int{"fork": 1}
		}
		step.run = func(ctx context.Context) error {
			kind, err := service.SuggestionKind(ctx, artifactID, id)
			if err == nil && kind != "replace" {
				return fmt.Errorf("the suggestion's kind is %q, want replace", kind)
			}
			return err
		}
		return step
	}
	// projectMark is ProjectMark after a write in the same transaction, whose rendering its margin
	// write takes in place of reading the fork (applyMargin).
	projectMark := func(id string, record MarkRecord) forkStep {
		return forkStep{
			name:  "ProjectMark",
			run:   func(ctx context.Context) error { return service.ProjectMark(ctx, artifactID, id, record, reuseAlice) },
			reads: forkReads{renderingReused: 1},
			sites: map[string]int{"fork": 1, "margin": 1},
		}
	}
	comment := MarkRecord{Kind: "comment", By: "user:alice", CreatedAt: "2026-10-28T00:00:00Z", Text: "note", Replies: []MarkReply{}}
	suggestion := func(content, status string) MarkRecord {
		return MarkRecord{Kind: "replace", By: "user:alice", CreatedAt: "2026-10-28T00:00:00Z", Replies: []MarkReply{}, Content: content, Status: status}
	}

	for _, mode := range forkReadsModes {
		// Steps 41 and 42 for the warm transactions, 51 and 52 for the cold ones.
		section := 41
		if mode.cold {
			section = 51
		}
		t.Run(mode.name+"/an anchored comment", func(t *testing.T) {
			var number int
			id := "c-flow-" + mode.name
			runForkSteps(t, service, artifactID, checks, mode,
				markQuote(MarkComment, id, fmt.Sprintf("Step %d one is taken first", section)),
				version(&number, false, true),
				projectMark(id, comment),
			)
			requireVersionIsTheCommittedDocument(t, service, artifactID, number)
		})
		t.Run(mode.name+"/a suggestion", func(t *testing.T) {
			var number int
			id := "s-flow-" + mode.name
			runForkSteps(t, service, artifactID, checks, mode,
				markQuote(MarkSuggestion, id, fmt.Sprintf("Step %d two is taken next", section)),
				version(&number, false, true),
				suggestionKind(id, true, true),
				projectMark(id, suggestion("taken later", "pending")),
			)
			requireVersionIsTheCommittedDocument(t, service, artifactID, number)
		})
		t.Run(mode.name+"/an edit and its version", func(t *testing.T) {
			var number int
			find := fmt.Sprintf("Step %d one is taken first", section+1)
			runForkSteps(t, service, artifactID, checks, mode,
				forkStep{
					name:  "ApplyOps",
					run:   applyOps(service, artifactID, reuseAgent, model.EditOp{Op: "replace", Find: find, With: find + " and again"}),
					reads: forkReads{read: 2},
					sites: map[string]int{"fork": 1},
				},
				version(&number, false, true),
			)
			if fresh := requireVersionIsTheCommittedDocument(t, service, artifactID, number); !strings.Contains(fresh, find+" and again") {
				t.Error("the committed document does not hold the edit")
			}
		})
	}

	// An accept loads the room for the projection kind it reads first, so it runs warm whatever
	// held the document before.
	t.Run("warm/an accept", func(t *testing.T) {
		const id = "s-accept"
		quote := "Step 43 two is taken next"
		if err := joinedWrite(service, func(ctx context.Context) error {
			if _, err := service.MarkQuote(ctx, artifactID, MarkSpec{Kind: MarkSuggestion, ID: id, By: reuseBob}, quote, nil); err != nil {
				return err
			}
			return service.ProjectMark(ctx, artifactID, id, suggestion("Step 43 two is taken last", "pending"), reuseBob)
		}); err != nil {
			t.Fatalf("suggest: %v", err)
		}
		checks.requireNoRenderingFailures(t)
		var number int
		runForkSteps(t, service, artifactID, checks, forkReadsModes[0],
			suggestionKind(id, false, false),
			forkStep{
				name: "AcceptSuggestion",
				run: func(ctx context.Context) error {
					return service.AcceptSuggestion(ctx, artifactID, id, "Step 43 two is taken last", reuseAlice)
				},
				reads: forkReads{read: 2},
				sites: map[string]int{"fork": 1},
			},
			projectMark(id, suggestion("Step 43 two is taken last", "accepted")),
			version(&number, true, true),
		)
		if fresh := requireVersionIsTheCommittedDocument(t, service, artifactID, number); !strings.Contains(fresh, "Step 43 two is taken last") || strings.Contains(fresh, quote) {
			t.Error("the committed document does not hold the accepted replacement")
		}
	})

	// A browser's commit between two operations moves the fork incrementally, which drops the
	// write's rendering: the reads after it read the fork, and none takes a rendering.
	t.Run("warm/a browser commit between operations", func(t *testing.T) {
		const id = "s-moved"
		var number int
		runForkSteps(t, service, artifactID, checks, forkReadsModes[0],
			markQuote(MarkSuggestion, id, "Step 44 one is taken first"),
			forkStep{
				name: "a browser's edit",
				run: func(context.Context) error {
					editAsConnectedPeer(t, service, artifactID, 2, reuseBob, replaceRun("Paragraph 44 opens", "Paragraph 44 then opens"))
					return nil
				},
				sites: map[string]int{},
			},
			suggestionKind(id, true, false),
			version(&number, false, false),
		)
		waitForLandedAppends(t, service, artifactID)
		if fresh := requireVersionIsTheCommittedDocument(t, service, artifactID, number); !strings.Contains(fresh, "Paragraph 44 then opens") {
			t.Error("the committed document does not hold the browser's edit")
		}
	})
}
