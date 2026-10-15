package seed

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
)

const (
	gcpSecretID = "envoy-nats-seed-example-host-a"
	gcpSecret   = "projects/example-project/secrets/" + gcpSecretID
	gcpLatestOf = gcpSecret + "/versions/latest"
	gcpToken    = "example-access-token"
)

const (
	enabled   = secretmanagerpb.SecretVersion_ENABLED
	disabled  = secretmanagerpb.SecretVersion_DISABLED
	destroyed = secretmanagerpb.SecretVersion_DESTROYED
)

type gcpVersion struct {
	number int
	value  string
	state  secretmanagerpb.SecretVersion_State
}

// gcpModeCalls are the calls each mode makes: check reads metadata alone, verify also reads the
// value, and only generate lists versions and adds one.
var gcpModeCalls = map[string][]string{
	"check":    {"GetSecret", "GetSecretVersion"},
	"verify":   {"GetSecret", "GetSecretVersion", "AccessSecretVersion"},
	"generate": {"GetSecret", "GetSecretVersion", "ListSecretVersions", "AddSecretVersion", "AccessSecretVersion"},
}

// fakeGCP is Secret Manager's gRPC service, over TLS, holding the one secret gcpSecret. It answers
// only the calls the mode under test makes, each request compared whole with the one the store
// should send, and only with a bearer token that bearer accepts: any other call, a request with a
// field it does not expect, or a call without that token, is recorded and fails the test that made it.
type fakeGCP struct {
	secretmanagerpb.UnimplementedSecretManagerServiceServer
	t  *testing.T
	mu sync.Mutex

	exists   bool
	versions []gcpVersion // in the order they were added: the last is the newest
	bearer   func(token string) error

	mode       string   // the command's mode, whose calls the fake answers
	calls      []string // each call's method, in order
	unexpected []string
	added      int      // AddSecretVersion calls that wrote a version
	sent       []string // every payload AddSecretVersion was sent, written or failed

	failures     map[string]codes.Code // the code a call fails with, by its method
	failAfterAdd map[string]codes.Code // the code a call fails with once a version was added
	pageSize     int                   // versions per ListSecretVersions page; 0 for all at once
	repeatToken  bool                  // every ListSecretVersions page names the same next page token
	addState     secretmanagerpb.SecretVersion_State
	racing       *gcpVersion // another writer's version, added right after the store's
	lateWriter   *gcpVersion // another writer's version, added just before AccessSecretVersion reads
	echoRequests bool        // every error message quotes the request
	// Secret Manager's listings and its latest alias are eventually consistent: after a version is
	// added, the next lagList listings leave it out and the next lagAccess reads of latest answer
	// NOT_FOUND, as they do for a secret with no version.
	lagList, lagAccess int
	// The same before the store's write: until it adds a version, the listing leaves out the first
	// unlisted versions the secret holds, and latest the first unnamed ones.
	unlisted, unnamed int
}

func newFakeGCP(t *testing.T) *fakeGCP {
	f := &fakeGCP{t: t, exists: true, bearer: func(token string) error {
		if token != gcpToken {
			return errors.New("not the test's access token")
		}
		return nil
	}}
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, what := range f.unexpected {
			t.Errorf("the client sent a call the fake does not expect: %s", what)
		}
	})
	return f
}

// with adds a version holding value in state.
func (f *fakeGCP) with(value string, state secretmanagerpb.SecretVersion_State) *fakeGCP {
	f.versions = append(f.versions, gcpVersion{number: len(f.versions) + 1, value: value, state: state})
	return f
}

// versionName is a version's name as Secret Manager answers it, with the project's number.
func versionName(number int) string {
	return fmt.Sprintf("projects/000000000000/secrets/%s/versions/%d", gcpSecretID, number)
}

func (f *fakeGCP) intercept(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	method := path.Base(info.FullMethod)
	md, _ := metadata.FromIncomingContext(ctx)
	authorization := md.Get("authorization")
	if len(authorization) != 1 || !strings.HasPrefix(authorization[0], "Bearer ") {
		return nil, f.reject(method + " without a bearer token")
	}
	if err := f.bearer(strings.TrimPrefix(authorization[0], "Bearer ")); err != nil {
		return nil, f.reject(fmt.Sprintf("%s with a bearer token the fake refuses: %v", method, err))
	}
	if !slices.Contains(gcpModeCalls[f.mode], method) {
		return nil, f.reject(fmt.Sprintf("%s from %s, which makes only %s", method, f.mode, strings.Join(gcpModeCalls[f.mode], ", ")))
	}
	f.calls = append(f.calls, method)
	if add, ok := req.(*secretmanagerpb.AddSecretVersionRequest); ok {
		f.sent = append(f.sent, string(add.GetPayload().GetData()))
	}
	if code, ok := f.failures[method]; ok {
		return nil, f.fail(code, req)
	}
	if code, ok := f.failAfterAdd[method]; ok && f.added > 0 {
		return nil, f.fail(code, req)
	}
	return handler(ctx, req)
}

