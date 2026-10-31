package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/store"
)

// fixtureVMAccount is the Google service account the VM rows below run as, and fixtureVMRuntime
// one VM of it, keyed as enroll.VMClaims.RuntimeID keys one.
const (
	fixtureVMAccount = "example-fleet@example-project.iam.gserviceaccount.com"
	fixtureVMRuntime = fixtureVMAccount + "@example-project/example-zone-a/1000000000000000001"
)

// newVMEnrollment inserts a live vm enrollment running as account, under a launcher credential
// naming service and no operator: the shape a VM's own token login and enrollment make.
func newVMEnrollment(t *testing.T, st *store.Store, service, account, runtimeID string) (string, *ecdsa.PrivateKey) {
	t.Helper()
	return insertEnrollment(t, st, "vm", runtimeID, nil, &service, &account)
}

// TestAServicesSecretGoesToItsOwnVMsAlone pins who a VM service's secret reaches: a VM enrolled
// under its own login for the service, running as the account BROKER_VM_SERVICES binds to that
// service, gets SERVICE_KEY at once, with a grant and no record, and its value is released, also
// after the policy moves. A VM under the service's login running as another account, a VM running
// as the bound account under another service's login, a VM whose account BROKER_SERVICES binds for
// pods but BROKER_VM_SERVICES does not, and a pod whose subject is the VM account (a pod proves its
// service only through BROKER_SERVICES) are each denied it with no record.
func TestAServicesSecretGoesToItsOwnVMsAlone(t *testing.T) {
	m, _, _, _ := newFixture(t)
	m.VMServicesByAccount = map[string]string{fixtureVMAccount: fixtureService}
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
		"a VM under the service's login running as another account":                      {impostor, impostorKey},
		"a VM running as the bound account under an unlisted service":                    {unlisted, unlistedKey},
		"a VM running as the account BROKER_SERVICES binds for the service's pods":       {podAccountVM, podAccountVMKey},
		"a pod whose subject is the VM account BROKER_VM_SERVICES binds for its service": {vmAccountPod, vmAccountPodKey},
	} {
		denied, err := m.Create(ctx, r.enrollment, signRequest(t, m, r.key, "need it", "SERVICE_KEY"), "")
		if err != nil || denied.State != "denied" || denied.GrantID != nil || denied.RecordID != nil {
			t.Errorf("Create(%s) = %+v, %v; want denied with no grant and no record", who, denied, err)
		}
	}
	var records int
	if err := m.Store.Pool.QueryRow(ctx, `select count(*) from credential_requests`).Scan(&records); err != nil || records != 0 {
		t.Fatalf("credential_requests rows = %d, %v; want none", records, err)
	}
}

// TestAVMGrantStopsWhenItsServiceAccountChanges pins that a VM's grant of its service's secret is
// re-checked on every release, as a pod's is: once BROKER_VM_SERVICES binds another account to the
// service and no longer the VM's, the VM's grant releases nothing and its next request is denied
// rather than reusing the grant.
func TestAVMGrantStopsWhenItsServiceAccountChanges(t *testing.T) {
	m, _, _, _ := newFixture(t)
	m.VMServicesByAccount = map[string]string{fixtureVMAccount: fixtureService}
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
	again, err := m.Create(ctx, vm, signRequest(t, m, vmKey, "need it again", "SERVICE_KEY"), "")
	if err != nil || again.ID == granted.ID || again.State != "denied" || again.GrantID != nil {
		t.Fatalf("Create after the service's account changed = %+v, %v; want a new request, denied, not request %s reused", again, err, granted.ID)
	}
}
