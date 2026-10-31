package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// newerAskAttribute is an attribute of the decision block that this build's manifest does not
// declare and the newer build's manifest (useNewerAskManifest) does.
const newerAskAttribute = "test_newer_attr"

// useNewerAskManifest makes this process read and write by the embedded manifest with one more
// attribute on the decision block, newerAskAttribute, as a newer build does, until the test ends.
// The manifest is package-global, so no other build of this test may run pmdoc while it is
// installed or restored.
func useNewerAskManifest(t *testing.T) {
	t.Helper()
	var manifest map[string]any
	if err := json.Unmarshal(pmdoc.SchemaJSON(), &manifest); err != nil {
		t.Fatalf("decode the manifest: %v", err)
	}
	for _, typ := range manifest["types"].([]any) {
		block := typ.(map[string]any)
		if block["name"] == "ask" {
			block["attributes"].(map[string]any)[newerAskAttribute] = map[string]any{"kind": "string"}
		}
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("encode the manifest: %v", err)
	}
	restore, err := pmdoc.UseManifestForTest(encoded)
	if err != nil {
		t.Fatalf("install the manifest: %v", err)
	}
	t.Cleanup(restore)
}

// newerBuildDocuments is a second Dispatch process on database, with its own document service and
// routes, as a newer build serving the same database during a release runs.
func newerBuildDocuments(t *testing.T, database *store.Store, settle time.Duration) (http.Handler, *docs.Service) {
	t.Helper()
	service := docs.New(docs.Deps{Store: database, Settle: settle})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	deps, err := NewDeps(DepsInput{
		Store:       database,
		Identity:    headerIdentity(database),
		AgentTokens: sharedAgentTokens(t, "agent-token"),
		Docs:        service,
		ServerURL:   "https://dispatch.example",
	})
	if err != nil {
		t.Fatalf("new API dependencies of the newer build: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, t.Context(), deps).Wait)
	return mux, service
}

// Two builds serve one database during a release, and the version a human approved can hold an
// element only the newer one knows: here an attribute on the decision block. This build cannot read
// that version, so it cannot decide whether his answer carries his approval. Its settlement writes
// no version, leaves his approval as it was and logs that a build that reads the element must write
// the version. The newer build's settlement resumption then writes the version with his approval
// carried.
func TestAVersionWhoseApprovedVersionHoldsNewerContentIsWrittenByABuildThatReadsIt(t *testing.T) {
	logged := captureLog(t)
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "TEST", "Approved newer content", "Context\n\n"+regionAsk)
	asks := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, map[string]string{"region": "Which region goes first?"})
	approveVersion(t, handler, issue.PrimaryArtifactID, "alice", 1)

	// The newer build's rendering of the approved version: its decision block holds the attribute.
	written, err := database.Pool.Exec(ctx, `
		update artifact_versions set markdown = replace(markdown, '{#region ', '{#region `+newerAskAttribute+`="first" ')
		where artifact_id = $1 and number = 1 and markdown like '%{#region %'
	`, issue.PrimaryArtifactID)
	if err != nil || written.RowsAffected() != 1 {
		t.Fatalf("store the newer build's rendering of version 1: %v, %d rows; want one", err, written.RowsAffected())
	}

	answerDecisionBlock(t, handler, asks["region"], "alice", []string{"East"}, "", nil)
	refusal := ""
	for deadline := time.Now().Add(10 * time.Second); refusal == ""; {
		for line := range strings.Lines(logged.String()) {
			if strings.Contains(line, newerAskAttribute) {
				refusal = line
				break
			}
		}
		if refusal == "" && time.Now().After(deadline) {
			t.Fatalf("no settlement met the approved version's attribute:\n%s", logged.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(refusal, "no version written") || !strings.Contains(refusal, "a build that reads the element writes it") || !strings.Contains(refusal, "version 1") {
		t.Fatalf("settlement log = %q, want it to say it wrote no version, that a build that reads the element writes it, and which version holds the element", refusal)
	}
	var latest int
	if err := database.Pool.QueryRow(ctx, `select max(number) from artifact_versions where artifact_id = $1`, issue.PrimaryArtifactID).Scan(&latest); err != nil {
		t.Fatalf("read the latest version: %v", err)
	}
	if latest != 1 {
		t.Fatalf("this build wrote version %d over an approved version it cannot read, want no version", latest)
	}
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "approved")

	// The document still owes its settlement. The newer build's resumption arms a settlement owed
	// for a minute, so the row is aged past that.
	owed, err := database.Pool.Exec(ctx, `
		update doc_settlements_pending set marked_at = now() - interval '2 minutes' where artifact_id = $1
	`, issue.PrimaryArtifactID)
	if err != nil || owed.RowsAffected() != 1 {
		t.Fatalf("age the owed settlement: %v, %d rows; want the document's one", err, owed.RowsAffected())
	}
	useNewerAskManifest(t)
	newer, service := newerBuildDocuments(t, database, 20*time.Millisecond)
	resumption, stop := context.WithCancel(ctx)
	resumed := make(chan struct{})
	go func() {
		defer close(resumed)
		service.RunSettlementResumption(resumption)
	}()
	t.Cleanup(func() {
		stop()
		<-resumed
	})
	waitForArtifactVersion(t, newer, issue.PrimaryArtifactID, 2)
	requireCarriedReview(t, newer, issue.PrimaryArtifactID, 2, 1, nil)
}
