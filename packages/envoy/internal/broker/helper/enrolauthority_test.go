// packages/envoy/internal/broker/helper/enrolauthority_test.go
//go:build linux

// Which credential an enrolment is signed with, and which operator it states.
//
// These drive Broker's launcher calls against an httptest server directly: no unix socket, no
// pidfd, so they run wherever Go runs even though the rest of this package's suite needs
// SO_PEERPIDFD. The rule itself — the person signed in on this box, else the machine — is
// locked in internal/brokerclient, whose whole suite runs anywhere (signinauthority_test.go).

package helper

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/brokerclient"
)

// enrolCall is one launcher call the broker received: the body it carried, and the launcher
// credential its proof was signed with, read from the proof rather than the body.
type enrolCall struct {
	body       map[string]any
	credential string
}

// enrolBroker is a broker's two launcher routes and nothing else. It verifies every proof
// against the keys credentials names, so a test reads which credential signed rather than
// trusting the caller to say, and answers what the test scripted.
type enrolBroker struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex
	// credentials maps a launcher credential id to the thumbprint of the key that may sign for
	// it, as the broker's own launcher_credentials table does.
	credentials map[string]string
	enrols      []enrolCall
	revokes     []enrolCall
	// enrolRefusals and revokeRefusals, when set, answer that many calls of that kind with
	// enrolStatus/enrolCode (revokeStatus/revokeCode) instead of the success.
	enrolRefusals, revokeRefusals int
	enrolStatus, revokeStatus     int
	enrolCode, revokeCode         string
	next                          int
}

func newEnrolBroker(t *testing.T) *enrolBroker {
	t.Helper()
	b := &enrolBroker{t: t, credentials: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/enrollments", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		credential := b.verify(r, http.MethodPost, b.srv.URL+"/v1/enrollments")
		b.mu.Lock()
		b.enrols = append(b.enrols, enrolCall{body: body, credential: credential})
		status, code := 0, ""
		if b.enrolRefusals > 0 {
			b.enrolRefusals--
			status, code = b.enrolStatus, b.enrolCode
		}
		b.next++
		id := b.next
		b.mu.Unlock()
		if status != 0 {
			writeJSON(w, status, map[string]string{"code": code, "error": "the broker refused this call"})
			return
		}
		writeJSON(w, 201, map[string]string{
			"enrollment_id":    "enr-" + string(rune('0'+id)),
			"lease_expires_at": time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339Nano),
		})
	})
	mux.HandleFunc("DELETE /v1/enrollments/{id}", func(w http.ResponseWriter, r *http.Request) {
		credential := b.verify(r, http.MethodDelete, b.srv.URL+"/v1/enrollments/"+r.PathValue("id"))
		b.mu.Lock()
		b.revokes = append(b.revokes, enrolCall{body: map[string]any{"id": r.PathValue("id")}, credential: credential})
		status, code := 0, ""
		if b.revokeRefusals > 0 {
			b.revokeRefusals--
			status, code = b.revokeStatus, b.revokeCode
		}
		b.mu.Unlock()
		if status != 0 {
			writeJSON(w, status, map[string]string{"code": code, "error": "the broker refused this call"})
			return
		}
		w.WriteHeader(204)
	})
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

// verify reads the launcher credential a call's proof was signed with, holding it to the key
// that credential was minted for.
func (b *enrolBroker) verify(r *http.Request, method, url string) string {
	b.t.Helper()
	v := &proof.Verifier{
		Skew: time.Minute,
		LookupLauncher: func(_ context.Context, id string) (string, bool, error) {
			b.mu.Lock()
			defer b.mu.Unlock()
			thumbprint, ok := b.credentials[id]
			return thumbprint, ok, nil
		},
		Replay: func(context.Context, string, time.Time) (bool, error) { return true, nil },
	}
	subject, err := v.Verify(r.Context(), r.Header.Get("Proof"), method, url, time.Now())
	if err != nil {
		b.t.Errorf("the broker could not verify a launcher proof: %v", err)
		return ""
	}
	return subject.LauncherID
}

