package seed_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/seed"
	"github.com/sjawhar/envoy/internal/testnats"
)

const secretName = "example/envoy/nats-nkey-seed-example-host-a"

// nats-io's own vector: the user seed and the public key nkeys' README and nk's README print for it
// (github.com/nats-io/nkeys v0.4.15 README.md lines 45 and 49, nk/README.md lines 36 and 44). The
// canary of the tests below: it must appear in no output and no error.
const (
	readmeSeed      = "SUAKYRHVIOREXV7EUZTBHUHL7NUMHPMAS7QMDU3GTIUWEI5LDNOXD43IZY"
	readmePublicKey = "UD466L6EBCM3YY5HEGHJANNTN4LSKTSUXTH7RILHCKEQMQHTBNLHJJXT"
)

// quiet is an announce that prints nothing, for a test that calls Generate directly.
func quiet(string) error { return nil }

type version struct {
	id    string
	value []byte
}

// fakeStore holds the one secret secretName in a chosen state and records every call.
type fakeStore struct {
	t       *testing.T
	exists  bool
	deleted bool
	// versions in the order they were written; current is the one the store names as its default.
	versions []version
	current  string
	calls    []string
	puts     [][]byte
	// unserved is the state of the default version when the store does not serve it, and
	// unservedAtCurrent the state it enters just before Current reads it.
	unserved, unservedAtCurrent string

	describeErr         error    // every Describe fails
	versionsErr         error    // every Versions fails
	versionsErrAfterPut error    // a Versions once Put has written fails
	putErr              error    // Put fails and writes nothing
	putErrAfterWrite    error    // Put writes, then answers its version with this error
	currentErr          error    // Current fails
	readBack            []byte   // what Current answers instead of the stored value
	racing              *version // another writer's version, which lands with Put's
	lateWriter          *version // another writer's version, which lands just before Current reads and is served
	beforePut           func()   // runs as Put is called, before it writes
	// An eventually consistent store: after the write, the next lagVersions listings count the
	// versions as they were before it, and the next lagCurrent reads find no version served.
	lagVersions, lagCurrent int
	// listsNone makes every listing before a write count no version, as a listing that has not
	// caught up with another writer's first version does.
	listsNone bool
}

func newStore(t *testing.T) *fakeStore { return &fakeStore{t: t, exists: true} }

func (f *fakeStore) Label() string { return "AWSCURRENT" }

func (f *fakeStore) holding(values ...string) *fakeStore {
	for i, v := range values {
		f.versions = append(f.versions, version{id: fmt.Sprintf("v%d", i+1), value: []byte(v)})
	}
	if len(f.versions) > 0 {
		f.current = f.versions[len(f.versions)-1].id
	}
	return f
}

func (f *fakeStore) named(name string) {
	if name != secretName {
		f.t.Errorf("the store was asked for %q, not %q", name, secretName)
	}
}

func (f *fakeStore) Describe(_ context.Context, name string) (seed.Description, error) {
	f.calls = append(f.calls, "describe")
	f.named(name)
	if f.describeErr != nil {
		return seed.Description{}, f.describeErr
	}
	switch {
	case !f.exists:
		return seed.Description{}, nil
	case f.deleted:
		return seed.Description{Exists: true, ScheduledForDeletion: true, DeletionDate: "2026-09-30 00:00:00+00:00"}, nil
	}
	d := seed.Description{Exists: true}
	if f.current != "" {
		d.Default = &seed.Version{ID: f.current, State: f.unserved}
	}
	return d, nil
}

func (f *fakeStore) Versions(_ context.Context, name string) (int, error) {
	f.calls = append(f.calls, "versions")
	f.named(name)
	if f.versionsErr != nil {
		return 0, f.versionsErr
	}
	if len(f.puts) > 0 && f.versionsErrAfterPut != nil {
		return 0, f.versionsErrAfterPut
	}
	if len(f.puts) == 0 && f.listsNone {
		return 0, nil
	}
	if len(f.puts) > 0 && f.lagVersions > 0 {
		f.lagVersions--
		return len(f.versions) - len(f.puts), nil
	}
	return len(f.versions), nil
}

func (f *fakeStore) Put(_ context.Context, name string, value []byte) (string, error) {
	f.calls = append(f.calls, "put")
	f.named(name)
	if f.beforePut != nil {
		f.beforePut()
	}
	f.puts = append(f.puts, bytes.Clone(value))
	if f.putErr != nil {
		return "", f.putErr
	}
	id := fmt.Sprintf("v%d", len(f.versions)+1)
	f.versions = append(f.versions, version{id: id, value: bytes.Clone(value)})
	f.current = id
	if f.racing != nil {
		f.versions = append(f.versions, *f.racing)
	}
	return id, f.putErrAfterWrite
}

func (f *fakeStore) Current(_ context.Context, name string) (string, []byte, error) {
	f.calls = append(f.calls, "current")
	f.named(name)
	if f.currentErr != nil {
		return "", nil, f.currentErr
	}
	if f.unservedAtCurrent != "" {
		f.unserved, f.unservedAtCurrent = f.unservedAtCurrent, ""
	}
	if f.unserved != "" {
		return "", nil, &seed.Unserved{
			Version: seed.Version{ID: f.current, State: f.unserved},
			Err:     errors.New("get-secret-value " + secretName + " failed: " + f.unserved),
		}
	}
	if len(f.puts) > 0 && f.lagCurrent > 0 {
		f.lagCurrent--
		return "", nil, fmt.Errorf("get-secret-value %s failed: ResourceNotFoundException (%w)", name, seed.ErrNotServed)
	}
	if len(f.puts) > 0 && f.lateWriter != nil {
		f.versions = append(f.versions, *f.lateWriter)
		f.current, f.lateWriter = f.lateWriter.id, nil
	}
	served := f.current
	for _, v := range f.versions {
		if v.id == served {
			// A fresh buffer, as a real store's: the caller clears the one it is handed.
			if f.readBack != nil {
				return v.id, bytes.Clone(f.readBack), nil
			}
			return v.id, bytes.Clone(v.value), nil
		}
	}
	f.t.Errorf("Current was called on a store serving no version")
	return "", nil, errors.New("fake: no version is served")
}

