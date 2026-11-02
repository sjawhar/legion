package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/store"
)

// fixtureVMAccount is the Google service account the VM rows below run as, fixtureVMRuntime one VM
// of it, keyed as enroll.VMClaims.RuntimeID keys one, and fixtureVMHost that VM's credential host,
// <project>/<zone>/<instance name>, as its token login records it.
const (
	fixtureVMAccount = "example-fleet@example-project.iam.gserviceaccount.com"
	fixtureVMRuntime = fixtureVMAccount + "@example-project/example-zone-a/1000000000000000001"
	fixtureVMHost    = "example-project/example-zone-a/example-vm-1"
)

// withVMFleet binds fixtureVMAccount to fixtureService in example-project for instances named
// example-vm-*, as BROKER_VM_SERVICES, BROKER_VM_PROJECTS and BROKER_VM_INSTANCES do.
func withVMFleet(m *Machine) {
	m.VMServicesByAccount = map[string]string{fixtureVMAccount: fixtureService}
	m.VMProjects = map[string]string{fixtureVMAccount: "example-project"}
	m.VMInstancePatterns = map[string]string{fixtureVMAccount: "example-vm-*"}
}

// newVMEnrollment inserts a live vm enrollment running as account, under a launcher credential
// naming service, no operator and fixtureVMHost: the shape a VM's own token login and enrollment
// make.
func newVMEnrollment(t *testing.T, st *store.Store, service, account, runtimeID string) (string, *ecdsa.PrivateKey) {
	t.Helper()
	id, key := insertEnrollment(t, st, "vm", runtimeID, nil, &service, &account)
	if _, err := st.Pool.Exec(context.Background(), `update launcher_credentials set host = $2
		where id = (select launcher_credential_id from enrollments where id = $1)`, id, fixtureVMHost); err != nil {
		t.Fatalf("set the VM credential's host: %v", err)
	}
	return id, key
}

// TestAVMsGrantStopsWhenItsFleetIsNarrowed pins that a VM is held to its fleet's project and
// instance names on every use, not only at its login and renewal: once BROKER_VM_PROJECTS names
// another project, or BROKER_VM_INSTANCES a glob its name does not match, its grant releases nothing
// and its next request finds no live enrollment at all (pgx.ErrNoRows), while its lease has not
// lapsed.
func TestAVMsGrantStopsWhenItsFleetIsNarrowed(t *testing.T) {
	for name, narrow := range map[string]func(*Machine){
		"another project":       func(m *Machine) { m.VMProjects = map[string]string{fixtureVMAccount: "example-other-project"} },
		"another instance glob": func(m *Machine) { m.VMInstancePatterns = map[string]string{fixtureVMAccount: "other-vm-*"} },
	} {
		t.Run(name, func(t *testing.T) {
			m, _, _, _ := newFixture(t)
			withVMFleet(m)
			ctx := context.Background()
			vm, vmKey := newVMEnrollment(t, m.Store, fixtureService, fixtureVMAccount, fixtureVMRuntime)
			granted, err := m.Create(ctx, vm, signRequest(t, m, vmKey, "need it", "SERVICE_KEY"), "")
			if err != nil || granted.State != "granted" || granted.GrantID == nil {
				t.Fatalf("Create(the fleet's VM) = %+v, %v; want granted", granted, err)
			}
			narrow(m)
			if values, _, err := m.Values(ctx, *granted.GrantID, vm, nil); !errors.Is(err, ErrGrantNotLive) {
				t.Fatalf("Values after the fleet was narrowed to %s = %v, %v; want ErrGrantNotLive", name, values, err)
			}
			if again, err := m.Create(ctx, vm, signRequest(t, m, vmKey, "need it again", "SHARED_KEY"), ""); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("Create after the fleet was narrowed to %s = %+v, %v; want pgx.ErrNoRows, no identity", name, again, err)
			}
		})
	}
}

