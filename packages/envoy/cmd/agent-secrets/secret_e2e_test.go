// packages/envoy/cmd/agent-secrets/secret_e2e_test.go
//
// The secret forms against the real broker (brokertest.NewRig: real handlers, real Postgres), its
// secrets.Local standing in for Secrets Manager as the CLI's AWS. They skip without
// BROKER_TEST_DATABASE_URL, as every rig test does.
package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// secretsLocalWithoutValue is an agent-tier secret owner holds with no value, which the broker
// refuses as no-current-value.
func secretsLocalWithoutValue(name, owner string) secrets.LocalSecret {
	return policytest.Secret(name, owner, policy.TierAgent, "")
}

// TestSecretCreateIsServedByTheBrokerAtOnce drives Done-when 1's broker half end to end: the CLI
// writes to (fake) AWS and the broker serves the secret on its very next read, no reload.
func TestSecretCreateIsServedByTheBrokerAtOnce(t *testing.T) {
	rig := brokertest.NewRig(t)
	t.Setenv("AGENT_SECRETS_URL", rig.URL)
	restore := awsClients
	awsClients = func(context.Context, string, string) (secretsAPI, stsAPI, error) {
		return rig.Secrets, fakeSTS{account: "111122223333", arn: "arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc/" + rig.Operator}, nil
	}
	t.Cleanup(func() { awsClients = restore })
	stdin := secretStdin
	secretStdin = strings.NewReader("fresh-v1")
	t.Cleanup(func() { secretStdin = stdin })
	var out, errOut bytes.Buffer
	if code := cmdSecret([]string{"create", "FRESH_KEY", "--owner", "me", "--tier", "agent"}, &out, &errOut); code != 0 {
		t.Fatalf("create: exit %d, stderr %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "broker: serving FRESH_KEY") {
		t.Fatalf("stdout %q must confirm the broker serves it", out.String())
	}
}

// TestSecretDeleteAndRestoreAreServedByTheBrokerAtOnce: the broker stops serving a secret the
// moment the CLI deletes it, and serves it again the moment the CLI restores it, each on the
// CLI's own reread; a retag the broker would refuse is reported, and exits 1.
func TestSecretDeleteAndRestoreAreServedByTheBrokerAtOnce(t *testing.T) {
	rig := brokertest.NewRig(t)
	t.Setenv("AGENT_SECRETS_URL", rig.URL)
	useAWS(t, rig.Secrets, testAccount, "arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc/"+rig.Operator)
	for _, step := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"delete", "TEST_AUTO_SECRET"}, 0, "broker: TEST_AUTO_SECRET is deleted (restorable until "},
		{[]string{"restore", "TEST_AUTO_SECRET"}, 0, "broker: serving TEST_AUTO_SECRET"},
		{[]string{"retag", "TEST_AUTO_SECRET", "--tier", "human"}, 0, "broker: serving TEST_AUTO_SECRET"},
	} {
		stdout, stderr, code := runSecret(step.args...)
		if code != step.code || !strings.Contains(stdout, step.want) {
			t.Fatalf("secret %s: exit %d, stdout %q, stderr %q; want %d with %q", strings.Join(step.args, " "), code, stdout, stderr, step.code, step.want)
		}
	}
	// A secret Secrets Manager holds with no value is one the broker refuses: the CLI's write
	// stands and the broker's refusal is reported, exit 1.
	rig.Secrets.Put(secretsLocalWithoutValue("NO_VALUE_KEY", rig.Operator))
	stdout, stderr, code := runSecret("retag", "NO_VALUE_KEY", "--tier", "human")
	if code != 1 || !strings.Contains(stdout, "broker: refusing NO_VALUE_KEY (reason=no-current-value)") {
		t.Fatalf("retag of a secret with no value: exit %d, stdout %q, stderr %q; want 1 with the broker's refusal", code, stdout, stderr)
	}
}
