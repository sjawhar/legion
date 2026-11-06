// packages/envoy/internal/broker/helper/identitywake_test.go
//go:build linux

// What a 401 LAUNCHER_INVALID leaves a helper that proves an identity of its own.
//
// In machine-login mode a dropped credential is handed to the refresh loop, which settles it at
// once. In identity mode there is no refresh loop: the trade loop (brokerclient.Client.Run)
// holds the credential and sleeps until the token in hand is nearly spent. A drop that does not
// wake it leaves the helper answering NO_CREDENTIAL to every register and sign for the rest of
// that sleep — ten minutes at a 900 s token, fifty-five at an hour's.
//
// Like enrolauthority_test.go's, these drive Broker's launcher calls against an httptest server
// directly: no unix socket and no pidfd.

package helper

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/brokerclient"
)

// serveTokenExchange adds the broker's token endpoint to f, minting a launcher credential of
// its own for each trade, for the machine key the helper proves. The credential of an earlier
// trade stays in f.credentials, so a proof signed with it still verifies and only the broker's
// scripted refusal refuses it. It answers how many trades have been made.
func serveTokenExchange(t *testing.T, f *enrolBroker, mux *http.ServeMux, thumbprint string) func() int {
	t.Helper()
	var mu sync.Mutex
	trades := 0
	mux.HandleFunc("POST /v1/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		trades++
		id := "credential-of-trade-" + strconv.Itoa(trades)
		mu.Unlock()
		f.mu.Lock()
		f.credentials[id] = thumbprint
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{
			"access_token": "access-token", "token_type": "DPoP", "expires_in": 3600,
			"credential_id": id, "credential_expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return trades
	}
}

// TestADroppedCredentialInIdentityModeIsTradedAgainAtOnce: the broker refuses the enrolment's
// launcher proof, the helper throws that credential away, and the trade loop — which would
// otherwise sleep out the hour its token lives — trades again straight away, so the next
// register finds a credential rather than NO_CREDENTIAL.
func TestADroppedCredentialInIdentityModeIsTradedAgainAtOnce(t *testing.T) {
	f := newEnrolBroker(t)
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	trades := serveTokenExchange(t, f, mux, thumbprint)
	// The enrolment routes stay f's own; this mux only adds the token endpoint in front of them.
	mux.Handle("/", f.srv.Config.Handler)
	f.srv.Config.Handler = mux

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("projected.service.account.token"), 0o600); err != nil {
		t.Fatal(err)
	}
	// machine, not identity: identity is the type one reading of a session's broker identity
	// has (Session.identity), which this test's enrol below names.
	machine, err := brokerclient.New(brokerclient.Config{
		BrokerURL: f.srv.URL, Key: key, HTTP: f.srv.Client(),
		Subject: brokerclient.NewProjectedToken(tokenFile),
	})
	if err != nil {
		t.Fatal(err)
	}
	b := &Broker{
		URL: f.srv.URL, Identity: machine, HTTP: f.srv.Client(),
		OperatorFile: filepath.Join(t.TempDir(), "no-operator-here"),
	}
	t.Cleanup(b.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !b.HasCredential() {
		t.Fatal("the helper listens with no credential, so this test proves nothing about dropping one")
	}
	first := trades()

	f.mu.Lock()
	f.enrolRefusals, f.enrolStatus, f.enrolCode = 1, http.StatusUnauthorized, "LAUNCHER_INVALID"
	f.mu.Unlock()
	// A Session's identity fields are its own to replace (Session.rename), so a caller that
	// holds no session enrols one reading of an identity instead (Broker.EnrollIdentity).
	if _, _, err := b.EnrollIdentity(ctx, identity{RuntimeID: "this-box", Thumbprint: sessionThumbprint(t)}); err == nil {
		t.Fatal("the refused enrolment answered no error")
	}

	deadline := time.Now().Add(5 * time.Second)
	for !b.HasCredential() {
		if time.Now().After(deadline) {
			t.Fatalf("the helper held no credential 5 s after the drop; it traded %d times, want the drop to wake the trade loop rather than leave every register answering NO_CREDENTIAL until the token's own renewal", trades()-first)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if again := trades() - first; again != 1 {
		t.Fatalf("the trade loop traded %d times after the drop, want exactly one", again)
	}
}

// sessionThumbprint is the thumbprint of a key a session would hold, which an enrolment names.
func sessionThumbprint(t *testing.T) string {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return thumbprint
}
