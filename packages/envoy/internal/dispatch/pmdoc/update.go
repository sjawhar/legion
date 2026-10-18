package pmdoc

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"unsafe"

	"github.com/reearth/ygo/crdt"
)

// Update writes want into frag with the same element and rich-text layout that
// y-prosemirror's updateYFragment produces. Unchanged Yjs types are retained.
func Update(txn *crdt.Transaction, frag *crdt.YXmlFragment, want *Node) error {
	if txn == nil || frag == nil || want == nil {
		return fmt.Errorf("%w: Update requires transaction, fragment, and document", ErrSchema)
	}
	if want.Type != "doc" {
		return fmt.Errorf("%w: Update wants a doc, got %q", ErrSchema, want.Type)
	}
	if err := want.Validate(); err != nil {
		return err
	}
	return updateChildren(txn, frag, want.Children)
}

type normalizedChild struct {
	element *Node
	text    []*Node
}

func normalizePNodeContent(nodes []*Node) []normalizedChild {
	out := make([]normalizedChild, 0, len(nodes))
	for index := 0; index < len(nodes); {
		node := nodes[index]
		if node.Type != "text" {
			out = append(out, normalizedChild{element: node})
			index++
			continue
		}

		start := index
		for index < len(nodes) && nodes[index].Type == "text" {
			index++
		}
		out = append(out, normalizedChild{text: nodes[start:index]})
	}
	return out
}

func (child normalizedChild) isText() bool {
	return len(child.text) > 0
}

func updateChildren(txn *crdt.Transaction, frag *crdt.YXmlFragment, want []*Node) error {
	pchildren := normalizePNodeContent(want)
	ychildren := frag.Children()
	left, right := 0, 0
	limit := len(ychildren)
	if len(pchildren) < limit {
		limit = len(pchildren)
	}

	for left < limit && equalYChildPChild(ychildren[left], pchildren[left]) {
		left++
	}
	for right+left < limit && equalYChildPChild(ychildren[len(ychildren)-right-1], pchildren[len(pchildren)-right-1]) {
		right++
	}

	for len(ychildren)-left-right > 0 && len(pchildren)-left-right > 0 {
		leftY := ychildren[left]
		leftP := pchildren[left]
		rightY := ychildren[len(ychildren)-right-1]
		rightP := pchildren[len(pchildren)-right-1]

		if ytext, ok := leftY.(*crdt.YXmlText); ok && leftP.isText() {
			if err := updateYText(txn, ytext, leftP.text); err != nil {
				return err
			}
			left++
			continue
		}

		leftElement, leftMatches := leftY.(*crdt.YXmlElement)
		leftMatches = leftMatches && sameKind(leftY, leftP)
		rightElement, rightMatches := rightY.(*crdt.YXmlElement)
		rightMatches = rightMatches && sameKind(rightY, rightP)

		if leftMatches && rightMatches {
			leftFactor := computeChildEqualityFactor(leftElement, leftP.element)
			rightFactor := computeChildEqualityFactor(rightElement, rightP.element)
			if leftFactor < rightFactor {
				leftMatches = false
			} else {
				rightMatches = false
			}
		}

		switch {
		case leftMatches:
			if err := updateElement(txn, leftElement, leftP.element); err != nil {
				return err
			}
			left++
		case rightMatches:
			if err := updateElement(txn, rightElement, rightP.element); err != nil {
				return err
			}
			right++
		default:
			// Neither end matches, so the left child is replaced, and so is every child after it
			// that could not match either: the right end, which decided nothing here, is the same
			// for each. The run is deleted and its replacements inserted in one call each, which
			// writes what replacing them one at a time wrote - each replacement lands before the
			// run's first deleted child. ygo's Delete and Insert each walk the fragment from its
			// start to the index they are given (leftChildAt, deleteChildRange), so each run costs
			// a walk to its index: a document whose replaced children stand apart, every other
			// block changing type, costs a walk per replaced child, as it did before runs were
			// batched.
			run := 1
			for left+run < len(ychildren)-right && left+run < len(pchildren)-right && !sameKind(ychildren[left+run], pchildren[left+run]) {
				run++
			}
			frag.Delete(txn, left, run)
			if err := insertPChildren(txn, frag, left, pchildren[left:left+run]); err != nil {
				return err
			}
			left += run
		}
	}

	deleteCount := len(ychildren) - left - right
	if len(ychildren) == 1 && len(pchildren) == 0 {
		if text, ok := ychildren[0].(*crdt.YXmlText); ok {
			text.Delete(txn, 0, text.Len())
			return nil
		}
	}
	if deleteCount > 0 {
		frag.Delete(txn, left, deleteCount)
	}
	return insertPChildren(txn, frag, left, pchildren[left:len(pchildren)-right])
}