// values is every value the store holds or was handed: none may reach an output or an error.
func (f *fakeStore) values() []string {
	var all []string
	for _, v := range f.versions {
		all = append(all, string(v.value))
	}
	for _, p := range f.puts {
		all = append(all, string(p))
	}
	if f.readBack != nil {
		all = append(all, string(f.readBack))
	}
	return all
}

type opened struct {
	calls           int
	region, account string
}

// run runs the command against store as --store fake-store, returning its exit code and output.
func run(t *testing.T, store *fakeStore, args ...string) (code int, stdout, stderr string, o *opened) {
	t.Helper()
	o = &opened{}
	stores := seed.Stores{"fake-store": {Help: "the fake store's own help", Open: func(_ context.Context, region, account string) (seed.Store, error) {
		o.calls++
		o.region, o.account = region, account
		return store, nil
	}}}
	var out, errs bytes.Buffer
	code = seed.Command(context.Background(), args, &out, &errs, stores)
	return code, out.String(), errs.String(), o
}

func mode(name string, extra ...string) []string {
	return append([]string{name, "--store", "fake-store", "--name", secretName}, extra...)
}

func requireNoValue(t *testing.T, store *fakeStore, texts ...string) {
	t.Helper()
	for _, value := range append(store.values(), readmeSeed) {
		if value == "" {
			continue // an empty stored value is in every text
		}
		for _, text := range texts {
			require.NotContains(t, text, value, "a seed reached an output or an error")
		}
	}
}

func TestCheckPrintsOneOfTheReferenceLines(t *testing.T) {
	rows := []struct {
		name  string
		store func(*fakeStore) *fakeStore
		want  string
	}{
		{"absent", func(f *fakeStore) *fakeStore { f.exists = false; return f }, secretName + ": absent"},
		{"scheduled for deletion", func(f *fakeStore) *fakeStore { f.deleted = true; return f.holding(readmeSeed) },
			secretName + ": present, scheduled for deletion (2026-09-30 00:00:00+00:00)"},
		{"empty", func(f *fakeStore) *fakeStore { return f }, secretName + ": present; AWSCURRENT: no"},
		{"seeded", func(f *fakeStore) *fakeStore { return f.holding(readmeSeed) }, secretName + ": present; AWSCURRENT: yes (version v1)"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			store := row.store(newStore(t))
			code, out, errs, _ := run(t, store, mode("check")...)
			require.Equal(t, 0, code, errs)
			require.Equal(t, row.want+"\n", out)
			require.Empty(t, errs)
			require.Empty(t, store.puts)
			require.Equal(t, []string{"describe"}, store.calls, "check describes the secret and lists no versions")
			requireNoValue(t, store, out, errs)

			line, err := seed.Check(context.Background(), row.store(newStore(t)), secretName)
			require.NoError(t, err)
			require.Equal(t, row.want, line)
		})
	}
}

func TestGenerateWritesOneSeedAndReadsBackTheVersionItWrote(t *testing.T) {
	store := newStore(t)
	code, out, errs, _ := run(t, store, mode("generate")...)
	require.Equal(t, 0, code, errs)
	require.Empty(t, errs)
	require.Equal(t, []string{"describe", "versions", "put", "versions", "current"}, store.calls)
	require.Len(t, store.puts, 1)
	written := store.puts[0]
	require.True(t, bytes.HasPrefix(written, []byte("SU")), "the stored value is a bare user seed")
	kp, err := nkeys.FromSeed(written)
	require.NoError(t, err)
	public, err := kp.PublicKey()
	require.NoError(t, err)
	require.Equal(t, "public key: "+public+"\nversion id: v1\n", out)
	requireNoValue(t, store, out, errs)

	// A second store gets a seed of its own.
	again := newStore(t)
	code, _, errs, _ = run(t, again, mode("generate")...)
	require.Equal(t, 0, code, errs)
	require.NotEqual(t, written, again.puts[0])
}

