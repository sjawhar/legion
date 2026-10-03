// contract_test.go drives Client against the real broker (packages/envoy's own httptest server,
// mounted through its brokertest.NewRig) instead of client_test.go's fakeBroker: this is the
// byte-compatibility proof for jws.go, judged by the broker's own real record.VerifyRequestObject
// and proof.Verifier, not by a reimplementation of the wire contract by inspection. It skips
// without BROKER_TEST_DATABASE_URL (brokertest.NewRig's own skip).
//
// This is the only file in this module that imports anything under github.com/sjawhar/envoy/...:
// that import is test-only, never leaking into the daemon's production build.
//
// The rig itself lives at packages/envoy/internal/broker/brokertest, duplicated verbatim from
// Plan B's Task 4 (a sibling branch stacked on the same Plan A rig, not yet merged) — a
// deliberate, coordinated duplication rather than an accident, since Plan B and Plan C are
// independent siblings and neither should block on the other landing first. Whichever of the two
// PRs merges into main second must drop its own copy of that internal package in favor of the
// one main already carries.
//
// This file imports packages/envoy/brokertest instead of that internal package directly: Go's
// internal-package rule refuses to let github.com/sjawhar/legion/daemon (this module) import
// anything under github.com/sjawhar/envoy/internal/..., workspace-linked or not, so
// packages/envoy/brokertest is a thin public facade added specifically to cross that boundary
// (see its own doc comment). That facade is new, not part of Plan B's duplicated rig, and stays
// regardless of which plan's copy of the internal rig ultimately wins.
package agentsecrets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/brokertest"
)

// wireMachineLoginRecord is the small slice of POST /v1/machine-logins/lookup's response this
// test actually reads.
type wireMachineLoginRecord struct {
	RecordID string `json:"record_id"`
}

// approveMachineLogin drives the human-approval half of a machine login through the real UI
// routes exactly as packages/envoy/internal/broker/api/api_test.go's own
// TestMachineLoginApprovalMintsAKeyBoundLauncherCredentialForEnrollment does: look up the pending
// record by its confirmation code, then approve it with the same code and the operator's login,
// the body Dispatch's server sends.
func approveMachineLogin(t *testing.T, rig *brokertest.Rig, code string) {
	t.Helper()
	status, body := rig.UI(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": code})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/machine-logins/lookup = %d: %s", status, body)
	}
	var looked wireMachineLoginRecord
	if err := json.Unmarshal(body, &looked); err != nil || looked.RecordID == "" {
		t.Fatalf("decode machine-login lookup: %v (body: %s)", err, body)
	}
	status, body = rig.UI(t, http.MethodPost, "/v1/credential-requests/"+looked.RecordID+"/approve",
		map[string]any{"approver": rig.Operator, "code": code})
	if status != http.StatusOK {
		t.Fatalf("approve machine login = %d: %s", status, body)
	}
}

// expireLauncherCredential SQL-expires the one launcher credential this test's single Login
// minted — a fresh Rig never carries another launcher_credentials row, so this is the row's own
// identity check rather than a lookup by key_thumbprint.
func expireLauncherCredential(t *testing.T, rig *brokertest.Rig) {
	t.Helper()
	tag, err := rig.Store.Pool.Exec(context.Background(),
		`update launcher_credentials set expires_at = now() - interval '1 hour'`)
	if err != nil {
		t.Fatalf("expire launcher credential: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("expire launcher credential: affected %d rows, want exactly 1 (a fresh rig mints exactly one)", tag.RowsAffected())
	}
}

// TestContractMachineLoginEnrollRevokeExpireReenroll is Task 3's whole flow, driven against a
// real broker: Login mints a key and asks for a launcher credential; approving it through the
// real UI routes (the operator's login and the typed code, real record.VerifyRequestObject
// verification) moves LoginStatus to issued; Enroll registers a pod authenticated by a real k8s pod
// token against a real local OIDC issuer (brokertest.Rig's own PodVerifier, wired in NewRig);
// Revoke ends it and is idempotent; SQL-expiring the launcher credential and enrolling again proves
// doProof's automatic re-login: the broker answers 401 LAUNCHER_INVALID, the client starts a fresh
// Login, and Enroll surfaces NO_MACHINE_CREDENTIAL naming that fresh code. That second login is
// also approved and driven to issued before the test returns, so no poll goroutine outlives it.
func TestContractMachineLoginEnrollRevokeExpireReenroll(t *testing.T) {
	withFastPolling(t)
	rig := brokertest.NewRig(t)
	ctx := context.Background()
	client := &Client{URL: rig.URL, Operator: rig.Operator}

	code, err := client.Login(ctx)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	approveMachineLogin(t, rig, code)
	eventually(t, func() bool { return client.LoginStatus().State == "issued" },
		"LoginStatus reaches issued after approval")

	podToken := rig.MintPodToken(t, "pod-uid-1")
	enr, err := client.Enroll(ctx, PodEnrollment{PodUID: "pod-uid-1", Thumbprint: "tp-pod-1", PodToken: podToken})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if enr.ID == "" {
		t.Fatalf("Enroll returned an empty enrollment id")
	}

	if err := client.Revoke(ctx, enr.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if err := client.Revoke(ctx, enr.ID); err != nil {
		t.Fatalf("Revoke (idempotent retry of an already-revoked enrollment): %v", err)
	}

	expireLauncherCredential(t, rig)

	podToken2 := rig.MintPodToken(t, "pod-uid-2")
	_, err = client.Enroll(ctx, PodEnrollment{PodUID: "pod-uid-2", Thumbprint: "tp-pod-2", PodToken: podToken2})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "NO_MACHINE_CREDENTIAL" {
		t.Fatalf("Enroll (after the launcher credential expired) = %v, want *APIError{Code: NO_MACHINE_CREDENTIAL}", err)
	}
	newState := client.LoginStatus()
	if newState.State != "pending" {
		t.Fatalf("LoginStatus (after the automatic re-login) = %+v, want State=pending", newState)
	}
	if newState.Code == code {
		t.Fatalf("LoginStatus (after the automatic re-login) reused the original code %q, want a fresh one", code)
	}

	// Resolve the second login before returning: an unresolved pending login's poll goroutine
	// would otherwise keep running past this test (context.Background(), no terminal state),
	// racing withFastPolling's own t.Cleanup restore of the shared backoff variables — the exact
	// hazard client_test.go's TestExpiredCredentialTriggersAFreshLoginWithANewKey documents and
	// guards against the same way.
	approveMachineLogin(t, rig, newState.Code)
	eventually(t, func() bool { return client.LoginStatus().State == "issued" },
		"LoginStatus reaches issued after the second approval")
}
