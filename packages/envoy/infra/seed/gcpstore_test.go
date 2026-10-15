package seed

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/googleapis/gax-go/v2/apierror"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	"google.golang.org/api/option/internaloption"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/testnats"
)

func TestGCPCheck(t *testing.T) {
	// The date is printed as the AWS store prints DeletedDate, whose format TestAWSCheck pins;
	// time.Local is left alone, since the gRPC client's goroutines read it.
	expires := time.Unix(1759190400, 500_000_000)
	seedValue, _ := testnats.User(t)
	metadataOnly := []string{"GetSecret", "GetSecretVersion"}
	rows := []struct {
		name  string
		fake  func(*fakeGCP) *fakeGCP
		want  string
		calls []string
	}{
		{"absent", func(f *fakeGCP) *fakeGCP { f.exists = false; return f }, gcpSecret + ": absent", []string{"GetSecret"}},
		// Secret Manager deletes a secret, with every version, when its expire_time comes.
		{"set to expire", func(f *fakeGCP) *fakeGCP { f.expireAt = &expires; return f.with(seedValue, enabled) },
			gcpSecret + ": present, scheduled for deletion (" + pythonDate(expires, time.Local) + ")", []string{"GetSecret"}},
		{"no versions", func(f *fakeGCP) *fakeGCP { return f }, gcpSecret + ": present; latest: no", metadataOnly},
		{"enabled", func(f *fakeGCP) *fakeGCP { return f.with(seedValue, disabled).with(seedValue, enabled) },
			gcpSecret + ": present; latest: yes (version 2)", metadataOnly},
		// versions/latest names the newest version whatever its state, so an enabled older version
		// does not make the secret's default readable.
		{"disabled", func(f *fakeGCP) *fakeGCP { return f.with(seedValue, enabled).with(seedValue, disabled) },
			gcpSecret + ": present; latest: version 2 is DISABLED", metadataOnly},
		{"destroyed", func(f *fakeGCP) *fakeGCP { return f.with(seedValue, enabled).with("", destroyed) },
			gcpSecret + ": present; latest: version 2 is DESTROYED", metadataOnly},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := row.fake(newFakeGCP(t))
			serveGCP(t, f)
			code, out, errs := runGCP(t, f, "check")
			require.Equal(t, 0, code, errs)
			require.Equal(t, row.want+"\n", out)
			require.Empty(t, errs)
			require.Equal(t, row.calls, f.recorded())
		})
	}
}

func TestGCPGenerateWritesTheBareSeedAndReadsItBack(t *testing.T) {
	f := newFakeGCP(t)
	serveGCP(t, f)
	code, out, errs := runGCP(t, f, "generate")
	require.Equal(t, 0, code, errs)
	require.Empty(t, errs)
	require.Equal(t, []string{"GetSecret", "GetSecretVersion", "ListSecretVersions", "AccessSecretVersion 1", "AccessSecretVersion 1",
		"AddSecretVersion", "AccessSecretVersion 1", "ListSecretVersions", "AccessSecretVersion"}, f.recorded(),
		"version 1 is read by number in the checks and by Put just before the write, to find none, and after it, to read the write back")
	require.Len(t, f.versions, 1)
	written := f.versions[0]
	require.True(t, strings.HasPrefix(written.value, "SU"), "the payload is the bare seed string")
	require.Equal(t, enabled, written.state)
	require.Equal(t, "public key: "+publicKeyOfSeed(t, written.value)+"\nversion id: 1\n", out)

	again := newFakeGCP(t)
	serveGCP(t, again)
	code, _, errs = runGCP(t, again, "generate")
	require.Equal(t, 0, code, errs)
	require.NotEqual(t, written.value, again.versions[0].value, "each generate makes a user of its own")
}

// The Secret Manager store keeps every Store's contract against the fake. AddSecretVersion has no
// request id or precondition, so Put reads version 1 by number, strongly consistent with an add,
// and refuses when it exists: a second first version is refused, and nothing is written.
func TestGCPStoreKeepsTheStoreContract(t *testing.T) {
	testStoreContract(t, func(t *testing.T) (Store, string) { return gcpStoreOn(t, newFakeGCP(t)), gcpSecret })
}

// A failed read of version 1 just before the add wrote nothing, so Put refuses: ErrRefused, no
// version id, no AddSecretVersion.
func TestGCPPutRefusesWhenItCannotReadVersionOne(t *testing.T) {
	f := newFakeGCP(t)
	f.failures = map[string]codes.Code{"AccessSecretVersion 1": codes.PermissionDenied}
	s := gcpStoreOn(t, f)
	version, err := s.Put(context.Background(), gcpSecret, []byte("example-first-value"))
	require.ErrorIs(t, err, ErrRefused)
	require.Empty(t, version)
	require.EqualError(t, err, "access-secret-version "+gcpSecret+"/versions/1 failed: PermissionDenied; nothing was written")
	require.Equal(t, []string{"AccessSecretVersion 1"}, f.recorded())
	require.Empty(t, f.versions)
}

// Two generate runs overlap on one secret: A's first version lands after B's checks found the
// secret empty, just before B writes. B's Put finds version 1 by number and refuses: exit 2, no
// version id line, nothing written, and A's seed stays the one served.
func TestGCPOverlappingGenerateRunsLeaveTheFirstSeedServed(t *testing.T) {
	seedA, keyA := testnats.User(t)
	f := newFakeGCP(t)
	f.landing = &gcpVersion{value: seedA, state: enabled}
	f.landsAfter = "AccessSecretVersion 1" // B's last check before its write
	serveGCP(t, f)
	code, out, errs := runGCP(t, f, "generate")
	require.Equal(t, 2, code, errs)
	require.True(t, strings.HasPrefix(out, "public key: "), out)
	require.NotContains(t, out, "version id")
	require.Equal(t, "refused: add-secret-version "+gcpSecret+": "+gcpSecret+"/versions/1 already exists; another generate "+
		"wrote this secret's first version, with another seed; nothing was written, and the public key above is not stored\n", errs)
	require.Equal(t, []string{"GetSecret", "GetSecretVersion", "ListSecretVersions", "AccessSecretVersion 1", "AccessSecretVersion 1"},
		f.recorded(), "B's Put reads version 1 and sends no AddSecretVersion")
	require.Len(t, f.versions, 1, "B wrote nothing")

	code, out, errs = runGCP(t, f, "verify", "--public-key", keyA)
	require.Equal(t, 0, code, errs)
	require.True(t, strings.HasSuffix(out, " derives "+keyA+": yes\n"), out)
}

