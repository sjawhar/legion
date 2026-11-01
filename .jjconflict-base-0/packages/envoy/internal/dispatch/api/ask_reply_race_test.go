package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// A reply and an answer race on the same ask. Both take the owner row before either reads the
// ask's state, so one fully precedes the other and the reply lands on exactly one side of the
// transition: recorded under the open ask with a turn, or under the answered ask with none.
// A turn on an ask that was already answered is the failure - it tells a stream consumer the
// ask is waiting on someone when nobody is expected to act.
func TestReplyRacingAnAnswerLandsOnOneSideOfTheTransition(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Reply race", "A spec")

	for round := range 24 {
		askID := openAskAs(t, handler, issue.Key, "s1", "Ship it?")

		var wait sync.WaitGroup
		start := make(chan struct{})
		var reply, answer *httptest.ResponseRecorder
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			reply = sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
				"body":   "Considered.",
				"ask_id": askID,
				"actor":  map[string]any{"kind": "session", "id": "s2"},
			})
		}()
		go func() {
			defer wait.Done()
			<-start
			answer = dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
				"selected": []string{"Yes"},
			}, "alice")
		}()
		close(start)
		wait.Wait()

		if reply.Code != http.StatusCreated {
			t.Fatalf("round %d: reply: status=%d body=%s", round, reply.Code, reply.Body.String())
		}
		if answer.Code != http.StatusOK && answer.Code != http.StatusCreated {
			t.Fatalf("round %d: answer: status=%d body=%s", round, answer.Code, answer.Body.String())
		}

		// The event sequence is allocated under the owner row both writers hold to commit, so
		// it is their commit order. The reply's turn has to agree with the side it committed on.
		var replyFirst bool
		var turn *string
		if err := database.Pool.QueryRow(context.Background(), `
			select (select seq from events where issue_key = $1 and type = 'comment.created'
			        order by seq desc limit 1)
			     < (select seq from events where issue_key = $1 and type = 'ask.answered'
			        order by seq desc limit 1),
			       (select turn from comments where ask_id = $2)
		`, issue.Key, askID).Scan(&replyFirst, &turn); err != nil {
			t.Fatalf("round %d: read outcome: %v", round, err)
		}
		switch {
		case replyFirst && turn == nil:
			t.Fatalf("round %d: reply committed before the answer but recorded no turn", round)
		case !replyFirst && turn != nil:
			t.Fatalf("round %d: reply committed after the ask was answered but recorded turn %q", round, *turn)
		}
	}
}
