package seed

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	smithy "github.com/aws/smithy-go"
	"github.com/aws/smithy-go/logging"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/testnats"
)

func publicKeyOfSeed(t *testing.T, seed string) string {
	t.Helper()
	kp, err := nkeys.FromSeed([]byte(seed))
	require.NoError(t, err)
	public, err := kp.PublicKey()
	require.NoError(t, err)
	return public
}

func TestAWSConfigReachesTheEndpointAndLogsNothing(t *testing.T) {
	server := serve(t, newFakeAWS(t))
	cfg, err := loadAWSConfig(context.Background(), awsRegion)
	require.NoError(t, err)
	require.Equal(t, server.URL, aws.ToString(cfg.BaseEndpoint))
	require.Equal(t, awsRegion, cfg.Region)
	require.Equal(t, logging.Nop{}, cfg.Logger, "the SDK's logger, which can print request bodies, is off")
	require.Zero(t, cfg.ClientLogMode)
}

func TestAWSCheck(t *testing.T) {
	// Python's str of the datetime boto3 parses DeletedDate into, in the machine's zone: botocore
	// 1.40.61's parse_timestamp under TZ=America/Los_Angeles gave these two for these epochs.
	zone := time.Local
	time.Local = time.FixedZone("PDT", -7*60*60)
	t.Cleanup(func() { time.Local = zone })
	seedValue, _ := testnats.User(t)
	rows := []struct {
		name  string
		fake  func(*fakeAWS) *fakeAWS
		want  string
		calls []string
	}{
		{"absent", func(f *fakeAWS) *fakeAWS { f.exists = false; return f }, awsSecret + ": absent", []string{"DescribeSecret"}},
		{"scheduled for deletion", func(f *fakeAWS) *fakeAWS { f.deletedAt = 1759190400.5; return f.holding(seedValue) },
			awsSecret + ": present, scheduled for deletion (2025-09-29 17:00:00.500000-07:00)", []string{"DescribeSecret"}},
		{"scheduled for deletion on the second", func(f *fakeAWS) *fakeAWS { f.deletedAt = 1759190400; return f },
			awsSecret + ": present, scheduled for deletion (2025-09-29 17:00:00-07:00)", []string{"DescribeSecret"}},
		{"empty", func(f *fakeAWS) *fakeAWS { return f }, awsSecret + ": present; AWSCURRENT: no", []string{"DescribeSecret"}},
		{"seeded", func(f *fakeAWS) *fakeAWS { return f.holding(seedValue, seedValue) }, awsSecret + ": present; AWSCURRENT: yes (version v2)",
			[]string{"DescribeSecret"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := row.fake(newFakeAWS(t))
			serve(t, f)
			code, out, errs := runAWS(t, f, "check")
			require.Equal(t, 0, code, errs)
			require.Equal(t, row.want+"\n", out)
			require.Empty(t, errs)
			require.Equal(t, row.calls, f.recorded())
		})
	}
}

// generate writes the bare seed string as the first version, under one fixed ClientRequestToken: the
// same for every secret, since a version id is per secret, so that a second run's first-version
// write with another seed is refused rather than written beside the first.
func TestAWSGenerateWritesTheBareSeedWithTheFirstVersionTokenAndReadsItBack(t *testing.T) {
	f := newFakeAWS(t)
	serve(t, f)
	code, out, errs := runAWS(t, f, "generate")
	require.Equal(t, 0, code, errs)
	require.Empty(t, errs)
	require.Equal(t, []string{"DescribeSecret", "ListSecretVersionIds", "PutSecretValue", "ListSecretVersionIds", "GetSecretValue"},
		f.recorded())
	require.Len(t, f.versions, 1)
	written := f.versions[0]
	require.True(t, strings.HasPrefix(written.value, "SU"), "the value is the bare seed string")
	require.Len(t, f.tokens, 1)
	require.Equal(t, f.tokens[0], written.id, "Secrets Manager names the version by the request token")
	require.Equal(t, "public key: "+publicKeyOfSeed(t, written.value)+"\nversion id: "+written.id+"\n", out)

	again := newFakeAWS(t)
	serve(t, again)
	code, _, errs = runAWS(t, again, "generate")
	require.Equal(t, 0, code, errs)
	require.Equal(t, f.tokens[0], again.tokens[0], "every generate sends the one first-version token")
	require.Equal(t, firstVersionToken, f.tokens[0])
}

