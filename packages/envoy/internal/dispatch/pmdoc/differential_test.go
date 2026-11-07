package pmdoc

// The pmdoc differential (gen/differential.sh) reads one corpus at two revisions. Each revision
// reads every document, renders what it read and what the browser editor's engine read, and reads
// each rendering back; the engine reads the same renderings; then the head judges the base against
// itself. This file is the reading, which both revisions run: the driver copies it into the base
// revision, so it uses only what every revision since the write-path read-back check has. The
// judgment, which runs in the head's binary alone and asks this revision's own errors, is
// differential_gate_test.go, which the driver does not copy. Both tests skip unless the driver
// sets their inputs.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// diffDump is one revision's record of one document.
type diffDump struct {
	ID string `json:"id"`
	// Tree is what Parse reads the document as, block ids dropped; Parse is its refusal.
	Tree  json.RawMessage `json:"tree,omitempty"`
	Parse string          `json:"parse,omitempty"`
	// Write is ParseForWrite's refusal: what an upload of the document is answered. Whether that
	// refusal is of the rendering the document would be stored as is read from the error itself,
	// by the head's own binary at the comparison (refusesItsRendering), since the dump is written
	// by each revision's binary and a refusal's sentence is no contract.
	Write string `json:"write,omitempty"`
	// Render is Tree's rendering, the markdown the document is stored as, and ReadsBack whether
	// Parse reads it back as Tree.
	Render    *string `json:"render"`
	RenderErr string  `json:"render_err,omitempty"`
	ReadsBack bool    `json:"reads_back"`
	// FixedPoint is whether Render is the document's markdown byte for byte: reading the
	// document and writing it back gives back what was read.
	FixedPoint bool `json:"fixed_point"`
	// EngineRender is the rendering of the engine's reading of the document, the tree a browser
	// holds, and EngineReadsBack whether Parse reads it back as that tree.
	EngineRender    *string `json:"engine_render"`
	EngineRenderErr string  `json:"engine_render_err,omitempty"`
	EngineReadsBack bool    `json:"engine_reads_back"`
	// Panics names each step that panicked, recovered or not.
	Panics []string `json:"panics,omitempty"`
}

// diffReading is the engine's reading of one markdown field: {pm} or {err}; nil where the field
// was null.
type diffReading struct {
	PM  json.RawMessage `json:"pm"`
	Err string          `json:"err"`
}

// diffLines reads a JSONL file one object at a time, in order.
type diffLines struct {
	name    string
	scanner *bufio.Scanner
	file    *os.File
}

func openDiffLines(t *testing.T, variable string) *diffLines {
	t.Helper()
	path := os.Getenv(variable)
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("%s: %v", variable, err)
	}
	t.Cleanup(func() { file.Close() })
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<30)
	return &diffLines{name: path, scanner: scanner, file: file}
}

func (lines *diffLines) next(t *testing.T, into any) bool {
	t.Helper()
	for lines.scanner.Scan() {
		if strings.TrimSpace(lines.scanner.Text()) == "" {
			continue
		}
		if err := json.Unmarshal(lines.scanner.Bytes(), into); err != nil {
			t.Fatalf("%s: %v", lines.name, err)
		}
		return true
	}
	if err := lines.scanner.Err(); err != nil {
		t.Fatalf("%s: %v", lines.name, err)
	}
	return false
}

// diffStep runs one step, recording a panic, recovered into ErrPanic or not, under name.
func diffStep(dump *diffDump, name string, step func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: %v", ErrPanic, recovered)
		}
		if err != nil && errors.Is(err, ErrPanic) {
			dump.Panics = append(dump.Panics, name)
		}
	}()
	return step()
}

// diffRead decodes the engine's ProseMirror JSON as Go holds the document, and reports whether
// Go's schema refused what the engine read, which is not the engine failing to read. The engine's
// reading is compared as it stands: both readers hold a mark on text alone, never on an inline
// leaf, as the browser editor's store (y-prosemirror) does, so FromJSON refuses a reading carrying
// one on a leaf, and TestDifferentialCompare counts that document under `engine reading Go's
// schema refuses`, which gates, rather than under `engine cannot read`, which counts an engine
// throw and is reported alone. Each short table body row is padded to its header's width as Parse
// pads it, the documented canonicalisation. No revision is judged by these.
func diffRead(reading *diffReading) (*Node, bool) {
	if reading == nil || reading.Err != "" || reading.PM == nil {
		return nil, false
	}
	doc, err := FromJSON(reading.PM)
	if err != nil {
		return nil, true
	}
	padShortRows(doc)
	return doc, false
}

