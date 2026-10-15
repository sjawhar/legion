package seed

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/testnats"
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

// gcpCert is the fake's certificate, for 127.0.0.1. TestMain makes it a root the whole test process
// trusts through SSL_CERT_FILE, which Go reads once, when it first loads the system's roots: the
// client then verifies the fake as it verifies Secret Manager, against the roots the system trusts.
var gcpCert tls.Certificate

func TestMain(m *testing.M) { os.Exit(runTrustingTheFake(m)) }

func runTrustingTheFake(m *testing.M) int {
	dir, err := os.MkdirTemp("", "envoy-infra-seed-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "envoy-infra seed test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	roots := filepath.Join(dir, "roots.pem")
	if err := os.WriteFile(roots, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.Setenv("SSL_CERT_FILE", roots); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	gcpCert = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return m.Run()
}

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

func TestGCPCheck(t *testing.T) {
	seedValue, _ := testnats.User(t)
	metadataOnly := []string{"GetSecret", "GetSecretVersion"}
	rows := []struct {
		name  string
		fake  func(*fakeGCP) *fakeGCP
		want  string
		calls []string
	}{
		{"absent", func(f *fakeGCP) *fakeGCP { f.exists = false; return f }, gcpSecret + ": absent", []string{"GetSecret"}},
		{"no versions", func(f *fakeGCP) *fakeGCP { return f }, gcpSecret + ": present, no versions", metadataOnly},
		{"enabled", func(f *fakeGCP) *fakeGCP { return f.with(seedValue, disabled).with(seedValue, enabled) },
			gcpSecret + ": present; latest: version 2", metadataOnly},
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
	require.Equal(t, []string{"GetSecret", "GetSecretVersion", "ListSecretVersions", "AddSecretVersion", "ListSecretVersions", "AccessSecretVersion"},
		f.recorded())
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
			"already holds version 1, which latest names", 0},
		{"a disabled version", func(f *fakeGCP) *fakeGCP { return f.with(seedValue, disabled) },
			"already holds version 1, which latest names", 0},
		{"a version latest names and the listing leaves out", func(f *fakeGCP) *fakeGCP {
			f.unlisted = 1
			return f.with(seedValue, enabled)
		}, "already holds version 1, which latest names", 0},
		// The listing counts what latest does not name yet, destroyed versions included, across
		// every page.
		{"a destroyed version only the listing shows", func(f *fakeGCP) *fakeGCP {
			f.unnamed = 1
			return f.with("", destroyed)
		}, "already holds 1 version(s)", 1},
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
	require.Equal(t, []string{"GetSecret", "GetSecretVersion", "ListSecretVersions", "AddSecretVersion"}, f.recorded(),
		"one AddSecretVersion, not retried, and no call after it")
}

// Secret Manager numbers a secret's versions from 1, one more for each (resources.proto,
// SecretVersion.name), so a write answered with any other number shows the secret held a version
// both eventually consistent reads before the write, latest and the listing, missed: an error after
// the write, with nothing read back.
func TestGCPGenerateAfterWritingAVersionOtherThanTheFirst(t *testing.T) {
	earlier, _ := testnats.User(t)
	f := newFakeGCP(t).with(earlier, enabled)
	f.unlisted, f.unnamed = 1, 1
	serveGCP(t, f)
	code, out, errs := runGCP(t, f, "generate")
	require.Equal(t, 1, code, errs)
	require.Equal(t, 1, f.added)
	require.Equal(t, "public key: "+publicKeyOfSeed(t, f.versions[1].value)+"\nversion id: 2\n", out)
	require.Equal(t, "error after writing: version 2 was written, not version 1: "+gcpSecret+" held a version "+
		"neither latest nor the listing showed before the write; run check\n", errs)
	require.Equal(t, []string{"GetSecret", "GetSecretVersion", "ListSecretVersions", "AddSecretVersion"}, f.recorded(),
		"no read-back after the write")
}

// Secret Manager's listings and its latest alias are eventually consistent: only an access by
// version number is strongly consistent with the add
// (https://cloud.google.com/secret-manager/docs/access-secret-version, "Resource consistency"). So
// generate looks again while neither shows its write yet, and a write the store never shows is an
// error after the write.
func TestGCPGenerateWaitsForTheStoreToShowItsWrite(t *testing.T) {
	SetReadBackWaits(t, time.Millisecond, time.Millisecond, time.Millisecond)
	before := []string{"GetSecret", "GetSecretVersion", "ListSecretVersions", "AddSecretVersion"}
	t.Run("caught up", func(t *testing.T) {
		f := newFakeGCP(t)
		f.lagList, f.lagAccess = 1, 2
		serveGCP(t, f)
		code, out, errs := runGCP(t, f, "generate")
		require.Equal(t, 0, code, errs)
		require.Empty(t, errs)
		require.Equal(t, "public key: "+publicKeyOfSeed(t, f.versions[0].value)+"\nversion id: 1\n", out)
		require.Equal(t, slices.Concat(before, []string{
			"ListSecretVersions",                        // the listing leaves the write out
			"ListSecretVersions", "AccessSecretVersion", // latest is not found yet
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
	// version lands just before AccessSecretVersion reads, which Secret Manager then refuses.
	t.Run("a latest version disabled between describe and access", func(t *testing.T) {
		f := newFakeGCP(t).with(seedValue, enabled)
		f.lateWriter = &gcpVersion{value: seedValue, state: disabled}
		serveGCP(t, f)
		code, out, errs := runGCP(t, f, "verify", "--public-key", public)
		require.Equal(t, 1, code, errs)
		require.Equal(t, gcpSecret+": present; latest: version 2 is DISABLED\n", out)
		require.Empty(t, errs)
		require.Equal(t, []string{"GetSecret", "GetSecretVersion", "AccessSecretVersion", "GetSecret", "GetSecretVersion"}, f.recorded())
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
		require.Equal(t, []string{"GetSecret", "GetSecretVersion", "AccessSecretVersion", "GetSecret", "GetSecretVersion"}, f.recorded(),
			"a failed read describes the secret again, which shows the latest version still served")
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

// A --name the store would refuse is refused before the store opens, so no refusal of the open
// quotes it: a seed pasted there must not reach the terminal, whatever else is wrong.
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
			require.Equal(t, "refused: --name for --store gcp-secret-manager is a secret's name, projects/<project>/secrets/<secret>\n", errs)
			require.Empty(t, f.recorded())
		})
	}
}

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
		require.Equal(t, gcpSecret+": present, no versions\n", out)
		require.Equal(t, []string{"GetSecret", "GetSecretVersion"}, f.recorded())
	})
	t.Run("GOOGLE_OAUTH_ACCESS_TOKEN over application default credentials", func(t *testing.T) {
		f := newFakeGCP(t)
		file, _ := serviceAccount(t)
		serveGCP(t, f)
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", file)
		code, out, errs := runGCP(t, f, "check")
		require.Equal(t, 0, code, errs)
		require.Equal(t, gcpSecret+": present, no versions\n", out)
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
		got := gcpFailed("add-secret-version", gcpSecret, row.err).Error()
		require.Equal(t, "add-secret-version "+gcpSecret+" failed: "+row.want, got)
		require.NotContains(t, got, canary)
	}
}