// mint records a launcher credential for key, as a trade or an approved machine login does, and
// answers its id.
func (b *enrolBroker) mint(id string, key *ecdsa.PrivateKey) string {
	b.t.Helper()
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		b.t.Fatal(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.credentials[id] = thumbprint
	return id
}

func (b *enrolBroker) calls() (enrols, revokes []enrolCall) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]enrolCall(nil), b.enrols...), append([]enrolCall(nil), b.revokes...)
}

// machineIdentityBroker is a helper on a machine that proves an identity of its own: the
// credential the exchange minted is installed, and no operator file exists at all, since a
// machine's credential carries no person and nothing may read one for it.
func machineIdentityBroker(t *testing.T, f *enrolBroker) (*Broker, *ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := brokerclient.New(brokerclient.Config{
		BrokerURL: f.srv.URL, Key: key,
		Subject: brokerclient.NewProjectedToken(filepath.Join(t.TempDir(), "token")),
	})
	if err != nil {
		t.Fatal(err)
	}
	b := &Broker{
		URL: f.srv.URL, Identity: identity, HTTP: f.srv.Client(),
		// The file a machine login would read its approver from, which this machine has not
		// got: a test that sees it read is a test that caught the bug.
		OperatorFile: filepath.Join(t.TempDir(), "no-operator-here"),
	}
	t.Cleanup(b.Close)
	credential := f.mint("credential-of-this-machine", key)
	b.credMu.Lock()
	b.installCredential(&machineCredential{key: key, id: credential, expiresAt: time.Now().Add(24 * time.Hour), gone: make(chan struct{})}, &loginState{State: "issued"})
	b.credMu.Unlock()
	return b, key, credential
}

// stubSignIns is a person signed in on the box, as the Broker asks about them: it answers the
// authority the test gives it, and records every Refused.
type stubSignIns struct {
	mu        sync.Mutex
	authority *brokerclient.Authority
	err       error
	refused   []string
	// onRefused, when set, runs inside Refused, so a test makes the box's answer change
	// exactly when the broker rejects the credential.
	onRefused func()
	calls     int
}

func (s *stubSignIns) Current(context.Context) (*brokerclient.Authority, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.authority, s.err
}

func (s *stubSignIns) Refused(credentialID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refused = append(s.refused, credentialID)
	if s.onRefused != nil {
		s.onRefused()
	}
}

// signedIn is the authority of a person signed in on this box: a box key of their own and the
// credential their trade minted, which carries them as its operator.
func signedIn(t *testing.T, f *enrolBroker, person, credentialID string) *brokerclient.Authority {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return &brokerclient.Authority{Key: key, CredentialID: f.mint(credentialID, key), Operator: person}
}

// TestAMachinesSessionEnrolsWithNoOperatorAtAll is the regression: a machine's launcher
// credential carries no person, and an enrolment that states one under it is refused outright
// (enroll.Service.Create, ErrOperatorMismatch), so the helper must state none — and must not
// read the operator file at all, which on such a machine names nothing and would fail the
// enrolment before it was ever sent.
func TestAMachinesSessionEnrolsWithNoOperatorAtAll(t *testing.T) {
	f := newEnrolBroker(t)
	b, _, credential := machineIdentityBroker(t, f)

	id, _, err := b.EnrollSessionKey(context.Background(), "example-host:1234:77", "thumbprint-of-the-box-key", nil)
	if err != nil {
		t.Fatalf("a machine with no operator file could not enrol a session: %v", err)
	}
	if id == "" {
		t.Fatal("the broker answered no enrollment id")
	}
	enrols, _ := f.calls()
	if len(enrols) != 1 {
		t.Fatalf("%d enrolments, want one", len(enrols))
	}
	if operator, stated := enrols[0].body["operator"]; stated {
		t.Errorf("the enrolment stated operator %v; a machine's credential carries none, and stating one is refused OPERATOR_MISMATCH", operator)
	}
	if enrols[0].credential != credential {
		t.Errorf("the enrolment was signed with %q, want this machine's own credential %q", enrols[0].credential, credential)
	}
}