// reject records a call the store should never make and answers it with an error.
func (f *fakeGCP) reject(what string) error {
	f.unexpected = append(f.unexpected, what)
	return status.Error(codes.Unimplemented, "the fake does not answer this call")
}

func (f *fakeGCP) fail(code codes.Code, req any) error {
	message := "the fake fails this call"
	if f.echoRequests {
		message += ": " + prototext.Format(req.(proto.Message))
	}
	return status.Error(code, message)
}

func (f *fakeGCP) notFound() error {
	return status.Error(codes.NotFound, "Secret ["+gcpSecret+"] not found or has no versions.")
}

func (f *fakeGCP) GetSecret(_ context.Context, req *secretmanagerpb.GetSecretRequest) (*secretmanagerpb.Secret, error) {
	if !proto.Equal(req, &secretmanagerpb.GetSecretRequest{Name: gcpSecret}) {
		return nil, f.reject("GetSecret " + prototext.Format(req))
	}
	if !f.exists {
		return nil, status.Error(codes.NotFound, "Secret ["+gcpSecret+"] not found.")
	}
	return &secretmanagerpb.Secret{Name: "projects/000000000000/secrets/" + gcpSecretID}, nil
}

func (f *fakeGCP) GetSecretVersion(_ context.Context, req *secretmanagerpb.GetSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	if !proto.Equal(req, &secretmanagerpb.GetSecretVersionRequest{Name: gcpLatestOf}) {
		return nil, f.reject("GetSecretVersion " + prototext.Format(req) + ": the latest version alone")
	}
	visible := f.versions
	if f.added == 0 {
		visible = visible[f.unnamed:]
	}
	if !f.exists || len(visible) == 0 {
		return nil, f.notFound()
	}
	newest := visible[len(visible)-1]
	return &secretmanagerpb.SecretVersion{Name: versionName(newest.number), State: newest.state}, nil
}

func (f *fakeGCP) ListSecretVersions(_ context.Context, req *secretmanagerpb.ListSecretVersionsRequest) (*secretmanagerpb.ListSecretVersionsResponse, error) {
	if !proto.Equal(req, &secretmanagerpb.ListSecretVersionsRequest{Parent: gcpSecret, PageToken: req.GetPageToken()}) {
		return nil, f.reject("ListSecretVersions " + prototext.Format(req) + ": every version, with no filter")
	}
	if !f.exists {
		return nil, f.notFound()
	}
	if f.repeatToken {
		if lists := slices.DeleteFunc(slices.Clone(f.calls), func(c string) bool { return c != "ListSecretVersions" }); len(lists) > 10 {
			return nil, f.reject("a repeated page token was followed")
		}
		return &secretmanagerpb.ListSecretVersionsResponse{NextPageToken: "page-2"}, nil
	}
	start := 0
	if token := req.GetPageToken(); token != "" {
		if _, err := fmt.Sscanf(token, "page-%d", &start); err != nil {
			return nil, f.reject("ListSecretVersions page token " + token)
		}
	}
	// Secret Manager lists the newest version first.
	versions := slices.Clone(f.versions)
	if f.added == 0 {
		versions = versions[f.unlisted:]
	}
	if f.added > 0 && f.lagList > 0 {
		f.lagList--
		versions = versions[:len(versions)-f.added]
	}
	slices.Reverse(versions)
	end := len(versions)
	if f.pageSize > 0 {
		end = min(start+f.pageSize, len(versions))
	}
	out := &secretmanagerpb.ListSecretVersionsResponse{TotalSize: int32(len(versions))}
	for _, v := range versions[start:end] {
		out.Versions = append(out.Versions, &secretmanagerpb.SecretVersion{Name: versionName(v.number), State: v.state})
	}
	if end < len(versions) {
		out.NextPageToken = fmt.Sprintf("page-%d", end)
	}
	return out, nil
}

