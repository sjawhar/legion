package pmdoc

// The walk measures every subtree once, up front, rather than each node again for each of its
// ancestors. These tests hold it to referenceWalk, which measures as it visits: the same
// nodes, in the same order, with the same paths and ranges, stopped at the same visit, on every
// tree this package ships as test data, on synthetic trees at the edges of the size rule, and on a
// generated document of a real spec's size. Failures name a node by its type, path, range and the
// length of its text, never the text itself, so a private spec read by the opt-in corpus test
// never reaches a log.

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// walker is the shape walk and referenceWalk share, so one recorder runs either.
type walker func(doc *Node, visit func(n *Node, path []int, pos, end int) bool)

// referenceWalk is walk without the up-front measure: every node's end is asked of
// referenceNodeSize as the node is visited. It is frozen here, not shared with walk, so that any
// change to walk's visits shows up as a difference from it. It pins the walk's algorithm and,
// through referenceLen16, len16; it shares isLeafNodeType with walk, since the schema's leaf list
// is not the walk's to pin.
func referenceWalk(doc *Node, visit func(n *Node, path []int, pos, end int) bool) {
	if doc == nil || visit == nil {
		return
	}
	referenceWalkNode(doc, nil, 0, visit)
}

func referenceWalkNode(node *Node, path []int, pos int, visit func(n *Node, path []int, pos, end int) bool) bool {
	end := pos + referenceNodeSize(node)
	if !visit(node, path, pos, end) {
		return false
	}
	if node.Type == "text" || isLeafNodeType(node.Type) {
		return true
	}

	childPos := pos
	if node.Type != "doc" {
		childPos++
	}
	for index, child := range node.Children {
		if !referenceWalkNode(child, append(path, index), childPos, visit) {
			return false
		}
		childPos += referenceNodeSize(child)
	}
	return true
}

// referenceNodeSize is the ProseMirror size rule, recomputed for every call: a text's UTF-16
// length, one for a leaf, and two around the children of any other node but the document.
func referenceNodeSize(node *Node) int {
	if node == nil {
		return 0
	}
	if node.Type == "text" {
		return referenceLen16(node.Text)
	}
	if isLeafNodeType(node.Type) {
		return 1
	}
	size := 0
	if node.Type != "doc" {
		size = 2
	}
	for _, child := range node.Children {
		size += referenceNodeSize(child)
	}
	return size
}

// walkVisit is one call of a walk's visitor. path is a copy, since a walk's path is valid only for
// the duration of the call, and nilPath records whether the walk passed a nil path, as both walks
// do for the root.
type walkVisit struct {
	node     *Node
	path     []int
	nilPath  bool
	pos, end int
}

// recordWalk runs run over doc and records every visit. Its visitor stops the walk at the stopAt'th
// visit, or never when stopAt is 0. A nil child makes both walks panic once it has been visited and
// passed over, as they read its type; the panic is recovered and reported, so the visits before it
// can still be compared.
func recordWalk(run walker, doc *Node, stopAt int) (visits []walkVisit, panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	run(doc, func(n *Node, path []int, pos, end int) bool {
		visits = append(visits, walkVisit{node: n, path: slices.Clone(path), nilPath: path == nil, pos: pos, end: end})
		return stopAt == 0 || len(visits) < stopAt
	})
	return visits, false
}

// describeVisit names a visit by everything but its text, of which it gives only the length, so a
// failure on a private document leaks none of it.
func describeVisit(visit walkVisit) string {
	nodeType, textLen := "<nil>", 0
	if visit.node != nil {
		nodeType, textLen = visit.node.Type, len(visit.node.Text)
	}
	return fmt.Sprintf("{type %s path %v nilPath %t pos %d end %d textLen %d}", nodeType, visit.path, visit.nilPath, visit.pos, visit.end, textLen)
}

func sameVisit(left, right walkVisit) bool {
	return left.node == right.node && slices.Equal(left.path, right.path) && left.nilPath == right.nilPath && left.pos == right.pos && left.end == right.end
}

