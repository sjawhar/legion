package pmdoc

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"unsafe"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"

	"github.com/sjawhar/envoy/internal/dispatch/docs/crafted"
)

// ReadUnshared reads a text by walking its items rather than through ygo's ToDelta, which Read
// keeps using; these tests hold the walk to Read's answer, so a ygo upgrade that changes what
// ToDelta makes of a text fails them. readersDiffer is their one oracle.

// readersDiffer is how ReadUnshared's reading of frag differs from Read's, or "" when it does not:
// both refuse it with the same error, as ErrSchema or not alike, or both read trees whose dumps
// (readerDump) are equal. A run carrying two marks whose values are not objects is refused with
// the error of whichever of them the read's map iteration meets first, so there ReadUnshared's
// error only has to be one Read also gives.
func readersDiffer(frag *crdt.YXmlFragment) string {
	want, wantErr := Read(frag)
	valid, gotErr := ReadUnshared(frag)
	if wantErr != nil || gotErr != nil {
		switch {
		case wantErr == nil || gotErr == nil:
			return fmt.Sprintf("Read: %v\nReadUnshared: %v", wantErr, gotErr)
		case errors.Is(wantErr, ErrSchema) != errors.Is(gotErr, ErrSchema):
			return fmt.Sprintf("ErrSchema: Read %v (%t), ReadUnshared %v (%t)", wantErr, errors.Is(wantErr, ErrSchema), gotErr, errors.Is(gotErr, ErrSchema))
		case wantErr.Error() != gotErr.Error() && !(isMarkValueError(wantErr) && isMarkValueError(gotErr) && readRefuses(frag, gotErr.Error())):
			return fmt.Sprintf("Read: %v\nReadUnshared: %v", wantErr, gotErr)
		}
		return ""
	}
	if valid.Node() == nil {
		return "ReadUnshared returned no tree and no error"
	}
	if wantDump, gotDump := readerDump(want), readerDump(valid.Node()); wantDump != gotDump {
		return fmt.Sprintf("Read:\n%s\nReadUnshared:\n%s", wantDump, gotDump)
	}
	return ""
}

// isMarkValueError reports whether err is a read's refusal of a mark value that is not an object.
func isMarkValueError(err error) bool {
	return strings.Contains(err.Error(), " attributes: want object, got ")
}

// readRefuses reports whether Read refuses frag with message in some of many reads.
func readRefuses(frag *crdt.YXmlFragment, message string) bool {
	for range 200 {
		if _, err := Read(frag); err != nil && err.Error() == message {
			return true
		}
	}
	return false
}

// readerDump is tree as text that two equal readings print alike: each node's type, text and
// attributes, and its marks sorted by their own dumps, since sortMarks leaves marks that tie in
// whatever order they came. %#v prints a map's keys sorted, a value's type, and NaN as NaN, which
// reflect.DeepEqual never finds equal to itself.
func readerDump(tree *Node) string {
	var out strings.Builder
	var dump func(n *Node, depth int)
	dump = func(n *Node, depth int) {
		marks := make([]string, len(n.Marks))
		for index, mark := range n.Marks {
			marks[index] = fmt.Sprintf("%s %#v", mark.Type, mark.Attrs)
		}
		sort.Strings(marks)
		fmt.Fprintf(&out, "%*s%s %q attrs=%#v marks(nil=%t)=%q children(nil=%t)\n",
			2*depth, "", n.Type, n.Text, n.Attrs, n.Marks == nil, marks, n.Children == nil)
		for _, child := range n.Children {
			dump(child, depth+1)
		}
	}
	dump(tree, 0)
	return out.String()
}

// A text script is what the table and the fuzzer do to a paragraph's text: steps of
// textStepSize bytes, a kind and three operands, each operand taken modulo what it indexes.
const textStepSize = 4