func TestGCPGenerateRefusesBeforeWriting(t *testing.T) {
	seedValue, _ := testnats.User(t)
	rows := []struct {
		name  string
		fake  func(*fakeGCP) *fakeGCP
		want  string
		lists int
	}{
		// Describe's read of latest names the newest version whatever its state: generate refuses
		// on it before it lists, so neither eventually consistent read alone lets a write through.
		// A destroyed version holds no value, but a client may have read the seed it held.
		{"a destroyed version", func(f *fakeGCP) *fakeGCP { return f.with("", destroyed) },
			"already holds version 1, which latest names though it is DESTROYED", 0},
		{"a disabled version", func(f *fakeGCP) *fakeGCP { return f.with(seedValue, disabled) },
			"already holds version 1, which latest names though it is DISABLED", 0},
		{"a version latest names and the listing leaves out", func(f *fakeGCP) *fakeGCP {
			f.unlisted = 1
			return f.with(seedValue, enabled)
		}, "already serves version 1 by default (latest)", 0},
		// The listing counts what latest does not name yet, destroyed versions included, across
		// every page.
		{"a destroyed version only the listing shows", func(f *fakeGCP) *fakeGCP {
			f.unnamed = 1
			return f.with("", destroyed)
		}, "already holds 1 version(s)", 1},
		// latest and the listing are eventually consistent, and can both lag behind a version the
		// secret holds; reading version 1 by number cannot, and finds it in any state.
		{"a version neither latest nor the listing shows", func(f *fakeGCP) *fakeGCP {
			f.unlisted, f.unnamed = 1, 1
			return f.with(seedValue, enabled)
		}, "already holds 1 version(s)", 1},
		{"a disabled version neither latest nor the listing shows", func(f *fakeGCP) *fakeGCP {
			f.unlisted, f.unnamed = 1, 1
			return f.with(seedValue, disabled)
		}, "already holds 1 version(s)", 1},
		{"a destroyed version neither latest nor the listing shows", func(f *fakeGCP) *fakeGCP {
			f.unlisted, f.unnamed = 1, 1
			return f.with("", destroyed)
		}, "already holds 1 version(s)", 1},
		{"reading version 1 is denied", func(f *fakeGCP) *fakeGCP {
			f.failures = map[string]codes.Code{"AccessSecretVersion 1": codes.PermissionDenied}
			return f
		}, "access-secret-version " + gcpSecret + "/versions/1 failed: PermissionDenied", 1},
		{"set to expire", func(f *fakeGCP) *fakeGCP {
			expires := time.Now().Add(time.Hour)
			f.expireAt = &expires
			return f
		}, "is scheduled for deletion", 0},
		{"versions on every page, which latest does not name yet", func(f *fakeGCP) *fakeGCP {
			f.pageSize, f.unnamed = 2, 3
			return f.with("", destroyed).with(seedValue, disabled).with(seedValue, enabled)
		}, "already holds 3 version(s)", 2},
		{"a listing that repeats its page token", func(f *fakeGCP) *fakeGCP { f.repeatToken = true; return f },
			"list-secret-versions " + gcpSecret + " failed: the listing handed out page token page-2 twice", 2},
		{"absent", func(f *fakeGCP) *fakeGCP { f.exists = false; return f }, gcpSecret + " does not exist", 0},
		{"get-secret denied", func(f *fakeGCP) *fakeGCP {
			f.failures = map[string]codes.Code{"GetSecret": codes.PermissionDenied}
			return f
		}, "get-secret " + gcpSecret + " failed: PermissionDenied", 0},
		{"get-secret-version denied", func(f *fakeGCP) *fakeGCP {
			f.failures = map[string]codes.Code{"GetSecretVersion": codes.PermissionDenied}
			return f
		}, "get-secret-version " + gcpLatestOf + " failed: PermissionDenied", 0},
		{"list-secret-versions denied", func(f *fakeGCP) *fakeGCP {
			f.failures = map[string]codes.Code{"ListSecretVersions": codes.PermissionDenied}
			return f
		}, "list-secret-versions " + gcpSecret + " failed: PermissionDenied", 1},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := row.fake(newFakeGCP(t))
			serveGCP(t, f)
			code, out, errs := runGCP(t, f, "generate")
			require.Equal(t, 2, code, errs)
			require.Empty(t, out)
			require.True(t, strings.HasPrefix(errs, "refused: "), errs)
			require.Contains(t, errs, row.want)
			require.NotContains(t, f.recorded(), "AddSecretVersion")
			lists := 0
			for _, call := range f.recorded() {
				if call == "ListSecretVersions" {
					lists++
				}
			}
			require.Equal(t, row.lists, lists)
		})
	}
}

