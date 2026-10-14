package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

const (
	awsSecret  = "example/envoy/nats-nkey-seed-example-host-a"
	awsAccount = "000000000000"
	awsRegion  = "us-east-1"
	awsRoleARN = "arn:aws:iam::" + awsAccount + ":role/example"
)

type awsVersion struct {
	id, value string
	stages    []string
}

type awsFailure struct{ code, message string }

// fakeAWS is Secrets Manager (JSON 1.1, X-Amz-Target) and STS (query) on one endpoint, holding the
// one secret awsSecret. It answers only the requests the store makes, each checked field by field:
// any other request, or a field it does not expect, is recorded and fails the test that made it.
type fakeAWS struct {
	t  *testing.T
	mu sync.Mutex

	account   string
	exists    bool
	deletedAt float64 // seconds since the epoch; 0 for a secret not scheduled for deletion
	versions  []awsVersion

	calls      []string // each call's name, in order
	tokens     []string // each PutSecretValue's ClientRequestToken
	unexpected []string

	failures     map[string]awsFailure // the error a call answers, by its name
	failAfterPut map[string]awsFailure // the error a call answers once a version was written
	pageSize     int                   // versions per ListSecretVersionIds page; 0 for all at once
	repeatToken  bool                  // every ListSecretVersionIds page names the same NextToken
	putStages    []string              // the stages PutSecretValue answers; nil for AWSCURRENT
	loseAnswers  int                   // how many PutSecretValue answers are lost after the write
	racing       *awsVersion           // another writer's version, landing with the put
	echoRequests bool                  // every error message quotes the request body
	// generating is set for a generate run, the one mode that lists versions: check and verify make
	// only DescribeSecret and GetSecretValue, and a listing from either is a request the fake does
	// not expect.
	generating bool
	// Eventual consistency: after the write, the next staleLists listings leave out the written
	// version, and the next staleGets reads find no AWSCURRENT version, as a read just after a
	// write may.
	staleLists, staleGets int
}

func newFakeAWS(t *testing.T) *fakeAWS {
	f := &fakeAWS{t: t, account: awsAccount, exists: true}
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, what := range f.unexpected {
			t.Errorf("the SDK sent a request the fake does not expect: %s", what)
		}
	})
	return f
}

func (f *fakeAWS) holding(values ...string) *fakeAWS {
	for i, v := range values {
		stages := []string(nil)
		if i == len(values)-1 {
			stages = []string{"AWSCURRENT"}
		}
		f.versions = append(f.versions, awsVersion{id: fmt.Sprintf("v%d", i+1), value: v, stages: stages})
	}
	return f
}

func (f *fakeAWS) written() bool { return len(f.tokens) > 0 }

func (f *fakeAWS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.reject(w, "unreadable body: "+err.Error())
		return
	}
	switch {
	case r.Method != http.MethodPost || r.URL.Path != "/":
		f.reject(w, r.Method+" "+r.URL.Path)
	case !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/"):
		f.reject(w, "a request not signed with the test's credentials")
	case r.Header.Get("X-Amz-Target") != "":
		target, ok := strings.CutPrefix(r.Header.Get("X-Amz-Target"), "secretsmanager.")
		if !ok || r.Header.Get("Content-Type") != "application/x-amz-json-1.1" {
			f.reject(w, "X-Amz-Target "+r.Header.Get("X-Amz-Target")+" with Content-Type "+r.Header.Get("Content-Type"))
			return
		}
		f.secretsManager(w, target, body)
	case r.Header.Get("Content-Type") == "application/x-www-form-urlencoded":
		f.sts(w, body)
	default:
		f.reject(w, "a request of neither protocol")
	}
}

// reject records a request the store should never make and answers it with an error.
func (f *fakeAWS) reject(w http.ResponseWriter, what string) {
	f.unexpected = append(f.unexpected, what)
	f.jsonError(w, awsFailure{"UnexpectedRequest", "the fake does not answer this request"}, nil)
}

func (f *fakeAWS) jsonError(w http.ResponseWriter, failure awsFailure, body []byte) {
	message := failure.message
	if f.echoRequests {
		message += ": " + string(body)
	}
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"__type": failure.code, "message": message})
}

func (f *fakeAWS) answer(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	_ = json.NewEncoder(w).Encode(v)
}