// awsStoreOn serves f and opens the AWS store against it, in the test's region, with no account.
// f answers version listings, which through the command only generate makes.
func awsStoreOn(t *testing.T, f *fakeAWS) Store {
	t.Helper()
	f.generating = true
	serve(t, f)
	s, err := OpenAWS(context.Background(), awsRegion, "")
	require.NoError(t, err)
	return s
}

// The AWS store keeps every Store's contract against the fake, which applies Secrets Manager's
// ClientRequestToken rules: it refuses a second first version since every Put sends
// firstVersionToken.
func TestAWSStoreKeepsTheStoreContract(t *testing.T) {
	testStoreContract(t, func(t *testing.T) (Store, string) { return awsStoreOn(t, newFakeAWS(t)), awsSecret })
}

// Two generate runs overlap on one secret. B's checks are answered as they would have been before
// A's write landed, so B finds the secret empty; A has written and printed its key. B's write carries
// the same first-version token with another seed, so Secrets Manager refuses it
// (ResourceExistsException): B writes nothing and exits 2, and A's printed key still verifies.
func TestAWSOverlappingGenerateRunsLeaveTheFirstSeedServed(t *testing.T) {
	f := newFakeAWS(t)
	serve(t, f)
	code, outA, errs := runAWS(t, f, "generate")
	require.Equal(t, 0, code, errs)
	keyA, _, _ := strings.Cut(strings.TrimPrefix(outA, "public key: "), "\n")

	f.mu.Lock()
	f.calls, f.staleViews = nil, 2
	f.mu.Unlock()
	code, outB, errs := runAWS(t, f, "generate")
	require.Equal(t, 2, code, errs)
	require.Equal(t, []string{"DescribeSecret", "ListSecretVersionIds", "PutSecretValue"}, f.recorded())
	keyB, rest, _ := strings.Cut(strings.TrimPrefix(outB, "public key: "), "\n")
	require.NotEqual(t, keyA, keyB)
	require.Empty(t, rest, "after B's key, no version id: no version holds it")
	require.Equal(t, "refused: put-secret-value "+awsSecret+" failed: ResourceExistsException; another generate wrote "+
		"this secret's first version, with another seed; nothing was written, and the public key above is not stored\n", errs)
	require.Len(t, f.versions, 1, "B wrote no version")

	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	code, out, errs := runAWS(t, f, "verify", "--public-key", keyA)
	require.Equal(t, 0, code, errs)
	require.True(t, strings.HasSuffix(out, " derives "+keyA+": yes\n"), out)
}

