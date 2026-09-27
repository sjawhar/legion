package bus

import (
	"fmt"
	"os"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

// The two variables a process names its NATS user's nkey seed with: a file holding the seed, or
// the seed itself.
const (
	nkeySeedFileVariable = "NATS_NKEY_SEED_FILE"
	nkeySeedVariable     = "NATS_NKEY_SEED"
)

// nkeyCredential is the NATS user this process connects as, from its environment: the trimmed
// contents of the file NATS_NKEY_SEED_FILE names, else NATS_NKEY_SEED. A set variable is
// authoritative, and the file pointer wins over the seed: an empty pointer, or a missing,
// unreadable or blank file, is an error naming the variable and the path, never a fallback to
// NATS_NKEY_SEED or to no credential (Legion's X_FILE rule, packages/daemon/src/daemon/secrets.ts
// readSecretPointer); so is a blank NATS_NKEY_SEED, and a seed that is not a user nkey seed. Neither
// set is a nil option: the connection carries no credential, as every connection did before servers
// required one. No error carries the seed.
func nkeyCredential(lookup func(string) (string, bool)) (nats.Option, error) {
	var seed, source string
	if file, set := lookup(nkeySeedFileVariable); set {
		if file == "" {
			return nil, fmt.Errorf("%s is set but empty", nkeySeedFileVariable)
		}
		contents, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("%s names %s, which could not be read: %w", nkeySeedFileVariable, file, err)
		}
		seed = strings.TrimSpace(string(contents))
		if seed == "" {
			return nil, fmt.Errorf("%s names %s, which is empty", nkeySeedFileVariable, file)
		}
		source = fmt.Sprintf("%s (%s)", nkeySeedFileVariable, file)
	} else if value, set := lookup(nkeySeedVariable); set {
		seed = strings.TrimSpace(value)
		if seed == "" {
			return nil, fmt.Errorf("%s is set but empty", nkeySeedVariable)
		}
		source = nkeySeedVariable
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
