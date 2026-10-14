package seed_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

type version struct {
	id    string
	value []byte
}

// fakeStore holds the one secret secretName in a chosen state and records every call.
type fakeStore struct {
	t       *testing.T
	exists  bool
	deleted bool
	// versions in the order they were written; current is the one the store serves by default.
	versions []version
	current  string
	calls    []string
	puts     [][]byte
	// newest makes the store's default the newest version whatever its state, as GCP's
	// versions/latest is; unreadable is then that version's state when it is not served, and
	// unreadableAtCurrent the state it enters just before Current reads it.
	newest              bool
	unreadable          string
	unreadableAtCurrent string

	describeErr             error    // every Describe fails
	describeErrAfterCurrent error    // a Describe once Current was called fails
	versionsErr             error    // every Versions fails
	versionsErrAfterPut     error    // a Versions once Put has written fails
	putErr                  error    // Put fails and writes nothing
	putErrAfterWrite        error    // Put writes, then answers its version with this error
	currentErr              error    // Current fails
	readBack                []byte   // what Current answers instead of the stored value
	racing                  *version // another writer's version, which lands with Put's
	lateWriter              *version // another writer's version, which lands just before Current reads and is served
}

func newStore(t *testing.T) *fakeStore { return &fakeStore{t: t, exists: true} }

// latest gives f the default GCP's Secret Manager has: the newest version, labelled latest.
func (f *fakeStore) latest() *fakeStore {
	f.newest = true
	return f
}

func (f *fakeStore) Label() string {
	if f.newest {
		return "latest"
	}
	return "AWSCURRENT"
}

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
	if f.describeErrAfterCurrent != nil && slices.Contains(f.calls, "current") {
		return seed.Description{}, f.describeErrAfterCurrent
	}
	switch {
	case !f.exists:
		return seed.Description{}, nil
	case f.deleted:
		return seed.Description{Exists: true, ScheduledForDeletion: true, DeletionDate: "2026-09-30 00:00:00+00:00"}, nil
	case f.newest:
		d := seed.Description{Exists: true, DefaultIsNewest: true}
		if len(f.versions) > 0 {
			d.NewestVersion, d.NewestState = f.versions[len(f.versions)-1].id, "ENABLED"
			if f.unreadable != "" {
				d.NewestState = f.unreadable
			} else {
				d.CurrentVersion = d.NewestVersion
			}
		}
		return d, nil
	}
	return seed.Description{Exists: true, CurrentVersion: f.current}, nil
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
	return len(f.versions), nil
}

