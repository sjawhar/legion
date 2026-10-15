package seed

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"
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
		&out, &errs, Stores{AWSSecretsManager: OpenAWS})
	require.Equal(t, 2, code)
	require.Equal(t, "refused: "+awsSecret+": no AWS region: pass --region, or name one in AWS_REGION or the profile\n", errs.String())
	require.Empty(t, f.recorded())
}

// The resolved configuration, and the endpoints the two clients resolve with the SDK's own
// resolvers, are checked as well, so a seed the sources above would all miss, as one the SDK
// assembled would be, is still refused. A clean configuration resolves AWS's own endpoints.
func TestAWSResolvedConfigurationAndEndpointsHoldNoSeed(t *testing.T) {
	seedValue, _ := testnats.User(t)
	ctx := context.Background()
	withSeed := aws.String("https://example.invalid/" + seedValue)
	require.Equal(t, "the resolved AWS configuration's AppID", configSeed(aws.Config{AppID: seedValue}))
	require.Equal(t, "the resolved AWS configuration's BaseEndpoint", configSeed(aws.Config{BaseEndpoint: withSeed}))
	require.Empty(t, configSeed(aws.Config{Region: awsRegion, AppID: "example-app"}))

	secrets := func(endpoint *string) secretsmanager.Options {
		return secretsmanager.Options{Region: awsRegion, BaseEndpoint: endpoint, EndpointResolverV2: secretsmanager.NewDefaultEndpointResolverV2()}
	}
	identity := func(endpoint *string) sts.Options {
		return sts.Options{Region: awsRegion, BaseEndpoint: endpoint, EndpointResolverV2: sts.NewDefaultEndpointResolverV2()}
	}
	require.Equal(t, "the Secrets Manager endpoint", endpointSeed(ctx, secrets(withSeed), identity(nil)))
	require.Equal(t, "the STS endpoint", endpointSeed(ctx, secrets(nil), identity(withSeed)))
	require.Empty(t, endpointSeed(ctx, secrets(nil), identity(nil)))
}

// configCase is the AWS configuration one row of TestAWSASeedInTheAWSConfigurationIsRefusedBeforeAnyCall
// builds: the fake every endpoint points at, and a directory for the row's files.
type configCase struct {
	t   *testing.T
	f   *fakeAWS
	url string // the fake's URL, http://127.0.0.1:<port>
	dir string
}

func (c configCase) env(pairs ...string) {
	for i := 0; i+1 < len(pairs); i += 2 {
		c.t.Setenv(pairs[i], pairs[i+1])
	}
}

// file writes text to name in the row's directory and returns its path.
func (c configCase) file(name, text string) string {
	c.t.Helper()
	path := filepath.Join(c.dir, name)
	require.NoError(c.t, os.WriteFile(path, []byte(text), 0o600))
	return path
}

func (c configCase) config(text string) { c.env("AWS_CONFIG_FILE", c.file("config", text)) }

func (c configCase) credentials(text string) {
	c.env("AWS_SHARED_CREDENTIALS_FILE", c.file("credentials", text))
}

// noEnvCredentials clears the static credentials serve gives the environment, so the profile or
// the provider a row configures supplies them.
func (c configCase) noEnvCredentials() { c.env("AWS_ACCESS_KEY_ID", "", "AWS_SECRET_ACCESS_KEY", "") }

// withRole is a profile "example" assuming awsRoleARN from a profile "base" with static keys.
func withRole(extra string) string {
	return "[profile example]\nrole_arn = " + awsRoleARN + "\nsource_profile = base\n" + extra +
		"\n[profile base]\naws_access_key_id = AKIDEXAMPLE\naws_secret_access_key = example-signing-key\n"
}

// caBundle is a PEM certificate, as a CA bundle holds; the endpoints are plain http on loopback,
// so the SDK loads it and never uses it.
func caBundle(t *testing.T) string {
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
}

// percentEncoded spells every byte of text as %XX.
func percentEncoded(text string) string {
	var b strings.Builder
	for i := range len(text) {
		fmt.Fprintf(&b, "%%%02X", text[i])
	}
	return b.String()
}