// TestAMachineLoginsSessionStillStatesItsApprover: where a person approved this machine's login,
// the credential carries that person, so the enrolment states them exactly as it always has.
func TestAMachineLoginsSessionStillStatesItsApprover(t *testing.T) {
	f := newEnrolBroker(t)
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	b := &Broker{URL: f.srv.URL, OperatorFile: operatorFile(t, "ada@example.com"), HTTP: f.srv.Client()}
	t.Cleanup(b.Close)
	b.credMu.Lock()
	b.installCredential(&machineCredential{key: key, id: f.mint("credential-of-the-machine-login", key), expiresAt: time.Now().Add(24 * time.Hour), gone: make(chan struct{})}, &loginState{State: "issued"})
	b.credMu.Unlock()

	if _, _, err := b.EnrollSessionKey(context.Background(), "example-host:1234:77", "thumbprint", nil); err != nil {
		t.Fatal(err)
	}
	enrols, _ := f.calls()
	if got := enrols[0].body["operator"]; got != "ada@example.com" {
		t.Errorf("the enrolment stated operator %v, want the person who approved this machine's login", got)
	}
}

// TestASignedInPersonsSessionEnrolsAsThem: with a sign-in on the box, the session is that
// person's — signed with their box key under their own credential, stating them — and this
// machine's credential signs nothing.
func TestASignedInPersonsSessionEnrolsAsThem(t *testing.T) {
	f := newEnrolBroker(t)
	b, _, machine := machineIdentityBroker(t, f)
	person := signedIn(t, f, "ada@example.com", "credential-of-adas-sign-in")
	b.SignIns = &stubSignIns{authority: person}

	if _, _, err := b.EnrollSessionKey(context.Background(), "example-host:1234:77", "thumbprint", nil); err != nil {
		t.Fatal(err)
	}
	enrols, _ := f.calls()
	if len(enrols) != 1 {
		t.Fatalf("%d enrolments, want one", len(enrols))
	}
	if got := enrols[0].body["operator"]; got != "ada@example.com" {
		t.Errorf("the enrolment stated operator %v, want the person signed in on this box", got)
	}
	if enrols[0].credential != person.CredentialID {
		t.Errorf("the enrolment was signed with %q, want the sign-in's own credential %q (this machine's is %q)",
			enrols[0].credential, person.CredentialID, machine)
	}
}

// TestARevokedSignInEnrolsUnderTheMachineInTheSameCall: the broker rejects a credential it
// revoked with the sign-in, which is the moment the box learns of it. The helper asks the box
// again and enrols under this machine's own credential in the same call, so the launcher
// waiting on the enrolment gets one; and this machine's credential, which nothing refused,
// is still held.
func TestARevokedSignInEnrolsUnderTheMachineInTheSameCall(t *testing.T) {
	f := newEnrolBroker(t)
	b, _, machine := machineIdentityBroker(t, f)
	person := signedIn(t, f, "ada@example.com", "credential-of-adas-sign-in")
	signIns := &stubSignIns{authority: person}
	// The revoke reaches the box as the broker's refusal of the credential; the trade that
	// follows is refused too, which is what leaves the box with no sign-in.
	signIns.onRefused = func() { signIns.authority = nil }
	b.SignIns = signIns
	f.enrolRefusals, f.enrolStatus, f.enrolCode = 1, http.StatusUnauthorized, "LAUNCHER_INVALID"

	if _, _, err := b.EnrollSessionKey(context.Background(), "example-host:1234:77", "thumbprint", nil); err != nil {
		t.Fatalf("a session registered after the sign-in was revoked must still enrol: %v", err)
	}
	enrols, _ := f.calls()
	if len(enrols) != 2 {
		t.Fatalf("%d enrolments, want two: the refused one and the one under this machine", len(enrols))
	}
	if enrols[1].credential != machine {
		t.Errorf("the second enrolment was signed with %q, want this machine's own credential %q", enrols[1].credential, machine)
	}
	if operator, stated := enrols[1].body["operator"]; stated {
		t.Errorf("the enrolment under this machine stated operator %v; it carries none", operator)
	}
	if got := signIns.refused; len(got) != 1 || got[0] != person.CredentialID {
		t.Errorf("the box was told of %v, want the refused credential %q alone", got, person.CredentialID)
	}
	if !b.HasCredential() {
		t.Error("a refusal of the person's credential dropped this machine's own, which nothing refused")
	}
}