// firstVisitMismatch describes the first difference between two recorded walks, or is empty when
// they agree visit for visit and both panicked or neither did.
func firstVisitMismatch(got, want []walkVisit, gotPanicked, wantPanicked bool) string {
	shared := min(len(got), len(want))
	for index := range shared {
		if !sameVisit(got[index], want[index]) {
			return fmt.Sprintf("visit %d: got %s, want %s", index, describeVisit(got[index]), describeVisit(want[index]))
		}
	}
	switch {
	case len(got) > shared:
		return fmt.Sprintf("visit %d: got %s, want no visit", shared, describeVisit(got[shared]))
	case len(want) > shared:
		return fmt.Sprintf("visit %d: got no visit, want %s", shared, describeVisit(want[shared]))
	case gotPanicked != wantPanicked:
		return fmt.Sprintf("after %d visits: panicked %t, want %t", shared, gotPanicked, wantPanicked)
	}
	return ""
}

// compareVisits reports the first difference between two recorded walks of the tree named name.
func compareVisits(t *testing.T, name string, got, want []walkVisit, gotPanicked, wantPanicked bool) bool {
	t.Helper()
	if mismatch := firstVisitMismatch(got, want, gotPanicked, wantPanicked); mismatch != "" {
		t.Errorf("%s: %s", name, mismatch)
		return false
	}
	return true
}

// stopPoints are the visits at which a walk of n visits is stopped: the first two, the last, and
// eight spread between, each once and each a visit the walk makes.
func stopPoints(n int) []int {
	candidates := []int{1, 2, n}
	for i := 1; i <= 8; i++ {
		candidates = append(candidates, n*i/9)
	}
	var points []int
	for _, point := range candidates {
		if point >= 1 && point <= n && !slices.Contains(points, point) {
			points = append(points, point)
		}
	}
	slices.Sort(points)
	return points
}

// treeDepth is how many levels below node its deepest descendant stands, counting as Children do,
// leaves' and texts' children included.
func treeDepth(node *Node) int {
	if node == nil {
		return 0
	}
	deepest := 0
	for _, child := range node.Children {
		if child != nil {
			deepest = max(deepest, treeDepth(child)+1)
		}
	}
	return deepest
}

type namedTree struct {
	name string
	tree *Node
}

