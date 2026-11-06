// packages/envoy/internal/broker/airlockproxy/ca.go
// The proxy's certificate authority: the one key and certificate every pod of the deployment
// mounts (BROKER_AIRLOCK_CA_CERT_FILE, BROKER_AIRLOCK_CA_KEY_FILE), and the leaf it signs for
// each bound host, so the command's own TLS verification holds while the proxy reads the
// request. A command trusts this certificate and nothing else about the proxy, which is why a
// key and certificate that do not match, and a certificate that is not an authority, are refused
// here rather than found out by the first command that dials a bound host.
package airlockproxy

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"sync"
	"time"
)

// leafLifetime is how long one host's leaf lasts, and leafBackdate how far before now it starts.
// A day or so: long enough that the map below signs a host once for the life of a pod, short
// enough that a leaf which somehow left this process is worth nothing for long. Nothing renews a
// leaf, because nothing outlives it - a pod is rolled by every deployment and by every change to
// the rules file - and the backdate covers the seconds of clock skew between the pod and the
// command's own box.
const (
	leafLifetime = 25 * time.Hour
	leafBackdate = time.Minute
)

// leafCacheSize is how many hosts' leaves Authority keeps. There is a cap because the hosts
// are not the proxy's to count: an entry may bind a pattern ("*.example.com"), and a command
// that CONNECTs to a distinct resolvable subdomain each time would otherwise grow this map by
// a key and a certificate per name for as long as the pod lives. A thousand is far more hosts
// than a grant reaches and small enough to bound; past it the oldest leaf goes, and a host
// that comes back is signed again, which costs one key and one signature and changes nothing
// a client sees, since no leaf is ever held anywhere but here.
const leafCacheSize = 1024

// Authority is the proxy's certificate authority: the mounted key and certificate, and one leaf
// per host, minted on that host's first bound connection and kept until the cap above evicts it.
// Every method is safe for concurrent use, since one map is shared by every connection.
type Authority struct {
	cert *x509.Certificate
	key  crypto.Signer

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
	// minted is the hosts of leaves in the order they were signed, oldest first, which is the
	// order the cap evicts in.
	minted []string
}

// NewAuthority reads the PEM certificate of certFile and the PEM private key of keyFile, and
// refuses: a file it cannot read or cannot parse, a key that is not the certificate's (which
// tls.LoadX509KeyPair compares), a certificate that is not an authority, and an authority whose
// key usage does not allow it to sign certificates. Each refusal names the file, because the two
// are mounted from one Kubernetes Secret and an operator reads which half is wrong.
func NewAuthority(certFile, keyFile string) (*Authority, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("airlock proxy certificate authority (%s, %s): %w", certFile, keyFile, err)
	}
	// LoadX509KeyPair parses the leaf only far enough to compare the two public keys, so the
	// certificate itself is parsed here.
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("airlock proxy certificate authority %s: %w", certFile, err)
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("airlock proxy certificate authority %s: not a certificate authority (no CA basic constraint), so nothing it signed would be trusted", certFile)
	}
	// A certificate that declares key usages at all must declare this one: a command's
	// verification of the chain reads it, so an authority without it signs leaves nobody
	// accepts. A certificate that declares none (KeyUsage 0) constrains nothing and is left to
	// the verifier that reads it.
	if cert.KeyUsage != 0 && cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, fmt.Errorf("airlock proxy certificate authority %s: its key usage does not allow signing certificates", certFile)
	}
	key, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("airlock proxy certificate authority %s: the key is of a kind this proxy cannot sign with (%T)", keyFile, pair.PrivateKey)
	}
	return &Authority{cert: cert, key: key, leaves: map[string]*tls.Certificate{}}, nil
}

// Certificate is the authority's own certificate, which a test and a caller that reports the
// deployment's trust anchor read.
func (a *Authority) Certificate() *x509.Certificate { return a.cert }

// leaf is host's certificate, signed once and then answered from the map: a host the agent calls
// a thousand times costs one key and one signature. The chain it answers holds the authority's
// own certificate as well as the leaf, so a command that trusts the authority alone - which is
// every command the broker releases to - completes the chain without fetching anything. The map
// holds leafCacheSize hosts and the oldest goes to make room, which costs the host that comes
// back one key and one signature again.
func (a *Authority) leaf(host string) (*tls.Certificate, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.leaves[host]; ok {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-leafBackdate),
		NotAfter:     now.Add(leafLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	// The host as the certificate's one name: its one DNS name, or its one IP address for a rule
	// that binds an address rather than a name, since a verifier reads an address out of the IP
	// SAN and never out of the DNS SAN.
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der, a.cert.Raw}, PrivateKey: key}
	if len(a.minted) >= leafCacheSize {
		delete(a.leaves, a.minted[0])
		a.minted = a.minted[1:]
	}
	a.leaves[host] = c
	a.minted = append(a.minted, host)
	return c, nil
}
