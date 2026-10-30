package docs

import (
	"encoding/json"
	"errors"
	"maps"
	"math"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// separateTokens is the oracle the one-pass tokens are held to: each block's token encoded on its
// own (nodeToken), in pre-order, the last node carrying an id holding it, and the first block that
// cannot be encoded its error.
func separateTokens(t *testing.T, tree *pmdoc.Node) (map[string]string, error) {
	t.Helper()
	tokens := make(map[string]string)
	var failure error
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if id == "" {
			return true
		}
		token, err := nodeToken(node)
		if err != nil {
			failure = err
			return false
		}
		tokens[id] = token
		return true
	})
	if failure != nil {
		return nil, failure
	}
	return tokens, nil
}

// parseTokenTree parses markdown into the tree a test plants its block ids and values in. The ids
// the parse mints are removed, so every id in the tree is one the test chose.
func parseTokenTree(t *testing.T, markdown string) *pmdoc.Node {
	t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse %q: %v", markdown, err)
	}
	removeBlockIDs(tree)
	return tree
}

// setBlockID gives node the block id id.
func setBlockID(node *pmdoc.Node, id string) {
	if node.Attrs == nil {
		node.Attrs = pmdoc.Attrs{}
	}
	node.Attrs[pmdoc.BlockIDAttr] = id
}

// validTokenTree is tree validated as the read cache validates it, for the reads that take a
// validated tree (documentTokens, renderedBlocks, buildDocumentRead).
func validTokenTree(t *testing.T, tree *pmdoc.Node) pmdoc.ValidDocument {
	t.Helper()
	valid, err := pmdoc.ValidateDocument(tree, "BlockPathsAndTableRows")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	return valid
}

// sameError reports whether got is want as a caller tells errors apart: the same message and the
// same answer to whether it is a document outside the schema.
func sameError(got, want error) bool {
	if got == nil || want == nil {
		return got == want
	}
	return got.Error() == want.Error() && errors.Is(got, ErrDocOutsideSchema) == errors.Is(want, ErrDocOutsideSchema)
}

func TestDocumentTokensKeepTheLastNodeInPreOrderForARepeatedID(t *testing.T) {
	tree := parseTokenTree(t, "- Item one.\n\nFirst.\n\nSecond.\n")
	list := tree.Children[0]
	item := list.Children[0]
	itemParagraph := item.Children[0]
	first, second := tree.Children[1], tree.Children[2]
	setBlockID(item, "dup-a")
	setBlockID(itemParagraph, "dup-a")
	setBlockID(first, "dup-b")
	setBlockID(second, "dup-b")
	if err := tree.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	want, err := separateTokens(t, tree)
	if err != nil {
		t.Fatalf("separate tokens: %v", err)
	}
	wantDocument, err := nodeToken(tree)
	if err != nil {
		t.Fatalf("document token: %v", err)
	}
	for id, node := range map[string]*pmdoc.Node{"dup-a": itemParagraph, "dup-b": second} {
		token, err := nodeToken(node)
		if err != nil {
			t.Fatal(err)
		}
		if want[id] != token {
			t.Fatalf("oracle holds %q as %s, want the last node's %s", id, want[id], token)
		}
	}

	tokens := documentTokens(validTokenTree(t, tree))
	if tokens.documentErr != nil || tokens.blocksErr != nil {
		t.Fatalf("documentTokens errors: document %v, blocks %v", tokens.documentErr, tokens.blocksErr)
	}
	if tokens.document != wantDocument {
		t.Fatalf("document token = %s, want %s", tokens.document, wantDocument)
	}
	if !maps.Equal(tokens.blocks, want) {
		t.Fatalf("block tokens = %v, want %v", tokens.blocks, want)
	}

	document, blocks, err := encodedTokens(tree)
	if err != nil {
		t.Fatalf("encodedTokens: %v", err)
	}
	if document != wantDocument || !maps.Equal(blocks, want) {
		t.Fatalf("encodedTokens = %s %v, want %s %v", document, blocks, wantDocument, want)
	}
}

