package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestDecisionFromRefusesAnOrdinaryParagraph(t *testing.T) {
	handler, database := blockIndexHandler(t)
	issue, paragraph := pointerSpec(t, handler)
	target := "dispatch://" + issue.Key + "/spec#b-" + paragraph
	response := uploadChecks(t, handler, issue.Key, ":::requirement{#r2 from=\""+target+"\"}\nA derived requirement.\n:::\n")
	expectRefusal(t, "a provenance pointer to a paragraph", response, http.StatusBadRequest,
		"INVALID_MARKDOWN", `"from"`, target, `"paragraph"`, "ask")
	if state := readChecks(t, database, issue.Key); state.ID != "" {
		t.Fatalf("refused paragraph pointer created checks.md: %+v", state)
	}
}

func TestDecisionFromRefusesARequirement(t *testing.T) {
	handler, database := blockIndexHandler(t)
	issue, _ := pointerSpec(t, handler)
	target := "dispatch://" + issue.Key + "/spec#b-r1"
	response := uploadChecks(t, handler, issue.Key, ":::requirement{#r2 from=\""+target+"\"}\nA derived requirement.\n:::\n")
	expectRefusal(t, "a provenance pointer to a requirement", response, http.StatusBadRequest,
		"INVALID_MARKDOWN", `"from"`, target, `"requirement"`, "ask")
	if state := readChecks(t, database, issue.Key); state.ID != "" {
		t.Fatalf("refused requirement pointer created checks.md: %+v", state)
	}
}

func TestDecisionFromDocumentAskDerivesAndReadsBack(t *testing.T) {
	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", ":::ask{#q1}\nWhich option should shape the requirement?\n- Keep it.\n- Change it.\n:::\n")
	target := "dispatch://" + issue.Key + "/spec#b-q1"
	response := uploadChecks(t, handler, issue.Key, ":::requirement{#r2 from=\""+target+"\"}\nA derived requirement.\n:::\n")
	if response.Code != http.StatusCreated {
		t.Fatalf("document ask provenance: %d %s", response.Code, response.Body.String())
	}
	checks := readChecks(t, database, issue.Key)
	rows := readBlockRefs(t, database, checks.ID)
	if len(rows) != 1 || rows[0].BlockID != "r2" || rows[0].FromNode != issue.Key+"/checks-md#r2" ||
		rows[0].Kind != "from" || rows[0].ToKind != "block" || rows[0].ToID != issue.Key+"/spec#q1" {
		t.Fatalf("derived block_refs = %+v, want exactly r2 --from--> q1", rows)
	}
	markdown := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+checks.ID+"/text", nil, "alice")).Markdown
	if !strings.Contains(markdown, `from="`+target+`"`) {
		t.Fatalf("readback lost the provenance pointer: %q", markdown)
	}
	requireEdgeSources(t, "provenance edges to the document ask", graphEdges(t, handler, url.Values{
		"to": {target}, "kind": {"from"},
	}).Edges, "from block "+issue.Key+"/checks-md#r2")
	links := readBlockLinks(t, handler, "dispatch://"+issue.Key+"/artifact/checks-md", "dispatch://"+issue.Key+"/spec").Links
	if len(links) != 1 || links[0].Kind != "from" || links[0].From.BlockID != "r2" || links[0].To.BlockID != "q1" {
		t.Fatalf("readback links = %+v, want exactly r2 --from--> q1", links)
	}
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+checks.ID+"/blocks", nil, "alice"))
	for _, block := range blocks {
		if block.ID != "r2" {
			continue
		}
		for _, attribute := range block.Attributes {
			if attribute.Name == "from" && attribute.Kind == "block[]" && attribute.Value == target {
				return
			}
		}
		t.Fatalf("r2 attributes = %+v, want the from pointer", block.Attributes)
	}
	t.Fatal("readback contains no r2 block")
}

func TestARequirementFromACommentBlockIsAnEdgeBothWays(t *testing.T) {
	handler, database := blockIndexHandler(t)
	issue, _ := pointerSpec(t, handler)
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Keep this source."}, "alice")
	commentRef := "dispatch://" + issue.Key + "/comment/" + comment.ID
	target := commentRef + "#b-b1"
	response := uploadChecks(t, handler, issue.Key, ":::requirement{#r9 from=\""+target+"\"}\nNeed.\n:::\n")
	if response.Code != http.StatusCreated {
		t.Fatalf("comment provenance: %d %s", response.Code, response.Body.String())
	}
	checks := readChecks(t, database, issue.Key)
	rows := readBlockRefs(t, database, checks.ID)
	if len(rows) != 1 || rows[0].ToKind != "comment_block" || rows[0].ToID != "comment/"+comment.ID+"#b1" || rows[0].Kind != "from" {
		t.Fatalf("item provenance rows = %+v", rows)
	}
	from := "dispatch://" + issue.Key + "/artifact/checks-md#b-r9"
	links := graphEdges(t, handler, url.Values{"from": {from}, "kind": {"from"}}).Edges
	if len(links) != 1 || links[0].Kind != "from" || links[0].Node.Ref != target {
		t.Fatalf("outgoing from edges = %+v", links)
	}
	requireEdgeSources(t, "incoming provenance", graphEdges(t, handler, url.Values{"to": {commentRef}, "kind": {"from"}}).Edges,
		"from block "+issue.Key+"/checks-md#r9")
}