const (
	stepInsert           = iota // at, text
	stepInsertFormatted         // at, text, attributes
	stepFormat                  // at, length, attributes
	stepUnformat                // at, length, key: Format with a nil value
	stepDelete                  // at, length
	stepEmbed                   // at, embed, attributes
	stepElementAttribute        // name, value: on the paragraph
	stepCrafted                 // item: appended after the text's last item by a crafted update
	stepKinds
)

var scriptTexts = []string{"a", "bc", "😀", "de f"}

type namedAttributes struct {
	name  string
	attrs crdt.Attributes
}

var scriptAttributes = []namedAttributes{
	{"none", nil},
	{"strong", crdt.Attributes{"strong": map[string]any{}}},
	{"emphasis", crdt.Attributes{"emphasis": map[string]any{}}},
	{"link a", crdt.Attributes{"link": map[string]any{"href": "https://a.example"}}},
	{"link b", crdt.Attributes{"link": map[string]any{"href": "https://b.example"}}},
	{"strong and link a", crdt.Attributes{"strong": map[string]any{}, "link": map[string]any{"href": "https://a.example"}}},
	{"comment", crdt.Attributes{"proofComment--AAAAAAAA": map[string]any{"id": "c1", "by": "user:a"}}},
	{"strong NaN", crdt.Attributes{"strong": map[string]any{"x": math.NaN()}}},
	{"strong number", crdt.Attributes{"strong": map[string]any{"n": 1.5}}},
	{"strong not an object", crdt.Attributes{"strong": true}},
	{"two not objects", crdt.Attributes{"strong": true, "emphasis": "x"}},
	{"unknown mark", crdt.Attributes{"unknown": map[string]any{}}},
}

var scriptKeys = []string{"strong", "emphasis", "link", "proofComment--AAAAAAAA", "unknown"}

var scriptEmbeds = []any{"x", "", "yz", map[string]any{"image": "y"}}

var scriptElementAttributeNames = []string{"foo", "bar"}

// deleteAttribute among scriptElementAttributeValues deletes the attribute.
var deleteAttribute = new(int)

var scriptElementAttributeValues = []any{"v", 1.5, map[string]any{"a": "b"}, []any{"x"}, nil, deleteAttribute}

// craftedItem is content a crafted client appends to a text by an update ygo decodes: content ygo's
// own writers never put in a text, or put only through their checks.
type craftedItem struct {
	name  string
	write func(*encoding.Encoder) byte // writes the content, returns its V1 tag
}

func craftedString(s string) func(*encoding.Encoder) byte {
	return func(e *encoding.Encoder) byte { e.WriteVarString(s); return 4 }
}

func craftedJSON(tag byte, json ...string) func(*encoding.Encoder) byte {
	return func(e *encoding.Encoder) byte {
		for _, part := range json {
			e.WriteVarString(part)
		}
		return tag
	}
}

func craftedAny(values ...any) func(*encoding.Encoder) byte {
	return func(e *encoding.Encoder) byte {
		e.WriteVarUint(uint64(len(values)))
		for _, value := range values {
			e.WriteAny(value)
		}
		return 8
	}
}

var craftedItems = []craftedItem{
	{"embed string", craftedJSON(5, `"s"`)},
	{"embed empty string", craftedJSON(5, `""`)},
	{"embed map", craftedJSON(5, `{"a":1}`)},
	{"embed null", craftedJSON(5, `null`)},
	// ygo decodes an empty string item and drops it: it never reaches a text's items (the hand-made
	// item lists below hold one).
	{"empty string", craftedString("")},
	{"string", craftedString("w")},
	{"format strong", craftedJSON(6, "strong", `{}`)},
	{"format strong null", craftedJSON(6, "strong", `null`)},
	{"format strong not an object", craftedJSON(6, "strong", `7`)},
	{"any", craftedAny(1.0, "v")},
	{"any NaN", craftedAny(math.NaN())},
	{"json", func(e *encoding.Encoder) byte { e.WriteVarUint(1); e.WriteVarString(`"j"`); return 2 }},
	{"binary", func(e *encoding.Encoder) byte { e.WriteVarBytes([]byte{1, 2}); return 3 }},
	{"map type", func(e *encoding.Encoder) byte { e.WriteUint8(1); return 7 }},
	{"deleted", func(e *encoding.Encoder) byte { e.WriteVarUint(2); return 1 }},
}