// TestAPersonsCredentialRefusedTwiceIsTheEnrolmentsAnswer: the retry is one, not a loop, so a
// broker refusing everything answers the caller rather than spinning.
func TestAPersonsCredentialRefusedTwiceIsTheEnrolmentsAnswer(t *testing.T) {
	f := newEnrolBroker(t)
	b, _, _ := machineIdentityBroker(t, f)
	b.SignIns = &stubSignIns{authority: signedIn(t, f, "ada@example.com", "credential-of-adas-sign-in")}
	f.enrolRefusals, f.enrolStatus, f.enrolCode = 2, http.StatusUnauthorized, "LAUNCHER_INVALID"
	// The second call is refused too: the stub keeps answering the same authority.
	_, _, err := b.EnrollSessionKey(context.Background(), "example-host:1234:77", "thumbprint", nil)
	var be *BrokerError
	if !errors.As(err, &be) || be.Code != "LAUNCHER_INVALID" {
		t.Fatalf("two refusals answered %v, want the broker's own refusal", err)
	}
	enrols, _ := f.calls()
	if len(enrols) != 2 {
		t.Fatalf("%d enrolments, want two: one try and one retry", len(enrols))
	}
}

// TestAnUndecidedSignInFailsTheEnrolmentRatherThanDownscopingIt: a box that cannot say whose
// sessions these are (two people signed in and nothing naming which, a record it cannot read)
// enrols nothing, rather than quietly giving the session this machine's lesser authority.
func TestAnUndecidedSignInFailsTheEnrolmentRatherThanDownscopingIt(t *testing.T) {
	f := newEnrolBroker(t)
	b, _, _ := machineIdentityBroker(t, f)
	b.SignIns = &stubSignIns{err: errors.New("2 people are signed in on this machine")}

	_, _, err := b.EnrollSessionKey(context.Background(), "example-host:1234:77", "thumbprint", nil)
	if err == nil || !strings.Contains(err.Error(), "2 people are signed in") {
		t.Fatalf("an undecided box answered %v, want the refusal that says why", err)
	}
	if enrols, _ := f.calls(); len(enrols) != 0 {
		t.Fatalf("%d enrolments reached the broker, want none", len(enrols))
	}
}

