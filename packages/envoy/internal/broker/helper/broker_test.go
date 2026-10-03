// packages/envoy/internal/broker/helper/broker_test.go
//go:build linux

package helper

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// failAlways, set as a failure count (failFirst, revokeFailFirst), fails every such call a test
// can make.
const failAlways = 1 << 30

// fakeBroker records enrollment traffic. It answers 201 for a new thumbprint, 200 for a repeat
// (the contract's idempotent enroll), 401 LAUNCHER_INVALID for a missing or forced-invalid Proof
// header, and verifies renew proofs against the thumbprint it enrolled. leaseExpiry models the
// real broker's enrollment row surviving its own lease: Renew refuses (401) once the current time
// is past the enrollment's stored expiry, WITHOUT deleting anything, and a repeat Enroll for that
// same thumbprint (the idempotent-enroll conflict path) answers with the SAME enrollment id and
// the SAME stored (possibly already-expired) lease rather than a freshly computed one — the real
// bug C2's fix works around. runtimeIDToID additionally models the real broker's OTHER
// idempotent-enroll conflict key, (launcher_credential_id, runtime_id) rather than thumbprint
// (AGENTC-834 thermonuclear finding on Recover's ordering): an Enroll whose runtime_id already
// has a different, still-live enrollment id refuses with 409 ALREADY_ENROLLED, whatever
// thumbprint it carries — exactly the conflict a fresh Recover enroll racing ahead of the old
// row's revoke can hit for real, which byTP alone (keyed on the NEW session's fresh thumbprint)
// can never reproduce. conflicts counts how many times that 409 fired, so a test can assert none
// ever did. revokeFirstDelay, when set, delays the FIRST DELETE this fake receives (dropped to
// zero for every later one) so a test can hold an old row "live" for a window and prove the
// helper never calls Enroll for that runtime_id before the delayed DELETE actually clears it —
// without holding f.mu for the delay itself, which would just serialize the racing Enroll behind
// the DELETE instead of letting a genuinely concurrent (buggy) caller collide with it.
// revokeFailFirst, when set, answers the next N DELETEs with 503 before any of them touches the
// row — an unreachable broker, as opposed to revokeFirstDelay's merely slow one — so a revoke
// stuck retrying with backoff can be observed and torn down mid-retry. revokeAttempts counts
// every DELETE this fake received, whether it failed or actually deleted the row, independent of
// revokeFailFirst, so a test can poll for exactly when an attempt has been answered instead of
// guessing with a raw sleep. revokeForbidden answers a DELETE of each id it names with 403
// OPERATOR_MISMATCH, as the broker does for an enrollment made under another operator's launcher
// credential.
//
// The launcher half (Enroll, Revoke) now authenticates with a Proof header carrying an "lid"
// claim instead of a bearer token (AGENTC-834 Task 1): enrollUnauthorizedNext simulates an
// expired or revoked credential (401 LAUNCHER_INVALID) independent of failFirst's unrelated 503.
// The fake also serves the machine-login routes a Broker.Login talks to: POST
// /v1/launcher-credentials verifies the posted request object with record.VerifyRequestObject and
// mints a pending id and confirmation code; GET /v1/launcher-credentials/{pending} answers
// loginOutcome ("issued" by default, or "denied"/"expired" when a test sets it before Login, or
// "pending" to hold the login undecided until the test sets "issued"), minting a fresh credential
// id on "issued".
type fakeBroker struct {
	mu               sync.Mutex
	srv              *httptest.Server
	enrolled         map[string]string    // enrollment id -> thumbprint
	byTP             map[string]string    // thumbprint -> enrollment id
	runtimeIDToID    map[string]string    // runtime_id -> the currently live enrollment id for it
	leaseExpiry      map[string]time.Time // enrollment id -> its current lease expiry
	posts            []map[string]any
	deletes          []string
	renews           int
	conflicts        int           // 409 ALREADY_ENROLLED answers from the runtime_id-keyed conflict path
	failFirst        int           // 503 this many enroll calls first
	revokeFirstDelay time.Duration // sleep this long before the first DELETE actually removes its row
	revokeFailFirst  int           // 503 this many DELETE calls first, row untouched (an unreachable broker)
	revokeAttempts   int           // every DELETE this fake received, failed or not
	lease            time.Duration // lease length the fake grants; 900 s unless a test shortens it
	seen             map[string]bool
	revokeForbidden  map[string]bool
	next             int // ids are minted, never derived from the key, like the broker's uuids

	lastAuthorization      string // the Authorization header the most recent Enroll call carried; must stay empty
	enrollUnauthorizedNext int    // fail the next N Enroll calls with 401 LAUNCHER_INVALID (an expired/revoked credential)
	lastProof              string // the Proof header the most recent Enroll call carried
	lastProofURL           string // the exact URL that Proof header was signed for

	lastLoginRequest   string // the compact request object the most recent POST /v1/launcher-credentials carried
	pendingThumbprint  string // the embedded key's thumbprint from that request object
	pendingID          string // the opaque id minted for the most recent login
	pendingCode        string // the confirmation code minted for the most recent login
	loginOutcome       string // what the pending login's poll answers: "issued", "pending", "denied", or "expired"
	issuedCredentialID string // the launcher credential id minted for the most recent "issued" login
	// loginLifetime is how long an issued credential lives: the poll answers expires_at that far
	// past the poll, a week unless a test shortens it; zero answers no expires_at, as a broker from
	// before it does.
	loginLifetime time.Duration

	// refuseRenewNext answers the next N renews 401 LEASE_EXPIRED whatever the lease, so a test
	// lapses a session on its next renew without racing the lease's wall clock. A renew that
	// renewFail also covers gets renewFail's 503 instead, since renewFail is checked first.
	refuseRenewNext int
	// enrollGate, when set, holds every enroll POST until the test closes it, so a test can keep a
	// session enrolling for exactly as long as it needs.
	enrollGate chan struct{}
	// enrollArrivals counts every enroll POST this fake received, on arrival and before any gate, so
	// a test can tell that an enroll is waiting at the gate.
	enrollArrivals int
	// renewGate, when set, holds every renew until the test closes it. A renew reads the gate as it
	// arrives, so a test can hold one renew, swap in another gate for the next, and release them
	// one at a time.
	renewGate chan struct{}
	// renewAttempts counts every renew this fake received, on arrival and before any gate, so a
	// test can tell that a renew is waiting at the gate.
	renewAttempts int
	// renewFail answers the next N renews 503 without touching the lease: an outage, not a
	// refusal. It is checked before refuseRenewNext, so with both set the 503s come first.
	renewFail int
	// nextLease, when set, is the lease the next new enrollment gets, and that enrollment clears
	// it, so every later one gets lease. A test gives one enrollment a lease short enough that it
	// renews soon, and nothing it re-enrolls can lapse under the test however the steps interleave.
	nextLease time.Duration
}

