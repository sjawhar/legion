package api

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// A check's words are its assertion. Rewording it under an unchanged serves edge
// must stay stale; INFRA-1253 R4 inherits this characterization of today's carry.
func TestACheckWhoseWordsChangedCarriesNoApprovalThoughItsServesDidNot(t *testing.T) {
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	issue := createInteractionIssue(t, handler, "WORDS", "Check words", ":::requirement{#r1}\nThe read succeeds.\n:::\n")
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", fmt.Sprintf(":::check{#c1 serves=\"dispatch://%s/spec#b-r1\" surface=\"api\" run=\"go test ./x\"}\nThe read returns 200.\n:::\n", issue.Key))
	id := verification.Artifact.ID
	reviewDocument(t, handler, id, "alice", "approved", "")
	reviseDocument(t, handler, id, "The read returns 200.", "The read returns 404.", 2)
	requireNoCarry(t, handler, id, 1, "stale")
	if approval := readApproval(t, handler, id).Approval; approval.Version == nil || *approval.Version != 1 {
		t.Fatalf("approval = %#v", approval)
	}
	var edges int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from block_refs r join artifacts a on r.from_node = a.ref_key || '#c1' join artifacts s on r.to_id = s.ref_key || '#r1' where a.id = $1 and s.id = $2 and r.to_kind = 'block' and r.kind = 'serves'`, id, issue.PrimaryArtifactID).Scan(&edges); err != nil {
		t.Fatal(err)
	}
	if edges != 1 {
		t.Fatalf("unchanged serves edges = %d, want 1", edges)
	}
	// Negative control: byte-identical restore really does carry an approval.
	reviseDocument(t, handler, id, "The read returns 404.", "The read returns 200.", 3)
	reviews := documentReviews(t, handler, id)
	approval := readApproval(t, handler, id).Approval
	if approval.State != "approved" || approval.Version == nil || *approval.Version != 3 || len(reviews) != 2 || reviews[0].Version != 3 || reviews[0].Reason == nil || len(*reviews[0].Reason) < len("Carried forward") || (*reviews[0].Reason)[:len("Carried forward")] != "Carried forward" {
		t.Fatalf("restore approval=%#v reviews=%#v", approval, reviews)
	}
}
