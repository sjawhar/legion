// Package seed is `envoy-infra seed`: it writes a NATS nkey user seed as a secret's first version,
// and checks and verifies the seed a secret holds, without the seed reaching argv, an output, a log
// or an error. The consumer declares the secret, empty; the public key generate prints goes into
// the server's authorization, so the stack that runs NATS never holds a seed.
package seed

import (
	"bytes"
	"context"
	"encoding/base32"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/infra/core"
)

// Store is a secret store holding the seed. None of its errors carries a value: each names the call
// and the secret.
//
// check makes one call, Describe, and verify two, Describe and Current, so reading a secret needs
// no grant beyond its metadata and its value. Only generate counts versions.
type Store interface {
	// Describe reports, from the secret's metadata alone, whether it exists, whether it is
	// scheduled for deletion and when, and which version the store serves by default.
	Describe(ctx context.Context, name string) (Description, error)
	// Versions counts every version the secret holds, deprecated ones included.
	Versions(ctx context.Context, name string) (int, error)
	// Put writes value as the secret's first version and returns its version id. An error with a
	// version id means that version was written; one without may follow a write that landed.
	Put(ctx context.Context, name string, value []byte) (version string, err error)
	// Current reads the version the store serves by default. The value is the caller's, who
	// clears it once read.
	Current(ctx context.Context, name string) (version string, value []byte, err error)
}

// Description is what a store's metadata says of a secret. CurrentVersion is read only for a
// secret that exists and is not scheduled for deletion.
type Description struct {
	Exists, ScheduledForDeletion bool
	DeletionDate                 string
	CurrentVersion               string // "" when the store serves none
	// The newest version's id and state, for a store whose default names the newest version
	// whatever its state (GCP's versions/latest); "" for a store without such a default.
	NewestVersion, NewestState string
}

// ErrRefused is every refusal's: nothing was written, so fixing the precondition its message names
// and running again is safe. The command exits 2.
//
// Generate's other errors come after Put returned: a version may have been written, so the secret
// is checked before anything else is done. The command exits 1 on them, and on ErrMismatch.
var ErrRefused = errors.New("seed: refused")

// ErrMismatch is Verify's when the value the store serves by default is no user seed or derives
// another public key. The command exits 1.
var ErrMismatch = errors.New("seed: the stored value does not derive the public key")

// defaultLabel names the version a store serves by default in Check's and Verify's lines: Secrets
// Manager's staging label.
const defaultLabel = awsCurrent

type refusal struct{ msg string }

func (r *refusal) Error() string        { return r.msg }
func (r *refusal) Is(target error) bool { return target == ErrRefused }

func refuse(format string, args ...any) error {
	return &refusal{msg: fmt.Sprintf(format, args...)}
}

// afterWrite is an error once Put has returned. It wraps nothing, so no store error it quotes can
// make it a refusal.
type afterWrite struct{ msg string }

func (e *afterWrite) Error() string { return e.msg }

func afterWritef(format string, args ...any) error {
	return &afterWrite{msg: fmt.Sprintf(format, args...)}
}

// Check returns the report line for the secret: absent, scheduled for deletion, or present with or
// without a version the store serves by default. It reads no value.
func Check(ctx context.Context, s Store, name string) (string, error) {
	d, err := s.Describe(ctx, name)
	if err != nil {
		return "", refuse("%v", err)
	}
	switch {
	case !d.Exists:
		return name + ": absent", nil
	case d.ScheduledForDeletion:
		return fmt.Sprintf("%s: present, scheduled for deletion (%s)", name, d.DeletionDate), nil
	case d.CurrentVersion == "":
		return fmt.Sprintf("%s: present; %s: no", name, defaultLabel), nil
	}
	return fmt.Sprintf("%s: present; %s: yes (version %s)", name, defaultLabel, d.CurrentVersion), nil
}

// existing refuses a secret that is absent or scheduled for deletion.
func existing(d Description, name string) error {
	if !d.Exists {
		return refuse("%s does not exist. The consumer declares it, with no value; apply that declaration first", name)
	}
	if d.ScheduledForDeletion {
		return refuse("%s is scheduled for deletion (%s); nothing is written to it", name, d.DeletionDate)
	}
	return nil
}

