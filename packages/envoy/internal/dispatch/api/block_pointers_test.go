package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
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
// as the write leaves it, and a target no document holds is refused too.
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

	expectRefusal(t, "a new document pointing at a block no document holds", uploadChecks(t, handler, issue.Key, claimMarkdown("c1", spec+"#b-nowhere")), http.StatusBadRequest,
		"INVALID_MARKDOWN", `"serves"`, spec+"#b-nowhere", "no document holds")
	if state := readChecks(t, database, issue.Key); state.ID != "" {
		t.Fatalf("the refused upload left checks.md %+v, want no document", state)
	}

	if response := uploadChecks(t, handler, issue.Key, claimMarkdown("c1", spec+"#b-r1")); response.Code != http.StatusCreated {
		t.Fatalf("a pointer to the spec's requirement: status=%d body=%s, want 201", response.Code, response.Body.String())
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

// A pointer names a block of the writing document's own issue or of an issue above it; one in a
// sibling tree is refused, as is a needs naming a plan task of another document, and a document
// its project holds points only inside itself. Every refusal names the reference and writes
// nothing.
func TestAWriteRefusesABlockPointerOutsideItsIssueOrDocument(t *testing.T) {
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(nil))
	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	requirement := ":::requirement{#r1}\nA requirement.\n:::\n"
	root := createSpecIssue(t, handler, "CORE", requirement)
	middle := createChildIssue(t, handler, "CORE", root.Key, requirement)
	issue := createChildIssue(t, handler, "CORE", middle.Key, requirement+"\n:::plan_task{#t9}\nTheirs.\n:::\n")
	sibling := createChildIssue(t, handler, "CORE", middle.Key, requirement)
	checks := "dispatch://" + issue.Key + "/artifact/checks-md"

	// A requirement of the issue's own spec, of its parent's and of its root's is named.
	for _, key := range []string{issue.Key, middle.Key, root.Key} {
		if response := uploadChecks(t, handler, issue.Key, claimMarkdown("c1", "dispatch://"+key+"/spec#b-r1")); response.Code != http.StatusCreated {
			t.Fatalf("a pointer at the requirement of %s: status=%d body=%s, want 201", key, response.Code, response.Body.String())
		}
	}
	saved := readChecks(t, database, issue.Key)

	siblingR1 := "dispatch://" + sibling.Key + "/spec#b-r1"
	expectRefusal(t, "a pointer into a sibling tree", uploadChecks(t, handler, issue.Key, claimMarkdown("c1", siblingR1)), http.StatusBadRequest,
		"INVALID_MARKDOWN", `"serves"`, siblingR1, sibling.Key, issue.Key)
	theirTask := "dispatch://" + issue.Key + "/spec#b-t9"
	ourTask := ":::plan_task{#t1}\nOurs.\n:::\n"
	expectRefusal(t, "a needs naming a plan task of another document",
		uploadChecks(t, handler, issue.Key, ourTask+"\n:::plan_task{#t2 needs=\""+theirTask+"\"}\nAfter theirs.\n:::\n"),
		http.StatusBadRequest, "INVALID_MARKDOWN", `"needs"`, theirTask, "its own document")
	if after := readChecks(t, database, issue.Key); !reflect.DeepEqual(after, saved) {
		t.Fatalf("checks.md after refused writes = %+v, want %+v unchanged", after, saved)
	}

	// The control: a needs naming a plan task of the same document is written.
	if response := uploadChecks(t, handler, issue.Key, ourTask+"\n:::plan_task{#t2 needs=\""+checks+"#b-t1\"}\nAfter ours.\n:::\n"); response.Code != http.StatusCreated {
		t.Fatalf("a needs naming a plan task of the same document: status=%d body=%s, want 201", response.Code, response.Body.String())
	}

	// A document its project holds, and no issue does, points only at a block of itself.
	handbook := createProjectDocument(t, handler, "CORE", "handbook.md", requirement)
	notes := createProjectDocument(t, handler, "CORE", "notes.md", "Before.\n")
	handbookR1 := "dispatch://CORE/artifact/" + handbook.Slug + "#b-r1"
	outside := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+notes.ID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": claimMarkdown("c1", handbookR1)}},
	}, "alice")
	expectRefusal(t, "a project document pointing at another document", outside, http.StatusBadRequest,
		"INVALID_OP", `field "attributes"`, `"serves"`, handbookR1, "a block of itself")
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+handbook.ID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": claimMarkdown("c1", handbookR1)}},
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("a project document pointing at a requirement of its own: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
}

// A browser's write is never refused: a settled browser tree whose pointer names a block no
// document holds writes its version and stores the pointer, which stays an unresolved reference,
// as any citation of an absent block is.
func TestABrowserWritePointingAtAMissingBlockIsSavedAndUnresolved(t *testing.T) {
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(nil))
	documents, handler, database := browserDocumentService(t)
	issue, _ := pointerSpec(t, handler)
	if response := uploadChecks(t, handler, issue.Key, "Before.\n"); response.Code != http.StatusCreated {
		t.Fatalf("upload checks.md: status=%d body=%s, want 201", response.Code, response.Body.String())
	}
	checks := readChecks(t, database, issue.Key)
	missing := "dispatch://" + issue.Key + "/spec#b-nowhere"

	writeBrowserDocument(t, documents, checks.ID, claimMarkdown("c1", missing))
	settledVersion(t, handler, documents, checks.ID, 2)

	stored := readBlockRefs(t, database, checks.ID)
	if len(stored) != 1 || stored[0].BlockID != "c1" || stored[0].Kind != "serves" || stored[0].ToID != issue.Key+"/spec#nowhere" {
		t.Fatalf("block_refs of checks.md = %#v, want c1's serves row at the missing block", stored)
	}
	requireReferenceNotFound(t, handler, url.Values{"to": {missing}}, `no block "nowhere" in dispatch://`+issue.Key+"/spec")
}

