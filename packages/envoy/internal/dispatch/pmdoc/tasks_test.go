package pmdoc

import "testing"

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
