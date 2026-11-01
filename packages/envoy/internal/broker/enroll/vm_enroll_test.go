package enroll

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// vmFleetService is the service the VMs below log in as.
const vmFleetService = "example-fleet"

// vmRig is a Service trusting a local issuer in Google's place for vmTokenAudience, with the
// issuer and its key, so a test mints VM identity tokens.
type vmRig struct {
	svc    *Service
	issuer *oidctest.Issuer
	key    *oidctest.Key
}

// newVMRig is newService with a VM verifier and vmTokenAccount bound to vmFleetService in
// vmTokenProject, as BROKER_VM_SERVICES and BROKER_VM_PROJECTS bind one.
func newVMRig(t *testing.T) vmRig {
	t.Helper()
	svc := newService(t)
	verifier, issuer, key := newGoogleVMVerifier(t)
	svc.VM = verifier
	svc.VMServicesByAccount = map[string]string{vmTokenAccount: vmFleetService}
	svc.VMProjects = map[string]string{vmTokenAccount: vmTokenProject}
	svc.VMInstancePatterns = map[string]string{vmTokenAccount: "example-vm-*"}
	return vmRig{svc: svc, issuer: issuer, key: key}
}

// vm is one VM of the fleet: vmTokenAccount in vmTokenProject, in zone, with instance id id.
func vm(zone, id string) VMClaims {
	return VMClaims{Account: vmTokenAccount, Project: vmTokenProject, Zone: zone, Instance: "example-vm-" + id, InstanceID: id}
}

// token mints vm's identity token for audience, expiring lifetime from now, its email verified
// as verified says.
func (r vmRig) token(t *testing.T, vm VMClaims, audience string, lifetime time.Duration, verified any) string {
	t.Helper()
	claims := r.issuer.Claims(vm.Account, audience)
	expires := time.Now().Add(lifetime)
	claims["iat"] = expires.Add(-time.Hour).Unix()
	claims["nbf"] = claims["iat"]
	claims["exp"] = expires.Unix()
	claims["email"] = vm.Account
	claims["email_verified"] = verified
	claims["google"] = map[string]any{"compute_engine": map[string]any{
		"project_id": vm.Project, "zone": vm.Zone, "instance_name": vm.Instance, "instance_id": vm.InstanceID,
	}}
	return r.issuer.Mint(t, r.key, claims)
}

// current is vm's current, valid identity token.
func (r vmRig) current(t *testing.T, vm VMClaims) string {
	t.Helper()
	return r.token(t, vm, vmTokenAudience, time.Hour, true)
}

// login issues vm's own launcher credential as its token login does (IssueTokenLogin under the
// VM's runtime id), and returns it as Credential reads it back.
func (r vmRig) login(t *testing.T, vm VMClaims) Credential {
	t.Helper()
	ctx := context.Background()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	jwk, err := (jose.JSONWebKey{Key: &key.PublicKey}).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	id, err := r.svc.IssueTokenLogin(ctx, TokenLogin{
		Subject: vm.RuntimeID(), Service: vmFleetService, Host: vm.Project + "/" + vm.Zone + "/" + vm.Instance,
		TokenExpires: expires, Thumbprint: thumbprint, JWK: jwk, Expires: expires,
	})
	if err != nil {
		t.Fatalf("IssueTokenLogin(%s): %v", vm.RuntimeID(), err)
	}
	cred, err := r.svc.Credential(ctx, id.String())
	if err != nil {
		t.Fatalf("Credential(%s): %v", id, err)
	}
	return cred
}

// enroll enrolls vm under cred with its current token and a fresh key's thumbprint.
func (r vmRig) enroll(t *testing.T, cred Credential, vm VMClaims) Enrollment {
	t.Helper()
	enr, err := r.svc.Create(context.Background(), cred, Enrollment{Kind: "vm", RuntimeID: vm.RuntimeID(), Thumbprint: "tp-" + vm.RuntimeID(), VMToken: r.current(t, vm)})
	if err != nil {
		t.Fatalf("Create(vm %s): %v", vm.RuntimeID(), err)
	}
	return enr
}

