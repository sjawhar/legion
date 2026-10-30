package docs

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// stormDocument is a document holding every kind of top-level block the schema carries, with a
// rule that is not first, tables with and without spans, footnote definitions and references,
// asks, callouts, nested lists of both markers, empty-looking paragraphs between lists, and code.
func stormDocument() string {
	var b strings.Builder
	b.WriteString("# Storm specification\n\n")
	for section := range 8 {
		fmt.Fprintf(&b, "## Section %d\n\n", section)
		fmt.Fprintf(&b, "Paragraph %d opens the section with words to type into, a reference[^fn-%d], a [link](https://example.com/%d \"title\"), www.example.com/%d, `code`, **bold**, _italic_ and ~~struck~~ text.\n\n", section, section, section, section)
		fmt.Fprintf(&b, "- Bullet %d one\n  - Bullet %d nested\n    1. Ordered %d deep\n    2. Ordered %d deeper\n- [ ] Task %d open\n- [x] Task %d done\n\n", section, section, section, section, section, section)
		fmt.Fprintf(&b, "* Star %d one\n* Star %d two\n\n", section, section)
		fmt.Fprintf(&b, "%d. Ordered %d one\n%d. Ordered %d two\n\n", section+1, section, section+2, section)
		fmt.Fprintf(&b, "| Head %d | Value | Note |\n| --- | :---: | ---: |\n| cell %d a | %d | note %d a |\n| cell %d b | %d | note %d b |\n\n", section, section, section, section, section, section+1, section)
		fmt.Fprintf(&b, "> Quote %d holds words.\n>\n> - Quoted bullet %d\n\n", section, section)
		fmt.Fprintf(&b, "```go\nfunc storm%d() string {\n\treturn \"storm %d\"\n}\n```\n\n", section, section)
		fmt.Fprintf(&b, ":::callout{#callout-%d kind=\"note\" title=\"Note %d\"}\nCallout %d holds words a browser types into.\n:::\n\n", section, section, section)
		fmt.Fprintf(&b, ":::ask{#ask-%d urgency=\"high\" multiple=\"false\" state=\"open\"}\nQuestion %d waits for an answer?\n\n- REST: answer %d one\n- gRPC: answer %d two\n:::\n\n", section, section, section, section)
		if section%3 == 1 {
			b.WriteString("---\n\n")
		}
		fmt.Fprintf(&b, "[^fn-%d]: Footnote %d defines the reference.\n\n", section, section)
	}
	return b.String()
}

// stormPeer is a browser's editor on a room, like typingBrowser, that can hold an edit back before
// it sends it, so two peers can edit one state concurrently and the room merge both.
type stormPeer struct {
	t          *testing.T
	service    *Service
	artifactID string
	doc        *crdt.Doc
	fragment   *crdt.YXmlFragment
	// pending is the state vector the peer's unsent edits started from, nil with none.
	pending crdt.StateVector
	// sent is every update the room has been sent, by its bytes, shared by the peers: a delete
	// set names the client whose structs it deletes, not the peer that deleted them, so two
	// peers deleting one range encode the same bytes.
	sent map[string]bool
}

func newStormPeer(t *testing.T, service *Service, artifactID string, conn uint64, actor model.Actor, sent map[string]bool) *stormPeer {
	t.Helper()
	if err := service.warmLiveDocument(t.Context(), artifactID); err != nil {
		t.Fatalf("warm the room for %s: %v", actor.ID, err)
	}
	service.addConnection(artifactID, conn, actor)
	t.Cleanup(func() { service.removeConnection(artifactID, conn) })
	p := &stormPeer{t: t, service: service, artifactID: artifactID, doc: crdt.New(), sent: sent}
	p.fragment = p.doc.GetXmlFragment(fragmentName)
	p.sync()
	return p
}

// sync takes in what the room holds that the peer's document does not.
func (p *stormPeer) sync() {
	p.t.Helper()
	room := p.service.srv.GetDoc(p.artifactID)
	if err := crdt.ApplyUpdateV1(p.doc, crdt.EncodeStateAsUpdateV1(room, p.doc.StateVector()), nil); err != nil {
		p.t.Fatalf("sync the peer from the room: %v", err)
	}
}

// edit makes edit in the peer's own document, in one transaction, without sending it.
func (p *stormPeer) edit(edit func(txn *crdt.Transaction)) {
	p.t.Helper()
	if p.pending == nil {
		p.pending = p.doc.StateVector()
	}
	p.doc.Transact(edit)
}

