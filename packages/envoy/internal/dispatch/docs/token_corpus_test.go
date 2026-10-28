package docs

// The token corpus differential reads a directory of markdown specs at two revisions and dumps
// everything the document read builds from each, so a change to how the read computes its tokens
// can be judged by comparing the two dumps byte for byte (cmp). Both the test and the benchmark
// skip unless DISPATCH_TOKEN_CORPUS names the directory. Each revision dumps with its own copy of
// this file, which reads each spec as that revision's cold read folds it (foldLoaded here); the
// dump's lines keep one format across revisions, so the two dumps compare.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// tokenCorpusLine is one spec's record in the dump. A spec any step refuses carries only its file
// name and the refusal, so both revisions write a line for every spec and the dumps stay aligned.
type tokenCorpusLine struct {
	File               string              `json:"file"`
	Token              string              `json:"token,omitempty"`
	MarkdownSha256     string              `json:"markdownSha256,omitempty"`
	Blocks             []tokenCorpusBlock  `json:"blocks,omitempty"`
	TableDescendantIDs map[string][]string `json:"tableDescendantIDs,omitempty"`
	Error              string              `json:"error,omitempty"`
}

// tokenCorpusBlock is a block as the read holds it. ArtifactBlock leaves its descendant ids out of
// its JSON, so they are named here to have the dump compare them too.
type tokenCorpusBlock struct {
	model.ArtifactBlock
	DescendantIDs []string `json:"descendantIds,omitempty"`
}

// tokenCorpusSpecs lists the directory's markdown files in sorted file-name order, which is the
// order the dump lines take.
func tokenCorpusSpecs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".md") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// tokenCorpusDoc parses markdown into the document a room would hold: block ids renamed in
// pre-order so both revisions mint the same ones, anchor marks on every fifth text node so the
// tokens cover marks, written through encodeDocumentTree and applied into a fresh document as a
// load applies the stored update. A panic in any step is returned as its error, since a spec
// either revision cannot read must still be a line and not a failed test.
func tokenCorpusDoc(markdown string) (doc *crdt.Doc, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			doc, err = nil, fmt.Errorf("panic: %v", recovered)
		}
	}()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		return nil, err
	}
	pmdoc.EnsureBlockIDs(tree)
	blockIDs := 0
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if id, ok := node.Attrs[pmdoc.BlockIDAttr].(string); ok && id != "" {
			blockIDs++
			node.Attrs[pmdoc.BlockIDAttr] = "b-" + strconv.Itoa(blockIDs)
		}
		return true
	})
	tokenCorpusMark(tree, nil, new(int))
	update, err := encodeDocumentTree(tree)
	if err != nil {
		return nil, err
	}
	doc = crdt.New()
	if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
		return nil, err
	}
	return doc, nil
}

// tokenCorpusMark gives every fifth text node under node, counted in pre-order by texts, a comment
// mark as MarkQuote writes one, with the time a stored comment carries, and an authorship mark
// unless the text is code, keeping the rule simple. Only the parent's subtree is validated: the
// parsed tree is valid, and a text node's marks are judged only by the node and its parent, so a
// node whose marks the parent refuses goes without them.
func tokenCorpusMark(node, parent *pmdoc.Node, texts *int) {
	if node.Type == "text" {
		*texts++
		if *texts%5 != 0 || parent == nil {
			return
		}
		n := *texts / 5
		kept := node.Marks
		marks := append([]pmdoc.Mark(nil), node.Marks...)
		marks = append(marks, pmdoc.Mark{Type: "proofComment", Attrs: pmdoc.Attrs{
			"id": "c-" + strconv.Itoa(n), "by": "user:corpus", "at": int64(1700000000 + n),
		}})
		if parent.Type != "code_block" {
			marks = append(marks, pmdoc.Mark{Type: "proofAuthored", Attrs: pmdoc.Attrs{"by": "ai:corpus"}})
		}
		node.Marks = marks
		if parent.Validate() != nil {
			node.Marks = kept
		}
		return
	}
	for _, child := range node.Children {
		tokenCorpusMark(child, node, texts)
	}
}

// tokenCorpusRead folds doc as foldDocument folds the document it loads (foldLoaded), the call the
// corpus times: the read and its tree, or why the fold has no read. A panic is returned as its
// error, as tokenCorpusDoc returns one.
func tokenCorpusRead(doc *crdt.Doc) (read *documentRead, tree *pmdoc.Node, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			read, tree, err = nil, nil, fmt.Errorf("panic: %v", recovered)
		}
	}()
	fold, err := foldLoaded("corpus", LoadedDocument{Doc: doc})
	if fold.treeErr != nil {
		return nil, nil, fold.treeErr
	}
	if err != nil {
		return nil, nil, err
	}
	return fold.read, fold.tree, nil
}