func TestDocumentTokensRefuseAnUnencodableValueInsideABlockAsTheBlockDoes(t *testing.T) {
	tree := parseTokenTree(t, "Intro.\n\nBody text.\n")
	paragraph := tree.Children[1]
	setBlockID(tree.Children[0], "intro")
	setBlockID(paragraph, "body")
	text := paragraph.Children[0]
	text.Marks = append(text.Marks, pmdoc.Mark{Type: "strong", Attrs: pmdoc.Attrs{"weight": math.NaN()}})
	if err := tree.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	_, wantBlocksErr := separateTokens(t, tree)
	if wantBlocksErr == nil {
		t.Fatal("the oracle encoded a block holding NaN")
	}
	_, wantDocumentErr := nodeToken(tree)
	if wantDocumentErr == nil {
		t.Fatal("the document holding NaN encoded")
	}

	tokens := documentTokens(validTokenTree(t, tree))
	if tokens.documentErr == nil || tokens.documentErr.Error() != wantDocumentErr.Error() {
		t.Fatalf("documentErr = %v, want %v", tokens.documentErr, wantDocumentErr)
	}
	if tokens.blocksErr == nil || tokens.blocksErr.Error() != wantBlocksErr.Error() {
		t.Fatalf("blocksErr = %v, want %v", tokens.blocksErr, wantBlocksErr)
	}
	if tokens.blocks != nil {
		t.Fatalf("blocks = %v, want nil", tokens.blocks)
	}

	want := documentSchemaError(wantBlocksErr)
	rows, err := pmdoc.TableRows(tree)
	if err != nil {
		t.Fatal(err)
	}
	_, err = renderedBlocks(validTokenTree(t, tree), rows)
	if !sameError(err, want) {
		t.Fatalf("renderedBlocks error = %v, want %v", err, want)
	}
	var unsupported *json.UnsupportedValueError
	if !errors.As(err, &unsupported) {
		t.Fatalf("renderedBlocks error %v is not a *json.UnsupportedValueError", err)
	}
	_, err = buildDocumentRead("artifact", DocumentStamp{Version: 1}, validTokenTree(t, tree))
	if !sameError(err, want) {
		t.Fatalf("buildDocumentRead error = %v, want %v", err, want)
	}
	if !errors.As(err, &unsupported) {
		t.Fatalf("buildDocumentRead error %v is not a *json.UnsupportedValueError", err)
	}

	err = checkEditPrecondition(tree, model.EditPrecondition{Blocks: []model.EditBlockPrecondition{{ID: "intro", Token: "sha256:stale"}}})
	if !sameError(err, wantDocumentErr) {
		t.Fatalf("checkEditPrecondition error = %v, want %v", err, wantDocumentErr)
	}
	if !errors.As(err, &unsupported) {
		t.Fatalf("checkEditPrecondition error %v is not a *json.UnsupportedValueError", err)
	}
}

func TestDocumentTokensKeepEveryBlockWhenOnlyTheDocumentCannotBeEncoded(t *testing.T) {
	tree := parseTokenTree(t, "Intro.\n\n- Item one.\n- Item two.\n")
	setBlockID(tree.Children[0], "intro")
	setBlockID(tree.Children[1], "list")
	setBlockID(tree.Children[1].Children[1], "item-two")
	setBlockID(tree.Children[1].Children[1].Children[0], "item-two-text")
	if tree.Attrs == nil {
		tree.Attrs = pmdoc.Attrs{}
	}
	tree.Attrs["weight"] = math.NaN()
	if err := tree.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	want, err := separateTokens(t, tree)
	if err != nil {
		t.Fatalf("separate tokens: %v", err)
	}
	if len(want) != 4 {
		t.Fatalf("oracle holds %d blocks, want 4: %v", len(want), want)
	}
	_, wantDocumentErr := nodeToken(tree)
	if wantDocumentErr == nil {
		t.Fatal("the document holding NaN encoded")
	}

	tokens := documentTokens(validTokenTree(t, tree))
	if tokens.blocksErr != nil {
		t.Fatalf("blocksErr = %v, want nil", tokens.blocksErr)
	}
	if !maps.Equal(tokens.blocks, want) {
		t.Fatalf("block tokens = %v, want %v", tokens.blocks, want)
	}
	if tokens.documentErr == nil || tokens.documentErr.Error() != wantDocumentErr.Error() {
		t.Fatalf("documentErr = %v, want %v", tokens.documentErr, wantDocumentErr)
	}

	_, err = buildDocumentRead("artifact", DocumentStamp{Version: 1}, validTokenTree(t, tree))
	if !sameError(err, wantDocumentErr) {
		t.Fatalf("buildDocumentRead error = %v, want %v", err, wantDocumentErr)
	}
	if _, ok := err.(*json.UnsupportedValueError); !ok {
		t.Fatalf("buildDocumentRead error %v (%T) is not an unwrapped *json.UnsupportedValueError", err, err)
	}

	_, blocks, err := documentBlocks(tree)
	if err != nil {
		t.Fatalf("documentBlocks: %v", err)
	}
	if len(blocks) == 0 {
		t.Fatal("documentBlocks listed no blocks")
	}
	for _, block := range blocks {
		if token, found := want[block.ID]; !found || block.Token != token {
			t.Fatalf("block %q token = %s, want %s", block.ID, block.Token, token)
		}
	}
}
