// packages/envoy/internal/broker/airlockproxy/ca_test.go
// The authority's own rows: what it refuses to be built from, and what one host's leaf is. A
// command trusts this certificate and nothing else about the proxy, so a pod whose files are
// wrong must say so at boot rather than fail every bound host for every command.
package airlockproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTheAuthorityRefusesFilesItCannotSignWith: a key that is not the certificate's, and a
// certificate that is no authority at all.
func TestTheAuthorityRefusesFilesItCannotSignWith(t *testing.T) {
	certFile, keyFile := writeCAFiles(t)
	_, otherKeyFile := writeCAFiles(t)
	leafCertFile, leafKeyFile := writeLeafFiles(t)

	for _, row := range []struct{ name, certFile, keyFile, want string }{
		{"a key that is not this certificate's", certFile, otherKeyFile, "private key does not match"},
		{"a certificate that is not an authority", leafCertFile, leafKeyFile, "not a certificate authority"},
		{"a certificate file that is not there", filepath.Join(t.TempDir(), "absent.crt"), keyFile, "absent.crt"},
	} {
		t.Run(row.name, func(t *testing.T) {
			authority, err := NewAuthority(row.certFile, row.keyFile)
			if err == nil {
				t.Fatalf("NewAuthority(%s) built an authority; want a refusal naming %q", row.name, row.want)
			}
			if authority != nil {
				t.Errorf("NewAuthority answered an authority beside its refusal: %+v", authority)
			}
			if !strings.Contains(err.Error(), row.want) {
				t.Errorf("the refusal reads %q; want it to name %q", err, row.want)
			}
		})
	}
	if _, err := NewAuthority(certFile, keyFile); err != nil {
		t.Fatalf("NewAuthority on a key and certificate that do match: %v", err)
	}
}

// TestOneLeafPerHostSignedOnce: a host is signed once and the same certificate is answered
// afterwards, it carries that host as its one name, and it chains to the authority. A proxy that
// signed a key per request would spend a key generation on every connection of every command.
func TestOneLeafPerHostSignedOnce(t *testing.T) {
	authority := newTestAuthority(t)
	first, err := authority.leaf("api.example.com")
	if err != nil {
		t.Fatalf("the first leaf: %v", err)
	}
	again, err := authority.leaf("api.example.com")
	if err != nil {
		t.Fatalf("the second leaf: %v", err)
	}
	if first != again {
		t.Fatal("the authority signed the same host twice; want one leaf per host, kept")
	}
	other, err := authority.leaf("api.example.net")
	if err != nil {
		t.Fatalf("another host's leaf: %v", err)
	}
	if other == first {
		t.Fatal("two hosts were served one certificate; want a leaf of its own per host")
	}

	parsed, err := x509.ParseCertificate(first.Certificate[0])
	if err != nil {
		t.Fatalf("parse the leaf: %v", err)
	}
	if len(parsed.DNSNames) != 1 || parsed.DNSNames[0] != "api.example.com" {
		t.Errorf("the leaf names %v; want api.example.com and nothing else", parsed.DNSNames)
	}
	if err := parsed.CheckSignatureFrom(authority.Certificate()); err != nil {
		t.Errorf("the leaf is not signed by the authority: %v", err)
	}
	if lifetime := time.Until(parsed.NotAfter); lifetime > 48*time.Hour || lifetime < 12*time.Hour {
		t.Errorf("the leaf lasts %v; want a day or so", lifetime)
	}
	// The chain the proxy answers with holds the authority too, so a command that trusts the
	// authority alone needs nothing else.
	if len(first.Certificate) != 2 {
		t.Errorf("the leaf's chain is %d certificate(s); want the leaf and the authority", len(first.Certificate))
	}
}

// TestALeafNearingItsEndIsSignedAgain: a pod outlives a leaf - nothing in this repository rolls
// the broker within a day - and a pod that answered the cached leaf whatever its NotAfter said
// would fail every command's verification of every bound host from its twenty-fifth hour until
// somebody restarted it. The clock is injected, so the row drives a day of a pod's life without
// waiting one.
func TestALeafNearingItsEndIsSignedAgain(t *testing.T) {
	const host = "api.example.com"
	authority := newTestAuthority(t)
	clock := &testClock{now: time.Now()}
	authority.now = clock.read
	began := clock.read()

	first, err := authority.leaf(host)
	if err != nil {
		t.Fatalf("the first leaf: %v", err)
	}
	// Well inside the leaf's life: the same certificate, since signing one per connection is
	// a key generation and a signature per connection.
	clock.set(began.Add(leafLifetime - 2*leafRenewBefore))
	again, err := authority.leaf(host)
	if err != nil {
		t.Fatalf("the leaf an hour before the renewal window: %v", err)
	}
	if again != first {
		t.Fatal("the authority signed the host again while its leaf had hours left; want the one it holds")
	}

	// Inside the last hour: signed again, and the new one lasts a leaf's life from now.
	clock.set(began.Add(leafLifetime - leafRenewBefore/2))
	renewed, err := authority.leaf(host)
	if err != nil {
		t.Fatalf("the leaf inside the renewal window: %v", err)
	}
	if renewed == first {
		t.Fatal("the authority answered a leaf within half an hour of its end; want it signed again")
	}
	parsed, err := x509.ParseCertificate(renewed.Certificate[0])
	if err != nil {
		t.Fatalf("parse the renewed leaf: %v", err)
	}
	if !parsed.NotAfter.After(clock.read().Add(leafLifetime - time.Minute)) {
		t.Errorf("the renewed leaf ends at %v, with the clock at %v; want a leaf's life from now", parsed.NotAfter, clock.read())
	}
	if len(parsed.DNSNames) != 1 || parsed.DNSNames[0] != host {
		t.Errorf("the renewed leaf names %v; want %s and nothing else", parsed.DNSNames, host)
	}
	if err := parsed.CheckSignatureFrom(authority.Certificate()); err != nil {
		t.Errorf("the renewed leaf is not signed by the authority: %v", err)
	}

	// And the host that was signed again is one host still: a renewal adds nothing for the
	// cap to evict.
	authority.mu.Lock()
	held, minted := len(authority.leaves), len(authority.minted)
	authority.mu.Unlock()
	if held != 1 || minted != 1 {
		t.Errorf("the authority holds %d leaves and %d minted hosts; want one host, signed twice", held, minted)
	}

	// A day later still: the renewed leaf has had its own life, and the host is signed again.
	clock.set(began.Add(2 * leafLifetime))
	third, err := authority.leaf(host)
	if err != nil {
		t.Fatalf("the leaf a day after the renewal: %v", err)
	}
	if third == renewed {
		t.Fatal("the authority answered a leaf that had expired; want it signed again")
	}
}

// writeLeafFiles writes a certificate that is no authority, with its key: what a deployment that
// mounted the wrong half of its Secret would give the proxy.
func writeLeafFiles(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("the leaf's key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "not an authority"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("the leaf's certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("the leaf's key, encoded: %v", err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "leaf.crt"), filepath.Join(dir, "leaf.key")
	writeFile(t, certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeFile(t, keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certFile, keyFile
}