// The AWS store's own answers, each from the one call it makes; generate's messages over them are
// seed_test.go's. DescribeSecret's ResourceNotFoundException is a secret that does not exist and its
// DeletedDate one scheduled for deletion; ListSecretVersionIds is counted with deprecated versions
// and across every page, and a page token handed out twice is refused; a written version must be
// AWSCURRENT; GetSecretValue's ResourceNotFoundException matches ErrNotServed; and a failure names
// the call, the secret and the AWS error code.
func TestAWSStoreAnswersFromTheCallItMakes(t *testing.T) {
	zone := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = zone })
	seedValue, _ := testnats.User(t)
	ctx := context.Background()
	describe := func(s Store) (any, error) { return s.Describe(ctx, awsSecret) }
	versions := func(s Store) (any, error) { return s.Versions(ctx, awsSecret) }
	put := func(s Store) (any, error) { return s.Put(ctx, awsSecret, []byte(seedValue)) }
	current := func(s Store) (any, error) {
		version, value, err := s.Current(ctx, awsSecret)
		require.Empty(t, value, "a failed read answers no value")
		return version, err
	}
	failing := func(call, code string) func(*fakeAWS) *fakeAWS {
		return func(f *fakeAWS) *fakeAWS {
			f.failures = map[string]awsFailure{call: {code, "the request failed"}}
			return f.holding(seedValue)
		}
	}
	rows := []struct {
		name  string
		fake  func(*fakeAWS) *fakeAWS
		call  func(Store) (any, error)
		want  any
		err   string
		is    error // an error the store's must match, when not nil
		calls []string
	}{
		{"a secret that does not exist", func(f *fakeAWS) *fakeAWS { f.exists = false; return f }, describe,
			Description{}, "", nil, []string{"DescribeSecret"}},
		{"a secret scheduled for deletion", func(f *fakeAWS) *fakeAWS { f.deletedAt = 1759190400; return f.holding(seedValue) }, describe,
			Description{Exists: true, ScheduledForDeletion: true, DeletionDate: "2025-09-30 00:00:00+00:00"}, "", nil, []string{"DescribeSecret"}},
		{"describe-secret denied", failing("DescribeSecret", "AccessDeniedException"), describe,
			Description{}, "describe-secret " + awsSecret + " failed: AccessDeniedException", nil, []string{"DescribeSecret"}},
		{"a deprecated version DescribeSecret does not show", func(f *fakeAWS) *fakeAWS {
			f.versions = []awsVersion{{id: "v1", value: seedValue}}
			return f
		}, versions, 1, "", nil, []string{"ListSecretVersionIds"}},
		{"versions on every page", func(f *fakeAWS) *fakeAWS { f.pageSize = 2; return f.holding(seedValue, seedValue, seedValue) }, versions,
			3, "", nil, []string{"ListSecretVersionIds", "ListSecretVersionIds"}},
		{"a listing that repeats its page token", func(f *fakeAWS) *fakeAWS { f.repeatToken = true; return f }, versions,
			0, "list-secret-version-ids " + awsSecret + " failed: the listing handed out page token page-2 twice", nil,
			[]string{"ListSecretVersionIds", "ListSecretVersionIds"}},
		{"list-secret-version-ids denied", failing("ListSecretVersionIds", "AccessDeniedException"), versions,
			0, "list-secret-version-ids " + awsSecret + " failed: AccessDeniedException", nil, []string{"ListSecretVersionIds"}},
		{"a written version that is not AWSCURRENT", func(f *fakeAWS) *fakeAWS { f.putStages = []string{"AWSPENDING"}; return f }, put,
			firstVersionToken, "version " + firstVersionToken + " was written but is not AWSCURRENT; run check", nil, []string{"PutSecretValue"}},
		{"no version AWSCURRENT", func(f *fakeAWS) *fakeAWS { return f }, current,
			"", "get-secret-value " + awsSecret + " failed: ResourceNotFoundException", ErrNotServed, []string{"GetSecretValue"}},
		{"get-secret-value failing", failing("GetSecretValue", "DecryptionFailure"), current,
			"", "get-secret-value " + awsSecret + " failed: DecryptionFailure", nil, []string{"GetSecretValue"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := row.fake(newFakeAWS(t))
			got, err := row.call(awsStoreOn(t, f))
			require.Equal(t, row.want, got)
			if row.err == "" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, row.err)
				require.NotErrorIs(t, err, ErrRefused, "the store refuses only another writer's first version")
			}
			if row.is != nil {
				require.ErrorIs(t, err, row.is)
			}
			require.Equal(t, row.calls, f.recorded())
		})
	}
}

// Secrets Manager's reads are eventually consistent: just after the write, a listing can leave the
// new version out and GetSecretValue can find no AWSCURRENT version. generate looks again rather
// than calling either a race or a failed read-back.
func TestAWSGenerateWaitsForTheStoreToShowItsWrite(t *testing.T) {
	SetReadBackWaits(t, 0, 0, 0)
	rows := []struct {
		name                  string
		staleLists, staleGets int
		calls                 []string
	}{
		{"a stale listing", 1, 0, []string{"DescribeSecret", "ListSecretVersionIds", "PutSecretValue",
			"ListSecretVersionIds", "ListSecretVersionIds", "GetSecretValue"}},
		{"a stale read", 0, 1, []string{"DescribeSecret", "ListSecretVersionIds", "PutSecretValue",
			"ListSecretVersionIds", "GetSecretValue", "ListSecretVersionIds", "GetSecretValue"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := newFakeAWS(t)
			f.staleLists, f.staleGets = row.staleLists, row.staleGets
			serve(t, f)
			code, out, errs := runAWS(t, f, "generate")
			require.Equal(t, 0, code, errs)
			require.Empty(t, errs)
			require.Equal(t, "public key: "+publicKeyOfSeed(t, f.versions[0].value)+"\nversion id: "+f.tokens[0]+"\n", out)
			require.Equal(t, row.calls, f.recorded())
		})
	}
}