func (f *fakeStore) Put(_ context.Context, name string, value []byte) (string, error) {
	f.calls = append(f.calls, "put")
	f.named(name)
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
	if f.unreadableAtCurrent != "" {
		f.unreadable, f.unreadableAtCurrent = f.unreadableAtCurrent, ""
	}
	if f.newest && f.unreadable != "" {
		return "", nil, errors.New("access-secret-version " + secretName + "/versions/latest failed: FailedPrecondition")
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
	stores := seed.Stores{"fake-store": func(_ context.Context, region, account string) (seed.Store, error) {
		o.calls++
		o.region, o.account = region, account
		return store, nil
	}}
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
		{"generate: one version", mode("generate"), func(f *fakeStore) *fakeStore { return f.holding(readmeSeed) }, "already holds 1 version(s)"},
		// A version without the default label is still a seed someone may have read.
		{"generate: two versions", mode("generate"), func(f *fakeStore) *fakeStore { return f.holding(readmeSeed, readmeSeed) }, "already holds 2 version(s)"},
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
		{"verify: no latest version", mode("verify", "--public-key", readmePublicKey), func(f *fakeStore) *fakeStore { return f.latest() },
			"has no latest version; nothing to verify"},
		{"verify: describe fails", mode("verify", "--public-key", otherPublic), func(f *fakeStore) *fakeStore { f.describeErr = denied; return f.holding(readmeSeed) },
			"AccessDeniedException"},
		{"verify: reading fails", mode("verify", "--public-key", otherPublic), func(f *fakeStore) *fakeStore {
			f.currentErr = errors.New("get-secret-value " + secretName + " failed: DecryptionFailure")
			return f.holding(readmeSeed)
		}, "DecryptionFailure"},
		{"verify: a malformed public key", mode("verify", "--public-key", "U-not-a-key"), func(f *fakeStore) *fakeStore { return f.holding(readmeSeed) },
			"--public-key is not an nkey user public key"},
		// A seed pasted where the public key belongs is refused without being echoed.
		{"verify: a seed as the public key", mode("verify", "--public-key", readmeSeed), func(f *fakeStore) *fakeStore { return f.holding(readmeSeed) },
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
	stores := seed.Stores{"fake-store": func(context.Context, string, string) (seed.Store, error) {
		return nil, fmt.Errorf("%w: these credentials are for account 111111111111, but --expect-account is 000000000000", seed.ErrRefused)
	}}
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

// A store whose default is the newest version whatever its state (GCP's versions/latest) gets
// check's latest lines: no versions, the newest version, or the newest version and the state it is
// in when the store does not serve it.
func TestCheckPrintsTheLatestLines(t *testing.T) {
	rows := []struct {
		name  string
		store func(*fakeStore) *fakeStore
		want  string
	}{
		{"absent", func(f *fakeStore) *fakeStore { f.exists = false; return f.latest() }, secretName + ": absent"},
		{"no versions", func(f *fakeStore) *fakeStore { return f.latest() }, secretName + ": present, no versions"},
		{"enabled", func(f *fakeStore) *fakeStore { return f.latest().holding(readmeSeed, readmeSeed) },
			secretName + ": present; latest: version v2"},
		{"disabled", func(f *fakeStore) *fakeStore {
			f.unreadable = "DISABLED"
			return f.latest().holding(readmeSeed, readmeSeed)
		},
			secretName + ": present; latest: version v2 is DISABLED"},
		{"destroyed", func(f *fakeStore) *fakeStore { f.unreadable = "DESTROYED"; return f.latest().holding(readmeSeed) },
			secretName + ": present; latest: version v1 is DESTROYED"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			store := row.store(newStore(t))
			code, out, errs, _ := run(t, store, mode("check")...)
			require.Equal(t, 0, code, errs)
			require.Equal(t, row.want+"\n", out)
			require.Empty(t, errs)
			require.Equal(t, []string{"describe"}, store.calls)
			requireNoValue(t, store, out, errs)

			line, err := seed.Check(context.Background(), row.store(newStore(t)), secretName)
			require.NoError(t, err)
			require.Equal(t, row.want, line)
		})
	}
}

func TestVerifyOfTheLatestVersion(t *testing.T) {
	_, otherPublic := testnats.User(t)
	for _, row := range []struct {
		key, want string
		code      int
	}{
		{readmePublicKey, secretName + " latest (version v2) derives " + readmePublicKey + ": yes", 0},
		{otherPublic, secretName + " latest (version v2) derives " + otherPublic + ": no", 1},
	} {
		store := newStore(t).latest().holding(readmeSeed, readmeSeed)
		code, out, errs, _ := run(t, store, mode("verify", "--public-key", row.key)...)
		require.Equal(t, row.code, code, errs)
		require.Equal(t, row.want+"\n", out)
		require.Empty(t, errs)
		require.Equal(t, []string{"describe", "current"}, store.calls)
		requireNoValue(t, store, out, errs)
	}
}

// A latest version the store does not serve fails verify with check's line for it, exit 1: a failed
// check, not a refusal, though nothing was written. When Describe already reports it, verify reads no
// value; when it stops being served between Describe and Current, Current fails and verify describes
// the secret again to tell that from a refused read.
func TestVerifyOfAnUnreadableLatestVersionFailsWithItsState(t *testing.T) {
	for _, state := range []string{"DISABLED", "DESTROYED"} {
		for _, when := range []struct {
			name  string
			set   func(*fakeStore)
			calls []string
		}{
			{"before verify", func(f *fakeStore) { f.unreadable = state }, []string{"describe"}},
			{"between describe and current", func(f *fakeStore) { f.unreadableAtCurrent = state },
				[]string{"describe", "current", "describe"}},
		} {
			t.Run(state+"/"+when.name, func(t *testing.T) {
				want := secretName + ": present; latest: version v2 is " + state
				unreadable := func() *fakeStore {
					f := newStore(t).latest().holding(readmeSeed, readmeSeed)
					when.set(f)
					return f
				}
				store := unreadable()
				code, out, errs, _ := run(t, store, mode("verify", "--public-key", readmePublicKey)...)
				require.Equal(t, 1, code, errs)
				require.Equal(t, want+"\n", out)
				require.Empty(t, errs)
				require.Equal(t, when.calls, store.calls)
				requireNoValue(t, store, out, errs)

				line, err := seed.Verify(context.Background(), unreadable(), secretName, readmePublicKey)
				require.ErrorIs(t, err, seed.ErrMismatch)
				require.NotErrorIs(t, err, seed.ErrRefused)
				require.Equal(t, want, line)
			})
		}
	}
}

// When Current fails for a store whose default is the newest version and describing the secret again
// shows that version still served, or fails, the read's own failure is the refusal, exit 2.
func TestVerifyRefusesAFailedReadOfAServedLatestVersion(t *testing.T) {
	readFails := errors.New("access-secret-version " + secretName + "/versions/latest failed: PermissionDenied")
	for _, row := range []struct {
		name  string
		store func(*fakeStore) *fakeStore
	}{
		{"still served", func(f *fakeStore) *fakeStore { f.currentErr = readFails; return f }},
		{"describing again fails", func(f *fakeStore) *fakeStore {
			f.unreadableAtCurrent = "DISABLED"
			f.describeErrAfterCurrent = errors.New("get-secret " + secretName + " failed: Unavailable")
			return f
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			store := row.store(newStore(t).latest().holding(readmeSeed))
			code, out, errs, _ := run(t, store, mode("verify", "--public-key", readmePublicKey)...)
			require.Equal(t, 2, code, errs)
			require.Empty(t, out)
			require.Equal(t, []string{"describe", "current", "describe"}, store.calls)
			require.True(t, strings.HasPrefix(errs, "refused: access-secret-version "+secretName+"/versions/latest failed: "), errs)
			requireNoValue(t, store, out, errs)
		})
	}
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

// After the write, generate exits 1, never 2, and its stdout already holds the public key and the
// version id when Put answered one.
func TestGenerateReportsEveryFailureAfterItsWrite(t *testing.T) {
	otherSeed, _ := testnats.User(t)
	rows := []struct {
		name        string
		store       func(*fakeStore) *fakeStore
		want        string
		printsTheV1 bool
	}{
		{"put fails", func(f *fakeStore) *fakeStore {
			f.putErr = errors.New("put-secret-value " + secretName + " failed: RequestSendError")
			return f
		}, "whether a version was written is unknown, run check", false},
		{"put writes a version that is not served", func(f *fakeStore) *fakeStore {
			f.putErrAfterWrite = errors.New("version v1 was written but is not AWSCURRENT; run check")
			return f
		}, "version v1 was written but is not AWSCURRENT; run check", true},
		{"listing afterwards fails", func(f *fakeStore) *fakeStore {
			f.versionsErrAfterPut = errors.New("list-secret-version-ids " + secretName + " failed: ThrottlingException")
			return f
		}, "version v1 was written, but listing versions afterwards failed: list-secret-version-ids " + secretName +
			" failed: ThrottlingException", true},
		{"another writer raced it", func(f *fakeStore) *fakeStore {
			f.racing = &version{id: "their-version", value: []byte(otherSeed)}
			return f
		}, secretName + " now holds 2 version(s), not only version v1: another writer raced this one. " +
			"Run verify with the public key above before committing it", true},
		{"another writer's version is served by the read-back", func(f *fakeStore) *fakeStore {
			f.lateWriter = &version{id: "their-version", value: []byte(otherSeed)}
			return f
		}, "version v1 was written, but " + secretName + " serves version their-version by default: another writer raced this one", true},
		{"reading back fails", func(f *fakeStore) *fakeStore {
			f.currentErr = errors.New("get-secret-value " + secretName + " failed: DecryptionFailure")
			return f
		}, "version v1 was written, but reading it back failed: get-secret-value " + secretName + " failed: DecryptionFailure", true},
		{"it reads back as another seed", func(f *fakeStore) *fakeStore { f.readBack = []byte(readmeSeed); return f },
			"version v1 was written, but it does not derive the public key above", true},
		{"it reads back as no seed", func(f *fakeStore) *fakeStore { f.readBack = []byte(readmeSeed[:5] + "A" + readmeSeed[6:]); return f },
			"version v1 was written, but it reads back as no user seed: the value's checksum does not match", true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			store := row.store(newStore(t))
			code, out, errs, _ := run(t, store, mode("generate")...)
			require.Equal(t, 1, code, errs)
			require.True(t, strings.HasPrefix(errs, "error after writing: "), errs)
			require.Contains(t, errs, row.want)
			require.NotContains(t, errs, "refused")
			lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
			if row.printsTheV1 {
				kp, err := nkeys.FromSeed(store.puts[0])
				require.NoError(t, err)
				public, err := kp.PublicKey()
				require.NoError(t, err)
				require.Equal(t, []string{"public key: " + public, "version id: v1"}, lines)
			} else {
				require.Empty(t, out)
			}
			requireNoValue(t, store, out, errs)
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
		"latest":             func(f *fakeStore) *fakeStore { return f.latest().holding(readmeSeed) },
		"latest disabled":    func(f *fakeStore) *fakeStore { f.unreadable = "DISABLED"; return f.latest().holding(readmeSeed) },
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
				public, ver, err := seed.Generate(ctx, direct, secretName)
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
		// There is no seed flag: a seed never reaches argv on purpose.
		{"a seed flag", mode("generate", "--seed", readmeSeed), "flag provided but not defined: -seed"},
		{"a seed flag with =", mode("generate", "--seed="+readmeSeed), "flag provided but not defined: -seed"},
		{"an argument after the flags", mode("generate", readmeSeed), "unexpected arguments after the flags"},
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
			require.Zero(t, o.calls)
			require.Empty(t, store.calls)
		})
	}
}

func TestHelpExitsZero(t *testing.T) {
	code, out, errs, o := run(t, newStore(t), "--help")
	require.Equal(t, 0, code)
	require.Empty(t, out)
	require.Contains(t, errs, "usage: envoy-infra seed {check|generate|verify}")
	require.Zero(t, o.calls)
}
