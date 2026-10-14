package api

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The version a human approved is held to the asks rows as the version his answer writes is
// (docs.carryApproval, LEGION-752). A stored version whose decided block shows something other
// than its row - a selection, an answer, a copy's provenance, or a question other than the one
// its row was decided on - approved inside the settle window, before settlement wrote the row
// back over it, is not a version whose blocks are as the rows have them: that block is a change
// of the new version's, and his later answer to another block is not the whole change since what
// he approved, so nothing is carried. The same sequence over a stored version whose block is as
// its row has it carries, so what the stored block shows is what refuses.
//
// Each test approves inside the window a live write opens, so each guards the race it could lose:
// the review route pins the latest version, and a settlement that fires first writes the row back
// and moves the latest on. The review's version must be the one the test meant, and that
// version's markdown must show what the test put there, or the test fails as setup rather than
// passing over a version that was as its row has it all along.

// approveVersion approves the document as login through the review route, which pins its latest
// version, and fails t unless that is version number: the version the test meant.
func approveVersion(t *testing.T, handler http.Handler, artifactID, login string, number int) {
	t.Helper()
	review := reviewDocument(t, handler, artifactID, login, "approved", "")
	if review.Version != number {
		t.Fatalf("%s approved version %d, want version %d: settlement wrote the row back before the approval landed", login, review.Version, number)
	}
}

// Stale: the version he approved shows a server-owned attribute other than its row's on the block
// he answered - a selection, an answer, or a copy's provenance (askServerAttributeNames; `invalid`
// no rendering writes, and a state, answerer or time other than the row's were refused on the
// approved version before this). The human answered the first block, and a write through the
// server's own live attribute path put another value on it, which the agent named a version of;
// the human approved that version inside the settle window, and settlement then wrote the row back
// over it, which carries nothing (he answered the block before approving). The agent adds a second
// block and the human answers it: the cut leaves both blocks out and every block is his, but the
// first is not as the version he approved held it - that version showed a decision other than
// the row's - and his answer to it predates his approval, so nothing is carried. The row-true
// control, which approves the version settlement wrote from the row, carries.
func TestAnApprovedVersionShowingADecisionOtherThanItsRowsCarriesNothing(t *testing.T) {
	for _, tc := range []struct {
		attribute string
		shown     any
		rendered  string // what version 3's markdown shows for the attribute
	}{
		{"selected", []string{"West"}, `selected="[&#x22;West&#x22;]"`},
		{"answer", "Pick West.", `answer="Pick West."`},
		{"copied_from", "00000000-0000-0000-0000-000000000000", `copied_from="00000000-0000-0000-0000-000000000000"`},
		{"copied_from_document", "dispatch://TEST/artifact/other", `copied_from_document="dispatch://TEST/artifact/other"`},
	} {
		t.Run(tc.attribute, func(t *testing.T) {
			handler, database, documents := settlingCarryDocuments(t, 2*time.Second)
			issue := createInteractionIssue(t, handler, "TEST", "Approved "+tc.attribute+" other than the row's", "Context\n\n"+regionAsk)
			asks := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, map[string]string{"region": "Which region goes first?"})
			answerDecisionBlock(t, handler, asks["region"], "alice", []string{"East"}, "", nil)
			waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 2)

			setLiveBlockAttributes(t, database, documents, issue.PrimaryArtifactID, "region", map[string]any{tc.attribute: tc.shown})
			named := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions", map[string]any{"summary": "Name the " + tc.attribute + " other than the row's", "actor": sessionActor()})
			if named.Code != http.StatusCreated {
				t.Fatalf("name the version showing the %s other than the row's: status=%d body=%s", tc.attribute, named.Code, named.Body.String())
			}
			waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 3)
			shown := versionMarkdown(t, handler, issue.PrimaryArtifactID, 3)
			if !strings.Contains(shown, tc.rendered) || !strings.Contains(shown, `answered_by="alice"`) {
				t.Fatalf("version 3 = %s, want it to show the %s other than the row's on the block alice answered", shown, tc.attribute)
			}
			approveVersion(t, handler, issue.PrimaryArtifactID, "alice", 3)

			waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 4)
			restored := versionMarkdown(t, handler, issue.PrimaryArtifactID, 4)
			if !strings.Contains(restored, `selected="[&#x22;East&#x22;]"`) || strings.Contains(restored, tc.rendered) {
				t.Fatalf("version 4 = %s, want settlement to have written the row back over the %s", restored, tc.attribute)
			}
			if got := readApproval(t, handler, issue.PrimaryArtifactID); got.Approval.State != "stale" {
				t.Fatalf("approval after settlement wrote the row back = %#v, want stale", got.Approval)
			}
			for id, ask := range insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 5, map[string]string{"wave": "How many regions take the first wave?"}) {
				asks[id] = ask
			}

			answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
			waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 6)
			requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
		})
	}
	t.Run("row-true", func(t *testing.T) {
		handler := settlingCarryHandler(t, 2*time.Second)
		issue := createInteractionIssue(t, handler, "TEST", "Approved as the row has it", "Context\n\n"+regionAsk)
		asks := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, map[string]string{"region": "Which region goes first?"})
		answerDecisionBlock(t, handler, asks["region"], "alice", []string{"East"}, "", nil)
		waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 2)
		approveVersion(t, handler, issue.PrimaryArtifactID, "alice", 2)
		for id, ask := range insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 3, map[string]string{"wave": "How many regions take the first wave?"}) {
			asks[id] = ask
		}

		answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
		waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 4)
		requireCarriedReview(t, handler, issue.PrimaryArtifactID, 4, 2, nil)
	})
}

