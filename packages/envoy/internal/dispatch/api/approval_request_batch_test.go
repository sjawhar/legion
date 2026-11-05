package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// An approval request reads the document's approval twice inside its own transaction, once
// before and once after it opens or renews the ask, each a batch on that transaction
// (attachApproval, queueApprovals). Run in two concurrent waves, one on documents of their own
// and one on a single document, every request finishes, none fails, each document is left
// awaiting its one open request, and the transactions give every connection back. What one
// request answers in sequence - 201 for the first, 200 for a repeat - is reviews_test.go's
// (TestApprovalRequestOpensAnAskWhoseAnswerPinsAReviewToTheDocumentVersion).
func TestApprovalRequestsSentAtOnceReadTheirApprovalInTheirTransaction(t *testing.T) {
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documents := docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
		return documents
	})
	first := createInteractionIssue(t, handler, "APPR", "Approve the first", "A spec")
	documents := []string{first.PrimaryArtifactID}
	for range 7 {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "APPR", "title": "Approve another", "spec": "A spec", "force": true,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create issue: status=%d body=%s", response.Code, response.Body.String())
		}
		documents = append(documents, decodeBody[struct {
			PrimaryArtifactID string `json:"primary_artifact_id"`
		}](t, response).PrimaryArtifactID)
	}
	request := func(artifactID string) int {
		return sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/approval-requests",
			map[string]any{"actor": sessionActor()}).Code
	}
	// atOnce sends each of targets' requests at the same moment and answers their statuses, or
	// fails when one has not answered within a bound no healthy transaction reaches.
	atOnce := func(targets []string) []int {
		t.Helper()
		statuses := make([]int, len(targets))
		var started, finished sync.WaitGroup
		started.Add(1)
		for index, artifactID := range targets {
			finished.Add(1)
			go func() {
				defer finished.Done()
				started.Wait()
				statuses[index] = request(artifactID)
			}()
		}
		done := make(chan struct{})
		go func() { finished.Wait(); close(done) }()
		started.Done()
		select {
		case <-done:
		case <-time.After(90 * time.Second):
			t.Fatalf("approval requests sent at once did not all answer within 90 s")
		}
		return statuses
	}

	// At once, one each on eight documents: every one opens its document's ask.
	for index, code := range atOnce(documents) {
		if code != http.StatusCreated {
			t.Fatalf("request %d on its own document: status=%d, want 201", index, code)
		}
	}
	// At once, sixteen on one document that already has its request open. Each is 200: a request
	// that names no summary takes the open request's own, so the hand-back switch sees
	// summary == openSummary on a request waiting on the human (reviews.go, renewApprovalAsk),
	// which is neither the 409 a different summary would be refused with nor the hand-back that
	// would answer 201. Nothing in this test replies to the ask or writes a version, so the turn
	// every one of them reads is the human's.
	same := make([]string, 16)
	for index := range same {
		same[index] = documents[1]
	}
	for index, code := range atOnce(same) {
		if code != http.StatusOK {
			t.Fatalf("request %d on one document: status=%d, want 200", index, code)
		}
	}
	for _, artifactID := range documents {
		if got := readApproval(t, handler, artifactID); got.Approval == nil || got.Approval.State != "awaiting" || got.Approval.AskID == nil {
			t.Fatalf("document %s approval = %#v, want awaiting its open request", artifactID, got.Approval)
		}
	}
	if use := database.Pool.Use(); use.InUse != 0 {
		t.Fatalf("connections still in use after every request answered: %d", use.InUse)
	}
}

// countedApprovalRequest opens a document's approval request through the handler on a request a
// roundTrips counts (countedRead, issue_read_batch_test.go).
func countedApprovalRequest(t *testing.T, handler http.Handler, artifactID string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"actor": sessionActor()})
	if err != nil {
		t.Fatalf("encode the approval request: %v", err)
	}
	ctx := context.WithValue(context.Background(), countedRead{}, true)
	request := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/artifacts/"+artifactID+"/approval-requests", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer agent-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// One approval request on a document with no request open costs twenty-one round trips, counted