// Every refusal exits 2, names the secret, writes nothing and prints nothing on stdout.
func TestEveryRefusalExitsTwoAndWritesNothing(t *testing.T) {
	_, otherPublic := testnats.User(t)
	denied := errors.New("describe-secret " + secretName + " failed: AccessDeniedException")
	rows := []struct {
		name  string
		args  []string
		store func(*fakeStore) *fakeStore
		want  string
	}{
		{"generate: absent", mode("generate"), func(f *fakeStore) *fakeStore { f.exists = false; return f }, "does not exist"},
		{"generate: scheduled for deletion", mode("generate"), func(f *fakeStore) *fakeStore { f.deleted = true; return f },
			"is scheduled for deletion (2026-09-30 00:00:00+00:00)"},
		// The default version is named from metadata alone: generate refuses on it before it lists,
		// so the refusal stands even when the listing, which is eventually consistent, shows none.
		{"generate: a default version", mode("generate"), func(f *fakeStore) *fakeStore { return f.holding(readmeSeed) },
			"already serves version v1 by default (AWSCURRENT)"},
		{"generate: a served version the listing does not show", mode("generate"), func(f *fakeStore) *fakeStore {
			f.listsNone = true
			return f.holding(readmeSeed)
		}, "already serves version v1 by default (AWSCURRENT)"},
		{"generate: a default version the store does not serve", mode("generate"), func(f *fakeStore) *fakeStore {
			f.unserved = "DISABLED"
			return f.holding(readmeSeed, readmeSeed)
		}, "already holds version v2, which AWSCURRENT names though it is DISABLED"},
		// A version the store does not name as its default is still a seed someone may have read.
		{"generate: versions and no default", mode("generate"), func(f *fakeStore) *fakeStore {
			f.holding(readmeSeed, readmeSeed).current = ""
			return f
		}, "already holds 2 version(s)"},
		{"generate: describe fails", mode("generate"), func(f *fakeStore) *fakeStore { f.describeErr = denied; return f }, "AccessDeniedException"},
		{"generate: listing versions fails", mode("generate"), func(f *fakeStore) *fakeStore {
			f.versionsErr = errors.New("list-secret-version-ids " + secretName + " failed: ThrottlingException")
			return f
		}, "ThrottlingException"},
		{"check: describe fails", mode("check"), func(f *fakeStore) *fakeStore { f.describeErr = denied; return f }, "AccessDeniedException"},
		{"verify: absent", mode("verify", "--public-key", readmePublicKey), func(f *fakeStore) *fakeStore { f.exists = false; return f }, "does not exist"},
		{"verify: scheduled for deletion", mode("verify", "--public-key", readmePublicKey),
			func(f *fakeStore) *fakeStore { f.deleted = true; return f.holding(readmeSeed) }, "is scheduled for deletion"},
		{"verify: no default version", mode("verify", "--public-key", readmePublicKey), func(f *fakeStore) *fakeStore { return f },
			"has no AWSCURRENT version; nothing to verify"},
		{"verify: describe fails", mode("verify", "--public-key", otherPublic), func(f *fakeStore) *fakeStore { f.describeErr = denied; return f.holding(readmeSeed) },
			"AccessDeniedException"},
		{"verify: reading fails", mode("verify", "--public-key", otherPublic), func(f *fakeStore) *fakeStore {
			f.currentErr = errors.New("get-secret-value " + secretName + " failed: DecryptionFailure")
			return f.holding(readmeSeed)
		}, "DecryptionFailure"},
		{"verify: a malformed public key", mode("verify", "--public-key", "U-not-a-key"), func(f *fakeStore) *fakeStore { return f.holding(readmeSeed) },
			"--public-key is not an nkey user public key"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			store := row.store(newStore(t))
			before := slices.Clone(store.versions)
			code, out, errs, _ := run(t, store, row.args...)
			require.Equal(t, 2, code, errs)
			require.Empty(t, out)
			require.True(t, strings.HasPrefix(errs, "refused: "), errs)
			require.Contains(t, errs, secretName)
			require.Contains(t, errs, row.want)
			require.NotContains(t, store.calls, "put")
			require.Equal(t, before, store.versions)
			requireNoValue(t, store, out, errs)
		})
	}
}

// A refusal of the store's opener (the account check) exits 2 before the store is called.
func TestARefusedOpenExitsTwoBeforeAnyStoreCall(t *testing.T) {
	store := newStore(t)
	stores := seed.Stores{"fake-store": {Open: func(context.Context, string, string) (seed.Store, error) {
		return nil, fmt.Errorf("%w: these credentials are for account 111111111111, but --expect-account is 000000000000", seed.ErrRefused)
	}}}
	var out, errs bytes.Buffer
	code := seed.Command(context.Background(), mode("generate", "--expect-account", "000000000000"), &out, &errs, stores)
	require.Equal(t, 2, code)
	require.Empty(t, out.String())
	require.Contains(t, errs.String(), "refused: "+secretName+": ")
	require.Contains(t, errs.String(), "account 111111111111")
	require.Empty(t, store.calls)
}

func TestTheOpenerGetsTheRegionAndTheExpectedAccount(t *testing.T) {
	store := newStore(t)
	code, _, errs, o := run(t, store, mode("check", "--region", "us-east-1", "--expect-account", "000000000000")...)
	require.Equal(t, 0, code, errs)
	require.Equal(t, opened{calls: 1, region: "us-east-1", account: "000000000000"}, *o)
}

func TestVerify(t *testing.T) {
	_, otherPublic := testnats.User(t)
	t.Run("the stored seed derives the key", func(t *testing.T) {
		store := newStore(t).holding(readmeSeed)
		code, out, errs, _ := run(t, store, mode("verify", "--public-key", readmePublicKey)...)
		require.Equal(t, 0, code, errs)
		require.Equal(t, secretName+" AWSCURRENT (version v1) derives "+readmePublicKey+": yes\n", out)
		require.Empty(t, errs)
		require.Equal(t, []string{"describe", "current"}, store.calls, "verify describes and reads, and lists no versions")
		requireNoValue(t, store, out, errs)
	})
	t.Run("the stored seed derives another key", func(t *testing.T) {
		store := newStore(t).holding(readmeSeed)
		code, out, errs, _ := run(t, store, mode("verify", "--public-key", otherPublic)...)
		require.Equal(t, 1, code)
		require.Equal(t, secretName+" AWSCURRENT (version v1) derives "+otherPublic+": no\n", out)
		require.Empty(t, errs)
		require.Equal(t, []string{"describe", "current"}, store.calls, "verify describes and reads, and lists no versions")
		requireNoValue(t, store, out, errs)

		line, err := seed.Verify(context.Background(), newStore(t).holding(readmeSeed), secretName, otherPublic)
		require.ErrorIs(t, err, seed.ErrMismatch)
		require.NotErrorIs(t, err, seed.ErrRefused)
		require.Equal(t, secretName+" AWSCURRENT (version v1) derives "+otherPublic+": no", line)
	})
}