// craftedClient is the client the crafted updates come from.
const craftedClient crdt.ClientID = 9

// appendCrafted applies the update a crafted client sends to append item after text's last item,
// returning ygo's error applying it. A text with no items yet takes none.
func appendCrafted(doc *crdt.Doc, text *crdt.YXmlText, item craftedItem) error {
	var origin *crdt.Item
	for at := yTextStart(text); at != nil; at = at.Right {
		if at.Content.Len() > 0 {
			origin = at
		}
	}
	if origin == nil {
		return nil
	}
	update := encoding.EncodeBytes(func(e *encoding.Encoder) {
		e.WriteVarUint(1) // clients
		e.WriteVarUint(1) // structs
		e.WriteVarUint(uint64(craftedClient))
		e.WriteVarUint(doc.StateVector()[craftedClient])
		content := encoding.NewEncoder()
		tag := item.write(content)
		e.WriteUint8(tag | 0x80) // the item has an origin, its left neighbour
		e.WriteVarUint(uint64(origin.ID.Client))
		e.WriteVarUint(origin.ID.Clock + uint64(origin.Content.Len()) - 1)
		e.WriteRaw(content.Bytes())
		e.WriteVarUint(0) // delete set
	})
	return crdt.ApplyUpdateV1(doc, update, nil)
}

// paletteIndex is the operand naming value among palette.
func paletteIndex[T any](palette []T, matches func(T) bool) byte {
	index := slices.IndexFunc(palette, matches)
	if index < 0 {
		panic("no such palette entry")
	}
	return byte(index)
}

func textIndex(text string) byte {
	return paletteIndex(scriptTexts, func(entry string) bool { return entry == text })
}

func attributesIndex(name string) byte {
	return paletteIndex(scriptAttributes, func(entry namedAttributes) bool { return entry.name == name })
}

func insertStep(at int, text string) []byte {
	return []byte{stepInsert, byte(at), textIndex(text), 0}
}

func insertFormattedStep(at int, text, attributes string) []byte {
	return []byte{stepInsertFormatted, byte(at), textIndex(text), attributesIndex(attributes)}
}

func formatStep(at, length int, attributes string) []byte {
	return []byte{stepFormat, byte(at), byte(length), attributesIndex(attributes)}
}

func unformatStep(at, length int, key string) []byte {
	return []byte{stepUnformat, byte(at), byte(length), paletteIndex(scriptKeys, func(entry string) bool { return entry == key })}
}

func deleteStep(at, length int) []byte {
	return []byte{stepDelete, byte(at), byte(length), 0}
}

func embedStep(at int, embed any, attributes string) []byte {
	index := paletteIndex(scriptEmbeds, func(entry any) bool { return fmt.Sprintf("%#v", entry) == fmt.Sprintf("%#v", embed) })
	return []byte{stepEmbed, byte(at), index, attributesIndex(attributes)}
}

func elementAttributeStep(name string, value any) []byte {
	return []byte{stepElementAttribute,
		paletteIndex(scriptElementAttributeNames, func(entry string) bool { return entry == name }),
		paletteIndex(scriptElementAttributeValues, func(entry any) bool { return fmt.Sprintf("%#v", entry) == fmt.Sprintf("%#v", value) }),
		0}
}

func craftedStep(name string) []byte {
	return []byte{stepCrafted, paletteIndex(craftedItems, func(entry craftedItem) bool { return entry.name == name }), 0, 0}
}

// maxScriptSteps bounds a fuzzed script, so each input stays quick.
const maxScriptSteps = 64