// tokenCorpusDescendants is the table descendants of tree, a panic returned as its error.
func tokenCorpusDescendants(tree *pmdoc.Node) (descendants map[string][]string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			descendants, err = nil, fmt.Errorf("panic: %v", recovered)
		}
	}()
	return pmdoc.TableDescendantIDs(tree)
}

func TestTokenCorpus(t *testing.T) {
	dir := os.Getenv("DISPATCH_TOKEN_CORPUS")
	if dir == "" {
		t.Skip("DISPATCH_TOKEN_CORPUS unset; set it to a directory of markdown specs to dump their reads")
	}
	outPath := os.Getenv("DISPATCH_TOKEN_CORPUS_OUT")
	if outPath == "" {
		t.Fatal("DISPATCH_TOKEN_CORPUS_OUT must name the file the dump is written to")
	}
	names, err := tokenCorpusSpecs(dir)
	if err != nil {
		t.Fatalf("DISPATCH_TOKEN_CORPUS: %v", err)
	}
	out, err := os.Create(outPath)
	if err != nil {
		t.Fatalf("DISPATCH_TOKEN_CORPUS_OUT: %v", err)
	}
	defer out.Close()
	writer := bufio.NewWriter(out)

	var specs, tokens, markdownBytes int
	var reading time.Duration
	for _, name := range names {
		source, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		specs++
		line := tokenCorpusLine{File: name}
		// The timing is only logged, never dumped: it differs on every run and would break cmp.
		func() {
			doc, err := tokenCorpusDoc(string(source))
			if err != nil {
				line.Error = err.Error()
				return
			}
			started := time.Now()
			read, tree, err := tokenCorpusRead(doc)
			reading += time.Since(started)
			if err != nil {
				line.Error = err.Error()
				return
			}
			descendants, err := tokenCorpusDescendants(tree)
			if err != nil {
				line.Error = err.Error()
				return
			}
			sum := sha256.Sum256([]byte(read.markdown))
			line.Token = read.token
			line.MarkdownSha256 = hex.EncodeToString(sum[:])
			line.TableDescendantIDs = descendants
			line.Blocks = make([]tokenCorpusBlock, len(read.blocks))
			for index, block := range read.blocks {
				line.Blocks[index] = tokenCorpusBlock{ArtifactBlock: block, DescendantIDs: block.DescendantIDs}
				if block.Token != "" {
					tokens++
				}
			}
			markdownBytes += len(read.markdown)
		}()
		encoded, err := json.Marshal(line)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		writer.Write(encoded)
		writer.WriteByte('\n')
	}
	if err := writer.Flush(); err != nil {
		t.Fatalf("DISPATCH_TOKEN_CORPUS_OUT: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("DISPATCH_TOKEN_CORPUS_OUT: %v", err)
	}
	t.Logf("token corpus: %d specs, %d block tokens, %d markdown bytes, foldLoaded %s",
		specs, tokens, markdownBytes, reading)
}

// BenchmarkTokenCorpusLargest measures buildDocumentRead on the corpus's largest spec, its tree
// read outside the timer.
func BenchmarkTokenCorpusLargest(b *testing.B) {
	dir := os.Getenv("DISPATCH_TOKEN_CORPUS")
	if dir == "" {
		b.Skip("DISPATCH_TOKEN_CORPUS unset; set it to a directory of markdown specs to benchmark the largest")
	}
	names, err := tokenCorpusSpecs(dir)
	if err != nil {
		b.Fatal(err)
	}
	var largest string
	var largestSize int64 = -1
	for _, name := range names {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			b.Fatal(err)
		}
		if info.Size() > largestSize {
			largest, largestSize = name, info.Size()
		}
	}
	if largest == "" {
		b.Skip("no markdown specs in DISPATCH_TOKEN_CORPUS")
	}
	source, err := os.ReadFile(filepath.Join(dir, largest))
	if err != nil {
		b.Fatal(err)
	}
	doc, err := tokenCorpusDoc(string(source))
	if err != nil {
		b.Fatalf("%s: %v", largest, err)
	}
	tree, err := treeOf(doc)
	if err != nil {
		b.Fatalf("%s: %v", largest, err)
	}
	b.Logf("largest spec: %s, %d bytes", largest, len(source))
	b.SetBytes(int64(len(source)))
	b.ReportAllocs()
	for b.Loop() {
		valid, err := pmdoc.ValidateDocument(tree, "BlockPathsAndTableRows")
		if err != nil {
			b.Fatal(err)
		}
		if _, err := buildDocumentRead("corpus", DocumentStamp{}, valid); err != nil {
			b.Fatal(err)
		}
	}
}