// A default version the store names but does not serve gets check's line with its state, which
// verify fails with, exit 1: a failed check, not a refusal, though nothing was written. When
// Describe already names it so, verify reads no value; when it stops being served between Describe
// and Current, Current answers it as an Unserved.
func TestADefaultVersionTheStoreDoesNotServe(t *testing.T) {
	for _, state := range []string{"DISABLED", "DESTROYED"} {
		want := secretName + ": present; AWSCURRENT: version v2 is " + state
		t.Run(state+"/check", func(t *testing.T) {
			store := newStore(t).holding(readmeSeed, readmeSeed)
			store.unserved = state
			code, out, errs, _ := run(t, store, mode("check")...)
			require.Equal(t, 0, code, errs)
			require.Equal(t, want+"\n", out)
			require.Equal(t, []string{"describe"}, store.calls)
		})
		for _, when := range []struct {
			name  string
			set   func(*fakeStore)
			calls []string
		}{
			{"before verify", func(f *fakeStore) { f.unserved = state }, []string{"describe"}},
			{"between describe and current", func(f *fakeStore) { f.unservedAtCurrent = state }, []string{"describe", "current"}},
		} {
			t.Run(state+"/verify/"+when.name, func(t *testing.T) {
				unserved := func() *fakeStore {
					f := newStore(t).holding(readmeSeed, readmeSeed)
					when.set(f)
					return f
				}
				store := unserved()
				code, out, errs, _ := run(t, store, mode("verify", "--public-key", readmePublicKey)...)
				require.Equal(t, 1, code, errs)
				require.Equal(t, want+"\n", out)
				require.Empty(t, errs)
				require.Equal(t, when.calls, store.calls)
				requireNoValue(t, store, out, errs)

				line, err := seed.Verify(context.Background(), unserved(), secretName, readmePublicKey)
				require.ErrorIs(t, err, seed.ErrMismatch)
				require.NotErrorIs(t, err, seed.ErrRefused)
				require.Equal(t, want, line)
			})
		}
	}
}

// Any other failed read is the refusal, exit 2, with the read's own error.
func TestVerifyRefusesAFailedRead(t *testing.T) {
	store := newStore(t).holding(readmeSeed)
	store.currentErr = errors.New("get-secret-value " + secretName + " failed: AccessDeniedException")
	code, out, errs, _ := run(t, store, mode("verify", "--public-key", readmePublicKey)...)
	require.Equal(t, 2, code, errs)
	require.Empty(t, out)
	require.Equal(t, []string{"describe", "current"}, store.calls)
	require.Equal(t, "refused: get-secret-value "+secretName+" failed: AccessDeniedException\n", errs)
}

// A stored value that is no user seed gets verify's not-a-seed line and exit 1, with the reason
// decided in this order: not base32 (strict: no line break, lowercase or misplaced padding), not 36
// bytes, a wrong checksum, not a seed's prefix, not a user's type, not the canonical spelling. Every
// want is the line the reference script printed for the same value, under the same secret name and
// version; its value is never echoed.
func TestVerifyReportsAStoredValueThatIsNoUserSeed(t *testing.T) {
	const notASeed = secretName + " AWSCURRENT (version v1) is not an nkey user seed: "
	rows := []struct{ name, value, want string }{
		{"a user public key", readmePublicKey, notASeed + "the value decodes to 35 bytes, not the 36 of a seed"},
		{"empty", "", notASeed + "the value decodes to 0 bytes, not the 36 of a seed"},
		{"a trailing line break", readmeSeed + "\n", notASeed + "the value is not base32"},
		{"a trailing carriage return and line break", readmeSeed + "\r\n", notASeed + "the value is not base32"},
		{"lowercase", strings.ToLower(readmeSeed), notASeed + "the value is not base32"},
		{"not base32", "SU!!!not-base32", notASeed + "the value is not base32"},
		{"padding inside", readmeSeed[:8] + "=" + readmeSeed[9:], notASeed + "the value is not base32"},
		{"a truncated seed", readmeSeed[:56], notASeed + "the value decodes to 35 bytes, not the 36 of a seed"},
		{"one character changed", readmeSeed[:5] + "A" + readmeSeed[6:], notASeed + "the value's checksum does not match"},
		// The README seed's 32 bytes behind other prefix bytes, each with a valid checksum: (0, 0);
		// a seed's prefix with type 8, which nkeys knows no key of; a seed's prefix with the account
		// type (nkeys' account seed).
		{"another prefix byte", "AAAKYRHVIOREXV7EUZTBHUHL7NUMHPMAS7QMDU3GTIUWEI5LDNOXD43I5I",
			notASeed + "the value is not a seed (wrong prefix byte)"},
		{"a seed of no known type", "SBAKYRHVIOREXV7EUZTBHUHL7NUMHPMAS7QMDU3GTIUWEI5LDNOXD442ME",
			notASeed + "the value is a seed, but not a user's"},
		{"an account seed", "SAAKYRHVIOREXV7EUZTBHUHL7NUMHPMAS7QMDU3GTIUWEI5LDNOXD443AI",
			notASeed + "the value is a seed, but not a user's"},
		// Y -> Z changes only the last character's two unused bits: the same bytes, another spelling.
		{"another spelling", strings.TrimSuffix(readmeSeed, "Y") + "Z", notASeed + "the value is not the canonical encoding of its seed"},
		{"padded", readmeSeed + "======", notASeed + "the value is not the canonical encoding of its seed"},
	}
	require.True(t, strings.HasSuffix(readmeSeed, "Y"), "the spelling row changes a final Y")
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			store := newStore(t).holding(row.value)
			code, out, errs, _ := run(t, store, mode("verify", "--public-key", readmePublicKey)...)
			require.Equal(t, 1, code, errs)
			require.Equal(t, row.want+"\n", out)
			require.Empty(t, errs)
			requireNoValue(t, store, out, errs)

			line, err := seed.Verify(context.Background(), newStore(t).holding(row.value), secretName, readmePublicKey)
			require.ErrorIs(t, err, seed.ErrMismatch)
			require.Equal(t, row.want, line)
		})
	}
}