// TestAServicesSecretGoesToItsOwnVMsAlone pins who a VM service's secret reaches: a VM enrolled
// under its own login for the service, running as the account BROKER_VM_SERVICES binds to that
// service, gets SERVICE_KEY at once, with a grant and no record, and its value is released, also
// after the policy moves. A VM its fleet does not allow — under the service's login running as
// another account, running as the bound account under another service's login, or running as an
// account BROKER_SERVICES binds for pods but BROKER_VM_SERVICES does not — has no identity at all
// (pgx.ErrNoRows, for any secret), and a pod whose subject is the VM account (a pod proves its
// service only through BROKER_SERVICES) is denied it with no record.
func TestAServicesSecretGoesToItsOwnVMsAlone(t *testing.T) {
	m, _, _, _ := newFixture(t)
	withVMFleet(m)
	ctx := context.Background()
	vm, vmKey := newVMEnrollment(t, m.Store, fixtureService, fixtureVMAccount, fixtureVMRuntime)

	granted, err := m.Create(ctx, vm, signRequest(t, m, vmKey, "need it", "SERVICE_KEY"), "")
	if err != nil || granted.State != "granted" || granted.GrantID == nil || granted.RecordID != nil {
		t.Fatalf("Create(the service's VM) = %+v, %v; want granted at once, with a grant and no record", granted, err)
	}
	if values, _, err := m.Values(ctx, *granted.GrantID, vm, nil); err != nil || values["SERVICE_KEY"] != "service-v1" {
		t.Fatalf("Values(the service's VM) = %v, %v; want SERVICE_KEY released", values, err)
	}
	retag(t, m, policytest.Secret("SHARED_TOKEN", policy.OwnerShared, policy.TierAgent, "shared-token-v1"))
	if values, _, err := m.Values(ctx, *granted.GrantID, vm, nil); err != nil || values["SERVICE_KEY"] != "service-v1" {
		t.Fatalf("Values(the service's VM) after the policy moved = %v, %v; want SERVICE_KEY still released", values, err)
	}

	impostor, impostorKey := newVMEnrollment(t, m.Store, fixtureService, "example-other@example-project.iam.gserviceaccount.com", "impostor-"+t.Name())
	unlisted, unlistedKey := newVMEnrollment(t, m.Store, "unlisted-service", fixtureVMAccount, "unlisted-"+t.Name())
	podAccountVM, podAccountVMKey := newVMEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-account-"+t.Name())
	vmAccountPod, vmAccountPodKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureVMAccount, "vm-account-pod-"+t.Name())
	for who, r := range map[string]struct {
		enrollment string
		key        *ecdsa.PrivateKey
	}{
		"a VM under the service's login running as another account":                {impostor, impostorKey},
		"a VM running as the bound account under an unlisted service":              {unlisted, unlistedKey},
		"a VM running as the account BROKER_SERVICES binds for the service's pods": {podAccountVM, podAccountVMKey},
	} {
		for _, secret := range []string{"SERVICE_KEY", "SHARED_KEY"} {
			if refused, err := m.Create(ctx, r.enrollment, signRequest(t, m, r.key, "need it", secret), ""); !errors.Is(err, pgx.ErrNoRows) {
				t.Errorf("Create(%s, %s) = %+v, %v; want pgx.ErrNoRows, no identity", who, secret, refused, err)
			}
		}
	}
	denied, err := m.Create(ctx, vmAccountPod, signRequest(t, m, vmAccountPodKey, "need it", "SERVICE_KEY"), "")
	if err != nil || denied.State != "denied" || denied.GrantID != nil || denied.RecordID != nil {
		t.Errorf("Create(a pod whose subject is the VM account) = %+v, %v; want denied with no grant and no record", denied, err)
	}
	var records int
	if err := m.Store.Pool.QueryRow(ctx, `select count(*) from credential_requests`).Scan(&records); err != nil || records != 0 {
		t.Fatalf("credential_requests rows = %d, %v; want none", records, err)
	}
}

// TestAVMGrantStopsWhenItsServiceAccountChanges pins that a VM's grant of its service's secret is
// re-checked on every release, as a pod's is: once BROKER_VM_SERVICES binds another account to the
// service and no longer the VM's, the VM's grant releases nothing and its next request finds no
// live enrollment, rather than reusing the grant.
func TestAVMGrantStopsWhenItsServiceAccountChanges(t *testing.T) {
	m, _, _, _ := newFixture(t)
	withVMFleet(m)
	ctx := context.Background()
	vm, vmKey := newVMEnrollment(t, m.Store, fixtureService, fixtureVMAccount, fixtureVMRuntime)
	granted, err := m.Create(ctx, vm, signRequest(t, m, vmKey, "need it", "SERVICE_KEY"), "")
	if err != nil || granted.State != "granted" || granted.GrantID == nil {
		t.Fatalf("Create(the service's VM) = %+v, %v; want granted", granted, err)
	}
	m.VMServicesByAccount = map[string]string{"example-other@example-project.iam.gserviceaccount.com": fixtureService}
	if values, _, err := m.Values(ctx, *granted.GrantID, vm, nil); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values after the service's account changed = %v, %v; want ErrGrantNotLive", values, err)
	}
	if again, err := m.Create(ctx, vm, signRequest(t, m, vmKey, "need it again", "SERVICE_KEY"), ""); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("Create after the service's account changed = %+v, %v; want pgx.ErrNoRows, not request %s reused", again, err, granted.ID)
	}
}
