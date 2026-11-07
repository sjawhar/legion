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

// leafLifetime is how long one host's leaf lasts, leafBackdate how far before now it starts, and
// leafRenewBefore how close to its end a cached leaf may come before the host is signed again.
// A day or so: long enough that a host costs about one key and one signature a day whatever the
// load, short enough that a leaf which somehow left this process is worth nothing for long. The
// renewal is there because a pod does outlive a leaf - nothing in this repository rolls the
// broker within a day - and a pod answering a certificate that has expired fails every command's
// verification of every bound host until somebody restarts it. The backdate covers the seconds
// of clock skew between the pod and the command's own box, and the hour before the end covers
// that same skew at the other end, along with a call still running on the leaf it was dialled
// with.
const (
	leafLifetime    = 25 * time.Hour
	leafBackdate    = time.Minute
	leafRenewBefore = time.Hour
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
// per host, signed on that host's first bound connection, signed again as it nears its end, and
// kept until the cap above evicts it. Every method is safe for concurrent use, since one map is
// shared by every connection.
type Authority struct {
	cert *x509.Certificate
	key  crypto.Signer
	// now is the clock the leaves are signed and renewed by; a test drives it rather than
	// waiting a day out. NewAuthority leaves it time.Now.
	now func() time.Time

	mu     sync.Mutex
	leaves map[string]heldLeaf
	// minted is the hosts of leaves in the order they were first signed, oldest first, which
	// is the order the cap evicts in. A host signed again keeps the place it had: what the cap
	// is about is how many hosts this map holds, and a renewal adds no host.
	minted []string
}

// heldLeaf is one host's certificate and the moment it stops being one: read on every bound
// connection to that host, so the expiry is held beside the certificate rather than parsed out
// of the certificate again on each handshake.
type heldLeaf struct {
	certificate *tls.Certificate
	notAfter    time.Time
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
	return &Authority{cert: cert, key: key, now: time.Now, leaves: map[string]heldLeaf{}}, nil
}

// Certificate is the authority's own certificate, which a test and a caller that reports the
// deployment's trust anchor read.
func (a *Authority) Certificate() *x509.Certificate { return a.cert }

// leaf is host's certificate, signed once and then answered from the map until it nears its end:
// a host the agent calls a thousand times costs one key and one signature a day. The chain it
// answers holds the authority's own certificate as well as the leaf, so a command that trusts
// the authority alone - which is every command the broker releases to - completes the chain
// without fetching anything. The map holds leafCacheSize hosts and the oldest goes to make room,
// which costs the host that comes back one key and one signature again.
//
// A cached leaf within leafRenewBefore of its NotAfter is signed again here rather than handed
// out: a pod that has served one host for longer than a leaf lasts would otherwise answer every
// handshake for that host with an expired certificate, which is every command's verification of
// every bound host failing until the pod is restarted.
func (a *Authority) leaf(host string) (*tls.Certificate, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	held, kept := a.leaves[host]
	if kept && now.Add(leafRenewBefore).Before(held.notAfter) {
		return held.certificate, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, err
	}
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
	if !kept {
		// One more host in the map, so the cap applies; a host signed again is the host that
		// was already there, and the order the cap evicts in does not change for it.
		if len(a.minted) >= leafCacheSize {
			delete(a.leaves, a.minted[0])
			a.minted = a.minted[1:]
		}
		a.minted = append(a.minted, host)
	}
	a.leaves[host] = heldLeaf{certificate: c, notAfter: template.NotAfter}
	return c, nil
}