// TestAVMEnrolsWithItsOwnTokenAndNoOperator: under its own login's credential, a VM's current
// identity token enrolls it as kind vm, keyed by its runtime id, its subject the token's verified
// account, with no operator; Get reads it back as a vm.
func TestAVMEnrolsWithItsOwnTokenAndNoOperator(t *testing.T) {
	r := newVMRig(t)
	ctx := context.Background()
	one := vm(vmTokenZone, "1001")
	cred := r.login(t, one)
	if cred.Operator != nil || cred.Service == nil || *cred.Service != vmFleetService || cred.Subject == nil || *cred.Subject != one.RuntimeID() {
		t.Fatalf("the VM's credential = %+v, want no operator, service %s, subject %s", cred, vmFleetService, one.RuntimeID())
	}
	if cred.Host != "example-project/example-zone-a/example-vm-1001" {
		t.Fatalf("the VM's credential host = %q, want example-project/example-zone-a/example-vm-1001", cred.Host)
	}
	enr := r.enroll(t, cred, one)
	if enr.Existing || enr.Operator != nil || enr.Subject == nil || *enr.Subject != vmTokenAccount {
		t.Fatalf("Create(vm) = %+v, want a new enrollment with no operator and subject %s", enr, vmTokenAccount)
	}
	got, err := r.svc.Get(ctx, enr.ID.String())
	if err != nil || got.Kind != "vm" || got.RuntimeID != one.RuntimeID() || got.Operator != nil {
		t.Fatalf("Get(vm enrollment) = %+v, %v; want kind vm, runtime %s, no operator", got, err, one.RuntimeID())
	}
	if _, live, err := r.svc.Lookup(ctx, enr.ID.String()); err != nil || !live {
		t.Fatalf("Lookup(vm enrollment) live = %v, %v; want live", live, err)
	}
}

// TestAVMEnrollmentRejectsAnotherInstancesToken: a vm enrollment proves its runtime id with the
// VM's own token, under the credential that VM's own login holds. Another VM's token, a runtime id
// the token does not name, another VM's credential, a Kubernetes service's credential, a token for
// another audience (review focus 1), one whose email is not verified (review focus 2), and one
// without the Compute Engine claims are each ErrVMIdentity; an expired token is ErrVMTokenExpired;
// a person's credential is ErrOperatorMismatch, as for a pod; a broker with no VM verifier refuses
// every one. Nothing is enrolled.
func TestAVMEnrollmentRejectsAnotherInstancesToken(t *testing.T) {
	r := newVMRig(t)
	ctx := context.Background()
	one, two := vm(vmTokenZone, "1001"), vm(vmTokenZone, "1002")
	cred := r.login(t, one)
	twoCred := r.login(t, two)
	daemon := mintCredential(t, r.svc, nil, str("legion-daemon"), "legion-system/legion-daemon")
	person := mintCredential(t, r.svc, str("ada@example.com"), nil, "devbox")
	noCompute := r.issuer.Claims(vmTokenAccount, vmTokenAudience)
	noCompute["email"], noCompute["email_verified"] = vmTokenAccount, true
	for _, tc := range []struct {
		name    string
		cred    Credential
		runtime string
		token   string
		want    error
	}{
		{"another VM's token", cred, one.RuntimeID(), r.current(t, two), ErrVMIdentity},
		{"a runtime id the token does not name", cred, two.RuntimeID(), r.current(t, one), ErrVMIdentity},
		{"another VM's credential", twoCred, one.RuntimeID(), r.current(t, one), ErrVMIdentity},
		{"a Kubernetes service's credential", daemon, one.RuntimeID(), r.current(t, one), ErrVMIdentity},
		{"a token for another audience", cred, one.RuntimeID(), r.token(t, one, "another-audience", time.Hour, true), ErrVMIdentity},
		{"a token whose email is not verified", cred, one.RuntimeID(), r.token(t, one, vmTokenAudience, time.Hour, false), ErrVMIdentity},
		{"a token without the Compute Engine claims", cred, one.RuntimeID(), r.issuer.Mint(t, r.key, noCompute), ErrVMIdentity},
		{"no token", cred, one.RuntimeID(), "", ErrVMIdentity},
		{"an expired token", cred, one.RuntimeID(), r.token(t, one, vmTokenAudience, -time.Minute, true), ErrVMTokenExpired},
		{"a person's credential", person, one.RuntimeID(), r.current(t, one), ErrOperatorMismatch},
	} {
		_, err := r.svc.Create(ctx, tc.cred, Enrollment{Kind: "vm", RuntimeID: tc.runtime, Thumbprint: "tp-" + tc.name, VMToken: tc.token})
		if !errors.Is(err, tc.want) {
			t.Errorf("Create(%s) = %v, want %v", tc.name, err, tc.want)
		}
	}
	plain := newService(t)
	if _, err := plain.Create(ctx, cred, Enrollment{Kind: "vm", RuntimeID: one.RuntimeID(), Thumbprint: "tp", VMToken: r.current(t, one)}); !errors.Is(err, ErrVMIdentity) {
		t.Errorf("Create(vm) on a broker with no VM verifier = %v, want ErrVMIdentity", err)
	}
	var enrolled int
	if err := r.svc.Store.Pool.QueryRow(ctx, `select count(*) from enrollments`).Scan(&enrolled); err != nil || enrolled != 0 {
		t.Fatalf("enrollments = %d, %v; want none", enrolled, err)
	}
}

