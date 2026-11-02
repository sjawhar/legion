// machine_requester_test.go drives the authority of a session a machine enrolled under the
// launcher credential its Google identity token's exchange minted: its fleet's service, and no
// person's.
package requests

import (
	"context"
	"crypto/ecdsa"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
)

// fixtureMachineAccount is the Google service account the machine below runs as, and
// fixtureMachineSubject the subject its credential is keyed by (exchange.MachineSubject for a
// token minted on a Compute Engine instance). Both are placeholders.
const (
	fixtureMachineAccount = "example-fleet@example-project.iam.gserviceaccount.com"
	fixtureMachineSubject = fixtureMachineAccount + "@example-project/example-zone-a/1000000000000000001"
)

// newMachineEnrollment inserts a live session enrollment under a launcher credential with no
// operator, service and the credential subject the exchange gives a machine: the row
// enroll.Service.Create writes for a machine's own session.
func newMachineEnrollment(t *testing.T, m *Machine, service, subject, runtimeID string) (string, *ecdsa.PrivateKey) {
	t.Helper()
	id, key := insertEnrollment(t, m.Store, "session", runtimeID, nil, &service, &subject)
	return id, key
}

// TestAMachinesSessionIsItsFleetsAndNoPersons: the session a machine enrols reads the secrets
// its fleet's service owns and the shared agent-tier ones, and no person's — a person's secret
// is an approval request to that person, never an automatic grant, because the credential the
// exchange minted names no person whatever its fleet's entry says. A machine whose account
// BROKER_VM_SERVICES no longer binds to that service is nobody's, so even its service's secret
// is denied.
func TestAMachinesSessionIsItsFleetsAndNoPersons(t *testing.T) {
	ctx := context.Background()
	m, _, _, operator := newFixture(t)
	m.VMServicesByAccount = map[string]string{fixtureMachineAccount: fixtureService}
	retag(t, m, policytest.Secret("SHARED_TOKEN", policy.OwnerShared, policy.TierAgent, "shared-token-v1"))
	machine, machineKey := newMachineEnrollment(t, m, fixtureService, fixtureMachineSubject, "machine-"+t.Name())

	for _, want := range []struct {
		name, secret, state, value string
	}{
		{name: "its fleet's own secret", secret: "SERVICE_KEY", state: "granted", value: "service-v1"},
		{name: "a shared agent-tier secret", secret: "SHARED_TOKEN", state: "granted", value: "shared-token-v1"},
	} {
		t.Run(want.name, func(t *testing.T) {
			granted, err := m.Create(ctx, machine, signRequest(t, m, machineKey, "need it", want.secret), "")
			if err != nil || granted.State != want.state || granted.GrantID == nil {
				t.Fatalf("Create(a machine's session, %s) = %+v, %v; want %s at once", want.secret, granted, err, want.state)
			}
			if values, _, err := m.Values(ctx, *granted.GrantID, machine, nil); err != nil || values[want.secret] != want.value {
				t.Fatalf("Values(%s) = %v, %v; want it released", want.secret, values, err)
			}
		})
	}

	t.Run("a person's agent-tier secret is that person's to approve", func(t *testing.T) {
		asked, err := m.Create(ctx, machine, signRequest(t, m, machineKey, "need it", "AUTO_TOKEN"), "")
		if err != nil || asked.State != "pending" || asked.GrantID != nil {
			t.Fatalf("Create(a machine's session, AUTO_TOKEN) = %+v, %v; want it pending with no grant: %s owns it and no one has signed in on this machine", asked, err, operator)
		}
	})

	t.Run("an account the setting no longer binds is nobody's", func(t *testing.T) {
		m.VMServicesByAccount = map[string]string{}
		denied, err := m.Create(ctx, machine, signRequest(t, m, machineKey, "need it again", "SERVICE_KEY"), "")
		if err != nil || denied.State != "denied" {
			t.Fatalf("Create(a machine whose account is no longer bound, SERVICE_KEY) = %+v, %v; want it denied", denied, err)
		}
	})
}

// TestAMachineSubjectReadsBackAsItsAccount guards the one reading the authority above turns on:
// the credential subject the exchange writes is what names the fleet's account.
func TestAMachineSubjectReadsBackAsItsAccount(t *testing.T) {
	account, ok := exchange.MachineAccount(fixtureMachineSubject)
	if !ok || account != fixtureMachineAccount {
		t.Fatalf("MachineAccount(%q) = %q, %t; want %q", fixtureMachineSubject, account, ok, fixtureMachineAccount)
	}
}