// runTextScript is a document holding a paragraph whose text script wrote, step by step as an
// editor writes it, each step its own transaction. A crafted update ygo refuses leaves the document
// as ygo leaves it.
func runTextScript(script []byte) *crdt.Doc {
	doc := crdt.New(crdt.WithClientID(1))
	fragment := doc.GetXmlFragment("prosemirror")
	paragraph := crdt.NewYXmlElement("paragraph")
	text := crdt.NewYXmlText()
	doc.Transact(func(txn *crdt.Transaction) {
		fragment.InsertElement(txn, 0, paragraph)
		paragraph.InsertText(txn, 0, text)
	})
	for step := 0; step < maxScriptSteps && (step+1)*textStepSize <= len(script); step++ {
		kind, a, b, c := script[step*textStepSize]%stepKinds, int(script[step*textStepSize+1]), int(script[step*textStepSize+2]), int(script[step*textStepSize+3])
		at := a % (text.Len() + 1)
		length := b % (text.Len() - at + 1)
		if kind == stepCrafted {
			_ = appendCrafted(doc, text, craftedItems[a%len(craftedItems)])
			continue
		}
		doc.Transact(func(txn *crdt.Transaction) {
			switch kind {
			case stepInsert:
				text.Insert(txn, at, scriptTexts[b%len(scriptTexts)], nil)
			case stepInsertFormatted:
				text.Insert(txn, at, scriptTexts[b%len(scriptTexts)], scriptAttributes[c%len(scriptAttributes)].attrs)
			case stepFormat:
				text.Format(txn, at, length, scriptAttributes[c%len(scriptAttributes)].attrs)
			case stepUnformat:
				text.Format(txn, at, length, crdt.Attributes{scriptKeys[c%len(scriptKeys)]: nil})
			case stepDelete:
				text.Delete(txn, at, length)
			case stepEmbed:
				text.InsertEmbed(txn, at, scriptEmbeds[b%len(scriptEmbeds)], scriptAttributes[c%len(scriptAttributes)].attrs)
			case stepElementAttribute:
				name := scriptElementAttributeNames[a%len(scriptElementAttributeNames)]
				if value := scriptElementAttributeValues[b%len(scriptElementAttributeValues)]; value == deleteAttribute {
					paragraph.DeleteAttribute(txn, name)
				} else {
					paragraph.SetAttributeValue(txn, name, value)
				}
			}
		})
	}
	return doc
}

type textScriptCase struct {
	name   string
	script []byte
}

