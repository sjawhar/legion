package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// randomBlockID is a block id of length incompressible random hex characters, so no Postgres
// index can hold one past its key limit by compressing it.
func randomBlockID(t *testing.T, length int) string {
	t.Helper()
	raw := make([]byte, (length+1)/2)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw)[:length]
}

// boundCallout is a callout under id, as markdown writes one.
func boundCallout(id string) string {
	return ":::callout{#" + id + " kind=\"note\" title=\"\"}\nA note.\n:::\n"
}

// A write through the API whose markdown names a block id past text.MaxBlockIDBytes - a spec
// seeded at issue creation, an uploaded version, an inserted block, an accepted suggestion - is
// refused with 400 naming the bound in bytes, and the document is unchanged. An id at the bound is
// written, and its block is a node the graph reads.
func TestAWriteNamingABlockIDPastTheBoundIsRefused(t *testing.T) {
	long := randomBlockID(t, 3200)
	bound := fmt.Sprintf("past the %d bytes a block id may hold", text.MaxBlockIDBytes)
	for _, test := range []struct {
		name, code string
		write      func(t *testing.T, handler http.Handler, issueKey, artifactID string) *httptest.ResponseRecorder
	}{
		{name: "a seeded spec", code: "INVALID_MARKDOWN",
			write: func(t *testing.T, handler http.Handler, _, _ string) *httptest.ResponseRecorder {
				return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
					"project": "TEST", "title": "Seeded callout", "spec": boundCallout(long),
				}, "alice")
			}},
		{name: "an uploaded version", code: "INVALID_MARKDOWN",
			write: func(t *testing.T, handler http.Handler, issueKey, artifactID string) *httptest.ResponseRecorder {
				return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/artifacts", map[string]any{
					"name": "spec.md", "content": documentMarkdown(t, handler, artifactID) + "\n" + boundCallout(long),
				}, "alice")
			}},
		{name: "an inserted callout", code: "INVALID_OP",
			write: func(t *testing.T, handler http.Handler, _, artifactID string) *httptest.ResponseRecorder {
				return dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
					"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": boundCallout(long)}},
				}, "alice")
			}},
		{name: "an accepted suggestion", code: "INVALID_MARKDOWN",
			write: func(t *testing.T, handler http.Handler, issueKey, _ string) *httptest.ResponseRecorder {
				created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments", map[string]any{
					"body": "a note first", "anchor": map[string]any{"artifact": "spec", "quote": "Context"},
					"suggestion": map[string]string{"replace_with": boundCallout(long)}, "actor": sessionActor(),
				})
				if created.Code != http.StatusCreated {
					t.Fatalf("create the suggestion: status=%d body=%s", created.Code, created.Body.String())
				}
				comment := decodeBody[model.Comment](t, created)
				return dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, handler, _ := browserDocumentService(t)
			issue := createInteractionIssue(t, handler, "TEST", "Long block id", "Context paragraph.\n")
			before := documentMarkdown(t, handler, issue.PrimaryArtifactID)

			written := test.write(t, handler, issue.Key, issue.PrimaryArtifactID)

			if written.Code != http.StatusBadRequest || responseCode(t, written) != test.code || !strings.Contains(written.Body.String(), bound) {
				t.Fatalf("%s naming a block id of 3200 bytes: status=%d body=%.400s; want 400 %s saying %q", test.name, written.Code, written.Body.String(), test.code, bound)
			}
			if after := documentMarkdown(t, handler, issue.PrimaryArtifactID); after != before {
				t.Fatalf("document after the refusal = %.400q, want it unchanged: %q", after, before)
			}
		})
	}

	t.Run("an id at the bound", func(t *testing.T) {
		_, handler, _ := browserDocumentService(t)
		issue := createInteractionIssue(t, handler, "TEST", "Block id at the bound", "Context paragraph.\n")
		atBound := randomBlockID(t, text.MaxBlockIDBytes)
		if edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
			"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": boundCallout(atBound)}},
		}, "alice"); edited.Code != http.StatusOK {
			t.Fatalf("insert a callout under an id of %d bytes: status=%d body=%s", text.MaxBlockIDBytes, edited.Code, edited.Body.String())
		}
		if read := graphRequest(t, handler, url.Values{"to": {"dispatch://" + issue.Key + "/spec#b-" + atBound}}); read.Code != http.StatusOK {
			t.Fatalf("read the block at the bound: status=%d body=%s, want its node", read.Code, read.Body.String())
		}
	})
}

