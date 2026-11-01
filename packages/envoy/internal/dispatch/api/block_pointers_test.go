package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/pmdoctest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

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
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(nil))
	handler, database := blockIndexHandler(t)
	issue, paragraph := pointerSpec(t, handler)
	spec := "dispatch://" + issue.Key + "/spec"
	checks := "dispatch://" + issue.Key + "/artifact/checks-md"

	expectRefusal(t, "a new document pointing at a paragraph", uploadChecks(t, handler, issue.Key, claimMarkdown("c1", spec+"#b-"+paragraph)), http.StatusBadRequest,
		"INVALID_MARKDOWN", `"serves"`, spec+"#b-"+paragraph, `"paragraph"`, "requirement")
	expectRefusal(t, "a new document pointing at a callout", uploadChecks(t, handler, issue.Key, claimMarkdown("c1", spec+"#b-k1")), http.StatusBadRequest,
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

	expectRefusal(t, "a replacement pointing at a paragraph", uploadChecks(t, handler, issue.Key, claimMarkdown("c1", spec+"#b-"+paragraph)), http.StatusBadRequest,
		"INVALID_MARKDOWN", `"serves"`, `"paragraph"`, "requirement")
	inserted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+saved.ID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": claimMarkdown("c2", spec+"#b-k1")}},
	}, "alice")
	expectRefusal(t, "an inserted claim pointing at a callout", inserted, http.StatusBadRequest, "INVALID_OP", `field "attributes"`, `"serves"`, `"callout"`, "requirement")
	if after := readChecks(t, database, issue.Key); !reflect.DeepEqual(after, saved) {
		t.Fatalf("checks.md after refused writes = %+v, want %+v unchanged", after, saved)
	}

	// Same-document targets are the blocks the write leaves, not the ones stored before it.
	if response := uploadChecks(t, handler, issue.Key, ":::requirement{#r2}\nOurs.\n:::\n\n"+claimMarkdown("c1", checks+"#b-r2")); response.Code != http.StatusCreated {
		t.Fatalf("a pointer to a requirement the same upload adds: status=%d body=%s, want 201", response.Code, response.Body.String())
	}
	saved = readChecks(t, database, issue.Key)
	expectRefusal(t, "a pointer to a callout the same upload adds",
		uploadChecks(t, handler, issue.Key, ":::callout{#k2 kind=\"note\" title=\"\"}\nOurs.\n:::\n\n"+claimMarkdown("c1", checks+"#b-k2")),
		http.StatusBadRequest, "INVALID_MARKDOWN", `"serves"`, checks+"#b-k2", `"callout"`, "requirement")
	expectRefusal(t, "a pointer to a requirement the same upload makes a callout",
		uploadChecks(t, handler, issue.Key, ":::callout{#r2 kind=\"note\" title=\"\"}\nOurs.\n:::\n\n"+claimMarkdown("c3", checks+"#b-r2")),
		http.StatusBadRequest, "INVALID_MARKDOWN", `"serves"`, checks+"#b-r2", `"callout"`, "requirement")
	if after := readChecks(t, database, issue.Key); !reflect.DeepEqual(after, saved) {
		t.Fatalf("checks.md after refused same-document writes = %+v, want %+v unchanged", after, saved)
	}
}

// The document an upload creates has no artifacts row to read until the upload commits, and a
// pointer at a block of that same document is still resolved from the blocks the upload writes:
// a claim pointing at a callout the new document adds is refused and nothing is written, while
// one pointing at a requirement it adds is created.
func TestANewDocumentIsRefusedAPointerToItsOwnDisallowedBlock(t *testing.T) {
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(nil))
	handler, database := blockIndexHandler(t)
	issue, _ := pointerSpec(t, handler)
	checks := "dispatch://" + issue.Key + "/artifact/checks-md"

	expectRefusal(t, "a new document pointing at a callout of its own",
		uploadChecks(t, handler, issue.Key, ":::callout{#k2 kind=\"note\" title=\"\"}\nOurs.\n:::\n\n"+claimMarkdown("c1", checks+"#b-k2")),
		http.StatusBadRequest, "INVALID_MARKDOWN", `"serves"`, checks+"#b-k2", `"callout"`, "requirement")
	if state := readChecks(t, database, issue.Key); state.ID != "" {
		t.Fatalf("the refused upload left checks.md %+v, want no document", state)
	}
	if response := uploadChecks(t, handler, issue.Key, ":::requirement{#r2}\nOurs.\n:::\n\n"+claimMarkdown("c1", checks+"#b-r2")); response.Code != http.StatusCreated {
		t.Fatalf("a pointer to a requirement the same new document adds: status=%d body=%s, want 201", response.Code, response.Body.String())
	}
}

// A pointer a write leaves as it was is never refused, so retyping its target elsewhere to a type
// the attribute does not allow blocks neither the retype nor an unrelated edit of the citing
// document, while a new pointer to the retyped target is refused.
func TestAnUnchangedPointerIsNotRefusedWhenItsTargetIsRetyped(t *testing.T) {
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(nil))
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
	expectRefusal(t, "a new pointer to the retyped target", inserted, http.StatusBadRequest, "INVALID_OP", `"serves"`, `"callout"`, "requirement")
}

// A rebuild from supplied markdown writes that markdown, so a pointer it introduces to a block of
// a type the attribute does not allow is refused before anything is written, and one to an
// allowed block is rebuilt.
func TestARebuildFromMarkdownRefusesABlockPointerToADisallowedType(t *testing.T) {
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(nil))
	handler, database, broken := newBrokenTestServer(t)
	issue, _ := pointerSpec(t, handler)
	if response := uploadChecks(t, handler, issue.Key, "Before.\n"); response.Code != http.StatusCreated {
		t.Fatalf("upload checks.md: status=%d body=%s, want 201", response.Code, response.Body.String())
	}
	saved := readChecks(t, database, issue.Key)
	rebuild := func(markdown string) *httptest.ResponseRecorder {
		broken.failNextLoad(saved.ID)
		return dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+saved.ID+"/rebuild", map[string]any{"markdown": markdown}, "alice")
	}

	k1 := "dispatch://" + issue.Key + "/spec#b-k1"
	expectRefusal(t, "a rebuild pointing at a callout", rebuild(claimMarkdown("c1", k1)), http.StatusBadRequest, "INVALID_MARKDOWN", `"serves"`, k1, `"callout"`, "requirement")
	if after := readChecks(t, database, issue.Key); !reflect.DeepEqual(after, saved) {
		t.Fatalf("checks.md after a refused rebuild = %+v, want %+v unchanged", after, saved)
	}

	// The control: the same rebuild pointing at the requirement is written.
	if rebuilt := rebuild(claimMarkdown("c1", "dispatch://"+issue.Key+"/spec#b-r1")); rebuilt.Code != http.StatusOK {
		t.Fatalf("a rebuild pointing at a requirement: status=%d body=%s, want 200", rebuilt.Code, rebuilt.Body.String())
	}
}
