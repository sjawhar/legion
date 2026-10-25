package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestAnAccountProvesOnlyTheServiceItIsListedFor drives the REAL compiled broker on a real
// Postgres, over HTTP alone, with two registered services that each log in with a token of their
// own: example-service runs its pods as two accounts, other-service as one. A pod's service is
// its launcher's only when BROKER_SERVICES lists the pod's account for that same service, so an
// account listed for one service proves nothing under another service's launcher credential: a
// pod enrolled by other-service's credential while running as one of example-service's accounts,
// and a pod enrolled by example-service's credential while running as other-service's account,
// are each denied both services' secrets, with no grant, no record and no approver. The pod
// running as other-service's own account under its credential is granted that service's secret,
// so the denials are the account check and not a service that cannot be served.
func TestAnAccountProvesOnlyTheServiceItIsListedFor(t *testing.T) {
	databaseURL := storetest.URL(t)
	issuer := oidctest.New(t)
	podKey := issuer.PublishKey(t, "signing-key")
	const (
		podAudience  = "example-broker-pod"
		service      = "example-service"
		otherService = "other-service"
		loginAccount = "system:serviceaccount:example-system:example-service-login"
		otherLogin   = "system:serviceaccount:other-system:other-service-login"
		accountA     = "system:serviceaccount:example:example-worker-a"
		accountB     = "system:serviceaccount:example-other:example-worker-b"
		accountD     = "system:serviceaccount:other:other-worker"
		secretName   = "SERVICE_KEY"
		otherSecret  = "OTHER_KEY"
		kmsKey       = "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
	)
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	fakeSecrets := `{"secrets": [
		{"name": "example/agent-secrets/service-key", "kms_key_id": "` + kmsKey + `", "tags": {"owner": "` + service + `", "tier": "agent"}, "value": "service-value"},
		{"name": "example/agent-secrets/other-key", "kms_key_id": "` + kmsKey + `", "tags": {"owner": "` + otherService + `", "tier": "agent"}, "value": "other-value"}
	]}`
	if err := os.WriteFile(fakeSecretsFile, []byte(fakeSecrets), 0o600); err != nil {
		t.Fatal(err)
	}

	addr, _, _ := startBroker(t,
		"BROKER_DATABASE_URL="+databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE="+fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER="+issuer.URL(),
		"BROKER_K8S_OIDC_AUDIENCE="+podAudience,
		"BROKER_SERVICE_LOGINS="+service+"="+loginAccount+" "+otherService+"="+otherLogin,
		"BROKER_SERVICES="+service+"="+accountA+" "+service+"="+accountB+" "+otherService+"="+accountD,
	)
	requireHealthy(t, addr)
	broker := podBroker{t: t, base: "http://" + addr, issuer: issuer, key: podKey, audience: podAudience}

	exampleLauncher := broker.logIn(service, loginAccount)
	otherLauncher := broker.logIn(otherService, otherLogin)

	t.Log("--- other-service's own pod, under its credential, is granted its secret")
	own := broker.ask(broker.enroll(otherLauncher, "pod-other-own", accountD), otherSecret)
	if own.State != "granted" || own.GrantID == nil || own.RecordID != nil || string(own.Approver) != "null" {
		t.Fatalf("other-service's own pod's request = %+v (approver %s), want granted with no record and approver null", own, own.Approver)
	}

	for who, p := range map[string]pod{
		"a pod under other-service's credential running as example-service's account A": broker.enroll(otherLauncher, "pod-other-as-a", accountA),
		"a pod under other-service's credential running as example-service's account B": broker.enroll(otherLauncher, "pod-other-as-b", accountB),
		"a pod under example-service's credential running as other-service's account":   broker.enroll(exampleLauncher, "pod-example-as-d", accountD),
	} {
		for _, name := range []string{secretName, otherSecret} {
			t.Logf("--- %s asks for %s", who, name)
			reply := broker.ask(p, name)
			if reply.State != "denied" || reply.GrantID != nil || reply.RecordID != nil || string(reply.Approver) != "null" {
				t.Errorf("%s asking for %s = %+v (approver %s), want denied with no grant, no record and approver null", who, name, reply, reply.Approver)
			}
		}
	}
}