// decode reads body into v, refusing any field v does not declare.
func decode(body []byte, v any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	return decoder.Decode(v)
}

func (f *fakeAWS) secretsManager(w http.ResponseWriter, call string, body []byte) {
	f.calls = append(f.calls, call)
	if failure, ok := f.failures[call]; ok {
		f.jsonError(w, failure, body)
		return
	}
	if failure, ok := f.failAfterPut[call]; ok && f.written() {
		f.jsonError(w, failure, body)
		return
	}
	arn := "arn:aws:secretsmanager:" + awsRegion + ":" + awsAccount + ":secret:" + awsSecret + "-AbCdEf"
	switch call {
	case "DescribeSecret":
		var in struct{ SecretId string }
		if err := decode(body, &in); err != nil || in.SecretId != awsSecret {
			f.reject(w, fmt.Sprintf("DescribeSecret %s (%v)", body, err))
			return
		}
		if !f.exists {
			f.jsonError(w, awsFailure{"ResourceNotFoundException", "Secrets Manager can't find the specified secret."}, body)
			return
		}
		out := map[string]any{"ARN": arn, "Name": awsSecret}
		stages := map[string][]string{}
		for _, v := range f.versions {
			if len(v.stages) > 0 {
				stages[v.id] = v.stages
			}
		}
		if len(stages) > 0 {
			out["VersionIdsToStages"] = stages
		}
		if f.deletedAt != 0 {
			out["DeletedDate"] = f.deletedAt
		}
		f.answer(w, out)
	case "ListSecretVersionIds":
		if !f.generating {
			f.reject(w, "ListSecretVersionIds from check or verify, which make only DescribeSecret and GetSecretValue")
			return
		}
		var in struct {
			SecretId          string
			IncludeDeprecated *bool
			NextToken         *string
		}
		if err := decode(body, &in); err != nil || in.SecretId != awsSecret || in.IncludeDeprecated == nil || !*in.IncludeDeprecated {
			f.reject(w, fmt.Sprintf("ListSecretVersionIds %s (%v): every version, deprecated ones included, is asked for", body, err))
			return
		}
		if f.repeatToken {
			if lists := len(slices.DeleteFunc(slices.Clone(f.calls), func(c string) bool { return c != "ListSecretVersionIds" })); lists > 10 {
				f.reject(w, "a repeated page token was followed")
				return
			}
			f.answer(w, map[string]any{"ARN": arn, "Name": awsSecret, "Versions": []any{}, "NextToken": "page-2"})
			return
		}
		start := 0
		if in.NextToken != nil {
			if _, err := fmt.Sscanf(*in.NextToken, "page-%d", &start); err != nil {
				f.reject(w, "NextToken "+*in.NextToken)
				return
			}
		}
		versions := slices.Clone(f.versions)
		if f.written() && f.staleLists > 0 {
			f.staleLists--
			versions = slices.DeleteFunc(versions, func(v awsVersion) bool { return v.id == f.tokens[0] })
		}
		if f.racing != nil && f.written() {
			versions = append(versions, *f.racing)
		}
		end := len(versions)
		if f.pageSize > 0 {
			end = min(start+f.pageSize, len(versions))
		}
		page := []map[string]any{}
		for _, v := range versions[start:end] {
			entry := map[string]any{"VersionId": v.id}
			if len(v.stages) > 0 {
				entry["VersionStages"] = v.stages
			}
			page = append(page, entry)
		}
		out := map[string]any{"ARN": arn, "Name": awsSecret, "Versions": page}
		if end < len(versions) {
			out["NextToken"] = fmt.Sprintf("page-%d", end)
		}
		f.answer(w, out)
	case "PutSecretValue":
		var in struct{ SecretId, SecretString, ClientRequestToken string }
		if err := decode(body, &in); err != nil || in.SecretId != awsSecret || in.SecretString == "" ||
			len(in.ClientRequestToken) < 32 || len(in.ClientRequestToken) > 64 {
			f.reject(w, fmt.Sprintf("PutSecretValue (%v): a SecretString and a ClientRequestToken of 32 to 64 characters, nothing else", err))
			return
		}
		f.tokens = append(f.tokens, in.ClientRequestToken)
		// Secrets Manager names the version by the token, and a retry with the token and the same
		// value writes nothing more.
		known := slices.IndexFunc(f.versions, func(v awsVersion) bool { return v.id == in.ClientRequestToken })
		if known >= 0 && f.versions[known].value != in.SecretString {
			f.jsonError(w, awsFailure{"ResourceExistsException", "a version with this token holds another value"}, body)
			return
		}
		if known < 0 {
			for i := range f.versions {
				f.versions[i].stages = nil
			}
			f.versions = append(f.versions, awsVersion{id: in.ClientRequestToken, value: in.SecretString, stages: []string{"AWSCURRENT"}})
		}
		if f.loseAnswers > 0 {
			f.loseAnswers--
			hijacked, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				f.t.Errorf("hijack: %v", err)
				return
			}
			_ = hijacked.Close()
			return
		}
		stages := f.putStages
		if stages == nil {
			stages = []string{"AWSCURRENT"}
		}
		f.answer(w, map[string]any{"ARN": arn, "Name": awsSecret, "VersionId": in.ClientRequestToken, "VersionStages": stages})
	case "GetSecretValue":
		var in struct{ SecretId string }
		if err := decode(body, &in); err != nil || in.SecretId != awsSecret {
			f.reject(w, fmt.Sprintf("GetSecretValue %s (%v): the version served by default, by name alone", body, err))
			return
		}
		if f.deletedAt != 0 {
			f.jsonError(w, awsFailure{"InvalidRequestException", "the secret is scheduled for deletion"}, body)
			return
		}
		if f.written() && f.staleGets > 0 {
			f.staleGets--
			f.jsonError(w, awsFailure{"ResourceNotFoundException", "no version has the staging label AWSCURRENT"}, body)
			return
		}
		for _, v := range f.versions {
			if slices.Contains(v.stages, "AWSCURRENT") {
				f.answer(w, map[string]any{
					"ARN": arn, "Name": awsSecret, "VersionId": v.id, "SecretString": v.value,
					"VersionStages": v.stages, "CreatedDate": 1.7591904e9,
				})
				return
			}
		}
		f.jsonError(w, awsFailure{"ResourceNotFoundException", "no version has the staging label AWSCURRENT"}, body)
	default:
		f.calls = f.calls[:len(f.calls)-1]
		f.reject(w, "secretsmanager."+call)
	}
}

