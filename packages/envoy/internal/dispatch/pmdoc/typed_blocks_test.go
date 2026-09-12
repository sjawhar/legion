package pmdoc

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestTypedBlockDirectiveRoundTrips(t *testing.T) {
	SetBlockIDGenerator(func() string { return "generated-paragraph" })
	t.Cleanup(func() { SetBlockIDGenerator(nil) })

	const markdown = ":::callout{#callout-1 kind=\"warning\" title=\"Read this\"}\nBody text.\n:::\n"
	doc, err := Parse(markdown)
	if err != nil {
		t.Fatalf("parse typed block: %v", err)
	}
	want := &Node{Type: "doc", Children: []*Node{{
		Type:  "callout",
		Attrs: Attrs{BlockIDAttr: "callout-1", "kind": "warning", "title": "Read this"},
		Children: []*Node{{
			Type:     "paragraph",
			Attrs:    Attrs{BlockIDAttr: "generated-paragraph"},
			Children: []*Node{{Type: "text", Text: "Body text."}},
		}},
	}}}
	if !doc.EqualWithBlockIDs(want) {
		gotJSON, _ := doc.JSON()
		wantJSON, _ := want.JSON()
		t.Fatalf("parsed typed block = %s, want %s", gotJSON, wantJSON)
	}

	rendered, _, err := Render(doc)
	if err != nil {
		t.Fatalf("render typed block: %v", err)
	}
	if rendered != markdown {
		t.Fatalf("typed block round-trip = %q, want %q", rendered, markdown)
	}
}

func TestTypedBlockDirectiveErrorsNameTheProblem(t *testing.T) {
	cases := []struct {
		name     string
		markdown string
		want     string
	}{
		{
			name:     "unknown type",
			markdown: ":::unknown{#block-1}\nBody.\n:::\n",
			want:     `unknown typed block "unknown" (known types: callout)`,
		},
		{
			name:     "invalid enum",
			markdown: ":::callout{#block-1 kind=\"urgent\" title=\"Read this\"}\nBody.\n:::\n",
			want:     `attribute "kind": must be one of note, warning, got "urgent"`,
		},
		{
			name:     "unknown attribute",
			markdown: ":::callout{#block-1 kind=\"note\" title=\"Read this\" priority=\"high\"}\nBody.\n:::\n",
			want:     `does not declare attribute "priority"`,
		},
		{
			name:     "Pandoc fenced div",
			markdown: "::: {.callout}\nBody.\n:::\n",
			want:     "Pandoc fenced divs and malformed directives are not supported",
		},
		{
			name:     "leaf directive",
			markdown: "::callout{#block-1}\n",
			want:     "leaf directives (::name) are not supported",
		},
		{
			name:     "text directive",
			markdown: ":callout{#block-1}\n",
			want:     "text directives (:name{...}) are not supported",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.markdown)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse(%q) = %v, want error containing %q", tc.markdown, err, tc.want)
			}
		})
	}
}

func TestDocumentLevelUnclosedTypedBlockIsRejected(t *testing.T) {
	markdown, err := os.ReadFile("testdata/corpus-errors/unclosed-typed-block.md")
	if err != nil {
		t.Fatalf("read unclosed directive corpus: %v", err)
	}

	_, err = Parse(string(markdown))
	if err == nil || !strings.Contains(err.Error(), `typed block "callout" is unclosed at document level`) {
		t.Fatalf("Parse(unclosed typed block) = %v, want document-level unclosed error", err)
	}
}

func TestNestedUnclosedTypedBlockRunsToItsParentEnd(t *testing.T) {
	next := 0
	SetBlockIDGenerator(func() string {
		next++
		return fmt.Sprintf("generated-block-%d", next)
	})

	doc, err := Parse("> :::callout{#callout-1 kind=\"note\" title=\"Read this\"}\n> Body text.\n")
	if err != nil {
		t.Fatalf("parse nested unclosed typed block: %v", err)
	}
	if len(doc.Children) != 1 || doc.Children[0].Type != "blockquote" {
		t.Fatalf("outer document = %#v, want one blockquote", doc.Children)
	}
	children := doc.Children[0].Children
	if len(children) != 1 || children[0].Type != "callout" {
		t.Fatalf("blockquote children = %#v, want one callout", children)
	}
}
func TestTypedBlockBodyFollowsTheSchemaContentRule(t *testing.T) {
	const markdown = ":::callout{#callout-1 kind=\"note\" title=\"Body blocks\"}\nIntroduction.\n\n- Choice one\n- Choice two\n\n```text\nexample\n```\n\n> Quoted context.\n:::\n"

	doc, err := Parse(markdown)
	if err != nil {
		t.Fatalf("parse block-rich typed block: %v", err)
	}
	got := doc.Children[0].Children
	want := []string{"paragraph", "bullet_list", "code_block", "blockquote"}
	if len(got) != len(want) {
		t.Fatalf("typed block children = %d, want %d", len(got), len(want))
	}
	for index, typeName := range want {
		if got[index].Type != typeName {
			t.Fatalf("typed block child %d = %q, want %q", index, got[index].Type, typeName)
		}
	}
}