func (f *fakeGCP) AddSecretVersion(_ context.Context, req *secretmanagerpb.AddSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	data := req.GetPayload().GetData()
	if len(data) == 0 || !proto.Equal(req, &secretmanagerpb.AddSecretVersionRequest{Parent: gcpSecret, Payload: &secretmanagerpb.SecretPayload{Data: data}}) {
		return nil, f.reject("AddSecretVersion: a parent and the payload's data, nothing else")
	}
	if !f.exists {
		return nil, f.notFound()
	}
	state := f.addState
	if state == secretmanagerpb.SecretVersion_STATE_UNSPECIFIED {
		state = enabled
	}
	f.added++
	f.with(string(data), state)
	mine := f.versions[len(f.versions)-1]
	if f.racing != nil {
		f.with(f.racing.value, f.racing.state)
	}
	return &secretmanagerpb.SecretVersion{Name: versionName(mine.number), State: mine.state}, nil
}

func (f *fakeGCP) AccessSecretVersion(_ context.Context, req *secretmanagerpb.AccessSecretVersionRequest) (*secretmanagerpb.AccessSecretVersionResponse, error) {
	if !proto.Equal(req, &secretmanagerpb.AccessSecretVersionRequest{Name: gcpLatestOf}) {
		return nil, f.reject("AccessSecretVersion " + prototext.Format(req) + ": the latest version alone")
	}
	if f.lateWriter != nil {
		f.with(f.lateWriter.value, f.lateWriter.state)
		f.lateWriter = nil
	}
	if !f.exists || len(f.versions) == 0 {
		return nil, f.notFound()
	}
	if f.added > 0 && f.lagAccess > 0 {
		f.lagAccess--
		return nil, f.notFound()
	}
	newest := f.versions[len(f.versions)-1]
	if newest.state != enabled {
		return nil, status.Errorf(codes.FailedPrecondition, "Secret Version [%s] is in %s state.", versionName(newest.number), newest.state)
	}
	return &secretmanagerpb.AccessSecretVersionResponse{
		Name:    versionName(newest.number),
		Payload: &secretmanagerpb.SecretPayload{Data: []byte(newest.value)},
	}, nil
}

// values is every secret value the fake holds or will be sent: none may reach an output.
func (f *fakeGCP) values() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []string
	for _, v := range f.versions {
		all = append(all, v.value)
	}
	all = append(all, f.sent...)
	for _, v := range []*gcpVersion{f.racing, f.lateWriter} {
		if v != nil {
			all = append(all, v.value)
		}
	}
	return all
}

func (f *fakeGCP) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeGCP) setMode(mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode = mode
}

// serveGCP starts f on a loopback port over TLS and points the client's environment at it as an
// operator's would: SECRET_MANAGER_ENDPOINT and an access token in GOOGLE_OAUTH_ACCESS_TOKEN, with
// no credentials file, no gcloud configuration and the library's logging unset.
func serveGCP(t *testing.T, f *fakeGCP) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer(grpc.Creds(credentials.NewServerTLSFromCert(&gcpCert)), grpc.UnaryInterceptor(f.intercept))
	secretmanagerpb.RegisterSecretManagerServiceServer(server, f)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	for key, value := range map[string]string{
		"SECRET_MANAGER_ENDPOINT":           "https://" + listener.Addr().String(),
		"GOOGLE_OAUTH_ACCESS_TOKEN":         gcpToken,
		"GOOGLE_APPLICATION_CREDENTIALS":    "",
		"CLOUDSDK_CONFIG":                   t.TempDir(),
		"GOOGLE_SDK_GO_LOGGING_LEVEL":       "",
		"GOOGLE_CLOUD_UNIVERSE_DOMAIN":      "",
		"GOOGLE_CLOUD_QUOTA_PROJECT":        "",
		"GOOGLE_API_USE_CLIENT_CERTIFICATE": "",
		"GOOGLE_API_USE_MTLS_ENDPOINT":      "",
	} {
		t.Setenv(key, value)
	}
	return listener.Addr().String()
}

// runGCP runs the command with the Secret Manager store, against f, for gcpSecret.
func runGCP(t *testing.T, f *fakeGCP, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return runGCPAs(t, f, gcpSecret, args...)
}

func runGCPAs(t *testing.T, f *fakeGCP, name string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	f.setMode(args[0])
	var out, errs bytes.Buffer
	code = Command(context.Background(), append(args, "--store", GCPSecretManager, "--name", name),
		&out, &errs, Stores{GCPSecretManager: OpenGCP})
	stdout, stderr = out.String(), errs.String()
	for _, value := range f.values() {
		if value != "" { // a destroyed version holds none, and "" is in every text
			require.NotContains(t, stdout+stderr, value, "a seed reached an output")
		}
	}
	return code, stdout, stderr
}