// textScriptCases are texts as a browser's edits leave them, and as crafted updates do.
func textScriptCases() []textScriptCase {
	steps := func(steps ...[]byte) []byte { return slices.Concat(steps...) }
	return []textScriptCase{
		{"plain insert", steps(insertStep(0, "de f"))},
		{"insert with attributes", steps(insertFormattedStep(0, "bc", "link a"), insertStep(2, "a"), insertFormattedStep(0, "de f", "strong and link a"))},
		{"insert into a formatted run splits it", steps(insertFormattedStep(0, "de f", "strong"), insertStep(2, "bc"), insertFormattedStep(1, "a", "emphasis"))},
		{"format over a range and back", steps(insertStep(0, "de f"), formatStep(1, 2, "strong"), unformatStep(1, 2, "strong"))},
		{"format back with a nil value over part", steps(insertStep(0, "de f"), formatStep(0, 4, "link a"), unformatStep(1, 1, "link"), formatStep(2, 2, "link b"))},
		{"overlapping formats", steps(insertStep(0, "de f"), insertStep(4, "bc"), formatStep(0, 4, "strong"), formatStep(2, 4, "emphasis"), formatStep(1, 2, "comment"))},
		{"delete inside a run", steps(insertFormattedStep(0, "de f", "strong"), deleteStep(1, 2))},
		{"delete across runs", steps(insertFormattedStep(0, "de f", "strong"), insertFormattedStep(4, "bc", "emphasis"), insertStep(6, "a"), deleteStep(2, 4))},
		{"delete a whole run between equal runs", steps(insertFormattedStep(0, "bc", "strong"), insertStep(2, "a"), insertFormattedStep(3, "bc", "strong"), deleteStep(2, 1))},
		{"astral characters split", steps(insertStep(0, "😀"), insertStep(0, "😀"), formatStep(2, 2, "strong"), insertStep(2, "a"))},
		{"a NaN mark value never joins its runs", steps(insertFormattedStep(0, "bc", "strong NaN"), formatStep(1, 1, "strong NaN"))},
		{"mark value not an object", steps(insertStep(0, "a"), insertFormattedStep(1, "bc", "strong not an object"))},
		{"two mark values not objects on one run", steps(insertFormattedStep(0, "bc", "two not objects"))},
		{"unknown mark", steps(insertFormattedStep(0, "bc", "unknown mark"))},
		{"string embeds", steps(insertStep(0, "de f"), embedStep(2, "x", "none"), embedStep(0, "yz", "strong"), embedStep(6, "x", "link a"))},
		{"empty string embed", steps(insertStep(0, "bc"), embedStep(1, "", "none"))},
		{"map embed", steps(insertStep(0, "bc"), embedStep(1, map[string]any{"image": "y"}, "none"))},
		{"element attributes", steps(insertStep(0, "a"), elementAttributeStep("foo", "v"), elementAttributeStep("bar", map[string]any{"a": "b"}), elementAttributeStep("bar", deleteAttribute), elementAttributeStep("foo", nil), elementAttributeStep("bar", []any{"x"}))},
		{"crafted embed string", steps(insertStep(0, "a"), craftedStep("embed string"), craftedStep("string"))},
		{"crafted embed empty string", steps(insertStep(0, "a"), craftedStep("embed empty string"), craftedStep("string"))},
		{"crafted embed map", steps(insertStep(0, "a"), craftedStep("embed map"))},
		{"crafted embed null", steps(insertStep(0, "a"), craftedStep("embed null"))},
		{"crafted empty string between formats", steps(insertStep(0, "a"), craftedStep("format strong"), craftedStep("empty string"), craftedStep("format strong null"), craftedStep("string"))},
		{"crafted format not an object", steps(insertStep(0, "a"), craftedStep("format strong not an object"), craftedStep("string"))},
		{"crafted contents a text ignores", steps(insertStep(0, "a"), craftedStep("any"), craftedStep("json"), craftedStep("binary"), craftedStep("map type"), craftedStep("deleted"), craftedStep("any NaN"), craftedStep("string"))},
	}
}

// readersAgreeOn fails t unless ReadUnshared reads doc's tree as Read does, on doc itself and on
// the documents its V1 and V2 updates decode into, as the read cache's fold decodes a history.
func readersAgreeOn(t *testing.T, doc *crdt.Doc) {
	t.Helper()
	if diff := readersDiffer(doc.GetXmlFragment("prosemirror")); diff != "" {
		t.Fatalf("the live document:\n%s", diff)
	}
	v1 := crdt.New()
	if err := crdt.ApplyUpdateV1(v1, crdt.EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
		t.Fatalf("decode its V1 update: %v", err)
	}
	if diff := readersDiffer(v1.GetXmlFragment("prosemirror")); diff != "" {
		t.Fatalf("its V1 update decoded:\n%s", diff)
	}
	v2 := crdt.New()
	if err := crdt.ApplyUpdateV2(v2, crdt.EncodeStateAsUpdateV2(doc, nil), nil); err != nil {
		t.Fatalf("decode its V2 update: %v", err)
	}
	if diff := readersDiffer(v2.GetXmlFragment("prosemirror")); diff != "" {
		t.Fatalf("its V2 update decoded:\n%s", diff)
	}
}

