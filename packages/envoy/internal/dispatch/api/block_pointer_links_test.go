package api

import (
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/pmdoctest"
)

// edgeSources is each edge as "<kind> <node kind> <node id>", sorted.
func edgeSources(edges []model.GraphEdge) []string {
	sources := make([]string, len(edges))
	for index, edge := range edges {
		sources[index] = edge.Kind + " " + edge.Node.Kind + " " + edge.Node.ID
	}
	sort.Strings(sources)
	return sources
}

// requireEdgeSources fails unless edges are exactly want (edgeSources), in any order.
func requireEdgeSources(t *testing.T, stage string, edges []model.GraphEdge, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := edgeSources(edges); !reflect.DeepEqual(got, want) && (len(got) != 0 || len(want) != 0) {
		t.Fatalf("%s: edges = %q, want %q", stage, got, want)
	}
}

// A3: a saved claim whose serves attribute names a requirement is a link named serves from the
// claim's block to the requirement, listed and queried by that name, and is the only link the
// pointer makes: neither the claim nor its document lists a mentions edge beside it.
func TestASavedBlockPointerIsALinkNamedByItsAttribute(t *testing.T) {
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(nil))
	handler, database := blockIndexHandler(t)
	issue, _ := pointerSpec(t, handler)
	specRef := "dispatch://" + issue.Key + "/spec"
	r1 := specRef + "#b-r1"
	checksRef := "dispatch://" + issue.Key + "/artifact/checks-md"
	c1 := issue.Key + "/checks-md#c1"
	if response := uploadChecks(t, handler, issue.Key, claimMarkdown("c1", r1)); response.Code != http.StatusCreated {
		t.Fatalf("upload the claim: status=%d body=%s", response.Code, response.Body.String())
	}
	checks := readChecks(t, database, issue.Key).ID

	requireEdgeSources(t, "links to r1", graphEdges(t, handler, url.Values{"to": {r1}}).Edges, "serves block "+c1)
	requireEdgeSources(t, "serves links to r1", graphEdges(t, handler, url.Values{"to": {r1}, "kind": {"serves"}}).Edges, "serves block "+c1)
	requireEdgeSources(t, "mentions of r1", graphEdges(t, handler, url.Values{"to": {r1}, "kind": {"mentions"}}).Edges)
	expectRefusal(t, "an unknown kind", graphRequest(t, handler, url.Values{"to": {r1}, "kind": {"nope"}}), http.StatusBadRequest, "INVALID_KIND")

	atR1 := 0
	for _, edge := range graphEdges(t, handler, url.Values{"to": {specRef}}).Edges {
		if edge.Node.Kind == "artifact" && edge.Node.ID == checks {
			t.Fatalf("the spec's links list checks.md's own %s edge beside its claim's: %#v", edge.Kind, edge)
		}
		if edge.Block != nil && edge.Block.ID == issue.Key+"/spec#r1" && edge.Node.ID == c1 && edge.Kind == "serves" {
			atR1++
		}
	}
	if atR1 != 1 {
		t.Fatalf("the spec's links at r1 from the claim = %d, want 1 serves edge", atR1)
	}
	links := readBlockLinks(t, handler, checksRef, specRef).Links
	if len(links) != 1 || links[0].Kind != "serves" || links[0].From.BlockID != "c1" || links[0].To.BlockID != "r1" {
		t.Fatalf("block links from checks.md to the spec = %#v, want c1 serves r1", links)
	}

	// The control: a claim pointing at nothing links nothing.
	if response := uploadChecks(t, handler, issue.Key, claimMarkdown("c1", "")); response.Code != http.StatusCreated {
		t.Fatalf("upload the claim with no pointer: status=%d body=%s", response.Code, response.Body.String())
	}
	requireEdgeSources(t, "links to r1 once the claim points at nothing", graphEdges(t, handler, url.Values{"to": {r1}}).Edges)
	if links := readBlockLinks(t, handler, checksRef, specRef).Links; len(links) != 0 {
		t.Fatalf("block links once the claim points at nothing = %#v, want none", links)
	}
}

// A pointer and a prose citation of one target are separate edges, each from the block holding
// it: the claim's serves and answers edges from the claim, and the mention from the claim's
// paragraph. Dropping the prose keeps the pointer's edge as it was first written.
func TestAPointerAndAProseCitationOfOneTargetAreTwoEdges(t *testing.T) {
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(map[string]pmdoc.BlockAttributeSchema{
		"serves":  {Kind: "block[]", Targets: []string{"requirement"}},
		"answers": {Kind: "block[]"},
	}))
	handler, database := blockIndexHandler(t)
	issue, _ := pointerSpec(t, handler)
	r1 := "dispatch://" + issue.Key + "/spec#b-r1"
	c1 := issue.Key + "/checks-md#c1"
	claim := func(body string) string {
		return ":::claim{#c1 answers=\"" + r1 + "\" serves=\"" + r1 + "\"}\n" + body + "\n:::\n"
	}
	prose := "It holds, per " + r1 + "."
	if response := uploadChecks(t, handler, issue.Key, claim(prose)); response.Code != http.StatusCreated {
		t.Fatalf("upload the claim: status=%d body=%s", response.Code, response.Body.String())
	}
	checks := readChecks(t, database, issue.Key).ID
	paragraph := blockIDsByText(t, handler, checks)[prose]
	if paragraph == "" {
		t.Fatalf("GET …/blocks of checks.md lists no block for %q", prose)
	}
	requireEdgeSources(t, "links to r1", graphEdges(t, handler, url.Values{"to": {r1}}).Edges,
		"serves block "+c1, "answers block "+c1, "mentions block "+issue.Key+"/checks-md#"+paragraph)
	servesVersion := func() int {
		t.Helper()
		for _, row := range readBlockRefs(t, database, checks) {
			if row.BlockID == "c1" && row.Kind == "serves" {
				return row.CreatedVersion
			}
		}
		t.Fatal("checks.md holds no serves row from c1")
		return 0
	}
	if version := servesVersion(); version != 1 {
		t.Fatalf("the serves edge was created at version %d, want 1", version)
	}

	if response := uploadChecks(t, handler, issue.Key, claim("It holds.")); response.Code != http.StatusCreated {
		t.Fatalf("upload the claim without its prose citation: status=%d body=%s", response.Code, response.Body.String())
	}
	requireEdgeSources(t, "links to r1 without the prose citation", graphEdges(t, handler, url.Values{"to": {r1}}).Edges,
		"serves block "+c1, "answers block "+c1)
	if version := servesVersion(); version != 1 {
		t.Fatalf("the serves edge was created at version %d after the prose went, want still 1", version)
	}
}