func TestGCPGenerateAfterItsWrite(t *testing.T) {
	theirs, _ := testnats.User(t)
	rows := []struct {
		name string
		fake func(*fakeGCP) *fakeGCP
		want string
	}{
		{"the written version is not enabled", func(f *fakeGCP) *fakeGCP { f.addState = disabled; return f },
			"error after writing: version 1 was written but is DISABLED; run check"},
		{"another writer raced it", func(f *fakeGCP) *fakeGCP { f.racing = &gcpVersion{value: theirs, state: enabled}; return f },
			"error after writing: " + gcpSecret + " now holds 2 version(s), not only version 1: another writer raced this one"},
		{"listing afterwards fails", func(f *fakeGCP) *fakeGCP {
			f.failAfterAdd = map[string]codes.Code{"ListSecretVersions": codes.Unavailable}
			return f
		}, "error after writing: version 1 was written, but listing versions afterwards failed: list-secret-versions " + gcpSecret +
			" failed: Unavailable"},
		{"another writer's version is served by the read-back", func(f *fakeGCP) *fakeGCP {
			f.lateWriter = &gcpVersion{value: theirs, state: enabled}
			return f
		}, "error after writing: version 1 was written, but " + gcpSecret + " serves version 2 by default: another writer raced this one"},
		// An access by number is strongly consistent with the add, so the write's own read-back
		// reads version 1 that way, before latest and the listing are asked about another writer.
		{"reading version 1 back fails", func(f *fakeGCP) *fakeGCP {
			f.failAfterAdd = map[string]codes.Code{"AccessSecretVersion 1": codes.PermissionDenied}
			return f
		}, "error after writing: version 1 was written, but reading it back by number failed: access-secret-version " + gcpSecret +
			"/versions/1 failed: PermissionDenied; run check"},
		{"version 1 reads back as another value", func(f *fakeGCP) *fakeGCP { f.numberReadsBack = theirs; return f },
			"error after writing: version 1 was written, but " + gcpSecret + "/versions/1 reads back another value; run check"},
		{"reading back fails", func(f *fakeGCP) *fakeGCP {
			f.failAfterAdd = map[string]codes.Code{"AccessSecretVersion": codes.PermissionDenied}
			return f
		}, "error after writing: version 1 was written, but reading it back failed: access-secret-version " + gcpLatestOf +
			" failed: PermissionDenied"},
		// latest names a version it will not serve: a failure, not a store still catching up.
		{"another writer's disabled version is latest at the read-back", func(f *fakeGCP) *fakeGCP {
			f.lateWriter = &gcpVersion{value: theirs, state: disabled}
			return f
		}, "error after writing: version 1 was written, but reading it back failed: access-secret-version " + gcpLatestOf +
			" failed: FailedPrecondition"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := row.fake(newFakeGCP(t))
			serveGCP(t, f)
			code, out, errs := runGCP(t, f, "generate")
			require.Equal(t, 1, code, errs)
			require.True(t, strings.HasPrefix(errs, row.want), errs)
			require.Equal(t, 1, f.added)
			mine := f.versions[0]
			require.Equal(t, "public key: "+publicKeyOfSeed(t, mine.value)+"\nversion id: 1\n", out)
		})
	}
}

// AddSecretVersion carries no request id and the library does not retry it, so a write whose answer
// is lost is reported as unknown, not refused, after the public key generate printed before it.
func TestGCPGenerateCallsAFailedWriteUnknownNotRefused(t *testing.T) {
	f := newFakeGCP(t)
	f.failures = map[string]codes.Code{"AddSecretVersion": codes.Unavailable}
	serveGCP(t, f)
	code, out, errs := runGCP(t, f, "generate")
	require.Equal(t, 1, code, errs)
	key, rest, _ := strings.Cut(out, "\n")
	require.True(t, strings.HasPrefix(key, "public key: ") && core.IsUserPublicKey(strings.TrimPrefix(key, "public key: ")),
		"stdout %q does not start with the public key", out)
	require.Equal(t, "version id: unknown\n", rest)
	require.Equal(t, "error after writing: add-secret-version "+gcpSecret+" failed: Unavailable; "+
		"whether a version was written is unknown, run check\n", errs)
	require.Equal(t, []string{"GetSecret", "GetSecretVersion", "ListSecretVersions", "AccessSecretVersion 1", "AccessSecretVersion 1",
		"AddSecretVersion"}, f.recorded(),
		"one AddSecretVersion, not retried, and no call after it")
}

// Two generate runs overlap on one secret so that both read version 1, and find none, before either
// adds: Secret Manager has no conditional add, so both adds land, A's as version 1 and B's as
// version 2, which latest names. B's error after the write says, in order, what happened, what it
// means for A's key, and the gcloud and envoy-infra commands that recover; nothing is read back.
// verify then shows the consequence it states: the served seed derives B's key, not A's.
func TestGCPOverlappingGenerateRunsWhoseReadsBothPrecedeBothAdds(t *testing.T) {
	seedA, keyA := testnats.User(t)
	f := newFakeGCP(t)
	f.earlyWriter = &gcpVersion{value: seedA, state: enabled} // A's add lands between B's read and B's add
	serveGCP(t, f)
	code, out, errs := runGCP(t, f, "generate")
	require.Equal(t, 1, code, errs)
	require.Equal(t, 1, f.added)
	keyB := publicKeyOfSeed(t, f.versions[1].value)
	require.Equal(t, "public key: "+keyB+"\nversion id: 2\n", out)

	what := "version 2 was written, not version 1: another generate wrote version 1 of " + gcpSecret + " between this " +
		"run's read of version 1 and its write, so this run's version 2 is now the newest, the one latest serves.\n"
	consequence := "The public key the other run printed does not derive the served seed, so a client given it is " +
		"refused; the public key above does.\n"
	recovery := "To keep this run's seed, put the public key above in the NATS authorization in place of the other " +
		"run's, then destroy the other run's version:\n" +
		"  gcloud secrets versions destroy " + gcpSecret + "/versions/1\n" +
		"To keep the other run's seed instead, copy version 1 to a new version, then destroy this run's (destroying " +
		"or disabling version 2 alone leaves latest naming it, and no seed is served):\n" +
		"  gcloud secrets versions access " + gcpSecret + "/versions/1 --format='get(payload.data)' | tr '_-' '/+' | " +
		"base64 -d | gcloud secrets versions add " + gcpSecret + " --data-file=-\n" +
		"  gcloud secrets versions destroy " + gcpSecret + "/versions/2\n" +
		"Then check the secret, and verify the key you kept:\n" +
		"  envoy-infra seed check --store gcp-secret-manager --name " + gcpSecret + "\n" +
		"  envoy-infra seed verify --store gcp-secret-manager --name " + gcpSecret + " --public-key <the key you kept>\n"
	require.Equal(t, "error after writing: "+what+consequence+recovery, errs, "what happened, its consequence, the recovery")
	require.Equal(t, []string{"GetSecret", "GetSecretVersion", "ListSecretVersions", "AccessSecretVersion 1", "AccessSecretVersion 1",
		"AddSecretVersion"}, f.recorded(), "no read-back after the write")

	code, out, errs = runGCP(t, f, "verify", "--public-key", keyA)
	require.Equal(t, 1, code, errs)
	require.True(t, strings.HasSuffix(out, " derives "+keyA+": no\n"), out)
	code, out, errs = runGCP(t, f, "verify", "--public-key", keyB)
	require.Equal(t, 0, code, errs)
	require.True(t, strings.HasSuffix(out, " derives "+keyB+": yes\n"), out)
}

