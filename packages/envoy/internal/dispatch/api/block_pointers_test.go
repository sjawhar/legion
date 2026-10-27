package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// useBlockPointerSchema makes the server's block schema the shipped one (its callout included)
// with two test types: a requirement, and a claim whose serves attribute points only at
// requirements. A test calls it before it builds its server, and does not run in parallel
// (pmdoc.UseBlockSchemaForTest).
func useBlockPointerSchema(t *testing.T) {
	t.Helper()
	var schema pmdoc.BlockSchema
	if err := json.Unmarshal(pmdoc.SchemaJSON(), &schema); err != nil {
		t.Fatalf("decode the shipped block schema: %v", err)
	}
	schema.Types = append(schema.Types,
		pmdoc.BlockTypeSchema{Name: "requirement", Content: pmdoc.BlockContentParagraphs, Render: "host", Attributes: map[string]pmdoc.BlockAttributeSchema{}},
		pmdoc.BlockTypeSchema{Name: "claim", Content: pmdoc.BlockContentParagraphs, Render: "host", Attributes: map[string]pmdoc.BlockAttributeSchema{
			"serves": {Kind: "block[]", Targets: []string{"requirement"}},
		}},
	)
	pmdoc.UseBlockSchemaForTest(t, schema)
}

// pointerSpec is an issue whose spec holds a requirement r1, a callout k1 and a paragraph, and
// the paragraph's block id, read from GET …/blocks.
func pointerSpec(t *testing.T, handler http.Handler) (model.Issue, string) {
	t.Helper()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", ":::requirement{#r1}\nA requirement.\n:::\n\n"+
		":::callout{#k1 kind=\"note\" title=\"\"}\nA callout.\n:::\n\nA paragraph.\n")
	for text, id := range blockIDsByText(t, handler, issue.PrimaryArtifactID) {
		if strings.TrimSpace(text) == "A paragraph." {
			return issue, id
		}
	}
	t.Fatal("GET …/blocks lists no block for the spec's own paragraph")
	return model.Issue{}, ""
}

func claimMarkdown(id, serves string) string {
	return ":::claim{#" + id + " serves=\"" + serves + "\"}\nIt holds.\n:::\n"
}

func uploadChecks(t *testing.T, handler http.Handler, issueKey, content string) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/artifacts", map[string]any{
		"name": "checks.md", "content": content,
	}, "alice")
}

// requireRefused fails unless response is a 400 of code whose message names each of names.
func requireRefused(t *testing.T, what string, response *httptest.ResponseRecorder, code string, names ...string) {
	t.Helper()
	body := decodeBody[struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}](t, response)
	if response.Code != http.StatusBadRequest || body.Code != code {
		t.Fatalf("%s: status=%d body=%s, want 400 %s", what, response.Code, response.Body.String(), code)
	}
	for _, name := range names {
		if !strings.Contains(body.Error, name) {
			t.Fatalf("%s: %q, want it to name %s", what, body.Error, name)
		}
	}
}

// checksState is what a refused write must leave of checks.md: its artifact (absent: ""), its
// versions, and the block links it writes.
type checksState struct {
	ID       string
	Versions int
	Links    []blockRefRow
}

func readChecks(t *testing.T, database *store.Store, issueKey string) checksState {
	t.Helper()
	var state checksState
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce((select id::text from artifacts where issue_key = $1 and slug = 'checks-md'), '')
	`, issueKey).Scan(&state.ID); err != nil {
		t.Fatalf("read checks.md: %v", err)
	}
	if state.ID == "" {
		return state
	}
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id = $1`, state.ID).Scan(&state.Versions); err != nil {
		t.Fatalf("count the versions of checks.md: %v", err)
	}
	state.Links = readBlockRefs(t, database, state.ID)
	return state
}

