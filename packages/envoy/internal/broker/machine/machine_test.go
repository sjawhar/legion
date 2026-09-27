package machine

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/approvers"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/rules"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/broker/webauthntest"
)

const (
	testAudience = "https://secrets.test"
	testOrigin   = "https://dispatch.test"
	testRPID     = "dispatch.test"
	testURL      = "https://secrets.test/v1/requests"
)

var testAAGUID = uuid.MustParse("ee882879-721c-4913-9775-3dfcce97072a")

// codePattern is the confirmation code's own shape: eight symbols from confirmationAlphabet
// (a 32-symbol subset of A-Z2-9 with the easily-confused characters removed) as XXXX-XXXX.
var codePattern = regexp.MustCompile(`^[A-Z2-9]{4}-[A-Z2-9]{4}$`)

// newFixture wires a Service against a fresh Postgres schema: a real approvers.Service with
// "sjawhar"'s key already seeded (so ApplyDecision's VerifyAssertion has a genuine key to check),
// a real enroll.Service wired with the matching ChainVerifier (so AuthenticateLauncher's own
// issuance-chain re-verification is the genuine thing, not a stub), and rules.Current loaded from
// a minimal valid rules file — a machine login's own decision never consults the rules (it always
// requires approval), so the fixture needs only a valid, versioned Set.
func newFixture(t *testing.T) (*Service, *webauthntest.Authenticator) {
	t.Helper()
	st := storetest.Open(t)
	ca := webauthntest.NewCA(t)
	verifier := &approvers.Verifier{Roots: ca.Pool(), Origin: testOrigin, AAGUIDs: map[uuid.UUID]bool{testAAGUID: true}}
	approversSvc := &approvers.Service{Store: st, Verifier: verifier}

	auth := ca.NewAuthenticator(t, testAAGUID)
	nonce := strings.Repeat("a", 64)
	challenge := record.RegisterChallenge("sjawhar", nonce)
	entry := approvers.KeyEntry{
		CredentialID:   base64RawURL(auth.CredentialID),
		ChallengeNonce: nonce,
		Registration:   auth.Register(t, testRPID, testOrigin, challenge[:]),
		Seed:           true,
	}
	if _, err := st.Pool.Exec(context.Background(), `insert into approver_key_seeds (login, credential_id) values ($1,$2)`, "sjawhar", entry.CredentialID); err != nil {
		t.Fatalf("insert approver_key_seeds fixture row: %v", err)
	}
	if err := approversSvc.Reconcile(context.Background(), map[string][]approvers.KeyEntry{"sjawhar": {entry}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	enr := &enroll.Service{Store: st, Lease: time.Hour}
	enr.Chain = enroll.NewChainVerifier(st, testAudience, time.Minute)

	rulesPath := t.TempDir() + "/rules.yaml"
	if err := os.WriteFile(rulesPath, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cur, err := rules.NewCurrent(context.Background(), rules.FileLoader{Path: rulesPath}, time.Hour, func(error) {})
	if err != nil {
		t.Fatalf("rules.NewCurrent: %v", err)
	}

	svc := &Service{
		Store: st, Enroll: enr, Approvers: approversSvc, Rules: cur,
		Audience: testAudience, Skew: time.Minute, PendingTTL: 10 * time.Minute, CredentialLifetime: 24 * time.Hour,
	}
	return svc, auth
}

// base64RawURL is the wire form a WebAuthn credential id takes throughout approvers.KeyEntry.
func base64RawURL(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// signMachineLogin builds a machine's own credential-request object for a launcher_credential
// login: identifier is the host, service (optional) distinguishes a service credential from a
// personal one exactly as record.AuthorizationDetail's own doc describes.
func signMachineLogin(t *testing.T, loginHint, identifier, service string) (compact string) {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	compact, err = record.Sign(key, testAudience, []record.AuthorizationDetail{
		{Type: "launcher_credential", Identifier: identifier, Service: service},
	}, "", loginHint, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return compact
}

// TestLoginIssuesAKeyBoundCredentialOnTypedCodeApproval drives the whole machine-login flow end
// to end: a machine signs its own request object, Login opens a pending record and mints a
// typed code, LookupByCode shows it (with challenges) to the approving human, ApplyDecision
// mints the credential once the human's assertion verifies, Read reports it issued — with no
// token anywhere in any response — and the resulting credential authenticates a
// proof.SignLauncher proof by the very key the machine signed its login with, never by another.
func TestLoginIssuesAKeyBoundCredentialOnTypedCodeApproval(t *testing.T) {
	svc, approverAuth := newFixture(t)
	ctx := context.Background()

	machineKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	compact, err := record.Sign(machineKey, testAudience, []record.AuthorizationDetail{
		{Type: "launcher_credential", Identifier: "sami-agents"},
	}, "", "sjawhar", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	pendingID, code, err := svc.Login(ctx, compact)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !codePattern.MatchString(code) {
		t.Fatalf("code = %q, want the shape XXXX-XXXX", code)
	}

	view, err := svc.LookupByCode(ctx, code)
	if err != nil {
		t.Fatalf("LookupByCode: %v", err)
	}
	if view.Host != "sami-agents" || view.Approver != "sjawhar" || view.State != "pending" {
		t.Fatalf("LookupByCode = %+v, want host sami-agents, approver sjawhar, state pending", view)
	}
	if len(view.ApproveChallenge) != 32 || len(view.DenyChallenge) != 32 {
		t.Fatalf("LookupByCode challenges = %d/%d bytes, want 32/32", len(view.ApproveChallenge), len(view.DenyChallenge))
	}

	assertion := approverAuth.Assert(t, testRPID, testOrigin, view.ApproveChallenge)
	state, credentialID, err := svc.ApplyDecision(ctx, view.RecordID, true, assertion, code)
	if err != nil {
		t.Fatalf("ApplyDecision: %v", err)
	}
	if state != "issued" || credentialID == "" {
		t.Fatalf("ApplyDecision = state=%q credentialID=%q, want issued and a credential id", state, credentialID)
	}

	readState, readCredentialID, err := svc.Read(ctx, pendingID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if readState != "issued" || readCredentialID != credentialID {
		t.Fatalf("Read = %q %q, want issued %q", readState, readCredentialID, credentialID)
	}

	// The key-bound credential authenticates a proof.SignLauncher proof by the same machine key
	// through AuthenticateLauncher's own issuance-chain re-verification, and refuses one by
	// another key.
	verifier := &proof.Verifier{
		Skew:           time.Minute,
		LookupLauncher: svc.Enroll.AuthenticateLauncher,
		Replay:         func(context.Context, string, time.Time) (bool, error) { return true, nil },
	}
	sameKeyProof, err := proof.SignLauncher(machineKey, credentialID, "POST", testURL, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(ctx, sameKeyProof, "POST", testURL, time.Now()); err != nil {
		t.Fatalf("proof by the enrolled key: %v", err)
	}

	otherKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	otherKeyProof, err := proof.SignLauncher(otherKey, credentialID, "POST", testURL, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(ctx, otherKeyProof, "POST", testURL, time.Now()); !errors.Is(err, proof.ErrInvalid) {
		t.Fatalf("proof by another key must fail, got %v", err)
	}
}

// TestApproveWithWrongCodeRefuses pins that ApplyDecision refuses a mismatched code before ever
// looking at the assertion.
func TestApproveWithWrongCodeRefuses(t *testing.T) {
	svc, approverAuth := newFixture(t)
	ctx := context.Background()

	compact := signMachineLogin(t, "sjawhar", "sami-agents", "")
	_, code, err := svc.Login(ctx, compact)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	view, err := svc.LookupByCode(ctx, code)
	if err != nil {
		t.Fatalf("LookupByCode: %v", err)
	}
	assertion := approverAuth.Assert(t, testRPID, testOrigin, view.ApproveChallenge)

	if _, _, err := svc.ApplyDecision(ctx, view.RecordID, true, assertion, "WRONG-CODE"); !errors.Is(err, ErrCodeMismatch) {
		t.Fatalf("ApplyDecision(wrong code) = %v, want ErrCodeMismatch", err)
	}
}

// TestServiceCredentialEnrollsOnlyPods pins that a login whose launcher_credential detail names a
// service mints a credential with a nil operator (never the approving human's own login), and
// that this new-flow-minted credential still respects enroll's own authorized() trust boundary:
// a service credential enrols pods only.
func TestServiceCredentialEnrollsOnlyPods(t *testing.T) {
	svc, approverAuth := newFixture(t)
	ctx := context.Background()

	compact := signMachineLogin(t, "sjawhar", "cluster", "legion-daemon")
	_, code, err := svc.Login(ctx, compact)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	view, err := svc.LookupByCode(ctx, code)
	if err != nil {
		t.Fatalf("LookupByCode: %v", err)
	}
	if view.Service != "legion-daemon" {
		t.Fatalf("view.Service = %q, want legion-daemon", view.Service)
	}

	assertion := approverAuth.Assert(t, testRPID, testOrigin, view.ApproveChallenge)
	state, credentialID, err := svc.ApplyDecision(ctx, view.RecordID, true, assertion, code)
	if err != nil {
		t.Fatalf("ApplyDecision: %v", err)
	}
	if state != "issued" {
		t.Fatalf("state = %q, want issued", state)
	}

	var operator *string
	var service string
	if err := svc.Store.Pool.QueryRow(ctx, `select operator, service from launcher_credentials where id=$1`, credentialID).Scan(&operator, &service); err != nil {
		t.Fatalf("read launcher_credentials: %v", err)
	}
	if operator != nil {
		t.Fatalf("operator = %v, want nil for a service credential", *operator)
	}
	if service != "legion-daemon" {
		t.Fatalf("service = %q, want legion-daemon", service)
	}

	cred := enroll.Credential{ID: uuid.MustParse(credentialID), Service: &service, Host: "cluster"}
	if _, err := svc.Enroll.Create(ctx, cred, enroll.Enrollment{Kind: "box", RuntimeID: "box-1", Operator: new("sjawhar"), Thumbprint: "tp-1"}); !errors.Is(err, enroll.ErrOperatorMismatch) {
		t.Fatalf("Create(service cred, kind box) = %v, want ErrOperatorMismatch", err)
	}
}

// TestAForgedCredentialRowAuthenticatesNothing pins the security property enroll.Service.Chain
// exists for: a launcher_credentials row with no backing credential-request record — however
// live, unrevoked, and unexpired it looks — never authenticates.
func TestAForgedCredentialRowAuthenticatesNothing(t *testing.T) {
	svc, _ := newFixture(t)
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
	id := uuid.New()
	if _, err := svc.Store.Pool.Exec(ctx, `insert into launcher_credentials (id, host, key_thumbprint, public_jwk, expires_at) values ($1,'forged-host',$2,$3, now() + interval '1 hour')`,
		id, thumbprint, []byte(jwk)); err != nil {
		t.Fatalf("insert forged launcher_credentials row: %v", err)
	}

	if _, live, err := svc.Enroll.AuthenticateLauncher(ctx, id.String()); err != nil || live {
		t.Fatalf("AuthenticateLauncher(forged row, no record) = live=%v err=%v, want live=false, err=nil", live, err)
	}
}

// TestExpiredCredentialRefuses pins that a launcher credential past its own expires_at no longer
// authenticates, even with a perfectly valid issuance chain behind it.
func TestExpiredCredentialRefuses(t *testing.T) {
	svc, approverAuth := newFixture(t)
	ctx := context.Background()

	compact := signMachineLogin(t, "sjawhar", "sami-agents", "")
	_, code, err := svc.Login(ctx, compact)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	view, err := svc.LookupByCode(ctx, code)
	if err != nil {
		t.Fatalf("LookupByCode: %v", err)
	}
	assertion := approverAuth.Assert(t, testRPID, testOrigin, view.ApproveChallenge)
	_, credentialID, err := svc.ApplyDecision(ctx, view.RecordID, true, assertion, code)
	if err != nil {
		t.Fatalf("ApplyDecision: %v", err)
	}

	if _, err := svc.Store.Pool.Exec(ctx, `update launcher_credentials set expires_at = now() - interval '1 minute' where id=$1`, credentialID); err != nil {
		t.Fatalf("backdate expires_at: %v", err)
	}

	if _, live, err := svc.Enroll.AuthenticateLauncher(ctx, credentialID); err != nil || live {
		t.Fatalf("AuthenticateLauncher(expired) = live=%v err=%v, want live=false, err=nil", live, err)
	}
}

// TestExpirePendingMarksOverdueLoginsExpired exercises ExpirePending's own contract: a record
// past its own expires_at with no decision gets one 'expired' event and reads back as such,
// while a decided record is left alone.
func TestExpirePendingMarksOverdueLoginsExpired(t *testing.T) {
	svc, approverAuth := newFixture(t)
	ctx := context.Background()

	overdueCompact := signMachineLogin(t, "sjawhar", "overdue-host", "")
	overduePending, _, err := svc.Login(ctx, overdueCompact)
	if err != nil {
		t.Fatalf("Login(overdue): %v", err)
	}

	decidedCompact := signMachineLogin(t, "sjawhar", "decided-host", "")
	_, decidedCode, err := svc.Login(ctx, decidedCompact)
	if err != nil {
		t.Fatalf("Login(decided): %v", err)
	}
	decidedView, err := svc.LookupByCode(ctx, decidedCode)
	if err != nil {
		t.Fatalf("LookupByCode(decided): %v", err)
	}
	assertion := approverAuth.Assert(t, testRPID, testOrigin, decidedView.DenyChallenge)
	if _, _, err := svc.ApplyDecision(ctx, decidedView.RecordID, false, assertion, decidedCode); err != nil {
		t.Fatalf("ApplyDecision(deny decided): %v", err)
	}

	if err := svc.ExpirePending(ctx, time.Now().Add(svc.PendingTTL+time.Minute)); err != nil {
		t.Fatalf("ExpirePending: %v", err)
	}

	if state, _, err := svc.Read(ctx, overduePending); err != nil || state != "expired" {
		t.Fatalf("Read(overdue) = %q, %v, want expired", state, err)
	}
	if state, _, err := svc.recordState(ctx, decidedView.RecordID); err != nil || state != "denied" {
		t.Fatalf("recordState(decided) = %q, %v, want denied (ExpirePending must not overwrite a real decision)", state, err)
	}
}
