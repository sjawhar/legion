// Ordinary, always-visible unit tests for the Stage 4a agent-secrets checks' pure logic
// (AGENTC-393), which live_secrets_pure_test.go defines with no e2e build tag: shellJoin's shell
// quoting and agentSecretsBlockReason's blocking-reason string (what live_secrets_test.go's
// secretsBlocked delegates to). The e2e-tagged checks these back (checkSecretsTwoPodsEnrolled and
// friends) need a real cluster and a real broker and stay untestable here, exactly as the plan
// anticipates (SKIPPED-BLOCKED without them).
package sandbox

import (
	"strings"
	"testing"
)

func TestShellJoin(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no args":              {nil, ""},
		"plain args":           {[]string{"self", "--json"}, "'self' '--json'"},
		"single quote in arg":  {[]string{"it's"}, `'it'\''s'`},
		"arg with a space":     {[]string{"s4a cross-issue probe"}, "'s4a cross-issue probe'"},
		"empty string arg":     {[]string{""}, "''"},
		"shell metacharacters": {[]string{"$(rm -rf /)", "a;b"}, `'$(rm -rf /)' 'a;b'`},
	} {
		t.Run(name, func(t *testing.T) {
			if got := shellJoin(tc.args); got != tc.want {
				t.Fatalf("shellJoin(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestAgentSecretsBlockReason(t *testing.T) {
	const url, operator, sha, bin = "https://secrets.internal.trajectorylabs.com", "sami", "deadbeef", "/bin/agent-secrets"
	for name, tc := range map[string]struct {
		url, operator, sha, bin string
		wantBlocked             bool
		wantNames               []string
	}{
		"everything set": {url, operator, sha, bin, false, nil},
		"everything unset": {
			"", "", "", "", true,
			[]string{"LEGION_E2E_AGENT_SECRETS_URL", "LEGION_E2E_AGENT_SECRETS_OPERATOR", "LEGION_E2E_AGENT_SECRETS_AUTO_SHA256", "LEGION_E2E_AGENT_SECRETS_BIN"},
		},
		"only URL unset":      {"", operator, sha, bin, true, []string{"LEGION_E2E_AGENT_SECRETS_URL"}},
		"only operator unset": {url, "", sha, bin, true, []string{"LEGION_E2E_AGENT_SECRETS_OPERATOR"}},
		"only sha unset":      {url, operator, "", bin, true, []string{"LEGION_E2E_AGENT_SECRETS_AUTO_SHA256"}},
		"only bin unset":      {url, operator, sha, "", true, []string{"LEGION_E2E_AGENT_SECRETS_BIN"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := agentSecretsBlockReason(tc.url, tc.operator, tc.sha, tc.bin)
			if (got != "") != tc.wantBlocked {
				t.Fatalf("agentSecretsBlockReason(%q, %q, %q, %q) = %q, want blocked=%t", tc.url, tc.operator, tc.sha, tc.bin, got, tc.wantBlocked)
			}
			for _, name := range tc.wantNames {
				if !strings.Contains(got, name) {
					t.Fatalf("reason %q does not name the unset %s", got, name)
				}
			}
			if !tc.wantBlocked && got != "" {
				t.Fatalf("reason = %q, want empty (nothing missing)", got)
			}
			if tc.wantBlocked {
				const wantTail = "(the production broker with Plan D's rules, and an attended machine login approved on the Dispatch credential page, supply them)"
				if !strings.HasSuffix(got, wantTail) {
					t.Fatalf("reason %q does not end with %q", got, wantTail)
				}
			}
		})
	}
}
