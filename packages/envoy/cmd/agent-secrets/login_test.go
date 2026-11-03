// packages/envoy/cmd/agent-secrets/login_test.go
//
// The session name `agent-secrets login` trades under. The broker binds each name to one
// principal, so a person's trade must ask under a name of their own: unset, the exchange
// refuses a person's trade for want of one, and this machine's own name belongs to
// google:<account> the moment the helper has traded under it — after which the person's trade is
// refused, or, trading first, takes the name and the helper's next machine trade is refused
// invalid_grant, which is permanent and stops its renewal loop for good.

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// loginBroker is the token, enrollment and session endpoints `agent-secrets login` and the
// commands after it call: it records the session name each trade asked under, hands out an
// enrollment id per registration, and answers a session proof naming any enrollment but the
// last one it made exactly as the broker answers one whose lease has lapsed.
type loginBroker struct {
	server *httptest.Server
	mu     sync.Mutex
	// sessions is the session form field of every trade, in order.
	sessions []string
	// enrollments counts the registrations, and live is the id of the one whose lease the
	// broker still holds; lapse() ends it, as BROKER_LEASE_SECONDS does.
	enrollments int
	live        string
}

func newLoginBroker(t *testing.T) *loginBroker {
	t.Helper()
	b := &loginBroker{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("the trade's body did not parse: %v", err)
		}
		b.mu.Lock()
		b.sessions = append(b.sessions, r.PostForm.Get("session"))
		b.mu.Unlock()
		writeLoginJSON(w, map[string]any{
			"access_token": "access-token", "token_type": "DPoP", "expires_in": 3600,
			"credential_id": "credential-1", "credential_expires_at": "2099-01-01T00:00:00Z",
		})
	})
	mux.HandleFunc("POST /v1/enrollments", func(w http.ResponseWriter, _ *http.Request) {
		b.mu.Lock()
		b.enrollments++
		b.live = fmt.Sprintf("enrollment-%d", b.enrollments)
		id := b.live
		b.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		writeLoginJSON(w, map[string]any{"enrollment_id": id})
	})
	mux.HandleFunc("GET /v1/enrollments/self", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		live := b.live
		b.mu.Unlock()
		if proofEnrollment(t, r) != live {
			w.WriteHeader(http.StatusUnauthorized)
			writeLoginJSON(w, map[string]any{"code": "PROOF_INVALID", "error": "proof invalid: not live"})
			return
		}
		writeLoginJSON(w, map[string]any{"enrollment_id": live, "runtime_id": "this-box", "grants": []any{}})
	})
	b.server = httptest.NewServer(mux)
	t.Cleanup(b.server.Close)
	return b
}

// lapse ends the lease of the enrollment the broker holds, as BROKER_LEASE_SECONDS does for a
// session nothing renews.
func (b *loginBroker) lapse() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.live = ""
}

// liveEnrollment is the enrollment the broker holds a lease for.
func (b *loginBroker) liveEnrollment() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.live
}