// itemsDoc is a document holding a paragraph whose text's item list is items, made by hand: the
// contents and the order no update ygo decodes can make, such as a format whose value is a typed
// nil or an empty string. The walks read only an item's content, whether it is deleted, and its
// right neighbour.
func itemsDoc(items ...*crdt.Item) *crdt.Doc {
	doc := runTextScript(insertStep(0, "a"))
	text := crafted.FirstText(doc.GetXmlFragment("prosemirror"))
	for index := range items {
		if index > 0 {
			items[index].Left = items[index-1]
			items[index-1].Right = items[index]
		}
	}
	var head *crdt.Item
	if len(items) > 0 {
		head = items[0]
	}
	*yTextStartField(text) = head
	return doc
}

func stringItem(s string) *crdt.Item { return &crdt.Item{Content: crdt.NewContentString(s)} }
func formatItem(key string, value any) *crdt.Item {
	return &crdt.Item{Content: crdt.NewContentFormat(key, value)}
}
func embedItem(value any) *crdt.Item        { return &crdt.Item{Content: crdt.NewContentEmbed(value)} }
func contentItem(c crdt.Content) *crdt.Item { return &crdt.Item{Content: c} }
func deleted(item *crdt.Item) *crdt.Item    { item.Deleted = true; return item }

type itemsCase struct {
	name  string
	items func() []*crdt.Item
}

func itemsCases() []itemsCase {
	empty := map[string]any{}
	return []itemsCase{
		{"a typed nil format value sets its key", func() []*crdt.Item {
			return []*crdt.Item{stringItem("a"), formatItem("strong", map[string]any(nil)), stringItem("b")}
		}},
		{"a typed nil format value that is not an object", func() []*crdt.Item {
			return []*crdt.Item{formatItem("strong", (*int)(nil)), stringItem("a")}
		}},
		{"a nil format value deletes its key", func() []*crdt.Item {
			return []*crdt.Item{formatItem("strong", empty), stringItem("a"), formatItem("strong", nil), stringItem("b")}
		}},
		{"an empty string between formats ends the run", func() []*crdt.Item {
			return []*crdt.Item{stringItem("a"), formatItem("strong", empty), stringItem(""), formatItem("strong", nil), stringItem("b")}
		}},
		{"an empty string after a format", func() []*crdt.Item {
			return []*crdt.Item{stringItem("a"), formatItem("strong", empty), stringItem(""), stringItem("b")}
		}},
		{"only an empty string", func() []*crdt.Item {
			return []*crdt.Item{formatItem("strong", empty), stringItem("")}
		}},
		{"deleted items are skipped", func() []*crdt.Item {
			return []*crdt.Item{stringItem("a"), deleted(formatItem("strong", empty)), deleted(stringItem("x")), deleted(embedItem(7.0)), stringItem("b")}
		}},
		{"contents a text ignores join no run", func() []*crdt.Item {
			return []*crdt.Item{stringItem("a"), contentItem(crdt.NewContentAny(1.0)), contentItem(crdt.NewContentJSON("j")),
				contentItem(crdt.NewContentBinary([]byte{1})), contentItem(crdt.NewContentDeleted(1)), stringItem("b")}
		}},
		{"a string embed carries the live attributes", func() []*crdt.Item {
			return []*crdt.Item{formatItem("strong", empty), stringItem("a"), formatItem("emphasis", empty), embedItem("e"), stringItem("b")}
		}},
		{"a string embed between equal runs", func() []*crdt.Item {
			return []*crdt.Item{stringItem("a"), embedItem("e"), embedItem("f"), stringItem("b")}
		}},
		{"an empty string embed", func() []*crdt.Item {
			return []*crdt.Item{stringItem("a"), embedItem(""), stringItem("b")}
		}},
		{"a nil embed", func() []*crdt.Item { return []*crdt.Item{stringItem("a"), embedItem(nil)} }},
		{"a map embed", func() []*crdt.Item { return []*crdt.Item{embedItem(map[string]any{"a": 1.0}), stringItem("a")} }},
		{"a mark refused before an embed refused", func() []*crdt.Item {
			return []*crdt.Item{formatItem("strong", true), stringItem("a"), embedItem(1.0)}
		}},
		{"an equal format value joins the runs", func() []*crdt.Item {
			return []*crdt.Item{formatItem("strong", empty), stringItem("a"), formatItem("strong", map[string]any{}), stringItem("b")}
		}},
		{"a NaN format value never joins the runs", func() []*crdt.Item {
			return []*crdt.Item{formatItem("strong", map[string]any{"x": math.NaN()}), stringItem("a"), formatItem("strong", map[string]any{"x": math.NaN()}), stringItem("b")}
		}},
		{"Attributes never equals a map", func() []*crdt.Item {
			return []*crdt.Item{formatItem("strong", empty), stringItem("a"), formatItem("strong", crdt.Attributes{}), stringItem("b")}
		}},
		{"a format back to the run's attributes joins it", func() []*crdt.Item {
			return []*crdt.Item{stringItem("a"), formatItem("strong", empty), formatItem("strong", nil), stringItem("b")}
		}},
		{"two marks not objects on one run", func() []*crdt.Item {
			return []*crdt.Item{formatItem("strong", true), formatItem("emphasis", "x"), stringItem("a")}
		}},
	}
}