// A lost answer is retried with the same ClientRequestToken, so Secrets Manager writes one version.
func TestAWSGenerateRetriesALostAnswerWithItsToken(t *testing.T) {
	f := newFakeAWS(t)
	f.loseAnswers = 1
	serve(t, f)
	code, out, errs := runAWS(t, f, "generate")
	require.Equal(t, 0, code, errs)
	require.Len(t, f.tokens, 2)
	require.Equal(t, f.tokens[0], f.tokens[1])
	require.Len(t, f.versions, 1)
	require.Contains(t, out, "version id: "+f.tokens[0]+"\n")
}

// A write whose answer never came may have landed, as here, so the operator gets the public key
// generate printed before it, and verify can check the secret against it.
func TestAWSGenerateCallsAnUnansweredWriteUnknownNotRefused(t *testing.T) {
	f := newFakeAWS(t)
	f.loseAnswers = 1
	serve(t, f)
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	code, out, errs := runAWS(t, f, "generate")
	require.Equal(t, 1, code, errs)
	require.Equal(t, "error after writing: put-secret-value "+awsSecret+" failed: RequestSendError; "+
		"whether a version was written is unknown, run check\n", errs)
	require.Len(t, f.versions, 1, "the write landed")
	require.Equal(t, "public key: "+publicKeyOfSeed(t, f.versions[0].value)+"\nversion id: unknown\n", out)
}

// A service message can quote the request, and a write's request carries the seed: an AWS error is
// rendered by its code alone.
func TestAWSAnErrorQuotingTheRequestNeverPrintsIt(t *testing.T) {
	f := newFakeAWS(t)
	f.echoRequests = true
	f.failures = map[string]awsFailure{"PutSecretValue": {"InvalidRequestException", "rejected"}}
	serve(t, f)
	code, _, errs := runAWS(t, f, "generate")
	require.Equal(t, 1, code)
	require.Equal(t, "error after writing: put-secret-value "+awsSecret+" failed: InvalidRequestException; "+
		"whether a version was written is unknown, run check\n", errs)
}

func TestAWSVerify(t *testing.T) {
	seedValue, public := testnats.User(t)
	_, another := testnats.User(t)
	t.Run("yes", func(t *testing.T) {
		f := newFakeAWS(t).holding(seedValue)
		serve(t, f)
		code, out, errs := runAWS(t, f, "verify", "--public-key", public)
		require.Equal(t, 0, code, errs)
		require.Equal(t, awsSecret+" AWSCURRENT (version v1) derives "+public+": yes\n", out)
		require.Equal(t, []string{"DescribeSecret", "GetSecretValue"}, f.recorded())
	})
	t.Run("no", func(t *testing.T) {
		f := newFakeAWS(t).holding(seedValue)
		serve(t, f)
		code, out, errs := runAWS(t, f, "verify", "--public-key", another)
		require.Equal(t, 1, code, errs)
		require.Equal(t, awsSecret+" AWSCURRENT (version v1) derives "+another+": no\n", out)
		require.Empty(t, errs)
	})
	t.Run("reading is denied", func(t *testing.T) {
		f := newFakeAWS(t).holding(seedValue)
		f.failures = map[string]awsFailure{"GetSecretValue": {"AccessDeniedException", "not allowed"}}
		serve(t, f)
		code, out, errs := runAWS(t, f, "verify", "--public-key", public)
		require.Equal(t, 2, code)
		require.Empty(t, out)
		require.Equal(t, "refused: get-secret-value "+awsSecret+" failed: AccessDeniedException\n", errs)
	})
	// generate waits on a store that serves no version yet; verify, which reads only after
	// DescribeSecret named an AWSCURRENT version, refuses it at once, naming the AWS error.
	t.Run("the read finds no AWSCURRENT version", func(t *testing.T) {
		f := newFakeAWS(t).holding(seedValue)
		f.failures = map[string]awsFailure{"GetSecretValue": {"ResourceNotFoundException", "no version has the staging label AWSCURRENT"}}
		serve(t, f)
		code, out, errs := runAWS(t, f, "verify", "--public-key", public)
		require.Equal(t, 2, code)
		require.Empty(t, out)
		require.Equal(t, "refused: get-secret-value "+awsSecret+" failed: ResourceNotFoundException\n", errs)
		require.Equal(t, []string{"DescribeSecret", "GetSecretValue"}, f.recorded())
	})
}