// walkTestTrees are every tree TestWalkMatchesTheReferenceWalk holds the walk to: the
// shipped corpus and fixtures, the synthetic edges of the size rule, and the generated document.
func walkTestTrees(t *testing.T) []namedTree {
	t.Helper()
	var trees []namedTree

	paths, err := filepath.Glob(filepath.Join("testdata", "corpus", "*.md"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("corpus: %d files, %v", len(paths), err)
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := Parse(LineFeeds(string(raw)))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		trees = append(trees, namedTree{"corpus/" + filepath.Base(path), doc})
	}

	for _, fx := range loadFixtures(t) {
		doc, err := Parse(LineFeeds(fx.Markdown))
		if err != nil {
			t.Fatalf("fixture %s: %v", fx.Name, err)
		}
		trees = append(trees, namedTree{"fixture/" + fx.Name + "/markdown", doc})
		if len(fx.PMJSON) == 0 || string(fx.PMJSON) == "null" {
			continue
		}
		fromJSON, err := FromJSON(fx.PMJSON)
		if err != nil {
			t.Fatalf("fixture %s: %v", fx.Name, err)
		}
		trees = append(trees, namedTree{"fixture/" + fx.Name + "/json", fromJSON})
	}

	trees = append(trees, syntheticWalkTrees(t)...)

	markdown := generatedMarkdown()
	if len(markdown) < 400_000 {
		t.Fatalf("generated markdown is %d bytes, want at least 400,000", len(markdown))
	}
	generated, err := Parse(LineFeeds(markdown))
	if err != nil {
		t.Fatalf("generated document: %v", err)
	}
	depth := treeDepth(generated)
	if depth < 10 {
		t.Fatalf("generated document is %d levels deep, want at least 10", depth)
	}
	nodes := 0
	Walk(generated, func(*Node) bool {
		nodes++
		return true
	})
	t.Logf("generated document: %d bytes of markdown, %d nodes, %d levels deep", len(markdown), nodes, depth)
	return append(trees, namedTree{"generated", generated})
}

func synthText(value string, marks ...Mark) *Node {
	return &Node{Type: "text", Text: value, Marks: marks}
}

func synthParagraph(children ...*Node) *Node {
	return &Node{Type: "paragraph", Children: children}
}

func synthDoc(children ...*Node) *Node {
	return &Node{Type: "doc", Children: children}
}

// syntheticWalkTrees are trees built directly, at the edges of the size rule that markdown does
// not reach: texts whose UTF-8 and UTF-16 lengths differ or that are not UTF-8 at all, empty texts,
// leaves and texts carrying children the walk must neither descend into nor count, a nil child,
// roots other than a document, the deepest tree Validate admits and a wide one.
func syntheticWalkTrees(t *testing.T) []namedTree {
	t.Helper()
	strong := Mark{Type: "strong"}
	emphasis := Mark{Type: "emphasis"}

	wide := synthDoc()
	for index := range 5_000 {
		block := synthParagraph(synthText(fmt.Sprintf("block %d ", index)), synthText(pairA+"\u00e9", strong))
		if index%7 == 0 {
			block = &Node{Type: "heading", Attrs: Attrs{"level": 2}, Children: []*Node{synthText(pairD + " heading")}}
		}
		wide.Children = append(wide.Children, block)
	}

	deepest := blockquoteChains(MaxTreeDepth-2, MaxTreeDepth+1)
	if depth := treeDepth(deepest); depth != MaxTreeDepth {
		t.Fatalf("the deepest synthetic tree is %d levels deep, want %d", depth, MaxTreeDepth)
	}

	return []namedTree{
		{"synthetic/nil root", nil},
		{"synthetic/empty doc", synthDoc()},
		{"synthetic/astral and invalid text", synthDoc(
			synthParagraph(synthText("a"+pairA+"b"+pairC+"\U0010FFFF")),
			synthParagraph(synthText("\xff\xed\xa0\x80"+pairB+"\xf4\x90\x80\x80"+"\xc3")),
			&Node{Type: "heading", Attrs: Attrs{"level": 1}, Children: []*Node{synthText(pairD + "\xe2\x9a" + "\uFFFD")}},
		)},
		{"synthetic/empty texts", synthDoc(
			synthParagraph(synthText(""), synthText("a"), synthText("")),
			synthParagraph(synthText("")),
			synthParagraph(),
		)},
		{"synthetic/text split by marks", synthDoc(synthParagraph(
			synthText("plain "),
			synthText("bold"+pairC, strong),
			synthText(" both", strong, emphasis),
			synthText(pairA, emphasis),
			synthText(" end"),
		))},
		{"synthetic/every leaf", synthDoc(
			synthParagraph(synthText("a"), &Node{Type: "hardbreak"}, &Node{Type: "image", Attrs: Attrs{"src": "x.png"}}, &Node{Type: "html", Attrs: Attrs{"value": "<b>"}}, &Node{Type: "footnote_reference", Attrs: Attrs{"label": "1"}}, synthText(pairB)),
			&Node{Type: "hr"},
			&Node{Type: "html", Attrs: Attrs{"value": "<div></div>"}},
		)},
		{"synthetic/leaves and texts carrying children", synthDoc(
			synthParagraph(
				&Node{Type: "text", Text: "ab" + pairA, Children: []*Node{synthParagraph(synthText("not walked"))}},
				&Node{Type: "image", Children: []*Node{synthText("not walked either")}},
			),
			&Node{Type: "hr", Children: []*Node{synthParagraph(synthText("nor this")), {Type: "hr"}}},
			synthParagraph(synthText("after")),
		)},
		{"synthetic/nil block child", synthDoc(synthParagraph(synthText("before")), nil, synthParagraph(synthText("after")))},
		{"synthetic/nil inline child", synthDoc(synthParagraph(synthText("before"), nil, synthText("after")))},
		{"synthetic/paragraph root", synthParagraph(synthText("a"+pairA), &Node{Type: "image"}, synthText("b"))},
		{"synthetic/blockquote root", &Node{Type: "blockquote", Children: []*Node{
			synthParagraph(synthText("quoted")),
			{Type: "bullet_list", Children: []*Node{{Type: "list_item", Children: []*Node{synthParagraph(synthText(pairC + "item"))}}}},
		}}},
		{"synthetic/chain to MaxTreeDepth", deepest},
		{"synthetic/wide doc", wide},
	}
}

// walk visits what referenceWalk visits, with the same paths and ranges, and stops where it stops;
// Size agrees with referenceNodeSize.
func TestWalkMatchesTheReferenceWalk(t *testing.T) {
	for _, named := range walkTestTrees(t) {
		want, wantPanicked := recordWalk(referenceWalk, named.tree, 0)
		got, gotPanicked := recordWalk(walk, named.tree, 0)
		if !compareVisits(t, named.name, got, want, gotPanicked, wantPanicked) {
			continue
		}
		if got, want := Size(named.tree), referenceNodeSize(named.tree); got != want {
			t.Errorf("%s: Size = %d, want %d", named.name, got, want)
		}
		for _, stopAt := range stopPoints(len(want)) {
			// A walk stopped at visit k makes the full walk's first k visits, and stops before any
			// panic, since a panic follows a visit that let the walk go on.
			want, wantPanicked := want[:stopAt], false
			got, gotPanicked := recordWalk(walk, named.tree, stopAt)
			if !compareVisits(t, fmt.Sprintf("%s stopped at visit %d", named.name, stopAt), got, want, gotPanicked, wantPanicked) {
				break
			}
		}
	}
}

// walkWords are the generated document's words: ASCII, characters of two and three UTF-8 bytes,
// and characters outside the Basic Multilingual Plane, which take two UTF-16 units each.
var walkWords = []string{
	"proof", "dispatch", "ledger", "anchor", "comment", "review", "budget", "signal", "measure",
	"window", "settle", "branch", "render", "quote", "marker", "column", "worker", "planner",
	"caf\u00e9", "na\u00efve", "\u00fcber", "\u26a1charge", "\u2192next", "\u4e2d\u6587",
	"\U0001F4CAchart", "\U0001F60Asmile", "\U00010348gothic", "\U0001D11Eclef", "\U0001F680launch",
	"\U0001FA0Atile",
}

// walkSentence is a sentence of 8 to 19 words, some of them under a mark: strong, emphasis, code,
// strikethrough or a link.
func walkSentence(r *rand.Rand) string {
	words := make([]string, 8+r.IntN(12))
	for index := range words {
		word := walkWords[r.IntN(len(walkWords))]
		switch r.IntN(24) {
		case 0:
			word = "**" + word + "**"
		case 1:
			word = "_" + word + "_"
		case 2:
			word = "`" + word + "`"
		case 3:
			word = "~~" + word + "~~"
		case 4:
			word = fmt.Sprintf("[%s](https://example.com/w%d)", word, r.IntN(1000))
		}
		words[index] = word
	}
	return strings.Join(words, " ") + "."
}

func walkParagraph(b *strings.Builder, r *rand.Rand) {
	for sentence := range 3 + r.IntN(3) {
		if sentence > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(walkSentence(r))
	}
	b.WriteString("\n\n")
}

// walkNestedList writes a bullet list depth lists deep: each level's first item holds the next.
func walkNestedList(b *strings.Builder, r *rand.Rand, indent string, depth int) {
	for item := range 2 {
		b.WriteString(indent + "- " + walkSentence(r) + "\n")
		if item == 0 && depth > 1 {
			walkNestedList(b, r, indent+"  ", depth-1)
		}
	}
}

// walkQuotedOrderedList writes, inside a blockquote, an ordered list depth lists deep.
func walkQuotedOrderedList(b *strings.Builder, r *rand.Rand, indent string, depth int) {
	for item := 1; item <= 2; item++ {
		fmt.Fprintf(b, "> %s%d. %s\n", indent, item, walkSentence(r))
		if item == 1 && depth > 1 {
			walkQuotedOrderedList(b, r, indent+"   ", depth-1)
		}
	}
}

// generatedMarkdown is a spec of a real spec's size, the same on every run: sections of prose
// under marks and astral text, bullet lists six deep, blockquoted ordered lists five deep, tables,
// callouts holding lists and rules.
func generatedMarkdown() string {
	r := rand.New(rand.NewPCG(641, 2026))
	var b strings.Builder
	for section := 0; b.Len() < 420_000; section++ {
		fmt.Fprintf(&b, "## Section %d %s\n\n", section, walkSentence(r))
		for range 3 {
			walkParagraph(&b, r)
		}
		walkNestedList(&b, r, "", 6)
		b.WriteString("\n")
		walkParagraph(&b, r)
		walkQuotedOrderedList(&b, r, "", 5)
		b.WriteString("\n| Column | Owner | State | Note |\n| --- | --- | --- | --- |\n")
		for range 6 {
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", walkWords[r.IntN(len(walkWords))], walkWords[r.IntN(len(walkWords))], walkWords[r.IntN(len(walkWords))], walkSentence(r))
		}
		b.WriteString("\n")
		walkParagraph(&b, r)
		fmt.Fprintf(&b, ":::callout{#walk-callout-%d kind=\"note\" title=\"Section %d\"}\n%s\n\n", section, section, walkSentence(r))
		walkNestedList(&b, r, "", 2)
		b.WriteString(":::\n\n---\n\n")
	}
	return b.String()
}

// blockquoteChains is a document of exactly nodes nodes, itself included: chains of depth
// blockquotes, each ending in a paragraph holding a text, and a shorter chain, or an empty
// paragraph, making up what is left.
func blockquoteChains(depth, nodes int) *Node {
	doc := &Node{Type: "doc"}
	for count := 1; count < nodes; {
		remaining := nodes - count
		if remaining == 1 {
			doc.Children = append(doc.Children, &Node{Type: "paragraph"})
			count++
			continue
		}
		quotes := min(depth, remaining-2)
		chain := &Node{Type: "paragraph", Children: []*Node{{Type: "text", Text: "chained text \U0001F4CA"}}}
		for range quotes {
			chain = &Node{Type: "blockquote", Children: []*Node{chain}}
		}
		doc.Children = append(doc.Children, chain)
		count += quotes + 2
	}
	return doc
}

// positionedWalkSink keeps the benchmark's count alive so the walk cannot be dropped.
var positionedWalkSink int

// The positioned walk on a real-size spec, and on two documents of the same node count that differ
// only in depth: before the walk measured each subtree once, its cost grew with the depth as well
// as the node count.
func BenchmarkPositionedWalk(b *testing.B) {
	generated, err := Parse(LineFeeds(generatedMarkdown()))
	if err != nil {
		b.Fatal(err)
	}
	for _, bench := range []struct {
		name string
		doc  *Node
	}{
		{"generated", generated},
		{"depth=5", blockquoteChains(5, 60_000)},
		{"depth=20", blockquoteChains(20, 60_000)},
	} {
		b.Run(bench.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				count := 0
				walk(bench.doc, func(*Node, []int, int, int) bool {
					count++
					return true
				})
				positionedWalkSink = count
			}
		})
	}
}