// Every string the AWS configuration carries into a client is checked for a seed before any client
// is built or any credential retrieved: the environment the SDK reads (every AWS_ variable and the
// Lambda trace id), the shared config and credentials files' paths, every value of every profile
// the loader reads, source profiles included, and the resolved endpoints, in every part of their
// URLs. A row puts a seed in one place; the refusal names that place, never the value, and no
// request reaches the fake. Each row's clean subtest is its control: the same configuration with a
// clean value in place of the seed is not refused, and reaches the request unless the row says the
// SDK stops before any request with a clean value too. --region is not given.
func TestAWSASeedInTheAWSConfigurationIsRefusedBeforeAnyCall(t *testing.T) {
	seedValue, _ := testnats.User(t)
	pemText := caBundle(t)
	const notEchoed = ": an nkey seed, which is never AWS configuration (not echoed)"
	profile := func(name, key string) string { return fmt.Sprintf("the shared config's profile %q: %s", name, key) }
	// split cuts v in two DNS labels, as a host a seed is split across.
	split := func(v string) string { return v[:len(v)/2] + "." + v[len(v)/2:] }
	// shown reports whether text shows the seed in any spelling a reader takes for it: in either
	// case, and with '.', '-', '_' and whitespace removed.
	shown := func(text string) bool {
		joined := strings.Map(func(r rune) rune {
			if strings.ContainsRune(".-_ \t\r\n", r) {
				return -1
			}
			return unicode.ToUpper(r)
		}, text)
		return strings.Contains(joined, seedValue)
	}
	rows := []struct {
		name  string
		args  []string // check's flags beyond --store and --name
		setup func(c configCase, v string)
		want  string // the source the refusal names
		clean string // the value of the control run
		// reach is whether the control run reaches the fake, or runs a credential_process. It is
		// false where the SDK stops before any request with that configuration, clean or not.
		reach bool
	}{
		// The environment.
		{name: "AWS_REGION", setup: func(c configCase, v string) { c.env("AWS_REGION", v) }, want: "AWS_REGION", clean: awsRegion, reach: true},
		{name: "AWS_REGION, lowercase", setup: func(c configCase, v string) { c.env("AWS_REGION", strings.ToLower(v)) },
			want: "AWS_REGION", clean: awsRegion, reach: true},
		{name: "AWS_DEFAULT_REGION", setup: func(c configCase, v string) { c.env("AWS_REGION", "", "AWS_DEFAULT_REGION", v) },
			want: "AWS_DEFAULT_REGION", clean: awsRegion, reach: true},
		{name: "AWS_PROFILE", setup: func(c configCase, v string) {
			c.config("[profile " + v + "]\nregion = " + awsRegion + "\n")
			c.env("AWS_PROFILE", v)
		}, want: "AWS_PROFILE", clean: "example", reach: true},
		{name: "AWS_DEFAULT_PROFILE", setup: func(c configCase, v string) {
			c.config("[profile " + v + "]\nregion = " + awsRegion + "\n")
			c.env("AWS_DEFAULT_PROFILE", v)
		}, want: "AWS_DEFAULT_PROFILE", clean: "example", reach: true},
		{name: "AWS_CONFIG_FILE", setup: func(c configCase, v string) {
			c.env("AWS_CONFIG_FILE", c.file(v, "[default]\nregion = "+awsRegion+"\n"))
		}, want: "AWS_CONFIG_FILE", clean: "config", reach: true},
		{name: "AWS_SHARED_CREDENTIALS_FILE", setup: func(c configCase, v string) {
			c.noEnvCredentials()
			c.env("AWS_SHARED_CREDENTIALS_FILE", c.file(v, "[default]\naws_access_key_id = AKIDEXAMPLE\naws_secret_access_key = example-signing-key\n"))
		}, want: "AWS_SHARED_CREDENTIALS_FILE", clean: "credentials", reach: true},
		{name: "HOME, the files' default directory", setup: func(c configCase, v string) {
			home := filepath.Join(c.dir, v)
			require.NoError(c.t, os.MkdirAll(filepath.Join(home, ".aws"), 0o700))
			require.NoError(c.t, os.WriteFile(filepath.Join(home, ".aws", "config"), []byte("[default]\nregion = "+awsRegion+"\n"), 0o600))
			c.env("HOME", home, "AWS_CONFIG_FILE", "", "AWS_SHARED_CREDENTIALS_FILE", "")
		}, want: "the shared config file's default path", clean: "home", reach: true},
		{name: "AWS_ENDPOINT_URL's path", setup: func(c configCase, v string) { c.env("AWS_ENDPOINT_URL", c.url+"/"+v) },
			want: "AWS_ENDPOINT_URL", clean: "clean-path", reach: true},
		{name: "AWS_ENDPOINT_URL's path, lowercase", setup: func(c configCase, v string) {
			c.env("AWS_ENDPOINT_URL", c.url+"/"+strings.ToLower(v))
		}, want: "AWS_ENDPOINT_URL", clean: "clean-path", reach: true},
		// A host that is not loopback is refused for plain http, before any request, so the control
		// stops there too; the binary check sends the same shape over https through a proxy.
		{name: "AWS_ENDPOINT_URL's host, split across DNS labels", setup: func(c configCase, v string) {
			c.env("AWS_ENDPOINT_URL", "http://"+split(v)+".example.test")
		}, want: "AWS_ENDPOINT_URL", clean: "cleanlabels"},
		{name: "AWS_ENDPOINT_URL's path, percent-encoded", setup: func(c configCase, v string) {
			c.env("AWS_ENDPOINT_URL", c.url+"/"+percentEncoded(v))
		}, want: "AWS_ENDPOINT_URL", clean: "clean-path", reach: true},
		{name: "AWS_ENDPOINT_URL's query key", setup: func(c configCase, v string) { c.env("AWS_ENDPOINT_URL", c.url+"/?"+v+"=1") },
			want: "AWS_ENDPOINT_URL", clean: "key", reach: true},
		{name: "AWS_ENDPOINT_URL's query value", setup: func(c configCase, v string) { c.env("AWS_ENDPOINT_URL", c.url+"/?key="+v) },
			want: "AWS_ENDPOINT_URL", clean: "value", reach: true},
		{name: "AWS_ENDPOINT_URL's fragment", setup: func(c configCase, v string) { c.env("AWS_ENDPOINT_URL", c.url+"/#"+v) },
			want: "AWS_ENDPOINT_URL", clean: "fragment", reach: true},
		{name: "AWS_ENDPOINT_URL's user", setup: func(c configCase, v string) {
			c.env("AWS_ENDPOINT_URL", "http://"+v+"@"+strings.TrimPrefix(c.url, "http://")+"/")
		}, want: "AWS_ENDPOINT_URL", clean: "user", reach: true},
		{name: "AWS_ENDPOINT_URL's password", setup: func(c configCase, v string) {
			c.env("AWS_ENDPOINT_URL", "http://user:"+v+"@"+strings.TrimPrefix(c.url, "http://")+"/")
		}, want: "AWS_ENDPOINT_URL", clean: "password", reach: true},
		{name: "AWS_ENDPOINT_URL_SECRETS_MANAGER's path", setup: func(c configCase, v string) {
			c.env("AWS_ENDPOINT_URL_SECRETS_MANAGER", c.url+"/"+v)
		}, want: "AWS_ENDPOINT_URL_SECRETS_MANAGER", clean: "clean-path", reach: true},
		{name: "AWS_ENDPOINT_URL_STS's path", args: []string{"--expect-account", awsAccount},
			setup: func(c configCase, v string) { c.env("AWS_ENDPOINT_URL_STS", c.url+"/"+v) },
			want:  "AWS_ENDPOINT_URL_STS", clean: "clean-path", reach: true},
		{name: "AWS_ACCESS_KEY_ID", setup: func(c configCase, v string) { c.env("AWS_ACCESS_KEY_ID", v) },
			want: "AWS_ACCESS_KEY_ID", clean: "AKIDCLEAN", reach: true},
		{name: "AWS_SECRET_ACCESS_KEY", setup: func(c configCase, v string) { c.env("AWS_SECRET_ACCESS_KEY", v) },
			want: "AWS_SECRET_ACCESS_KEY", clean: "clean-signing-key", reach: true},
		{name: "AWS_SESSION_TOKEN", setup: func(c configCase, v string) { c.env("AWS_SESSION_TOKEN", v) },
			want: "AWS_SESSION_TOKEN", clean: "example-token", reach: true},
		{name: "AWS_SDK_UA_APP_ID", setup: func(c configCase, v string) { c.env("AWS_SDK_UA_APP_ID", v) },
			want: "AWS_SDK_UA_APP_ID", clean: "example-app", reach: true},
		{name: "AWS_MAX_ATTEMPTS", setup: func(c configCase, v string) { c.env("AWS_MAX_ATTEMPTS", v) },
			want: "AWS_MAX_ATTEMPTS", clean: "2", reach: true},
		{name: "AWS_CA_BUNDLE", setup: func(c configCase, v string) { c.env("AWS_CA_BUNDLE", c.file(v, pemText)) },
			want: "AWS_CA_BUNDLE", clean: "ca.pem", reach: true},
		{name: "AWS_ROLE_ARN", setup: func(c configCase, v string) {
			c.noEnvCredentials()
			c.env("AWS_ROLE_ARN", "arn:aws:iam::"+awsAccount+":role/"+v, "AWS_WEB_IDENTITY_TOKEN_FILE", c.file("token", "example-token"))
		}, want: "AWS_ROLE_ARN", clean: "example", reach: true},
		{name: "AWS_ROLE_SESSION_NAME", setup: func(c configCase, v string) {
			c.noEnvCredentials()
			c.env("AWS_ROLE_ARN", awsRoleARN, "AWS_ROLE_SESSION_NAME", v, "AWS_WEB_IDENTITY_TOKEN_FILE", c.file("token", "example-token"))
		}, want: "AWS_ROLE_SESSION_NAME", clean: "example-session", reach: true},
		{name: "AWS_WEB_IDENTITY_TOKEN_FILE", setup: func(c configCase, v string) {
			c.noEnvCredentials()
			c.env("AWS_ROLE_ARN", awsRoleARN, "AWS_WEB_IDENTITY_TOKEN_FILE", c.file(v, "example-token"))
		}, want: "AWS_WEB_IDENTITY_TOKEN_FILE", clean: "token", reach: true},
		{name: "AWS_CONTAINER_CREDENTIALS_FULL_URI", setup: func(c configCase, v string) {
			c.noEnvCredentials()
			c.env("AWS_CONTAINER_CREDENTIALS_FULL_URI", c.url+"/"+v)
		}, want: "AWS_CONTAINER_CREDENTIALS_FULL_URI", clean: "credentials", reach: true},
		{name: "AWS_CONTAINER_AUTHORIZATION_TOKEN", setup: func(c configCase, v string) {
			c.noEnvCredentials()
			c.env("AWS_CONTAINER_CREDENTIALS_FULL_URI", c.url+"/credentials", "AWS_CONTAINER_AUTHORIZATION_TOKEN", v)
		}, want: "AWS_CONTAINER_AUTHORIZATION_TOKEN", clean: "example-token", reach: true},
		{name: "AWS_EC2_METADATA_SERVICE_ENDPOINT", setup: func(c configCase, v string) {
			c.noEnvCredentials()
			c.env("AWS_EC2_METADATA_DISABLED", "false", "AWS_EC2_METADATA_SERVICE_ENDPOINT", c.url+"/"+v)
		}, want: "AWS_EC2_METADATA_SERVICE_ENDPOINT", clean: "metadata", reach: true},
		{name: "_X_AMZN_TRACE_ID, on Lambda", setup: func(c configCase, v string) {
			c.env("AWS_LAMBDA_FUNCTION_NAME", "example", "_X_AMZN_TRACE_ID", "Root="+v)
		}, want: "_X_AMZN_TRACE_ID", clean: "1-example", reach: true},
		{name: "an AWS_ variable the SDK does not read", setup: func(c configCase, v string) { c.env("AWS_EXAMPLE_UNREAD", v) },
			want: "AWS_EXAMPLE_UNREAD", clean: "example", reach: true},

		// The shared config and credentials files.
		{name: "the default profile's region", setup: func(c configCase, v string) {
			c.env("AWS_REGION", "")
			c.config("[default]\nregion = " + v + "\n")
		}, want: profile("default", "region"), clean: awsRegion, reach: true},
		{name: "a named profile's region", setup: func(c configCase, v string) {
			c.env("AWS_REGION", "", "AWS_PROFILE", "example")
			c.config("[profile example]\nregion = " + v + "\n")
		}, want: profile("example", "region"), clean: awsRegion, reach: true},
		{name: "a source_profile that does not exist", setup: func(c configCase, v string) {
			c.env("AWS_PROFILE", "example")
			c.config("[profile example]\nrole_arn = " + awsRoleARN + "\nsource_profile = " + v + "\n")
		}, want: "a profile the shared config names", clean: "missing"},
		{name: "a source_profile that exists", setup: func(c configCase, v string) {
			c.noEnvCredentials()
			c.env("AWS_PROFILE", "example")
			c.config("[profile example]\nrole_arn = " + awsRoleARN + "\nsource_profile = " + v + "\n\n[profile " + v +
				"]\naws_access_key_id = AKIDEXAMPLE\naws_secret_access_key = example-signing-key\n")
		}, want: profile("example", "source_profile"), clean: "base", reach: true},
		{name: "a source profile's own value", setup: func(c configCase, v string) {
			c.noEnvCredentials()
			c.env("AWS_PROFILE", "example")
			c.config("[profile example]\nrole_arn = " + awsRoleARN + "\nsource_profile = base\n\n[profile base]\naws_access_key_id = " + v +
				"\naws_secret_access_key = example-signing-key\n")
		}, want: profile("base", "aws_access_key_id"), clean: "AKIDEXAMPLE", reach: true},
		{name: "role_arn", setup: func(c configCase, v string) {
			c.noEnvCredentials()
			c.env("AWS_PROFILE", "example")
			c.config("[profile example]\nrole_arn = arn:aws:iam::" + awsAccount + ":role/" + v + "\nsource_profile = base\n\n[profile base]\n" +
				"aws_access_key_id = AKIDEXAMPLE\naws_secret_access_key = example-signing-key\n")
		}, want: profile("example", "role_arn"), clean: "example", reach: true},
		{name: "role_session_name", setup: func(c configCase, v string) {
			c.noEnvCredentials()
			c.env("AWS_PROFILE", "example")
			c.config(withRole("role_session_name = " + v + "\n"))
		}, want: profile("example", "role_session_name"), clean: "example-session", reach: true},
		{name: "external_id", setup: func(c configCase, v string) {
			c.noEnvCredentials()
			c.env("AWS_PROFILE", "example")
			c.config(withRole("external_id = " + v + "\n"))
		}, want: profile("example", "external_id"), clean: "example-external-id", reach: true},
		{name: "mfa_serial", setup: func(c configCase, v string) {
			c.noEnvCredentials()
			c.env("AWS_PROFILE", "example")
			c.config(withRole("mfa_serial = arn:aws:iam::" + awsAccount + ":mfa/" + v + "\n"))
		}, want: profile("example", "mfa_serial"), clean: "example"},
		{name: "credential_source", setup: func(c configCase, v string) {
			c.env("AWS_PROFILE", "example")
			c.config("[profile example]\nrole_arn = " + awsRoleARN + "\ncredential_source = " + v + "\n")
		}, want: profile("example", "credential_source"), clean: "Environment", reach: true},
		{name: "credential_process", setup: func(c configCase, v string) {
			c.noEnvCredentials()
			process := c.file("process", "#!/bin/sh\nprintf '%s' \"$*\" > '"+filepath.Join(c.dir, "process-ran")+"'\nexit 1\n")
			require.NoError(c.t, os.Chmod(process, 0o700))
			c.config("[default]\ncredential_process = " + process + " " + v + "\n")
		}, want: profile("default", "credential_process"), clean: "clean", reach: true},
		{name: "web_identity_token_file", setup: func(c configCase, v string) {
			c.noEnvCredentials()
			c.config("[default]\nrole_arn = " + awsRoleARN + "\nweb_identity_token_file = " + c.file(v, "example-token") + "\n")
		}, want: profile("default", "web_identity_token_file"), clean: "token", reach: true},
		{name: "sso_session", setup: func(c configCase, v string) {
			c.noEnvCredentials()
			c.config("[default]\nsso_session = " + v + "\nsso_account_id = " + awsAccount + "\nsso_role_name = example\n\n[sso-session " + v +
				"]\nsso_region = " + awsRegion + "\nsso_start_url = https://example.invalid/start\n")
		}, want: profile("default", "sso_session"), clean: "example"},
		{name: "an sso-session's sso_region", setup: func(c configCase, v string) {
			c.noEnvCredentials()
			c.config("[default]\nsso_session = example\nsso_account_id = " + awsAccount + "\nsso_role_name = example\n\n[sso-session example]\n" +
				"sso_region = " + v + "\nsso_start_url = https://example.invalid/start\n")
		}, want: profile("default", "the sso_region of its sso-session"), clean: awsRegion},
		{name: "an sso-session's sso_start_url", setup: func(c configCase, v string) {
			c.noEnvCredentials()
			c.config("[default]\nsso_session = example\nsso_account_id = " + awsAccount + "\nsso_role_name = example\n\n[sso-session example]\n" +
				"sso_region = " + awsRegion + "\nsso_start_url = https://example.invalid/" + v + "\n")
		}, want: profile("default", "the sso_start_url of its sso-session"), clean: "start"},
		{name: "endpoint_url's path", setup: func(c configCase, v string) {
			c.env("AWS_ENDPOINT_URL", "")
			c.config("[default]\nendpoint_url = " + c.url + "/" + v + "\n")
		}, want: profile("default", "endpoint_url"), clean: "clean-path", reach: true},
		{name: "a services section's Secrets Manager endpoint_url", setup: func(c configCase, v string) {
			c.config("[default]\nservices = example\n\n[services example]\nsecrets_manager =\n  endpoint_url = " + c.url + "/" + v + "\n")
		}, want: profile("default", "the secrets_manager endpoint_url of its services section"), clean: "clean-path", reach: true},
		{name: "a services section's STS endpoint_url", args: []string{"--expect-account", awsAccount}, setup: func(c configCase, v string) {
			c.env("AWS_ENDPOINT_URL_STS", "")
			c.config("[default]\nservices = example\n\n[services example]\nsts =\n  endpoint_url = " + c.url + "/" + v + "\n")
		}, want: profile("default", "the sts endpoint_url of its services section"), clean: "clean-path", reach: true},
		{name: "aws_access_key_id", setup: func(c configCase, v string) {
			c.noEnvCredentials()
			c.credentials("[default]\naws_access_key_id = " + v + "\naws_secret_access_key = example-signing-key\n")
		}, want: profile("default", "aws_access_key_id"), clean: "AKIDEXAMPLE", reach: true},
		{name: "aws_session_token", setup: func(c configCase, v string) {
			c.noEnvCredentials()
			c.credentials("[default]\naws_access_key_id = AKIDEXAMPLE\naws_secret_access_key = example-signing-key\naws_session_token = " + v + "\n")
		}, want: profile("default", "aws_session_token"), clean: "example-token", reach: true},
		{name: "sdk_ua_app_id", setup: func(c configCase, v string) { c.config("[default]\nsdk_ua_app_id = " + v + "\n") },
			want: profile("default", "sdk_ua_app_id"), clean: "example-app", reach: true},
		{name: "ca_bundle", setup: func(c configCase, v string) { c.config("[default]\nca_bundle = " + c.file(v, pemText) + "\n") },
			want: profile("default", "ca_bundle"), clean: "ca.pem", reach: true},
		{name: "max_attempts", setup: func(c configCase, v string) { c.config("[default]\nmax_attempts = " + v + "\n") },
			want: "a value the shared config or credentials file holds", clean: "2", reach: true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			for _, run := range []struct {
				name  string
				value string
			}{{"seed", seedValue}, {"clean", row.clean}} {
				t.Run(run.name, func(t *testing.T) {
					f := newFakeAWS(t)
					server := serve(t, f)
					c := configCase{t: t, f: f, url: server.URL, dir: t.TempDir()}
					home := filepath.Join(c.dir, "home")
					require.NoError(t, os.Mkdir(home, 0o700))
					// Every endpoint a credential provider may call is the fake, so no run reaches AWS.
					c.env("HOME", home, "AWS_REGION", awsRegion, "AWS_ENDPOINT_URL_STS", server.URL,
						"AWS_ENDPOINT_URL_SSO", server.URL, "AWS_ENDPOINT_URL_SSO_OIDC", server.URL)
					row.setup(c, run.value)
					var out, errs bytes.Buffer
					args := append([]string{"check", "--store", AWSSecretsManager, "--name", awsSecret}, row.args...)
					code := Command(context.Background(), args, &out, &errs, Stores{AWSSecretsManager: OpenAWS})
					_, statErr := os.Stat(filepath.Join(c.dir, "process-ran"))
					ran := statErr == nil
					if run.name == "clean" {
						require.NotContains(t, errs.String(), notEchoed, "a clean configuration is not refused as a seed")
						if row.reach {
							require.True(t, f.requests() > 0 || ran, "the clean configuration reaches the request; stderr %q", errs.String())
						}
						f.forgetUnexpected()
						return
					}
					require.False(t, shown(out.String()+errs.String()), "the seed reached an output: %q", errs.String())
					require.Zero(t, f.requests(), "no request reached the fake")
					require.False(t, ran, "no credential_process ran")
					require.Equal(t, 2, code, errs.String())
					require.Empty(t, out.String())
					require.Equal(t, "refused: "+awsSecret+": "+row.want+notEchoed+"\n", errs.String())
				})
			}
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
		got := awsFailed("put-secret-value", awsSecret, row.err).Error()
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
