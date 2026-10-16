package seed

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// gcpCert is the fake's certificate, for 127.0.0.1. TestMain makes it a root the whole test process
// trusts through SSL_CERT_FILE, which Go reads once, when it first loads the system's roots: the
// client then verifies the fake as it verifies Secret Manager, against the roots the system trusts.
var gcpCert tls.Certificate

// TestMain is the seed package's one, and it governs every test in the package's binary, the AWS
// store's and the command's included: SSL_CERT_FILE is set for all of them. Setup another test
// needs, such as testnats.Main's, goes into it rather than into a second TestMain.
func TestMain(m *testing.M) { os.Exit(runTrustingTheFake(m)) }

func runTrustingTheFake(m *testing.M) int {
	dir, err := os.MkdirTemp("", "envoy-infra-seed-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "envoy-infra seed test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	roots := filepath.Join(dir, "roots.pem")
	if err := os.WriteFile(roots, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.Setenv("SSL_CERT_FILE", roots); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	gcpCert = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return m.Run()
}
