package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestOneServicesTwoAccountsEachGetItsSecret drives the REAL compiled broker on a real Postgres,
// with a local OIDC issuer standing in for the cluster's service-account token issuer, over HTTP
// alone. BROKER_SERVICES names two accounts for one service, so the broker boots serving the
// secret that service owns instead of refusing its owner tag, which proves its policy loader was
// given the service and not the accounts. The service logs in once with its own token, and its one
// credential enrolls three pods, each proving with its projected token that it runs as its own
// account. The pods running as either of the service's accounts are each granted its secret at
// once, naming no approver, and read its value; the pod running as an account no entry names is
// denied it at once, with no grant, no record and no approver to ask.
func TestOneServicesTwoAccountsEachGetItsSecret(t *testing.T) {
	databaseURL := storetest.URL(t)
	issuer := oidctest.New(t)
	podKey := issuer.PublishKey(t, "signing-key")
	const (
		podAudience  = "example-broker-pod"
		service      = "example-service"
		loginAccount = "system:serviceaccount:example-system:example-service-login"
		accountA     = "system:serviceaccount:example:example-worker-a"
		accountB     = "system:serviceaccount:example-other:example-worker-b"
		accountC     = "system:serviceaccount:example:example-worker-c"
		secretName   = "SERVICE_KEY"
		secretValue  = "service-value"
		kmsKey       = "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
	)
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	fakeSecrets := `{"secrets": [
		{"name": "example/agent-secrets/service-key", "kms_key_id": "` + kmsKey + `", "tags": {"owner": "` + service + `", "tier": "agent"}, "value": "` + secretValue + `"}
	]}`
	if err := os.WriteFile(fakeSecretsFile, []byte(fakeSecrets), 0o600); err != nil {
		t.Fatal(err)
	}

	addr, boot, _ := startBroker(t,
		"BROKER_DATABASE_URL="+databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE="+fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER="+issuer.URL(),
		"BROKER_K8S_OIDC_AUDIENCE="+podAudience,
		"BROKER_SERVICE_LOGINS="+service+"="+loginAccount,
		"BROKER_SERVICES="+service+"="+accountA+" "+service+"="+accountB,
	)
	requireHealthy(t, addr)
	if strings.Contains(boot, "agent secret policy refused") || strings.Contains(boot, policy.ReasonOwnerTagMalformed) {
		t.Fatalf("boot log refuses the secret %s owns, which BROKER_SERVICES registers for two accounts:\n%s", service, boot)
	}
	broker := podBroker{t: t, base: "http://" + addr, issuer: issuer, key: podKey, audience: podAudience}

	t.Log("--- the service logs in once with its own token")
	launcher := broker.logIn(service, loginAccount)

	t.Log("--- the one credential enrolls a pod as each of accounts A, B and C")
	granted := []struct {
		name string
		pod  pod
	}{{"A", broker.enroll(launcher, "pod-a", accountA)}, {"B", broker.enroll(launcher, "pod-b", accountB)}}
	podC := broker.enroll(launcher, "pod-c", accountC)

	for _, p := range granted {
		t.Logf("--- pod %s, running as one of the service's accounts, is granted its secret at once", p.name)
		reply := broker.ask(p.pod, secretName)
		if reply.State != "granted" || reply.GrantID == nil || reply.RecordID != nil || string(reply.Approver) != "null" {
			t.Fatalf("pod %s's request = %+v (approver %s), want granted with a grant, no record and approver null", p.name, reply, reply.Approver)
		}
		if values := broker.values(p.pod, *reply.GrantID); values[secretName] != secretValue {
			t.Fatalf("pod %s's grant values = %v, want %s=%s", p.name, values, secretName, secretValue)
		}
	}

	t.Log("--- pod C, running as an account no entry names, is denied it at once with no one to ask")
	reply := broker.ask(podC, secretName)
	if reply.State != "denied" || reply.GrantID != nil || reply.RecordID != nil || string(reply.Approver) != "null" {
		t.Fatalf("pod C's request = %+v (approver %s), want denied with no grant, no record and approver null", reply, reply.Approver)
	}
}