func (f *fakeAWS) sts(w http.ResponseWriter, body []byte) {
	form, err := url.ParseQuery(string(body))
	if err != nil {
		f.reject(w, fmt.Sprintf("STS %s (%v)", body, err))
		return
	}
	// AssumeRole is a profile's credential source, which the fake only ever refuses, with the
	// failure the test names: a test never needs a role's credentials.
	call := form.Get("Action")
	switch {
	case form.Encode() == "Action=GetCallerIdentity&Version=2011-06-15":
	case call == "AssumeRole" && form.Get("Version") == "2011-06-15" && form.Get("RoleArn") == awsRoleARN &&
		form.Get("RoleSessionName") != "" && f.failures["AssumeRole"] != (awsFailure{}):
	default:
		f.reject(w, fmt.Sprintf("STS %s", body))
		return
	}
	f.calls = append(f.calls, call)
	w.Header().Set("Content-Type", "text/xml")
	if failure, ok := f.failures[call]; ok {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><Error><Type>Sender</Type>`+
			`<Code>%s</Code><Message>%s</Message></Error><RequestId>00000000-0000-0000-0000-000000000000</RequestId></ErrorResponse>`,
			failure.code, failure.message)
		return
	}
	fmt.Fprintf(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult>`+
		`<Arn>arn:aws:iam::%[1]s:user/example</Arn><UserId>AIDAEXAMPLE</UserId><Account>%[1]s</Account>`+
		`</GetCallerIdentityResult><ResponseMetadata><RequestId>00000000-0000-0000-0000-000000000000</RequestId>`+
		`</ResponseMetadata></GetCallerIdentityResponse>`, f.account)
}

// values is every secret value the fake holds or was sent: none may reach an output.
func (f *fakeAWS) values() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []string
	for _, v := range f.versions {
		all = append(all, v.value)
	}
	if f.racing != nil {
		all = append(all, f.racing.value)
	}
	return all
}