// After the write, generate exits 1, never 2. Its stdout already holds the public key, which it
// printed before the write, and the version id Put answered, or "unknown" when Put answered none:
// a version may exist either way, and verify needs the key.
func TestGenerateReportsEveryFailureAfterItsWrite(t *testing.T) {
	otherSeed, _ := testnats.User(t)
	rows := []struct {
		name    string
		store   func(*fakeStore) *fakeStore
		want    string
		version string
	}{
		{"put fails", func(f *fakeStore) *fakeStore {
			f.putErr = errors.New("put-secret-value " + secretName + " failed: RequestSendError")
			return f
		}, "whether a version was written is unknown, run check", "unknown"},
		{"put writes a version that is not served", func(f *fakeStore) *fakeStore {
			f.putErrAfterWrite = errors.New("version v1 was written but is not AWSCURRENT; run check")
			return f
		}, "version v1 was written but is not AWSCURRENT; run check", "v1"},
		{"listing afterwards fails", func(f *fakeStore) *fakeStore {
			f.versionsErrAfterPut = errors.New("list-secret-version-ids " + secretName + " failed: ThrottlingException")
			return f
		}, "version v1 was written, but listing versions afterwards failed: list-secret-version-ids " + secretName +
			" failed: ThrottlingException", "v1"},
		{"another writer raced it", func(f *fakeStore) *fakeStore {
			f.racing = &version{id: "their-version", value: []byte(otherSeed)}
			return f
		}, secretName + " now holds 2 version(s), not only version v1: another writer raced this one. " +
			"Run verify with the public key above before committing it", "v1"},
		{"another writer's version is served by the read-back", func(f *fakeStore) *fakeStore {
			f.lateWriter = &version{id: "their-version", value: []byte(otherSeed)}
			return f
		}, "version v1 was written, but " + secretName + " serves version their-version by default: another writer raced this one", "v1"},
		{"reading back fails", func(f *fakeStore) *fakeStore {
			f.currentErr = errors.New("get-secret-value " + secretName + " failed: DecryptionFailure")
			return f
		}, "version v1 was written, but reading it back failed: get-secret-value " + secretName + " failed: DecryptionFailure", "v1"},
		{"it reads back as another seed", func(f *fakeStore) *fakeStore { f.readBack = []byte(readmeSeed); return f },
			"version v1 was written, but it does not derive the public key above", "v1"},
		{"it reads back as no seed", func(f *fakeStore) *fakeStore { f.readBack = []byte(readmeSeed[:5] + "A" + readmeSeed[6:]); return f },
			"version v1 was written, but it reads back as no user seed: the value's checksum does not match", "v1"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			store := row.store(newStore(t))
			code, out, errs, _ := run(t, store, mode("generate")...)
			require.Equal(t, 1, code, errs)
			require.True(t, strings.HasPrefix(errs, "error after writing: "), errs)
			require.Contains(t, errs, row.want)
			require.NotContains(t, errs, "refused")
			public := publicKeyOfSeed(t, store.puts[0])
			require.Equal(t, "public key: "+public+"\nversion id: "+row.version+"\n", out)
			requireNoValue(t, store, out, errs)

			direct := row.store(newStore(t))
			got, version, err := seed.Generate(context.Background(), direct, secretName, quiet)
			require.Error(t, err)
			require.Equal(t, publicKeyOfSeed(t, direct.puts[0]), got, "Generate returns the key on every path after Put")
			require.Equal(t, strings.TrimSuffix(row.version, "unknown"), version)
		})
	}
}

// publicKeyOfSeed is the public key of a user seed, by nkeys.
func publicKeyOfSeed(t *testing.T, seed []byte) string {
	t.Helper()
	kp, err := nkeys.FromSeed(seed)
	require.NoError(t, err)
	public, err := kp.PublicKey()
	require.NoError(t, err)
	return public
}

