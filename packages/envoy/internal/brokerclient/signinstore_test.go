// packages/envoy/internal/brokerclient/signinstore_test.go
//
// The rules a machine's sign-ins are kept and chosen by. They live here rather than in
// cmd/agent-secrets, whose own tests cannot run in a pod under gVisor (no SO_PEERPIDFD), so the
// one thing a person's sign-in must never get wrong — reading one colleague's secrets under
// another's name — is locked by a test that runs anywhere.

package brokerclient

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// storeFor is an empty store in a directory of this test's own.
func storeFor(t *testing.T) SignInStore {
	t.Helper()
	return SignInStore{Dir: filepath.Join(t.TempDir(), "signins")}
}

// signedIn is the record `agent-secrets login` keeps for person, with a box key of its own.
func signedIn(t *testing.T, person string) SignInRecord {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("make a box key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("write the box key: %v", err)
	}
	return SignInRecord{
		Person: person, Issuer: "https://accounts.example", ClientID: "example-device-client",
		RefreshToken: "refresh-token-of-" + person,
		BoxKey:       string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		Thumbprint:   "thumbprint-of-" + person, SignedInAt: time.Now().UTC(),
	}
}

// TestASecondPersonSignsInBesideTheFirst: one record per person, each holding its own box key
// and refresh token, and a second sign-in as the same person replaces that person's record
// alone.
func TestASecondPersonSignsInBesideTheFirst(t *testing.T) {
	store := storeFor(t)
	ada, grace := signedIn(t, "ada@example.com"), signedIn(t, "grace@example.com")
	for _, record := range []SignInRecord{ada, grace} {
		if err := store.Write(record); err != nil {
			t.Fatalf("Write %s: %v", record.Person, err)
		}
	}
	records, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 2 || records[0].Person != "ada@example.com" || records[1].Person != "grace@example.com" {
		t.Fatalf("List = %v, want both people, by person", records)
	}
	if records[0].RefreshToken == records[1].RefreshToken || records[0].BoxKey == records[1].BoxKey {
		t.Error("the two sign-ins share a refresh token or a box key; each person's is their own")
	}

	// Signing in again as one of them replaces that person's record, and only that one.
	again := signedIn(t, "ada@example.com")
	again.EnrollmentID = "enrollment-2"
	if err := store.Write(again); err != nil {
		t.Fatalf("Write again: %v", err)
	}
	records, err = store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("List = %d records, want 2: a second sign-in as one person adds no record", len(records))
	}
	if records[0].EnrollmentID != "enrollment-2" || records[1].RefreshToken != grace.RefreshToken {
		t.Errorf("the second sign-in left %v, want ada's replaced and grace's untouched", records)
	}
}

// TestOneSignInIsUsedWithoutBeingNamed: the common machine, where the person who signed in is
// the person every session of theirs acts as.
func TestOneSignInIsUsedWithoutBeingNamed(t *testing.T) {
	store := storeFor(t)
	if err := store.Write(signedIn(t, "ada@example.com")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	chosen, err := store.Choose("")
	if err != nil {
		t.Fatalf("Choose with nothing named: %v", err)
	}
	if chosen.Person != "ada@example.com" {
		t.Errorf("Choose = %s, want the one person signed in here", chosen.Person)
	}
	if _, err := chosen.Key(); err != nil {
		t.Errorf("the chosen record's box key: %v", err)
	}
}

// TestMoreThanOneSignInIsRefusedUntilToldWhich: the rule that stops a session reading one
// colleague's secrets under another's name, and the refusal that names them both.
func TestMoreThanOneSignInIsRefusedUntilToldWhich(t *testing.T) {
	store := storeFor(t)
	for _, person := range []string{"ada@example.com", "grace@example.com"} {
		if err := store.Write(signedIn(t, person)); err != nil {
			t.Fatalf("Write %s: %v", person, err)
		}
	}
	_, err := store.Choose("")
	if err == nil {
		t.Fatal("Choose with two people signed in and nothing named answered a sign-in")
	}
	for _, want := range []string{"ada@example.com", "grace@example.com", PersonVar} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is %q, want it to carry %q", err, want)
		}
	}
	chosen, err := store.Choose("GRACE@example.com")
	if err != nil {
		t.Fatalf("Choose grace: %v", err)
	}
	if chosen.Person != "grace@example.com" {
		t.Errorf("Choose = %s, want grace, whom the variable named whatever its case", chosen.Person)
	}
	_, err = store.Choose("mallory@example.com")
	if err == nil || !strings.Contains(err.Error(), SignInCommand) {
		t.Fatalf("Choose of someone not signed in here = %v, want a refusal naming %s", err, SignInCommand)
	}
}

// TestNobodySignedInNamesTheCommandThatSignsOneIn.
func TestNobodySignedInNamesTheCommandThatSignsOneIn(t *testing.T) {
	store := storeFor(t)
	records, err := store.List()
	if err != nil || records != nil {
		t.Fatalf("List of a machine with no sign-ins = %v, %v; want none and no error", records, err)
	}
	if _, err := store.Choose(""); err == nil || !strings.Contains(err.Error(), SignInCommand) {
		t.Fatalf("Choose on a machine with no sign-ins = %v, want a refusal naming %s", err, SignInCommand)
	}
}

// TestASignInIsReadableByItsOwnUserAlone: the record holds a key and a Google refresh token, so
// the directory is 0700 and the file 0600.
func TestASignInIsReadableByItsOwnUserAlone(t *testing.T) {
	store := storeFor(t)
	record := signedIn(t, "ada@example.com")
	if err := store.Write(record); err != nil {
		t.Fatalf("Write: %v", err)
	}
	directory, err := os.Stat(store.Dir)
	if err != nil {
		t.Fatalf("stat the sign-in directory: %v", err)
	}
	if mode := directory.Mode().Perm(); mode != 0o700 {
		t.Errorf("the sign-in directory is %o, want 700", mode)
	}
	file, err := os.Stat(store.Path(record.Person))
	if err != nil {
		t.Fatalf("stat the record: %v", err)
	}
	if mode := file.Mode().Perm(); mode != 0o600 {
		t.Errorf("%s's record is %o, want 600: it holds a box key and a Google refresh token", record.Person, mode)
	}
}
