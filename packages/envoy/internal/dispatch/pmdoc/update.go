package pmdoc

import (
	"fmt"
	"reflect"
	"unicode/utf16"
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
		if leftMatches {
			leftMatches = !leftP.isText() && leftElement.NodeName == leftP.element.Type
		}
		rightElement, rightMatches := rightY.(*crdt.YXmlElement)
		if rightMatches {
			rightMatches = !rightP.isText() && rightElement.NodeName == rightP.element.Type
		}

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
			frag.Delete(txn, left, 1)
			if err := insertPChild(txn, frag, left, leftP); err != nil {
				return err
			}
			left++
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
	for index := left; index < len(pchildren)-right; index++ {
		if err := insertPChild(txn, frag, index, pchildren[index]); err != nil {
			return err
		}
	}
	return nil
}

func updateElement(txn *crdt.Transaction, element *crdt.YXmlElement, want *Node) error {
	if element.NodeName != want.Type {
		return fmt.Errorf("%w: Yjs element %q does not match %q", ErrSchema, element.NodeName, want.Type)
	}
	current := Attrs(element.GetAttributeValues())
	for key, value := range want.Attrs {
		if value == nil {
			if _, ok := current[key]; ok {
				element.DeleteAttribute(txn, key)
			}
			continue
		}
		if !attrsEqual(Attrs{key: value}, Attrs{key: current[key]}) {
			element.SetAttributeValue(txn, key, value)
		}
	}
	for key := range current {
		if value, ok := want.Attrs[key]; !ok || value == nil {
			element.DeleteAttribute(txn, key)
		}
	}
	return updateChildren(txn, &element.YXmlFragment, want.Children)
}

func insertPChild(txn *crdt.Transaction, frag *crdt.YXmlFragment, index int, child normalizedChild) error {
	if child.isText() {
		text, err := createTypeFromTextNodes(txn, child.text)
		if err != nil {
			return err
		}
		frag.InsertText(txn, index, text)
		return nil
	}
	element, err := createTypeFromElementNode(txn, child.element)
	if err != nil {
		return err
	}
	frag.InsertElement(txn, index, element)
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
	for key, value := range node.Attrs {
		if value != nil && key != "ychange" {
			element.SetAttributeValue(txn, key, value)
		}
	}
	for index, child := range normalizePNodeContent(node.Children) {
		if err := insertPChild(txn, &element.YXmlFragment, index, child); err != nil {
			return nil, err
		}
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
	if element.NodeName != node.Type || !attrsEqual(Attrs(element.GetAttributeValues()), node.Attrs) {
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
	staleAttributes := make(crdt.Attributes)
	for _, operation := range operations {
		value, ok := operation.Insert.(string)
		if !ok {
			return fmt.Errorf("%w: unsupported Yjs text embed %T", ErrSchema, operation.Insert)
		}
		current += value
		for key := range operation.Attributes {
			staleAttributes[key] = nil
		}
	}

	var desired string
	for _, node := range want {
		if node.Type != "text" {
			return fmt.Errorf("%w: Yjs text wants %q", ErrSchema, node.Type)
		}
		desired += node.Text
	}
	marksChanged := !equalYTextMarks(operations, want)
	index, remove, insert := simpleDiff(current, desired)
	if remove > 0 {
		text.Delete(txn, index, remove)
	}
	if insert != "" {
		text.Insert(txn, index, insert, nil)
	}
	if !marksChanged {
		return nil
	}

	delta := make([]crdt.Delta, 0, len(want))
	for _, node := range want {
		attributes := make(crdt.Attributes, len(staleAttributes)+len(node.Marks))
		for key := range staleAttributes {
			attributes[key] = nil
		}
		for key, value := range marksToAttributes(node.Marks) {
			attributes[key] = value
		}
		delta = append(delta, crdt.Delta{
			Op:         crdt.DeltaOpRetain,
			Retain:     len16(node.Text),
			Attributes: attributes,
		})
	}
	text.ApplyDelta(txn, delta)
	return nil
}

func equalYTextMarks(operations []crdt.Delta, nodes []*Node) bool {
	if len(operations) != len(nodes) {
		return false
	}
	for index, operation := range operations {
		if operation.Op != crdt.DeltaOpInsert || !equalYMarks(operation.Attributes, nodes[index].Marks) {
			return false
		}
	}
	return true
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
		attributes[mark.Type] = value
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

func simpleDiff(current, want string) (index, remove int, insert string) {
	left := utf16.Encode([]rune(current))
	right := utf16.Encode([]rune(want))
	for index < len(left) && index < len(right) && left[index] == right[index] {
		index++
	}
	currentEnd, wantEnd := len(left), len(right)
	for currentEnd > index && wantEnd > index && left[currentEnd-1] == right[wantEnd-1] {
		currentEnd--
		wantEnd--
	}
	return index, currentEnd - index, string(utf16.Decode(right[index:wantEnd]))
}