// Secret Manager's listings and its latest alias are eventually consistent: only an access by
// version number is strongly consistent with the add
// (https://cloud.google.com/secret-manager/docs/access-secret-version, "Resource consistency"). So
// generate looks again while neither shows its write yet, and a write the store never shows is an
// error after the write.
func TestGCPGenerateWaitsForTheStoreToShowItsWrite(t *testing.T) {
	SetReadBackWaits(t, time.Millisecond, time.Millisecond, time.Millisecond)
	before := []string{"GetSecret", "GetSecretVersion", "ListSecretVersions", "AccessSecretVersion 1", "AccessSecretVersion 1",
		"AddSecretVersion", "AccessSecretVersion 1"}
	t.Run("caught up", func(t *testing.T) {
		f := newFakeGCP(t)
		f.lagList, f.lagAccess = 1, 2
		serveGCP(t, f)
		code, out, errs := runGCP(t, f, "generate")
		require.Equal(t, 0, code, errs)
		require.Empty(t, errs)
		require.Equal(t, "public key: "+publicKeyOfSeed(t, f.versions[0].value)+"\nversion id: 1\n", out)
		require.Equal(t, slices.Concat(before, []string{
			"ListSecretVersions", "AccessSecretVersion 1", "AccessSecretVersion", // the listing leaves the write out, latest is not found yet
			"ListSecretVersions", "AccessSecretVersion", // nor now
			"ListSecretVersions", "AccessSecretVersion",
		}), f.recorded())
	})
	t.Run("never caught up", func(t *testing.T) {
		f := newFakeGCP(t)
		f.lagAccess = 4
		serveGCP(t, f)
		code, out, errs := runGCP(t, f, "generate")
		require.Equal(t, 1, code, errs)
		require.Equal(t, "public key: "+publicKeyOfSeed(t, f.versions[0].value)+"\nversion id: 1\n", out)
		require.Equal(t, "error after writing: version 1 was written, but "+gcpSecret+" did not show it within 3ms: "+
			"the store has not caught up. Run check, then verify with the public key above\n", errs)
	})
}

// A status message can quote the request, and a write's request carries the seed: an error is
// rendered by its code alone.
func TestGCPAnErrorQuotingTheRequestNeverPrintsIt(t *testing.T) {
	f := newFakeGCP(t)
	f.echoRequests = true
	f.failures = map[string]codes.Code{"AddSecretVersion": codes.InvalidArgument}
	serveGCP(t, f)
	code, _, errs := runGCP(t, f, "generate")
	require.Equal(t, 1, code)
	require.Equal(t, "error after writing: add-secret-version "+gcpSecret+" failed: InvalidArgument; "+
		"whether a version was written is unknown, run check\n", errs)
}

func TestGCPVerify(t *testing.T) {
	seedValue, public := testnats.User(t)
	_, another := testnats.User(t)
	t.Run("yes", func(t *testing.T) {
		f := newFakeGCP(t).with(seedValue, enabled)
		serveGCP(t, f)
		code, out, errs := runGCP(t, f, "verify", "--public-key", public)
		require.Equal(t, 0, code, errs)
		require.Equal(t, gcpSecret+" latest (version 1) derives "+public+": yes\n", out)
		require.Empty(t, errs)
		require.Equal(t, []string{"GetSecret", "GetSecretVersion", "AccessSecretVersion"}, f.recorded())
	})
	t.Run("no", func(t *testing.T) {
		f := newFakeGCP(t).with(seedValue, disabled).with(seedValue, enabled)
		serveGCP(t, f)
		code, out, errs := runGCP(t, f, "verify", "--public-key", another)
		require.Equal(t, 1, code, errs)
		require.Equal(t, gcpSecret+" latest (version 2) derives "+another+": no\n", out)
		require.Empty(t, errs)
	})
	for _, state := range []secretmanagerpb.SecretVersion_State{disabled, destroyed} {
		t.Run("a "+strings.ToLower(state.String())+" latest version", func(t *testing.T) {
			f := newFakeGCP(t).with(seedValue, enabled).with(seedValue, state)
			serveGCP(t, f)
			code, out, errs := runGCP(t, f, "verify", "--public-key", public)
			require.Equal(t, 1, code, errs)
			require.Equal(t, gcpSecret+": present; latest: version 2 is "+state.String()+"\n", out)
			require.Empty(t, errs)
			require.Equal(t, []string{"GetSecret", "GetSecretVersion"}, f.recorded(), "verify reads no value of a version Secret Manager does not serve")
		})
	}
	// The newest version can stop being served after GetSecretVersion reported it: here a disabled
	// version lands just before AccessSecretVersion reads, which Secret Manager then refuses, and the
	// store names that version with one more GetSecretVersion.
	t.Run("a latest version disabled between describe and access", func(t *testing.T) {
		f := newFakeGCP(t).with(seedValue, enabled)
		f.lateWriter = &gcpVersion{value: seedValue, state: disabled}
		serveGCP(t, f)
		code, out, errs := runGCP(t, f, "verify", "--public-key", public)
		require.Equal(t, 1, code, errs)
		require.Equal(t, gcpSecret+": present; latest: version 2 is DISABLED\n", out)
		require.Empty(t, errs)
		require.Equal(t, []string{"GetSecret", "GetSecretVersion", "AccessSecretVersion", "GetSecretVersion"}, f.recorded())
	})
	t.Run("no versions", func(t *testing.T) {
		f := newFakeGCP(t)
		serveGCP(t, f)
		code, out, errs := runGCP(t, f, "verify", "--public-key", public)
		require.Equal(t, 2, code, errs)
		require.Empty(t, out)
		require.Equal(t, "refused: "+gcpSecret+" has no latest version; nothing to verify\n", errs)
	})
	t.Run("reading is denied", func(t *testing.T) {
		f := newFakeGCP(t).with(seedValue, enabled)
		f.failures = map[string]codes.Code{"AccessSecretVersion": codes.PermissionDenied}
		serveGCP(t, f)
		code, out, errs := runGCP(t, f, "verify", "--public-key", public)
		require.Equal(t, 2, code)
		require.Empty(t, out)
		require.Equal(t, "refused: access-secret-version "+gcpLatestOf+" failed: PermissionDenied\n", errs)
		require.Equal(t, []string{"GetSecret", "GetSecretVersion", "AccessSecretVersion"}, f.recorded(),
			"a read refused for any reason but the version's state asks nothing more")
	})
}