// TestRevokeFallsBackToTheMachinesOwnCredential: the broker lets a credential end only the
// enrolments of its own operator, and the helper keeps no note of which credential enrolled a
// given session — a key a launcher brought leaves it none. So a 403 from the person's
// credential is answered by signing with this machine's, and a session the machine enrolled
// before anyone signed in is still revocable.
func TestRevokeFallsBackToTheMachinesOwnCredential(t *testing.T) {
	f := newEnrolBroker(t)
	b, _, machine := machineIdentityBroker(t, f)
	person := signedIn(t, f, "ada@example.com", "credential-of-adas-sign-in")
	b.SignIns = &stubSignIns{authority: person}
	f.revokeRefusals, f.revokeStatus, f.revokeCode = 1, http.StatusForbidden, "NOT_ENROLLING_CREDENTIAL"

	if err := b.Revoke(context.Background(), "enr-1"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	_, revokes := f.calls()
	if len(revokes) != 2 {
		t.Fatalf("%d revokes, want two: the person's credential and then this machine's", len(revokes))
	}
	if revokes[0].credential != person.CredentialID || revokes[1].credential != machine {
		t.Errorf("the revokes were signed with %q then %q, want the sign-in's then this machine's", revokes[0].credential, revokes[1].credential)
	}
}

// TestRevokeUnderOneCredentialIsOneCall: nothing retries a revoke the broker accepted, or one
// it refused for any reason but "not this credential's".
func TestRevokeUnderOneCredentialIsOneCall(t *testing.T) {
	f := newEnrolBroker(t)
	b, _, machine := machineIdentityBroker(t, f)
	if err := b.Revoke(context.Background(), "enr-1"); err != nil {
		t.Fatal(err)
	}
	if _, revokes := f.calls(); len(revokes) != 1 || revokes[0].credential != machine {
		t.Fatalf("a revoke under this machine's own credential made %d calls", len(revokes))
	}
}

// TestBothKindsOfSessionFollowTheBoxAcrossASignIn is the whole rule in one test, over both
// sessions a helper in machine mode enrols: its own host session (Enroll, a process it pinned
// and holds the key of) and an agent box's key a launcher brings it (EnrollSessionKey). Before
// anybody signs in, both enrol under this machine's own credential and state no operator;
// after `agent-secrets login`, a session registered afresh — either kind — enrols under that
// person's credential and states them; after the sign-in is revoked, both are back to the
// machine's. Nothing is restarted between the three: the same Broker answers all six.
func TestBothKindsOfSessionFollowTheBoxAcrossASignIn(t *testing.T) {
	f := newEnrolBroker(t)
	b, _, machine := machineIdentityBroker(t, f)
	signIns := &stubSignIns{}
	b.SignIns = signIns

	// Before the sign-in: the box offers no authority, so both enrol as this machine.
	hostSession := func(t *testing.T, pid int) {
		t.Helper()
		sess, err := newSession(pid, 77, "example-host:"+strconv.Itoa(pid)+":77", "", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := b.EnrollIdentity(context.Background(), sess.identity()); err != nil {
			t.Fatalf("enrol a host session: %v", err)
		}
	}
	box := func(t *testing.T, n int) {
		t.Helper()
		if _, _, err := b.EnrollSessionKey(context.Background(), "agent-box-"+strconv.Itoa(n), "thumbprint-of-box-"+strconv.Itoa(n), nil); err != nil {
			t.Fatalf("enrol an agent box: %v", err)
		}
	}
	hostSession(t, 1234)
	box(t, 1)

	// `agent-secrets login`: the person's sign-in is readable by the user the helper runs as,
	// and the next session of either kind is theirs.
	person := signedIn(t, f, "ada@example.com", "credential-of-adas-sign-in")
	signIns.mu.Lock()
	signIns.authority = person
	signIns.mu.Unlock()
	hostSession(t, 5678)
	box(t, 2)

	// The person revokes the sign-in on Dispatch: the box has none again.
	signIns.mu.Lock()
	signIns.authority = nil
	signIns.mu.Unlock()
	hostSession(t, 9012)
	box(t, 3)

	enrols, _ := f.calls()
	if len(enrols) != 6 {
		t.Fatalf("%d enrolments, want six: a host session and an agent box before, during and after the sign-in", len(enrols))
	}
	for i, want := range []struct {
		credential, operator string
	}{
		{machine, ""}, {machine, ""},
		{person.CredentialID, "ada@example.com"}, {person.CredentialID, "ada@example.com"},
		{machine, ""}, {machine, ""},
	} {
		if enrols[i].credential != want.credential {
			t.Errorf("enrolment %d was signed with %q, want %q", i, enrols[i].credential, want.credential)
		}
		operator, stated := enrols[i].body["operator"]
		switch {
		case want.operator == "" && stated:
			t.Errorf("enrolment %d stated operator %v under a credential that carries none", i, operator)
		case want.operator != "" && operator != want.operator:
			t.Errorf("enrolment %d stated operator %v, want %q", i, operator, want.operator)
		}
		if kind := enrols[i].body["kind"]; kind != "session" {
			t.Errorf("enrolment %d is kind %v, want session", i, kind)
		}
	}
}
