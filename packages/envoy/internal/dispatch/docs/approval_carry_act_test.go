package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// An answer an agent made for its person carries that person's approval forward only when the
// agent could have approved in the first place: the act's own scopes decide it. A credential the
// person narrowed to the person scope alone may answer their questions and may not settle what
// they settle, so an answer it made is no ground for carrying an approval - not its own block's,
// and not another block's the person answered beside it. The scopes read are the act's, written
// when the answer was made, so what a token holds today decides nothing about what it did; and an
// answer stored before acts existed, whose JSON carries no act key at all, carries as it always
// did.

// actAnswerSpec is the two-block document these tests decide: region, the block an agent answers,
// and wave, the block the person answers beside it.
const actAnswerSpec = "Context\n\n" +
	":::ask{#region urgency=\"low\" multiple=\"false\"}\nWhich region goes first?\n\n- East: The quiet one.\n- West: The busy one.\n:::\n\n" +
	":::ask{#wave urgency=\"low\" multiple=\"false\"}\nHow many regions take the first wave?\n\n- One: One region.\n- Two: Two regions.\n:::\n"

// seededActDocument creates actAnswerSpec as a document, starts a service that settles a live
// write, seeds the document's live text and waits until settlement has indexed both blocks as
// asks of the document - the rows the carry reads.
//
// The settle is a second, as the other carry tests that answer and then name a version use
// (approval_carry_approved_test.go), and not the 20 ms a test that only seeds can afford: a
// settlement firing while one of these tests holds its joined transaction takes the document's
// rows in the other order and the two deadlock, which `-race` is slow enough to hit. The seeding
// below schedules one settlement itself and waits up to three seconds for it, so a second costs
// the seed nothing.
func seededActDocument(t *testing.T) (*store.Store, *Service, string) {
	t.Helper()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, actAnswerSpec)
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Second})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, actAnswerSpec)
	service.ScheduleSettlement(artifactID)
	waitFor(t, 3*time.Second, "settlement to index both blocks", func() bool {
		var rows int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from asks where block_artifact_id = $1 and block_id = any($2::text[])
		`, artifactID, []string{"region", "wave"}).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows == 2
	})
	return database, service, artifactID
}

// answerActBlock writes answer as the answer of the block ask blockID of artifactID - the row's
// state and answer, and the block's attributes through the service's own live attribute write, in
// one joined transaction, as answerBlockAs does - storing stored as the row's answer JSON. The
// stored JSON is the test's own, so a test can store exactly what a release wrote: an answer
// carrying an act, and one from before acts existed, carrying no such key.
func answerActBlock(t *testing.T, service *Service, artifactID, blockID string, answer model.AskAnswer, stored []byte) {
	t.Helper()
	if stored == nil {
		encoded, err := json.Marshal(answer)
		if err != nil {
			t.Fatal(err)
		}
		stored = encoded
	}
	joined(t, service, artifactID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			update asks set state = 'answered', answer = $3 where block_artifact_id = $1 and block_id = $2
		`, artifactID, blockID, stored); err != nil {
			return err
		}
		return service.SetBlockAttributes(ctx, artifactID, blockID, map[string]any{
			"state": "answered", "answered_by": answer.User,
			"answered_at": answer.At.UTC().Format(time.RFC3339Nano),
			"selected":    answer.Selected, "answer": nil,
		}, model.Actor{Kind: "user", ID: answer.User})
	})
}

// answerWithAct is the answer login gave now, carrying act.
func answerWithAct(login string, selected []string, act *model.ActorAct) model.AskAnswer {
	return model.AskAnswer{User: login, Selected: selected, At: time.Now().UTC(), Act: act}
}

// nameVersion writes a new version of the document as the agent that edits it, the write the
// carry runs inside.
func nameVersion(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	joined(t, service, artifactID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := service.NamedVersion(ctx, artifactID, "Named by the agent", model.Actor{Kind: "session", ID: "session-0123456789abcdef"})
		return err
	})
}