// TestTwoVMsOfOneAccountEnrolSeparatelyAndOneRevocationLeavesTheOther (review focus 4): two VMs of
// one service account each log in under a credential of their own and hold an enrollment of their
// own. No launcher credential ends a VM's enrollment, its own included (ErrVMEndsByRevokingItsLogin);
// a person revoking one VM's machine login (RevokeCredential) ends that VM's enrollment alone: the
// other's stays live and renews.
func TestTwoVMsOfOneAccountEnrolSeparatelyAndOneRevocationLeavesTheOther(t *testing.T) {
	r := newVMRig(t)
	ctx := context.Background()
	one, two := vm(vmTokenZone, "1001"), vm(vmTokenZone, "1002")
	oneCred, twoCred := r.login(t, one), r.login(t, two)
	if oneCred.ID == twoCred.ID {
		t.Fatalf("two VMs share launcher credential %s; want one each", oneCred.ID)
	}
	oneEnr, twoEnr := r.enroll(t, oneCred, one), r.enroll(t, twoCred, two)
	if oneEnr.ID == twoEnr.ID {
		t.Fatalf("two VMs share enrollment %s; want one each", oneEnr.ID)
	}
	for _, tc := range []struct {
		name string
		cred Credential
		id   string
	}{
		{"the second VM's enrollment, by the first VM's credential", oneCred, twoEnr.ID.String()},
		{"the first VM's own enrollment, by its own credential", oneCred, oneEnr.ID.String()},
	} {
		if err := r.svc.Revoke(ctx, tc.cred, tc.id, "launcher:"+tc.cred.ID.String()); !errors.Is(err, ErrVMEndsByRevokingItsLogin) {
			t.Fatalf("Revoke(%s) = %v, want ErrVMEndsByRevokingItsLogin", tc.name, err)
		}
	}
	if err := r.svc.RevokeCredential(ctx, oneCred.ID.String(), "ada@example.com", "human:ada@example.com"); err != nil {
		t.Fatalf("RevokeCredential(the first VM's machine login) = %v", err)
	}
	if _, live, err := r.svc.Lookup(ctx, oneEnr.ID.String()); err != nil || live {
		t.Fatalf("the first VM's enrollment after its login's revoke: live = %v, %v; want ended", live, err)
	}
	if _, live, err := r.svc.Lookup(ctx, twoEnr.ID.String()); err != nil || !live {
		t.Fatalf("the second VM's enrollment after the first's revoke: live = %v, %v; want live", live, err)
	}
	if _, err := r.svc.RenewVM(ctx, twoEnr.ID.String(), r.current(t, two)); err != nil {
		t.Fatalf("RenewVM(the second VM) after the first's revoke = %v", err)
	}
}