// Generate makes an nkey user and writes its seed as the secret's first version: it refuses a
// secret that is absent, scheduled for deletion or holds any version, so it never replaces a seed a
// client may already read. It then lists the versions again, to check the secret holds that version
// alone, and reads back the version served by default, which must be that version and derive the
// public key it returns. Once Put has written a version, every error comes with the public key and
// the version id.
func Generate(ctx context.Context, s Store, name string) (publicKey, version string, err error) {
	d, err := s.Describe(ctx, name)
	if err != nil {
		return "", "", refuse("%v", err)
	}
	if err := existing(d, name); err != nil {
		return "", "", err
	}
	held, err := s.Versions(ctx, name)
	if err != nil {
		return "", "", refuse("%v", err)
	}
	if held > 0 {
		return "", "", refuse("%s already holds %d version(s). generate writes only a secret's first version, so it "+
			"never replaces a seed a client may already read; use check or verify", name, held)
	}
	user, err := nkeys.CreateUser()
	if err != nil {
		return "", "", refuse("make an nkey user for %s: %v", name, err)
	}
	defer user.Wipe()
	value, err := user.Seed()
	if err != nil {
		return "", "", refuse("read the seed of the nkey user made for %s: %v", name, err)
	}
	publicKey, err = user.PublicKey()
	if err != nil {
		return "", "", refuse("read the public key of the nkey user made for %s: %v", name, err)
	}

	version, err = s.Put(ctx, name, value)
	if err != nil {
		if version == "" {
			return "", "", afterWritef("%v; whether a version was written is unknown, run check", err)
		}
		return publicKey, version, afterWritef("%v", err)
	}
	held, err = s.Versions(ctx, name)
	if err != nil {
		return publicKey, version, afterWritef("version %s was written, but listing versions afterwards failed: %v", version, err)
	}
	if held != 1 {
		return publicKey, version, afterWritef("%s now holds %d version(s), not only version %s: another writer raced "+
			"this one. Run verify with the public key above before committing it", name, held, version)
	}
	read, stored, err := s.Current(ctx, name)
	if err != nil {
		return publicKey, version, afterWritef("version %s was written, but reading it back failed: %v", version, err)
	}
	defer clear(stored)
	if read != version {
		return publicKey, version, afterWritef("version %s was written, but %s serves version %s by default: another "+
			"writer raced this one. Run verify with the public key above before committing it", version, name, read)
	}
	derived, err := publicKeyOf(stored)
	if err != nil {
		return publicKey, version, afterWritef("version %s was written, but it reads back as no user seed: %v", version, err)
	}
	if derived != publicKey {
		return publicKey, version, afterWritef("version %s was written, but it does not derive the public key above", version)
	}
	return publicKey, version, nil
}

// Verify returns the line saying whether the seed the store serves by default derives
// publicKey, with ErrMismatch when it does not or is no user seed. publicKey is never echoed unless
// it is a user public key: a seed pasted there by mistake must not reach the terminal twice.
func Verify(ctx context.Context, s Store, name, publicKey string) (string, error) {
	if !core.IsUserPublicKey(publicKey) {
		return "", refuse("%s: --public-key is not an nkey user public key (U and 55 base32 characters with a valid checksum)", name)
	}
	d, err := s.Describe(ctx, name)
	if err != nil {
		return "", refuse("%v", err)
	}
	if err := existing(d, name); err != nil {
		return "", err
	}
	if d.CurrentVersion == "" {
		return "", refuse("%s has no %s version; nothing to verify", name, defaultLabel)
	}
	version, stored, err := s.Current(ctx, name)
	if err != nil {
		return "", refuse("%v", err)
	}
	defer clear(stored)
	derived, err := publicKeyOf(stored)
	if err != nil {
		return fmt.Sprintf("%s %s (version %s) is not an nkey user seed: %v", name, defaultLabel, version, err), ErrMismatch
	}
	if derived != publicKey {
		return fmt.Sprintf("%s %s (version %s) derives %s: no", name, defaultLabel, version, publicKey), ErrMismatch
	}
	return fmt.Sprintf("%s %s (version %s) derives %s: yes", name, defaultLabel, version, publicKey), nil
}

// publicKeyOf returns the public key of value, which must be a user seed spelled exactly as nkeys
// writes one. nkeys' own decoder skips line breaks and ignores the last character's two unused
// bits, so a value is taken only when its seed encodes back to the same text. Its errors say why
// value is no user seed, never quoting it.
func publicKeyOf(value []byte) (string, error) {
	prefix, raw, err := nkeys.DecodeSeed(value)
	var corrupt base32.CorruptInputError
	switch {
	case errors.As(err, &corrupt):
		return "", errors.New("it is not base32")
	case errors.Is(err, nkeys.ErrInvalidEncoding):
		return "", errors.New("it is too short to be one")
	case errors.Is(err, nkeys.ErrInvalidChecksum):
		return "", errors.New("its checksum does not match")
	case errors.Is(err, nkeys.ErrInvalidSeed):
		return "", errors.New("it is not a seed (wrong prefix byte)")
	case err != nil:
		return "", errors.New("it does not decode as an nkey seed")
	}
	defer clear(raw)
	if prefix != nkeys.PrefixByteUser {
		return "", errors.New("it is a seed, but not a user's")
	}
	canonical, err := nkeys.EncodeSeed(nkeys.PrefixByteUser, raw)
	if err != nil {
		return "", fmt.Errorf("it holds %d seed bytes, not the 32 of an ed25519 seed", len(raw))
	}
	defer clear(canonical)
	if !bytes.Equal(canonical, value) {
		return "", errors.New("it is not the canonical encoding of its seed")
	}
	user, err := nkeys.FromSeed(canonical)
	if err != nil {
		return "", errors.New("it does not decode as an nkey seed")
	}
	defer user.Wipe()
	return user.PublicKey()
}