func TestFencedCodeMayQuoteUnsupportedDirectiveSyntax(t *testing.T) {
	markdown, err := os.ReadFile("testdata/corpus/code-directive-example.md")
	if err != nil {
		t.Fatalf("read code directive corpus: %v", err)
	}

	doc, err := Parse(string(markdown))
	if err != nil {
		t.Fatalf("parse fenced directive example: %v", err)
	}
	if doc.Children[0].Type != "code_block" {
		t.Fatalf("fenced directive example type = %q, want code_block", doc.Children[0].Type)
	}
}

func TestSchemaSupportsTypedBlockContentRules(t *testing.T) {
	for _, content := range []string{"paragraph+", "block+", "paragraph+ bullet_list?"} {
		t.Run(content, func(t *testing.T) {
			err := validateBlockSchema(BlockSchema{
				Version: 1,
				Types: []BlockTypeSchema{{
					Name:       "test",
					Content:    BlockContentRule(content),
					Render:     "host",
					Attributes: map[string]BlockAttributeSchema{},
				}},
			})
			if err != nil {
				t.Fatalf("validate content rule %q: %v", content, err)
			}
		})
	}
}

func TestTypedBlockContentRulesValidateTheirChildSequences(t *testing.T) {
	cases := []struct {
		name     string
		content  BlockContentRule
		children []*Node
		valid    bool
	}{
		{
			name:     "paragraphs",
			content:  BlockContentParagraphs,
			children: []*Node{{Type: "paragraph"}, {Type: "paragraph"}},
			valid:    true,
		},
		{
			name:     "paragraphs reject code",
			content:  BlockContentParagraphs,
			children: []*Node{{Type: "code_block"}},
		},
		{
			name:     "blocks accept list code and quote",
			content:  BlockContentBlocks,
			children: []*Node{{Type: "bullet_list"}, {Type: "code_block"}, {Type: "blockquote"}},
			valid:    true,
		},
		{
			name:     "ask paragraphs and optional list",
			content:  BlockContentParagraphsOptionalBulletList,
			children: []*Node{{Type: "paragraph"}, {Type: "paragraph"}, {Type: "bullet_list"}},
			valid:    true,
		},
		{
			name:     "ask rejects code",
			content:  BlockContentParagraphsOptionalBulletList,
			children: []*Node{{Type: "paragraph"}, {Type: "code_block"}},
		},
		{
			name:     "ask rejects a paragraph after its list",
			content:  BlockContentParagraphsOptionalBulletList,
			children: []*Node{{Type: "paragraph"}, {Type: "bullet_list"}, {Type: "paragraph"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTypedBlock(
				&Node{Type: "test", Children: tc.children},
				BlockTypeSchema{
					Name:       "test",
					Content:    tc.content,
					Attributes: map[string]BlockAttributeSchema{},
				},
			)
			if (err == nil) != tc.valid {
				t.Fatalf("validate content %q with children %#v = %v, valid = %t", tc.content, tc.children, err, tc.valid)
			}
		})
	}
}

func TestParseMintsAnOmittedTypedBlockID(t *testing.T) {
	next := 0
	SetBlockIDGenerator(func() string {
		next++
		if next == 1 {
			return "generated-id"
		}
		return "generated-paragraph"
	})
	t.Cleanup(func() { SetBlockIDGenerator(nil) })

	doc, err := Parse(":::callout{kind=\"note\" title=\"Read this\"}\nBody text.\n:::\n")
	if err != nil {
		t.Fatalf("parse typed block without id: %v", err)
	}
	callout := doc.Children[0]
	if got := callout.Attrs[BlockIDAttr]; got != "generated-id" {
		t.Fatalf("generated typed block id = %#v, want generated-id", got)
	}
}
