package docs

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A comment's or suggestion's margin record is written without reading or rendering the document
// twice (applyMargin), where it was once written as every other document write is (applyLive): the
// record set on the marks map, the document read and rendered before and after to tell what the
// write changed. The margin write keeps everything that write left - the stored update and the
// class it is stored with, the anchors it refreshes and the events that refresh appends, the
// rendering the transaction keeps for its version, the credits, the room's record of the publish
// and the rendering the room keeps after it - only cheaper. This drives both writes through the
// document calls each comment route makes, on real-size specs whose open anchors a browser has
// just moved, and holds the margin write to what the whole-document write leaves, field by field.
// A margin write that recorded its update as a content change would be settled as an edit and
// credited to its comment's author; one that skipped the anchor refresh would leave a moved
// comment quoting text the document no longer holds; one that kept a rendering from before a
// browser edit landed in its transaction would version, and refresh anchors against, a document
// that never existed.
//
// The two writes run on two services with a database each, every flow on a document of its own,
// seeded on both sides from the same bytes under the same ids, so the stored histories match byte
// for byte before each flow and every difference after it is the write's. When
// DISPATCH_TEST_SPEC_CORPUS names a directory, every *.md in it is one more document. Those may be
// private specs: the test logs only their size, sha256 and anchor count, and a difference on one
// names only lengths and sha256s.
func TestAMarginWriteMatchesTheWholeDocumentWrite(t *testing.T) {
	sources := []equivalenceSource{
		{name: "generated-390KiB", markdown: generatedSpec(390 << 10)},
		{name: "generated-600KiB", markdown: generatedSpec(600 << 10)},
	}
	sources = append(sources, corpusSpecs(t)...)
	sides := [2]*equivalenceSide{
		newEquivalenceSide(t, "margin write", true, projectMarginRecord),
		newEquivalenceSide(t, "whole-document write", false, wholeDocumentWrite),
	}
	flows := equivalenceFlows()
	number := 0
	for _, source := range sources {
		t.Run(source.name, func(t *testing.T) {
			document := prepareEquivalenceDocument(t, source)
			for _, flow := range flows {
				number++
				ids := flowIDsFor(number)
				t.Run(flow.name, func(t *testing.T) {
					compareFlow(t, sides, document, flow, ids)
				})
			}
		})
	}
}

// marginWriter writes record as markID's margin record in the transaction ctx joined: the margin
// write (ProjectMark) on one side, the whole-document write on the other. Every flow takes it, so
// one flow's code drives both.
type marginWriter func(ctx context.Context, s *Service, artifactID, markID string, record MarkRecord, actor model.Actor) error

func projectMarginRecord(ctx context.Context, s *Service, artifactID, markID string, record MarkRecord, actor model.Actor) error {
	return s.ProjectMark(ctx, artifactID, markID, record, actor)
}

// wholeDocumentWrite is ProjectMark as it was before the margin write: the record set on the marks
// map through applyLive, which reads and renders the document on either side of the write, as
// every other document write still does.
func wholeDocumentWrite(ctx context.Context, s *Service, artifactID, markID string, record MarkRecord, actor model.Actor) error {
	plain, err := toPlain(record)
	if err != nil {
		return err
	}
	return s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, _ *pmdoc.Node, transact func(func(*crdt.Transaction))) error {
		marks := doc.GetMap(marksMapName)
		transact(func(txn *crdt.Transaction) {
			marks.Set(txn, markID, plain)
		})
		return nil
	})
}

// The people of the flows: alice wrote every seeded comment, carol makes every flow's write, and
// bob and dana are the browsers whose edits land before and during a flow.
var (
	equivalenceAuthor = model.Actor{Kind: "user", ID: "alice"}
	flowActor         = model.Actor{Kind: "user", ID: "carol"}
	browserEditor     = model.Actor{Kind: "user", ID: "bob"}
	lateEditor        = model.Actor{Kind: "user", ID: "dana"}
)

// Fixed clocks for every row and record a flow writes, so the two databases hold the same values
// and every event payload that carries them compares.
var (
	seededAt  = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	flowAt    = time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)
	repliedAt = time.Date(2026, 10, 2, 9, 31, 0, 0, time.UTC)
)

// The client ids of the seed and of the two browsers, the same on both sides so the stored
// histories match byte for byte. A transaction's own client id is random (livewrite.go), so it
// differs between the sides and is compared under one name (rowEffect).
const (
	seedClient     crdt.ClientID = 7_000_001
	editClient     crdt.ClientID = 7_000_002
	lateEditClient crdt.ClientID = 7_000_003
)

// equivalenceArtifactName is every flow's document's name, which its events carry.
const equivalenceArtifactName = "spec.md"

// Each document holds anchorCount seeded anchors on unique sentences spread evenly through it, in
// document order, and one more unique sentence a flow's new comment anchors to (newQuote). The
// indexes name each anchor's part in the flows and in the browser edits.
const (
	anchorCount      = 48
	replyRoot        = 12
	suggestionAnchor = 24
	reopenTarget     = 30
	resolveTarget    = 36
	movedAnchor      = 45
	lateEditAnchor   = 9
)

var (
	// revisedAnchors have their quoted text changed by the browser edit before each flow, the
	// reopen target among them, so its reopen refreshes it.
	revisedAnchors = []int{6, 18, 42, reopenTarget}
	// deletedAnchors have their quoted text deleted by that edit, which orphans them.
	deletedAnchors = []int{3, 27}
)

const suggestedWording = "The accepted wording replaces the suggested sentence."

var (
	newCommentID   = equivalenceCommentID(100)
	replyCommentID = equivalenceCommentID(101)
)

