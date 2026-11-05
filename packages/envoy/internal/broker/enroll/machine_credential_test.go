// machine_credential_test.go drives the launcher credential the RFC 8693 token exchange mints
// for a machine: the upsert joined to the caller's own transaction, how long it keeps
// authenticating, and what it may enrol.
package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/exchange"
)

const (
	machineAccountEmail = "example-fleet@example-project.iam.gserviceaccount.com"
	machineFleet        = "example-fleet"
)

// issueMachineCredential mints or moves the credential of the machine subject names, in a
// transaction of its own, the way the exchange's seam does inside its own.
func issueMachineCredential(t *testing.T, svc *Service, subject, thumbprint string) string {
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
// is keyed by the machine and the key it holds (exchange.MachineSubject) and authenticates on
// the account alone — where the machine runs was the exchange entry's decision, taken before
// the credential existed. Dropping the account from BROKER_VM_SERVICES, and listing it for
// another service, each end the credential at its next call.
func TestAMachinesCredentialAuthenticatesWhileItsAccountIsListed(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	svc.VMServicesByAccount = map[string]string{machineAccountEmail: machineFleet}
	onInstance := exchange.MachineSubject(exchange.GoogleIdentity{
		Account: machineAccountEmail,
		Compute: exchange.ComputeEngine{Project: "example-project", Zone: "example-zone-a", Instance: "example-machine-1", InstanceID: "1"},
	}, "tp-on-instance")
	keyed := exchange.MachineSubject(exchange.GoogleIdentity{Account: machineAccountEmail}, "tp-pool-machine")

	for _, subject := range []string{onInstance, keyed} {
		id := issueMachineCredential(t, svc, subject, "tp-"+subject)
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
// machine that exchanges again moves its key and its expiry under the same id rather than taking
// a second row. A machine of the same account with a key of its own holds a credential of its
// own.
func TestAMachinesSecondExchangeMovesItsOneCredential(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	svc.VMServicesByAccount = map[string]string{machineAccountEmail: machineFleet}
	subject := exchange.MachineSubject(exchange.GoogleIdentity{Account: machineAccountEmail}, "tp-one")

	first := issueMachineCredential(t, svc, subject, "tp-one")
	again := issueMachineCredential(t, svc, subject, "tp-one-rotated")
	if again != first {
		t.Fatalf("the machine's second exchange = credential %s, want the one it already holds, %s", again, first)
	}
	thumbprint, live, err := svc.AuthenticateLauncher(ctx, first)
	if err != nil || !live || thumbprint != "tp-one-rotated" {
		t.Fatalf("AuthenticateLauncher after the second exchange = %q, %t, %v; want the key it last proved", thumbprint, live, err)
	}
	other := exchange.MachineSubject(exchange.GoogleIdentity{Account: machineAccountEmail}, "tp-two")
	if second := issueMachineCredential(t, svc, other, "tp-two"); second == first {
		t.Fatalf("a second machine of %s took the first's credential %s; each key holds its own", machineAccountEmail, first)
	}
}

// TestAMachineEnrolsASessionThatActsForItsFleetAndNoPerson: the credential the exchange minted
// is the bridge to every secrets route. It carries its fleet's service and no person, so the
// session it enrols carries no operator either and records the credential's subject, which is
// what makes the session its fleet's and nobody's (requests.Machine.requester). Nothing here is
// refused for want of a person: secret authority follows the access token, and a person reaches
// their own secrets on that machine only through their own sign-in.
func TestAMachineEnrolsASessionThatActsForItsFleetAndNoPerson(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	svc.VMServicesByAccount = map[string]string{machineAccountEmail: machineFleet}
	subject := exchange.MachineSubject(exchange.GoogleIdentity{Account: machineAccountEmail}, "tp-machine")

	cred, err := svc.Credential(ctx, issueMachineCredential(t, svc, subject, "tp-machine"))
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if cred.Operator != nil || cred.Service == nil || *cred.Service != machineFleet {
		t.Fatalf("the machine's credential = operator %v, service %v; want its fleet's service and no person", cred.Operator, cred.Service)
	}
	enrolled, err := svc.Create(ctx, cred, Enrollment{Kind: "session", RuntimeID: "machine-1", Thumbprint: "tp-session"})
	if err != nil {
		t.Fatalf("a machine's session enrollment: %v", err)
	}
	if enrolled.Operator != nil {
		t.Errorf("the enrollment's operator = %v, want none: a machine acts for its fleet, never for a person", *enrolled.Operator)
	}
	if enrolled.Subject == nil || *enrolled.Subject != subject {
		t.Fatalf("the enrollment's subject = %v, want the credential's %q: it is what proves the fleet", enrolled.Subject, subject)
	}
}

// TestOnlyAMachinesOwnCredentialEnrolsItsSession: a Kubernetes service's token login enrols the
// pods its tokens prove and no session, and one machine's credential never ends another
// machine's session.
func TestOnlyAMachinesOwnCredentialEnrolsItsSession(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	svc.VMServicesByAccount = map[string]string{machineAccountEmail: machineFleet}
	svc.ServiceLogins = map[string]string{"system:serviceaccount:example:example-daemon": "legion-daemon"}

	daemon, err := svc.Credential(ctx, func() string {
		tx, err := svc.Store.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		id, err := svc.IssueTokenLoginTx(ctx, tx, TokenLogin{
			Subject: "system:serviceaccount:example:example-daemon", Service: "legion-daemon",
			Host: "example/example-daemon", TokenExpires: time.Now().Add(time.Hour),
			Thumbprint: "tp-daemon", JWK: json.RawMessage(`{"kty":"EC"}`), Expires: time.Now().Add(time.Hour),
		})
		if err != nil {
			t.Fatalf("IssueTokenLoginTx(the daemon): %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return id.String()
	}())
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if _, err := svc.Create(ctx, daemon, Enrollment{Kind: "session", RuntimeID: "daemon-1", Thumbprint: "tp-daemon-session"}); !errors.Is(err, ErrOperatorMismatch) {
		t.Fatalf("a Kubernetes service login's session enrollment = %v, want ErrOperatorMismatch: it enrols the pods its tokens prove", err)
	}

	one := exchange.MachineSubject(exchange.GoogleIdentity{Account: machineAccountEmail}, "tp-one")
	two := exchange.MachineSubject(exchange.GoogleIdentity{Account: machineAccountEmail}, "tp-two")
	credOne, err := svc.Credential(ctx, issueMachineCredential(t, svc, one, "tp-one"))
	if err != nil {
		t.Fatal(err)
	}
	credTwo, err := svc.Credential(ctx, issueMachineCredential(t, svc, two, "tp-two"))
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := svc.Create(ctx, credOne, Enrollment{Kind: "session", RuntimeID: "machine-one", Thumbprint: "tp-session-one"})
	if err != nil {
		t.Fatalf("the first machine's session enrollment: %v", err)
	}
	if err := svc.Revoke(ctx, credTwo, enrolled.ID.String(), "launcher:two"); !errors.Is(err, ErrNotEnrollingCredential) {
		t.Fatalf("another machine's revoke of this one's session = %v, want ErrNotEnrollingCredential", err)
	}
	if err := svc.Revoke(ctx, credOne, enrolled.ID.String(), "launcher:one"); err != nil {
		t.Fatalf("the machine's own revoke of its session: %v", err)
	}
}

// TestAMachineSessionRenewsOnlyWhileItsMachineLoginIsLive is the safeguard the vm enrollment
// kind used to carry: a machine session's renewal proves only that its renewer holds the
// session key, and the key and the enrollment id are two files on the box, so a copy of them
// taken off it would renew for ever. Renewal is therefore held to the launcher credential the
// machine's exchange minted, and the two endings a revoke is not — the account dropped from
// BROKER_VM_SERVICES, and the credential past the expiry its Google identity token gave it —
// each refuse the next renewal. Listing the account again lets it renew, so this is the
// configuration as it is now and not a mark on the row.
func TestAMachineSessionRenewsOnlyWhileItsMachineLoginIsLive(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	svc.VMServicesByAccount = map[string]string{machineAccountEmail: machineFleet}
	subject := exchange.MachineSubject(exchange.GoogleIdentity{Account: machineAccountEmail}, "tp-machine")
	cred, err := svc.Credential(ctx, issueMachineCredential(t, svc, subject, "tp-machine"))
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	enrolled, err := svc.Create(ctx, cred, Enrollment{Kind: "session", RuntimeID: "machine-1", Thumbprint: "tp-session"})
	if err != nil {
		t.Fatalf("a machine's session enrollment: %v", err)
	}
	id := enrolled.ID.String()
	if _, err := svc.Renew(ctx, id, ""); err != nil {
		t.Fatalf("the first renewal of a live machine's session: %v; want it renewed", err)
	}

	svc.VMServicesByAccount = map[string]string{}
	if _, err := svc.Renew(ctx, id, ""); !errors.Is(err, ErrMachineLoginEnded) {
		t.Fatalf("the renewal of a delisted machine's session = %v, want ErrMachineLoginEnded", err)
	}
	svc.VMServicesByAccount = map[string]string{machineAccountEmail: machineFleet}
	if _, err := svc.Renew(ctx, id, ""); err != nil {
		t.Fatalf("the renewal after the account was listed again: %v; want it renewed", err)
	}

	// RetireCredential is the one operation that ends a credential by its expiry and leaves
	// the enrollments it made alone, which is exactly the state a machine that has stopped
	// trading reaches when its last Google identity token runs out.
	if err := svc.RetireCredential(ctx, cred.ID.String(), "broker"); err != nil {
		t.Fatalf("RetireCredential: %v", err)
	}
	if _, err := svc.Renew(ctx, id, ""); !errors.Is(err, ErrMachineLoginEnded) {
		t.Fatalf("the renewal of a session whose machine login has lapsed = %v, want ErrMachineLoginEnded", err)
	}
}

// TestAPersonsSessionOnAMachineRenewsPastItsCredentialsExpiry holds the asymmetry the rule
// above is written around. A person's sign-in on a box mints a credential of the same shape and
// of the same hour's life, and their sessions must outlive it: a person signs in once per box
// and their sessions are theirs until they revoke that sign-in, which ends them at once
// (RevokePersonSignIn). Only a machine's own session, which no person ever enrolled, is held to
// its credential.
func TestAPersonsSessionOnAMachineRenewsPastItsCredentialsExpiry(t *testing.T) {
	ctx := context.Background()
	const person = "ada@example.com"
	svc := newService(t)
	svc.VMServicesByAccount = map[string]string{machineAccountEmail: machineFleet}

	tx, err := svc.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	credID, err := svc.IssueTokenLoginTx(ctx, tx, TokenLogin{
		Subject: exchange.PersonSubject(person, machineAccountEmail, "tp-box"), Service: machineFleet,
		Operator: person, Host: "example-project/example-zone-a/example-machine-1",
		TokenExpires: time.Now().Add(time.Hour), Thumbprint: "tp-box",
		JWK: json.RawMessage(`{"kty":"EC"}`), Expires: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("IssueTokenLoginTx(a person's sign-in): %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	cred, err := svc.Credential(ctx, credID.String())
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	operator := person
	enrolled, err := svc.Create(ctx, cred, Enrollment{Kind: "session", RuntimeID: "box-1", Operator: &operator, Thumbprint: "tp-person-session"})
	if err != nil {
		t.Fatalf("a person's session enrollment on a machine: %v", err)
	}
	if err := svc.RetireCredential(ctx, credID.String(), "broker"); err != nil {
		t.Fatalf("RetireCredential: %v", err)
	}
	if _, err := svc.Renew(ctx, enrolled.ID.String(), ""); err != nil {
		t.Fatalf("the renewal of a person's session past its sign-in credential's expiry: %v; want it renewed", err)
	}
}
