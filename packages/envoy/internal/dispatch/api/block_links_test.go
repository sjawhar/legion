package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func blockLinksRequest(t *testing.T, handler http.Handler, query url.Values) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodGet, "/api/v1/block-links?"+query.Encode(), nil, "alice")
}

// readBlockLinks is GET /api/v1/block-links from the document from to the document to.
func readBlockLinks(t *testing.T, handler http.Handler, from, to string) model.BlockLinks {
	t.Helper()
	response := blockLinksRequest(t, handler, url.Values{"from": {from}, "to": {to}})
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/block-links from %s to %s: status=%d body=%s", from, to, response.Code, response.Body.String())
	}
	return decodeBody[model.BlockLinks](t, response)
}

// uploadIssueDocument uploads markdown as the issue document name and returns it.
func uploadIssueDocument(t *testing.T, handler http.Handler, key, name, markdown string) model.Artifact {
	t.Helper()
	response := multipartRequest(t, handler, "/api/v1/issues/"+key+"/artifacts", map[string]string{"name": name}, name, "text/markdown", []byte(markdown), "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload %s: status=%d body=%s", name, response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, response).Artifact
}

// blockLink is a link as a test compares it: its kind, both blocks, and when and in which version
// its source block first held it.
type blockLink struct {
	Kind                   string
	FromKind, ToKind       string
	FromDocument, FromID   string
	ToDocument, ToID       string
	FromBlockID, ToBlockID string
	CreatedVersion         int
	CreatedAt              time.Time
}

// compareBlockLinks fails unless got is want, in order.
func compareBlockLinks(t *testing.T, stage string, got []model.BlockLink, want []blockLink) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: links = %#v, want %d", stage, got, len(want))
	}
	for index, link := range got {
		gotLink := blockLink{
			Kind:     link.Kind,
			FromKind: link.From.Kind, ToKind: link.To.Kind,
			FromDocument: link.From.Document, FromID: link.From.ID,
			ToDocument: link.To.Document, ToID: link.To.ID,
			FromBlockID: link.From.BlockID, ToBlockID: link.To.BlockID,
			CreatedVersion: link.CreatedVersion,
			CreatedAt:      link.CreatedAt.UTC(),
		}
		wanted := want[index]
		wanted.CreatedAt = wanted.CreatedAt.UTC()
		if gotLink != wanted {
			t.Fatalf("%s: link %d = %#v, want %#v", stage, index, gotLink, wanted)
		}
	}
}

// A4: decisions.md has two blocks citing blocks of the spec, written in two versions, and the
// spec has one block citing decisions.md's first block. The links from decisions.md to the spec
// are exactly its two, newest first, each dated by the version that wrote it; the links back are
// exactly the spec's one.
func TestBlockLinksListLinksFromBlocksOfOneDocumentToBlocksOfAnother(t *testing.T) {
	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	holdIt := ":::callout{#hold-it kind=\"note\" title=\"\"}\nHold it.\n:::"
	issue := createSpecIssue(t, handler, "CORE", shipIt("Ship it.")+"\n\n"+holdIt+"\n\nOther paragraph.\n")
	specRef := "dispatch://" + issue.Key + "/spec"
	specKey := issue.Key + "/spec"

	shipParagraph := "Ship per " + specRef + "#b-ship%5Fit."
	decisions := uploadIssueDocument(t, handler, issue.Key, "decisions.md",
		":::callout{#first_call kind=\"note\" title=\"\"}\nDecided.\n:::\n\n"+shipParagraph+"\n")
	decisionsRef := "dispatch://" + issue.Key + "/artifact/" + decisions.Slug
	decisionsKey := decisions.RefKey
	decisionsV1 := latestVersion(t, database, decisions.ID)
	holdParagraph := "Hold per " + specRef + "#b-hold-it."
	editIssueDocument(t, handler, issue.Key, decisions.Slug, []map[string]any{{"op": "insert", "after": "end", "markdown": holdParagraph}})
	decisionsV2 := latestVersion(t, database, decisions.ID)
	if decisionsV1.Number != 1 || decisionsV2.Number != 2 {
		t.Fatalf("decisions.md versions = %d, %d, want 1, 2", decisionsV1.Number, decisionsV2.Number)
	}

	citeParagraph := "Spec cites " + decisionsRef + "#b-first%5Fcall."
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "insert", "markdown": citeParagraph, "before": "Other paragraph."}})
	specV2 := latestVersion(t, database, issue.PrimaryArtifactID)

	decisionsBlocks := blockIDsByText(t, handler, decisions.ID)
	shipFrom, holdFrom := decisionsBlocks[shipParagraph], decisionsBlocks[holdParagraph]
	citeFrom := blockIDsByText(t, handler, issue.PrimaryArtifactID)[citeParagraph]
	if shipFrom == "" || holdFrom == "" || citeFrom == "" {
		t.Fatalf("citing paragraphs' ids = %q, %q, %q, want each listed by GET …/blocks", shipFrom, holdFrom, citeFrom)
	}

	forward := readBlockLinks(t, handler, decisionsRef, specRef)
	if forward.From.Kind != "artifact" || forward.From.ID != decisions.ID || forward.From.Ref != decisionsRef ||
		forward.To.Kind != "artifact" || forward.To.ID != issue.PrimaryArtifactID || forward.To.Ref != specRef {
		t.Fatalf("documents = %#v, %#v, want decisions.md then the spec", forward.From, forward.To)
	}
	compareBlockLinks(t, "decisions.md to the spec", forward.Links, []blockLink{
		{
			Kind:         "mentions",
			FromDocument: decisionsRef, FromID: decisionsKey + "#" + holdFrom, FromKind: "block", FromBlockID: holdFrom,
			ToDocument: specRef, ToID: specKey + "#hold-it", ToKind: "block", ToBlockID: "hold-it",
			CreatedVersion: 2, CreatedAt: decisionsV2.CreatedAt,
		},
		{
			Kind:         "mentions",
			FromDocument: decisionsRef, FromID: decisionsKey + "#" + shipFrom, FromKind: "block", FromBlockID: shipFrom,
			ToDocument: specRef, ToID: specKey + "#ship_it", ToKind: "block", ToBlockID: "ship_it",
			CreatedVersion: 1, CreatedAt: decisionsV1.CreatedAt,
		},
	})

	backward := readBlockLinks(t, handler, specRef, decisionsRef)
	if backward.From.ID != issue.PrimaryArtifactID || backward.To.ID != decisions.ID {
		t.Fatalf("documents = %#v, %#v, want the spec then decisions.md", backward.From, backward.To)
	}
	compareBlockLinks(t, "the spec to decisions.md", backward.Links, []blockLink{{
		Kind:         "mentions",
		FromDocument: specRef, FromID: specKey + "#" + citeFrom, FromKind: "block", FromBlockID: citeFrom,
		ToDocument: decisionsRef, ToID: decisionsKey + "#first_call", ToKind: "block", ToBlockID: "first_call",
		CreatedVersion: 2, CreatedAt: specV2.CreatedAt,
	}})
}

