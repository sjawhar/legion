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
	"os"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
)

// Store is a secret store holding the seed. None of its errors carries a value: each names the call
// and the secret.
//
// check makes one call, Describe, and verify two, Describe and Current, so reading a secret needs
// no grant beyond its metadata and its value; a Current that finds its default version unserved may
// read that version's metadata once more to report it as Unserved. Only generate counts versions.
type Store interface {
	// Label names the version the store serves by default, in check's and verify's lines.
	Label() string
	// Describe reports, from the secret's metadata alone, whether it exists, whether it is
	// scheduled for deletion and when, and which version the store names as its default.
	Describe(ctx context.Context, name string) (Description, error)
	// Versions counts every version the secret holds, deprecated ones included.
	Versions(ctx context.Context, name string) (int, error)
	// Put writes value as the secret's first version and returns its version id. An error with a
	// version id means that version was written; one matching ErrRefused means nothing was, since
	// another writer's first version is already there; any other may follow a write that landed.
	// A store's Put must refuse a second first version with an error matching ErrRefused, or two
	// overlapping generate runs both write and the second's seed is served (testStoreContract).
	Put(ctx context.Context, name string, value []byte) (version string, err error)
	// Current reads the version the store serves by default. The value is the caller's, who
	// clears it once read. When the store finds no version to serve, the error matches
	// ErrNotServed; when it names one it will not serve, the error is an *Unserved.
	Current(ctx context.Context, name string) (version string, value []byte, err error)
}

// Version is the version a store names as its default: ID, the store's id for it, and State, ""
// when the store serves it, otherwise the state that keeps it from being served, as Secret
// Manager's versions/latest names the newest version even when it is DISABLED or DESTROYED.
type Version struct{ ID, State string }

// Description is what a store's metadata says of a secret. Default is read only for a secret that
// exists and is not scheduled for deletion.
type Description struct {
	Exists, ScheduledForDeletion bool
	DeletionDate                 string
	Default                      *Version // nil when the store names no version as its default
}

// Unserved is a Store's Current error when the version the store names as its default is one it
// will not serve, as a version disabled after Describe read it is: Version is that version, with
// its State. Its text is the failed read's own.
type Unserved struct {
	Version Version
	Err     error
}

func (e *Unserved) Error() string { return e.Err.Error() }
func (e *Unserved) Unwrap() error { return e.Err }

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

// ErrNotServed is matched by a Store's Current error when the store finds no version of the secret
// to serve by default, as it may for a moment after a write. A version the store finds and will not
// serve, such as a disabled newest version on Secret Manager, is a failure of its own.
var ErrNotServed = errors.New("seed: the store serves no version")

// notServed is a store's error that reads as its own and matches ErrNotServed.
type notServed struct{ error }

func (notServed) Is(target error) bool { return target == ErrNotServed }

// readBackWaits are the pauses generate takes, in turn, while the store does not yet show the
// version it wrote: both stores' reads are eventually consistent, Secrets Manager's and Secret
// Manager's listings and its latest alias, so a listing or a read right after a write can miss it.
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

// seededName refuses a secret's name that holds an nkey seed (bus.HoldsSeed), naming no part of it:
// every line, refusal and error after a write that a mode or a store makes quotes the name, and the
// exported API, unlike the command, takes the name as its caller gives it. Check, Generate, Verify
// and each store's methods call it first.
func seededName(name string) error {
	if bus.HoldsSeed(name) {
		return refuse("the secret's name holds an nkey seed, which is never a secret's name (not echoed)")
	}
	return nil
}

// Check returns the report line for the secret: absent, scheduled for deletion, or present with the
// version the store names as its default, in that store's label. It reads no value.
func Check(ctx context.Context, s Store, name string) (string, error) {
	if err := seededName(name); err != nil {
		return "", err
	}
	d, err := s.Describe(ctx, name)
	if err != nil {
		return "", refuse("%v", err)
	}
	switch {
	case !d.Exists:
		return name + ": absent", nil
	case d.ScheduledForDeletion:
		return fmt.Sprintf("%s: present, scheduled for deletion (%s)", name, d.DeletionDate), nil
	}
	return defaultLine(name, s.Label(), d.Default), nil
}