// diffTree is the tree diffRead reads, for a caller that treats either refusal the same way.
func diffTree(reading *diffReading) *Node {
	doc, _ := diffRead(reading)
	return doc
}

// padShortRows gives every short body row of every table under node the cells Parse pads it with,
// each taking its header cell's alignment. A row holding no cell, which both parsers read a table
// with no body row as holding, stays empty.
func padShortRows(node *Node) {
	if node.Type != "table" || len(node.Children) == 0 {
		for _, child := range node.Children {
			padShortRows(child)
		}
		return
	}
	header := node.Children[0]
	for _, row := range node.Children[1:] {
		for column := len(row.Children); len(row.Children) > 0 && column < len(header.Children); column++ {
			row.Children = append(row.Children, &Node{
				Type:     "table_cell",
				Attrs:    Attrs{"alignment": header.Children[column].Attrs["alignment"], "colspan": 1, "colwidth": nil, "rowspan": 1},
				Children: []*Node{{Type: "paragraph"}},
			})
		}
	}
}

func withoutBlockIDs(doc *Node) *Node {
	out := cloneNode(doc)
	Walk(out, func(node *Node) bool {
		delete(node.Attrs, BlockIDAttr)
		return true
	})
	return out
}

func TestDifferentialDump(t *testing.T) {
	if os.Getenv("PMDOC_DIFF_OUT") == "" {
		t.Skip("PMDOC_DIFF_OUT unset; gen/differential.sh runs this")
	}
	sources := openDiffLines(t, "PMDOC_DIFF_CORPUS")
	engine := openDiffLines(t, "PMDOC_DIFF_ENGINE")
	out, err := os.Create(os.Getenv("PMDOC_DIFF_OUT"))
	if err != nil {
		t.Fatal(err)
	}
	// A typed block's id is written in its markdown, so each step mints ids from a counter
	// restarted for it, and two runs of one revision write the same bytes.
	t.Cleanup(func() { SetBlockIDGenerator(nil) })
	defer out.Close()
	writer := bufio.NewWriterSize(out, 1<<20)
	defer writer.Flush()
	for {
		var source struct{ ID, MD string }
		if !sources.next(t, &source) {
			break
		}
		var read struct {
			ID string       `json:"id"`
			MD *diffReading `json:"md"`
		}
		if !engine.next(t, &read) || read.ID != source.ID {
			t.Fatalf("the engine's readings do not follow the corpus at %q", source.ID)
		}
		dump := diffDump{ID: source.ID}
		var tree *Node
		SetBlockIDGenerator(counterBlockIDs())
		if err := diffStep(&dump, "parse", func() (err error) { tree, err = Parse(source.MD); return err }); err != nil {
			dump.Parse = err.Error()
		}
		SetBlockIDGenerator(counterBlockIDs())
		if err := diffStep(&dump, "write", func() error { _, err := ParseForWrite(source.MD, nil); return err }); err != nil {
			dump.Write = err.Error()
		}
		if tree != nil {
			dump.Tree, _ = withoutBlockIDs(tree).JSON()
			dump.Render, dump.RenderErr, dump.ReadsBack = diffRender(&dump, "render", tree)
			dump.FixedPoint = dump.Render != nil && *dump.Render == source.MD
		}
		if engineTree := diffTree(read.MD); engineTree != nil {
			dump.EngineRender, dump.EngineRenderErr, dump.EngineReadsBack = diffRender(&dump, "engine render", engineTree)
		}
		line, err := json.Marshal(dump)
		if err != nil {
			t.Fatal(err)
		}
		writer.Write(append(line, '\n'))
	}
}

// diffRender renders tree and reports whether Parse reads the rendering back as tree.
func diffRender(dump *diffDump, name string, tree *Node) (*string, string, bool) {
	var markdown string
	if err := diffStep(dump, name, func() (err error) { markdown, err = Render(tree); return err }); err != nil {
		return nil, err.Error(), false
	}
	var back *Node
	if err := diffStep(dump, name+" read back", func() (err error) { back, err = Parse(markdown); return err }); err != nil {
		return &markdown, "", false
	}
	return &markdown, "", StripAnchorMarks(tree).Equal(back)
}