// Right after the write, an eventually consistent store may list no version yet, or serve none by
// default. generate looks again after each of its pauses, and is satisfied once it sees its own
// version alone and reads it back; a store still lagging after the last pause fails as an error
// after writing, naming no race.
func TestGenerateWaitsForTheStoreToShowItsWrite(t *testing.T) {
	seed.SetReadBackWaits(t, 0, 0, 0)
	rows := []struct {
		name                    string
		lagVersions, lagCurrent int
		calls                   []string
	}{
		{"the listing lags", 2, 0, []string{"describe", "versions", "put", "versions", "versions", "versions", "current"}},
		{"the read-back lags", 0, 2, []string{"describe", "versions", "put", "versions", "current", "versions", "current", "versions", "current"}},
		{"both lag", 1, 1, []string{"describe", "versions", "put", "versions", "versions", "current", "versions", "current"}},
		{"the last look sees it", 3, 0, []string{"describe", "versions", "put", "versions", "versions", "versions", "versions", "current"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			store := newStore(t)
			store.lagVersions, store.lagCurrent = row.lagVersions, row.lagCurrent
			code, out, errs, _ := run(t, store, mode("generate")...)
			require.Equal(t, 0, code, errs)
			require.Empty(t, errs)
			require.Equal(t, "public key: "+publicKeyOfSeed(t, store.puts[0])+"\nversion id: v1\n", out)
			require.Equal(t, row.calls, store.calls)
		})
	}
	for _, lag := range []struct {
		name              string
		versions, current int
	}{{"the listing", 4, 0}, {"the read-back", 0, 4}} {
		t.Run(lag.name+" lags past the last pause", func(t *testing.T) {
			store := newStore(t)
			store.lagVersions, store.lagCurrent = lag.versions, lag.current
			code, out, errs, _ := run(t, store, mode("generate")...)
			require.Equal(t, 1, code, errs)
			require.Equal(t, "public key: "+publicKeyOfSeed(t, store.puts[0])+"\nversion id: v1\n", out)
			require.Equal(t, "error after writing: version v1 was written, but "+secretName+" did not show it within 0s: "+
				"the store has not caught up. Run check, then verify with the public key above\n", errs)
			afterPut := store.calls[slices.Index(store.calls, "put")+1:]
			require.Equal(t, 4, countOf(afterPut, "versions"), "one look after the write and one after each of the three pauses: %v", afterPut)
		})
	}
}

func countOf(calls []string, call string) int {
	n := 0
	for _, c := range calls {
		if c == call {
			n++
		}
	}
	return n
}

// putRefusal is a store's refusal of a write: another writer's first version, with another value,
// is already there, so this one wrote nothing.
type putRefusal struct{}

func (putRefusal) Error() string {
	return "put-secret-value " + secretName + " failed: ResourceExistsException; another generate wrote this secret's first version"
}
func (putRefusal) Is(target error) bool { return target == seed.ErrRefused }

// A Put the store refuses wrote nothing: generate exits 2 with the refusal, and after the public key
// it printed before the write, it prints no version id, since no version holds that key.
func TestGenerateRefusesWhenTheStoreRefusesItsWrite(t *testing.T) {
	store := newStore(t)
	store.putErr = putRefusal{}
	code, out, errs, _ := run(t, store, mode("generate")...)
	require.Equal(t, 2, code)
	require.Equal(t, "public key: "+publicKeyOfSeed(t, store.puts[0])+"\n", out)
	require.Equal(t, "refused: put-secret-value "+secretName+" failed: ResourceExistsException; another generate "+
		"wrote this secret's first version; nothing was written, and the public key above is not stored\n", errs)
	requireNoValue(t, store, out, errs)
}

// generate prints the public key before it writes the seed, so the key is on the terminal whatever
// the write and the checks after it do, the process ending among them.
func TestGeneratePrintsThePublicKeyBeforeItsWrite(t *testing.T) {
	store := newStore(t)
	var out, errs bytes.Buffer
	printed := "(Put was not called)"
	store.beforePut = func() { printed = out.String() }
	code := seed.Command(context.Background(), mode("generate"), &out, &errs,
		seed.Stores{"fake-store": {Open: func(context.Context, string, string) (seed.Store, error) { return store, nil }}})
	require.Equal(t, 0, code, errs.String())
	public := publicKeyOfSeed(t, store.puts[0])
	require.Equal(t, "public key: "+public+"\n", printed)
	require.Equal(t, "public key: "+public+"\nversion id: v1\n", out.String())
}

// failingWriter accepts its first ok writes and fails every later one with err, as a closed
// stdout (EBADF), a full disk (ENOSPC) or a pipe whose reader left (EPIPE, SIGPIPE ignored) does.
type failingWriter struct {
	ok      int
	err     error
	written bytes.Buffer
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.ok == 0 {
		return 0, w.err
	}
	w.ok--
	return w.written.Write(p)
}

var errStdout = errors.New("write /dev/stdout: bad file descriptor")

func commandWith(store *fakeStore, stdout io.Writer, args ...string) (code int, stderr string) {
	var errs bytes.Buffer
	code = seed.Command(context.Background(), args, stdout, &errs,
		seed.Stores{"fake-store": {Open: func(context.Context, string, string) (seed.Store, error) { return store, nil }}})
	return code, errs.String()
}

// generate with stdout on the null device would print its key nowhere, so it refuses before the
// store opens. check and verify write nothing, so they still run: a script may want only the exit
// code.
func TestGenerateRefusesANullStdoutBeforeOpeningTheStore(t *testing.T) {
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = null.Close() })
	store := newStore(t)
	opened := 0
	stores := seed.Stores{"fake-store": {Open: func(context.Context, string, string) (seed.Store, error) {
		opened++
		return store, nil
	}}}
	var errs bytes.Buffer
	code := seed.Command(context.Background(), mode("generate"), null, &errs, stores)
	require.Equal(t, 2, code)
	require.Equal(t, "refused: "+secretName+": stdout is the null device, where the public key would be lost (a closed "+
		"stdout is one too: Go opens it on /dev/null); send stdout somewhere you can read. Nothing was written\n", errs.String())
	require.Zero(t, opened)

	errs.Reset()
	require.Equal(t, 0, seed.Command(context.Background(), mode("check"), null, &errs, stores), errs.String())
	require.Equal(t, []string{"describe"}, store.calls)
}

// generate never writes a seed whose public key it could not print: a print that fails is a
// refusal before Put, so nothing is written and running again is safe.
func TestGenerateWritesNothingWhenItCannotPrintThePublicKey(t *testing.T) {
	store := newStore(t)
	code, errs := commandWith(store, &failingWriter{err: errStdout}, mode("generate")...)
	require.Equal(t, 2, code)
	require.Equal(t, "refused: "+secretName+": could not print the public key: "+errStdout.Error()+"; nothing was written\n", errs)
	require.Empty(t, store.puts)
	require.Equal(t, []string{"describe", "versions"}, store.calls, "Put is never called")
}