// The graph lists a pointer's edge only while its target has a type the attribute allows, and no
// other edge stands in for it while it is hidden; the document-level outgoing read lists the
// citation whatever the target's type (D7). A target retyped back, or deleted and restored, is
// listed again.
func TestABlockPointerIsListedOnlyWhileItsTargetHasAnAllowedType(t *testing.T) {
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(nil))
	handler, database := blockIndexHandler(t)
	issue, _ := pointerSpec(t, handler)
	specRef := "dispatch://" + issue.Key + "/spec"
	r1 := specRef + "#b-r1"
	r1Node := issue.Key + "/spec#r1"
	checksRef := "dispatch://" + issue.Key + "/artifact/checks-md"
	c1 := issue.Key + "/checks-md#c1"
	if response := uploadChecks(t, handler, issue.Key, claimMarkdown("c1", r1)); response.Code != http.StatusCreated {
		t.Fatalf("upload the claim: status=%d body=%s", response.Code, response.Body.String())
	}
	checks := readChecks(t, database, issue.Key).ID

	// target is the state of r1: "requirement", "callout" or "" (deleted).
	requireListed := func(stage, target string) {
		t.Helper()
		listed := target == "requirement"
		want := 0
		if listed {
			want = 1
		}
		switch target {
		case "requirement":
			requireEdgeSources(t, stage+": links to r1", graphEdges(t, handler, url.Values{"to": {r1}}).Edges, "serves block "+c1)
		case "callout":
			requireEdgeSources(t, stage+": links to r1", graphEdges(t, handler, url.Values{"to": {r1}}).Edges)
		default:
			requireReferenceNotFound(t, handler, url.Values{"to": {r1}}, `no block "r1" in `+specRef)
		}
		if links := readBlockLinks(t, handler, checksRef, specRef).Links; len(links) != want || listed && links[0].Kind != "serves" {
			t.Fatalf("%s: block links from checks.md to the spec = %#v, want %d serves link", stage, links, want)
		}
		toR1 := 0
		for _, edge := range graphEdges(t, handler, url.Values{"from": {checksRef}}).Edges {
			if edge.Node.ID == r1Node {
				if edge.Kind != "serves" || edge.Block == nil || edge.Block.ID != c1 {
					t.Fatalf("%s: checks.md's links to r1 hold %#v, want only c1's serves edge", stage, edge)
				}
				toR1++
			}
		}
		if toR1 != want {
			t.Fatalf("%s: checks.md's links to r1 = %d, want %d", stage, toR1, want)
		}
		atR1 := 0
		for _, edge := range graphEdges(t, handler, url.Values{"to": {specRef}}).Edges {
			if edge.Node.Kind == "artifact" && edge.Node.ID == checks {
				t.Fatalf("%s: the spec's links list checks.md's own %s edge: %#v", stage, edge.Kind, edge)
			}
			if edge.Block != nil && edge.Block.ID == r1Node {
				if edge.Kind != "serves" || edge.Node.ID != c1 {
					t.Fatalf("%s: the spec's links at r1 hold %#v, want only c1's serves edge", stage, edge)
				}
				atR1++
			}
		}
		if atR1 != want {
			t.Fatalf("%s: the spec's links at r1 = %d, want %d", stage, atR1, want)
		}
		if target != "" {
			outgoing := decodeBody[model.ArtifactReferences](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+checks+"/references", nil, "alice")).Outgoing
			if len(outgoing) != 1 || outgoing[0].Kind != "block" || outgoing[0].ToID != r1Node {
				t.Fatalf("%s: checks.md's outgoing references = %#v, want r1", stage, outgoing)
			}
		}
	}
	requireListed("pointing at a requirement", "requirement")

	editSpec(t, handler, issue.Key, []map[string]any{{"op": "retype", "block": "r1", "type": "callout"}})
	requireListed("r1 retyped to a callout", "callout")

	editSpec(t, handler, issue.Key, []map[string]any{{"op": "retype", "block": "r1", "type": "requirement"}})
	requireListed("r1 retyped back to a requirement", "requirement")

	editSpec(t, handler, issue.Key, []map[string]any{{"op": "delete", "block": "r1"}})
	requireListed("r1 deleted", "")

	editSpec(t, handler, issue.Key, []map[string]any{{"op": "insert", "after": "end", "markdown": ":::requirement{#r1}\nA requirement.\n:::\n"}})
	requireListed("r1 restored", "requirement")
}
