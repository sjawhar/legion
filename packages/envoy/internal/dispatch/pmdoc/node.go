package pmdoc

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

type Attrs map[string]any

type Mark struct {
	Type  string
	Attrs Attrs
}

type Node struct {
	Type     string
	Attrs    Attrs
	Text     string
	Marks    []Mark
	Children []*Node
}

var ErrSchema = errors.New("pmdoc: outside Proof schema")

var nodeTypes = map[string]bool{
	"doc": true, "paragraph": true, "heading": true, "blockquote": true, "bullet_list": true,
	"ordered_list": true, "list_item": true, "code_block": true, "hr": true, "hardbreak": true,
	"image": true, "html": true, "table": true, "table_header_row": true, "table_row": true,
	"table_cell": true, "table_header": true, "footnote_definition": true,
	"footnote_reference": true, "frontmatter": true, "text": true,
}

var markTypes = map[string]bool{
	"strong": true, "emphasis": true, "inlineCode": true, "link": true, "strike_through": true,
	"proofSuggestion": true, "proofComment": true, "proofFlagged": true, "proofApproved": true,
	"proofAuthored": true, "dispatchAsk": true,
}

func (n *Node) Validate() error {
	if !nodeTypes[n.Type] {
		return fmt.Errorf("%w: node %q", ErrSchema, n.Type)
	}
	for _, m := range n.Marks {
		if !markTypes[m.Type] {
			return fmt.Errorf("%w: mark %q", ErrSchema, m.Type)
		}
	}
	for _, c := range n.Children {
		if err := c.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (n *Node) Equal(o *Node) bool {
	if n == nil || o == nil {
		return n == o
	}
	if n.Type != o.Type || n.Text != o.Text || !attrsEqual(n.Attrs, o.Attrs) || len(n.Marks) != len(o.Marks) || len(n.Children) != len(o.Children) {
		return false
	}
	for i := range n.Marks {
		if n.Marks[i].Type != o.Marks[i].Type || !attrsEqual(n.Marks[i].Attrs, o.Marks[i].Attrs) {
			return false
		}
	}
	for i := range n.Children {
		if !n.Children[i].Equal(o.Children[i]) {
			return false
		}
	}
	return true
}

// attrsEqual treats nil and empty as equal and compares values by JSON semantics.
func attrsEqual(a, b Attrs) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !reflect.DeepEqual(normalizeJSON(av), normalizeJSON(bv)) {
			return false
		}
	}
	return true
}

// normalizeJSON makes ints and floats compare equal the way JSON does.
func normalizeJSON(v any) any {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case float32:
		return float64(x)
	}
	return v
}

type jsonNode struct {
	Type    string      `json:"type"`
	Attrs   Attrs       `json:"attrs,omitempty"`
	Content []*jsonNode `json:"content,omitempty"`
	Text    string      `json:"text,omitempty"`
	Marks   []jsonMark  `json:"marks,omitempty"`
}

type jsonMark struct {
	Type  string `json:"type"`
	Attrs Attrs  `json:"attrs,omitempty"`
}

func (n *Node) toJSON() *jsonNode {
	j := &jsonNode{Type: n.Type, Attrs: n.Attrs, Text: n.Text}
	for _, m := range n.Marks {
		j.Marks = append(j.Marks, jsonMark{Type: m.Type, Attrs: m.Attrs})
	}
	for _, c := range n.Children {
		j.Content = append(j.Content, c.toJSON())
	}
	return j
}

func (j *jsonNode) toNode() *Node {
	n := &Node{Type: j.Type, Attrs: j.Attrs, Text: j.Text}
	for _, m := range j.Marks {
		n.Marks = append(n.Marks, Mark{Type: m.Type, Attrs: m.Attrs})
	}
	for _, c := range j.Content {
		n.Children = append(n.Children, c.toNode())
	}
	return n
}

func (n *Node) JSON() ([]byte, error) {
	if err := n.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(n.toJSON())
}

func FromJSON(b []byte) (*Node, error) {
	var j jsonNode
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("pmdoc: decode json: %w", err)
	}
	n := j.toNode()
	if err := n.Validate(); err != nil {
		return nil, err
	}
	return n, nil
}
