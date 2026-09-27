// Package roots embeds the Yubico attestation trust roots (developers.yubico.com/PKI/) used to
// verify a packed attestation certificate's chain in production. Attestation trust roots are
// always a Verifier constructor parameter, never read from the environment: cmd/broker's main.go
// loads these three PEMs into an *x509.CertPool and passes it to approvers.Verifier.Roots.
// roots_test.go pins each file's SHA-256 so a silent swap of a trust root fails the build.
package roots

import "embed"

//go:embed yubico-ca-1.pem yubico-fido-ca-1.pem yubico-fido-ca-2.pem
var Files embed.FS

// Names lists the embedded PEM file names, in the fixed order roots_test.go checks them in.
var Names = []string{"yubico-ca-1.pem", "yubico-fido-ca-1.pem", "yubico-fido-ca-2.pem"}
