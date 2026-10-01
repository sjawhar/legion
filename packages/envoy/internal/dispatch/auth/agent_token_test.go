package auth

import (
	"net/http/httptest"
	"testing"
)

func TestBearerToken(t *testing.T) {
	for _, test := range []struct {
		name string
		// authorization is the header's value; empty sends no Authorization header.
		authorization string
		token         string
		present       bool
	}{
		{name: "no header", authorization: "", token: "", present: false},
		{name: "a blank header", authorization: " \t ", token: "", present: false},
		{name: "a bearer", authorization: "Bearer agent-token", token: "agent-token", present: true},
		{name: "a bearer in whitespace", authorization: " Bearer agent-token\t", token: "agent-token", present: true},
		{name: "an empty bearer", authorization: "Bearer ", token: "", present: true},
		{name: "a lowercase scheme", authorization: "bearer agent-token", token: "", present: true},
		{name: "two spaces after the scheme", authorization: "Bearer  agent-token", token: " agent-token", present: true},
		{name: "a tab after the scheme", authorization: "Bearer\tagent-token", token: "", present: true},
		{name: "no space after the scheme", authorization: "Beareragent-token", token: "", present: true},
		{name: "no scheme", authorization: "agent-token", token: "", present: true},
		{name: "another scheme", authorization: "Basic x", token: "", present: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", "/", nil)
			if test.authorization != "" {
				request.Header.Set("Authorization", test.authorization)
			}
			token, present := BearerToken(request)
			if token != test.token || present != test.present {
				t.Fatalf("BearerToken(Authorization %q) = (%q, %t), want (%q, %t)", test.authorization, token, present, test.token, test.present)
			}
		})
	}
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
		{name: "missing shared token and empty bearer", configured: "", token: "", want: false},
		{name: "empty bearer", configured: "agent-token", token: "", want: false},
		{name: "truncated bearer", configured: "agent-token", token: "agent-toke", want: false},
		{name: "longer bearer", configured: "agent-token", token: "agent-tokenx", want: false},
		{name: "different bearer", configured: "agent-token", token: "agent-tokem", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := MatchesSharedAgentToken(test.token, test.configured); got != test.want {
				t.Fatalf("MatchesSharedAgentToken(%q, %q) = %t, want %t", test.token, test.configured, got, test.want)
			}
		})
	}
}