func equivalenceCommentID(number int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", number)
}

// equivalenceSource is one document's markdown; private marks a corpus spec, whose content no
// message may carry.
type equivalenceSource struct {
	name     string
	markdown string
	private  bool
}

// corpusSpecs is every *.md in DISPATCH_TEST_SPEC_CORPUS, or none when it is unset.
func corpusSpecs(t *testing.T) []equivalenceSource {
	t.Helper()
	directory := os.Getenv("DISPATCH_TEST_SPEC_CORPUS")
	if directory == "" {
		t.Log("DISPATCH_TEST_SPEC_CORPUS is unset: the generated documents alone are compared")
		return nil
	}
	paths, err := filepath.Glob(filepath.Join(directory, "*.md"))
	if err != nil {
		t.Fatalf("list the spec corpus: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("DISPATCH_TEST_SPEC_CORPUS=%s holds no *.md", directory)
	}
	sources := make([]equivalenceSource, 0, len(paths))
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read corpus spec %s: %v", filepath.Base(path), err)
		}
		sources = append(sources, equivalenceSource{name: filepath.Base(path), markdown: string(content), private: true})
	}
	return sources
}

// generatedSpec is a spec of at least size bytes: numbered sections of prose whose sentences are
// each unique, with headings, tables, lists, ask blocks, code blocks, block quotes, links, inline
// code, emphasis, emoji and CJK Extension B, the text outside the Basic Multilingual Plane whose
// UTF-16 length differs from its rune count.
func generatedSpec(size int) string {
	subjects := []string{"cache layer", "replica", "queue worker", "settlement pass", "room observer", "upload path", "anchor index", "version writer"}
	verbs := []string{"drains", "renders", "verifies", "rebuilds", "publishes", "weighs", "refreshes", "compacts"}
	objects := []string{"its pending updates", "the stored history", "every open anchor", "the margin records", "the block offsets", "the ask answers", "the delete set", "the credit ledger"}
	conditions := []string{"before the next deploy", "once the lock is held", "while the browser stays connected", "after the transaction commits", "when the room reloads", "under the advisory lock", "unless the issue is closed", "as the replay measured"}
	var b strings.Builder
	for section := 1; b.Len() < size; section++ {
		fmt.Fprintf(&b, "## %d. The %s of part %d\n\n", section, subjects[section%len(subjects)], section)
		for paragraph := 1; paragraph <= 3; paragraph++ {
			for sentence := 1; sentence <= 6; sentence++ {
				n := section*31 + paragraph*7 + sentence
				extra := ""
				switch n % 5 {
				case 0:
					extra = " with 🚀 on the way"
				case 2:
					extra = " beside 𠀋 and 𠀍"
				}
				fmt.Fprintf(&b, "Note %d.%d.%d says the %s %s %s%s %s. ", section, paragraph, sentence,
					subjects[n%len(subjects)], verbs[(n/3)%len(verbs)], objects[(n/5)%len(objects)], extra,
					conditions[(n/7)%len(conditions)])
			}
			fmt.Fprintf(&b, "See `config.part_%d_%d` and [the guide to part %d](https://example.com/specs/%d/%d) for **the details** of *paragraph %d* 🧭 𠁆.\n\n",
				section, paragraph, section, section, paragraph, paragraph)
		}
		switch section % 6 {
		case 0:
			fmt.Fprintf(&b, "| Field | Meaning in part %d | Default |\n| --- | --- | --- |\n", section)
			for row := 1; row <= 4; row++ {
				fmt.Fprintf(&b, "| `field_%d_%d` | the %s of row %d in part %d | %d |\n", section, row, objects[row%len(objects)], row, section, row*section)
			}
			b.WriteString("\n")
		case 1:
			for item := 1; item <= 4; item++ {
				fmt.Fprintf(&b, "- Item %d.%d covers how the %s %s %s.\n", section, item, subjects[item%len(subjects)], verbs[item%len(verbs)], objects[(item+section)%len(objects)])
			}
			b.WriteString("\n")
		case 2:
			for step := 1; step <= 4; step++ {
				fmt.Fprintf(&b, "%d. Step %d of part %d %s %s.\n", step, step, section, verbs[step%len(verbs)], objects[(step+section)%len(objects)])
			}
			b.WriteString("\n")
		case 3:
			fmt.Fprintf(&b, "```go\nfunc part%d(input []byte) (int, error) {\n\tif len(input) == 0 {\n\t\treturn %d, nil\n\t}\n\treturn len(input) + %d, nil\n}\n```\n\n", section, section, section)
		case 4:
			fmt.Fprintf(&b, ":::ask{#ask-part-%d urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich behaviour should part %d keep?\n\n- Current: part %d keeps what it does today\n- Changed: part %d takes the new behaviour\n:::\n\n", section, section, section, section)
		case 5:
			fmt.Fprintf(&b, "> Quoted note %d says the %s %s %s %s, as the review of part %d recorded.\n\n", section, subjects[section%len(subjects)], verbs[section%len(verbs)], objects[section%len(objects)], conditions[section%len(conditions)], section)
		}
	}
	return b.String()
}

// equivalenceDocument is one source made ready for the flows: the seed both sides store, the
// anchors it holds, and the two browser edits.
type equivalenceDocument struct {
	name    string
	private bool
	// markdown is the seed's rendering, its first version.
	markdown string
	seed     []byte
	// edit is the browser edit that lands before each flow, from the seed; lateEdit the one that
	// lands inside the stale flow's transaction, from the seed and edit.
	edit     []byte
	lateEdit []byte
	anchors  []seedAnchor
	newQuote string
}

