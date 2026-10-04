package pmdoc

import (
	"fmt"
	"regexp"
)

// TaskProgress is a document's task-list items counted: Total every list item that carries a
// checkbox and Done every checked one.
type TaskProgress struct {
	Done  int
	Total int
}

// CountTasks counts the document's task items, nested lists and lists inside callouts and quotes
// included. A task item is a list item whose `checked` attribute is set (Parse sets it from
// `- [ ]` and `- [x]`, and leaves it nil for a plain item); a list item emptied of its text and
// written as a plain item carries none, and code is never a list, so neither counts.
func CountTasks(doc *Node) TaskProgress {
	var progress TaskProgress
	Walk(doc, func(node *Node) bool {
		if node.Type != "list_item" {
			return true
		}
		checked, task := node.Attrs["checked"].(bool)
		if !task {
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
	path, ok := taskItemPathAt(doc, position)
	return ok && path != nil
}

// SetTaskChecked returns a copy of doc with the checkbox of the task item whose first paragraph's
// text begins at position set to checked. A `replace` landing on that text whose `with` opens
// with a checkbox is ticking the item (docs: replacementMarkdown), the way one carrying a heading
// marker through `find` renames the heading.
func SetTaskChecked(doc *Node, position int, checked bool) (*Node, error) {
	path, ok := taskItemPathAt(doc, position)
	if !ok {
		return nil, fmt.Errorf("%w: task item at position %d", ErrTargetNotFound, position)
	}
	out := cloneNode(doc)
	item := nodeAtPath(out, path)
	attrs := make(Attrs, len(item.Attrs)+1)
	for name, value := range item.Attrs {
		attrs[name] = value
	}
	attrs["checked"] = checked
	item.Attrs = attrs
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// taskItemPathAt is the path of the task item whose first paragraph's text begins at position,
// and false when no task item's does.
func taskItemPathAt(doc *Node, position int) ([]int, bool) {
	var item []int
	found := false
	walk(doc, func(node *Node, path []int, pos, _ int) bool {
		if node.Type != "paragraph" || pos+1 != position {
			return true
		}
		if len(path) < 1 || path[len(path)-1] != 0 {
			return false
		}
		parent := nodeAtPath(doc, path[:len(path)-1])
		if parent.Type != "list_item" {
			return false
		}
		if _, task := parent.Attrs["checked"].(bool); !task {
			return false
		}
		item = append([]int(nil), path[:len(path)-1]...)
		found = true
		return false
	})
	return item, found
}