// Stores opens each store the command's --store names: in region when it is not "", and, when
// account is not "" (--expect-account), refusing with ErrRefused unless the store's credentials are
// that account's, with no call before that check but the one reading them.
type Stores map[string]func(ctx context.Context, region, account string) (Store, error)

const usageText = `usage: envoy-infra seed {check|generate|verify} --store <store> --name <secret id> [--region <r>] [--expect-account <id>] [--public-key U…]

  check     whether the secret exists and which version its store serves by default
  generate  make an nkey user, write its seed as the secret's first version, and print its
            public key and the version id
  verify    whether the seed the store serves by default derives --public-key

The seed is never an argument, an output or a log line. Exit codes: 0 done (verify: the seed
derives the key); 1 verify found another key or no seed, or generate failed after its write,
so check the secret before anything else; 2 refused before any write, or a usage error.

`

// Command runs `envoy-infra seed` on args, the words after "seed", and returns its exit code.
func Command(ctx context.Context, args []string, stdout, stderr io.Writer, stores Stores) int {
	names := make([]string, 0, len(stores))
	for name := range stores {
		names = append(names, name)
	}
	slices.Sort(names)

	fs := flag.NewFlagSet("envoy-infra seed", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var store, name, region, account, publicKey string
	fs.StringVar(&store, "store", "", "the secret store: "+strings.Join(names, ", "))
	fs.StringVar(&name, "name", "", "the secret's id")
	fs.StringVar(&region, "region", "", "the store's region (default: the one its configuration names)")
	fs.StringVar(&account, "expect-account", "", "refuse, before any other call, unless the credentials are this account's")
	fs.StringVar(&publicKey, "public-key", "", "verify only: the U… public key the stored seed must derive")
	fs.Usage = func() {
		fmt.Fprint(stderr, usageText)
		fs.PrintDefaults()
	}
	usage := func(problem string) int {
		fmt.Fprintf(stderr, "envoy-infra seed: %s\n", problem)
		fs.Usage()
		return 2
	}

	if len(args) == 0 {
		return usage("a mode is required")
	}
	mode := args[0]
	switch mode {
	case "-h", "-help", "--help":
		fs.Usage()
		return 0
	case "check", "generate", "verify":
	default:
		// Not echoed: a seed pasted in its place must not reach the terminal.
		return usage("the mode is check, generate or verify")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2 // the flag package has printed the problem and the usage
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	switch {
	case fs.NArg() > 0:
		return usage("unexpected arguments after the flags")
	case store == "":
		return usage("--store is required")
	case stores[store] == nil:
		return usage("--store is one of: " + strings.Join(names, ", "))
	case name == "":
		return usage("--name is required")
	case mode == "verify" && !given["public-key"]:
		return usage("--public-key is required by verify")
	case mode != "verify" && given["public-key"]:
		return usage("--public-key is accepted by verify only")
	}

	s, err := stores[store](ctx, region, account)
	if err != nil {
		fmt.Fprintf(stderr, "refused: %s: %v\n", name, err)
		return 2
	}
	switch mode {
	case "check":
		line, err := Check(ctx, s, name)
		if err != nil {
			return report(stderr, err)
		}
		fmt.Fprintln(stdout, line)
	case "generate":
		public, version, err := Generate(ctx, s, name)
		if version != "" {
			fmt.Fprintf(stdout, "public key: %s\nversion id: %s\n", public, version)
		}
		if err != nil {
			return report(stderr, err)
		}
	default:
		line, err := Verify(ctx, s, name, publicKey)
		if line != "" {
			fmt.Fprintln(stdout, line)
		}
		if errors.Is(err, ErrMismatch) {
			return 1
		}
		if err != nil {
			return report(stderr, err)
		}
	}
	return 0
}

// report writes err on stderr and returns its exit code: 2 for a refusal, 1 for an error after a
// write.
func report(stderr io.Writer, err error) int {
	if errors.Is(err, ErrRefused) {
		fmt.Fprintf(stderr, "refused: %v\n", err)
		return 2
	}
	fmt.Fprintf(stderr, "error after writing: %v\n", err)
	return 1
}
