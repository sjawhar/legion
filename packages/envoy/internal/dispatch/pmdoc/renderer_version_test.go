package pmdoc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"testing"
)

// rendererVersionGolden is testdata/renderer-version.json: the renderer version this build must
// stand at, the hash of what Render writes of every corpus document, and the hash of every corpus
// document's token. A change to either hash is a change to what a stored rendering or a stored
// token holds, so the version a reader compares with (RendererVersion) must move with it.
type rendererVersionGolden struct {
	RendererVersion  int    `json:"renderer_version"`
	RenderingsSHA256 string `json:"renderings_sha256"`
	TokensSHA256     string `json:"tokens_sha256"`
}

// corpusRenderingAndTokenHashes is the hash of the corpus's renderings and the hash of its
// document tokens, each over every document in the order filepath.Glob lists them, each entry
// framed by its name and its length so no two corpora hash alike.
//
// A typed block a document does not name an id for is given one as it is parsed, and a minted id
// is a random UUID that reaches both the rendering and the token, so the hashes stand only while
// the ids do: the walk mints them in document order instead (SetBlockIDGenerator, which is there
// for this), and puts the random mint back when it is done. What is left of the hash is what the
// renderer writes, which is what RendererVersion numbers.
func corpusRenderingAndTokenHashes(t *testing.T) (renderings, tokens string) {
	t.Helper()
	minted := 0
	SetBlockIDGenerator(func() string {
		minted++
		return fmt.Sprintf("00000000-0000-4000-8000-%012d", minted)
	})
	defer SetBlockIDGenerator(nil)
	renderingSum, tokenSum := sha256.New(), sha256.New()
	frame := func(sum hash.Hash, name string, payload []byte) {
		t.Helper()
		if _, err := fmt.Fprintf(sum, "%s %d\n", name, len(payload)); err != nil {
			t.Fatalf("hash %s: %v", name, err)
		}
		if _, err := sum.Write(payload); err != nil {
			t.Fatalf("hash %s: %v", name, err)
		}
	}
	for _, document := range corpusMarkdown(t) {
		tree, err := Parse(document.markdown)
		if err != nil {
			t.Fatalf("parse %s: %v", document.name, err)
		}
		rendered, err := Render(tree)
		if err != nil {
			t.Fatalf("render %s: %v", document.name, err)
		}
		frame(renderingSum, document.name, []byte(rendered))
		token, err := tree.TokenJSON()
		if err != nil {
			t.Fatalf("token of %s: %v", document.name, err)
		}
		frame(tokenSum, document.name, token)
	}
	return hex.EncodeToString(renderingSum.Sum(nil)), hex.EncodeToString(tokenSum.Sum(nil))
}

// A build whose renderings or document tokens of the corpus differ from the golden's stands at a
// renderer version of its own: a reader serves a stored rendering, and the token stored with it,
// only while the version that stored them was written at RendererVersion, so a change to either
// that left the number alone would serve text this build no longer writes.
func TestRendererVersionGoldenHoldsTheCorpus(t *testing.T) {
	path := filepath.Join("testdata", "renderer-version.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the renderer version golden: %v", err)
	}
	var golden rendererVersionGolden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if golden.RendererVersion != RendererVersion {
		t.Fatalf("%s holds renderer version %d, want RendererVersion %d", path, golden.RendererVersion, RendererVersion)
	}
	renderings, tokens := corpusRenderingAndTokenHashes(t)
	if renderings != golden.RenderingsSHA256 {
		t.Errorf("the corpus renders to %s, and %s holds %s: raise RendererVersion past %d and write the new hash",
			renderings, path, golden.RenderingsSHA256, RendererVersion)
	}
	if tokens != golden.TokensSHA256 {
		t.Errorf("the corpus's document tokens hash to %s, and %s holds %s: raise RendererVersion past %d and write the new hash",
			tokens, path, golden.TokensSHA256, RendererVersion)
	}
}
