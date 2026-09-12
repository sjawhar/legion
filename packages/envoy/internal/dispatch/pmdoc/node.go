package pmdoc

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
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

var yattrMarkSuffix = regexp.MustCompile(`^(.*)--[a-zA-Z0-9+/=]{8}$`)

func sortMarks(marks []Mark) {
	sort.Slice(marks, func(i, j int) bool { return marks[i].Type < marks[j].Type })
}

func marksEqual(left, right []Mark) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Type != right[i].Type || !attrsEqual(left[i].Attrs, right[i].Attrs) {
			return false
		}
	}
	return true
}

func sortNodeMarks(n *Node) {
	sortMarks(n.Marks)
	for _, child := range n.Children {
		sortNodeMarks(child)
	}
}

func yattrToMarkName(name string) string {
	return yattrMarkSuffix.ReplaceAllString(name, "$1")
}

func (n *Node) Validate() error {
	return validateNode(n)
}

func validateNode(n *Node) error {
	if n == nil {
		return fmt.Errorf("%w: nil node", ErrSchema)
	}
	if !nodeTypes[n.Type] {
		if _, typed := typedBlock(n.Type); !typed {
			return fmt.Errorf("%w: node %q", ErrSchema, n.Type)
		}
	}
	if n.Type != "text" && len(n.Marks) != 0 {
		return fmt.Errorf("%w: node %q cannot have marks", ErrSchema, n.Type)
	}
	for _, m := range n.Marks {
		if !markTypes[m.Type] {
			return fmt.Errorf("%w: mark %q", ErrSchema, m.Type)
		}
	}
	for _, child := range n.Children {
		if err := validateNode(child); err != nil {
			return err
		}
	}

	switch n.Type {
	case "doc", "blockquote", "footnote_definition":
		if len(n.Children) == 0 || !childrenAreBlocks(n.Children) {
			return fmt.Errorf("%w: %s requires block children", ErrSchema, n.Type)
		}
	case "paragraph", "heading":
		if !childrenAreInline(n.Children) {
			return fmt.Errorf("%w: %s requires inline children", ErrSchema, n.Type)
		}
	case "bullet_list", "ordered_list":
		if len(n.Children) == 0 || !childrenAre(n.Children, "list_item") {
			return fmt.Errorf("%w: %s requires list items", ErrSchema, n.Type)
		}
	case "list_item":
		if len(n.Children) == 0 || n.Children[0].Type != "paragraph" || !childrenAreBlocks(n.Children[1:]) {
			return fmt.Errorf("%w: list item requires a paragraph followed by blocks", ErrSchema)
		}
	case "code_block":
		if !childrenAre(n.Children, "text") {
			return fmt.Errorf("%w: code block requires text children", ErrSchema)
		}
		for _, child := range n.Children {
			for _, mark := range child.Marks {
				if mark.Type != "proofAuthored" && mark.Type != "proofSuggestion" && mark.Type != "proofComment" && mark.Type != "proofFlagged" && mark.Type != "proofApproved" {
					return fmt.Errorf("%w: code block cannot contain mark %q", ErrSchema, mark.Type)
				}
			}
		}
	case "table":
		if len(n.Children) < 2 || n.Children[0].Type != "table_header_row" || !childrenAre(n.Children[1:], "table_row") {
			return fmt.Errorf("%w: table requires a header row followed by rows", ErrSchema)
		}
	case "table_header_row":
		if !childrenAre(n.Children, "table_header") {
			return fmt.Errorf("%w: table header row requires header cells", ErrSchema)
		}
	case "table_row":
		if !childrenAre(n.Children, "table_cell") {
			return fmt.Errorf("%w: table row requires cells", ErrSchema)
		}
	case "table_header", "table_cell":
		if !childrenAre(n.Children, "paragraph") || len(n.Children) != 1 {
			return fmt.Errorf("%w: %s requires one paragraph", ErrSchema, n.Type)
		}
	case "frontmatter":
		if !childrenAre(n.Children, "text") {
			return fmt.Errorf("%w: frontmatter requires text children", ErrSchema)
		}
	case "text", "hr", "hardbreak", "image", "html", "footnote_reference":
		if len(n.Children) != 0 {
			return fmt.Errorf("%w: %s cannot have children", ErrSchema, n.Type)
		}
	default:
		if typ, typed := typedBlock(n.Type); typed {
			return validateTypedBlock(n, typ)
		}
	}
	return nil
}

func validateTypedBlock(n *Node, typ BlockTypeSchema) error {
	switch typ.Content {
	case BlockContentParagraphs:
		if len(n.Children) == 0 || !childrenAre(n.Children, "paragraph") {
			return fmt.Errorf("%w: typed block %q content %q requires one or more paragraphs", ErrSchema, n.Type, typ.Content)
		}
	case BlockContentBlocks:
		if len(n.Children) == 0 || !childrenAreBlocks(n.Children) {
			return fmt.Errorf("%w: typed block %q content %q requires one or more blocks", ErrSchema, n.Type, typ.Content)
		}
	case BlockContentParagraphsOptionalBulletList:
		if !paragraphsThenOptionalBulletList(n.Children) {
			return fmt.Errorf("%w: typed block %q content %q requires paragraphs followed by an optional bullet list", ErrSchema, n.Type, typ.Content)
		}
	default:
		return fmt.Errorf("%w: typed block %q has unsupported content rule %q", ErrSchema, n.Type, typ.Content)
	}
	for name, value := range n.Attrs {
		if name == BlockIDAttr {
			if _, ok := value.(string); !ok {
				return fmt.Errorf("%w: typed block %q blockId must be a string", ErrSchema, n.Type)
			}
			continue
		}
		definition, ok := typ.Attributes[name]
		if !ok {
			return fmt.Errorf("%w: typed block %q does not declare attribute %q", ErrSchema, n.Type, name)
		}
		if err := validateAttributeValue(definition, value); err != nil {
			return fmt.Errorf("%w: typed block %q attribute %q: %v", ErrSchema, n.Type, name, err)
		}
	}
	for name := range typ.Attributes {
		if _, present := n.Attrs[name]; !present {
			return fmt.Errorf("%w: typed block %q is missing attribute %q", ErrSchema, n.Type, name)
		}
	}
	return nil
}