func TestAWSExpectAccountRunsFirstAndAlone(t *testing.T) {
	t.Run("the expected account", func(t *testing.T) {
		f := newFakeAWS(t)
		serve(t, f)
		code, _, errs := runAWS(t, f, "check", "--expect-account", awsAccount)
		require.Equal(t, 0, code, errs)
		require.Equal(t, []string{"GetCallerIdentity", "DescribeSecret"}, f.recorded())
	})
	t.Run("another account", func(t *testing.T) {
		_, public := testnats.User(t)
		for _, mode := range [][]string{{"check"}, {"generate"}, {"verify", "--public-key", public}} {
			f := newFakeAWS(t)
			f.account = "111111111111"
			serve(t, f)
			code, out, errs := runAWS(t, f, append(mode, "--expect-account", awsAccount)...)
			require.Equal(t, 2, code, errs)
			require.Empty(t, out)
			require.Equal(t, "refused: "+awsSecret+": these credentials are for account 111111111111, but --expect-account is "+
				awsAccount+"; use that account's credentials\n", errs)
			require.Equal(t, []string{"GetCallerIdentity"}, f.recorded())
		}
	})
	t.Run("STS fails", func(t *testing.T) {
		f := newFakeAWS(t)
		f.failures = map[string]awsFailure{"GetCallerIdentity": {"InvalidClientTokenId", "The security token included in the request is invalid."}}
		serve(t, f)
		code, _, errs := runAWS(t, f, "check", "--expect-account", awsAccount)
		require.Equal(t, 2, code)
		require.Equal(t, "refused: "+awsSecret+": sts get-caller-identity failed: InvalidClientTokenId\n", errs)
		require.Equal(t, []string{"GetCallerIdentity"}, f.recorded())
	})
	t.Run("not an account id", func(t *testing.T) {
		f := newFakeAWS(t)
		serve(t, f)
		code, _, errs := runAWS(t, f, "check", "--expect-account", "0000")
		require.Equal(t, 2, code)
		require.Contains(t, errs, "--expect-account is a 12-digit AWS account id")
		require.Empty(t, f.recorded())
	})
	t.Run("without it, no STS call", func(t *testing.T) {
		f := newFakeAWS(t)
		serve(t, f)
		code, _, errs := runAWS(t, f, "check")
		require.Equal(t, 0, code, errs)
		require.NotContains(t, f.recorded(), "GetCallerIdentity")
	})
}