// A document already holding a block id past the bound - a browser's edit wrote it, which no
// route can refuse - takes the writes that leave that id where it is: an inserted block and an
// uploaded version that keeps it are written, each storing its version, and the id stays in the
// document. The block is no graph node, so a reference naming it is malformed.
func TestADocumentHoldingABlockIDPastTheBoundTakesUnrelatedWrites(t *testing.T) {
	documentService, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Held long block id", "Context paragraph.\n")
	long := randomBlockID(t, 3200)
	writeBrowserDocument(t, documentService, issue.PrimaryArtifactID, boundCallout(long)+"\nContext paragraph.\n")
	versions := func() int {
		var count int
		if err := database.Pool.QueryRow(context.Background(),
			`select count(*) from artifact_versions where artifact_id = $1`, issue.PrimaryArtifactID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	before := versions()

	if edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": "An agent's line."}},
	}, "alice"); edited.Code != http.StatusOK {
		t.Fatalf("insert into the document holding a long block id: status=%d body=%.400s", edited.Code, edited.Body.String())
	}
	if uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"name": "spec.md", "content": documentMarkdown(t, handler, issue.PrimaryArtifactID) + "\nAn uploaded line.\n",
	}, "alice"); uploaded.Code != http.StatusCreated {
		t.Fatalf("upload a version keeping the long block id: status=%d body=%.400s", uploaded.Code, uploaded.Body.String())
	}

	if after := versions(); after != before+2 {
		t.Fatalf("the document has %d versions after the two writes, want %d", after, before+2)
	}
	if markdown := documentMarkdown(t, handler, issue.PrimaryArtifactID); !strings.Contains(markdown, "{#"+long+" ") ||
		!strings.Contains(markdown, "An agent's line.") || !strings.Contains(markdown, "An uploaded line.") {
		t.Fatalf("document = %.400q, want the long id kept beside both writes", markdown)
	}
	if read := graphRequest(t, handler, url.Values{"to": {"dispatch://" + issue.Key + "/spec#b-" + long}}); read.Code != http.StatusBadRequest || responseCode(t, read) != "INVALID_REFERENCE" {
		t.Fatalf("read the long-id block: status=%d body=%.400s, want 400 INVALID_REFERENCE", read.Code, read.Body.String())
	}
}

