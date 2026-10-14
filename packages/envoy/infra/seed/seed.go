// Package seed is `envoy-infra seed`: it writes a NATS nkey user seed as a secret's first version,
// and checks and verifies the seed a secret holds, without the seed reaching argv, an output, a log
// or an error. The consumer declares the secret, empty; the public key generate prints goes into
// the server's authorization, so the stack that runs NATS never holds a seed.
package seed

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/infra/core"
)

// Store is a secret store holding the seed. None of its errors carries a value: each names the call
// and the secret.
//
// check makes one call, Describe, and verify two, Describe and Current, so reading a secret needs
// no grant beyond its metadata and its value; when Current fails for a store whose default is the
// newest version, verify describes the secret again. Only generate counts versions.
type Store interface {
	// Label names the version the store serves by default, in check's and verify's lines.
	Label() string
	// Describe reports, from the secret's metadata alone, whether it exists, whether it is
	// scheduled for deletion and when, and which version the store serves by default.
	Describe(ctx context.Context, name string) (Description, error)
	// Versions counts every version the secret holds, deprecated ones included.
	Versions(ctx context.Context, name string) (int, error)
	// Put writes value as the secret's first version and returns its version id. An error with a
	// version id means that version was written; one without may follow a write that landed.
	Put(ctx context.Context, name string, value []byte) (version string, err error)
	// Current reads the version the store serves by default. The value is the caller's, who
	// clears it once read. When the store serves no version, the error matches ErrNotServed.
	Current(ctx context.Context, name string) (version string, value []byte, err error)
}

// Description is what a store's metadata says of a secret. CurrentVersion is read only for a
// secret that exists and is not scheduled for deletion.
type Description struct {
	Exists, ScheduledForDeletion bool
	DeletionDate                 string
	CurrentVersion               string // "" when the store serves none
	// DefaultIsNewest is set by a store whose default is the newest version whatever its state
	// (GCP's versions/latest). It then reports that version's id and state, "" when the secret holds
	// none, and names it in CurrentVersion only when that state lets it be read.
	DefaultIsNewest            bool
	NewestVersion, NewestState string
}

// ErrRefused is every refusal's: nothing was written, so fixing the precondition its message names
// and running again is safe. The command exits 2.
//
// Generate's other errors come after Put returned: a version may have been written, so the secret
// is checked before anything else is done. The command exits 1 on them, and on ErrMismatch.
var ErrRefused = errors.New("seed: refused")

// ErrMismatch is Verify's when the version the store serves by default does not confirm the public
// key: its value is no user seed or derives another key, or the store does not serve the newest
// version it names. The command exits 1.
var ErrMismatch = errors.New("seed: the stored value does not derive the public key")

// ErrNotServed is matched by a Store's Current error when the store serves no version of the
// secret by default, as it may for a moment after a write.
var ErrNotServed = errors.New("seed: the store serves no version")

// readBackWaits are the pauses generate takes, in turn, while the store does not yet show the
// version it wrote: Secrets Manager's reads are eventually consistent, so a listing or a read right
// after a write can miss it.
var readBackWaits = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}

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

// Check returns the report line for the secret: absent, scheduled for deletion, or present with the
// version the store serves by default, in that store's terms. It reads no value.
func Check(ctx context.Context, s Store, name string) (string, error) {
	d, err := s.Describe(ctx, name)
	if err != nil {
		return "", refuse("%v", err)
	}
	label := s.Label()
	switch {
	case !d.Exists:
		return name + ": absent", nil
	case d.ScheduledForDeletion:
		return fmt.Sprintf("%s: present, scheduled for deletion (%s)", name, d.DeletionDate), nil
	case d.DefaultIsNewest:
		return newestLine(name, label, d), nil
	case d.CurrentVersion == "":
		return fmt.Sprintf("%s: present; %s: no", name, label), nil
	}
	return fmt.Sprintf("%s: present; %s: yes (version %s)", name, label, d.CurrentVersion), nil
}

