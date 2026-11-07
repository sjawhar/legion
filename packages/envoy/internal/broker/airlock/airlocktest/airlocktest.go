// Package airlocktest is the airlock's stand-ins: the HTTPS services a call behind the airlock
// reaches, the two minter endpoints the broker mints a kind's value at, the Microsoft token
// endpoint the azure kind exchanges at, and the AWS service that checks the signature the aws
// kind signs a call again with. Each of them records every request it was sent, which is what a
// row and a check script both read a result back from.
//
// It is an ordinary package rather than a _test.go file because cmd/dev-airlock-stands serves
// these same stand-ins to the broker's local stack, as internal/broker/store/storetest is an
// ordinary package so every broker package can import it: one implementation of each stand-in
// answers the Go tests and the check scripts, so a check that passes against a stand-in is a
// check against the stand-in the tests pin.
//
// Nothing here takes a *testing.T: a constructor answers an error, since the command that serves
// these stand-ins is no test and must report what it could not start rather than ending a test.
package airlocktest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// authorityLifetime and leafLifetime are how long a stand-in authority and the certificates it
// signs last. Both are far longer than any run of the tests or of the local stack, and far
// shorter than anything worth keeping: the key is made in memory at each start, so nothing a run
// signs is of any use to the next one.
const (
	authorityLifetime = 30 * 24 * time.Hour
	leafLifetime      = 7 * 24 * time.Hour
)

// Request is one request a stand-in received, as a row and a check script read it back. The JSON
// names are the ones cmd/dev-airlock-stands' recording route answers, so a shell script reads a
// field of a record by the name this type gives it.
type Request struct {
	Method string      `json:"method"`
	Path   string      `json:"path"`
	Query  string      `json:"query"`
	Host   string      `json:"host"`
	Header http.Header `json:"header"`
	Body   string      `json:"body"`
}

// Answer is what a stand-in answers one request with. It is given the request as the service
// recorded it, since the service has already read the body and nothing is left in r.Body, and it
// reports whether it answered: an Answer that returns false leaves the service's own body, which
// is how a stand-in that is only interested in some of the routes it is sent - the token
// endpoint of a host that also serves an API - leaves the rest alone.
type Answer func(w http.ResponseWriter, r *http.Request, seen Request) bool

// Authority is a certificate authority of a run's own: the key and certificate every stand-in
// service's certificate is signed by, and the two PEM files a script points
// BROKER_AIRLOCK_CA_KEY_FILE and BROKER_AIRLOCK_CA_CERT_FILE at.
type Authority struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
	keyPEM  []byte
}