// A comment citing a block by an id past the bound is posted: the citation names no block, so it
// writes no edge whose key the index could not hold.
func TestACommentCitingABlockIDPastTheBoundPosts(t *testing.T) {
	_, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Cited long block id", "Context paragraph.\n")
	cited := "dispatch://" + issue.Key + "/spec#b-" + randomBlockID(t, 3200)

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "See " + cited + " and dispatch://" + issue.Key + ".",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("post a comment citing a long block id: status=%d body=%.400s", created.Code, created.Body.String())
	}
	comment := decodeBody[model.Comment](t, created)
	var kinds []string
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(array_agg(to_kind || ':' || to_id order by to_kind), '{}') from refs where from_kind = 'comment' and from_id = $1
	`, comment.ID).Scan(&kinds); err != nil {
		t.Fatal(err)
	}
	if len(kinds) != 1 || kinds[0] != "issue:"+issue.Key {
		t.Fatalf("the comment's references = %q, want only its issue", kinds)
	}
}

// A document whose own reference key is long - a name of 2680 incompressible characters, which
// main stores, since its ref_key still fits artifacts_ref_key_key - takes its upload and later
// edits as main does: each stores its version. A block node's id is `<ref_key>#<block id>`, so
// such a key leaves no room in document_blocks' node_id key even for a minted 36-byte paragraph
// id; the block index must not turn that into a refusal of the document's writes.
func TestADocumentWhoseReferenceKeyLeavesNoRoomForABlockIDIsStillWritten(t *testing.T) {
	_, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Long document name", "Context paragraph.\n")
	name := randomBlockID(t, 2680) + ".md"

	uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"name": name, "content": "A paragraph of the long-named document.\n",
	}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload a document named with 2680 characters: status=%d body=%.400s, want 201 as main answers", uploaded.Code, uploaded.Body.String())
	}
	artifact := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact
	if edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifact.ID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": "An agent's line."}},
	}, "alice"); edited.Code != http.StatusOK {
		t.Fatalf("edit the long-named document: status=%d body=%.400s", edited.Code, edited.Body.String())
	}
	var versions int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from artifact_versions where artifact_id = $1`, artifact.ID).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 2 {
		t.Fatalf("the long-named document has %d versions after its upload and one edit, want 2", versions)
	}
}

// Text that cites a block of a document whose reference key is long - the 2680-character name
// above - is written as main writes it: a comment posts and a document citing it uploads. Main
// reads no `#b-` reference, so neither write indexes an edge there; a block reference's target
// id is `<ref_key>#<block id>`, which the refs and block_refs keys cannot hold at this length, so
// the block index must not turn the citation into a refusal of the write.
func TestTextCitingABlockOfALongNamedDocumentIsStillWritten(t *testing.T) {
	_, handler, _ := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Cites a long-named document", "Context paragraph.\n")
	name := randomBlockID(t, 2680) + ".md"
	uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"name": name, "content": boundCallout("x"),
	}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload a document named with 2680 characters: status=%d body=%.400s", uploaded.Code, uploaded.Body.String())
	}
	cited := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact
	block := "dispatch://" + issue.Key + "/artifact/" + cited.Slug + "#b-x"

	if created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "See " + block + " and dispatch://" + issue.Key + ".",
	}, "alice"); created.Code != http.StatusCreated {
		t.Fatalf("post a comment citing a block of the long-named document: status=%d body=%.400s, want 201 as main answers", created.Code, created.Body.String())
	}
	if citer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"name": "citer.md", "content": "This document cites " + block + ".\n",
	}, "alice"); citer.Code != http.StatusCreated {
		t.Fatalf("upload a document citing a block of the long-named document: status=%d body=%.400s, want 201 as main answers", citer.Code, citer.Body.String())
	}
}

// A comment citing a document by a 3200-character slug, or a comment by a 3200-character id - each
// a target id past text.MaxTargetBytes, which main fails to write with a 500 because refs_pkey
// cannot hold it - is posted: the citation names nothing, so it writes no edge, and the comment's
// other, ordinary reference is recorded as ever.
func TestACommentCitingATargetPastTheBoundPosts(t *testing.T) {
	_, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Cites long targets", "Context paragraph.\n")
	for _, test := range []struct{ name, cited string }{
		{"a document's slug", "dispatch://" + issue.Key + "/artifact/" + randomBlockID(t, 3200)},
		{"a comment's id", "dispatch://" + issue.Key + "/comment/" + randomBlockID(t, 3200)},
	} {
		t.Run(test.name, func(t *testing.T) {
			created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
				"body": "See " + test.cited + " and dispatch://" + issue.Key + ".",
			}, "alice")
			if created.Code != http.StatusCreated {
				t.Fatalf("post a comment citing %s of 3200 characters: status=%d body=%.400s", test.name, created.Code, created.Body.String())
			}
			comment := decodeBody[model.Comment](t, created)
			var targets []string
			if err := database.Pool.QueryRow(context.Background(), `
				select coalesce(array_agg(to_kind || ':' || to_id order by to_kind), '{}') from refs where from_kind = 'comment' and from_id = $1
			`, comment.ID).Scan(&targets); err != nil {
				t.Fatal(err)
			}
			if len(targets) != 1 || targets[0] != "issue:"+issue.Key {
				t.Fatalf("the comment's references = %.200q, want only its issue", targets)
			}
		})
	}
}

