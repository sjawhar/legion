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
	_, ok := resolve(file, lookup, nil)
	return ok
}

// Seed is the nkey seed of the NATS user a process connects as: the trimmed contents of file, the
// file the configuration's SeedFileKey names ("" when it names none); else those of the file
// NATS_NKEY_SEED_FILE names; else NATS_NKEY_SEED. The first source set is authoritative: an empty
// NATS_NKEY_SEED_FILE, or a missing, unreadable, blank, or other-readable file, is an error naming
// the key or variable and the path (config.ReadGroupSecretPointer: a daemon running as a non-root
// uid in a pod reads a kubelet-mounted Secret file through the pod's fsGroup, so its group may read
// it), never a fallback to the next source or to no credential; so is a blank NATS_NKEY_SEED, and a
// seed that is not a user nkey seed. None set is "": the connection carries no credential, as every
// connection did before servers required one. No error carries the seed.
// Deploy order: a process gets a seed only after its server has nkey users (the SRE's stage 1, with
// the no_auth_user fallback); a server with no users sends no nonce, and nats.go then refuses the
// nkey ("nats: nkeys not supported by the server") rather than connecting without it.
func Seed(file string, lookup func(string) (string, bool)) (string, error) {
	src, ok := resolve(file, lookup, config.ReadGroupSecretPointer)
	if !ok {
		return "", nil
	}
	return src.seed()
}

// SeedFile is Seed of the file the configuration's SeedFileKey names, alone, refused when its group
// or others can read it (config.ReadPrivateSecretPointer): what `legion controller start` checks,
// on the operator's own machine, before handing its Oh My Pi that file.
func SeedFile(file string) (string, error) {
	return fileSource(SeedFileKey, file, config.ReadPrivateSecretPointer).seed()
}

// MountedSeed is Seed of the two variables alone, reading NATS_NKEY_SEED_FILE whatever its mode: a
// pod's pointer names the providers Secret's key, which the kubelet mounts readable by the pod's
// group, and which only that pod can read. `legion probe-image` checks it inside the probe pod.
func MountedSeed(lookup func(string) (string, bool)) (string, error) {
	src, ok := resolve("", lookup, config.ReadSecretPointer)
	if !ok {
		return "", nil
	}
	return src.seed()
}

// source is one place a seed is read from: its name, which every refusal carries, and read, which
// answers the seed or why the source holds none.
type source struct {
	name string
	read func() (string, error)
}

// resolve is the one precedence every reader of a seed shares: file when the configuration names
// one, else NATS_NKEY_SEED_FILE when set, else NATS_NKEY_SEED when set, whatever each holds; ok is
// false when none is. read reads a file source (the key or variable naming it, and its path).
func resolve(file string, lookup func(string) (string, bool), read func(key, path string) (string, error)) (source, bool) {
	if file != "" {
		return fileSource(SeedFileKey, file, read), true
	}
	if path, set := lookup(SeedFileVariable); set {
		if path == "" {
			return source{SeedFileVariable, func() (string, error) {
				return "", fmt.Errorf("%s is set but empty", SeedFileVariable)
			}}, true
		}
		return fileSource(SeedFileVariable, path, read), true
	}
	if value, set := lookup(SeedVariable); set {
		return source{SeedVariable, func() (string, error) {
			if seed := strings.TrimSpace(value); seed != "" {
				return seed, nil
			}
			return "", fmt.Errorf("%s is set but empty", SeedVariable)
		}}, true
	}
	return source{}, false
}

// fileSource is the file path that key (a configuration key or a variable) names, which read reads.
func fileSource(key, path string, read func(key, path string) (string, error)) source {
	return source{fmt.Sprintf("%s (%s)", key, path), func() (string, error) { return read(key, path) }}
}

// seed is the source's seed, refused unless it is a user's nkey seed.
func (s source) seed() (string, error) {
	seed, err := s.read()
	if err != nil {
		return "", err
	}
	if _, err := userKey(seed, s.name); err != nil {
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

// PublicKey is the public key of the nkey user seed is the seed of, a seed Seed answered: what a
// process may say about the seed it holds without the seed leaving it.
func PublicKey(seed string) (string, error) {
	user, err := userKey(seed, "the NATS nkey seed")
	if err != nil {
		return "", err
	}
	return user.PublicKey()
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
