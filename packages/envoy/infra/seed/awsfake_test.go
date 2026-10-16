package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
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
	// staleViews makes the next staleViews DescribeSecret and ListSecretVersionIds answers show no
	// version, as a run whose checks were answered before another run's first write landed sees.
	staleViews int
	// The ARN every Secrets Manager answer gives the secret: secretARN when set (a test that wants
	// none sets it to " " and the field is left out), otherwise one in secretAccount and
	// secretRegion, by default awsAccount and awsRegion, whatever account STS answers.
	secretARN, secretAccount, secretRegion string
}

func newFakeAWS(t *testing.T) *fakeAWS {
	f := &fakeAWS{t: t, account: awsAccount, exists: true, secretAccount: awsAccount, secretRegion: awsRegion}
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
	arn := "arn:aws:secretsmanager:" + f.secretRegion + ":" + f.secretAccount + ":secret:" + awsSecret + "-AbCdEf"
	if f.secretARN != "" {
		arn = strings.TrimSpace(f.secretARN)
	}
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
		out := map[string]any{"Name": awsSecret}
		if arn != "" {
			out["ARN"] = arn
		}
		stages := map[string][]string{}
		if f.staleViews > 0 {
			f.staleViews--
			f.answer(w, out)
			return
		}
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
		if f.staleViews > 0 {
			f.staleViews--
			versions = nil
		}
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

// requests counts every request that reached f, expected or not.
func (f *fakeAWS) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls) + len(f.unexpected)
}

// forgetUnexpected drops the requests f did not expect, for a control run whose request is the
// point rather than a failure.
func (f *fakeAWS) forgetUnexpected() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unexpected = nil
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
		&out, &errs, Stores{AWSSecretsManager: AWS})
	stdout, stderr = out.String(), errs.String()
	for _, value := range f.values() {
		require.NotContains(t, stdout+stderr, value, "a seed reached an output")
	}
	return code, stdout, stderr
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