// The API reads a reference by the grammar text is indexed by, so one whose target id is past
// text.MaxTargetBytes is 400 INVALID_REFERENCE wherever a route parses one - the graph's ?to= and
// ?from= and block-links' documents - while one at the bound is read: a comment no one wrote is
// 404 NOT_FOUND there.
func TestAReferencePastTheTargetBoundIsAnInvalidReference(t *testing.T) {
	_, handler, _ := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Reads long targets", "Context paragraph.\n")
	atBound := "dispatch://" + issue.Key + "/comment/" + randomBlockID(t, text.MaxTargetBytes)
	if read := graphRequest(t, handler, url.Values{"to": {atBound}}); read.Code != http.StatusNotFound || responseCode(t, read) != "NOT_FOUND" {
		t.Fatalf("?to= a comment id at the bound: status=%d body=%.400s, want 404 NOT_FOUND", read.Code, read.Body.String())
	}
	longDocument := "dispatch://" + issue.Key + "/artifact/" + randomBlockID(t, text.MaxTargetBytes-len(issue.Key+"/")+1)
	for _, reference := range []string{atBound + "a", longDocument, "dispatch://CORE/component/" + randomBlockID(t, text.MaxTargetBytes)} {
		for _, direction := range []string{"to", "from"} {
			if read := graphRequest(t, handler, url.Values{direction: {reference}}); read.Code != http.StatusBadRequest || responseCode(t, read) != "INVALID_REFERENCE" {
				t.Fatalf("?%s= %.60s… one byte past the bound: status=%d body=%.400s, want 400 INVALID_REFERENCE", direction, reference, read.Code, read.Body.String())
			}
		}
	}
	spec := "dispatch://" + issue.Key + "/spec"
	for _, query := range []url.Values{{"from": {longDocument}, "to": {spec}}, {"from": {spec}, "to": {longDocument}}} {
		if read := blockLinksRequest(t, handler, query); read.Code != http.StatusBadRequest || responseCode(t, read) != "INVALID_REFERENCE" {
			t.Fatalf("block-links %v naming a document past the bound: status=%d body=%.400s, want 400 INVALID_REFERENCE", query, read.Code, read.Body.String())
		}
	}
}

// A comment and an ask anchored by a quote inside a block whose id is 3200 random hex characters -
// which a browser's edit writes, and which no Postgres B-tree key holds - are written as any
// anchor is, each anchored to that block, and the document's blocks count both against it. No
// write fails on an index key past its limit (SQLSTATE 54000).
func TestAnAnchorInsideABlockWhoseIDIsPastTheBoundIsWritten(t *testing.T) {
	logs := &storetest.LockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	documentService, handler, _ := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Anchored in a long-id block", "Context paragraph.\n")
	long := randomBlockID(t, 3200)
	// A quote across the callout's two paragraphs lies in no block below the callout, so its
	// anchor's block is the callout and its long id.
	writeBrowserDocument(t, documentService, issue.PrimaryArtifactID,
		":::callout{#"+long+" kind=\"note\" title=\"\"}\nOne line.\n\nTwo line.\n:::\n\nContext paragraph.\n")

	for _, write := range []struct{ name, path, field, text, quote string }{
		{"comment", "comments", "body", "Why both lines?", "One line. Two line."},
		{"ask", "asks", "question", "Are both lines needed?", "line. Two"},
	} {
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/"+write.path, map[string]any{
			write.field: write.text, "anchor": map[string]string{"artifact": "spec", "quote": write.quote},
		}, "alice")
		if created.Code != http.StatusCreated {
			t.Errorf("anchor a %s inside the block of a 3200-character id: status=%d body=%.400s log=%.600s", write.name, created.Code, created.Body.String(), logs.Lines("SQLSTATE"))
			continue
		}
		if anchored := decodeBody[anchoredRecord](t, created); anchored.Anchor.BlockID != long {
			t.Errorf("the %s's anchor names block %.40q…, want the callout's long id", write.name, anchored.Anchor.BlockID)
		}
	}
	if t.Failed() {
		return
	}

	blocks := decodeBody[[]blockWithReferences](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	counted := false
	for _, block := range blocks {
		if block.ID == long {
			counted = block.References.Comments == 1 && block.References.Asks == 1
		}
	}
	if !counted {
		t.Fatalf("blocks = %.400v, want the long-id callout counting one comment and one ask", blocks)
	}
	if line := logs.Lines("54000"); line != "" {
		t.Fatalf("a write failed on an index key past its limit: %.400s", line)
	}
}