// With no usable credentials, every mode is refused before any call, STS included, naming the fix.
// A credential chain that finds nothing fails with a plain Go error whose type names nothing, so
// the refusal names no class; an AWS answer, such as a profile's role refused for an expired
// token, keeps its code.
func TestAWSMissingCredentialsAreRefusedBeforeAnyCall(t *testing.T) {
	_, public := testnats.User(t)
	for _, args := range [][]string{{"check"}, {"generate"}, {"verify", "--public-key", public}, {"check", "--expect-account", awsAccount}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			f := newFakeAWS(t)
			serve(t, f)
			t.Setenv("AWS_ACCESS_KEY_ID", "")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "")
			code, out, errs := runAWS(t, f, args...)
			require.Equal(t, 2, code)
			require.Empty(t, out)
			require.Equal(t, "refused: "+awsSecret+": no usable AWS credentials; log in or set AWS_PROFILE\n", errs)
			require.Empty(t, f.recorded())
		})
	}
	t.Run("a profile whose role STS refuses for an expired token", func(t *testing.T) {
		f := newFakeAWS(t)
		f.failures = map[string]awsFailure{"AssumeRole": {"ExpiredToken", "The security token included in the request is expired"}}
		server := serve(t, f)
		config := filepath.Join(t.TempDir(), "config")
		require.NoError(t, os.WriteFile(config, []byte("[profile expired]\nrole_arn = "+awsRoleARN+"\nsource_profile = base\n\n"+
			"[profile base]\naws_access_key_id = AKIDEXAMPLE\naws_secret_access_key = example-signing-key\n"), 0o600))
		t.Setenv("AWS_CONFIG_FILE", config)
		t.Setenv("AWS_PROFILE", "expired")
		t.Setenv("AWS_ACCESS_KEY_ID", "")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "")
		t.Setenv("AWS_MAX_ATTEMPTS", "1")
		// The SDK builds a profile's STS client before it reads AWS_ENDPOINT_URL (config.go:67, :76),
		// so only the STS-specific variable keeps this request off the real STS (sts endpoints.go:210-220).
		t.Setenv("AWS_ENDPOINT_URL_STS", server.URL)
		code, out, errs := runAWS(t, f, "generate", "--expect-account", awsAccount)
		require.Equal(t, 2, code)
		require.Empty(t, out)
		require.Equal(t, "refused: "+awsSecret+": no usable AWS credentials (ExpiredToken); log in or set AWS_PROFILE\n", errs)
		require.Equal(t, []string{"AssumeRole"}, f.recorded(), "the credentials' own call, then nothing")
	})
}

// STS and Secrets Manager each resolve their own endpoint, so the account STS answers is not
// necessarily the account of the secret a write lands in. DescribeSecret, every mode's first call,
// answers the secret's ARN: with --expect-account, a secret of another account is refused before
// any other call, and a secret in another region than the client's always is.
func TestAWSTheSecretItselfMustBeInTheExpectedAccountAndRegion(t *testing.T) {
	seedValue, public := testnats.User(t)
	const otherAccount = "111111111111"
	accountMiss := "refused: describe-secret " + awsSecret + " answered a secret of account " + otherAccount +
		", but --expect-account is " + awsAccount + ": the Secrets Manager endpoint answers for another account\n"
	modes := [][]string{{"check"}, {"generate"}, {"verify", "--public-key", public}}
	for _, args := range modes {
		t.Run("another account: "+args[0], func(t *testing.T) {
			f := newFakeAWS(t)
			if args[0] != "generate" {
				f.holding(seedValue)
			}
			f.secretAccount = otherAccount
			serve(t, f)
			code, out, errs := runAWS(t, f, append(args, "--expect-account", awsAccount)...)
			require.Equal(t, 2, code)
			require.Empty(t, out)
			require.Equal(t, accountMiss, errs)
			require.Equal(t, []string{"GetCallerIdentity", "DescribeSecret"}, f.recorded())
		})
	}
	t.Run("another account, scheduled for deletion", func(t *testing.T) {
		f := newFakeAWS(t)
		f.secretAccount, f.deletedAt = otherAccount, 1759190400
		serve(t, f)
		code, _, errs := runAWS(t, f, "check", "--expect-account", awsAccount)
		require.Equal(t, 2, code)
		require.Equal(t, accountMiss, errs)
	})
	t.Run("another account without --expect-account: nothing to compare it with", func(t *testing.T) {
		f := newFakeAWS(t).holding(seedValue)
		f.secretAccount = otherAccount
		serve(t, f)
		code, out, errs := runAWS(t, f, "check")
		require.Equal(t, 0, code, errs)
		require.Equal(t, awsSecret+": present; AWSCURRENT: yes (version v1)\n", out)
	})
	rows := []struct{ name, arn, want string }{
		{"another region", "", "refused: describe-secret " + awsSecret + " answered a secret in region eu-west-1, but the client is in region " + awsRegion + "\n"},
		{"no ARN", " ", "refused: describe-secret " + awsSecret + " answered no Secrets Manager ARN, so the secret's account and region cannot be checked\n"},
		{"another service's ARN", "arn:aws:s3:::example-bucket", "refused: describe-secret " + awsSecret + " answered no Secrets Manager ARN, so the secret's account and region cannot be checked\n"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := newFakeAWS(t).holding(seedValue)
			f.secretARN = row.arn
			if row.name == "another region" {
				f.secretRegion = "eu-west-1"
			}
			serve(t, f)
			code, out, errs := runAWS(t, f, "verify", "--public-key", public)
			require.Equal(t, 2, code)
			require.Empty(t, out)
			require.Equal(t, row.want, errs)
			require.Equal(t, []string{"DescribeSecret"}, f.recorded(), "the seed is not read")
		})
	}
}