// seedAnchor is one seeded comment or suggestion: its row and its margin record.
type seedAnchor struct {
	id         string
	kind       MarkKind
	quote      string
	blockID    string
	body       string
	resolved   bool
	suggestion bool
}

// record is the anchor's margin record as the seed holds it (commentMarkRecord).
func (a seedAnchor) record() MarkRecord {
	record := MarkRecord{
		Kind:      "comment",
		By:        ActorRef(equivalenceAuthor),
		CreatedAt: seededAt.Format(time.RFC3339Nano),
		Text:      a.body,
		Resolved:  a.resolved,
		Replies:   []MarkReply{},
	}
	if a.suggestion {
		record.Kind, record.Content, record.Status = "replace", suggestedWording, "pending"
	}
	return record
}

// shown is text as a message may carry it: whole for a generated document, its length and sha256
// for a private one.
func (d *equivalenceDocument) shown(text string) string {
	if d.private {
		return fmt.Sprintf("<%d bytes, sha256 %x>", len(text), sha256.Sum256([]byte(text)))
	}
	return text
}

// prepareEquivalenceDocument seeds source's tree with its anchors, written in-process as
// MarkQuote writes them (pmdoc.MarkRange at pmdoc.FindQuote's range, MarkSpec's mark) beside
// their margin records, and builds the two browser edits from the seed.
func prepareEquivalenceDocument(t *testing.T, source equivalenceSource) *equivalenceDocument {
	t.Helper()
	document := &equivalenceDocument{name: source.name, private: source.private}
	tree, err := pmdoc.Parse(source.markdown)
	if err != nil {
		t.Fatalf("parse the document: %s", document.shown(err.Error()))
	}
	doc := crdt.New(crdt.WithClientID(seedClient))
	fragment := doc.GetXmlFragment(fragmentName)
	marks := doc.GetMap(marksMapName)
	if err := doc.TransactE(func(txn *crdt.Transaction) error { return pmdoc.Update(txn, fragment, tree) }); err != nil {
		t.Fatalf("write the document: %s", document.shown(err.Error()))
	}
	written, err := treeOf(doc)
	if err != nil {
		t.Fatalf("read the written document: %s", document.shown(err.Error()))
	}
	quotes, err := pickQuotes(written, anchorCount+1)
	if err != nil {
		t.Fatalf("choose the anchored sentences: %v", err)
	}
	document.newQuote = quotes[anchorCount]
	for index, quote := range quotes[:anchorCount] {
		anchor := seedAnchor{
			id:       equivalenceCommentID(index + 1),
			kind:     MarkComment,
			quote:    quote,
			body:     fmt.Sprintf("Seeded comment %d on this sentence.", index+1),
			resolved: index == reopenTarget,
		}
		if index == suggestionAnchor {
			anchor.kind, anchor.suggestion = MarkSuggestion, true
		}
		document.anchors = append(document.anchors, anchor)
	}
	if err := doc.TransactE(func(txn *crdt.Transaction) error {
		for _, anchor := range document.anchors {
			found, err := pmdoc.FindQuote(written, anchor.quote, nil, nil)
			if err != nil {
				return err
			}
			spec := MarkSpec{Kind: anchor.kind, ID: anchor.id, By: equivalenceAuthor}
			if err := pmdoc.MarkRange(txn, fragment, found, spec.pmMark()); err != nil {
				return err
			}
			plain, err := toPlain(anchor.record())
			if err != nil {
				return err
			}
			marks.Set(txn, anchor.id, plain)
		}
		return nil
	}); err != nil {
		t.Fatalf("anchor the seeded comments: %s", document.shown(err.Error()))
	}
	marked, err := treeOf(doc)
	if err != nil {
		t.Fatalf("read the anchored document: %s", document.shown(err.Error()))
	}
	for index := range document.anchors {
		anchor := &document.anchors[index]
		at, quote, found := pmdoc.FindMark(marked, string(anchor.kind), anchor.id)
		if !found {
			t.Fatalf("seeded anchor %s is not in the document", anchor.id)
		}
		if anchor.blockID, err = pmdoc.BlockIDForRange(marked, at); err != nil {
			t.Fatalf("place seeded anchor %s: %s", anchor.id, document.shown(err.Error()))
		}
		anchor.quote = quote
	}
	if document.markdown, err = documentMarkdown(marked); err != nil {
		t.Fatalf("render the seeded document: %s", document.shown(err.Error()))
	}
	document.seed = crdt.EncodeStateAsUpdateV1(doc, nil)
	var edited []byte
	document.edit, edited = browserEdit(t, document, document.seed, editClient, func(tree *pmdoc.Node) {
		for _, index := range revisedAnchors {
			document.reviseAnchor(t, tree, index, "Revised: ")
		}
		for _, index := range deletedAnchors {
			anchor := document.anchors[index]
			parent, at := findMarked(tree, anchor.kind, anchor.id)
			if parent == nil {
				t.Fatalf("the browser edit finds no text of anchor %s to delete", anchor.id)
			}
			parent.Children = slices.Delete(parent.Children, at, at+1)
		}
		moved := document.anchors[movedAnchor]
		from := -1
		for index, block := range tree.Children {
			if parent, _ := findMarked(block, moved.kind, moved.id); parent != nil {
				from = index
				break
			}
		}
		if from < 0 {
			t.Fatalf("the browser edit finds no block holding anchor %s to move", moved.id)
		}
		block := tree.Children[from]
		tree.Children = slices.Delete(tree.Children, from, from+1)
		to := 1
		if from <= 1 {
			to = len(tree.Children)
		}
		tree.Children = slices.Insert(tree.Children, to, block)
	})
	document.lateEdit, _ = browserEdit(t, document, edited, lateEditClient, func(tree *pmdoc.Node) {
		document.reviseAnchor(t, tree, lateEditAnchor, "Amended: ")
	})
	t.Logf("%s: %d bytes, sha256 %x, %d anchors (one of them the suggestion)",
		source.name, len(source.markdown), sha256.Sum256([]byte(source.markdown)), len(document.anchors))
	return document
}