// An answer made under an act that does not hold the approve scope carries no approval, and one
// whose act holds it carries, everything else about the two writes being the same. A person's own
// answer beside an agent's narrow one carries nothing either: the carry counts every block that
// changed since the approval, and one of them was decided by a credential that may not approve.
func TestAnActWithoutApproveCarriesNoApproval(t *testing.T) {
	for _, test := range []struct {
		name     string
		scopes   []string
		reviews  int
		approved int
	}{
		{name: "an act holding person alone", scopes: []string{model.ScopePerson}, reviews: 1, approved: 0},
		{name: "an act holding approve", scopes: []string{model.ScopePerson, model.ScopeApprove}, reviews: 2, approved: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, service, artifactID := seededActDocument(t)
			approveStoredVersion(t, database, artifactID, 1, "alice")
			act := &model.ActorAct{Session: "session-agent", TokenID: "token-1", TokenName: "architect", Scopes: test.scopes}

			answerActBlock(t, service, artifactID, "region", answerWithAct("alice", []string{"East"}, act), nil)
			answerActBlock(t, service, artifactID, "wave", answerWithAct("alice", []string{"One"}, act), nil)
			nameVersion(t, service, artifactID)

			if reviews, approvals := carriedReviews(t, database, artifactID); reviews != test.reviews || approvals != test.approved {
				t.Fatalf("reviews = %d, artifact.approved events = %d; want %d and %d",
					reviews, approvals, test.reviews, test.approved)
			}
		})
	}

	t.Run("a narrow agent's answer beside the person's own", func(t *testing.T) {
		database, service, artifactID := seededActDocument(t)
		approveStoredVersion(t, database, artifactID, 1, "alice")
		narrow := &model.ActorAct{Session: "session-agent", TokenID: "token-1", TokenName: "planner", Scopes: []string{model.ScopePerson}}

		answerActBlock(t, service, artifactID, "region", answerWithAct("alice", []string{"East"}, narrow), nil)
		// The person answers the other block themselves, at a browser, which carries no act.
		answerActBlock(t, service, artifactID, "wave", answerWithAct("alice", []string{"One"}, nil), nil)
		nameVersion(t, service, artifactID)

		if reviews, approvals := carriedReviews(t, database, artifactID); reviews != 1 || approvals != 0 {
			t.Fatalf("reviews = %d, artifact.approved events = %d; want alice's approval of version 1 alone and no event",
				reviews, approvals)
		}
	})
}

// The scopes on an act are what the credential held at the act and are never read back from the
// token, so a token narrowed, revoked or deleted after the answer changes nothing: the approval
// its answer carried still carries at every version written after it.
func TestTheActsScopesAreTheSnapshotTakenWhenTheAnswerWasMade(t *testing.T) {
	database, service, artifactID := seededActDocument(t)
	approveStoredVersion(t, database, artifactID, 1, "alice")
	// The token row as it stands now: narrowed to the person scope and revoked. The act below
	// names this very token and holds approve, as the token did when the answer was made, so a
	// carry that read the token back rather than the act would refuse every case here.
	const tokenID = "00000000-0000-0000-0000-0000000000a1"
	if _, err := database.Pool.Exec(context.Background(), `
		insert into agent_tokens (id, owner, name, token_hash, prefix, scopes, revoked_at)
		values ($1, 'alice', 'architect', $2, 'narrowed', $3, now())
	`, tokenID, []byte("a-hash-of-the-narrowed-token-0001"), []string{model.ScopePerson}); err != nil {
		t.Fatalf("store the narrowed, revoked token: %v", err)
	}
	act := &model.ActorAct{
		Session: "session-agent", TokenID: tokenID, TokenName: "architect",
		Scopes: []string{model.ScopePerson, model.ScopeApprove},
	}

	answerActBlock(t, service, artifactID, "region", answerWithAct("alice", []string{"East"}, act), nil)
	answerActBlock(t, service, artifactID, "wave", answerWithAct("alice", []string{"One"}, act), nil)
	nameVersion(t, service, artifactID)
	if reviews, approvals := carriedReviews(t, database, artifactID); reviews != 2 || approvals != 1 {
		t.Fatalf("reviews = %d, artifact.approved events = %d; want the answers to carry her approval", reviews, approvals)
	}

	// And again at the version after it: the answer's own act decides each carry.
	answerActBlock(t, service, artifactID, "region", answerWithAct("alice", []string{"West"}, act), nil)
	nameVersion(t, service, artifactID)
	if reviews, approvals := carriedReviews(t, database, artifactID); reviews != 3 || approvals != 2 {
		t.Fatalf("reviews = %d, artifact.approved events = %d; want the second answer to carry her approval too",
			reviews, approvals)
	}
}

// An answer stored before acts existed carries no act key at all, and the carry reads it as the
// person's own: the approval carries. The answer is built as the stored JSON a release wrote, not
// as a Go value with a nil field, so what is proved is the decode and not the literal.
func TestAnAnswerStoredBeforeActsExistedStillCarriesTheApproval(t *testing.T) {
	database, service, artifactID := seededActDocument(t)
	approveStoredVersion(t, database, artifactID, 1, "alice")

	for blockID, selected := range map[string]string{"region": "East", "wave": "One"} {
		answer := model.AskAnswer{User: "alice", Selected: []string{selected}, At: time.Now().UTC()}
		stored := fmt.Sprintf(`{"user":"alice","selected":["%s"],"text":null,"at":"%s"}`,
			selected, answer.At.Format(time.RFC3339Nano))
		answerActBlock(t, service, artifactID, blockID, answer, []byte(stored))
	}
	var act *string
	if err := database.Pool.QueryRow(context.Background(), `
		select answer ->> 'act' from asks where block_artifact_id = $1 and block_id = 'region'
	`, artifactID).Scan(&act); err != nil {
		t.Fatalf("read the stored answer: %v", err)
	}
	if act != nil {
		t.Fatalf("the stored answer carries act %q, want no such key", *act)
	}

	nameVersion(t, service, artifactID)
	if reviews, approvals := carriedReviews(t, database, artifactID); reviews != 2 || approvals != 1 {
		t.Fatalf("reviews = %d, artifact.approved events = %d; want the stored answers to carry her approval",
			reviews, approvals)
	}
}
