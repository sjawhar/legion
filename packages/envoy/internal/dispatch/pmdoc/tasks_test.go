package pmdoc

import (
	"strings"
	"testing"
)

func TestCountTasks(t *testing.T) {
	for _, test := range []struct {
		name     string
		markdown string
		want     TaskProgress
	}{
		{"no list", "A paragraph.\n", TaskProgress{}},
		{"plain list is not a task list", "- one\n- two\n", TaskProgress{}},
		{"flat", "- [ ] one\n- [x] two\n- [X] three\n", TaskProgress{Done: 2, Total: 3}},
		{"nested", "- [ ] parent\n  - [x] child\n  - [ ] child\n    - [x] grandchild\n", TaskProgress{Done: 2, Total: 4}},
		{"mixed list", "- [ ] task\n- plain\n- [x] task\n", TaskProgress{Done: 1, Total: 2}},
		{"ordered", "1. [x] one\n2. [ ] two\n", TaskProgress{Done: 1, Total: 2}},
		{"inside a callout", ":::callout{#c1 kind=\"note\" title=\"T\"}\n- [x] inside\n- [ ] inside\n:::\n\n- [ ] outside\n", TaskProgress{Done: 1, Total: 3}},
		{"inside a quote", "> - [x] quoted\n", TaskProgress{Done: 1, Total: 1}},
		{"code is text", "```\n- [ ] not a task\n```\n\n- [x] real\n", TaskProgress{Done: 1, Total: 1}},
		{"link is not a checkbox", "- [x](https://example.com) link\n", TaskProgress{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc, err := Parse(test.markdown)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := CountTasks(doc); got != test.want {
				t.Fatalf("CountTasks = %+v, want %+v", got, test.want)
			}
		})
	}
}

// A task item emptied of its text, as a browser leaves one it has just added, renders as a plain
// `- ` (render.go), so the live tree's count agrees with its stored rendering's: zero.
func TestCountTasksSkipsAnEmptyTaskItem(t *testing.T) {
	doc := &Node{Type: "doc", Children: []*Node{{Type: "bullet_list", Attrs: Attrs{"spread": false}, Children: []*Node{
		{Type: "list_item", Attrs: Attrs{"label": "•", "listType": "bullet", "checked": true, "spread": false}, Children: []*Node{{Type: "paragraph"}}},
		{Type: "list_item", Attrs: Attrs{"label": "•", "listType": "bullet", "checked": false, "spread": false}, Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: "real"}}}}},
	}}}}
	if got := CountTasks(doc); got != (TaskProgress{Done: 0, Total: 1}) {
		t.Fatalf("CountTasks = %+v, want 0/1: the empty checked item renders as a plain item", got)
	}
	markdown, err := Render(doc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	back, err := ParseRendering(markdown)
	if err != nil {
		t.Fatalf("parse rendering %q: %v", markdown, err)
	}
	if got := CountTasks(back); got != (TaskProgress{Done: 0, Total: 1}) {
		t.Fatalf("CountTasks of the rendering %q = %+v, want 0/1", markdown, got)
	}
}

// A stored version whose table the Proof schema refuses still has its task items counted. The
// shape is LEGION-130's stored spec on the first deploy: a code span holding bare pipes in a row
// of a two-column table, which goldmark reads as six cells, so ParseRendering refuses the whole
// document (markWideRows) and the reconciliation reported no progress at all for it; LEGION-36
// and LEGION-179 failed the same way with three and five cells.
func TestCountTasksMarkdownCountsPastATableTheSchemaRefuses(t *testing.T) {
	const markdown = "## Requirements\n\n" +
		"| Requirement | Where it comes from |\n" +
		"| :--- | :--- |\n" +
		"| `describePhaseHandoffProblems` renders a missing `phase` as `phase: expected one of architect|plan|implement|test|review`. | Reviewer item 4. |\n" +
		"| `HANDOFF_SCHEMA_VERSION` stays 1. | LEGION-53 spec. |\n\n" +
		"## Tasks\n\n" +
		"- [x] Scope: map the server\n" +
		"- [ ] Implement: count items\n" +
		"  - [x] nested done\n" +
		"- [ ] Deliver\n"
	if _, err := ParseRendering(markdown); err == nil {
		t.Fatal("ParseRendering accepted the wide row; the test no longer stands for a refused document")
	} else if !strings.Contains(err.Error(), "a table row holding 6 cells where its table has 2") {
		t.Fatalf("ParseRendering refused for another reason: %v", err)
	}
	got, err := CountTasksMarkdown(markdown)
	if err != nil {
		t.Fatalf("CountTasksMarkdown: %v", err)
	}
	if got != (TaskProgress{Done: 2, Total: 4}) {
		t.Fatalf("CountTasksMarkdown = %+v, want 2/4", got)
	}
}

