// machine_credential_test.go drives the launcher credential the RFC 8693 token exchange mints
// for a machine: the upsert joined to the caller's own transaction, how long it keeps
// authenticating, and what it may enrol.
package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/exchange"
)

const (
	machineAccountEmail = "example-fleet@example-project.iam.gserviceaccount.com"
	machineFleet        = "example-fleet"
	machinePerson       = "ada@example.com"
)

// issueMachineCredential mints or moves the credential of the machine subject names, in a
// transaction of its own, the way the exchange's seam does inside its own.
func issueMachineCredential(t *testing.T, svc *Service, subject, operator, thumbprint string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := svc.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	id, err := svc.IssueTokenLoginTx(ctx, tx, TokenLogin{
		Subject:      subject,
		Service:      machineFleet,
		Host:         "example-project/example-zone-a/example-machine-1",
		Operator:     operator,
		TokenExpires: time.Now().Add(time.Hour),
		Thumbprint:   thumbprint,
		JWK:          json.RawMessage(`{"kty":"EC"}`),
		Expires:      time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("IssueTokenLoginTx(%s): %v", subject, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return id.String()
}

// TestAMachinesCredentialAuthenticatesWhileItsAccountIsListed: the credential the exchange mints
// is keyed by the machine (exchange.MachineSubject) and authenticates on the account alone —
// where the machine runs was the exchange entry's decision, taken before the credential existed.
// Dropping the account from BROKER_VM_SERVICES, and listing it for another service, each end the
// credential at its next call.
func TestAMachinesCredentialAuthenticatesWhileItsAccountIsListed(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	svc.VMServicesByAccount = map[string]string{machineAccountEmail: machineFleet}
	onInstance := exchange.MachineSubject(exchange.GoogleIdentity{
		Account: machineAccountEmail,
		Compute: exchange.ComputeEngine{Project: "example-project", Zone: "example-zone-a", Instance: "example-machine-1", InstanceID: "1"},
	}, "tp-ignored")
	keyed := exchange.MachineSubject(exchange.GoogleIdentity{Account: machineAccountEmail}, "tp-pool-machine")

	for _, subject := range []string{onInstance, keyed} {
		id := issueMachineCredential(t, svc, subject, machinePerson, "tp-"+subject)
		thumbprint, live, err := svc.AuthenticateLauncher(ctx, id)
		if err != nil || !live || thumbprint != "tp-"+subject {
			t.Fatalf("AuthenticateLauncher(%s) = %q, %t, %v; want its key and live", subject, thumbprint, live, err)
		}
		svc.VMServicesByAccount = map[string]string{machineAccountEmail: "another-service"}
		if _, live, err := svc.AuthenticateLauncher(ctx, id); err != nil || live {
			t.Errorf("AuthenticateLauncher(%s) with the account listed for another service = %t, %v; want it no longer live", subject, live, err)
		}
		svc.VMServicesByAccount = map[string]string{}
		if _, live, err := svc.AuthenticateLauncher(ctx, id); err != nil || live {
			t.Errorf("AuthenticateLauncher(%s) with the account dropped = %t, %v; want it no longer live", subject, live, err)
		}
		svc.VMServicesByAccount = map[string]string{machineAccountEmail: machineFleet}
	}
}

// TestAMachinesSecondExchangeMovesItsOneCredential: the subject holds one credential, so a
// machine that exchanges again moves its key, its person and its expiry under the same id rather
// than taking a second row. A machine of the same account with a key of its own holds a
// credential of its own.
func TestAMachinesSecondExchangeMovesItsOneCredential(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	svc.VMServicesByAccount = map[string]string{machineAccountEmail: machineFleet}
	subject := exchange.MachineSubject(exchange.GoogleIdentity{Account: machineAccountEmail}, "tp-one")

	first := issueMachineCredential(t, svc, subject, machinePerson, "tp-one")
	again := issueMachineCredential(t, svc, subject, machinePerson, "tp-one-rotated")
	if again != first {
		t.Fatalf("the machine's second exchange = credential %s, want the one it already holds, %s", again, first)
	}
	thumbprint, live, err := svc.AuthenticateLauncher(ctx, first)
	if err != nil || !live || thumbprint != "tp-one-rotated" {
		t.Fatalf("AuthenticateLauncher after the second exchange = %q, %t, %v; want the key it last proved", thumbprint, live, err)
	}
	other := exchange.MachineSubject(exchange.GoogleIdentity{Account: machineAccountEmail}, "tp-two")
	if second := issueMachineCredential(t, svc, other, machinePerson, "tp-two"); second == first {
		t.Fatalf("a second machine of %s took the first's credential %s; each key holds its own", machineAccountEmail, first)
	}
}

// TestAMachineEnrolsASessionUnderItsFleetsPerson: the credential the exchange minted is the
// bridge to every secrets route. It enrols a session for the person its fleet's entry names,
// exactly as a dev box's machine login does; a fleet whose entry names none is refused
// ErrFleetNamesNoPerson, whose message names the setting to add and the account to add it for.
func TestAMachineEnrolsASessionUnderItsFleetsPerson(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	svc.VMServicesByAccount = map[string]string{machineAccountEmail: machineFleet}
	subject := exchange.MachineSubject(exchange.GoogleIdentity{Account: machineAccountEmail}, "tp-person")

	id := issueMachineCredential(t, svc, subject, machinePerson, "tp-person")
	cred, err := svc.Credential(ctx, id)
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	enrolled, err := svc.Create(ctx, cred, Enrollment{Kind: "session", RuntimeID: "machine-1", Thumbprint: "tp-session"})
	if err != nil {
		t.Fatalf("a machine's session enrollment: %v", err)
	}
	if enrolled.Operator == nil || *enrolled.Operator != machinePerson {
		t.Fatalf("the enrollment's operator = %v, want the person its fleet's entry names, %s", enrolled.Operator, machinePerson)
	}

	serviceOnly := exchange.MachineSubject(exchange.GoogleIdentity{Account: machineAccountEmail}, "tp-service-only")
	withoutPerson, err := svc.Credential(ctx, issueMachineCredential(t, svc, serviceOnly, "", "tp-service-only"))
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	_, err = svc.Create(ctx, withoutPerson, Enrollment{Kind: "session", RuntimeID: "machine-2", Thumbprint: "tp-session-2"})
	if !errors.Is(err, ErrFleetNamesNoPerson) {
		t.Fatalf("a session under a fleet that names no person = %v, want ErrFleetNamesNoPerson", err)
	}
	for _, names := range []string{"BROKER_VM_EXCHANGE_ACCOUNTS", machineAccountEmail} {
		if !strings.Contains(err.Error(), names) {
			t.Errorf("the refusal reads %q, which does not name %q", err, names)
		}
	}
}
