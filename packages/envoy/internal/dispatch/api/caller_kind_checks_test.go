// caller_kind_checks_test.go holds the checks the spec's "Checks that read the caller's kind"
// names, each proven for a person's own credential: a credential holding the person scope acts
// on them as the person at a browser does, because it is that person's own credential acting for
// them (INFRA-1017, R1). The route table's own rows are proven in routes_table_test.go and
// acting_agent_test.go; these are the checks inside handlers, which no route gate decides:
// taking a claim away from its holder and releasing anyone's claim (issue_claim.go,
// claimBlockedBy), following or unfollowing an ask for any session (ask_followers.go), rewording
// any ask (asks.go), and reading any direct conversation (messages.go).
//
// Each row drives the browser's own call first, so a failure says the person's credential was
// refused what the person was allowed, rather than that the action is refused everyone.
package api

import (
	"net/http"
	"testing"
)

// personalTokenActorBody is the body a person's own token sends on a route that takes an actor:
// the acting session, which the credential proves nowhere else.
func personalTokenActorBody(fields map[string]any) map[string]any {
	body := map[string]any{"actor": map[string]any{"kind": "session", "id": actingSession}}
	for name, value := range fields {
		body[name] = value
	}
	return body
}

// Taking a live holder's claim and releasing anyone's claim are the person's to make, so the
// person's own credential makes them: the claim route's `force` field already reads the person
// scope (requireCallerScope), and the rule the field feeds (claimBlockedBy) must read the same
// credential rather than the kind of the actor the write records, which for a bearer is always
// the session it acts for.
func TestAPersonsTokenForcesAndReleasesAClaimAsThePersonDoes(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	token := mintPersonalToken(t, handler, "alice", actingTokenName, nil)

	forced := createInteractionIssue(t, handler, "KND", "Forcing a claim", "A spec")
	if claimed := dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+forced.Key+"/claim", map[string]any{}, "bob"); claimed.Code != http.StatusOK {
		t.Fatalf("bob claims %s in a browser: status=%d body=%s", forced.Key, claimed.Code, claimed.Body.String())
	}
	took := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+forced.Key+"/claim",
		personalTokenActorBody(map[string]any{"force": true}), token.Token)
	if took.Code != http.StatusOK {
		t.Fatalf("alice's own token forces bob's claim on %s: status=%d body=%s, want 200 as alice's browser gets",
			forced.Key, took.Code, took.Body.String())
	}

	released := createInteractionIssue(t, handler, "KND", "Releasing a claim", "A spec")
	if claimed := dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+released.Key+"/claim", map[string]any{}, "bob"); claimed.Code != http.StatusOK {
		t.Fatalf("bob claims %s in a browser: status=%d body=%s", released.Key, claimed.Code, claimed.Body.String())
	}
	gave := agentRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+released.Key+"/claim",
		personalTokenActorBody(nil), token.Token)
	if gave.Code != http.StatusOK {
		t.Fatalf("alice's own token releases bob's claim on %s: status=%d body=%s, want 200 as alice's browser gets",
			released.Key, gave.Code, gave.Body.String())
	}
}

// Following an ask for another session is a person's act - they put a session on an ask so its
// answer reaches it - so the person's own credential makes it, where a session that is nobody's
// person may still follow only itself.
func TestAPersonsTokenFollowsAnAskForAnotherSession(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "KND", "Following for a session", "A spec")
	token := mintPersonalToken(t, handler, "alice", actingTokenName, nil)
	askID := openAskAs(t, handler, issue.Key, "session-asking", "Which option?")

	if followed := dispatchRequest(t, handler, http.MethodPut,
		"/api/v1/asks/"+askID+"/followers/session-watching", map[string]any{}, "alice"); followed.Code != http.StatusNoContent {
		t.Fatalf("alice follows the ask for another session in a browser: status=%d body=%s", followed.Code, followed.Body.String())
	}
	byToken := agentRequest(t, handler, http.MethodPut,
		"/api/v1/asks/"+askID+"/followers/session-watching-too", personalTokenActorBody(nil), token.Token)
	if byToken.Code != http.StatusNoContent {
		t.Fatalf("alice's own token follows the ask for another session: status=%d body=%s, want 204 as alice's browser gets",
			byToken.Code, byToken.Body.String())
	}
}

// Rewording an ask somebody else asked is a person's act, so the person's own credential makes
// it: a session that is nobody's person still rewords only its own ask (NOT_AUTHOR).
func TestAPersonsTokenRewordsAnAskItDidNotAsk(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "KND", "Rewording an ask", "A spec")
	token := mintPersonalToken(t, handler, "alice", actingTokenName, nil)
	askID := openAskAs(t, handler, issue.Key, "session-asking", "Which option?")

	if reworded := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID,
		map[string]any{"question": "Which option, as the person words it?"}, "alice"); reworded.Code != http.StatusOK {
		t.Fatalf("alice rewords the ask in a browser: status=%d body=%s", reworded.Code, reworded.Body.String())
	}
	byToken := agentRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID,
		personalTokenActorBody(map[string]any{"question": "Which option, as her agent words it for her?"}), token.Token)
	if byToken.Code != http.StatusOK {
		t.Fatalf("alice's own token rewords the ask: status=%d body=%s, want 200 as alice's browser gets",
			byToken.Code, byToken.Body.String())
	}
}

// A direct conversation is the person's own, so the person's own credential reads it as they do:
// whole, without naming a session in it. The session rule stands for a credential that is
// nobody's person, which names its session and reads only a conversation it is in.
func TestAPersonsTokenReadsTheirOwnDirectConversation(t *testing.T) {
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	token := mintPersonalToken(t, handler, "alice", actingTokenName, nil)

	own := postAgentMessage(t, handler, "s1", map[string]any{
		"body": "How far is the backfill?", "delivery": "aside",
	}, "alice")
	if own.Code != http.StatusCreated {
		t.Fatalf("alice sends a direct message in a browser: status=%d body=%s", own.Code, own.Body.String())
	}
	messageID := decodeBody[struct {
		ID string `json:"id"`
	}](t, own).ID

	if read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/messages/"+messageID, nil, "alice"); read.Code != http.StatusOK {
		t.Fatalf("alice reads her own conversation in a browser: status=%d body=%s", read.Code, read.Body.String())
	}
	byToken := agentRequest(t, handler, http.MethodGet, "/api/v1/messages/"+messageID, nil, token.Token)
	if byToken.Code != http.StatusOK {
		t.Fatalf("alice's own token reads her own conversation: status=%d body=%s, want 200 as alice's browser gets",
			byToken.Code, byToken.Body.String())
	}
}