// On every document both can read, CountTasksMarkdown agrees with CountTasks of the parsed tree:
// the empty item, closed front matter, and an unclosed `---` opener, which Parse reads as front
// matter dropping the list under it and which a count reading the markdown alone must drop too
// (the first version of this counter did not, and creation would have stored 1/2 where the
// document holds 0/0). Markdown the reader cannot read at all is the error.
func TestCountTasksMarkdownAgreesWithCountTasks(t *testing.T) {
	for _, markdown := range []string{
		"A paragraph.\n",
		"- one\n- two\n",
		"- [ ] one\n- [x] two\n- [X] three\n",
		"- [ ] parent\n  - [x] child\n  - [ ] child\n    - [x] grandchild\n",
		"1. [x] one\n2. [ ] two\n",
		":::callout{#c1 kind=\"note\" title=\"T\"}\n- [x] inside\n- [ ] inside\n:::\n\n- [ ] outside\n",
		"> - [x] quoted\n",
		"```\n- [ ] not a task\n```\n\n- [x] real\n",
		"- [x](https://example.com) link\n",
		"---\ntitle: Spec\n---\n\n- [x] after front matter\n- [ ] two\n",
		"---\n- [x] task one\n- [ ] task two\n",
		"---\n- [x] task one\n\n- [ ] after a blank\n",
		"- [ ] \n- [x] real\n",
	} {
		doc, err := ParseRendering(markdown)
		if err != nil {
			t.Fatalf("ParseRendering(%q): %v", markdown, err)
		}
		fromMarkdown, err := CountTasksMarkdown(markdown)
		if err != nil {
			t.Fatalf("CountTasksMarkdown(%q): %v", markdown, err)
		}
		if fromTree := CountTasks(doc); fromMarkdown != fromTree {
			t.Fatalf("CountTasksMarkdown(%q) = %+v, CountTasks = %+v", markdown, fromMarkdown, fromTree)
		}
	}
	if _, err := CountTasksMarkdown(strings.Repeat("> ", 200) + "- [x] deep\n"); err == nil {
		t.Fatal("CountTasksMarkdown read markdown nested past the reader's bound")
	}
}

// A document the Proof schema refuses for a block beside the list - an html block, an unknown
// typed block - still has its task items counted, as the wide-row table is.
func TestCountTasksMarkdownCountsPastOtherSchemaRefusals(t *testing.T) {
	for _, test := range []struct{ name, markdown, refusal string }{
		{"html block", "<div>\nx\n</div>\n\n- [x] one\n- [ ] two\n", "block HTML"},
		{"unknown typed block", ":::unknown{#b1}\nBody.\n:::\n\n- [x] one\n- [ ] two\n- [ ] three\n", "unknown typed block"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseRendering(test.markdown); err == nil {
				t.Fatal("ParseRendering accepted the document; the test no longer stands for a refused one")
			} else if !strings.Contains(err.Error(), test.refusal) {
				t.Fatalf("ParseRendering refused for another reason: %v", err)
			}
			got, err := CountTasksMarkdown(test.markdown)
			if err != nil {
				t.Fatalf("CountTasksMarkdown: %v", err)
			}
			want := TaskProgress{Done: 1, Total: 2}
			if test.name == "unknown typed block" {
				want.Total = 3
			}
			if got != want {
				t.Fatalf("CountTasksMarkdown = %+v, want %+v", got, want)
			}
		})
	}
}