// reviseAnchor puts prefix at the start of the text anchor index covers, inside its mark, so its
// quote changes.
func (d *equivalenceDocument) reviseAnchor(t *testing.T, tree *pmdoc.Node, index int, prefix string) {
	t.Helper()
	anchor := d.anchors[index]
	parent, at := findMarked(tree, anchor.kind, anchor.id)
	if parent == nil {
		t.Fatalf("the browser edit finds no text of anchor %s to revise", anchor.id)
	}
	parent.Children[at].Text = prefix + parent.Children[at].Text
}

// findMarked is the node holding the text kind's mark id covers, and that text's index in it, or
// nil.
func findMarked(node *pmdoc.Node, kind MarkKind, id string) (*pmdoc.Node, int) {
	for index, child := range node.Children {
		if child.Type == "text" {
			for _, mark := range child.Marks {
				if mark.Type == string(kind) && mark.Attrs["id"] == id {
					return node, index
				}
			}
			continue
		}
		if parent, at := findMarked(child, kind, id); parent != nil {
			return parent, at
		}
	}
	return nil, -1
}

// browserEdit makes edit in a browser's own document, client, synced from state, and returns the
// update the browser sends its room and the browser's whole state after it. Both sides' rooms are
// sent these same bytes.
func browserEdit(t *testing.T, document *equivalenceDocument, state []byte, client crdt.ClientID, edit func(*pmdoc.Node)) (update, after []byte) {
	t.Helper()
	peer := newDocumentCopy(crdt.WithClientID(client))
	if err := crdt.ApplyUpdateV1(peer, state, nil); err != nil {
		t.Fatalf("sync the browser: %v", err)
	}
	synced := peer.StateVector()
	tree, err := treeOf(peer)
	if err != nil {
		t.Fatalf("read the browser's document: %s", document.shown(err.Error()))
	}
	edit(tree)
	fragment := peer.GetXmlFragment(fragmentName)
	if err := peer.TransactE(func(txn *crdt.Transaction) error { return pmdoc.Update(txn, fragment, tree) }); err != nil {
		t.Fatalf("make the browser's edit: %s", document.shown(err.Error()))
	}
	return crdt.EncodeStateAsUpdateV1(peer, synced), crdt.EncodeStateAsUpdateV1(peer, nil)
}

// pickQuotes is count sentences of tree's paragraphs - outside tables and typed blocks - that each
// match once in the document, spread evenly through it in document order.
func pickQuotes(tree *pmdoc.Node, count int) ([]string, error) {
	var candidates []string
	var visit func(*pmdoc.Node)
	visit = func(node *pmdoc.Node) {
		switch node.Type {
		case "doc", "bullet_list", "ordered_list", "list_item", "blockquote":
			for _, child := range node.Children {
				visit(child)
			}
		case "paragraph":
			for _, child := range node.Children {
				if child.Type == "text" && len(child.Marks) == 0 {
					candidates = append(candidates, sentencesOf(child.Text)...)
				}
			}
		}
	}
	visit(tree)
	if len(candidates) < 2*count {
		return nil, fmt.Errorf("the document holds %d candidate sentences, want at least %d", len(candidates), 2*count)
	}
	picked := make([]string, 0, count)
	next := 0
	for slot := range count {
		found := false
		for index := max(next, (2*slot+1)*len(candidates)/(2*count)); index < len(candidates); index++ {
			if slices.Contains(picked, candidates[index]) {
				continue
			}
			if _, err := pmdoc.FindQuote(tree, candidates[index], nil, nil); err != nil {
				continue
			}
			picked, next, found = append(picked, candidates[index]), index+1, true
			break
		}
		if !found {
			return nil, fmt.Errorf("found %d unique sentences of the %d wanted", len(picked), count)
		}
	}
	return picked, nil
}

// sentencesOf is each whole sentence of text between 40 and 240 bytes long.
func sentencesOf(text string) []string {
	var sentences []string
	for _, piece := range strings.SplitAfter(text, ". ") {
		piece = strings.TrimSpace(piece)
		if len(piece) >= 40 && len(piece) <= 240 && strings.HasSuffix(piece, ".") {
			sentences = append(sentences, piece)
		}
	}
	return sentences
}

// flowIDs names one flow's document: the issue it is on and the artifact, the same on both sides.
type flowIDs struct {
	number   int
	issue    string
	artifact string
}

func flowIDsFor(number int) flowIDs {
	return flowIDs{number: number, issue: fmt.Sprintf("EQ-%d", number), artifact: fmt.Sprintf("00000000-0000-4000-a000-%012d", number)}
}

// equivalenceSide is one service on a database of its own, writing margin records with write.
type equivalenceSide struct {
	name     string
	margin   bool
	write    marginWriter
	database *store.Store
	service  *Service
	// classes records the class the side's rooms record for each update they apply, installed at
	// the side's first room load (recordClasses).
	classes *recordedClasses
}

func newEquivalenceSide(t *testing.T, name string, margin bool, write marginWriter) *equivalenceSide {
	t.Helper()
	database := storetest.Open(t)
	service, _ := newTestServiceInstance(t, database)
	if _, err := database.Pool.Exec(context.Background(), `insert into projects (key, name) values ('EQ', 'Equivalence')`); err != nil {
		t.Fatalf("create the %s's project: %v", name, err)
	}
	return &equivalenceSide{name: name, margin: margin, write: write, database: database, service: service}
}