// sameKind reports whether updateChildren can keep ychild for pchild: a text for a text, or an
// element of the node's type.
func sameKind(ychild any, pchild normalizedChild) bool {
	switch y := ychild.(type) {
	case *crdt.YXmlText:
		return pchild.isText()
	case *crdt.YXmlElement:
		return !pchild.isText() && y.NodeName == pchild.element.Type
	default:
		return false
	}
}

func updateElement(txn *crdt.Transaction, element *crdt.YXmlElement, want *Node) error {
	if element.NodeName != want.Type {
		return fmt.Errorf("%w: Yjs element %q does not match %q", ErrSchema, element.NodeName, want.Type)
	}
	current := Attrs(element.GetAttributeValues())
	wantAttrs := liveAttrs(want)
	// Attribute changes are written in name order, as createTypeFromElementNode writes them.
	for _, key := range slices.Sorted(maps.Keys(wantAttrs)) {
		value := wantAttrs[key]
		if value == nil {
			if _, ok := current[key]; ok {
				element.DeleteAttribute(txn, key)
			}
			continue
		}
		if !attrsEqual(Attrs{key: value}, Attrs{key: current[key]}) {
			element.SetAttributeValue(txn, key, yjsAttributeValue(value))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(current)) {
		if value, ok := wantAttrs[key]; !ok || value == nil {
			element.DeleteAttribute(txn, key)
		}
	}
	return updateChildren(txn, &element.YXmlFragment, want.Children)
}

// liveNulls is, by node type, the value the live document holds for an attribute whose tree value
// is null, where the browser editor's schema gives an absent attribute a default other than null.
// That editor builds each node it loads with its schema, so an absent attribute takes the default,
// and it writes the default back into the live document for any node it edits. A table cell with
// no alignment holds "none": the default, left, would turn an unaligned column left-aligned at its
// first edit, while "none" it keeps and writes unaligned, as the tree's null is written. A code
// block with no language and an image with no title hold "", that editor's default and its value
// for none.
// readElement reads each back as null (treeAttrs).
var liveNulls = map[string]map[string]any{
	"table_cell":   {"alignment": "none"},
	"table_header": {"alignment": "none"},
	"code_block":   {"language": ""},
	"image":        {"title": ""},
}

// liveAttrs is node's attributes as the live document holds them (liveNulls).
func liveAttrs(node *Node) Attrs {
	var attrs Attrs
	for name, value := range liveNulls[node.Type] {
		if node.Attrs[name] != nil {
			continue
		}
		if attrs == nil {
			attrs = make(Attrs, len(node.Attrs)+1)
			for key, current := range node.Attrs {
				attrs[key] = current
			}
		}
		attrs[name] = value
	}
	if attrs == nil {
		return node.Attrs
	}
	return attrs
}

// emptyChildren is an empty slice of ygo's unexported node type, which insertPChildren's batch must
// be to pass to YXmlFragment.Insert. It comes from Children of a detached element, which returns
// its buffered children, none, without walking any document; Children of the fragment being
// written would walk every item it holds. The element is never inserted anywhere, so its type name
// is never read. It is built once, so a batch allocates only its own nodes.
var emptyChildren = crdt.NewYXmlElement("paragraph").Children()

// insertPChildren creates the Yjs type of each of children and inserts them into frag at index in
// one call. ygo's fragment insert walks the fragment's children to index (leftChildAt), and an
// element no document holds yet copies the children it buffers on every insert, so inserting a
// fragment's n children one at a time took n walks, time quadratic in n (LEGION-465). One call
// walks to index once, and writes what one insert per child at consecutive indexes wrote: each
// child's origin is the child before it, and its right origin the child that stood at index. It is
// how y-prosemirror writes a fragment's new children (updateYFragment's tail insert, and
// createTypeFromElementNode's). A call still walks to index, so its cost grows with index as well
// as with children.
func insertPChildren(txn *crdt.Transaction, frag *crdt.YXmlFragment, index int, children []normalizedChild) error {
	if len(children) == 0 {
		return nil
	}
	nodes := slices.Grow(slices.Clip(emptyChildren), len(children))
	for _, child := range children {
		if child.isText() {
			text, err := createTypeFromTextNodes(txn, child.text)
			if err != nil {
				return err
			}
			nodes = append(nodes, text)
			continue
		}
		element, err := createTypeFromElementNode(txn, child.element)
		if err != nil {
			return err
		}
		nodes = append(nodes, element)
	}
	frag.Insert(txn, index, nodes...)
	return nil
}

func createTypeFromTextNodes(txn *crdt.Transaction, nodes []*Node) (*crdt.YXmlText, error) {
	text := crdt.NewYXmlText()
	delta := make([]crdt.Delta, 0, len(nodes))
	for _, node := range nodes {
		if node.Type != "text" {
			return nil, fmt.Errorf("%w: Yjs text contains %q", ErrSchema, node.Type)
		}
		delta = append(delta, crdt.Delta{
			Op:         crdt.DeltaOpInsert,
			Insert:     node.Text,
			Attributes: marksToAttributes(node.Marks),
		})
	}
	text.ApplyDelta(txn, delta)
	return text, nil
}

func createTypeFromElementNode(txn *crdt.Transaction, node *Node) (*crdt.YXmlElement, error) {
	if node.Type == "text" {
		return nil, fmt.Errorf("%w: text is not an element", ErrSchema)
	}
	element := crdt.NewYXmlElement(node.Type)
	attrs := liveAttrs(node)
	// Attributes are written in name order, so the update an element makes does not depend on map
	// iteration order.
	for _, key := range slices.Sorted(maps.Keys(attrs)) {
		if value := attrs[key]; value != nil && key != "ychange" {
			element.SetAttributeValue(txn, key, yjsAttributeValue(value))
		}
	}
	if err := insertPChildren(txn, &element.YXmlFragment, 0, normalizePNodeContent(node.Children)); err != nil {
		return nil, err
	}
	return element, nil
}

func equalYChildPChild(ychild any, pchild normalizedChild) bool {
	switch y := ychild.(type) {
	case *crdt.YXmlText:
		return pchild.isText() && equalYTextPText(y, pchild.text)
	case *crdt.YXmlElement:
		return !pchild.isText() && equalYElementPNode(y, pchild.element)
	default:
		return false
	}
}

func equalYElementPNode(element *crdt.YXmlElement, node *Node) bool {
	if element.NodeName != node.Type || !attrsEqual(treeAttrs(element.NodeName, element.GetAttributeValues()), node.Attrs) {
		return false
	}
	children := element.Children()
	normalized := normalizePNodeContent(node.Children)
	if len(children) != len(normalized) {
		return false
	}
	for index, child := range children {
		if !equalYChildPChild(child, normalized[index]) {
			return false
		}
	}
	return true
}

func equalYTextPText(text *crdt.YXmlText, nodes []*Node) bool {
	delta, err := yTextDeltaInTransaction(text)
	if err != nil {
		return false
	}
	if len(delta) != len(nodes) {
		return false
	}
	for index, operation := range delta {
		if operation.Op != crdt.DeltaOpInsert {
			return false
		}
		value, ok := operation.Insert.(string)
		if !ok || value != nodes[index].Text || !equalYMarks(operation.Attributes, nodes[index].Marks) {
			return false
		}
	}
	return true
}

func equalYMarks(attributes crdt.Attributes, marks []Mark) bool {
	if len(attributes) != len(marks) {
		return false
	}
	for name, value := range attributes {
		markName := yattrToMarkName(name)
		attrs, err := attrsFromY(value)
		if err != nil {
			return false
		}
		found := false
		for _, mark := range marks {
			if mark.Type == markName && attrsEqual(attrs, mark.Attrs) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func computeChildEqualityFactor(element *crdt.YXmlElement, node *Node) int {
	children := element.Children()
	normalized := normalizePNodeContent(node.Children)
	limit := len(children)
	if len(normalized) < limit {
		limit = len(normalized)
	}
	left, right := 0, 0
	for left < limit && equalYChildPChild(children[left], normalized[left]) {
		left++
	}
	for left+right < limit && equalYChildPChild(children[len(children)-right-1], normalized[len(normalized)-right-1]) {
		right++
	}
	return left + right
}

func updateYText(txn *crdt.Transaction, text *crdt.YXmlText, want []*Node) error {
	operations, err := yTextDeltaInTransaction(text)
	if err != nil {
		return err
	}
	var current string
	for _, operation := range operations {
		value, ok := operation.Insert.(string)
		if !ok {
			return fmt.Errorf("%w: unsupported Yjs text embed %T", ErrSchema, operation.Insert)
		}
		current += value
	}

	var desired string
	for _, node := range want {
		if node.Type != "text" {
			return fmt.Errorf("%w: Yjs text wants %q", ErrSchema, node.Type)
		}
		desired += node.Text
	}
	index, remove, insert := simpleDiff(current, desired)
	if remove > 0 {
		text.Delete(txn, index, remove)
	}
	if insert != "" {
		text.Insert(txn, index, insert, nil)
	}

	actual, err := yTextDeltaInTransaction(text)
	if err != nil {
		return err
	}
	offset := 0
	for _, node := range want {
		length := len16(node.Text)
		attributes, matches, err := textRangeMarks(actual, offset, length, node.Marks)
		if err != nil {
			return err
		}
		if !matches {
			text.Format(txn, offset, length, formatAttributes(attributes, node.Marks))
		}
		offset += length
	}
	return nil
}

func textRangeMarks(operations []crdt.Delta, from, length int, marks []Mark) (crdt.Attributes, bool, error) {
	to := from + length
	offset := 0
	matches := true
	attributes := make(crdt.Attributes)
	for _, operation := range operations {
		value, ok := operation.Insert.(string)
		if !ok {
			return nil, false, fmt.Errorf("%w: unsupported Yjs text embed %T", ErrSchema, operation.Insert)
		}
		end := offset + len16(value)
		if from < end && offset < to {
			if !equalYMarks(operation.Attributes, marks) {
				matches = false
			}
			for key := range operation.Attributes {
				attributes[key] = nil
			}
		}
		offset = end
	}
	if offset != to && offset < to {
		return nil, false, fmt.Errorf("%w: Yjs text is shorter than target", ErrSchema)
	}
	return attributes, matches, nil
}

func formatAttributes(existing crdt.Attributes, marks []Mark) crdt.Attributes {
	attributes := make(crdt.Attributes, len(existing)+len(marks))
	for key := range existing {
		attributes[key] = nil
	}
	for key, value := range marksToAttributes(marks) {
		attributes[key] = value
	}
	return attributes
}

func marksToAttributes(marks []Mark) crdt.Attributes {
	if len(marks) == 0 {
		return nil
	}
	attributes := make(crdt.Attributes, len(marks))
	for _, mark := range marks {
		value := make(crdt.Attributes, len(mark.Attrs))
		for key, attr := range mark.Attrs {
			value[key] = attr
		}
		attributes[markAttributeKey(mark)] = value
	}
	return attributes
}

// yTextDeltaInTransaction is ToDelta's lock-free body. ygo deliberately
// exposes ToDelta only outside Transact, while Update's API owns a transaction.
// The upstream type keeps the item-list head private, so obtain that one field
// reflectively and otherwise use its exported Item and Content APIs.
func yTextDeltaInTransaction(text *crdt.YXmlText) ([]crdt.Delta, error) {
	current := make(crdt.Attributes)
	var delta []crdt.Delta
	for item := yTextStart(text); item != nil; item = item.Right {
		if item.Deleted {
			continue
		}
		switch content := item.Content.(type) {
		case *crdt.ContentString:
			if len(delta) > 0 && attrsEqual(Attrs(delta[len(delta)-1].Attributes), Attrs(current)) {
				delta[len(delta)-1].Insert = delta[len(delta)-1].Insert.(string) + content.Str
				continue
			}
			delta = append(delta, crdt.Delta{
				Op:         crdt.DeltaOpInsert,
				Insert:     content.Str,
				Attributes: cloneYAttributes(current),
			})
		case *crdt.ContentFormat:
			if content.Val == nil {
				delete(current, content.Key)
			} else {
				current[content.Key] = content.Val
			}
		case *crdt.ContentEmbed:
			return nil, fmt.Errorf("%w: unsupported Yjs text embed %T", ErrSchema, content.Val)
		default:
			return nil, fmt.Errorf("%w: unsupported Yjs text content %T", ErrSchema, item.Content)
		}
	}
	return delta, nil
}

func cloneYAttributes(attrs crdt.Attributes) crdt.Attributes {
	if len(attrs) == 0 {
		return nil
	}
	copy := make(crdt.Attributes, len(attrs))
	for key, value := range attrs {
		copy[key] = value
	}
	return copy
}

func yTextStart(text *crdt.YXmlText) *crdt.Item {
	yText := reflect.ValueOf(text).Elem().FieldByName("YText")
	abstractType := yText.FieldByName("abstractType")
	start := abstractType.FieldByName("start")
	return reflect.NewAt(start.Type(), unsafe.Pointer(start.UnsafeAddr())).Elem().Interface().(*crdt.Item)
}

// simpleDiff is the characters current and want do not share, between their common prefix and
// suffix: current's from index, remove long, are replaced by insert. index and remove are in UTF-16
// units, because Yjs counts them.
func simpleDiff(current, want string) (index, remove int, insert string) {
	left, right := []rune(current), []rune(want)
	start := 0
	for start < len(left) && start < len(right) && left[start] == right[start] {
		start++
	}
	currentEnd, wantEnd := len(left), len(right)
	for currentEnd > start && wantEnd > start && left[currentEnd-1] == right[wantEnd-1] {
		currentEnd--
		wantEnd--
	}
	return units16(left[:start]), units16(left[start:currentEnd]), string(right[start:wantEnd])
}

// yjsAttributeValue returns the Yjs "Any" encoding of a tree attribute value. The tree keeps
// a typed block's `string[]` attribute as a Go []string; Yjs encodes arrays only as []any, and
// ygo panics on any other slice type from inside the transaction (leaving the document mutex
// held, so the failure surfaced as a hang rather than an error). Every element attribute
// write goes through here so the two paths (create and update) agree with the reader, which
// already accepts either shape.
func yjsAttributeValue(value any) any {
	if items, ok := value.([]string); ok {
		out := make([]any, len(items))
		for index, item := range items {
			out[index] = item
		}
		return out
	}
	return value
}
