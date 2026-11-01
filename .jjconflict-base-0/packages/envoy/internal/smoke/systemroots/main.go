// Command systemroots reports how many certificates of a PEM bundle Go's system root pool holds,
// as a binary in the image it runs in would load that pool, and how many a pool made from the
// bundle itself holds. The smoke test copies it into the Envoy image to show the RDS CA bundle the
// image ships is no binary's system root: a binary there trusts an RDS CA only where it names the
// bundle, as the broker's sslrootcert does. The second count is the check's control: a probe that
// found none of the bundle's certificates in the bundle's own pool could find none anywhere.
//
// Usage: systemroots <bundle.pem>. It prints "system=<n> bundle=<n> total=<n>" and exits 0, or
// exits 1 naming what it could not read.
package main

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: systemroots <bundle.pem>")
		os.Exit(1)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var certs []*x509.Certificate
	for rest := data; ; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			break
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "parse a certificate of %s: %v\n", os.Args[1], err)
			os.Exit(1)
		}
		certs = append(certs, cert)
	}
	system, err := x509.SystemCertPool()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load the system root pool: %v\n", err)
		os.Exit(1)
	}
	bundle := x509.NewCertPool()
	bundle.AppendCertsFromPEM(data)
	fmt.Printf("system=%d bundle=%d total=%d\n", holds(system, certs), holds(bundle, certs), len(certs))
}

// holds counts the certificates pool already holds: adding one it holds leaves the pool equal to
// itself, since a pool keeps one copy of each certificate.
func holds(pool *x509.CertPool, certs []*x509.Certificate) int {
	n := 0
	for _, cert := range certs {
		grown := pool.Clone()
		grown.AddCert(cert)
		if grown.Equal(pool) {
			n++
		}
	}
	return n
}