// equivalenceFlow is one route's document calls, run in a joined transaction of its own.
type equivalenceFlow struct {
	name string
	// corrupt gives the document a stored tree outside the schema, written while no room holds
	// it, in place of the browser edit every other flow starts from.
	corrupt func(*testing.T, *store.Store, string)
	// wantErr is the refusal the flow ends in, nil for one that commits.
	wantErr error
	// refreshing names the operations whose anchor refresh must write: the browser edit before
	// the flow moved anchors for them to refresh, so a refresh there that writes nothing tests
	// nothing.
	refreshing []string
	run        func(*flowRun) error
}

// equivalenceFlows are the flows each document is put through: the comment routes that write a
// margin record (api/comment_create.go, api/comment_actions.go), both after an operation of their
// own transaction left a rendering to reuse and as the transaction's first document operation,
// the record bound's refusal on each of those two paths, and a margin write on each stored tree
// outside the schema.
func equivalenceFlows() []equivalenceFlow {
	longBody := strings.Repeat("A margin record past its bound. ", maxMarginRecordBytes/32+8)
	flows := []equivalenceFlow{
		{name: "an anchored comment", refreshing: []string{"MarkQuote"}, run: func(r *flowRun) error {
			return r.anchoredComment("A new comment on this sentence.", false)
		}},
		{name: "an anchored comment beside a browser edit", refreshing: []string{"MarkQuote", "ProjectMark"}, run: func(r *flowRun) error {
			return r.anchoredComment("A new comment written while a browser edits.", true)
		}},
		{name: "a reply", refreshing: []string{"ProjectMark"}, run: func(r *flowRun) error {
			return r.reply("A reply to the seeded comment.")
		}},
		{name: "a resolve", refreshing: []string{"ProjectMark"}, run: (*flowRun).resolve},
		{name: "a reopen", refreshing: []string{"ProjectMark"}, run: (*flowRun).reopen},
		{name: "an accepted suggestion", refreshing: []string{"AcceptSuggestion"}, run: (*flowRun).accept},
		{name: "a reply past the record bound", wantErr: ErrDocumentTooLarge, run: func(r *flowRun) error {
			return r.reply(longBody)
		}},
		{name: "an anchored comment past the record bound", wantErr: ErrDocumentTooLarge, refreshing: []string{"MarkQuote"}, run: func(r *flowRun) error {
			return r.anchoredComment(longBody, false)
		}},
	}
	for _, corruption := range []struct {
		name    string
		corrupt func(*testing.T, *store.Store, string)
	}{
		{"a tree the reader refuses", appendSchemaInvalidUpdate},
		{"a tree only the renderer refuses", func(t *testing.T, database *store.Store, artifactID string) {
			if err := AppendRenderOnlySchemaViolationForTest(context.Background(), database, artifactID); err != nil {
				t.Fatal(err)
			}
		}},
		{"a paragraph whose text holds an embed", appendTextEmbed},
		{"a mark nested past the bound", appendNestedLink},
	} {
		flows = append(flows, equivalenceFlow{
			name: "a reply on " + corruption.name, corrupt: corruption.corrupt, wantErr: ErrDocSchema,
			run: func(r *flowRun) error { return r.reply("A reply to the seeded comment.") },
		})
	}
	return flows
}

// The stored trees outside the schema a document can come to hold, built as
// api/schema_repair_test.go's outsideSchemaCorruptions builds them, written while no room holds
// the document. A tree only the renderer refuses is AppendRenderOnlySchemaViolationForTest's.

// appendSchemaInvalidUpdate appends the element a crafted client can persist, which the tree
// reader refuses.
func appendSchemaInvalidUpdate(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	doc := crdt.New()
	fragment := doc.GetXmlFragment(fragmentName)
	doc.Transact(func(transaction *crdt.Transaction) {
		fragment.InsertElement(transaction, 0, crdt.NewYXmlElement("callout"))
	})
	if _, err := NewPgVersioned(database).AppendUpdate(context.Background(), artifactID, crdt.EncodeStateAsUpdateV1(doc, nil)); err != nil {
		t.Fatalf("append crafted document update: %v", err)
	}
}

// appendTextEmbed appends an embed in paragraph text, which the tree reader refuses and
// pmdoc.Update cannot diff against.
func appendTextEmbed(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	versioned := NewPgVersioned(database)
	loaded, err := versioned.Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load document before crafted embed: %v", err)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		t.Fatalf("decode document before crafted embed: %v", err)
	}
	before := doc.StateVector()
	var text *crdt.YXmlText
	for _, block := range doc.GetXmlFragment(fragmentName).Children() {
		if paragraph, ok := block.(*crdt.YXmlElement); ok && paragraph.NodeName == "paragraph" {
			for _, child := range paragraph.Children() {
				if found, ok := child.(*crdt.YXmlText); ok {
					text = found
				}
			}
		}
	}
	if text == nil {
		t.Fatal("document holds no paragraph text to embed into")
	}
	doc.Transact(func(transaction *crdt.Transaction) {
		text.InsertEmbed(transaction, 0, map[string]any{"image": "crafted"}, nil)
	})
	if _, err := versioned.AppendUpdate(context.Background(), artifactID, crdt.EncodeStateAsUpdateV1(doc, before)); err != nil {
		t.Fatalf("append crafted embed: %v", err)
	}
}