// An agent's or API write introducing a pointer to a block of a type the attribute does not allow
// is refused, naming the attribute, the target's type and the allowed type, and writes nothing,
// whether it creates the document, replaces it or edits it. A target the same write adds is read
// as the write leaves it, and a target no document holds is accepted.
func TestAWriteRefusesABlockPointerToADisallowedType(t *testing.T) {
	useBlockPointerSchema(t)
	handler, database := blockIndexHandler(t)
	issue, paragraph := pointerSpec(t, handler)
	spec := "dispatch://" + issue.Key + "/spec"
	checks := "dispatch://" + issue.Key + "/artifact/checks-md"

	requireRefused(t, "a new document pointing at a paragraph", uploadChecks(t, handler, issue.Key, claimMarkdown("c1", spec+"#b-"+paragraph)),
		"INVALID_MARKDOWN", `"serves"`, spec+"#b-"+paragraph, `"paragraph"`, "requirement")
	requireRefused(t, "a new document pointing at a callout", uploadChecks(t, handler, issue.Key, claimMarkdown("c1", spec+"#b-k1")),
		"INVALID_MARKDOWN", `"serves"`, spec+"#b-k1", `"callout"`, "requirement")
	if state := readChecks(t, database, issue.Key); state.ID != "" {
		t.Fatalf("refused uploads left checks.md %+v, want no document", state)
	}

	if response := uploadChecks(t, handler, issue.Key, claimMarkdown("c1", spec+"#b-nowhere")); response.Code != http.StatusCreated {
		t.Fatalf("a pointer to a block no document holds: status=%d body=%s, want 201", response.Code, response.Body.String())
	}
	saved := readChecks(t, database, issue.Key)
	if saved.ID == "" || saved.Versions == 0 {
		t.Fatalf("checks.md after the accepted upload = %+v, want a version", saved)
	}

	requireRefused(t, "a replacement pointing at a paragraph", uploadChecks(t, handler, issue.Key, claimMarkdown("c1", spec+"#b-"+paragraph)),
		"INVALID_MARKDOWN", `"serves"`, `"paragraph"`, "requirement")
	inserted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+saved.ID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": claimMarkdown("c2", spec+"#b-k1")}},
	}, "alice")
	requireRefused(t, "an inserted claim pointing at a callout", inserted, "INVALID_OP", `field "attributes"`, `"serves"`, `"callout"`, "requirement")
	if after := readChecks(t, database, issue.Key); !reflect.DeepEqual(after, saved) {
		t.Fatalf("checks.md after refused writes = %+v, want %+v unchanged", after, saved)
	}

	// Same-document targets are the blocks the write leaves, not the ones stored before it.
	if response := uploadChecks(t, handler, issue.Key, ":::requirement{#r2}\nOurs.\n:::\n\n"+claimMarkdown("c1", checks+"#b-r2")); response.Code != http.StatusCreated {
		t.Fatalf("a pointer to a requirement the same upload adds: status=%d body=%s, want 201", response.Code, response.Body.String())
	}
	saved = readChecks(t, database, issue.Key)
	requireRefused(t, "a pointer to a callout the same upload adds",
		uploadChecks(t, handler, issue.Key, ":::callout{#k2 kind=\"note\" title=\"\"}\nOurs.\n:::\n\n"+claimMarkdown("c1", checks+"#b-k2")),
		"INVALID_MARKDOWN", `"serves"`, checks+"#b-k2", `"callout"`, "requirement")
	requireRefused(t, "a pointer to a requirement the same upload makes a callout",
		uploadChecks(t, handler, issue.Key, ":::callout{#r2 kind=\"note\" title=\"\"}\nOurs.\n:::\n\n"+claimMarkdown("c3", checks+"#b-r2")),
		"INVALID_MARKDOWN", `"serves"`, checks+"#b-r2", `"callout"`, "requirement")
	if after := readChecks(t, database, issue.Key); !reflect.DeepEqual(after, saved) {
		t.Fatalf("checks.md after refused same-document writes = %+v, want %+v unchanged", after, saved)
	}
}

// A pointer a write leaves as it was is never refused, so retyping its target elsewhere to a type
// the attribute does not allow blocks neither the retype nor an unrelated edit of the citing
// document, while a new pointer to the retyped target is refused.
func TestAnUnchangedPointerIsNotRefusedWhenItsTargetIsRetyped(t *testing.T) {
	useBlockPointerSchema(t)
	handler, _ := blockIndexHandler(t)
	issue, _ := pointerSpec(t, handler)
	r1 := "dispatch://" + issue.Key + "/spec#b-r1"
	if response := uploadChecks(t, handler, issue.Key, claimMarkdown("c1", r1)); response.Code != http.StatusCreated {
		t.Fatalf("a pointer to a requirement: status=%d body=%s, want 201", response.Code, response.Body.String())
	}

	editSpec(t, handler, issue.Key, []map[string]any{{"op": "retype", "block": "r1", "type": "callout"}})
	editIssueDocument(t, handler, issue.Key, "checks-md", []map[string]any{{"op": "insert", "after": "end", "markdown": "More.\n"}})
	if response := uploadChecks(t, handler, issue.Key, claimMarkdown("c1", r1)+"\nReworded.\n"); response.Code != http.StatusCreated {
		t.Fatalf("a replacement keeping the pointer: status=%d body=%s, want 201", response.Code, response.Body.String())
	}

	// The control: r1 is a callout now, so a pointer to it a write introduces is refused.
	inserted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts/checks-md/edits", map[string]any{
		"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": claimMarkdown("c2", r1)}},
	}, "alice")
	requireRefused(t, "a new pointer to the retyped target", inserted, "INVALID_OP", `"serves"`, `"callout"`, "requirement")
}