// flush sends the room every edit the peer has held back, as one update, unless the peer has sent
// those very bytes before: an edit that creates no struct and only deletes again what the peer has
// already deleted encodes to the update it sent then, and applying one twice moves the room
// nowhere, while the room would log and classify a second commit for it, which the shadow reads as
// two classes of one update.
func (p *stormPeer) flush() {
	p.t.Helper()
	if p.pending == nil {
		return
	}
	before := p.pending
	p.pending = nil
	room := p.service.srv.GetDoc(p.artifactID)
	update := crdt.EncodeStateAsUpdateV1(p.doc, before)
	if p.sent[string(update)] {
		return
	}
	p.sent[string(update)] = true
	if err := crdt.ApplyUpdateV1(room, update, "peer"); err != nil {
		p.t.Fatalf("apply the peer's update to the room: %v", err)
	}
}

// send is edit then flush: the peer's usual keystroke.
func (p *stormPeer) send(edit func(txn *crdt.Transaction)) {
	p.t.Helper()
	p.edit(edit)
	p.flush()
}

// editTree sends the room edit of the peer's tree, written as pmdoc.Update writes a tree.
func (p *stormPeer) editTree(edit func(*pmdoc.Node) *pmdoc.Node) {
	p.t.Helper()
	tree, err := treeOf(p.doc)
	if err != nil {
		p.t.Fatalf("read the peer's document: %v", err)
	}
	want := edit(tree)
	if want == nil {
		return
	}
	pmdoc.EnsureBlockIDs(want)
	var updateErr error
	p.send(func(txn *crdt.Transaction) { updateErr = pmdoc.Update(txn, p.fragment, want) })
	if updateErr != nil {
		p.t.Fatalf("edit the peer's document: %v", updateErr)
	}
}

// texts is every text node under the peer's document fragment, in document order.
func (p *stormPeer) texts() []*crdt.YXmlText {
	var found []*crdt.YXmlText
	var walk func(parent *crdt.YXmlFragment)
	walk = func(parent *crdt.YXmlFragment) {
		for _, child := range parent.Children() {
			switch node := child.(type) {
			case *crdt.YXmlText:
				found = append(found, node)
			case *crdt.YXmlElement:
				walk(&node.YXmlFragment)
			}
		}
	}
	walk(p.fragment)
	return found
}

// markableTexts is every text node under the peer's document fragment that a mark may cover: the
// schema carries no mark inside a code block.
func (p *stormPeer) markableTexts() []*crdt.YXmlText {
	var found []*crdt.YXmlText
	var walk func(parent *crdt.YXmlFragment)
	walk = func(parent *crdt.YXmlFragment) {
		for _, child := range parent.Children() {
			switch node := child.(type) {
			case *crdt.YXmlText:
				found = append(found, node)
			case *crdt.YXmlElement:
				if node.NodeName == "code_block" {
					continue
				}
				walk(&node.YXmlFragment)
			}
		}
	}
	walk(p.fragment)
	return found
}

// stormWords are the words the storm types.
var stormWords = strings.Fields("one two three four five six seven eight nine ten alpha beta gamma delta")

// stormBlocks are the markdown blocks the storm inserts.
var stormBlocks = []string{
	"A plain inserted paragraph.\n",
	"## An inserted heading\n",
	"- Inserted bullet one\n- Inserted bullet two\n",
	"1. Inserted ordered one\n2. Inserted ordered two\n",
	"```text\ninserted code\n```\n",
	"> An inserted quote.\n",
	"| Inserted | Table |\n| --- | --- |\n| a | b |\n",
	"---\n",
	":::callout{kind=\"warning\" title=\"Inserted\"}\nAn inserted callout.\n:::\n",
	":::ask{urgency=\"low\" multiple=\"false\" state=\"open\"}\nAn inserted question?\n:::\n",
	"[^inserted]: An inserted footnote definition.\n",
	"A paragraph that references the inserted footnote[^inserted].\n",
	"\u00a0\n",
	"A paragraph with *emphasis*, `code`, a www.example.com/inserted link and \u00e9\u00e8\u00ea unicode.\n",
}

var stormMarks = []string{"strong", "emphasis", "inlineCode", "strike_through", "proofComment"}