// Stale: the version he approved shows the block he answered under wording other than its row
// was decided on - the one way an agent reaches this over HTTP alone. The human answered the
// first block; the agent reworded its question through the edits route, which versions in the
// route's own transaction; the human approved that version inside the settle window, and
// settlement then wrote the decided wording back, which carries nothing (he answered the block
// before approving). The agent adds a second block and the human answers it: the cut leaves both
// blocks out and every block is his, but the version he approved showed the first under words
// nobody decided, so the block is not as he approved it, and his answer to it predates his
// approval: nothing is carried.
func TestAnApprovedVersionShowingADecidedBlockUnderOtherWordingCarriesNothing(t *testing.T) {
	handler := settlingCarryHandler(t, 2*time.Second)
	issue := createInteractionIssue(t, handler, "TEST", "Approved under other wording", "Context\n\n"+regionAsk)
	asks := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, map[string]string{"region": "Which region goes first?"})
	answerDecisionBlock(t, handler, asks["region"], "alice", []string{"East"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 2)

	reviseDocument(t, handler, issue.PrimaryArtifactID, "Which region goes first?", "Which region goes last?", 3)
	reworded := versionMarkdown(t, handler, issue.PrimaryArtifactID, 3)
	if !strings.Contains(reworded, "Which region goes last?") || !strings.Contains(reworded, `answered_by="alice"`) {
		t.Fatalf("version 3 = %s, want it to hold the agent's reword under alice's answer", reworded)
	}
	approveVersion(t, handler, issue.PrimaryArtifactID, "alice", 3)

	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 4)
	restored := versionMarkdown(t, handler, issue.PrimaryArtifactID, 4)
	if !strings.Contains(restored, "Which region goes first?") || strings.Contains(restored, "goes last?") {
		t.Fatalf("version 4 = %s, want settlement to have written the decided wording back", restored)
	}
	if got := readApproval(t, handler, issue.PrimaryArtifactID); got.Approval.State != "stale" {
		t.Fatalf("approval after settlement wrote the wording back = %#v, want stale", got.Approval)
	}
	for id, ask := range insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 5, map[string]string{"wave": "How many regions take the first wave?"}) {
		asks[id] = ask
	}

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 6)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
}