// walkCensus counts what one tree holds, for the corpus test's summary.
type walkCensus struct {
	nodes, texts, astralTexts, emptyTexts, maxDepth int
}

func (c *walkCensus) add(other walkCensus) {
	c.nodes += other.nodes
	c.texts += other.texts
	c.astralTexts += other.astralTexts
	c.emptyTexts += other.emptyTexts
	c.maxDepth = max(c.maxDepth, other.maxDepth)
}

// checkWalkedTree holds tree's walk, each of its texts' UTF-16 lengths and its size to the
// reference, and returns how many of those disagree, the first disagreement described without
// text, and what the tree holds.
func checkWalkedTree(tree *Node) (mismatches int, first string, census walkCensus) {
	note := func(mismatch string) {
		mismatches++
		if first == "" {
			first = mismatch
		}
	}
	want, wantPanicked := recordWalk(referenceWalk, tree, 0)
	got, gotPanicked := recordWalk(walk, tree, 0)
	if mismatch := firstVisitMismatch(got, want, gotPanicked, wantPanicked); mismatch != "" {
		note("walk: " + mismatch)
	}
	census.nodes = len(want)
	for _, visit := range want {
		census.maxDepth = max(census.maxDepth, len(visit.path))
		if visit.node == nil || visit.node.Type != "text" {
			continue
		}
		census.texts++
		value := visit.node.Text
		if value == "" {
			census.emptyTexts++
		}
		if strings.ContainsFunc(value, func(r rune) bool { return r > 0xFFFF }) {
			census.astralTexts++
		}
		if got, want := len16(value), referenceLen16(value); got != want {
			note(fmt.Sprintf("len16 = %d, want %d at %s", got, want, describeVisit(visit)))
		}
	}
	if got, want := Size(tree), referenceNodeSize(tree); got != want {
		note(fmt.Sprintf("Size = %d, want %d", got, want))
	}
	return mismatches, first, census
}