func (f *fakeAWS) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// serve starts f and points the AWS configuration of the process at it, as an operator's
// environment does: AWS_ENDPOINT_URL, which the SDK takes as every client's BaseEndpoint, and
// static credentials, with no profile, no shared file and no instance metadata.
func serve(t *testing.T, f *fakeAWS) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	dir := t.TempDir()
	for key, value := range map[string]string{
		"AWS_ENDPOINT_URL":                    server.URL,
		"AWS_ENDPOINT_URL_SECRETS_MANAGER":    "",
		"AWS_ENDPOINT_URL_STS":                "",
		"AWS_IGNORE_CONFIGURED_ENDPOINT_URLS": "",
		"AWS_ACCESS_KEY_ID":                   "AKIDEXAMPLE",
		"AWS_SECRET_ACCESS_KEY":               "example-signing-key",
		"AWS_SESSION_TOKEN":                   "",
		"AWS_PROFILE":                         "",
		"AWS_DEFAULT_PROFILE":                 "",
		"AWS_CONFIG_FILE":                     filepath.Join(dir, "config"),
		"AWS_SHARED_CREDENTIALS_FILE":         filepath.Join(dir, "credentials"),
		"AWS_REGION":                          "",
		"AWS_DEFAULT_REGION":                  "",
		"AWS_EC2_METADATA_DISABLED":           "true",
		"AWS_MAX_ATTEMPTS":                    "",
		"AWS_RETRY_MODE":                      "",
	} {
		t.Setenv(key, value)
	}
	return server
}

// runAWS runs the command with the AWS store, against f, in the test's region.
func runAWS(t *testing.T, f *fakeAWS, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errs bytes.Buffer
	f.mu.Lock()
	f.generating = len(args) > 0 && args[0] == "generate"
	f.mu.Unlock()
	code = Command(context.Background(), append(args, "--store", AWSSecretsManager, "--name", awsSecret, "--region", awsRegion),
		&out, &errs, Stores{AWSSecretsManager: OpenAWS})
	stdout, stderr = out.String(), errs.String()
	for _, value := range f.values() {
		require.NotContains(t, stdout+stderr, value, "a seed reached an output")
	}
	return code, stdout, stderr
}

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

func TestAWSGenerateWritesTheBareSeedWithAFreshTokenAndReadsItBack(t *testing.T) {
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
	require.NotEqual(t, f.tokens[0], again.tokens[0], "each generate sends a fresh ClientRequestToken")
}

func TestAWSGenerateRefusesBeforeWriting(t *testing.T) {
	seedValue, _ := testnats.User(t)
	rows := []struct {
		name  string
		fake  func(*fakeAWS) *fakeAWS
		want  string
		lists int
	}{
		{"a deprecated version DescribeSecret does not show", func(f *fakeAWS) *fakeAWS {
			f.versions = []awsVersion{{id: "v1", value: seedValue}}
			return f
		}, "already holds 1 version(s)", 1},
		{"versions on every page", func(f *fakeAWS) *fakeAWS { f.pageSize = 2; return f.holding(seedValue, seedValue, seedValue) },
			"already holds 3 version(s)", 2},
		{"a listing that repeats its page token", func(f *fakeAWS) *fakeAWS { f.repeatToken = true; return f },
			"list-secret-version-ids " + awsSecret + " failed: the listing handed out page token page-2 twice", 2},
		{"absent", func(f *fakeAWS) *fakeAWS { f.exists = false; return f }, awsSecret + " does not exist", 0},
		{"scheduled for deletion", func(f *fakeAWS) *fakeAWS { f.deletedAt = 1759190400; return f }, "is scheduled for deletion", 0},
		{"describe-secret denied", func(f *fakeAWS) *fakeAWS {
			f.failures = map[string]awsFailure{"DescribeSecret": {"AccessDeniedException", "not allowed"}}
			return f
		}, "describe-secret " + awsSecret + " failed: AccessDeniedException", 0},
		{"list-secret-version-ids denied", func(f *fakeAWS) *fakeAWS {
			f.failures = map[string]awsFailure{"ListSecretVersionIds": {"AccessDeniedException", "not allowed"}}
			return f
		}, "list-secret-version-ids " + awsSecret + " failed: AccessDeniedException", 1},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := row.fake(newFakeAWS(t))
			serve(t, f)
			code, out, errs := runAWS(t, f, "generate")
			require.Equal(t, 2, code, errs)
			require.Empty(t, out)
			require.Contains(t, errs, "refused: ")
			require.Contains(t, errs, row.want)
			require.NotContains(t, f.recorded(), "PutSecretValue")
			lists := 0
			for _, call := range f.recorded() {
				if call == "ListSecretVersionIds" {
					lists++
				}
			}
			require.Equal(t, row.lists, lists)
		})
	}
}

