package pmdoc

import (
	"fmt"
	"maps"
	"regexp"
	"slices"

	"github.com/yuin/goldmark/ast"
	extensionast "github.com/yuin/goldmark/extension/ast"
)

// TaskProgress is a document's task-list items counted: Total every list item that carries a
// checkbox and Done every checked one.
type TaskProgress struct {
	Done  int
	Total int
}

// CountTasksMarkdown is CountTasks of a stored rendering read as markdown alone: the task items
// goldmark's tree holds, counted before the Proof schema's refusals (refuseBlocks, Validate) and
// without converting the tree. A document the schema refuses - a table row holding text past
// its table's width, an html block, an unsupported directive - still has its checkboxes read
// exactly as Parse would read them, since the task-list extension sets each item's checkbox on
// the goldmark tree and conversion only copies it (parseListItem). So the reconciliation counts a
// spec whose table the browser editor cannot hold rather than reporting no progress for it, which
// three production specs did on the first deploy (LEGION-542). The one refusal kept is the
// reader's own: markdown nested past its bounds or over its element budget is not read at all,
// and is the error. A task item emptied of its text is skipped as CountTasks skips it, so the two
// agree on every document both can read.
func CountTasksMarkdown(markdown string) (TaskProgress, error) {
	source := []byte(LineFeeds(markdown))
	_, rest, _ := parseFrontmatterBlock(source)
	source = source[rest:]
	root, _, _, err := blockReader.read(source, false, readBackBudget(), 0)
	if err != nil {
		return TaskProgress{}, err
	}
	var progress TaskProgress
	err = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		item, ok := node.(*ast.ListItem)
		if !ok {
			return ast.WalkContinue, nil
		}
		first := item.FirstChild()
		if first == nil {
			return ast.WalkContinue, nil
		}
		checkbox, ok := first.FirstChild().(*extensionast.TaskCheckBox)
		if !ok {
			return ast.WalkContinue, nil
		}
		// The checkbox is the paragraph's first inline; an item whose paragraph holds nothing
		// else is the empty item CountTasks skips, which the renderer writes as a plain `- `.
		if checkbox.NextSibling() == nil && first.NextSibling() == nil {
			return ast.WalkContinue, nil
		}
		progress.Total++
		if checkbox.IsChecked {
			progress.Done++
		}
		return ast.WalkContinue, nil
	})
	return progress, err
}

// CountTasks counts the document's task items as its rendering carries them, nested lists and
// lists inside callouts and quotes included. A task item is a list item whose `checked` attribute
// is set (Parse sets it from `- [ ]` and `- [x]`, and leaves it nil for a plain item) and that
// holds text: the renderer writes an item emptied of its text as a plain `- ` (render.go), so a
// blank checkbox a browser has just added is not a task until it has words, and the count of a
// live tree equals the count of the markdown its version stores. Code is never a list, so nothing
// inside a code block counts.
func CountTasks(doc *Node) TaskProgress {
	var progress TaskProgress
	Walk(doc, func(node *Node) bool {
		if node.Type != "list_item" {
			return true
		}
		checked, task := node.Attrs["checked"].(bool)
		if !task || holdsOnlyAnEmptyParagraph(node) {
			return true
		}
		progress.Total++
		if checked {
			progress.Done++
		}
		return true
	})
	return progress
}

// leadingTaskCheckbox matches the checkbox a task item renders before its text: `[ ] ` or `[x] `
// (`[X] ` too, as the parser reads it), with nothing but the brackets before the first space.
var leadingTaskCheckbox = regexp.MustCompile(`^\[([ xX])\][ \t]+`)

// LeadingTaskCheckbox reports whether markdown opens with a task checkbox, its state, and how many
// bytes it spans, so a caller can tell the box apart from the text that follows it.
func LeadingTaskCheckbox(markdown string) (checked, found bool, width int) {
	match := leadingTaskCheckbox.FindStringSubmatch(markdown)
	if match == nil {
		return false, false, 0
	}
	return match[1] != " ", true, len(match[0])
}

// TaskItemAt reports whether the textblock whose own text begins at position is a task item's
// first paragraph: the one the renderer writes the checkbox before. A paragraph anywhere else in
// an item, a plain list item and a paragraph outside a list are not.
func TaskItemAt(doc *Node, position int) bool {
	return taskItemPathAt(doc, position) != nil
}

// SetTaskChecked returns a copy of doc with the checkbox of the task item whose first paragraph's
// text begins at position set to checked. A `replace` landing on that text whose `with` opens
// with a checkbox is ticking the item (docs: replacementMarkdown), the way one carrying a heading
// marker through `find` renames the heading.
func SetTaskChecked(doc *Node, position int, checked bool) (*Node, error) {
	path := taskItemPathAt(doc, position)
	if path == nil {
		return nil, fmt.Errorf("%w: task item at position %d", ErrTargetNotFound, position)
	}
	out := cloneNode(doc)
	item := nodeAtPath(out, path)
	item.Attrs = maps.Clone(item.Attrs)
	item.Attrs["checked"] = checked
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// taskItemPathAt is the path of the task item whose first paragraph's text begins at position,
// and nil when no task item's does.
func taskItemPathAt(doc *Node, position int) []int {
	var item []int
	walk(doc, func(node *Node, path []int, pos, _ int) bool {
		if node.Type != "paragraph" || pos+1 != position {
			return true
		}
		if path[len(path)-1] != 0 {
			return false
		}
		parent := nodeAtPath(doc, path[:len(path)-1])
		if _, task := parent.Attrs["checked"].(bool); parent.Type == "list_item" && task {
			item = slices.Clone(path[:len(path)-1])
		}
		return false
	})
	return item
}