// markEvery is how many eligible text nodes, in walk order, pass between two comment marks.
const markEvery = 50

// markedReadBack holds parsed as the server holds a spec, in a Y XML fragment, puts a proofComment
// mark over the middle third of every markEvery'th text node of a textblock that takes one, so the
// mark splits the text into runs as an anchored comment's quote does, and reads the fragment back.
// It returns the tree read back and how many marks it put.
func markedReadBack(parsed *Node) (*Node, int, error) {
	ydoc := crdt.New()
	frag := ydoc.GetXmlFragment("prosemirror")
	var err error
	ydoc.Transact(func(txn *crdt.Transaction) {
		err = Update(txn, frag, parsed)
	})
	if err != nil {
		return nil, 0, err
	}

	// Texts stand only in textblocks, which hold nothing but texts and leaves, so the last node
	// visited that is neither is the textblock of every text visited after it.
	var ranges []Range
	eligible, markable := 0, false
	referenceWalk(parsed, func(n *Node, _ []int, pos, end int) bool {
		switch {
		case n == nil:
		case n.Type == "text":
			if markable && end > pos {
				if eligible%markEvery == 0 {
					if from, to := middleThird(n.Text); to > from {
						ranges = append(ranges, Range{From: pos + from, To: pos + to})
					}
				}
				eligible++
			}
		case !isLeafNodeType(n.Type):
			markable = n.Type == "paragraph" || n.Type == "heading" || n.Type == "code_block"
		}
		return true
	})
	for index, r := range ranges {
		mark := Mark{Type: "proofComment", Attrs: Attrs{"id": fmt.Sprintf("walk-%d", index), "by": "user:walk"}}
		ydoc.Transact(func(txn *crdt.Transaction) {
			err = MarkRange(txn, frag, r, mark)
		})
		if err != nil {
			return nil, index, err
		}
	}
	readBack, err := Read(frag)
	if err != nil {
		return nil, len(ranges), err
	}
	return readBack, len(ranges), nil
}

