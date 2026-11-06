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
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

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

// historyRow is one doc_updates row.
type historyRow struct {
	version        int64
	update         []byte
	contentChanged bool
}

func (r historyRow) equal(o historyRow) bool {
	return r.version == o.version && r.contentChanged == o.contentChanged && string(r.update) == string(o.update)
}

// storedHistory is artifactID's stored history, oldest first.
func storedHistory(t *testing.T, database *store.Store, artifactID string) []historyRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select version, update, content_changed from doc_updates where artifact_id = $1 order by version
	`, artifactID)
	if err != nil {
		t.Fatalf("read the stored history: %v", err)
	}
	history, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (historyRow, error) {
		var stored historyRow
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