// TestRenderReuseARandomisedEditStorm drives a live room with randomised browser edits - typing,
// deleting, marking, inserting, deleting, moving and replacing top-level blocks, several blocks in
// one transaction, attribute changes, margin writes, and two peers editing one state concurrently
// - and holds the replica's rendering and every update's class to the shadow's independent whole
// render after every commit. The touched blocks each update's splice is given come from the
// replica's own change finder here, not from the test, which is what the pmdoc differential cannot
// reach.
func TestRenderReuseARandomisedEditStorm(t *testing.T) {
	seeds, edits := []uint64{1, 2, 3, 4, 5, 6, 7, 8}, 320
	if stormRaceEnabled || testing.Short() {
		seeds, edits = []uint64{1, 2}, 80
	}
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			stormRound(t, seed, edits)
		})
	}
}

func stormRound(t *testing.T, seed uint64, edits int) {
	t.Helper()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	sh := watchRenders(t, service, artifactID)
	seedServiceText(t, service, artifactID, stormDocument())
	sent := map[string]bool{}
	bob := newStormPeer(t, service, artifactID, 1, reuseBob, sent)
	alice := newStormPeer(t, service, artifactID, 2, reuseAlice, sent)
	m := sh.measure()
	rng := rand.New(rand.NewPCG(644, seed))

	word := func() string { return stormWords[rng.IntN(len(stormWords))] }
	pickText := func(p *stormPeer) *crdt.YXmlText {
		texts := p.texts()
		if len(texts) == 0 {
			return nil
		}
		return texts[rng.IntN(len(texts))]
	}
	pickMarkable := func(p *stormPeer) *crdt.YXmlText {
		texts := p.markableTexts()
		if len(texts) == 0 {
			return nil
		}
		return texts[rng.IntN(len(texts))]
	}
	typeInto := func(p *stormPeer, text *crdt.YXmlText) {
		length := text.Len()
		at := rng.IntN(length + 1)
		p.send(func(txn *crdt.Transaction) { text.Insert(txn, at, word(), nil) })
	}

	kinds := map[string]int{}
	recovered := 0
	for edit := range edits {
		p := bob
		if rng.IntN(4) == 0 {
			p = alice
		}
		p.sync()
		kind := rng.IntN(12)
		switch kind {
		case 0, 1, 2: // type into a text node
			text := pickText(p)
			if text == nil {
				continue
			}
			typeInto(p, text)
			kinds["type"]++
		case 3: // delete characters
			text := pickText(p)
			if text == nil || text.Len() < 3 {
				continue
			}
			at := rng.IntN(text.Len() - 2)
			p.send(func(txn *crdt.Transaction) { text.Delete(txn, at, 1+rng.IntN(2)) })
			kinds["delete"]++
		case 4: // mark or unmark a run
			text := pickMarkable(p)
			if text == nil || text.Len() < 4 {
				continue
			}
			at := rng.IntN(text.Len() - 3)
			length := 1 + rng.IntN(min(3, text.Len()-at))
			mark := stormMarks[rng.IntN(len(stormMarks))]
			var value any = map[string]any{}
			if mark == "proofComment" {
				// An anchor mark: the rendering carries none of it, so the update changes no
				// content while it does touch the block.
				value = map[string]any{"id": fmt.Sprintf("c-%d-%d", seed, edit)}
			} else if rng.IntN(3) == 0 {
				value = nil
			}
			p.send(func(txn *crdt.Transaction) {
				text.Format(txn, at, length, crdt.Attributes{mark: value})
			})
			kinds["format"]++
		case 5: // several blocks in one transaction
			texts := p.texts()
			if len(texts) < 4 {
				continue
			}
			chosen := map[int]bool{}
			for range 2 + rng.IntN(5) {
				chosen[rng.IntN(len(texts))] = true
			}
			p.send(func(txn *crdt.Transaction) {
				for index := range chosen {
					texts[index].Insert(txn, rng.IntN(texts[index].Len()+1), word(), nil)
				}
			})
			kinds["multi"]++
		case 6: // insert top-level blocks
			source := stormBlocks[rng.IntN(len(stormBlocks))]
			parsed, err := pmdoc.Parse(source)
			if err != nil {
				t.Fatalf("parse %q: %v", source, err)
			}
			pmdoc.EnsureBlockIDs(parsed)
			p.editTree(func(tree *pmdoc.Node) *pmdoc.Node {
				at := rng.IntN(len(tree.Children) + 1)
				tree.Children = slices.Insert(tree.Children, at, parsed.Children...)
				return tree
			})
			kinds["insert"]++
		case 7: // delete a top-level block
			p.editTree(func(tree *pmdoc.Node) *pmdoc.Node {
				if len(tree.Children) < 4 {
					return nil
				}
				at := rng.IntN(len(tree.Children))
				tree.Children = slices.Delete(tree.Children, at, at+1)
				return tree
			})
			kinds["dropBlock"]++
		case 8: // move a top-level block
			p.editTree(func(tree *pmdoc.Node) *pmdoc.Node {
				if len(tree.Children) < 4 {
					return nil
				}
				from := rng.IntN(len(tree.Children))
				block := tree.Children[from]
				rest := slices.Delete(slices.Clone(tree.Children), from, from+1)
				to := rng.IntN(len(rest) + 1)
				tree.Children = slices.Insert(rest, to, block)
				return tree
			})
			kinds["moveBlock"]++
		case 9: // replace a top-level block with another kind
			source := stormBlocks[rng.IntN(len(stormBlocks))]
			parsed, err := pmdoc.Parse(source)
			if err != nil {
				t.Fatalf("parse %q: %v", source, err)
			}
			pmdoc.EnsureBlockIDs(parsed)
			p.editTree(func(tree *pmdoc.Node) *pmdoc.Node {
				if len(tree.Children) < 4 || len(parsed.Children) == 0 {
					return nil
				}
				at := rng.IntN(len(tree.Children))
				tree.Children[at] = parsed.Children[0]
				return tree
			})
			kinds["replaceBlock"]++
		case 10: // change a heading's level, a top-level attribute change
			p.editTree(func(tree *pmdoc.Node) *pmdoc.Node {
				headings := []int{}
				for index, block := range tree.Children {
					if block.Type == "heading" {
						headings = append(headings, index)
					}
				}
				if len(headings) == 0 {
					return nil
				}
				block := tree.Children[headings[rng.IntN(len(headings))]]
				attrs := pmdoc.Attrs{}
				for key, value := range block.Attrs {
					attrs[key] = value
				}
				attrs["level"] = float64(1 + rng.IntN(5))
				block.Attrs = attrs
				return tree
			})
			kinds["heading"]++
		case 11: // a margin write and two concurrent edits on one state
			marks := p.doc.GetMap(marksMapName)
			p.send(func(txn *crdt.Transaction) {
				marks.Set(txn, fmt.Sprintf("c-storm-%d", edit), map[string]any{"kind": "comment", "by": "user:bob", "text": word()})
			})
			kinds["margin"]++
			bob.sync()
			alice.sync()
			bobText, aliceText := pickText(bob), pickText(alice)
			if bobText == nil || aliceText == nil {
				continue
			}
			bob.edit(func(txn *crdt.Transaction) { bobText.Insert(txn, rng.IntN(bobText.Len()+1), word(), nil) })
			alice.edit(func(txn *crdt.Transaction) { aliceText.Insert(txn, rng.IntN(aliceText.Len()+1), word(), nil) })
			bob.flush()
			alice.flush()
			kinds["concurrent"] += 2
		}
		if edit%73 == 72 {
			// A forced recover: the replica cannot place the next update, so it starts again
			// from a base the room's load never rendered. The next update is a keystroke, so
			// recover's own class for an update it could not place - a content change with no
			// ask blocks, which main answers too - is what the document did.
			replica := service.replicaOf(service.srv.GetDoc(artifactID))
			replica.mu.Lock()
			replica.doc = nil
			replica.mu.Unlock()
			bob.sync()
			if text := pickText(bob); text != nil {
				typeInto(bob, text)
				recovered++
			}
		}
	}

	waitForLandedAppends(t, service, artifactID)
	sh.check(t)
	paths := m.since()
	t.Logf("seed %d: edits %v, recovers %d; paths incremental %d, whole %d, untouched %d, reused %d, inRoom %d, marginOnly %d, catchUpFromLoad %d, catchUpRendered %d",
		seed, kinds, recovered, paths.observerIncremental, paths.observerRendered, paths.observerUntouched,
		paths.observerReused, paths.observerInRoom, paths.observerMarginOnly, paths.catchUpFromLoad, paths.catchUpRendered)
	if paths.observerIncremental == 0 {
		t.Errorf("seed %d spliced no update: the storm proved nothing about the incremental path", seed)
	}
}