func TestAWSGenerateAfterItsWrite(t *testing.T) {
	theirs, _ := testnats.User(t)
	rows := []struct {
		name string
		fake func(*fakeAWS) *fakeAWS
		want string
	}{
		{"the written version is not AWSCURRENT", func(f *fakeAWS) *fakeAWS { f.putStages = []string{"AWSPENDING"}; return f },
			"was written but is not AWSCURRENT; run check"},
		{"another writer raced it", func(f *fakeAWS) *fakeAWS { f.racing = &awsVersion{id: "their-version", value: theirs}; return f },
			awsSecret + " now holds 2 version(s)"},
		{"listing afterwards fails", func(f *fakeAWS) *fakeAWS {
			f.failAfterPut = map[string]awsFailure{"ListSecretVersionIds": {"AccessDeniedException", "not allowed"}}
			return f
		}, "but listing versions afterwards failed: list-secret-version-ids " + awsSecret + " failed: AccessDeniedException"},
		{"reading back fails", func(f *fakeAWS) *fakeAWS {
			f.failAfterPut = map[string]awsFailure{"GetSecretValue": {"DecryptionFailure", "cannot decrypt"}}
			return f
		}, "but reading it back failed: get-secret-value " + awsSecret + " failed: DecryptionFailure"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := row.fake(newFakeAWS(t))
			serve(t, f)
			code, out, errs := runAWS(t, f, "generate")
			require.Equal(t, 1, code, errs)
			require.Contains(t, errs, "error after writing: ")
			require.Contains(t, errs, row.want)
			require.Len(t, f.tokens, 1)
			mine := f.versions[len(f.versions)-1]
			require.Equal(t, f.tokens[0], mine.id)
			require.Equal(t, "public key: "+publicKeyOfSeed(t, mine.value)+"\nversion id: "+mine.id+"\n", out)
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

// The fake fails a test on any request the store does not make: an operation it does not answer, a
// field it does not expect, an unsigned request, or a version listing outside generate.
func TestTheFakeRecordsEveryRequestItDoesNotExpect(t *testing.T) {
	listing := func() *http.Request {
		return signed(httptest.NewRequest(http.MethodPost, "/",
			strings.NewReader(`{"SecretId":"`+awsSecret+`","IncludeDeprecated":true}`)), "secretsmanager.ListSecretVersionIds")
	}
	f := &fakeAWS{t: t, account: awsAccount, exists: true}
	for _, request := range []*http.Request{
		signed(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"SecretId":"`+awsSecret+`"}`)), "secretsmanager.DeleteSecret"),
		signed(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"SecretId":"`+awsSecret+`","VersionStage":"AWSPREVIOUS"}`)),
			"secretsmanager.GetSecretValue"),
		signed(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"SecretId":"`+awsSecret+`"}`)), "secretsmanager.ListSecretVersionIds"),
		httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"SecretId":"`+awsSecret+`"}`)),
		listing(), // well formed, but no generate is running
	} {
		recorder := httptest.NewRecorder()
		f.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusBadRequest, recorder.Code)
	}
	require.Len(t, f.unexpected, 5)
	require.Contains(t, f.unexpected[4], "ListSecretVersionIds from check or verify")

	// The same listing during a generate is answered.
	f.unexpected, f.generating = nil, true
	recorder := httptest.NewRecorder()
	f.ServeHTTP(recorder, listing())
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Empty(t, f.unexpected)
}

func signed(r *http.Request, target string) *http.Request {
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261014/us-east-1/secretsmanager/aws4_request")
	r.Header.Set("X-Amz-Target", target)
	r.Header.Set("Content-Type", "application/x-amz-json-1.1")
	return r
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