func newFakeBroker(t *testing.T) *fakeBroker {
	f := &fakeBroker{
		enrolled: map[string]string{}, byTP: map[string]string{}, runtimeIDToID: map[string]string{},
		leaseExpiry: map[string]time.Time{}, seen: map[string]bool{}, lease: 900 * time.Second,
		loginOutcome: "issued", loginLifetime: 7 * 24 * time.Hour,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/enrollments", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.enrollArrivals++
		gate := f.enrollGate
		f.mu.Unlock()
		if gate != nil {
			<-gate
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lastAuthorization = r.Header.Get("Authorization")
		f.lastProof, f.lastProofURL = r.Header.Get("Proof"), f.srv.URL+"/v1/enrollments"
		if f.lastProof == "" || f.enrollUnauthorizedNext > 0 {
			if f.enrollUnauthorizedNext > 0 {
				f.enrollUnauthorizedNext--
			}
			writeJSON(w, 401, map[string]string{"code": "LAUNCHER_INVALID", "error": "the launcher credential is not valid"})
			return
		}
		if f.failFirst > 0 {
			f.failFirst--
			writeJSON(w, 503, map[string]string{"code": "DATABASE", "error": "postgres unreachable"})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.posts = append(f.posts, body)
		tp, _ := body["thumbprint"].(string)
		if id, ok := f.byTP[tp]; ok {
			// The idempotent-enroll conflict path: the real broker answers with the SAME id and
			// its CURRENT stored lease, which may already be in the past — it does not refresh
			// it, which is exactly what lets a lapsed id keep coming back forever until it is
			// explicitly revoked.
			writeJSON(w, 200, map[string]string{"enrollment_id": id, "lease_expires_at": f.leaseExpiry[id].UTC().Format(time.RFC3339Nano)})
			return
		}
		rid, _ := body["runtime_id"].(string)
		if existingID, ok := f.runtimeIDToID[rid]; ok {
			if _, live := f.enrolled[existingID]; live {
				// The runtime_id-keyed conflict path (enroll.go's recoverConflict): a different
				// key for a runtime that already has a live row is refused outright, not handed
				// a fresh enrollment.
				f.conflicts++
				writeJSON(w, 409, map[string]string{"code": "ALREADY_ENROLLED", "error": "this runtime is already enrolled and live"})
				return
			}
		}
		f.next++
		id := fmt.Sprintf("enr-%d", f.next)
		d := f.lease
		if f.nextLease > 0 {
			d, f.nextLease = f.nextLease, 0
		}
		lease := time.Now().Add(d)
		f.enrolled[id], f.byTP[tp] = tp, id
		f.leaseExpiry[id] = lease
		f.runtimeIDToID[rid] = id
		writeJSON(w, 201, map[string]string{"enrollment_id": id, "lease_expires_at": lease.UTC().Format(time.RFC3339Nano)})
	})
	mux.HandleFunc("DELETE /v1/enrollments/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proof") == "" {
			writeJSON(w, 401, map[string]string{"code": "LAUNCHER_INVALID", "error": "no proof presented"})
			return
		}
		f.mu.Lock()
		f.revokeAttempts++
		if f.revokeForbidden[r.PathValue("id")] {
			f.mu.Unlock()
			writeJSON(w, 403, map[string]string{"code": "OPERATOR_MISMATCH", "error": "the enrollment belongs to another operator"})
			return
		}
		if f.revokeFailFirst > 0 {
			f.revokeFailFirst--
			f.mu.Unlock()
			writeJSON(w, 503, map[string]string{"code": "DATABASE", "error": "postgres unreachable"})
			return
		}
		delay := f.revokeFirstDelay
		f.revokeFirstDelay = 0
		f.mu.Unlock()
		if delay > 0 {
			// Deliberately outside the lock: a delayed DELETE must let a concurrent (buggy)
			// Enroll for the same runtime_id observe the row as still live during this window,
			// which holding f.mu here would instead serialize away.
			time.Sleep(delay)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		id := r.PathValue("id")
		f.deletes = append(f.deletes, id)
		if tp, ok := f.enrolled[id]; ok {
			delete(f.byTP, tp)
			delete(f.enrolled, id)
			delete(f.leaseExpiry, id)
			w.WriteHeader(204)
			return
		}
		w.WriteHeader(404)
	})
	mux.HandleFunc("POST /v1/enrollments/{id}/renew", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.renewAttempts++
		gate := f.renewGate
		f.mu.Unlock()
		if gate != nil {
			<-gate
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		id := r.PathValue("id")
		if f.renewFail > 0 {
			f.renewFail--
			writeJSON(w, 503, map[string]string{"code": "DATABASE", "error": "postgres unreachable"})
			return
		}
		if f.refuseRenewNext > 0 {
			f.refuseRenewNext--
			writeJSON(w, 401, map[string]string{"code": "LEASE_EXPIRED", "error": "the lease has expired"})
			return
		}
		v := &proof.Verifier{
			Skew: time.Minute,
			Lookup: func(_ context.Context, eid string) (string, bool, error) {
				tp, ok := f.enrolled[eid]
				return tp, ok, nil
			},
			Replay: func(_ context.Context, jti string, _ time.Time) (bool, error) {
				if f.seen[jti] {
					return false, nil
				}
				f.seen[jti] = true
				return true, nil
			},
		}
		if sub, err := v.Verify(r.Context(), r.Header.Get("Proof"), "POST", f.srv.URL+"/v1/enrollments/"+id+"/renew", time.Now()); err != nil || sub.EnrollmentID != id {
			writeJSON(w, 401, map[string]string{"code": "PROOF_INVALID", "error": "proof invalid"})
			return
		}
		// The enrollment row itself survives its own lease — a renew past that stored expiry is
		// refused, but nothing about the enrollment is deleted; only Revoke deletes it.
		if exp, ok := f.leaseExpiry[id]; ok && !time.Now().Before(exp) {
			writeJSON(w, 401, map[string]string{"code": "LEASE_EXPIRED", "error": "the lease has expired"})
			return
		}
		f.renews++
		lease := time.Now().Add(f.lease)
		f.leaseExpiry[id] = lease
		writeJSON(w, 200, map[string]string{"lease_expires_at": lease.UTC().Format(time.RFC3339Nano)})
	})
	mux.HandleFunc("POST /v1/launcher-credentials", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body struct {
			Request string `json:"request"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.lastLoginRequest = body.Request
		ro, err := record.VerifyRequestObject(body.Request, f.srv.URL, time.Minute, time.Now())
		if err != nil {
			writeJSON(w, 400, map[string]string{"code": "INVALID_REQUEST", "error": err.Error()})
			return
		}
		f.pendingThumbprint = ro.Thumbprint
		f.next++
		f.pendingID = fmt.Sprintf("pending-%d", f.next)
		f.pendingCode = fakeConfirmationCode()
		writeJSON(w, http.StatusAccepted, map[string]string{"pending_id": f.pendingID, "code": f.pendingCode})
	})
	mux.HandleFunc("GET /v1/launcher-credentials/{pending}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.PathValue("pending") != f.pendingID {
			w.WriteHeader(404)
			return
		}
		switch f.loginOutcome {
		case "denied", "expired", "pending":
			writeJSON(w, 200, map[string]string{"state": f.loginOutcome})
			return
		default:
			f.next++
			f.issuedCredentialID = fmt.Sprintf("lcred-%d", f.next)
			issued := map[string]string{"state": "issued", "credential_id": f.issuedCredentialID}
			if f.loginLifetime != 0 {
				issued["expires_at"] = time.Now().Add(f.loginLifetime).UTC().Format(time.RFC3339Nano)
			}
			writeJSON(w, 200, issued)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// fakeConfirmationCode mints an 8-symbol confirmation code as XXXX-XXXX, matching the shape (if
// not the exact alphabet) the real broker's machine.Service mints.
func fakeConfirmationCode() string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	code := make([]byte, 8)
	for i, v := range raw {
		code[i] = alphabet[int(v)%len(alphabet)]
	}
	return string(code[:4]) + "-" + string(code[4:])
}

// lastProofSubject verifies the Proof header the fake's most recent Enroll call carried against
// the thumbprint of the key that logged in for the credential this fake issued — exactly as the
// real broker's AuthenticateLauncher would, via proof.Verifier.LookupLauncher — and returns its
// Subject. It fails the test on any verification error.
func (f *fakeBroker) lastProofSubject(t *testing.T) proof.Subject {
	t.Helper()
	f.mu.Lock()
	compact, url, launcherID, thumbprint := f.lastProof, f.lastProofURL, f.issuedCredentialID, f.pendingThumbprint
	f.mu.Unlock()
	if compact == "" {
		t.Fatal("no Proof header was recorded")
	}
	v := &proof.Verifier{
		Skew: time.Minute,
		LookupLauncher: func(_ context.Context, id string) (string, bool, error) {
			if id != launcherID {
				return "", false, nil
			}
			return thumbprint, true, nil
		},
		Replay: func(_ context.Context, _ string, _ time.Time) (bool, error) { return true, nil },
	}
	sub, err := v.Verify(context.Background(), compact, http.MethodPost, url, time.Now())
	if err != nil {
		t.Fatalf("verify proof: %v", err)
	}
	return sub
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// snapshot copies what the fake recorded, under its lock: tests read it from another goroutine
// than the handlers write it, and -race sees no happens-before across a socket.
func (f *fakeBroker) snapshot() (posts []map[string]any, deletes []string, renews int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.posts...), append([]string(nil), f.deletes...), f.renews
}

func operatorFile(t *testing.T, operator string) string {
	t.Helper()
	dir := t.TempDir()
	of := filepath.Join(dir, "operator")
	if err := os.WriteFile(of, []byte(operator+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return of
}

// loggedInBroker builds a Broker against f, logs it in against the fake's default "issued"
// outcome, and waits for the credential to install, so a test can exercise Enroll/Renew/Revoke
// without exercising Login itself.
func loggedInBroker(t *testing.T, f *fakeBroker, operator string) *Broker {
	t.Helper()
	b := &Broker{URL: f.srv.URL, OperatorFile: operatorFile(t, operator), HTTP: f.srv.Client()}
	if _, err := b.Login(context.Background(), "helper-host"); err != nil {
		t.Fatalf("login: %v", err)
	}
	waitFor(t, func() bool { return b.LoginStatus().State == "issued" })
	return b
}

// waitFor polls cond until it is true or 2s pass, whichever comes first, failing the test on
// timeout.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

func TestLoginHoldsTheKeyInMemoryAndEnrollSignsLauncherProofs(t *testing.T) {
	f := newFakeBroker(t) // records the login request object; issues after one poll
	b := &Broker{URL: f.srv.URL, OperatorFile: operatorFile(t, "ada@example.com"), HTTP: f.srv.Client()}
	code, err := b.Login(context.Background(), "example-host-devbox")
	if err != nil || !regexp.MustCompile(`^[A-Z2-9]{4}-[A-Z2-9]{4}$`).MatchString(code) {
		t.Fatalf("code %q err %v", code, err)
	}
	ro, err := record.VerifyRequestObject(f.lastLoginRequest, f.srv.URL, time.Minute, time.Now())
	if err != nil || ro.LoginHint != "ada@example.com" || ro.Details[0].Type != "launcher_credential" || ro.Details[0].Identifier != "example-host-devbox" {
		t.Fatalf("request object: %+v %v", ro, err)
	}
	waitFor(t, func() bool { return b.LoginStatus().State == "issued" })
	sess, _ := newSession(1234, 77, "h:1234:77", nil)
	if _, _, err := b.Enroll(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if f.lastAuthorization != "" {
		t.Fatalf("a bearer header was sent: %q", f.lastAuthorization)
	}
	sub := f.lastProofSubject(t) // fake verifies the Proof JWS with the login request's embedded key
	if sub.LauncherID != f.issuedCredentialID {
		t.Fatalf("proof lid %q, want %q", sub.LauncherID, f.issuedCredentialID)
	}
}

// TestNoCredentialAndExpiredCredentialBothNameTheLoginCommand covers the fail-closed contract:
// Enroll before any Login names the login command; once a broker 401 LAUNCHER_INVALID clears the
// credential (expired or revoked), the very next Enroll names it again, with no auto-relogin. The
// machine key and credential id are asserted to never touch disk by walking a temp HOME.
func TestNoCredentialAndExpiredCredentialBothNameTheLoginCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	f := newFakeBroker(t)
	b := &Broker{URL: f.srv.URL, OperatorFile: operatorFile(t, "ada@example.com"), HTTP: f.srv.Client()}
	sess, _ := newSession(1, 1, "h:1:1", nil)

	if _, _, err := b.Enroll(context.Background(), sess); err == nil || !strings.Contains(err.Error(), "agent-secrets launcher login") {
		t.Fatalf("no credential yet: expected the remedy in the error, got %v", err)
	}

	if _, err := b.Login(context.Background(), "example-host-devbox"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.LoginStatus().State == "issued" })
	if _, _, err := b.Enroll(context.Background(), sess); err != nil {
		t.Fatalf("a fresh credential must enroll: %v", err)
	}

	f.mu.Lock()
	f.enrollUnauthorizedNext = 1
	f.mu.Unlock()
	if _, _, err := b.Enroll(context.Background(), sess); err == nil || !strings.Contains(err.Error(), "agent-secrets launcher login") {
		t.Fatalf("expired credential: expected the remedy in the error, got %v", err)
	}
	if b.cred.Load() != nil {
		t.Fatal("a 401 LAUNCHER_INVALID must clear the credential")
	}
	if _, _, err := b.Enroll(context.Background(), sess); err == nil || !strings.Contains(err.Error(), "agent-secrets launcher login") {
		t.Fatalf("cleared credential: expected the remedy again, got %v", err)
	}

	files := 0
	if err := filepath.WalkDir(home, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if files != 0 {
		t.Fatalf("the machine key and credential id must never touch disk; found %d file(s) under HOME", files)
	}
}

// TestALateRejectionOfAnOldCredentialLeavesTheNewLoginAlone covers a 401 LAUNCHER_INVALID that
// answers a call signed with a credential another login has since replaced (a session's enroll
// retry in flight while the operator logs in again): it clears nothing, since the credential it
// rejects is no longer the one installed, and the new login still reports "issued". The caller
// gets the broker's refusal rather than errNoCredential, since the helper holds a credential and
// its retry can use it.
func TestALateRejectionOfAnOldCredentialLeavesTheNewLoginAlone(t *testing.T) {
	f := newFakeBroker(t)
	b := loggedInBroker(t, f, "ada@example.com")
	old := b.cred.Load()
	if _, err := b.Login(context.Background(), "helper-host"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.cred.Load() != old && b.LoginStatus().State == "issued" })
	fresh := b.cred.Load()

	rejected := &BrokerError{Status: http.StatusUnauthorized, Code: "LAUNCHER_INVALID", Message: "the launcher credential is not valid"}
	if err := b.clearOnInvalid(old, rejected); err != rejected {
		t.Fatalf("a rejection of a replaced credential reached its caller as %v; want the broker's refusal as it came", err)
	}
	if b.cred.Load() != fresh || b.LoginStatus().State != "issued" {
		t.Fatalf("a late rejection of the replaced credential changed the new login: cred replaced %v, state %q", b.cred.Load() != fresh, b.LoginStatus().State)
	}

	if err := b.clearOnInvalid(fresh, rejected); !errors.Is(err, errNoCredential) {
		t.Fatal(err)
	}
	if b.cred.Load() != nil || b.LoginStatus().State == "issued" {
		t.Fatalf("rejecting the installed credential must clear it and end its login: state %q", b.LoginStatus().State)
	}
}

// TestLoginStatusReadsARefusalWithTheClearThatCausedIt: a refusal clears the credential and
// records that the broker refused it in one write under stateMu. A login-status read between the
// two would find no credential and no refusal, and report the denied re-login before it as the
// answer, where the answer is the refused credential. The test hook reads login-status between the
// clear and the record; it waits up to 200 ms for that read, which returns at once unless the lock
// holds it back until the refusal is recorded.
func TestLoginStatusReadsARefusalWithTheClearThatCausedIt(t *testing.T) {
	f := newFakeBroker(t)
	b := loggedInBroker(t, f, "ada@example.com")
	held := b.cred.Load()
	f.mu.Lock()
	f.loginOutcome = "denied"
	f.mu.Unlock()
	if _, err := b.Login(context.Background(), "helper-host"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.LoginStatus().State == "denied" })
	read := make(chan loginState, 1)
	b.testClearHook = func() {
		go func() { read <- b.LoginStatus() }()
		select {
		case got := <-read:
			read <- got
		case <-time.After(200 * time.Millisecond):
		}
	}
	rejected := &BrokerError{Status: http.StatusUnauthorized, Code: "LAUNCHER_INVALID", Message: "the launcher credential is not valid"}
	if err := b.clearOnInvalid(held, rejected); !errors.Is(err, errNoCredential) {
		t.Fatal(err)
	}
	if got := <-read; got.State != "expired" || !got.Refused || got.CredentialHeld {
		t.Fatalf("login-status read as the credential was refused after a denied re-login: %+v, want expired, refused, no credential", got)
	}
}

// TestALoginPendingWhenTheCredentialIsRefusedReportsItsOwnOutcome: a re-login is waiting for
// approval when the broker refuses the credential an earlier login installed. While it waits,
// login-status reads it pending and the credential refused. Once the operator denies it, it reads
// denied and no longer refused: its outcome is newer than the refusal, and `launcher login`, which
// reads the same answer, must tell the operator it was denied.
func TestALoginPendingWhenTheCredentialIsRefusedReportsItsOwnOutcome(t *testing.T) {
	f := newFakeBroker(t)
	b := loggedInBroker(t, f, "ada@example.com")
	held := b.cred.Load()
	f.mu.Lock()
	f.loginOutcome = "pending"
	f.mu.Unlock()
	if _, err := b.Login(context.Background(), "helper-host"); err != nil {
		t.Fatal(err)
	}
	rejected := &BrokerError{Status: http.StatusUnauthorized, Code: "LAUNCHER_INVALID", Message: "the launcher credential is not valid"}
	if err := b.clearOnInvalid(held, rejected); !errors.Is(err, errNoCredential) {
		t.Fatal(err)
	}
	if got := b.LoginStatus(); got.State != "pending" || !got.Refused || got.CredentialHeld {
		t.Fatalf("a re-login pending when the credential was refused: %+v, want pending, refused, no credential", got)
	}
	f.mu.Lock()
	f.loginOutcome = "denied"
	f.mu.Unlock()
	deadline := time.Now().Add(15 * time.Second) // pollLogin backs off 2 s, then 4 s, between polls
	for b.login.Load().State == "pending" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := b.LoginStatus(); got.State != "denied" || got.Refused || got.CredentialHeld {
		t.Fatalf("that re-login once denied: %+v, want denied, not refused, no credential", got)
	}
}

// TestDeniedAndExpiredLoginsSurfaceTheirState drives the fake through both terminal non-issued
// outcomes and confirms a later Login, once the prior one is no longer pending, starts completely
// fresh rather than reusing the denied/expired attempt's key.
func TestDeniedAndExpiredLoginsSurfaceTheirState(t *testing.T) {
	f := newFakeBroker(t)
	b := &Broker{URL: f.srv.URL, OperatorFile: operatorFile(t, "ada@example.com"), HTTP: f.srv.Client()}

	f.mu.Lock()
	f.loginOutcome = "denied"
	f.mu.Unlock()
	if _, err := b.Login(context.Background(), "host-a"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.LoginStatus().State == "denied" })
	ro1, err := record.VerifyRequestObject(f.lastLoginRequest, f.srv.URL, time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	f.loginOutcome = "expired"
	f.mu.Unlock()
	if _, err := b.Login(context.Background(), "host-b"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.LoginStatus().State == "expired" })
	if b.LoginStatus().Refused {
		t.Fatal("a pending login nobody approved in time is expired, not a refused credential")
	}
	ro2, err := record.VerifyRequestObject(f.lastLoginRequest, f.srv.URL, time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	if ro1.Thumbprint == ro2.Thumbprint {
		t.Fatalf("a second login must start with a fresh key: same thumbprint %q", ro1.Thumbprint)
	}
	if b.cred.Load() != nil {
		t.Fatal("a denied or expired login must never install a credential")
	}
}

func TestEnrollSendsKindHostAndAcceptsBothStatuses(t *testing.T) {
	f := newFakeBroker(t)
	b := loggedInBroker(t, f, "ada@example.com")
	sess, _ := newSession(1234, 77, "example-host-devbox:1234:77", nil)
	id, lease, err := b.Enroll(context.Background(), sess)
	if err != nil || id == "" || time.Until(lease) < 10*time.Minute {
		t.Fatalf("enroll: %q %v %v", id, lease, err)
	}
	posts, _, _ := f.snapshot()
	post := posts[0]
	if post["kind"] != "host" || post["runtime_id"] != "example-host-devbox:1234:77" || post["operator"] != "ada@example.com" ||
		post["thumbprint"] != sess.Thumbprint || post["approver"] != nil ||
		post["session_id"] != nil || post["pod_token"] != nil {
		t.Fatalf("enroll body: %+v", post)
	}
	again, _, err := b.Enroll(context.Background(), sess)
	if err != nil || again != id {
		t.Fatalf("a repeat enroll must return the same enrollment (200): %q %v", again, err)
	}
}

func TestRenewSignsWithTheSessionKeyAndRevokeIsIdempotent(t *testing.T) {
	f := newFakeBroker(t)
	b := loggedInBroker(t, f, "ada@example.com")
	sess, _ := newSession(1, 1, "h:1:1", nil)
	id, _, err := b.Enroll(context.Background(), sess)
	if err != nil {
		t.Fatal(err)
	}
	sess.setEnrolled(id)
	if _, err := b.Renew(context.Background(), sess); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if _, _, renews := f.snapshot(); renews != 1 {
		t.Fatalf("renews %d", renews)
	}
	if err := b.Revoke(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := b.Revoke(context.Background(), id); err != nil {
		t.Fatalf("a second revoke (404) is done, not an error: %v", err)
	}
	_, err = b.Renew(context.Background(), sess)
	var be *BrokerError
	if !errors.As(err, &be) || be.Status != 401 {
		t.Fatalf("renew after revoke must be 401: %v", err)
	}
}

func TestMissingCredentialNamesTheLoginCommand(t *testing.T) {
	b := &Broker{URL: "http://127.0.0.1:1", OperatorFile: filepath.Join(t.TempDir(), "none"), HTTP: http.DefaultClient}
	sess, _ := newSession(1, 1, "h:1:1", nil)
	_, _, err := b.Enroll(context.Background(), sess)
	if err == nil || !strings.Contains(err.Error(), "agent-secrets launcher login") {
		t.Fatalf("expected the remedy in the error, got %v", err)
	}
}