// as the issue read's are (roundTrips): one for each statement it runs on its own and one for
// each batch, whatever that batch holds. The first eight read the coverage the answer reports,
// before the request's transaction opens (approvalCoverage); the thirteen after them are the
// transaction itself, and all but two of those are reads: this route's writes are queued and
// sent as one batch before the commit, which is what holds its transaction id to its database
// work (INFRA-636). In the order it sends them:
//
//  1. the artifact row the coverage is read for (loadArtifact);
//  2. a batch of 3: that artifact's versions, its latest review and its open approval request
//     (loadArtifactRow, queueArtifactDetails);
//  3. the issue's spec and whether it has a plan (issueCoverage);
//  4. the spec's settled version, to read its text at (docs.Text, through requirementBlocksIn);
//  5. begin, that text read's read-only snapshot;
//  6. the document's stored snapshot;
//  7. the updates after it;
//  8. commit, which ends the text read; this spec marks no requirement, so the coverage asks
//     nothing further - no descendant walk and no count of what serves each requirement;
//  9. begin, the request's own transaction;
//  10. the artifact row (loadArtifact);
//  11. a batch of 3: that artifact's versions, its latest review and its open approval request
//     (loadArtifactRow, queueArtifactDetails);
//  12. a batch of 2: the owning issue's owner lock, and in the statement after it the row that
//     lock guards (requireOpenOwner, through store.LockOwnerAndRead). The lock assigns no
//     transaction id, and the read is a statement of its own because a statement's snapshot is
//     taken before it waits;
//  13. the document's settled version number (settledVersionNumber);
//  14. a batch of 2: the approval before the write - the latest review and the open request
//     (attachApproval, queueApprovals);
//  15. the open approval ask naming this document, which there is none of
//     (docs.OpenApprovalAskForDocuments);
//  16. the new ask's id and the transaction's time (select gen_random_uuid()::text, now());
//  17. the latest approved review the request carries (docs.LatestApprovedReview);
//  18. the question's references as they stand, which the replacement is planned against
//     (replaceReferences);
//  19. the ask's block state, read for its parts' plan (refs.PlanAskParts, through
//     refs.ReadItemIndex);
//  20. a batch of 14: the whole write phase, sent back to back - the ask row, its block state and
//     the part rows of its question and options, the author's follow, the references, the
//     ask.opened event with its payload and the issue's last_seq, the reference stamps, and the
//     approval as the request leaves it;
//  21. commit.
//
// The transaction takes its transaction id at the first statement of 20 and gives it up at 21;
// the per-route window test measures that window, and this test bounds what the route sends
// around it. The one-phase route sent thirty-four, its write statements each a round trip of
// its own between the reads.
func TestApprovalRequestsCostTwentyOneRoundTrips(t *testing.T) {
	database := storetest.Open(t)
	counter := &roundTrips{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	handler, _, _ := newTestServer(t, testServerOptions{
		store: traced,
		// The caller's identity is resolved on the untraced pool, so the count is the request's
		// own statements and not the sign-in lookup every request makes.
		identity: headerIdentity(database),
		// A settlement of the document is database work of its own on the same pool; a delay no
		// test run reaches keeps it out of the count.
		settle: time.Hour,
	})
	issue := createInteractionIssue(t, handler, "APPRC", "Count the request", "A spec")
	counter.take()

	response := countedApprovalRequest(t, handler, issue.PrimaryArtifactID)
	counted := counter.take()
	if response.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", response.Code, response.Body.String())
	}
	const want = 21
	if len(counted) != want {
		t.Fatalf("the approval request made %d round trips, want %d:\n%s",
			len(counted), want, strings.Join(roundTripLines(counted), "\n"))
	}
}

// roundTripLines is each counted round trip's first words, numbered, so a failure names what the
// request sent without printing whole statements.
func roundTripLines(counted []string) []string {
	lines := make([]string, len(counted))
	for index, statement := range counted {
		first := statement
		for _, line := range strings.Split(statement, "\n") {
			if first = strings.TrimSpace(line); first != "" {
				break
			}
		}
		lines[index] = strconv.Itoa(index+1) + ". " + first
	}
	return lines
}