// setBlockAttributes writes typed-block state through the live room's own service call, as the
// answer route does (answerAsk's WriteBlock): inside a transaction joined with Docs.Join, the
// only way a document write runs, committed when the write succeeds. It returns
// SetBlockAttributes' own error, which a caller testing a refusal asserts on;
// setLiveBlockAttributes is the same write for a caller that wants it to succeed.
func setBlockAttributes(database *store.Store, documentService docs.API, artifactID, blockID string, attributes map[string]any, actor model.Actor) error {
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	joined, ledger := documentService.Join(ctx, tx)
	defer ledger.Discard()
	if err := documentService.SetBlockAttributes(joined, artifactID, blockID, attributes, actor); err != nil {
		return err
	}
	return ledger.Commit(ctx)
}

// The server's own write of a typed block's state (Docs.SetBlockAttributes) meets both refusals a
// `block[]` value is held to in the live room: one naming a block of a type the attribute's
// targets do not allow, refused against the block index (refuseBlockPointers), and one naming a
// part of an ask, which is no block of a document, refused as the write validates the tree it
// writes (validateBlockPointerValues, which pmdoc.SetBlockAttributes and pmdoc.Update both
// reach). The room keeps nothing of a refused write, and the block carries an allowed pointer
// afterwards.
func TestSetBlockAttributesRefusesABlockPointerInTheLiveRoom(t *testing.T) {
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(nil))
	// newTestServer, not blockIndexHandler: the document service this test calls is the
	// dependencies', the very one the handlers write through. Its documents never settle during
	// the test, as blockIndexHandler's do not.
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	issue, paragraph := pointerSpec(t, handler)
	spec := "dispatch://" + issue.Key + "/spec"
	checks := "dispatch://" + issue.Key + "/artifact/checks-md"
	ask := "dispatch://" + issue.Key + "/ask/22222222-2222-4222-8222-222222222222#b-b2"
	actor := model.Actor{Kind: "user", ID: "alice"}
	if response := uploadChecks(t, handler, issue.Key, ":::requirement{#r2}\nOurs.\n:::\n\n"+claimMarkdown("c1", spec+"#b-r1")); response.Code != http.StatusCreated {
		t.Fatalf("upload checks.md: status=%d body=%s, want 201", response.Code, response.Body.String())
	}
	saved := readChecks(t, database, issue.Key)
	before := documentMarkdown(t, handler, saved.ID)

	for _, test := range []struct {
		name string
		// serves is the one pointer the write sets, and target is true when it is refused for
		// the type of the block it names, not for naming something no pointer may name.
		serves string
		target bool
		wants  []string
	}{
		{name: "a pointer at a paragraph", serves: spec + "#b-" + paragraph, target: true, wants: []string{`"serves"`, spec + "#b-" + paragraph, `"paragraph"`, "requirement"}},
		{name: "a pointer at a callout", serves: spec + "#b-k1", target: true, wants: []string{`"serves"`, spec + "#b-k1", `"callout"`, "requirement"}},
		{name: "a pointer at a part of an ask", serves: ask, wants: []string{`"serves"`, ask}},
		{name: "a pointer at a pinned part of an ask", serves: ask + "@v1", wants: []string{`"serves"`, ask + "@v1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := setBlockAttributes(database, deps.Docs, saved.ID, "c1", map[string]any{"serves": []string{test.serves}}, actor)
			if err == nil {
				t.Fatal("the write was accepted, want it refused")
			}
			if test.target {
				if !errors.Is(err, pmdoc.ErrBlockPointer) {
					t.Fatalf("error = %v, want a block pointer refusal", err)
				}
			} else {
				// The value is refused where the write sets it, so the room never records it
				// and then answers for its own document as one outside the schema.
				if !errors.Is(err, pmdoc.ErrSchema) || errors.Is(err, docs.ErrDocSchema) {
					t.Fatalf("error = %v, want the value refused (%v), not the document (%v)", err, pmdoc.ErrSchema, docs.ErrDocSchema)
				}
				const sentence = "is a block of an ask: a block[] attribute points only at a block of a document"
				if !strings.HasSuffix(err.Error(), sentence) {
					t.Fatalf("error = %v, want it to end %q", err, sentence)
				}
			}
			for _, want := range test.wants {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %v, want it to name %s", err, want)
				}
			}
			if after := documentMarkdown(t, handler, saved.ID); after != before {
				t.Fatalf("the room after a refused write = %q, want %q unchanged", after, before)
			}
			if after := readChecks(t, database, issue.Key); !reflect.DeepEqual(after, saved) {
				t.Fatalf("checks.md after a refused write = %+v, want %+v unchanged", after, saved)
			}
		})
	}

	if err := setBlockAttributes(database, deps.Docs, saved.ID, "c1", map[string]any{"serves": []string{checks + "#b-r2"}}, actor); err != nil {
		t.Fatalf("a pointer at a requirement: %v, want the write accepted", err)
	}
	want := ":::requirement{#r2}\nOurs.\n:::\n\n" + claimMarkdown("c1", checks+"#b-r2")
	if after := documentMarkdown(t, handler, saved.ID); after != want {
		t.Fatalf("the room after the accepted write = %q, want %q", after, want)
	}
}