// appendNestedLink appends a link over the first text whose value nests 100 arrays deep, one more
// than a mark's attribute may.
func appendNestedLink(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	versioned := NewPgVersioned(database)
	loaded, err := versioned.Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load document before crafted link: %v", err)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		t.Fatalf("decode document before crafted link: %v", err)
	}
	if _, err := versioned.AppendUpdate(context.Background(), artifactID, docstest.NestedLinkUpdate(t, doc, docstest.NestedValue(100))); err != nil {
		t.Fatalf("append crafted link: %v", err)
	}
}

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
		flow.corrupt(t, sides[0].database, ids.artifact)
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

// execer is a pool or a transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// setUp creates the flow's document on the side - its issue, its artifact, its first version and
// the seed - with the seeded comments' rows, and, but for a flow that starts from a corrupted tree,
// loads its room and lands the browser edit that moves its anchors.
func (side *equivalenceSide) setUp(t *testing.T, document *equivalenceDocument, flow equivalenceFlow, ids flowIDs) {
	t.Helper()
	ctx := context.Background()
	pool := side.database.Pool
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`insert into issues (key, project_key, number, title, created_by, rank, created_at, updated_at)
			values ($1, 'EQ', $2, 'Equivalence', '{"kind":"user","id":"alice"}', 'U', $3, $3)`, []any{ids.issue, ids.number, seededAt}},
		{`insert into artifacts (id, issue_key, project_key, slug, name, kind, is_primary, created_by, created_at)
			values ($1, $2, 'EQ', 'spec', $3, 'doc', false, '{"kind":"user","id":"alice"}', $4)`, []any{ids.artifact, ids.issue, equivalenceArtifactName, seededAt}},
		{`insert into artifact_versions (artifact_id, number, markdown, authors, created_at)
			values ($1, 1, $2, '[{"kind":"user","id":"alice"}]', $3)`, []any{ids.artifact, document.markdown, seededAt}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("create the flow's document: %v", err)
		}
	}
	if _, err := NewPgVersioned(side.database).AppendUpdate(ctx, ids.artifact, document.seed); err != nil {
		t.Fatalf("store the seed: %v", err)
	}
	// The first version holds the seed, as a document's first version does (SeedText's caller).
	if _, err := pool.Exec(ctx, `
		update artifact_versions set doc_update_version = (select max(version) from doc_updates where artifact_id = $1)
		where artifact_id = $1 and number = 1
	`, ids.artifact); err != nil {
		t.Fatalf("align the first version with the seed: %v", err)
	}
	for _, anchor := range document.anchors {
		stored := model.Anchor{ArtifactID: ids.artifact, MarkID: anchor.id, Version: 1, Quote: anchor.quote}
		if anchor.blockID != "" {
			stored.BlockID = new(anchor.blockID)
		}
		var suggestion *model.Suggestion
		if anchor.suggestion {
			suggestion = &model.Suggestion{ReplaceWith: suggestedWording}
		}
		var resolvedBy *model.Actor
		var resolvedAt *time.Time
		if anchor.resolved {
			resolvedBy, resolvedAt = &equivalenceAuthor, &seededAt
		}
		if err := insertComment(ctx, pool, ids, anchor.id, nil, equivalenceAuthor, anchor.body, &stored, suggestion, resolvedBy, resolvedAt, seededAt); err != nil {
			t.Fatalf("insert seeded comment %s: %v", anchor.id, err)
		}
	}
	if flow.corrupt != nil {
		return
	}
	if side.classes == nil {
		side.classes, _ = recordClasses(t, side.service, ids.artifact)
	} else if err := side.service.warmLiveDocument(ctx, ids.artifact); err != nil {
		t.Fatalf("load the flow's room: %v", err)
	}
	side.applyBrowserEdit(t, ids.artifact, document.edit, browserEditor)
	waitForLandedAppends(t, side.service, ids.artifact)
}

// insertComment inserts a comment row as the comment routes do, with every clock fixed.
func insertComment(ctx context.Context, db execer, ids flowIDs, id string, replyTo *string, author model.Actor, body string, anchor *model.Anchor, suggestion *model.Suggestion, resolvedBy *model.Actor, resolvedAt *time.Time, createdAt time.Time) error {
	encodedAuthor, err := json.Marshal(author)
	if err != nil {
		return err
	}
	var encodedAnchor, encodedSuggestion, encodedResolver []byte
	if anchor != nil {
		if encodedAnchor, err = json.Marshal(anchor); err != nil {
			return err
		}
	}
	if suggestion != nil {
		if encodedSuggestion, err = json.Marshal(suggestion); err != nil {
			return err
		}
	}
	if resolvedBy != nil {
		if encodedResolver, err = json.Marshal(resolvedBy); err != nil {
			return err
		}
	}
	_, err = db.Exec(ctx, `
		insert into comments (id, issue_key, author, body, anchor, reply_to, suggestion, resolved, resolved_by, resolved_at, created_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, id, ids.issue, encodedAuthor, body, encodedAnchor, replyTo, encodedSuggestion, resolvedBy != nil, encodedResolver, resolvedAt, createdAt)
	return err
}

// applyBrowserEdit sends update to the side's room as a browser connected as editor sends it.
func (side *equivalenceSide) applyBrowserEdit(t *testing.T, artifactID string, update []byte, editor model.Actor) {
	t.Helper()
	connection := side.service.nextConnection.Add(1)
	side.service.addConnection(artifactID, connection, editor)
	defer side.service.removeConnection(artifactID, connection)
	var applyErr error
	if err := side.service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		applyErr = crdt.ApplyUpdateV1(doc, update, "peer")
	}); err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		t.Fatalf("send %s's edit to the room: %v", editor.ID, err)
	}
	if applyErr != nil {
		t.Fatalf("apply %s's edit: %v", editor.ID, applyErr)
	}
}

// cleanUp closes the flow's room and deletes its comment rows, whose ids the next flow reuses.
func (side *equivalenceSide) cleanUp(t *testing.T, ids flowIDs) {
	t.Helper()
	if err := side.service.Evict(context.Background(), ids.artifact); err != nil {
		t.Errorf("close the %s's room: %v", side.name, err)
	}
	if _, err := side.database.Pool.Exec(context.Background(), `delete from comments where issue_key = $1`, ids.issue); err != nil {
		t.Errorf("delete the %s's comments: %v", side.name, err)
	}
}

// storedRow is one doc_updates row.
type storedRow struct {
	version        int64
	update         []byte
	contentChanged bool
}

func (r storedRow) equal(o storedRow) bool {
	return r.version == o.version && r.contentChanged == o.contentChanged && string(r.update) == string(o.update)
}

// storedHistory is artifactID's stored history, oldest first.
func storedHistory(t *testing.T, database *store.Store, artifactID string) []storedRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select version, update, content_changed from doc_updates where artifact_id = $1 order by version
	`, artifactID)
	if err != nil {
		t.Fatalf("read the stored history: %v", err)
	}
	history, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedRow, error) {
		var stored storedRow
		return stored, row.Scan(&stored.version, &stored.update, &stored.contentChanged)
	})
	if err != nil {
		t.Fatalf("read the stored history: %v", err)
	}
	return history
}