// --expect-account and --region are Secrets Manager's: a Secret Manager secret's name names its
// project, and the store refuses both before it opens a client.
func TestGCPRefusesTheAWSFlagsBeforeAnyCall(t *testing.T) {
	_, public := testnats.User(t)
	for _, flag := range [][]string{{"--expect-account", "000000000000"}, {"--region", "us-central1"}} {
		for _, mode := range [][]string{{"check"}, {"generate"}, {"verify", "--public-key", public}} {
			t.Run(flag[0]+"/"+mode[0], func(t *testing.T) {
				f := newFakeGCP(t)
				serveGCP(t, f)
				code, out, errs := runGCP(t, f, append(mode, flag...)...)
				require.Equal(t, 2, code, errs)
				require.Empty(t, out)
				require.True(t, strings.HasPrefix(errs, "refused: "+gcpSecret+": "+flag[0]+" is not accepted with --store gcp-secret-manager"), errs)
				require.Empty(t, f.recorded())
			})
		}
	}
}

// --name is a global secret's name, projects/<project>/secrets/<secret>; anything else is refused
// before any call, without echoing what was given, since a seed may have been pasted there.
func TestGCPRefusesANameThatIsNoGlobalSecret(t *testing.T) {
	_, public := testnats.User(t)
	names := []string{
		gcpSecretID,
		gcpSecret + "/versions/1",
		"projects/example-project/locations/us-central1/secrets/" + gcpSecretID,
		"projects//secrets/" + gcpSecretID,
		"projects/example-project/secrets/",
		"projects/example project/secrets/" + gcpSecretID,
		"projects/example-project/secrets/" + strings.Repeat("a", 256),
	}
	for _, name := range names {
		for _, mode := range [][]string{{"check"}, {"generate"}, {"verify", "--public-key", public}} {
			f := newFakeGCP(t)
			serveGCP(t, f)
			code, out, errs := runGCPAs(t, f, name, mode...)
			require.Equal(t, 2, code, errs)
			require.Empty(t, out)
			require.Equal(t, "refused: --name for --store gcp-secret-manager is a secret's name, projects/<project>/secrets/<secret>\n", errs)
			require.Empty(t, f.recorded())
		}
	}
}

// A seed pasted as --name is refused by the command's flag rule, before the store's name rule and
// before the store opens, so no refusal of the open quotes it, whatever else is wrong.
func TestGCPRefusesAMalformedNameBeforeOpeningTheStore(t *testing.T) {
	pasted, _ := testnats.User(t)
	for _, row := range []struct {
		name  string
		flags []string
		env   map[string]string
	}{
		{"--region", []string{"--region", "us-central1"}, nil},
		{"--expect-account", []string{"--expect-account", "000000000000"}, nil},
		{"an empty access token", nil, map[string]string{"GOOGLE_OAUTH_ACCESS_TOKEN": ""}},
		{"a malformed endpoint", nil, map[string]string{"SECRET_MANAGER_ENDPOINT": "http://127.0.0.1:1"}},
		{"no credentials", nil, map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": filepath.Join(t.TempDir(), "absent.json")}},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newFakeGCP(t)
			serveGCP(t, f)
			for key, value := range row.env {
				t.Setenv(key, value)
			}
			if _, ok := row.env["GOOGLE_APPLICATION_CREDENTIALS"]; ok {
				require.NoError(t, os.Unsetenv("GOOGLE_OAUTH_ACCESS_TOKEN"))
			}
			code, out, errs := runGCPAs(t, f, pasted, append([]string{"check"}, row.flags...)...)
			require.Equal(t, 2, code, errs)
			require.Empty(t, out)
			require.NotContains(t, errs, pasted)
			require.True(t, strings.HasPrefix(errs, "envoy-infra seed: --name: an nkey seed, which is never an argument (not echoed)\n"), errs)
			require.Empty(t, f.recorded())
		})
	}
}