func TestARequirementFromMessageAndItemAskBlocksIsAnEdge(t *testing.T) {
	for _, kind := range []string{"message", "ask"} {
		t.Run(kind, func(t *testing.T) {
			handler, database := blockIndexHandler(t)
			issue, _ := pointerSpec(t, handler)
			var id string
			if kind == "message" {
				id = createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Choose this."}, "alice").ID
			} else {
				id = createPartAsk(t, handler, issue.Key, "Choose this?").ID
			}
			target := "dispatch://" + issue.Key + "/" + kind + "/" + id + "#b-b1"
			response := uploadChecks(t, handler, issue.Key, ":::requirement{#r9 from=\""+target+"\"}\nNeed.\n:::\n")
			if response.Code != http.StatusCreated {
				t.Fatalf("%s provenance: %d %s", kind, response.Code, response.Body.String())
			}
			rows := readBlockRefs(t, database, readChecks(t, database, issue.Key).ID)
			if len(rows) != 1 || rows[0].ToKind != kind+"_block" || rows[0].ToID != kind+"/"+id+"#b1" {
				t.Fatalf("%s provenance rows = %+v", kind, rows)
			}
		})
	}
}

func TestAFromPointerToAnotherIssuesCommentIsRefused(t *testing.T) {
	handler, database := blockIndexHandler(t)
	issue, _ := pointerSpec(t, handler)
	other := decodeBody[model.Issue](t, created(t, "create an unrelated issue", createIssueRequest(t, handler, map[string]any{
		"project": "CORE", "title": "Independent decision", "spec": "Other.",
	})))
	comment := createThreadComment(t, handler, other.Key, map[string]any{"body": "Other source."}, "alice")
	target := "dispatch://" + other.Key + "/comment/" + comment.ID + "#b-b1"
	response := uploadChecks(t, handler, issue.Key, ":::requirement{#r9 from=\""+target+"\"}\nNeed.\n:::\n")
	expectRefusal(t, "an unrelated source", response, http.StatusBadRequest, "INVALID_MARKDOWN", other.Key, "issue above it")
	if after := readChecks(t, database, issue.Key); after.ID != "" {
		t.Fatal("refused write created checks.md")
	}
	child := createChildIssue(t, handler, "CORE", other.Key, "Child.")
	response = uploadChecks(t, handler, child.Key, ":::requirement{#r9 from=\""+target+"\"}\nNeed.\n:::\n")
	if response.Code != http.StatusCreated {
		t.Fatalf("a parent issue's comment was refused: %d %s", response.Code, response.Body.String())
	}
}

func TestAFromPointerToARemovedCommentBlockIsRefused(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	issue, _ := pointerSpec(t, handler)
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Keep.\n\nRemove."}, "alice")
	requireEdit(t, "remove b2", editCommentAs(t, handler, comment.ID, "Keep.", nil, nil))
	target := "dispatch://" + issue.Key + "/comment/" + comment.ID + "#b-b2"
	response := uploadChecks(t, handler, issue.Key, ":::requirement{#r9 from=\""+target+"\"}\nNeed.\n:::\n")
	expectRefusal(t, "a removed source block", response, http.StatusBadRequest, "INVALID_MARKDOWN", "no document holds")
	current := strings.Replace(target, "#b-b2", "#b-b1", 1)
	if response := uploadChecks(t, handler, issue.Key, ":::requirement{#r9 from=\""+current+"\"}\nNeed.\n:::\n"); response.Code != http.StatusCreated {
		t.Fatalf("current source block: %d %s", response.Code, response.Body.String())
	}
}

func TestServesNamingACommentBlockIsStillRefused(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	issue, _ := pointerSpec(t, handler)
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Source."}, "alice")
	target := "dispatch://" + issue.Key + "/comment/" + comment.ID + "#b-b1"
	response := uploadChecks(t, handler, issue.Key, ":::check{#c1 serves=\""+target+"\"}\nCheck.\n:::\n")
	expectRefusal(t, "serves cannot name an item", response, http.StatusBadRequest, "INVALID_MARKDOWN", "block[] attribute points only at a block of a document")
}

func TestAFromPointerMustNameTheItemsOwnIssue(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	issue, _ := pointerSpec(t, handler)
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Source."}, "alice")
	wrongOwner := "dispatch://CORE-999/comment/" + comment.ID + "#b-b1"
	response := uploadChecks(t, handler, issue.Key, ":::requirement{#r9 from=\""+wrongOwner+"\"}\nNeed.\n:::\n")
	expectRefusal(t, "the item's issue is not CORE-999", response, http.StatusBadRequest, "INVALID_MARKDOWN")
	actualOwner := strings.Replace(wrongOwner, "CORE-999", issue.Key, 1)
	if response := uploadChecks(t, handler, issue.Key, ":::requirement{#r9 from=\""+actualOwner+"\"}\nNeed.\n:::\n"); response.Code != http.StatusCreated {
		t.Fatalf("the item's own reference: %d %s", response.Code, response.Body.String())
	}
}