// copyHistory copies into to, row for row, the rows of from's history past those to holds.
func copyHistory(t *testing.T, from, to *store.Store, artifactID string) {
	t.Helper()
	held := len(storedHistory(t, to, artifactID))
	for _, row := range storedHistory(t, from, artifactID)[held:] {
		if _, err := to.Pool.Exec(context.Background(), `
			insert into doc_updates (artifact_id, version, update, content_changed) values ($1, $2, $3, $4)
		`, artifactID, row.version, row.update, row.contentChanged); err != nil {
			t.Fatalf("copy the corruption to the other side: %v", err)
		}
	}
}

// marginPath is how a margin write came by the rendering it records: reused from the
// transaction's previous operation, or read and rendered from its fork.
type marginPath string

const (
	reusedRendering marginPath = "reused the rendering its transaction kept"
	readRendering   marginPath = "read and rendered the document"
)

// flowRun is one flow on one side: the transaction it runs in and what it observes.
type flowRun struct {
	t            *testing.T
	side         *equivalenceSide
	document     *equivalenceDocument
	ids          flowIDs
	ctx          context.Context
	tx           pgx.Tx
	ledger       *Ledger
	observed     *flowObservation
	marginWrites int
}

// operation runs one document call of the flow, recording its result and the events it collected.
func (r *flowRun) operation(name string, call func() (any, error)) error {
	before := len(r.ledger.events)
	result, err := call()
	operation := observedOperation{Name: name, Result: encodeObserved(result)}
	for _, event := range r.ledger.events[before:] {
		r.observed.Sequences = append(r.observed.Sequences, event.Seq)
		event.ID, event.Seq, event.CreatedAt = 0, 0, time.Time{}
		operation.Events = append(operation.Events, encodeObserved(event))
	}
	// refreshAnchors reads the open anchors in no order, so one operation's events are compared
	// as a set; the operations keep their order.
	slices.Sort(operation.Events)
	r.observed.Operations = append(r.observed.Operations, operation)
	return err
}

// project writes the margin record as the side does, and holds the margin write to the path the
// flow expects it to take.
func (r *flowRun) project(markID string, record MarkRecord, path marginPath) error {
	return r.operation("ProjectMark", func() (any, error) {
		var kept *pmdoc.Node
		if write := joinedLiveWrite(r.ctx, r.ids.artifact); write != nil {
			kept = write.tree
		}
		err := r.side.write(r.ctx, r.side.service, r.ids.artifact, markID, record, flowActor)
		if err != nil {
			return nil, err
		}
		r.marginWrites++
		if r.side.margin {
			took := readRendering
			if kept != nil && joinedLiveWrite(r.ctx, r.ids.artifact).tree == kept {
				took = reusedRendering
			}
			if took != path {
				r.t.Errorf("the margin write %s, want it to have %s", took, path)
			}
		}
		return nil, nil
	})
}

// anchoredComment is a new anchored comment as the create route writes it: the anchor
// (resolveAnchor's MarkQuote and SnapshotVersion), the comment's row, and its margin record, which
// reuses the rendering MarkQuote kept. With lateEdit a browser edit lands in the room between the
// row and the margin record, so the fork moves and the kept rendering no longer describes it.
func (r *flowRun) anchoredComment(body string, lateEdit bool) error {
	var anchored Anchored
	if err := r.operation("MarkQuote", func() (any, error) {
		var err error
		anchored, err = r.side.service.MarkQuote(r.ctx, r.ids.artifact, MarkSpec{Kind: MarkComment, ID: newCommentID, By: flowActor}, r.document.newQuote, nil)
		return anchored, err
	}); err != nil {
		return err
	}
	var snapshot VersionResult
	if err := r.operation("SnapshotVersion", func() (any, error) {
		var err error
		snapshot, err = r.side.service.SnapshotVersion(r.ctx, r.ids.artifact, flowActor)
		return comparableVersion(snapshot), err
	}); err != nil {
		return err
	}
	anchor := model.Anchor{ArtifactID: r.ids.artifact, MarkID: newCommentID, Version: snapshot.Version.Number, Quote: anchored.Quote}
	if anchored.BlockID != "" {
		anchor.BlockID = new(anchored.BlockID)
	}
	if err := insertComment(r.ctx, r.tx, r.ids, newCommentID, nil, flowActor, body, &anchor, nil, nil, nil, flowAt); err != nil {
		return err
	}
	path := reusedRendering
	if lateEdit {
		r.side.applyBrowserEdit(r.t, r.ids.artifact, r.document.lateEdit, lateEditor)
		path = readRendering
	}
	return r.project(newCommentID, MarkRecord{
		Kind: "comment", By: ActorRef(flowActor), CreatedAt: flowAt.Format(time.RFC3339Nano), Text: body, Replies: []MarkReply{},
	}, path)
}