// TestTwoVMsOfOneInstanceIdInDifferentZonesHoldTwoEnrollments: an instance id is unique only within
// a project and a zone, so two VMs with one id in two zones are two runtime ids, two credentials and
// two enrollments, and neither one's token enrolls the other.
func TestTwoVMsOfOneInstanceIdInDifferentZonesHoldTwoEnrollments(t *testing.T) {
	r := newVMRig(t)
	ctx := context.Background()
	a, b := vm("example-zone-a", "1001"), vm("example-zone-b", "1001")
	if a.RuntimeID() == b.RuntimeID() {
		t.Fatalf("runtime ids of one instance id in two zones are both %q", a.RuntimeID())
	}
	aCred, bCred := r.login(t, a), r.login(t, b)
	if aCred.ID == bCred.ID {
		t.Fatalf("two zones' VMs share launcher credential %s", aCred.ID)
	}
	if _, err := r.svc.Create(ctx, aCred, Enrollment{Kind: "vm", RuntimeID: a.RuntimeID(), Thumbprint: "tp-cross", VMToken: r.current(t, b)}); !errors.Is(err, ErrVMIdentity) {
		t.Fatalf("Create(zone a's VM with zone b's token) = %v, want ErrVMIdentity", err)
	}
	aEnr, bEnr := r.enroll(t, aCred, a), r.enroll(t, bCred, b)
	if aEnr.ID == bEnr.ID || aEnr.Existing || bEnr.Existing {
		t.Fatalf("enrollments = %+v and %+v; want two new ones", aEnr, bEnr)
	}
}

// TestAVMEnrollmentTakesNoSlot: like a session, a vm enrollment takes no slot.
func TestAVMEnrollmentTakesNoSlot(t *testing.T) {
	r := newVMRig(t)
	one := vm(vmTokenZone, "1001")
	cred := r.login(t, one)
	_, err := r.svc.Create(context.Background(), cred, Enrollment{Kind: "vm", RuntimeID: one.RuntimeID(), Slot: "implementer-g1", Thumbprint: "tp", VMToken: r.current(t, one)})
	if !errors.Is(err, ErrInvalidSlot) {
		t.Fatalf("Create(vm, with a slot) = %v, want ErrInvalidSlot", err)
	}
}