// NewAuthority makes one certificate authority, named name in its subject so an operator reading
// a certificate chain can tell which run signed it.
func NewAuthority(name string) (*Authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("the stand-in authority's key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, fmt.Errorf("the stand-in authority's serial number: %w", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(authorityLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("the stand-in authority's certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("the stand-in authority's certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("the stand-in authority's key, encoded: %w", err)
	}
	return &Authority{
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// Certificate is the authority's own certificate, the one a caller trusts it by.
func (a *Authority) Certificate() *x509.Certificate { return a.cert }

// CertificatePEM is that certificate as the PEM bytes a file holds.
func (a *Authority) CertificatePEM() []byte { return a.certPEM }

// Pool is a trust store holding this authority and nothing else, which is what a client of a
// stand-in service verifies with.
func (a *Authority) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(a.cert)
	return pool
}

// WriteFiles writes the authority's certificate and key into dir as ca.crt and ca.key and
// answers the two paths, the files BROKER_AIRLOCK_CA_CERT_FILE and BROKER_AIRLOCK_CA_KEY_FILE
// name. The key is written readable by its owner alone, as the one mounted in a pod is.
func (a *Authority) WriteFiles(dir string) (certFile, keyFile string, err error) {
	certFile, keyFile = filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	if err := os.WriteFile(certFile, a.certPEM, 0o644); err != nil {
		return "", "", fmt.Errorf("write the stand-in authority's certificate: %w", err)
	}
	if err := os.WriteFile(keyFile, a.keyPEM, 0o600); err != nil {
		return "", "", fmt.Errorf("write the stand-in authority's key: %w", err)
	}
	return certFile, keyFile, nil
}

// Leaf signs one server certificate for hosts, each of them a DNS name or an IP address, and
// answers it with its PEM bytes, which is what a service's own certificate file holds.
func (a *Authority) Leaf(hosts []string) (*tls.Certificate, []byte, error) {
	if len(hosts) == 0 {
		return nil, nil, errors.New("a stand-in service's certificate names at least one host")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("the stand-in service's key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, nil, fmt.Errorf("the stand-in service's serial number: %w", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hosts[0]},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(leafLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	// An address goes in the IP SAN and a name in the DNS SAN: a verifier reads an address out
	// of the one and a name out of the other, and never out of the other one.
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
			continue
		}
		template.DNSNames = append(template.DNSNames, host)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return nil, nil, fmt.Errorf("the stand-in service's certificate: %w", err)
	}
	certificate := &tls.Certificate{Certificate: [][]byte{der, a.cert.Raw}, PrivateKey: key}
	return certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// ServiceOptions is what a Service is built from. Everything left zero has the default its field
// names.
type ServiceOptions struct {
	// Addr is the address the service binds, and "" is 127.0.0.1:0, a port the operating
	// system picks. A stand-in is never bound wider than loopback: it holds a test key and
	// answers whoever reaches it.
	Addr string
	// Hosts are the names and addresses its certificate carries, and none is 127.0.0.1 and
	// localhost. A service a rules file binds a secret to carries that entry's host here, since
	// the command's own TLS verification reads the name it asked for out of the certificate.
	Hosts []string
	// Authority signs that certificate, and nil is one of the service's own, read back from
	// Authority. One authority over every service of a run is what lets a client trust them all
	// by one file.
	Authority *Authority
	// Plain serves http rather than https. It is for the two minter endpoints alone: the broker
	// mints a kind's value with an HTTP client of its own that trusts the host's roots and
	// nothing else, so a minter stand-in behind TLS would need a root in the pod's trust store,
	// while every stand-in a call reaches through the proxy is https, which the proxy's own
	// extra roots cover (BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE).
	Plain bool
	// Body is the line the service answers a request no Answer answered, and "" is "the service
	// answered".
	Body string
}

// Service is one stand-in service: an HTTPS (or, for a minter endpoint, HTTP) server on loopback
// that answers a body of its own and records every request it was sent, readable back through
// Requests. The services S and T of the airlock's checks are each one of these.
//
// It records each body whole, which is what lets the AWS stand-in hash one and a check script
// read one back; a check that uploads a large artifact therefore holds that artifact in this
// process's memory until the run ends.
type Service struct {
	authority *Authority
	leafPEM   []byte
	listener  net.Listener
	server    *http.Server
	body      string

	mu     sync.Mutex
	seen   []Request
	answer Answer
}

// NewService starts one. The caller closes it.
func NewService(o ServiceOptions) (*Service, error) {
	s := &Service{authority: o.Authority, body: o.Body}
	if s.body == "" {
		s.body = "the service answered"
	}
	addr := o.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("bind the stand-in service: %w", err)
	}
	s.listener = listener
	if !o.Plain {
		if s.authority == nil {
			if s.authority, err = NewAuthority("airlock stand-in authority"); err != nil {
				listener.Close()
				return nil, err
			}
		}
		hosts := o.Hosts
		if len(hosts) == 0 {
			hosts = []string{"127.0.0.1", "localhost"}
		}
		certificate, leafPEM, err := s.authority.Leaf(hosts)
		if err != nil {
			listener.Close()
			return nil, err
		}
		s.leafPEM = leafPEM
		s.listener = tls.NewListener(listener, &tls.Config{
			Certificates: []tls.Certificate{*certificate},
			MinVersion:   tls.VersionTLS12,
			// So a command may reach a stand-in over HTTP/2 as it reaches the service it
			// stands in for, which is what the proxy's own upstream transport negotiates.
			NextProtos: []string{"h2", "http/1.1"},
		})
	}
	s.server = &http.Server{Handler: http.HandlerFunc(s.serve), ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = s.server.Serve(s.listener) }()
	return s, nil
}

// serve records one request and answers it.
func (s *Service) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "the stand-in service could not read the body: "+err.Error(), http.StatusBadRequest)
		return
	}
	seen := Request{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Host: r.Host,
		Header: r.Header.Clone(), Body: string(body),
	}
	s.mu.Lock()
	s.seen = append(s.seen, seen)
	answer := s.answer
	s.mu.Unlock()
	if answer != nil && answer(w, r, seen) {
		return
	}
	fmt.Fprintln(w, s.body)
}

// Addr is the address the service is listening on, with the port the operating system gave it.
func (s *Service) Addr() string { return s.listener.Addr().String() }

// URL is that address as the URL a caller reaches it at.
func (s *Service) URL() string {
	if s.leafPEM == nil {
		return "http://" + s.Addr()
	}
	return "https://" + s.Addr()
}

// Authority is the authority that signed the service's certificate, nil for a plain one.
func (s *Service) Authority() *Authority { return s.authority }

// CertificatePEM is the service's own certificate, the file cmd/dev-airlock-stands writes beside
// the authority's. It is nil for a plain service, which has none.
func (s *Service) CertificatePEM() []byte { return s.leafPEM }

// Requests is every request the service has been sent, in order, as a copy of its own: a caller
// reading them while the service is still being called reads a list nothing else writes to.
func (s *Service) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.seen...)
}

// AnswerWith is what the service answers from here on, in place of its own body. A stand-in
// installs its own Answer here, and a row whose point is what the service received - the
// signature it verifies, the token it is given, the upgrade it answers - writes that answer
// here.
func (s *Service) AnswerWith(answer Answer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answer = answer
}

// Close stops the service.
func (s *Service) Close() {
	_ = s.server.Close()
}

// formOf reads the form a recorded request's body holds, which is how a stand-in that answers a
// token endpoint reads what it was asked for: the body is in the record, not in r.Body, which
// the service has already read.
func formOf(seen Request) (url.Values, error) {
	return url.ParseQuery(seen.Body)
}