// Once the key is printed and the seed written, a version id that cannot be printed is a failure
// after the write, exit 1, and adds to any failure the write already had.
func TestGenerateReportsAVersionIDItCannotPrintAsAFailureAfterItsWrite(t *testing.T) {
	t.Run("after a write that succeeded", func(t *testing.T) {
		store := newStore(t)
		stdout := &failingWriter{ok: 1, err: errStdout}
		code, errs := commandWith(store, stdout, mode("generate")...)
		require.Equal(t, 1, code)
		require.Equal(t, "error after writing: version v1 was written, but printing its version id failed: "+errStdout.Error()+"\n", errs)
		require.Equal(t, "public key: "+publicKeyOfSeed(t, store.puts[0])+"\n", stdout.written.String())
		require.Len(t, store.puts, 1)
	})
	t.Run("after a write that failed", func(t *testing.T) {
		store := newStore(t)
		store.currentErr = errors.New("get-secret-value " + secretName + " failed: DecryptionFailure")
		stdout := &failingWriter{ok: 1, err: errStdout}
		code, errs := commandWith(store, stdout, mode("generate")...)
		require.Equal(t, 1, code)
		require.Equal(t, "error after writing: version v1 was written, but reading it back failed: get-secret-value "+secretName+
			" failed: DecryptionFailure; and printing the version id failed: "+errStdout.Error()+"\n", errs)
		require.Equal(t, "public key: "+publicKeyOfSeed(t, store.puts[0])+"\n", stdout.written.String())
	})
}

// check and verify write nothing, so a result line they cannot print is exit 2: the caller has no
// result, and running again is safe. A verify mismatch is no exception, since its line is the
// result.
func TestCheckAndVerifyExitTwoWhenTheirLineCannotBePrinted(t *testing.T) {
	_, otherPublic := testnats.User(t)
	rows := []struct {
		name string
		args []string
	}{
		{"check", mode("check")},
		{"verify: yes", mode("verify", "--public-key", readmePublicKey)},
		{"verify: no", mode("verify", "--public-key", otherPublic)},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			store := newStore(t).holding(readmeSeed)
			code, errs := commandWith(store, &failingWriter{err: errStdout}, row.args...)
			require.Equal(t, 2, code)
			require.Equal(t, "refused: "+secretName+": could not print the result: "+errStdout.Error()+"\n", errs)
			require.NotContains(t, store.calls, "put")
		})
	}
}

// The canary seed, and every value a store holds or is handed, appears in no byte the command
// writes and in no error or line the functions return, whatever the store's state.
func TestNoSeedReachesAnOutputOrAnError(t *testing.T) {
	_, otherPublic := testnats.User(t)
	states := map[string]func(*fakeStore) *fakeStore{
		"seeded":             func(f *fakeStore) *fakeStore { return f.holding(readmeSeed) },
		"two versions":       func(f *fakeStore) *fakeStore { return f.holding(readmeSeed, readmeSeed) },
		"deleted":            func(f *fakeStore) *fakeStore { f.deleted = true; return f.holding(readmeSeed) },
		"empty":              func(f *fakeStore) *fakeStore { return f },
		"malformed":          func(f *fakeStore) *fakeStore { return f.holding(readmeSeed[:5] + "A" + readmeSeed[6:]) },
		"reads back canary":  func(f *fakeStore) *fakeStore { f.readBack = []byte(readmeSeed); return f },
		"reads back garbled": func(f *fakeStore) *fakeStore { f.readBack = []byte(readmeSeed + "\n"); return f },
		"default unserved":   func(f *fakeStore) *fakeStore { f.unserved = "DISABLED"; return f.holding(readmeSeed) },
		"unserved at read":   func(f *fakeStore) *fakeStore { f.unservedAtCurrent = "DISABLED"; return f.holding(readmeSeed) },
	}
	invocations := [][]string{
		mode("check"),
		mode("generate"),
		mode("verify", "--public-key", readmePublicKey),
		mode("verify", "--public-key", otherPublic),
		mode("verify", "--public-key", readmeSeed),
	}
	for name, state := range states {
		for _, args := range invocations {
			t.Run(name+"/"+strings.Join(args[:1], " "), func(t *testing.T) {
				store := state(newStore(t))
				code, out, errs, _ := run(t, store, args...)
				require.Contains(t, []int{0, 1, 2}, code)
				require.NotEmpty(t, out+errs, "every run reports something")
				requireNoValue(t, store, out, errs)

				ctx := context.Background()
				direct := state(newStore(t))
				var texts []string
				answered := func(text string, err error) {
					require.True(t, text != "" || err != nil, "every call answers a line or an error")
					texts = append(texts, text, fmt.Sprint(err))
				}
				answered(seed.Check(ctx, direct, secretName))
				public, ver, err := seed.Generate(ctx, direct, secretName, quiet)
				answered(public+ver, err)
				answered(seed.Verify(ctx, direct, secretName, readmePublicKey))
				answered(seed.Verify(ctx, direct, secretName, readmeSeed))
				requireNoValue(t, direct, texts...)
			})
		}
	}
}