// newestLine is Check's line for a secret of a store whose default is the newest version whatever
// its state: that the secret holds no version, or the newest one, with its state when the store does
// not serve it.
func newestLine(name, label string, d Description) string {
	switch {
	case d.NewestVersion == "":
		return name + ": present, no versions"
	case d.CurrentVersion == "":
		return fmt.Sprintf("%s: present; %s: version %s is %s", name, label, d.NewestVersion, d.NewestState)
	}
	return fmt.Sprintf("%s: present; %s: version %s", name, label, d.NewestVersion)
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
// public key it returns. Once Put has been called, every return carries the public key, and the
// version id when Put answered one: a version may exist, and verify needs the key to check it.
func Generate(ctx context.Context, s Store, name string) (publicKey, version string, err error) {
	return generate(ctx, s, name, func(string) {})
}

// generate is Generate, calling announce with the public key just before Put is called, so the
// command can print the key before anything is written.
func generate(ctx context.Context, s Store, name string, announce func(publicKey string)) (publicKey, version string, err error) {
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

	announce(publicKey)
	version, err = s.Put(ctx, name, value)
	if err != nil {
		if version == "" {
			return publicKey, "", afterWritef("%v; whether a version was written is unknown, run check", err)
		}
		return publicKey, version, afterWritef("%v", err)
	}
	read, stored, err := readBack(ctx, s, name, version)
	if err != nil {
		return publicKey, version, err
	}
	defer clear(stored)
	if read != version {
		return publicKey, version, afterWritef("version %s was written, but %s serves version %s by default: another "+
			"writer raced this one. Run verify with the public key above before committing it", version, name, read)
	}
	derived, err := core.UserSeedPublicKey(stored)
	if err != nil {
		return publicKey, version, afterWritef("version %s was written, but it reads back as no user seed: %v", version, err)
	}
	if derived != publicKey {
		return publicKey, version, afterWritef("version %s was written, but it does not derive the public key above", version)
	}
	return publicKey, version, nil
}

// readBack lists the secret's versions after generate wrote version, then reads the version the
// store serves by default. A listing of no version, or a store serving none, is a store that has
// not caught up with the write yet, so it looks again after each of readBackWaits. More than one
// version is another writer's race at once; so is a version served by default other than the
// written one, which the caller reports. Every error is an error after the write.
func readBack(ctx context.Context, s Store, name, version string) (read string, stored []byte, err error) {
	for attempt := 0; ; attempt++ {
		held, err := s.Versions(ctx, name)
		if err != nil {
			return "", nil, afterWritef("version %s was written, but listing versions afterwards failed: %v", version, err)
		}
		if held > 1 {
			return "", nil, afterWritef("%s now holds %d version(s), not only version %s: another writer raced "+
				"this one. Run verify with the public key above before committing it", name, held, version)
		}
		if held == 1 {
			read, stored, err = s.Current(ctx, name)
			if err == nil {
				return read, stored, nil
			}
			if !errors.Is(err, ErrNotServed) {
				return "", nil, afterWritef("version %s was written, but reading it back failed: %v", version, err)
			}
		}
		if attempt == len(readBackWaits) {
			var waited time.Duration
			for _, wait := range readBackWaits {
				waited += wait
			}
			return "", nil, afterWritef("version %s was written, but %s did not show it within %s: the store has not "+
				"caught up. Run check, then verify with the public key above", version, name, waited)
		}
		select {
		case <-ctx.Done():
			return "", nil, afterWritef("version %s was written, but looking for it again was stopped (%v). Run check, "+
				"then verify with the public key above", version, ctx.Err())
		case <-time.After(readBackWaits[attempt]):
		}
	}
}

// Verify returns the line saying whether the seed the store serves by default derives
// publicKey, with ErrMismatch when it does not or is no user seed. For a store whose default is the
// newest version whatever its state, a newest version the store does not serve fails with Check's
// line for it: before any value is read when Describe reports it, and after a failed Current when
// describing the secret again does. publicKey is never echoed unless it is a user public key: a
// seed pasted there by mistake must not reach the terminal twice.
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
	label := s.Label()
	if d.DefaultIsNewest && d.NewestVersion != "" && d.CurrentVersion == "" {
		return newestLine(name, label, d), ErrMismatch
	}
	if d.CurrentVersion == "" {
		return "", refuse("%s has no %s version; nothing to verify", name, label)
	}
	version, stored, err := s.Current(ctx, name)
	if err != nil {
		// The newest version can stop being served between Describe and Current: describing the
		// secret again tells that from a refused read, whose own error is the refusal otherwise.
		if d.DefaultIsNewest {
			if again, derr := s.Describe(ctx, name); derr == nil && again.NewestVersion != "" && again.CurrentVersion == "" {
				return newestLine(name, label, again), ErrMismatch
			}
		}
		return "", refuse("%v", err)
	}
	defer clear(stored)
	derived, err := core.UserSeedPublicKey(stored)
	if err != nil {
		return fmt.Sprintf("%s %s (version %s) is not an nkey user seed: %v", name, label, version, err), ErrMismatch
	}
	if derived != publicKey {
		return fmt.Sprintf("%s %s (version %s) derives %s: no", name, label, version, publicKey), ErrMismatch
	}
	return fmt.Sprintf("%s %s (version %s) derives %s: yes", name, label, version, publicKey), nil
}