func TestReadUnsharedReadsWhatReadReads(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("testdata", "corpus"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Run("corpus/"+entry.Name(), func(t *testing.T) {
			source, err := os.ReadFile(filepath.Join("testdata", "corpus", entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			tree, err := Parse(string(source))
			if err != nil {
				t.Skipf("Parse refuses it: %v", err)
			}
			readersAgreeOn(t, updatedDoc(t, tree))
		})
	}
	for _, fx := range loadFixtures(t) {
		t.Run("fixture/"+fx.Name, func(t *testing.T) {
			browser := crdt.New()
			if err := crdt.ApplyUpdateV1(browser, fx.YjsUpdateV1, nil); err != nil {
				t.Fatalf("apply browser update: %v", err)
			}
			readersAgreeOn(t, browser)
			tree, err := FromJSON(fx.PMJSON)
			if err != nil {
				t.Fatal(err)
			}
			readersAgreeOn(t, updatedDoc(t, tree))
		})
	}
	for _, test := range textScriptCases() {
		t.Run("script/"+test.name, func(t *testing.T) {
			readersAgreeOn(t, runTextScript(test.script))
		})
	}
	for _, test := range itemsCases() {
		t.Run("items/"+test.name, func(t *testing.T) {
			if diff := readersDiffer(itemsDoc(test.items()...).GetXmlFragment("prosemirror")); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

// updatedDoc is a document Update wrote tree into.
func updatedDoc(t *testing.T, tree *Node) *crdt.Doc {
	t.Helper()
	doc := crdt.New(crdt.WithClientID(1))
	writeUpdate(t, doc, doc.GetXmlFragment("prosemirror"), tree)
	return doc
}

// A run's text is a string of its own, never a string item's, which an item split leaves a slice
// of the string it split.
func TestReadUnsharedTextOwnsItsString(t *testing.T) {
	split := crdt.NewContentString("abc")
	right := split.Splice(1).(*crdt.ContentString)
	valid, err := ReadUnshared(itemsDoc(contentItem(split), formatItem("strong", map[string]any{}), contentItem(right)).GetXmlFragment("prosemirror"))
	if err != nil {
		t.Fatal(err)
	}
	texts := valid.Node().Children[0].Children
	if len(texts) != 2 || texts[0].Text != "a" || texts[1].Text != "bc" {
		t.Fatalf("read %s, want the texts a and bc", readerDump(valid.Node()))
	}
	for index, item := range []*crdt.ContentString{split, right} {
		if unsafe.StringData(texts[index].Text) == unsafe.StringData(item.Str) {
			t.Errorf("text %q shares its string item's bytes", texts[index].Text)
		}
	}
}

func FuzzReadUnshared(f *testing.F) {
	for _, test := range textScriptCases() {
		f.Add(test.script)
	}
	f.Fuzz(func(t *testing.T, script []byte) {
		readersAgreeOn(t, runTextScript(script))
	})
}