// reply is a reply to an anchored comment as the create route writes it: the reply's row, then
// the root's margin record with the reply in it, the transaction's first document operation.
func (r *flowRun) reply(body string) error {
	root := r.document.anchors[replyRoot]
	if err := insertComment(r.ctx, r.tx, r.ids, replyCommentID, &root.id, flowActor, body, nil, nil, nil, nil, repliedAt); err != nil {
		return err
	}
	record := root.record()
	record.Replies = []MarkReply{{By: ActorRef(flowActor), Text: body, At: repliedAt.Format(time.RFC3339Nano)}}
	return r.project(root.id, record, readRendering)
}

// resolve is the resolve route on an open anchored comment.
func (r *flowRun) resolve() error {
	target := r.document.anchors[resolveTarget]
	if err := r.setResolved(target.id, true); err != nil {
		return err
	}
	record := target.record()
	record.Resolved = true
	return r.project(target.id, record, readRendering)
}

// reopen is the reopen route on a resolved anchored comment, whose anchor the browser edit moved
// while it was resolved, so only the reopen's refresh brings it up to date.
func (r *flowRun) reopen() error {
	target := r.document.anchors[reopenTarget]
	if err := r.setResolved(target.id, false); err != nil {
		return err
	}
	record := target.record()
	record.Resolved = false
	return r.project(target.id, record, readRendering)
}

func (r *flowRun) setResolved(id string, resolved bool) error {
	if resolved {
		resolver, err := json.Marshal(flowActor)
		if err != nil {
			return err
		}
		_, err = r.tx.Exec(r.ctx, `update comments set resolved = true, resolved_by = $2, resolved_at = $3 where id = $1`, id, resolver, flowAt)
		return err
	}
	_, err := r.tx.Exec(r.ctx, `update comments set resolved = false, resolved_by = null, resolved_at = null where id = $1`, id)
	return err
}

// accept is the accept route on the seeded suggestion: its kind (SuggestionKind), the text change
// (AcceptSuggestion), the row, the margin record, which reuses the rendering the text change kept,
// and the named version the decision produces.
func (r *flowRun) accept() error {
	target := r.document.anchors[suggestionAnchor]
	var kind string
	if err := r.operation("SuggestionKind", func() (any, error) {
		var err error
		kind, err = r.side.service.SuggestionKind(r.ctx, r.ids.artifact, target.id)
		return kind, err
	}); err != nil {
		return err
	}
	if err := r.operation("AcceptSuggestion", func() (any, error) {
		return nil, r.side.service.AcceptSuggestion(r.ctx, r.ids.artifact, target.id, suggestedWording, flowActor)
	}); err != nil {
		return err
	}
	accepted := true
	suggestion, err := json.Marshal(model.Suggestion{ReplaceWith: suggestedWording, Accepted: &accepted})
	if err != nil {
		return err
	}
	resolver, err := json.Marshal(flowActor)
	if err != nil {
		return err
	}
	if _, err := r.tx.Exec(r.ctx, `
		update comments set resolved = true, resolved_by = $2, resolved_at = $3, suggestion = $4
		where id = $1 and suggestion->>'accepted' is null
	`, target.id, resolver, flowAt, suggestion); err != nil {
		return err
	}
	record := target.record()
	record.Kind, record.Resolved, record.Status = kind, true, "accepted"
	if err := r.project(target.id, record, reusedRendering); err != nil {
		return err
	}
	return r.operation("NamedVersion", func() (any, error) {
		named, err := r.side.service.NamedVersion(r.ctx, r.ids.artifact, target.body, flowActor)
		return comparableVersion(named), err
	})
}

// comparableVersion is result without the clock its row took from the database.
func comparableVersion(result VersionResult) VersionResult {
	result.Version.CreatedAt = time.Time{}
	return result
}

// flowObservation is everything one side's flow left that the two sides compare.
type flowObservation struct {
	Err        *observedError
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
	observed.Err = observeError(flowErr)
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
			if effect.DocumentToken, err = nodeToken(tree); err != nil {
				return err
			}
			effect.BlockTokens, err = blockTokens(tree)
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
	differ("the flow's error", margin.Err, whole.Err)
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
		t.Errorf("the %s's flow failed (%s), so it compares no write", side, document.shown(observed.Err.Message))
	case flow.wantErr != nil && observed.Err == nil:
		t.Errorf("the %s's flow committed, want the refusal %v", side, flow.wantErr)
	case flow.wantErr != nil && !errors.Is(observed.Err.asError(), flow.wantErr):
		t.Errorf("the %s's flow ended in %s, want the refusal %v", side, document.shown(observed.Err.Message), flow.wantErr)
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

// asError is an error that is each sentinel the observed error was.
func (o *observedError) asError() error {
	var matched []error
	for _, name := range o.Is {
		switch name {
		case "ErrDocOutsideSchema":
			matched = append(matched, ErrDocOutsideSchema)
		case "ErrDocSchema":
			matched = append(matched, ErrDocSchema)
		case "ErrDocumentTooLarge":
			matched = append(matched, ErrDocumentTooLarge)
		case "websocket.ErrNoChanges":
			matched = append(matched, websocket.ErrNoChanges)
		}
	}
	return errors.Join(matched...)
}