// A document's blocks can link to one another: the links from a document to itself are those.
// A document with none answers an empty list, never null.
func TestBlockLinksListALinkWithinOneDocument(t *testing.T) {
	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue, blockRef := createShipItSpec(t, handler, "CORE")
	specRef := "dispatch://" + issue.Key + "/spec"

	response := blockLinksRequest(t, handler, url.Values{"from": {specRef}, "to": {specRef}})
	if response.Code != http.StatusOK {
		t.Fatalf("block links before any: status=%d body=%s", response.Code, response.Body.String())
	}
	if links := decodeBody[struct {
		Links json.RawMessage `json:"links"`
	}](t, response).Links; string(links) != "[]" {
		t.Fatalf("block links before any = %s, want []", links)
	}

	paragraph := "See " + blockRef + "."
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "insert", "markdown": paragraph, "before": "Other paragraph."}})
	version := latestVersion(t, database, issue.PrimaryArtifactID)
	paragraphID := blockIDsByText(t, handler, issue.PrimaryArtifactID)[paragraph]
	if paragraphID == "" {
		t.Fatalf("the inserted paragraph %q is not among GET …/blocks", paragraph)
	}
	compareBlockLinks(t, "the spec to itself", readBlockLinks(t, handler, specRef, specRef).Links, []blockLink{{
		Kind:         "mentions",
		FromDocument: specRef, FromID: issue.Key + "/spec#" + paragraphID, FromKind: "block", FromBlockID: paragraphID,
		ToDocument: specRef, ToID: issue.Key + "/spec#ship_it", ToKind: "block", ToBlockID: "ship_it",
		CreatedVersion: version.Number, CreatedAt: version.CreatedAt,
	}})
}

// Each end must name a document: a block, an issue or no reference at all is 400, and a document
// that does not exist is 404.
func TestBlockLinksRefuseAnEndThatIsNotADocument(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue, blockRef := createShipItSpec(t, handler, "CORE")
	specRef := "dispatch://" + issue.Key + "/spec"
	missing := "dispatch://" + issue.Key + "/artifact/missing-md"
	documentsOnly := " must name a document: dispatch://KEY/spec, dispatch://KEY/artifact/<slug> or dispatch://PROJECT/artifact/<slug>"

	for _, row := range []struct {
		query         url.Values
		status        int
		code, message string
	}{
		{url.Values{"from": {blockRef}, "to": {specRef}}, http.StatusBadRequest, "INVALID_REFERENCE", "from" + documentsOnly},
		{url.Values{"from": {specRef}, "to": {"dispatch://" + issue.Key}}, http.StatusBadRequest, "INVALID_REFERENCE", "to" + documentsOnly},
		{url.Values{"from": {specRef}}, http.StatusBadRequest, "INVALID_REFERENCE", "to" + documentsOnly},
		{url.Values{"from": {missing}, "to": {specRef}}, http.StatusNotFound, "NOT_FOUND", "no document " + missing},
		{url.Values{"from": {specRef}, "to": {missing}}, http.StatusNotFound, "NOT_FOUND", "no document " + missing},
	} {
		response := blockLinksRequest(t, handler, row.query)
		if response.Code != row.status {
			t.Fatalf("GET /api/v1/block-links?%s: status=%d body=%s, want %d", row.query.Encode(), response.Code, response.Body.String(), row.status)
		}
		if body := decodeBody[apiErrorBody](t, response); body.Code != row.code || body.Error != row.message {
			t.Fatalf("GET /api/v1/block-links?%s = %#v, want %s %q", row.query.Encode(), body, row.code, row.message)
		}
	}
}
