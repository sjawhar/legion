package api

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

type agentTokenResponse struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	Token      string     `json:"token"`
}

type agentTokenRow struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

func TestMatchesSharedAgentToken(t *testing.T) {
	for _, test := range []struct {
		name       string
		configured string
		token      string
		want       bool
	}{
		{name: "exact shared token", configured: "agent-token", token: "agent-token", want: true},
		{name: "missing shared token", configured: "", token: "agent-token", want: false},
		{name: "truncated bearer", configured: "agent-token", token: "agent-toke", want: false},
		{name: "different bearer", configured: "agent-token", token: "agent-tokem", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := matchesSharedAgentToken(test.token, test.configured); got != test.want {
				t.Fatalf("matchesSharedAgentToken(%q, %q) = %t, want %t", test.token, test.configured, got, test.want)
			}
		})
	}
}

func TestAgentTokensArePrivateRevocableAndAttributeSessionWrites(t *testing.T) {
	handler := newTestHandler(t)

	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	issueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Token attribution",
	}, "alice")
	if issueResponse.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", issueResponse.Code, issueResponse.Body.String())
	}
	for _, test := range []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{name: "list", method: http.MethodGet, path: "/api/v1/me/agent-tokens"},
		{name: "mint", method: http.MethodPost, path: "/api/v1/me/agent-tokens", body: map[string]string{"name": "agent"}},
		{name: "revoke", method: http.MethodDelete, path: "/api/v1/me/agent-tokens/00000000-0000-0000-0000-000000000000"},
	} {
		t.Run("agent cannot "+test.name, func(t *testing.T) {
			response := agentRequest(t, handler, test.method, test.path, test.body, "agent-token")
			if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"HUMAN_ONLY"`) {
				t.Fatalf("%s agent token endpoint: status=%d body=%s", test.name, response.Code, response.Body.String())
			}
		})
	}
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, issueResponse)

	mintedResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{
		"name": "architect",
	}, "alice")
	if mintedResponse.Code != http.StatusCreated {
		t.Fatalf("mint agent token: status=%d body=%s", mintedResponse.Code, mintedResponse.Body.String())
	}
	minted := decodeBody[agentTokenResponse](t, mintedResponse)
	if !strings.HasPrefix(minted.Token, "dsp_") || len(minted.Token) != len("dsp_")+43 {
		t.Fatalf("minted token = %q, want dsp_ plus 43 base64url characters", minted.Token)
	}
	if minted.ID == "" || minted.Name != "architect" || minted.Prefix != minted.Token[len("dsp_"):len("dsp_")+8] || minted.CreatedAt.IsZero() || minted.LastUsedAt != nil || minted.RevokedAt != nil {
		t.Fatalf("minted token row = %#v", minted)
	}

	aliceTokens := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agent-tokens", nil, "alice")
	if aliceTokens.Code != http.StatusOK {
		t.Fatalf("list alice agent tokens: status=%d body=%s", aliceTokens.Code, aliceTokens.Body.String())
	}
	listed := decodeBody[[]agentTokenRow](t, aliceTokens)
	if len(listed) != 1 || listed[0].ID != minted.ID || listed[0].Name != "architect" || listed[0].Prefix != minted.Prefix {
		t.Fatalf("alice agent tokens = %#v", listed)
	}

	bobTokens := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agent-tokens", nil, "bob")
	if bobTokens.Code != http.StatusOK {
		t.Fatalf("list bob agent tokens: status=%d body=%s", bobTokens.Code, bobTokens.Body.String())
	}
	if got := decodeBody[[]agentTokenRow](t, bobTokens); len(got) != 0 {
		t.Fatalf("bob agent tokens = %#v, want none", got)
	}

	bobRevoke := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/me/agent-tokens/"+minted.ID, nil, "bob")
	if bobRevoke.Code != http.StatusNotFound {
		t.Fatalf("bob revokes alice token: status=%d body=%s", bobRevoke.Code, bobRevoke.Body.String())
	}

	askResponse := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which option?",
		"actor": map[string]any{
			"kind": "session", "id": "architect-session", "owner": "mallory",
		},
	}, minted.Token)
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create ask with personal token: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	ask := decodeBody[struct {
		Author struct {
			Kind  string  `json:"kind"`
			ID    string  `json:"id"`
			Owner *string `json:"owner"`
		} `json:"author"`
	}](t, askResponse)
	if ask.Author.Kind != "session" || ask.Author.ID != "architect-session" || ask.Author.Owner == nil || *ask.Author.Owner != "alice" {
		t.Fatalf("personal-token ask author = %#v, want session owned by alice", ask.Author)
	}

	usedTokens := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agent-tokens", nil, "alice")
	used := decodeBody[[]agentTokenRow](t, usedTokens)
	if len(used) != 1 || used[0].LastUsedAt == nil {
		t.Fatalf("personal-token use did not record last_used_at: %#v", used)
	}
	firstUsedAt := *used[0].LastUsedAt
	if response := agentRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, minted.Token); response.Code != http.StatusOK {
		t.Fatalf("read with personal token: status=%d body=%s", response.Code, response.Body.String())
	}
	usedTokens = dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agent-tokens", nil, "alice")
	used = decodeBody[[]agentTokenRow](t, usedTokens)
	if len(used) != 1 || used[0].LastUsedAt == nil || !used[0].LastUsedAt.Equal(firstUsedAt) {
		t.Fatalf("last_used_at updated more than once per minute: first=%s second=%v", firstUsedAt, used)
	}

	sharedAsk := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Does shared fallback work?",
		"actor": map[string]any{
			"kind": "session", "id": "shared-session", "owner": "mallory",
		},
	}, "agent-token")
	if sharedAsk.Code != http.StatusCreated {
		t.Fatalf("create ask with shared token: status=%d body=%s", sharedAsk.Code, sharedAsk.Body.String())
	}
	shared := decodeBody[struct {
		Author struct {
			Owner *string `json:"owner"`
		} `json:"author"`
	}](t, sharedAsk)
	if shared.Author.Owner != nil {
		t.Fatalf("shared-token ask owner = %q, want absent", *shared.Author.Owner)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		revoked := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/me/agent-tokens/"+minted.ID, nil, "alice")
		if revoked.Code != http.StatusNoContent {
			t.Fatalf("revoke token attempt %d: status=%d body=%s", attempt, revoked.Code, revoked.Body.String())
		}
	}
	revokedTokens := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agent-tokens", nil, "alice")
	if got := decodeBody[[]agentTokenRow](t, revokedTokens); len(got) != 1 || got[0].RevokedAt == nil {
		t.Fatalf("revoked agent token list = %#v", got)
	}

	denied := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "This must not be created.",
		"actor":    map[string]string{"kind": "session", "id": "architect-session"},
	}, minted.Token)
	if denied.Code != http.StatusUnauthorized || !strings.Contains(denied.Body.String(), `"code":"TOKEN_REVOKED"`) {
		t.Fatalf("revoked token write: status=%d body=%s", denied.Code, denied.Body.String())
	}
}