// A seed in a variable whose value the Secret Manager store, or a library it calls, sends in a
// request, dials as a host or quotes in an error is refused before any call, naming the variable
// and never its value. With each variable those libraries read set to a seed, a run against the
// fake found GOOGLE_APPLICATION_CREDENTIALS quoted on stderr ("open <seed>: no such file or
// directory"), GOOGLE_OAUTH_ACCESS_TOKEN and GOOGLE_CLOUD_QUOTA_PROJECT in request headers (the
// bearer token, x-goog-user-project), and GOOGLE_CLOUD_UNIVERSE_DOMAIN, GCE_METADATA_HOST and an
// https endpoint's host dialed. A seed is found as bus.HoldsSeed finds one: the whole value, inside
// a longer one (a path, a token, a host, a project), or percent-encoded, as in an endpoint's path or
// query, which the endpoint's form would refuse but only after the seed check names the variable.
func TestGCPRefusesASeedInItsConfiguration(t *testing.T) {
	for _, row := range []struct {
		name, variable string
		value          func(seed string) string
	}{
		{"a whole access token", "GOOGLE_OAUTH_ACCESS_TOKEN", whole},
		{"a seed inside an access token", "GOOGLE_OAUTH_ACCESS_TOKEN", func(s string) string { return "ya29." + s + ".x" }},
		{"a whole credentials path", "GOOGLE_APPLICATION_CREDENTIALS", whole},
		{"a seed inside a credentials path", "GOOGLE_APPLICATION_CREDENTIALS",
			func(s string) string { return "/home/example/.config/" + s + ".json" }},
		{"a whole quota project", "GOOGLE_CLOUD_QUOTA_PROJECT", whole},
		{"a seed inside a quota project", "GOOGLE_CLOUD_QUOTA_PROJECT", func(s string) string { return "example-" + s }},
		{"a whole universe domain", "GOOGLE_CLOUD_UNIVERSE_DOMAIN", whole},
		{"a seed inside a universe domain", "GOOGLE_CLOUD_UNIVERSE_DOMAIN", func(s string) string { return s + ".example.com" }},
		{"a whole metadata host", "GCE_METADATA_HOST", whole},
		{"a seed inside a metadata host", "GCE_METADATA_HOST", func(s string) string { return "metadata-" + s + ":80" }},
		{"a whole endpoint", "SECRET_MANAGER_ENDPOINT", whole},
		{"an endpoint whose host is a seed", "SECRET_MANAGER_ENDPOINT", func(s string) string { return "https://" + s }},
		{"a seed inside an endpoint's host", "SECRET_MANAGER_ENDPOINT", func(s string) string { return "https://" + s + ".example.com" }},
		{"an http endpoint whose host is a seed", "SECRET_MANAGER_ENDPOINT", func(s string) string { return "http://" + s }},
		{"a seed percent-encoded in an endpoint's path", "SECRET_MANAGER_ENDPOINT",
			func(s string) string { return "https://secretmanager.googleapis.com/" + percentEncoded(s) }},
		{"a seed percent-encoded in an endpoint's query", "SECRET_MANAGER_ENDPOINT",
			func(s string) string { return "https://secretmanager.googleapis.com/?k=" + percentEncoded(s) }},
		// A reader or a decoder takes a seed in lowercase, or split by '.', '-', '_' or whitespace, as
		// the seed.
		{"a lowercase access token", "GOOGLE_OAUTH_ACCESS_TOKEN", func(s string) string { return "ya29." + strings.ToLower(s) }},
		{"an access token split by a space", "GOOGLE_OAUTH_ACCESS_TOKEN", func(s string) string { return split(s, " ") }},
		{"a lowercase credentials path", "GOOGLE_APPLICATION_CREDENTIALS",
			func(s string) string { return "/home/example/" + strings.ToLower(s) + ".json" }},
		{"a credentials path split by a hyphen", "GOOGLE_APPLICATION_CREDENTIALS",
			func(s string) string { return "/home/example/" + split(s, "-") + ".json" }},
		{"a lowercase quota project", "GOOGLE_CLOUD_QUOTA_PROJECT", func(s string) string { return strings.ToLower(s) }},
		{"a quota project split by an underscore", "GOOGLE_CLOUD_QUOTA_PROJECT", func(s string) string { return split(s, "_") }},
		{"a lowercase universe domain", "GOOGLE_CLOUD_UNIVERSE_DOMAIN",
			func(s string) string { return strings.ToLower(s) + ".example.com" }},
		{"a universe domain split across DNS labels", "GOOGLE_CLOUD_UNIVERSE_DOMAIN",
			func(s string) string { return split(s, ".") + ".example.com" }},
		{"a lowercase metadata host", "GCE_METADATA_HOST", func(s string) string { return strings.ToLower(s) + ":80" }},
		{"a metadata host split across DNS labels", "GCE_METADATA_HOST", func(s string) string { return split(s, ".") + ":80" }},
		{"a lowercase endpoint host", "SECRET_MANAGER_ENDPOINT", func(s string) string { return "https://" + strings.ToLower(s) }},
		{"an endpoint host split across DNS labels", "SECRET_MANAGER_ENDPOINT",
			func(s string) string { return "https://" + split(s, ".") + ".example.com" }},
	} {
		t.Run(row.name, func(t *testing.T) {
			seedValue, _ := testnats.User(t)
			f := newFakeGCP(t)
			serveGCP(t, f)
			t.Setenv(row.variable, row.value(seedValue))
			code, out, errs := runGCP(t, f, "check")
			require.Equal(t, 2, code, errs)
			require.Empty(t, out)
			require.Equal(t, "refused: "+gcpSecret+": "+row.variable+": an nkey user seed, which is never Google Cloud "+
				"configuration (not echoed)\n", errs)
			require.Empty(t, f.recorded(), "no call was made")
			require.NotContains(t, strings.Join(f.headers, "\n"), seedValue)
		})
	}
}

// whole is a configuration value that is the seed itself.
func whole(seed string) string { return seed }

// split is seed with sep between its halves.
func split(seed, sep string) string { return seed[:len(seed)/2] + sep + seed[len(seed)/2:] }

