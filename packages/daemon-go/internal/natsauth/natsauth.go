// Package natsauth resolves the NATS nkey user a Legion process connects to Envoy's NATS as, and
// connects as it.
package natsauth

import (
	"fmt"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// SeedFileKey is the configuration key naming the seed's file, in `legion.yaml` and the operator's
// controller configuration alike. SeedFileVariable and SeedVariable are the two variables a process
// names it with otherwise: a file holding the seed, or the seed itself. The Envoy clients read the
// same two (packages/envoy/internal/bus/nkey.go), and SeedVariable is also the name of the launch
// secret every pane receives the seed as, behind a SeedFileVariable pointer.
const (
	SeedFileKey      = "nats_nkey_seed_file"
	SeedFileVariable = "NATS_NKEY_SEED_FILE"
	SeedVariable     = "NATS_NKEY_SEED"
)

// Configured reports whether Seed reads a seed at all: file (the SeedFileKey's, "" when the
// configuration names none) is set, or either variable is, whatever it holds.
func Configured(file string, lookup func(string) (string, bool)) bool {
	if file != "" {
		return true
	}
	if _, set := lookup(SeedFileVariable); set {
		return true
	}
	_, set := lookup(SeedVariable)
	return set
}

// Seed is the nkey seed of the NATS user a process connects as: the trimmed contents of file, the
// file the configuration's SeedFileKey names ("" when it names none); else those of the file
// NATS_NKEY_SEED_FILE names; else NATS_NKEY_SEED. The first source set is authoritative: an empty
// NATS_NKEY_SEED_FILE, or a missing, unreadable or blank file, is an error naming the key or
// variable and the path (config.ReadSecretPointer), never a fallback to the next source or to no
// credential; so is a blank NATS_NKEY_SEED, and a seed that is not a user nkey seed. None set is
// "": the connection carries no credential, as every connection did before servers required one.
// No error carries the seed.
// Deploy order: a process gets a seed only after its server has nkey users (the SRE's stage 1, with
// the no_auth_user fallback); a server with no users sends no nonce, and nats.go then refuses the
// nkey ("nats: nkeys not supported by the server") rather than connecting without it.
func Seed(file string, lookup func(string) (string, bool)) (string, error) {
	var seed, source string
	if file != "" {
		read, err := config.ReadSecretPointer(SeedFileKey, file)
		if err != nil {
			return "", err
		}
		seed, source = read, fmt.Sprintf("%s (%s)", SeedFileKey, file)
	} else if file, set := lookup(SeedFileVariable); set {
		if file == "" {
			return "", fmt.Errorf("%s is set but empty", SeedFileVariable)
		}
		read, err := config.ReadSecretPointer(SeedFileVariable, file)
		if err != nil {
			return "", err
		}
		seed, source = read, fmt.Sprintf("%s (%s)", SeedFileVariable, file)
	} else if value, set := lookup(SeedVariable); set {
		seed = strings.TrimSpace(value)
		if seed == "" {
			return "", fmt.Errorf("%s is set but empty", SeedVariable)
		}
		source = SeedVariable
	} else {
		return "", nil
	}
	if _, err := userKey(seed, source); err != nil {
		return "", err
	}
	return seed, nil
}

// Connect dials urls as the nkey user seed is the seed of, a seed Seed answered; "" connects
// without a credential.
func Connect(urls []string, seed string, options ...nats.Option) (*nats.Conn, error) {
	if seed != "" {
		user, err := userKey(seed, "the NATS nkey seed")
		if err != nil {
			return nil, err
		}
		public, err := user.PublicKey()
		if err != nil {
			return nil, err
		}
		options = append(options, nats.Nkey(public, user.Sign))
	}
	return nats.Connect(strings.Join(urls, ","), options...)
}

// userKey is seed's key pair, refused, naming source, when seed is no nkey seed or the seed of
// something other than a user.
func userKey(seed, source string) (nkeys.KeyPair, error) {
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
	return user, nil
}