func paragraphsThenOptionalBulletList(children []*Node) bool {
	if len(children) == 0 || children[0] == nil || children[0].Type != "paragraph" {
		return false
	}
	for index, child := range children {
		if child == nil {
			return false
		}
		if child.Type == "paragraph" {
			continue
		}
		return child.Type == "bullet_list" && index == len(children)-1
	}
	return true
}

// ReassertServerOwnedAttrs restores schema defaults for server-owned attributes. Type-specific
// reconcilers can overwrite their authoritative values before this generic closure runs.
func ReassertServerOwnedAttrs(tree *Node) bool {
	changed := false
	walk(tree, func(node *Node, _ []int, _, _ int) bool {
		typ, typed := typedBlock(node.Type)
		if !typed {
			return true
		}
		for name, definition := range typ.Attributes {
			if !definition.Server || definition.Default == nil {
				continue
			}
			if node.Attrs == nil {
				node.Attrs = Attrs{}
			}
			if !attrsEqual(Attrs{name: node.Attrs[name]}, Attrs{name: definition.Default}) {
				node.Attrs[name] = cloneSchemaValue(definition.Default)
				changed = true
			}
		}
		return true
	})
	return changed
}

func childrenAre(children []*Node, typeName string) bool {
	for _, child := range children {
		if child == nil || child.Type != typeName {
			return false
		}
	}
	return true
}

func childrenAreBlocks(children []*Node) bool {
	for _, child := range children {
		if child == nil || !isBlockNodeType(child.Type) {
			return false
		}
	}
	return true
}

func childrenAreInline(children []*Node) bool {
	for _, child := range children {
		if child == nil || !isInlineNodeType(child.Type) {
			return false
		}
	}
	return true
}

func isBlockNodeType(typeName string) bool {
	switch typeName {
	case "paragraph", "heading", "blockquote", "bullet_list", "ordered_list", "code_block", "hr", "table", "footnote_definition", "frontmatter":
		return true
	default:
		_, typed := typedBlock(typeName)
		return typed
	}
}

func isInlineNodeType(typeName string) bool {
	switch typeName {
	case "text", "hardbreak", "image", "html", "footnote_reference":
		return true
	default:
		return false
	}
}

// Equal compares document content and structure while ignoring block identity.
func (n *Node) Equal(o *Node) bool {
	return equalNode(n, o, false)
}

// EqualWithBlockIDs compares document content, structure, and block identity.
func (n *Node) EqualWithBlockIDs(o *Node) bool {
	return equalNode(n, o, true)
}

func equalNode(n, o *Node, includeBlockIDs bool) bool {
	if n == nil || o == nil {
		return n == o
	}
	if n.Type != o.Type || n.Text != o.Text || !nodeAttrsEqual(n.Attrs, o.Attrs, includeBlockIDs) || len(n.Marks) != len(o.Marks) || len(n.Children) != len(o.Children) {
		return false
	}
	for i := range n.Marks {
		if n.Marks[i].Type != o.Marks[i].Type || !attrsEqual(n.Marks[i].Attrs, o.Marks[i].Attrs) {
			return false
		}
	}
	for i := range n.Children {
		if !equalNode(n.Children[i], o.Children[i], includeBlockIDs) {
			return false
		}
	}
	return true
}

func nodeAttrsEqual(a, b Attrs, includeBlockIDs bool) bool {
	if includeBlockIDs {
		return attrsEqual(a, b)
	}
	for key, av := range a {
		if key == BlockIDAttr {
			continue
		}
		bv, ok := b[key]
		if !ok {
			if av == nil {
				continue
			}
			return false
		}
		if !reflect.DeepEqual(normalizeJSON(av), normalizeJSON(bv)) {
			return false
		}
	}
	for key, bv := range b {
		if key != BlockIDAttr {
			if _, ok := a[key]; !ok && bv != nil {
				return false
			}
		}
	}
	return true
}

// attrsEqual treats nil and empty as equal and compares values by JSON semantics.
func attrsEqual(a, b Attrs) bool {
	for key, av := range a {
		bv, ok := b[key]
		if !ok {
			if av == nil {
				continue
			}
			return false
		}
		if !reflect.DeepEqual(normalizeJSON(av), normalizeJSON(bv)) {
			return false
		}
	}
	for key, bv := range b {
		if _, ok := a[key]; !ok && bv != nil {
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
	sortNodeMarks(n)
	if err := n.Validate(); err != nil {
		return nil, err
	}
	return n, nil
}