// Stores opens each store the command's --store names: in region when it is not "", and, when
// account is not "" (--expect-account), refusing with ErrRefused unless the store's credentials are
// that account's, with no call before that check but the one reading them. A store that has no
// regions or accounts refuses a region or an account.
type Stores map[string]func(ctx context.Context, region, account string) (Store, error)

// nameRules refuse, before a store opens, a --name that store cannot hold, without quoting it: the
// refusal of an open names the secret, and a seed may have been pasted there.
var nameRules = map[string]func(name string) error{GCPSecretManager: gcpName}

// mode is one of the command's modes: it returns the line the command prints on stdout last, "" for
// none, and its error. Only generate writes to stdout itself, to print the public key before its
// write.
type mode func(ctx context.Context, s Store, name, publicKey string, stdout io.Writer) (string, error)

var modes = map[string]mode{
	"check": func(ctx context.Context, s Store, name, _ string, _ io.Writer) (string, error) {
		return Check(ctx, s, name)
	},
	"generate": generateMode,
	"verify": func(ctx context.Context, s Store, name, publicKey string, _ io.Writer) (string, error) {
		return Verify(ctx, s, name, publicKey)
	},
}

// generateMode prints the public key on stdout before the write, so it is on the terminal whatever
// the write and the checks after it do, the process ending among them, and returns the version id
// line once the write was tried: "unknown" when Put answered no version.
func generateMode(ctx context.Context, s Store, name, _ string, stdout io.Writer) (string, error) {
	announced := false
	_, version, err := generate(ctx, s, name, func(publicKey string) {
		fmt.Fprintf(stdout, "public key: %s\n", publicKey)
		announced = true
	})
	if !announced {
		return "", err
	}
	if version == "" {
		version = "unknown"
	}
	return "version id: " + version, err
}

const usageText = `usage: envoy-infra seed {check|generate|verify} --store <store> --name <secret id> [--region <r>] [--expect-account <id>] [--public-key U…]

  check     whether the secret exists and which version its store serves by default
  generate  make an nkey user, write its seed as the secret's first version, and print its
            public key and the version id
  verify    whether the seed the store serves by default derives --public-key

The seed is never an argument, an output or a log line. Exit codes: 0 done (verify: the seed
derives the key); 1 verify found another key, no seed or a latest version its store does not
serve, or generate failed after its write, so check the secret before anything else; 2 refused
before any write, or a usage error.

`

// Command runs `envoy-infra seed` on args, the words after "seed", and returns its exit code.
func Command(ctx context.Context, args []string, stdout, stderr io.Writer, stores Stores) int {
	names := slices.Sorted(maps.Keys(stores))

	fs := flag.NewFlagSet("envoy-infra seed", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var store, name, region, account, publicKey string
	fs.StringVar(&store, "store", "", "the secret store: "+strings.Join(names, ", "))
	fs.StringVar(&name, "name", "", "the secret's id; for "+GCPSecretManager+", projects/<project>/secrets/<secret>")
	fs.StringVar(&region, "region", "", AWSSecretsManager+" only: its region (default: the one its configuration names)")
	fs.StringVar(&account, "expect-account", "", AWSSecretsManager+" only: refuse, before any other call, unless the credentials are this account's")
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
	if mode == "-h" || mode == "-help" || mode == "--help" {
		fs.Usage()
		return 0
	}
	run, ok := modes[mode]
	if !ok {
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
	if rule := nameRules[store]; rule != nil {
		if err := rule(name); err != nil {
			return exitCode(stderr, refuse("%v", err))
		}
	}

	s, err := stores[store](ctx, region, account)
	if err != nil {
		return exitCode(stderr, refuse("%s: %v", name, err))
	}
	line, err := run(ctx, s, name, publicKey, stdout)
	if line != "" {
		fmt.Fprintln(stdout, line)
	}
	return exitCode(stderr, err)
}

// exitCode writes err on stderr and returns the command's exit code: 0 for none; 1 for ErrMismatch,
// whose line is already on stdout, so it writes nothing; 2 for a refusal; and 1 for any other error,
// which comes after a write.
func exitCode(stderr io.Writer, err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrMismatch):
		return 1
	case errors.Is(err, ErrRefused):
		fmt.Fprintf(stderr, "refused: %v\n", err)
		return 2
	}
	fmt.Fprintf(stderr, "error after writing: %v\n", err)
	return 1
}