// proofEnrollment reads the enrollment a request's session proof names (its `eid` claim).
func proofEnrollment(t *testing.T, r *http.Request) string {
	t.Helper()
	parts := strings.Split(r.Header.Get("Proof"), ".")
	if len(parts) != 3 {
		t.Fatalf("the request carries no session proof: %q", r.Header.Get("Proof"))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode the proof's claims: %v", err)
	}
	var claims struct {
		EnrollmentID string `json:"eid"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("read the proof's claims: %v", err)
	}
	return claims.EnrollmentID
}

func (b *loginBroker) tradedSessions() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.sessions...)
}

func writeLoginJSON(w http.ResponseWriter, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// userCodeLine is how login tells the person what to type, which the test reads to approve the
// code as that person would.
var userCodeLine = regexp.MustCompile(`type the code (\S+)`)

// approvingWriter approves the device code the moment login prints it, standing in for the
// person at the keyboard.
type approvingWriter struct {
	t      *testing.T
	issuer *oidctest.Issuer
	buf    bytes.Buffer
}

func (w *approvingWriter) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	if match := userCodeLine.FindStringSubmatch(w.buf.String()); match != nil {
		if approveErr := w.issuer.ApproveDeviceCode(match[1]); approveErr != nil {
			w.t.Errorf("the person types the code in: %v", approveErr)
		}
		w.buf.Reset()
	}
	return n, err
}

// TestLoginTradesUnderThePersonsOwnSessionName: login's first trade is the one the broker binds
// the name to, so it must ask under brokerclient.SessionFor and never under
// AGENT_SECRETS_SESSION — which is unset on most boxes, and on the rest is the name this machine
// trades its own identity under.
func TestLoginTradesUnderThePersonsOwnSessionName(t *testing.T) {
	const person = "ada@example.com"
	for _, machineSession := range []string{"", "machine-of-this-box"} {
		name := "with AGENT_SECRETS_SESSION unset"
		if machineSession != "" {
			name = "with AGENT_SECRETS_SESSION naming this machine's own session"
		}
		t.Run(name, func(t *testing.T) {
			broker := newLoginBroker(t)
			issuer := loginIssuer(t, person)
			keyDir := t.TempDir()
			t.Setenv("AGENT_SECRETS_KEY_DIR", keyDir)
			t.Setenv(brokerclient.SignInDirVar, filepath.Join(t.TempDir(), "signins"))
			t.Setenv(brokerclient.BrokerURLVar, broker.server.URL)
			t.Setenv(brokerclient.SessionVar, machineSession)
			t.Setenv(brokerclient.IdentityVar, brokerclient.IdentityKubernetes)
			t.Setenv(brokerclient.MachineTokenFileVar, writeLoginFile(t, "machine-token", "projected.service.account.token"))

			stderr := &approvingWriter{t: t, issuer: issuer}
			if err := login(context.Background(), io.Discard, stderr); err != nil {
				t.Fatalf("login: %v", err)
			}

			records, err := signInStore().List()
			if err != nil || len(records) != 1 {
				t.Fatalf("the sign-ins login kept = %+v, %v; want the one it just made", records, err)
			}
			want := brokerclient.SessionFor(records[0])
			traded := broker.tradedSessions()
			if len(traded) != 1 || traded[0] != want {
				t.Fatalf("login traded under %q; want %q, the name every later trade of this sign-in asks under", traded, want)
			}
			if machineSession != "" && strings.Contains(traded[0], machineSession) {
				t.Fatalf("login traded under %q, which carries this machine's own session name: that name belongs to google:<account>", traded[0])
			}
		})
	}
}

// TestASignInWhoseLeaseLapsedRegistersItselfAgain: nothing renews a sign-in's enrollment, so
// every one of them ends after BROKER_LEASE_SECONDS. Before this, the record kept the dead id
// and every `agent-secrets NAME -- cmd`, `request` and `status` on the box failed until
// somebody ran `agent-secrets login` again — which made a second box key and a second sign-in
// on that person's own list. The box registers the session again under the same box key
// instead, once, and writes the fresh id into the record it already has.
func TestASignInWhoseLeaseLapsedRegistersItselfAgain(t *testing.T) {
	const person = "ada@example.com"
	broker := newLoginBroker(t)
	issuer := loginIssuer(t, person)
	keyDir := t.TempDir()
	t.Setenv("AGENT_SECRETS_KEY_DIR", keyDir)
	t.Setenv(brokerclient.SignInDirVar, filepath.Join(t.TempDir(), "signins"))
	t.Setenv(brokerclient.BrokerURLVar, broker.server.URL)
	t.Setenv(brokerclient.SessionVar, "")
	t.Setenv(brokerclient.IdentityVar, brokerclient.IdentityKubernetes)
	t.Setenv(brokerclient.MachineTokenFileVar, writeLoginFile(t, "machine-token", "projected.service.account.token"))
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", filepath.Join(t.TempDir(), "absent.sock"))

	if err := login(context.Background(), io.Discard, &approvingWriter{t: t, issuer: issuer}); err != nil {
		t.Fatalf("login: %v", err)
	}
	store := signInStore()
	first, err := store.Choose("")
	if err != nil {
		t.Fatalf("the sign-in login kept: %v", err)
	}
	broker.lapse()

	_, signer, err := selectSigner(keyDir)
	if err != nil {
		t.Fatalf("selectSigner after the lease lapsed: %v", err)
	}
	if _, _, err := newClient(broker.server.URL).Self(context.Background(), signer); err != nil {
		t.Fatalf("a call on a lapsed sign-in = %v; want the session registered again and the call answered", err)
	}

	kept, err := store.Choose("")
	if err != nil {
		t.Fatalf("the sign-in after the re-registration: %v", err)
	}
	switch {
	case kept.EnrollmentID == first.EnrollmentID:
		t.Fatalf("the record still names %s, the enrollment the broker no longer holds", kept.EnrollmentID)
	case kept.EnrollmentID != broker.liveEnrollment():
		t.Fatalf("the record names %s; want %s, the enrollment the broker now holds", kept.EnrollmentID, broker.liveEnrollment())
	case kept.Thumbprint != first.Thumbprint:
		t.Fatalf("the box key changed from %s to %s; want the same key, so the person's list keeps one sign-in for this box", first.Thumbprint, kept.Thumbprint)
	}
	if records, err := store.List(); err != nil || len(records) != 1 {
		t.Fatalf("the box holds %+v, %v; want the one sign-in it started with", records, err)
	}
}

// loginIssuer is the device-flow issuer this machine is told to sign people in at, with the
// client's own secret in a file as the deployment gives it.
func loginIssuer(t *testing.T, person string) *oidctest.Issuer {
	t.Helper()
	const clientID, clientSecret = "example-device-client", "example-device-secret"
	issuer := oidctest.New(t)
	issuer.EnableDeviceFlow(issuer.PublishKey(t, "google-key"), clientID, clientSecret)
	issuer.SignInByDefault(map[string]any{"sub": person, "email": person, "email_verified": true})
	t.Setenv(brokerclient.SignInIssuerVar, issuer.URL())
	t.Setenv(brokerclient.SignInClientIDVar, clientID)
	t.Setenv(brokerclient.SignInClientSecretFileVar, writeLoginFile(t, "client-secret", clientSecret))
	return issuer
}

// writeLoginFile writes content into a file of the test's own directory and answers its path.
func writeLoginFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoginsSessionNameIsTheOneEveryLaterTradeAsks pins the name itself: the person's address
// and the box key's thumbprint, which is what tells two boxes of one person apart, and which
// registerSignInSession and SignInAuthority.Current ask under too.
func TestLoginsSessionNameIsTheOneEveryLaterTradeAsks(t *testing.T) {
	record := brokerclient.SignInRecord{Person: "ada@example.com", Thumbprint: "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"}
	if got, want := brokerclient.SessionFor(record), "signin-ada@example.com-abcdefghijkl"; got != want {
		t.Fatalf("SessionFor = %q, want %q", got, want)
	}
	if _, err := url.ParseQuery("session=" + url.QueryEscape(brokerclient.SessionFor(record))); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprint(brokerclient.SessionFor(record)), " ") {
		t.Fatal("the session name carries a space, which no form field of a trade may")
	}
}
