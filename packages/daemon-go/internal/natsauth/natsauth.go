// Package natsauth connects the daemon to Envoy's NATS as the NATS user its environment names.
package natsauth

import (
	"fmt"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// The two variables a process names its NATS user's nkey seed with: a file holding the seed, or
// the seed itself. The Envoy clients read the same two (packages/envoy/internal/bus/nkey.go).
const (
	SeedFileVariable = "NATS_NKEY_SEED_FILE"
	SeedVariable     = "NATS_NKEY_SEED"
)

// Connect dials urls as the NATS user lookup names (Credential): an unusable seed is an error
// before any dial, and neither variable set connects without a credential.
func Connect(urls []string, lookup func(string) (string, bool), options ...nats.Option) (*nats.Conn, error) {
	credential, err := Credential(lookup)
	if err != nil {
		return nil, err
	}
	if credential != nil {
		options = append(options, credential)
	}
	return nats.Connect(strings.Join(urls, ","), options...)
}

// Credential is the NATS user a process connects as: the trimmed contents of the file
// NATS_NKEY_SEED_FILE names, else NATS_NKEY_SEED. A set variable is authoritative, and the file
// pointer wins over the seed: an empty pointer, or a missing, unreadable or blank file, is an error
// naming the variable and the path (config.ReadSecretPointer), never a fallback to NATS_NKEY_SEED
// or to no credential; so is a blank NATS_NKEY_SEED, and a seed that is not a user nkey seed.
// Neither set is a nil option: the connection carries no credential, as every connection did
// before servers required one. No error carries the seed.
// Deploy order: a process gets a seed only after its server has nkey users (the SRE's stage 1, with
// the no_auth_user fallback); a server with no users sends no nonce, and nats.go then refuses the
// nkey ("nats: nkeys not supported by the server") rather than connecting without it.
func Credential(lookup func(string) (string, bool)) (nats.Option, error) {
	var seed, source string
	if file, set := lookup(SeedFileVariable); set {
		if file == "" {
			return nil, fmt.Errorf("%s is set but empty", SeedFileVariable)
		}
		read, err := config.ReadSecretPointer(SeedFileVariable, file)
		if err != nil {
			return nil, err
		}
		seed, source = read, fmt.Sprintf("%s (%s)", SeedFileVariable, file)
	} else if value, set := lookup(SeedVariable); set {
		seed = strings.TrimSpace(value)
		if seed == "" {
			return nil, fmt.Errorf("%s is set but empty", SeedVariable)
		}
		source = SeedVariable
	} else {
		return nil, nil
	}
	user, err := nkeys.FromSeed([]byte(seed))
	if err != nil {
		return nil, fmt.Errorf("%s does not hold a valid nkey seed: %w", source, err)
	}
	public, err := user.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("%s does not hold a valid nkey seed: %w", source, err)
	}
	if !nkeys.IsValidPublicUserKey(public) {
		return nil, fmt.Errorf("%s holds an nkey seed that is not a user's (public key %s)", source, public)
	}
	return nats.Nkey(public, user.Sign), nil
}