// A seed travels in a PutSecretValue request and a GetSecretValue answer, so a configured Secrets
// Manager endpoint that is not https is refused before any call, unless it is this machine's own
// loopback address, which the test endpoint is: plain text there never crosses a network.
func TestAWSAPlainHTTPEndpointBeyondThisMachineIsRefusedBeforeAnyCall(t *testing.T) {
	for _, variable := range []string{"AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_SECRETS_MANAGER"} {
		t.Run(variable, func(t *testing.T) {
			f := newFakeAWS(t)
			serve(t, f)
			t.Setenv(variable, "http://secrets.example.invalid")
			code, out, errs := runAWS(t, f, "generate")
			require.Equal(t, 2, code)
			require.Empty(t, out)
			require.Equal(t, "refused: "+awsSecret+": the Secrets Manager endpoint http://secrets.example.invalid is not https, "+
				"and is not this machine's loopback address: a seed is sent in plain text only to this machine\n", errs)
			require.Empty(t, f.recorded())
		})
	}
}

func TestAWSEndpointCheck(t *testing.T) {
	for _, endpoint := range []string{"https://secrets.example.invalid", "https://10.0.0.1:8443", "http://127.0.0.1:4566",
		"http://127.1.2.3", "http://[::1]:4566", "http://localhost:4566"} {
		require.NoError(t, endpointCheck(aws.String(endpoint)), endpoint)
	}
	require.NoError(t, endpointCheck(nil), "the SDK's own endpoint for the region")
	for _, endpoint := range []string{"http://10.0.0.1:4566", "http://secrets.example.invalid", "ftp://127.0.0.1", "127.0.0.1:4566"} {
		require.ErrorIs(t, endpointCheck(aws.String(endpoint)), ErrRefused, endpoint)
	}
	err := endpointCheck(aws.String("http://user:example-password@secrets.example.invalid"))
	require.ErrorIs(t, err, ErrRefused)
	require.NotContains(t, err.Error(), "example-password")
}

func TestAWSNoRegionIsRefusedBeforeAnyCall(t *testing.T) {
	f := newFakeAWS(t)
	serve(t, f)
	var out, errs bytes.Buffer
	code := Command(context.Background(), []string{"check", "--store", AWSSecretsManager, "--name", awsSecret},
		&out, &errs, Stores{AWSSecretsManager: AWS})
	require.Equal(t, 2, code)
	require.Equal(t, "refused: "+awsSecret+": no AWS region: pass --region, or name one in AWS_REGION or the profile\n", errs.String())
	require.Empty(t, f.recorded())
}

