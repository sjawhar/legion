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