// middleThird is the UTF-16 range of the middle third of value's characters, cut between
// characters so no mark splits a surrogate pair.
func middleThird(value string) (from, to int) {
	runes := []rune(value)
	first, last := len(runes)/3, len(runes)-len(runes)/3
	return units16(runes[:first]), units16(runes[:last])
}

// walk agrees with referenceWalk on a corpus of real specs, as parsed and as read back from a live
// document under comment marks. It runs only when PMDOC_WALK_CORPUS names a directory of markdown
// files, and logs counts and file names alone, never a spec's text.
func TestWalkMatchesTheReferenceWalkOnACorpus(t *testing.T) {
	dir := os.Getenv("PMDOC_WALK_CORPUS")
	if dir == "" {
		t.Skip("PMDOC_WALK_CORPUS unset; set it to a directory of .md files to run this")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("PMDOC_WALK_CORPUS: %v", err)
	}

	var (
		files, marks, mismatches, visitCountChanges int
		refusals, roundTripFailures                 []string
		parsedCensus, readCensus                    walkCensus
	)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		files++
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		parsed, err := Parse(LineFeeds(string(raw)))
		if err != nil {
			refusals = append(refusals, name)
			continue
		}

		count, first, census := checkWalkedTree(parsed)
		parsedCensus.add(census)
		readBack, marked, err := markedReadBack(parsed)
		marks += marked
		if err != nil {
			roundTripFailures = append(roundTripFailures, name)
		} else {
			readCount, readFirst, read := checkWalkedTree(readBack)
			readCensus.add(read)
			if read.nodes != census.nodes {
				visitCountChanges++
			}
			if first == "" && readFirst != "" {
				first = "read back: " + readFirst
			}
			count += readCount
		}
		if count > 0 {
			mismatches += count
			t.Errorf("%s: %d mismatches; first: %s", name, count, first)
		}
	}

	t.Logf("walk corpus: %d files; %d parse refusals %v; %d Update/mark/Read failures %v; "+
		"nodes %d parsed + %d read back; text nodes %d + %d; text nodes above U+FFFF %d + %d; "+
		"empty text nodes %d + %d; max depth %d; marks applied %d; mismatches %d; "+
		"read-back trees whose visit count differs from the parsed tree's %d",
		files, len(refusals), refusals, len(roundTripFailures), roundTripFailures,
		parsedCensus.nodes, readCensus.nodes, parsedCensus.texts, readCensus.texts,
		parsedCensus.astralTexts, readCensus.astralTexts, parsedCensus.emptyTexts, readCensus.emptyTexts,
		max(parsedCensus.maxDepth, readCensus.maxDepth), marks, mismatches, visitCountChanges)
}