// serviceAccount writes a service account key, as Application Default Credentials find one through
// GOOGLE_APPLICATION_CREDENTIALS, and returns it with a check that a bearer token is a JWT that key
// signed for that account. The library signs the JWT itself, so no token endpoint is called.
func serviceAccount(t *testing.T) (file string, signedByIt func(token string) error) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	const email = "envoy-infra-test@example-project.iam.gserviceaccount.com"
	content, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "example-project",
		"private_key_id": "example-key-id",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email":   email,
		"client_id":      "0",
		"token_uri":      "https://oauth2.googleapis.com/token",
	})
	require.NoError(t, err)
	file = filepath.Join(t.TempDir(), "service-account.json")
	require.NoError(t, os.WriteFile(file, content, 0o600))
	return file, func(token string) error {
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			return errors.New("not a JWT")
		}
		signature, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			return err
		}
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
			return fmt.Errorf("not signed by the account's key: %w", err)
		}
		claims, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return err
		}
		var issuer struct{ Iss string }
		if err := json.Unmarshal(claims, &issuer); err != nil || issuer.Iss != email {
			return fmt.Errorf("issued by %q, not the account", issuer.Iss)
		}
		return nil
	}
}

func TestGCPCredentials(t *testing.T) {
	t.Run("application default credentials", func(t *testing.T) {
		f := newFakeGCP(t)
		file, signedByIt := serviceAccount(t)
		f.bearer = signedByIt
		serveGCP(t, f)
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", file)
		require.NoError(t, os.Unsetenv("GOOGLE_OAUTH_ACCESS_TOKEN"))
		code, out, errs := runGCP(t, f, "check")
		require.Equal(t, 0, code, errs)
		require.Equal(t, gcpSecret+": present; latest: no\n", out)
		require.Equal(t, []string{"GetSecret", "GetSecretVersion"}, f.recorded())
	})
	t.Run("GOOGLE_OAUTH_ACCESS_TOKEN over application default credentials", func(t *testing.T) {
		f := newFakeGCP(t)
		file, _ := serviceAccount(t)
		serveGCP(t, f)
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", file)
		code, out, errs := runGCP(t, f, "check")
		require.Equal(t, 0, code, errs)
		require.Equal(t, gcpSecret+": present; latest: no\n", out)
	})
	t.Run("an empty GOOGLE_OAUTH_ACCESS_TOKEN", func(t *testing.T) {
		f := newFakeGCP(t)
		serveGCP(t, f)
		t.Setenv("GOOGLE_OAUTH_ACCESS_TOKEN", "")
		code, out, errs := runGCP(t, f, "check")
		require.Equal(t, 2, code)
		require.Empty(t, out)
		require.Equal(t, "refused: "+gcpSecret+": GOOGLE_OAUTH_ACCESS_TOKEN is set but empty; unset it to use Application Default Credentials\n", errs)
		require.Empty(t, f.recorded())
	})
}

func TestGCPEndpoint(t *testing.T) {
	for raw, want := range map[string]string{
		"https://secretmanager.googleapis.com":  "secretmanager.googleapis.com:443",
		"https://secretmanager.googleapis.com/": "secretmanager.googleapis.com:443",
		"https://127.0.0.1:8443":                "127.0.0.1:8443",
		"https://[::1]:8443":                    "[::1]:8443",
	} {
		got, err := gcpEndpoint(raw)
		require.NoError(t, err, raw)
		require.Equal(t, want, got, raw)
	}
	for _, raw := range []string{"", "127.0.0.1:8443", "http://127.0.0.1:8443", "https://", "https://127.0.0.1:8443/v1",
		"https://user@127.0.0.1", "https://127.0.0.1?x=1", "https://127.0.0.1#x", "dns:///127.0.0.1:8443"} {
		_, err := gcpEndpoint(raw)
		require.ErrorIs(t, err, ErrRefused, raw)
		require.Equal(t, "SECRET_MANAGER_ENDPOINT is https://<host>[:<port>], as https://secretmanager.googleapis.com", err.Error(), raw)
	}

	f := newFakeGCP(t)
	serveGCP(t, f)
	t.Setenv("SECRET_MANAGER_ENDPOINT", "http://127.0.0.1:1")
	code, out, errs := runGCP(t, f, "check")
	require.Equal(t, 2, code)
	require.Empty(t, out)
	require.Equal(t, "refused: "+gcpSecret+": SECRET_MANAGER_ENDPOINT is https://<host>[:<port>], as https://secretmanager.googleapis.com\n", errs)
	require.Empty(t, f.recorded())
}

// captureStderr runs run with the process's stderr pointed at a file and returns what it wrote.
func captureStderr(t *testing.T, run func()) string {
	t.Helper()
	file, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	require.NoError(t, err)
	saved := os.Stderr
	os.Stderr = file
	func() {
		defer func() { os.Stderr = saved }()
		run()
	}()
	require.NoError(t, file.Close())
	written, err := os.ReadFile(file.Name())
	require.NoError(t, err)
	return string(written)
}

// At GOOGLE_SDK_GO_LOGGING_LEVEL=debug the client library prints every request and response on
// stderr, and a write carries the seed in one, a read in the other, base64-encoded, so a search for
// the seed's text would miss it: only stderr left empty shows none leaked. The store's client logs
// nothing, whatever that variable says.
func TestGCPClientLogsNothing(t *testing.T) {
	ctx := context.Background()
	f := newFakeGCP(t)
	addr := serveGCP(t, f)
	t.Setenv("GOOGLE_SDK_GO_LOGGING_LEVEL", "debug")
	require.True(t, internaloption.GetLogger(nil).Enabled(ctx, slog.LevelDebug), "control: at debug the library logs every call")
	opts, err := gcpClientOptions()
	require.NoError(t, err)
	logger := internaloption.GetLogger(opts)
	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		require.False(t, logger.Enabled(ctx, level), "the store's client logs at %s", level)
	}

	written := captureStderr(t, func() {
		code, _, errs := runGCP(t, f, "generate")
		require.Equal(t, 0, code, errs)
	})
	require.Empty(t, written, "the client library wrote to stderr")

	// The control: the same library with no logger of the store's prints the request it sends.
	f.setMode("check")
	written = captureStderr(t, func() {
		client, err := secretmanager.NewClient(ctx, option.WithEndpoint(addr),
			option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: gcpToken})))
		require.NoError(t, err)
		defer client.Close()
		_, err = client.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: gcpSecret})
		require.NoError(t, err)
	})
	require.Contains(t, written, gcpSecret, "control: without the store's logger the library prints its request")
}

