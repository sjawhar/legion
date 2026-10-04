// The Stage 4a agent-secrets checks' pure logic: whether this run's broker inputs are
// set at all, and the shell-quoting `agent-secrets <args>` runs through kubectl exec with. None of
// it touches a cluster, so — unlike live_test.go and live_secrets_test.go, both `//go:build e2e` —
// this file carries no build tag: ordinary `go test ./...` compiles and exercises it directly
// (live_secrets_unit_test.go). live_secrets_test.go's secretsBlocked is a thin e2e-tagged wrapper
// over agentSecretsBlockReason below, reading the fields off *liveRig it needs.
package sandbox

import (
	"strings"
)

// agentSecretsBlockReason is secretsBlocked's logic: the four broker inputs every secrets-* check
// needs (the production broker's URL, the email of the person this run's machine login is
// approved by, the automatic rule's dummy value's hash, and the checkout's agent-secrets binary),
// named for whichever are unset. Empty means the run is not blocked.
func agentSecretsBlockReason(url, operator, autoSHA, bin string) string {
	var missing []string
	for name, value := range map[string]string{
		"LEGION_E2E_AGENT_SECRETS_URL": url, "LEGION_E2E_AGENT_SECRETS_OPERATOR": operator,
		"LEGION_E2E_AGENT_SECRETS_AUTO_SHA256": autoSHA, "LEGION_E2E_AGENT_SECRETS_BIN": bin,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return "no broker for this run: " + strings.Join(missing, ", ") + " unset (the production broker with Plan D's rules, and an attended machine login approved on the Dispatch credential page, supply them)"
}

// shellJoin quotes args for a POSIX shell -c string, the way (*liveRig).agentSecrets builds the
// command kubectl exec runs in a pod's container.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}