// defaultLine is Check's line for a secret that exists: that the store names no default version,
// the version it names, or that version and the state that keeps the store from serving it.
func defaultLine(name, label string, v *Version) string {
	switch {
	case v == nil:
		return fmt.Sprintf("%s: present; %s: no", name, label)
	case v.State != "":
		return fmt.Sprintf("%s: present; %s: version %s is %s", name, label, v.ID, v.State)
	}
	return fmt.Sprintf("%s: present; %s: yes (version %s)", name, label, v.ID)
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

// pythonDate is t in loc as Python's str prints a datetime: microseconds only when there are some,
// then the offset as ±hh:mm. check prints a deletion date in this form.
func pythonDate(t time.Time, loc *time.Location) string {
	t = t.In(loc)
	text := t.Format("2006-01-02 15:04:05")
	if micro := t.Nanosecond() / 1000; micro != 0 {
		text += fmt.Sprintf(".%06d", micro)
	}
	return text + t.Format("-07:00")
}

// Generate makes an nkey user and writes its seed as the secret's first version: it refuses a
// secret that is absent, scheduled for deletion, served by default or holding any version, so it
// never replaces a seed a client may already read. A version the store names as its default,
// whatever its state, is refused before the versions are listed; the listing then counts the rest,
// destroyed and disabled ones included. It calls announce with the public key just before Put is
// called, so the command prints the key before anything is written; when announce fails, Generate
// refuses and writes nothing, since a seed whose public key nobody saw could be checked by no one.
// After the write it lists the versions again, to check the secret holds that version alone, and
// reads back the version served by default, which must be that version and derive the public key
// it returns. Once Put has been called, every return carries the public key, and the version id
// when Put answered one: a version may exist, and verify needs the key to check it. A Put the
// store refuses wrote nothing, and its error matches ErrRefused.
func Generate(ctx context.Context, s Store, name string, announce func(publicKey string) error) (publicKey, version string, err error) {
	if err := seededName(name); err != nil {
		return "", "", err
	}
	d, err := s.Describe(ctx, name)
	if err != nil {
		return "", "", refuse("%v", err)
	}
	if err := existing(d, name); err != nil {
		return "", "", err
	}
	if v := d.Default; v != nil {
		// The listing below is eventually consistent; the default the metadata names is a witness
		// it cannot miss, so it is refused first, served or not.
		what := fmt.Sprintf("serves version %s by default (%s)", v.ID, s.Label())
		if v.State != "" {
			what = fmt.Sprintf("holds version %s, which %s names though it is %s", v.ID, s.Label(), v.State)
		}
		return "", "", refuse("%s already %s. generate writes only a secret's first version, so it never replaces a "+
			"seed a client may already read; use check or verify", name, what)
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

	if err := announce(publicKey); err != nil {
		return "", "", refuse("%s: could not print the public key: %v; nothing was written", name, err)
	}
	version, err = s.Put(ctx, name, value)
	if errors.Is(err, ErrRefused) {
		return publicKey, "", refuse("%v; nothing was written, and the public key above is not stored", err)
	}
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
// publicKey, with ErrMismatch when it does not or is no user seed. A default version the store does
// not serve fails with Check's line for it: before any value is read when Describe names it so, and
// after the read when Current answers it as Unserved. publicKey is never echoed unless it is a user
// public key: a seed pasted there by mistake must not reach the terminal twice.
func Verify(ctx context.Context, s Store, name, publicKey string) (string, error) {
	if err := seededName(name); err != nil {
		return "", err
	}
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
	switch {
	case d.Default == nil:
		return "", refuse("%s has no %s version; nothing to verify", name, label)
	case d.Default.State != "":
		return defaultLine(name, label, d.Default), ErrMismatch
	}
	version, stored, err := s.Current(ctx, name)
	if err != nil {
		var unserved *Unserved
		if errors.As(err, &unserved) {
			return defaultLine(name, label, &unserved.Version), ErrMismatch
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

// StoreType is one value of the command's --store. Open opens the store: in region when it is not
// "", and, when account is not "" (--expect-account), refusing with ErrRefused unless the store's
// credentials are that account's, with no call before that check but the one reading them; the
// store then holds each secret it describes to that account as well. A store that has no regions
// or accounts refuses a region or an account. CheckName, when set, refuses before Open a --name the
// store cannot hold, without quoting it: the refusal of an open names the secret, and a seed may
// have been pasted there. Help is what the command's usage says of the store and its flags.
type StoreType struct {
	Open      func(ctx context.Context, region, account string) (Store, error)
	CheckName func(name string) error
	Help      string
}

// Stores are the store types the command's --store names, by that name.
type Stores map[string]StoreType

// mode is one of the command's modes. It prints its result on stdout and returns its error; a
// result it cannot print is an error too, so the command never exits 0 having said nothing.
type mode func(ctx context.Context, s Store, name, publicKey string, stdout io.Writer) error

var modes = map[string]mode{
	"check": func(ctx context.Context, s Store, name, _ string, stdout io.Writer) error {
		line, err := Check(ctx, s, name)
		if err != nil {
			return err
		}
		return printResult(stdout, name, line, nil)
	},
	"generate": generateMode,
	"verify": func(ctx context.Context, s Store, name, publicKey string, stdout io.Writer) error {
		line, err := Verify(ctx, s, name, publicKey)
		if line == "" {
			return err
		}
		return printResult(stdout, name, line, err)
	},
}

// printResult prints the result line of a mode that writes nothing, then returns err. A line it
// cannot print leaves the caller with no result, so that is a refusal: running again is safe.
func printResult(stdout io.Writer, name, line string, err error) error {
	if _, printErr := fmt.Fprintln(stdout, line); printErr != nil {
		return refuse("%s: could not print the result: %v", name, printErr)
	}
	return err
}

// generateMode prints the public key on stdout before the write, so it is on the terminal whatever
// the write and the checks after it do, the process ending among them, and the version id once the
// write was tried: "unknown" when Put answered no version. A key it cannot print stops it before
// the write; a version id it cannot print is a failure after the write.
func generateMode(ctx context.Context, s Store, name, _ string, stdout io.Writer) error {
	announced := false
	_, version, err := Generate(ctx, s, name, func(publicKey string) error {
		if _, printErr := fmt.Fprintf(stdout, "public key: %s\n", publicKey); printErr != nil {
			return printErr
		}
		announced = true
		return nil
	})
	if !announced || errors.Is(err, ErrRefused) {
		// A refusal after the key was printed is a write the store refused: no version holds the key.
		return err
	}
	if version == "" {
		version = "unknown"
	}
	if _, printErr := fmt.Fprintf(stdout, "version id: %s\n", version); printErr != nil {
		if err != nil {
			return afterWritef("%v; and printing the version id failed: %v", err, printErr)
		}
		return afterWritef("version %s was written, but printing its version id failed: %v", version, printErr)
	}
	return err
}

const usageText = `usage: envoy-infra seed {check|generate|verify} --store <store> --name <secret> [--region <r>] [--expect-account <id>] [--public-key U…]

  check     whether the secret exists and which version its store serves by default
  generate  make an nkey user, write its seed as the secret's first version, and print its
            public key and the version id
  verify    whether the seed the store serves by default derives --public-key

The seed is never an argument, an output or a log line. Exit codes: 0 done (verify: the seed
derives the key); 1 verify found another key, no seed or a default version its store does not
serve, or generate failed after its write, so check the secret before anything else; 2 refused
before any write, or a usage error.

`

// Command runs `envoy-infra seed` on args, the words after "seed", and returns its exit code.
func Command(ctx context.Context, args []string, stdout, stderr io.Writer, stores Stores) int {
	names := slices.Sorted(maps.Keys(stores))

	fs := flag.NewFlagSet("envoy-infra seed", flag.ContinueOnError)
	// Parse's own messages echo an undefined flag word whole, and a seed pasted with a leading dash
	// would be that word, so Parse writes nowhere and Command prints every problem and the usage.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	var store, name, region, account, publicKey string
	fs.StringVar(&store, "store", "", "the secret store: "+strings.Join(names, ", "))
	fs.StringVar(&name, "name", "", "the secret, named as its store names it (below)")
	fs.StringVar(&region, "region", "", "the store's region, for a store that has regions (below)")
	fs.StringVar(&account, "expect-account", "", "refuse, before any other call, unless the credentials are this account's, "+
		"for a store that has accounts (below)")
	fs.StringVar(&publicKey, "public-key", "", "verify only: the U… public key the stored seed must derive")
	printUsage := func() {
		fmt.Fprint(stderr, usageText)
		fs.SetOutput(stderr)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)
		fmt.Fprintln(stderr, "\nStores:")
		for _, n := range names {
			fmt.Fprintf(stderr, "  %s\n    \t%s\n", n, stores[n].Help)
		}
	}
	usage := func(problem string) int {
		fmt.Fprintf(stderr, "envoy-infra seed: %s\n", problem)
		printUsage()
		return 2
	}

	if len(args) == 0 {
		return usage("a mode is required")
	}
	mode := args[0]
	if mode == "-h" || mode == "-help" || mode == "--help" {
		printUsage()
		return 0
	}
	run, ok := modes[mode]
	if !ok {
		// Not echoed: a seed pasted in its place must not reach the terminal.
		return usage("the mode is check, generate or verify")
	}
	if err := fs.Parse(args[1:]); err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			printUsage()
			return 0
		case strings.HasPrefix(err.Error(), "flag needs an argument: -"):
			// The flag package says this only of a flag it defines, so the name is one of ours.
			return usage(err.Error())
		}
		return usage("a flag this command does not take, or a malformed one (not echoed: a seed may have been pasted there)")
	}
	// A flag's value reaches the result lines, the refusals and the store's requests (--name is
	// every line's subject and the secret id it sends, --region the endpoint the SDK resolves), so a
	// value that holds an nkey seed (bus.HoldsSeed), whole or inside it, is refused naming the
	// flag, before the store opens. Visit visits the flags in name order.
	given := map[string]bool{}
	var seeded []string
	fs.Visit(func(f *flag.Flag) {
		given[f.Name] = true
		if bus.HoldsSeed(f.Value.String()) {
			seeded = append(seeded, "--"+f.Name)
		}
	})
	switch {
	case len(seeded) > 0:
		return usage(strings.Join(seeded, ", ") + ": an nkey seed, which is never an argument (not echoed)")
	case fs.NArg() > 0:
		return usage("unexpected arguments after the flags")
	case store == "":
		return usage("--store is required")
	case stores[store].Open == nil:
		return usage("--store is one of: " + strings.Join(names, ", "))
	case name == "":
		return usage("--name is required")
	case mode == "verify" && !given["public-key"]:
		return usage("--public-key is required by verify")
	case mode != "verify" && given["public-key"]:
		return usage("--public-key is accepted by verify only")
	case given["expect-account"] && account == "":
		// `--expect-account "$ACCOUNT"` with the variable unset would otherwise skip the check.
		return usage("--expect-account is given empty; it is a 12-digit AWS account id, or leave the flag out")
	}
	if mode == "generate" && nullDevice(stdout) {
		return exitCode(stderr, refuse("%s: stdout is the null device, where the public key would be lost (a closed stdout "+
			"is one too: Go opens it on %s); send stdout somewhere you can read. Nothing was written", name, os.DevNull))
	}
	if check := stores[store].CheckName; check != nil {
		if err := check(name); err != nil {
			return exitCode(stderr, refuse("%v", err))
		}
	}

	s, err := stores[store].Open(ctx, region, account)
	if err != nil {
		return exitCode(stderr, refuse("%s: %v", name, err))
	}
	return exitCode(stderr, run(ctx, s, name, publicKey, stdout))
}

// nullDevice reports whether w is the null device, comparing its device and inode numbers with
// os.DevNull's (os.SameFile, on Unix). A closed stdout is the null device too: the Go runtime opens a
// closed fd 0-2 on it at start (runtime/fds_unix.go checkfds). A writer that is no file, or a file
// that cannot be stat'd, is not: a write that then fails is caught where generate prints.
func nullDevice(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	got, err := f.Stat()
	if err != nil {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err == nil && os.SameFile(got, null)
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