// The seeds the store holds, a seed passed where the public key belongs, and a request a failing call
// quotes, reach no output, in any mode and whatever the secret's state.
func TestGCPNoSeedReachesAnOutput(t *testing.T) {
	stored, public := testnats.User(t)
	pasted, otherPublic := testnats.User(t)
	states := map[string]func(*fakeGCP) *fakeGCP{
		"enabled":      func(f *fakeGCP) *fakeGCP { return f.with(stored, enabled) },
		"disabled":     func(f *fakeGCP) *fakeGCP { return f.with(stored, disabled) },
		"destroyed":    func(f *fakeGCP) *fakeGCP { return f.with(stored, enabled).with("", destroyed) },
		"two versions": func(f *fakeGCP) *fakeGCP { return f.with(stored, disabled).with(stored, enabled) },
		"no versions":  func(f *fakeGCP) *fakeGCP { return f },
		"not a seed":   func(f *fakeGCP) *fakeGCP { return f.with(stored+"\n", enabled) },
		"failing calls quote their requests": func(f *fakeGCP) *fakeGCP {
			f.echoRequests = true
			f.failures = map[string]codes.Code{"AddSecretVersion": codes.InvalidArgument}
			return f
		},
	}
	invocations := [][]string{
		{"check"},
		{"generate"},
		{"verify", "--public-key", public},
		{"verify", "--public-key", otherPublic},
		{"verify", "--public-key", pasted},
	}
	for name, state := range states {
		for _, args := range invocations {
			t.Run(name+"/"+args[0], func(t *testing.T) {
				f := state(newFakeGCP(t))
				serveGCP(t, f)
				code, out, errs := runGCP(t, f, args...)
				require.Contains(t, []int{0, 1, 2}, code)
				require.NotEmpty(t, out+errs, "every run reports something")
				if args[len(args)-1] != pasted { // a seed as the public key is refused before any call
					require.NotEmpty(t, f.recorded(), "every run reaches Secret Manager")
				}
				require.NotContains(t, out+errs, pasted)
				require.NotContains(t, out+errs, stored)
			})
		}
	}
}

// The fake fails a test on any call the store does not make: a call outside the mode's, a request
// with a field it does not expect, or a call without the test's bearer token.
func TestTheGCPFakeRecordsEveryCallItDoesNotExpect(t *testing.T) {
	f := newFakeGCP(t)
	addr := serveGCP(t, f)
	leaf, err := x509.ParseCertificate(gcpCert.Certificate[0])
	require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(roots, "")))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client := secretmanagerpb.NewSecretManagerServiceClient(conn)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+gcpToken)

	calls := []struct {
		mode string
		call func() error
	}{
		{"check", func() error {
			_, err := client.ListSecretVersions(ctx, &secretmanagerpb.ListSecretVersionsRequest{Parent: gcpSecret})
			return err
		}},
		{"check", func() error {
			_, err := client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: gcpLatestOf})
			return err
		}},
		{"verify", func() error {
			_, err := client.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: gcpSecret,
				Payload: &secretmanagerpb.SecretPayload{Data: []byte("x")}})
			return err
		}},
		{"generate", func() error {
			_, err := client.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: gcpSecret})
			return err
		}},
		{"generate", func() error {
			_, err := client.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{Name: gcpSecret + "/versions/1"})
			return err
		}},
		{"generate", func() error {
			_, err := client.ListSecretVersions(ctx, &secretmanagerpb.ListSecretVersionsRequest{Parent: gcpSecret, Filter: "state:ENABLED"})
			return err
		}},
		{"generate", func() error {
			_, err := client.GetSecret(context.Background(), &secretmanagerpb.GetSecretRequest{Name: gcpSecret})
			return err
		}},
	}
	for _, c := range calls {
		f.setMode(c.mode)
		require.Error(t, c.call())
	}
	f.mu.Lock()
	require.Len(t, f.unexpected, len(calls))
	require.Contains(t, f.unexpected[0], "ListSecretVersions from check")
	require.Contains(t, f.unexpected[6], "without a bearer token")
	f.unexpected = nil
	f.mu.Unlock()

	// The same calls, as the store makes them in their mode, are answered.
	f.setMode("generate")
	_, err = client.ListSecretVersions(ctx, &secretmanagerpb.ListSecretVersionsRequest{Parent: gcpSecret})
	require.NoError(t, err)
	_, err = client.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: gcpSecret})
	require.NoError(t, err)
	f.mu.Lock()
	require.Empty(t, f.unexpected)
	f.mu.Unlock()
}

func TestGCPErrorsAreRenderedByCodeNeverByTheirText(t *testing.T) {
	const canary = "canary-value-7f3e"
	wrapped, ok := apierror.FromError(status.Error(codes.FailedPrecondition, "got "+canary))
	require.True(t, ok)
	rows := []struct {
		err  error
		want string
	}{
		{status.Error(codes.InvalidArgument, "got "+canary), "InvalidArgument"},
		{fmt.Errorf("wrapped: %w", status.Error(codes.PermissionDenied, canary)), "PermissionDenied"},
		{wrapped, "FailedPrecondition"},
		{fmt.Errorf("wrapped: %w", context.Canceled), "canceled"},
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), "deadline exceeded"},
		{errors.New(canary), "errorString"},
	}
	for _, row := range rows {
		got := failed("add-secret-version", gcpSecret, row.err).Error()
		require.Equal(t, "add-secret-version "+gcpSecret+" failed: "+row.want, got)
		require.NotContains(t, got, canary)
	}
}
