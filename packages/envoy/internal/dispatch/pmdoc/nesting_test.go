package pmdoc

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
)

const (
	expectedMaxNesting   = 100
	expectedMaxTreeDepth = 10_000
)

// A markdown document opens at most 100 blocks inside one another. The same parser handles a
// whole upload and the fragments written by edits and accepted suggestions.
func TestParseRefusesMarkdownNestedPastTheBound(t *testing.T) {
	parsers := []struct {
		name  string
		parse func(string) (*Node, error)
	}{
		{name: "document", parse: Parse},
		{name: "upload", parse: func(markdown string) (*Node, error) { return ParseForWrite(markdown, nil) }},
		{name: "fragment", parse: func(markdown string) (*Node, error) {
			return ParseFragment(markdown, true, NewTablePaddingBudget())
		}},
	}
	for _, test := range []struct {
		name   string
		open   string
		blocks int
	}{
		{name: "quotes", open: "> ", blocks: 1},
		{name: "bare quotes", open: ">", blocks: 1},
		{name: "lists", open: "- ", blocks: 2},
		{name: "callouts", blocks: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			deepest := strings.Repeat(test.open, expectedMaxNesting/test.blocks) + "a"
			over := strings.Repeat(test.open, expectedMaxNesting/test.blocks+1) + "a"
			if test.name == "callouts" {
				deepest = nestedCallouts(expectedMaxNesting)
				over = nestedCallouts(expectedMaxNesting + 1)
			}
			for _, reader := range parsers {
				t.Run(reader.name, func(t *testing.T) {
					if _, err := reader.parse(deepest); err != nil {
						t.Fatalf("%d nested blocks: %v, want them read", expectedMaxNesting, err)
					}
					_, err := reader.parse(over)
					if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), "line 1") || !strings.Contains(err.Error(), "100 blocks") {
						t.Fatalf("%d nested blocks: %v, want ErrSchema naming line 1 and the bound", expectedMaxNesting+1, err)
					}
				})
			}
		})
	}
}

// nestedCallouts is depth callouts inside one another, with the outermost fenced with the most colons.
func nestedCallouts(depth int) string {
	var markdown strings.Builder
	for level := depth; level >= 1; level-- {
		markdown.WriteString(strings.Repeat(":", level+2) + `callout{kind="note" title="T"}` + "\n")
	}
	markdown.WriteString("a\n")
	for level := 1; level <= depth; level++ {
		markdown.WriteString(strings.Repeat(":", level+2) + "\n")
	}
	return markdown.String()
}

// A megabyte of bare quote markers previously built a tree deep enough to overflow the process
// stack. Refusal must happen before the parser enters the document pipeline's recursive walks.
func TestParseRefusesAMebibyteOfQuotesInBoundedTime(t *testing.T) {
	started := time.Now()
	_, err := Parse(strings.Repeat(">", 1<<20))
	if !errors.Is(err, ErrSchema) {
		t.Fatalf("1 MiB of `>`: %v, want ErrSchema", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("refusing 1 MiB of `>` took %s, want under 10 s", elapsed)
	}
}

// A tree from any source is outside the schema when a node exceeds 10,000 ancestors. The CRDT
// chains below use the same Yjs element insertion a crafted browser client can send, not Update,
// because Update validates the server-authored tree before it writes it.
func TestTreesDeeperThanTenThousandAreOutsideTheSchema(t *testing.T) {
	chain := func(blockquotes int) *Node {
		node := &Node{Type: "paragraph", Children: []*Node{{Type: "text", Text: "a"}}}
		for range blockquotes {
			node = &Node{Type: "blockquote", Children: []*Node{node}}
		}
		return &Node{Type: "doc", Children: []*Node{node}}
	}

	deepest := chain(expectedMaxTreeDepth - 2)
	if err := deepest.Validate(); err != nil {
		t.Fatalf("a tree %d deep: %v, want it valid", expectedMaxTreeDepth, err)
	}
	if _, err := Render(deepest); err != nil {
		t.Fatalf("render a tree %d deep: %v", expectedMaxTreeDepth, err)
	}
	over := chain(expectedMaxTreeDepth - 1)
	if err := over.Validate(); !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), "10000") {
		t.Fatalf("a tree %d deep: %v, want ErrSchema naming the bound", expectedMaxTreeDepth+1, err)
	}

	for _, test := range []struct {
		name        string
		blockquotes int
		want        error
	}{
		{name: "at the bound", blockquotes: expectedMaxTreeDepth - 2},
		{name: "over the bound", blockquotes: expectedMaxTreeDepth - 1, want: ErrSchema},
	} {
		t.Run(test.name, func(t *testing.T) {
			ydoc := crdt.New()
			fragment := ydoc.GetXmlFragment("prosemirror")
			ydoc.Transact(func(txn *crdt.Transaction) {
				writeDeepCRDTChain(txn, fragment, test.blockquotes)
			})
			_, err := Read(fragment)
			if test.want == nil && err != nil {
				t.Fatalf("read back a chain %d deep: %v, want it valid", test.blockquotes+2, err)
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("read back a chain %d deep: %v, want %v", test.blockquotes+2, err, test.want)
			}
		})
	}
}

// A CRDT update can contain far more nodes than a markdown document. Read stops at the schema
// bound, rather than recursing through a million-element chain and overflowing the process stack.
func TestReadRefusesAMillionLevelCRDTTreeWithoutOverflow(t *testing.T) {
	ydoc := crdt.New()
	fragment := ydoc.GetXmlFragment("prosemirror")
	ydoc.Transact(func(txn *crdt.Transaction) {
		writeDeepCRDTChain(txn, fragment, 1_000_000)
	})

	_, err := Read(fragment)
	if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), "10001") {
		t.Fatalf("read a million-level CRDT tree: %v, want ErrSchema at level 10001", err)
	}
}

func writeDeepCRDTChain(txn *crdt.Transaction, fragment *crdt.YXmlFragment, blockquotes int) {
	parent := crdt.NewYXmlElement("blockquote")
	fragment.InsertElement(txn, 0, parent)
	for range blockquotes - 1 {
		child := crdt.NewYXmlElement("blockquote")
		parent.InsertElement(txn, 0, child)
		parent = child
	}
	paragraph := crdt.NewYXmlElement("paragraph")
	parent.InsertElement(txn, 0, paragraph)
	text := crdt.NewYXmlText()
	paragraph.InsertText(txn, 0, text)
	text.Insert(txn, 0, "a", nil)
}