// A usage error exits 2 with the usage text, before the store is opened, and echoes no argument a
// seed could have been pasted into.
func TestUsageErrorsExitTwoWithTheUsageBeforeOpeningTheStore(t *testing.T) {
	const notEchoed = "envoy-infra seed: a flag this command does not take, or a malformed one (not echoed: a seed may have been pasted there)\n"
	seedIn := func(flags string) string {
		return "envoy-infra seed: " + flags + ": an nkey seed, which is never an argument (not echoed)\n"
	}
	rows := []struct {
		name string
		args []string
		want string
	}{
		{"no mode", nil, "a mode is required"},
		{"an unknown mode", []string{readmeSeed, "--store", "fake-store", "--name", secretName}, "the mode is check, generate or verify"},
		{"no store", []string{"check", "--name", secretName}, "--store is required"},
		{"an unknown store", []string{"check", "--store", "elsewhere", "--name", secretName}, "--store is one of: fake-store"},
		{"no name", []string{"check", "--store", "fake-store"}, "--name is required"},
		{"verify without a public key", mode("verify"), "--public-key is required by verify"},
		{"a public key for check", mode("check", "--public-key", readmePublicKey), "--public-key is accepted by verify only"},
		{"a public key for generate", mode("generate", "--public-key", readmePublicKey), "--public-key is accepted by verify only"},
		// An empty value, which `--expect-account "$ACCOUNT"` becomes with the variable unset, is
		// not "not given": it is refused before the store opens.
		{"an empty --expect-account for check", mode("check", "--expect-account", ""), "--expect-account is given empty"},
		{"an empty --expect-account for generate", mode("generate", "--expect-account="), "--expect-account is given empty"},
		// There is no seed flag: a seed never reaches argv on purpose. A flag word the command does
		// not take is not echoed, since a seed pasted with a leading dash would be that word; a
		// defined flag missing its value is named, as only defined names can be.
		{"a seed flag", mode("generate", "--seed", readmeSeed), notEchoed},
		{"a seed flag with =", mode("generate", "--seed="+readmeSeed), notEchoed},
		{"a seed as a flag", mode("check", "-"+readmeSeed), notEchoed},
		{"a seed after three dashes", mode("check", "---"+readmeSeed), notEchoed},
		{"a flag missing its value", mode("verify", "--public-key"), "flag needs an argument: -public-key"},
		{"an argument after the flags", mode("generate", readmeSeed), "unexpected arguments after the flags"},
		// A seed pasted as any flag's value is refused naming the flag, never the value, before the
		// store opens: --name and --region would otherwise reach every line, every refusal and the
		// store's requests, and no store's refusal of the others is relied on.
		{"a seed as --name", []string{"check", "--store", "fake-store", "--name", readmeSeed}, seedIn("--name")},
		{"a seed as --name=", []string{"check", "--store", "fake-store", "--name=" + readmeSeed}, seedIn("--name")},
		{"a seed as --region", mode("check", "--region", readmeSeed), seedIn("--region")},
		{"a seed as --expect-account", mode("generate", "--expect-account", readmeSeed), seedIn("--expect-account")},
		{"a seed as --public-key", mode("verify", "--public-key", readmeSeed), seedIn("--public-key")},
		{"a seed as --store", []string{"check", "--store", readmeSeed, "--name", secretName}, seedIn("--store")},
		{"a seed in two flags", []string{"check", "--store", "fake-store", "--name", readmeSeed, "--region", readmeSeed}, seedIn("--name, --region")},
		// Inside a value too: a secret id or a region holding a seed reaches the same lines and
		// requests as one that is a seed.
		{"a seed inside --name", []string{"check", "--store", "fake-store", "--name", "example/" + readmeSeed}, seedIn("--name")},
		{"a seed inside --region", mode("check", "--region", "us-east-1/"+readmeSeed), seedIn("--region")},
		{"a seed inside --expect-account, percent-encoded", mode("check", "--expect-account", "%"+fmt.Sprintf("%X", readmeSeed[0])+readmeSeed[1:]),
			seedIn("--expect-account")},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			store := newStore(t)
			code, out, errs, o := run(t, store, row.args...)
			require.Equal(t, 2, code, errs)
			require.Empty(t, out)
			require.Contains(t, errs, row.want)
			require.Contains(t, errs, "usage: envoy-infra seed {check|generate|verify}")
			require.NotContains(t, errs, readmeSeed)
			require.NotContains(t, errs, "seed=")
			require.Zero(t, o.calls)
			require.Empty(t, store.calls)
		})
	}
}

// The usage takes each store's help from its StoreType, and names no store the caller did not hand
// the command.
func TestHelpExitsZero(t *testing.T) {
	code, out, errs, o := run(t, newStore(t), "--help")
	require.Equal(t, 0, code)
	require.Empty(t, out)
	require.Contains(t, errs, "usage: envoy-infra seed {check|generate|verify}")
	require.Contains(t, errs, "Stores:\n  fake-store\n    \tthe fake store's own help\n")
	require.NotContains(t, errs, seed.AWSSecretsManager)
	require.NotContains(t, errs, seed.GCPSecretManager)
	require.Zero(t, o.calls)
}

// A store type's CheckName refuses a --name before the store is opened, exit 2.
func TestAStoreTypesNameRuleRefusesBeforeTheOpen(t *testing.T) {
	opened := false
	stores := seed.Stores{"fake-store": {
		Open:      func(context.Context, string, string) (seed.Store, error) { opened = true; return newStore(t), nil },
		CheckName: func(string) error { return errors.New("--name for --store fake-store is not of its form") },
	}}
	var out, errs bytes.Buffer
	code := seed.Command(context.Background(), mode("check"), &out, &errs, stores)
	require.Equal(t, 2, code)
	require.Empty(t, out.String())
	require.Equal(t, "refused: --name for --store fake-store is not of its form\n", errs.String())
	require.False(t, opened)
}