// The AWS configuration a region or a profile name comes from is not a flag, so Command cannot
// check it: the store refuses one that is an nkey user seed before any call, naming where it came
// from and not the value. Otherwise the loader quotes a profile it cannot find ("failed to get
// shared config profile, <name>"), and a region reaches the request's signature and the ARN check's
// refusal. --region is not given, so the region is the configuration's.
func TestAWSASeedInTheAWSConfigurationIsRefusedBeforeAnyCall(t *testing.T) {
	seedValue, _ := testnats.User(t)
	const notEchoed = ": an nkey user seed, which is never AWS configuration (not echoed)"
	config := func(t *testing.T, text string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config")
		require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
		t.Setenv("AWS_CONFIG_FILE", path)
	}
	rows := []struct {
		name  string
		setup func(t *testing.T)
		want  string
	}{
		{"AWS_REGION", func(t *testing.T) { t.Setenv("AWS_REGION", seedValue) }, "AWS_REGION" + notEchoed},
		{"AWS_DEFAULT_REGION", func(t *testing.T) { t.Setenv("AWS_DEFAULT_REGION", seedValue) }, "AWS_DEFAULT_REGION" + notEchoed},
		{"AWS_PROFILE", func(t *testing.T) { t.Setenv("AWS_PROFILE", seedValue) }, "AWS_PROFILE" + notEchoed},
		{"AWS_DEFAULT_PROFILE", func(t *testing.T) { t.Setenv("AWS_DEFAULT_PROFILE", seedValue) }, "AWS_DEFAULT_PROFILE" + notEchoed},
		{"the default profile's region", func(t *testing.T) { config(t, "[default]\nregion = "+seedValue+"\n") },
			`the region of the shared config's profile "default"` + notEchoed},
		{"a named profile's region", func(t *testing.T) {
			config(t, "[profile example]\nregion = "+seedValue+"\n")
			t.Setenv("AWS_PROFILE", "example")
		}, `the region of the shared config's profile "example"` + notEchoed},
		{"a source_profile", func(t *testing.T) {
			config(t, "[profile example]\nregion = "+awsRegion+"\nrole_arn = "+awsRoleARN+"\nsource_profile = "+seedValue+"\n")
			t.Setenv("AWS_PROFILE", "example")
		}, "a profile the shared config names" + notEchoed},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := newFakeAWS(t)
			serve(t, f)
			row.setup(t)
			var out, errs bytes.Buffer
			code := Command(context.Background(), []string{"check", "--store", AWSSecretsManager, "--name", awsSecret},
				&out, &errs, Stores{AWSSecretsManager: AWS})
			require.NotContains(t, out.String()+errs.String(), seedValue, "the seed reached an output")
			require.Equal(t, 2, code, errs.String())
			require.Empty(t, out.String())
			require.Equal(t, "refused: "+awsSecret+": "+row.want+"\n", errs.String())
			require.Empty(t, f.recorded(), "no request carried it")
		})
	}
}

func TestAWSErrorsAreRenderedByCodeOrClassNeverByTheirText(t *testing.T) {
	const canary = "canary-value-7f3e"
	rows := []struct {
		err  error
		want string
	}{
		{&smithy.GenericAPIError{Code: "ValidationException", Message: "got " + canary}, "ValidationException"},
		{&smithy.OperationError{ServiceID: "Secrets Manager", OperationName: "PutSecretValue",
			Err: &smithy.SerializationError{Err: errors.New("SecretString " + canary)}}, "SerializationError"},
		{&smithy.OperationError{ServiceID: "Secrets Manager", OperationName: "GetSecretValue",
			Err: &smithy.DeserializationError{Err: errors.New("invalid character in " + canary)}}, "DeserializationError"},
		{&smithy.OperationError{Err: &smithyhttp.RequestSendError{Err: errors.New("dial " + canary)}}, "RequestSendError"},
		{&smithy.OperationError{Err: fmt.Errorf("wrapped: %w", context.Canceled)}, "canceled"},
		{&smithy.OperationError{Err: fmt.Errorf("wrapped: %w", context.DeadlineExceeded)}, "deadline exceeded"},
		{&smithy.OperationError{Err: fmt.Errorf("get identity: %w", errors.New(canary))}, "errorString"},
	}
	for _, row := range rows {
		got := failed("put-secret-value", awsSecret, row.err).Error()
		require.Equal(t, "put-secret-value "+awsSecret+" failed: "+row.want, got)
		require.NotContains(t, got, canary)
	}
}

func TestPythonDate(t *testing.T) {
	pdt := time.FixedZone("PDT", -7*60*60)
	at := time.Unix(1759190400, 0).UTC()
	require.Equal(t, "2025-09-29 17:00:00-07:00", pythonDate(at, pdt))
	require.Equal(t, "2025-09-29 17:00:00.500000-07:00", pythonDate(at.Add(500*time.Millisecond), pdt))
	require.Equal(t, "2025-09-30 00:00:00.123000+00:00", pythonDate(at.Add(123*time.Millisecond), time.UTC))
}
