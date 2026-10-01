package auth

import "testing"

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