// TestARenewalCarryingTheVMsCurrentTokenExtendsTheLease: RenewVM with the VM's current token moves
// the lease to Lease from now. Every renewal of a vm enrollment carries that token: none is
// ErrVMTokenRequired, another VM's token ErrVMIdentity, an expired one ErrVMTokenExpired, and a pod
// token ErrPodIdentity, each leaving the lease where it was. A session's renewal carrying a VM token
// is ErrVMIdentity.
func TestARenewalCarryingTheVMsCurrentTokenExtendsTheLease(t *testing.T) {
	r := newVMRig(t)
	ctx := context.Background()
	one, two := vm(vmTokenZone, "1001"), vm(vmTokenZone, "1002")
	enr := r.enroll(t, r.login(t, one), one)
	id := enr.ID.String()
	if _, err := r.svc.Store.Pool.Exec(ctx, `update enrollments set lease_expires_at = now() + interval '1 minute' where id = $1`, id); err != nil {
		t.Fatal(err)
	}
	lease := func() time.Time {
		t.Helper()
		var at time.Time
		if err := r.svc.Store.Pool.QueryRow(ctx, `select lease_expires_at from enrollments where id = $1`, id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	before := lease()
	for _, tc := range []struct {
		name  string
		renew func() (Renewal, error)
		want  error
	}{
		{"no token", func() (Renewal, error) { return r.svc.RenewVM(ctx, id, "") }, ErrVMTokenRequired},
		{"no token, as a pod or box renews", func() (Renewal, error) { return r.svc.Renew(ctx, id, "") }, ErrVMTokenRequired},
		{"another VM's token", func() (Renewal, error) { return r.svc.RenewVM(ctx, id, r.current(t, two)) }, ErrVMIdentity},
		{"an expired token", func() (Renewal, error) {
			return r.svc.RenewVM(ctx, id, r.token(t, one, vmTokenAudience, -time.Minute, true))
		}, ErrVMTokenExpired},
		{"a token for another audience", func() (Renewal, error) {
			return r.svc.RenewVM(ctx, id, r.token(t, one, "another-audience", time.Hour, true))
		}, ErrVMIdentity},
	} {
		if _, err := tc.renew(); !errors.Is(err, tc.want) {
			t.Errorf("renewal with %s = %v, want %v", tc.name, err, tc.want)
		}
		if got := lease(); !got.Equal(before) {
			t.Fatalf("renewal with %s moved the lease from %s to %s", tc.name, before, got)
		}
	}
	podIssuer, podKey := withPodVerifier(t, r.svc)
	if _, err := r.svc.Renew(ctx, id, mintPodToken(t, podIssuer, podKey, "system:serviceaccount:legion:worker", "pod-1")); !errors.Is(err, ErrPodIdentity) {
		t.Fatalf("Renew(vm enrollment, with a pod token) = %v, want ErrPodIdentity", err)
	}
	renewal, err := r.svc.RenewVM(ctx, id, r.current(t, one))
	if err != nil || renewal.WithoutPodToken {
		t.Fatalf("RenewVM(the VM's current token) = %+v, %v; want renewed", renewal, err)
	}
	if got := lease(); got.Before(time.Now().Add(r.svc.Lease - time.Minute)) {
		t.Fatalf("lease after RenewVM = %s, want about %s from now", got, r.svc.Lease)
	}

	box, err := r.svc.Create(ctx, mintCredential(t, r.svc, str("ada@example.com"), nil, "devbox"), Enrollment{Kind: "session", RuntimeID: "box-1", Thumbprint: "tp-box"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.RenewVM(ctx, box.ID.String(), r.current(t, one)); !errors.Is(err, ErrVMIdentity) {
		t.Fatalf("RenewVM(a session) = %v, want ErrVMIdentity", err)
	}
}

// TestAVMCredentialAuthenticatesWhileItsAccountIsBound: a VM login's credential authenticates
// while its fleet still allows the VM (VMService: BROKER_VM_SERVICES binds its account to its
// service, BROKER_VM_PROJECTS names its project, BROKER_VM_INSTANCES' glob matches its name), and
// stops at its next call once any of them changes, as a Kubernetes login's stops when
// BROKER_SERVICE_LOGINS drops it.
func TestAVMCredentialAuthenticatesWhileItsAccountIsBound(t *testing.T) {
	r := newVMRig(t)
	ctx := context.Background()
	cred := r.login(t, vm(vmTokenZone, "1001"))
	authenticates := func() bool {
		t.Helper()
		_, ok, err := r.svc.AuthenticateLauncher(ctx, cred.ID.String())
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !authenticates() {
		t.Fatal("a bound VM's credential does not authenticate")
	}
	r.svc.VMProjects = map[string]string{vmTokenAccount: "example-other-project"}
	if authenticates() {
		t.Fatal("a VM's credential authenticates after its account's project changed")
	}
	r.svc.VMProjects = map[string]string{vmTokenAccount: vmTokenProject}
	r.svc.VMInstancePatterns = map[string]string{vmTokenAccount: "other-vm-*"}
	if authenticates() {
		t.Fatal("a VM's credential authenticates after its fleet's instance names changed")
	}
	r.svc.VMInstancePatterns = map[string]string{vmTokenAccount: "example-vm-*"}
	if !authenticates() {
		t.Fatal("a VM's credential does not authenticate once its fleet allows it again")
	}
	r.svc.VMServicesByAccount = map[string]string{vmTokenAccount: "another-service"}
	if authenticates() {
		t.Fatal("a VM's credential authenticates after its account was bound to another service")
	}
	r.svc.VMServicesByAccount = nil
	if authenticates() {
		t.Fatal("a VM's credential authenticates after BROKER_VM_SERVICES dropped its account")
	}
	r.svc.ServiceLogins = map[string]string{cred.ID.String(): vmFleetService, *cred.Subject: vmFleetService}
	if authenticates() {
		t.Fatal("a VM's credential authenticates through BROKER_SERVICE_LOGINS")
	}
}

// TestANarrowedFleetEndsAnEnrolledVMAtItsNextRenewal: once BROKER_VM_PROJECTS names another
// project, BROKER_VM_INSTANCES a glob the VM's name does not match, or BROKER_VM_SERVICES drops its
// account, the VM's renewal with its own current token is ErrVMNotListed and leaves its lease where
// it was, and a new enrollment of it is ErrVMNotListed too.
func TestANarrowedFleetEndsAnEnrolledVMAtItsNextRenewal(t *testing.T) {
	for name, narrow := range map[string]func(*Service){
		"another project":       func(s *Service) { s.VMProjects = map[string]string{vmTokenAccount: "example-other-project"} },
		"another instance glob": func(s *Service) { s.VMInstancePatterns = map[string]string{vmTokenAccount: "other-vm-*"} },
		"the account dropped":   func(s *Service) { s.VMServicesByAccount = map[string]string{} },
	} {
		t.Run(name, func(t *testing.T) {
			r := newVMRig(t)
			ctx := context.Background()
			one := vm(vmTokenZone, "1001")
			cred := r.login(t, one)
			enr := r.enroll(t, cred, one)
			if _, live, err := r.svc.Lookup(ctx, enr.ID.String()); err != nil || !live {
				t.Fatalf("Lookup(the fleet's VM) before narrowing: live = %v, %v; want live", live, err)
			}
			narrow(r.svc)
			if _, live, err := r.svc.Lookup(ctx, enr.ID.String()); err != nil || live {
				t.Fatalf("Lookup after %s: live = %v, %v; want not live, the VM has no identity", name, live, err)
			}
			var before time.Time
			if err := r.svc.Store.Pool.QueryRow(ctx, `select lease_expires_at from enrollments where id = $1`, enr.ID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if _, err := r.svc.RenewVM(ctx, enr.ID.String(), r.current(t, one)); !errors.Is(err, ErrVMNotListed) {
				t.Fatalf("RenewVM after %s = %v, want ErrVMNotListed", name, err)
			}
			var after time.Time
			if err := r.svc.Store.Pool.QueryRow(ctx, `select lease_expires_at from enrollments where id = $1`, enr.ID).Scan(&after); err != nil || !after.Equal(before) {
				t.Fatalf("lease after the refused renewal = %s, %v; want it unchanged at %s", after, err, before)
			}
			two := vm(vmTokenZone, "1002")
			_, err := r.svc.Create(ctx, r.login(t, two), Enrollment{Kind: "vm", RuntimeID: two.RuntimeID(), Thumbprint: "tp-two", VMToken: r.current(t, two)})
			if !errors.Is(err, ErrVMNotListed) {
				t.Fatalf("Create after %s = %v, want ErrVMNotListed", name, err)
			}
		})
	}
}
