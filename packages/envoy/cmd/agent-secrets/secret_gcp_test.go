// packages/envoy/cmd/agent-secrets/secret_gcp_test.go
//
// The GCP secret forms run in-process here, against a fake Secret Manager gRPC server
// (fakeSecretManager) standing in for the real API, with gcpCredentials and gcpUserinfo replaced
// so no form makes a network call for its identity, a stub gcloud alone on PATH (useGcloud) where
// a form can reach for the person's gcloud login, and the shared fake broker (secretBroker,
// secret_test.go) answering GET /v1/settings with the GCP store's shape and the reread.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

const (
	// testGCPProject is a project NUMBER, the form the broker's gcp_project setting and the
	// secrets' own resource-name prefix take.
	testGCPProject = "100000000001"
	testKMSKeyName = "projects/acme-agent-infra/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets"
	testGCPPrefix  = "projects/100000000001/secrets/agent-secrets-"
)

func gcpTestSettings() Settings {
	return Settings{
		Store: secrets.StoreGCP, SecretsPrefix: testGCPPrefix,
		KMSKeyName: testKMSKeyName, GCPProject: testGCPProject,
	}
}

// fakeSecret is one secret fakeSecretManager holds.
type fakeSecret struct {
	secret      *secretmanagerpb.Secret
	versions    []*secretmanagerpb.SecretVersion
	etagCounter int
}

// fakeSecretManager is a minimal in-memory Secret Manager server, implementing only the RPCs the
// GCP secret forms call.
type fakeSecretManager struct {
	secretmanagerpb.UnimplementedSecretManagerServiceServer

	mu      sync.Mutex
	secrets map[string]*fakeSecret // full resource name -> secret
	// refuseAnnotationUpdates, when set, makes UpdateSecret refuse an annotations-field-mask
	// request with PermissionDenied, as IAM refuses a person changing a shared secret's tags.
	refuseAnnotationUpdates bool
	// race, when set, runs once on the secret named by the first GetSecret or UpdateSecret after the
	// first GetSecret, before that call is answered, as another writer's update landing between a
	// read and the next call does; gets counts GetSecret calls.
	race  func(*fakeSecret)
	gets  int
	raced bool
	// authorizations is the authorization metadata each call carried, in order: the sign-in it
	// ran under (recordAuthorization).
	authorizations []string
}

// raceOnce runs f.race on entry once, on a call after the first GetSecret.
func (f *fakeSecretManager) raceOnce(entry *fakeSecret) {
	if f.race != nil && !f.raced && f.gets > 0 {
		f.raced = true
		f.race(entry)
	}
}

// recordAuthorization notes the authorization metadata each call carries before the fake answers
// it, as the real API reads the caller's sign-in from it.
func (f *fakeSecretManager) recordAuthorization(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	f.mu.Lock()
	f.authorizations = append(f.authorizations, strings.Join(md.Get("authorization"), ","))
	f.mu.Unlock()
	return handler(ctx, req)
}

// calledUnder is the authorization every call so far carried, and how many calls there were; ""
// when the calls carried different ones or none was made.
func (f *fakeSecretManager) calledUnder() (authorization string, calls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, a := range f.authorizations {
		if i > 0 && a != authorization {
			return "", len(f.authorizations)
		}
		authorization = a
	}
	return authorization, len(f.authorizations)
}

// written is how many secrets f holds.
func (f *fakeSecretManager) written() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.secrets)
}

func newFakeSecretManager() *fakeSecretManager {
	return &fakeSecretManager{secrets: map[string]*fakeSecret{}}
}

func (f *fakeSecretManager) CreateSecret(_ context.Context, req *secretmanagerpb.CreateSecretRequest) (*secretmanagerpb.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := req.GetParent() + "/secrets/" + req.GetSecretId()
	if _, ok := f.secrets[name]; ok {
		return nil, status.Error(codes.AlreadyExists, "already exists")
	}
	secret := req.GetSecret()
	secret.Name = name
	secret.Etag = "1"
	f.secrets[name] = &fakeSecret{secret: secret, etagCounter: 1}
	return secret, nil
}

func (f *fakeSecretManager) GetSecret(_ context.Context, req *secretmanagerpb.GetSecretRequest) (*secretmanagerpb.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.secrets[req.GetName()]
	if ok {
		f.raceOnce(entry)
	}
	f.gets++
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return entry.secret, nil
}

// UpdateSecret refuses an Etag that does not match the secret's current one with
// FAILED_PRECONDITION, as the real API does for optimistic concurrency, and bumps the etag on
// every applied update; a request with no Etag (as delete/restore send) skips the check.
func (f *fakeSecretManager) UpdateSecret(_ context.Context, req *secretmanagerpb.UpdateSecretRequest) (*secretmanagerpb.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, path := range req.GetUpdateMask().GetPaths() {
		if path == "annotations" && f.refuseAnnotationUpdates {
			return nil, status.Error(codes.PermissionDenied, "caller does not have permission to update this secret's annotations")
		}
	}
	entry, ok := f.secrets[req.GetSecret().GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	f.raceOnce(entry)
	if etag := req.GetSecret().GetEtag(); etag != "" && etag != entry.secret.GetEtag() {
		return nil, status.Error(codes.FailedPrecondition, "etag mismatch")
	}
	for _, path := range req.GetUpdateMask().GetPaths() {
		switch path {
		case "annotations":
			entry.secret.Annotations = req.GetSecret().GetAnnotations()
		case "expire_time":
			entry.secret.Expiration = req.GetSecret().GetExpiration()
		}
	}
	entry.etagCounter++
	entry.secret.Etag = strconv.Itoa(entry.etagCounter)
	return entry.secret, nil
}

func (f *fakeSecretManager) ListSecrets(_ context.Context, req *secretmanagerpb.ListSecretsRequest) (*secretmanagerpb.ListSecretsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := &secretmanagerpb.ListSecretsResponse{}
	for name, entry := range f.secrets {
		if strings.HasPrefix(name, req.GetParent()+"/secrets/") {
			resp.Secrets = append(resp.Secrets, entry.secret)
		}
	}
	return resp, nil
}

func (f *fakeSecretManager) AddSecretVersion(_ context.Context, req *secretmanagerpb.AddSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.secrets[req.GetParent()]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	number := len(entry.versions) + 1
	name := req.GetParent() + "/versions/" + strconv.Itoa(number)
	version := &secretmanagerpb.SecretVersion{Name: name, State: secretmanagerpb.SecretVersion_ENABLED}
	entry.versions = append(entry.versions, version)
	return version, nil
}

func (f *fakeSecretManager) GetSecretVersion(_ context.Context, req *secretmanagerpb.GetSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parent := req.GetName()[:strings.LastIndex(req.GetName(), "/versions/")]
	entry, ok := f.secrets[parent]
	if !ok || len(entry.versions) == 0 {
		return nil, status.Error(codes.NotFound, "not found")
	}
	if strings.HasSuffix(req.GetName(), "/versions/latest") {
		return entry.versions[len(entry.versions)-1], nil
	}
	for _, v := range entry.versions {
		if v.GetName() == req.GetName() {
			return v, nil
		}
	}
	return nil, status.Error(codes.NotFound, "not found")
}

func (f *fakeSecretManager) ListSecretVersions(_ context.Context, req *secretmanagerpb.ListSecretVersionsRequest) (*secretmanagerpb.ListSecretVersionsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.secrets[req.GetParent()]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return &secretmanagerpb.ListSecretVersionsResponse{Versions: entry.versions}, nil
}

// startFakeSecretManager serves f over a real gRPC server on loopback for the rest of t, and
// points gcpClientDial at it. The fake takes no authentication, but each call carries the bearer
// token of the credentials it was dialed with (bearerOf), which the fake records
// (recordAuthorization), so a test sees which sign-in a call ran under.
func startFakeSecretManager(t *testing.T, f *fakeSecretManager) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer(grpc.UnaryInterceptor(f.recordAuthorization))
	secretmanagerpb.RegisterSecretManagerServiceServer(server, f)
	go server.Serve(lis)
	t.Cleanup(server.Stop)

	restore := gcpClientDial
	gcpClientDial = func(ctx context.Context, creds *google.Credentials) (*secretmanager.Client, error) {
		dial := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
		if creds != nil && creds.TokenSource != nil {
			dial = append(dial, grpc.WithPerRPCCredentials(bearerOf{creds.TokenSource}))
		}
		conn, err := grpc.NewClient(lis.Addr().String(), dial...)
		if err != nil {
			return nil, err
		}
		return secretmanager.NewClient(ctx, option.WithGRPCConn(conn))
	}
	t.Cleanup(func() { gcpClientDial = restore })
}

// bearerOf hands each call the access token source answers, as the real client does; the fake
// listens without TLS, so it asks for none.
type bearerOf struct{ source oauth2.TokenSource }

func (b bearerOf) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	token, err := b.source.Token()
	if err != nil {
		return nil, err
	}
	return map[string]string{"authorization": "Bearer " + token.AccessToken}, nil
}

func (bearerOf) RequireTransportSecurity() bool { return false }

// gcpServedBy answers a reread as the broker would from what f holds: absent for a secret it does
// not hold, or one scheduled for deletion (expire_time set, whether or not it has passed - a
// broker reread treats a scheduled deletion as absent the moment it is scheduled, as AWS's
// DeletedDate does), no-current-value for one with no version, served otherwise.
func gcpServedBy(f *fakeSecretManager, settings Settings) func(name string) Reread {
	return func(name string) Reread {
		slug, err := policy.NameToSlug(name)
		if err != nil {
			return Reread{Name: name, Reason: policy.ReasonAbsent}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		entry, ok := f.secrets[settings.SecretsPrefix+slug]
		switch {
		case !ok:
			return Reread{Name: name, Reason: policy.ReasonAbsent}
		case entry.secret.GetExpireTime() != nil:
			return Reread{Name: name, Reason: policy.ReasonAbsent}
		case len(entry.versions) == 0:
			return Reread{Name: name, Reason: policy.ReasonNoCurrentValue}
		}
		return Reread{Name: name, Served: true, Kind: entry.secret.GetAnnotations()[policy.TagKind]}
	}
}

// useGCPIdentity makes gcpCredentials and gcpUserinfo answer kind/email for the rest of t, with no
// network call. kind is the ADC JSON's "type" ("authorized_user" for a person's sign-in,
// "service_account" for a key file, "external_account" for a workload identity federation
// credential such as a dev box's); "" leaves the JSON empty, as a GCE/GKE metadata-server identity
// carries none.
func useGCPIdentity(t *testing.T, kind, email string) {
	t.Helper()
	restoreCreds, restoreUserinfo := gcpCredentials, gcpUserinfo
	gcpCredentials = func(context.Context) (*google.Credentials, error) {
		var creds google.Credentials
		if kind != "" {
			creds.JSON = []byte(`{"type":"` + kind + `"}`)
		}
		creds.TokenSource = oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake"})
		return &creds, nil
	}
	gcpUserinfo = userinfoOf(email)
	t.Cleanup(func() { gcpCredentials, gcpUserinfo = restoreCreds, restoreUserinfo })
}

// useNoADC makes gcpCredentials find no Application Default Credentials for the rest of t, as
// google.FindDefaultCredentials answers on a machine that has none.
func useNoADC(t *testing.T) {
	t.Helper()
	restoreCreds, restoreUserinfo := gcpCredentials, gcpUserinfo
	gcpCredentials = func(context.Context) (*google.Credentials, error) {
		return nil, errors.New("google: could not find default credentials")
	}
	gcpUserinfo = userinfoOf("")
	t.Cleanup(func() { gcpCredentials, gcpUserinfo = restoreCreds, restoreUserinfo })
}

// userinfoOf answers as Google's userinfo endpoint does, the account a token was issued to: the
// account the stub gcloud lent a token for (stubTokenPrefix), and email for any other token.
func userinfoOf(email string) func(context.Context, *google.Credentials) (string, error) {
	return func(_ context.Context, creds *google.Credentials) (string, error) {
		token, err := creds.TokenSource.Token()
		if err != nil {
			return "", err
		}
		if account, ok := strings.CutPrefix(token.AccessToken, stubTokenPrefix); ok {
			return account, nil
		}
		return email, nil
	}
}

// stubTokenPrefix starts every access token the stub gcloud lends (useGcloud): it lends account
// the token stubTokenPrefix + account, so a test reads whose login a call ran under.
const stubTokenPrefix = "stub-gcloud-token-for-"

// machineEmail is the service account a machine's own Application Default Credentials and its
// own gcloud login name.
const machineEmail = "robot@acme-agent-infra.iam.gserviceaccount.com"

// machineADCKinds are the Application Default Credentials that are a machine's own identity, as
// useGCPIdentity takes them: a service account key file, a workload identity federation credential
// (a dev box's), and the metadata server's identity, which carries no JSON.
var machineADCKinds = []string{"service_account", "external_account", ""}

// Federation logins as `gcloud auth list` names them after `gcloud auth login --cred-file`: by the
// principal identifier token introspection answers, a workforce one ending in the person's email.
const (
	workloadPrincipal  = "principal://iam.googleapis.com/projects/100000000001/locations/global/workloadIdentityPools/acme-pool/subject/robot"
	workforcePrincipal = "principal://iam.googleapis.com/locations/global/workforcePools/acme-staff/subject/ada@example.com"
	workforceSet       = "principalSet://iam.googleapis.com/locations/global/workforcePools/acme-staff/group/admins@example.com"
)

// gcloudVariables are the variables the stub gcloud records from its environment on every run:
// those that would make gcloud lend another identity's token, and those that keep it from
// prompting, from writing log files and from printing its HTTP traffic.
var gcloudVariables = []string{
	"CLOUDSDK_AUTH_ACCESS_TOKEN",
	"CLOUDSDK_AUTH_ACCESS_TOKEN_FILE",
	"CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE",
	"CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT",
	"CLOUDSDK_CORE_DISABLE_FILE_LOGGING",
	"CLOUDSDK_CORE_DISABLE_PROMPTS",
	"CLOUDSDK_CORE_LOG_HTTP",
}

// gcloudStub is a stand-in gcloud alone on PATH (useGcloud).
type gcloudStub struct{ dir string }

// useGcloud puts a stub gcloud alone on PATH for the rest of t, holding a login for each of
// accounts, listed as `gcloud auth list --format=json` lists them (the first one active). It
// answers `auth list` with them and `auth print-access-token … <account>` with stubTokenPrefix plus
// that account - or plus STUB_GCLOUD_LENDS_AS when that is set, as gcloud lends another identity's
// token under impersonation or an access token override - refuses any other command, and records
// every command line it runs and, for each run, gcloudVariables as its environment holds them. It
// also clears the variables a developer's own shell could set to steer the sign-in,
// GOOGLE_APPLICATION_CREDENTIALS and CLOUDSDK_CORE_ACCOUNT, so no test reaches the developer's own
// gcloud or credentials.
func useGcloud(t *testing.T, accounts ...string) gcloudStub {
	t.Helper()
	dir := t.TempDir()
	type listed struct {
		Account string `json:"account"`
		Status  string `json:"status"`
	}
	list := []listed{}
	for i, account := range accounts {
		entry := listed{Account: account}
		if i == 0 {
			entry.Status = "ACTIVE"
		}
		list = append(list, entry)
	}
	answer, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	environments := shellWord(filepath.Join(dir, "environments"))
	script := strings.Join([]string{
		"#!/bin/sh",
		`printf '%s\n' "$*" >>` + shellWord(filepath.Join(dir, "commands")),
		`for name in ` + strings.Join(gcloudVariables, " ") + `; do eval "value=\${$name-unset}"; printf '%s=%s\n' "$name" "$value"; done >>` + environments,
		`printf '%s\n' -- >>` + environments,
		`case "$1 $2" in`,
		`"auth list") printf '%s\n' ` + shellWord(string(answer)) + ` ;;`,
		`"auth print-access-token") for word; do account=$word; done; printf '` + stubTokenPrefix + `%s\n' "${STUB_GCLOUD_LENDS_AS:-$account}" ;;`,
		`*) printf 'stub gcloud: unexpected command: %s\n' "$*" >&2; exit 2 ;;`,
		"esac",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("CLOUDSDK_CORE_ACCOUNT", "")
	return gcloudStub{dir}
}

// environments is, for each run of the stub in order, gcloudVariables as its environment held
// them, "unset" for one it did not hold.
func (g gcloudStub) environments(t *testing.T) []map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(g.dir, "environments"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var runs []map[string]string
	run := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "--" {
			runs, run = append(runs, run), map[string]string{}
			continue
		}
		name, value, _ := strings.Cut(line, "=")
		run[name] = value
	}
	return runs
}

// useNoGcloud leaves PATH holding no gcloud for the rest of t, and clears the variables useGcloud
// clears.
func useNoGcloud(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("CLOUDSDK_CORE_ACCOUNT", "")
}

// commands is every command line the stub ran, in order.
func (g gcloudStub) commands(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(g.dir, "commands"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// lent is each account the stub was asked for an access token of, in order.
func (g gcloudStub) lent(t *testing.T) []string {
	t.Helper()
	var accounts []string
	for _, command := range g.commands(t) {
		if words := strings.Fields(command); len(words) > 2 && words[0] == "auth" && words[1] == "print-access-token" {
			accounts = append(accounts, words[len(words)-1])
		}
	}
	return accounts
}

// refuseValueReads makes reading a secret's value fail t for the rest of it (failingStdin): no
// value may be asked for before the sign-in is settled.
func refuseValueReads(t *testing.T) {
	t.Helper()
	restore := secretStdin
	secretStdin = failingStdin{t}
	t.Cleanup(func() { secretStdin = restore })
}

func alwaysServed(name string) Reread { return Reread{Name: name, Served: true} }

func TestGCPSecretCreateShowSetRetagDeleteRestoreLifecycle(t *testing.T) {
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	useStdin(t, "shh")
	stdout, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent")
	if code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	// Each line names the store it wrote to, so an answer of this CLI's is never read as
	// another secrets CLI's (source.go).
	if !strings.Contains(stdout, "created SCRATCH_TEST in GCP Secret Manager project ") || !strings.Contains(stdout, "(owner=ada@example.com, tier=agent)") {
		t.Fatalf("create stdout: %q", stdout)
	}

	stdout, stderr, code = runSecret("show", "SCRATCH_TEST")
	if code != 0 {
		t.Fatalf("show: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "owner: ada@example.com") || !strings.Contains(stdout, "tier: agent") {
		t.Fatalf("show stdout: %q", stdout)
	}
	if strings.Contains(stdout, "shh") {
		t.Fatalf("show printed the value: %q", stdout)
	}

	useStdin(t, "shh2")
	_, stderr, code = runSecret("set", "SCRATCH_TEST")
	if code != 0 {
		t.Fatalf("set: exit %d, stderr %q", code, stderr)
	}

	stdout, stderr, code = runSecret("retag", "SCRATCH_TEST", "--tier", "human")
	if code != 0 {
		t.Fatalf("retag: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "retagged SCRATCH_TEST in GCP Secret Manager project ") || !strings.Contains(stdout, "(owner=ada@example.com, tier=human)") {
		t.Fatalf("retag stdout: %q", stdout)
	}

	stdout, stderr, code = runSecret("delete", "SCRATCH_TEST")
	if code != 0 {
		t.Fatalf("delete: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "deleted SCRATCH_TEST in GCP Secret Manager project ") {
		t.Fatalf("delete stdout: %q", stdout)
	}

	stdout, stderr, code = runSecret("show", "SCRATCH_TEST")
	if code != 0 {
		t.Fatalf("show after delete: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "deleted:") {
		t.Fatalf("show after delete did not print deleted: %q", stdout)
	}
	if strings.Contains(stdout, "earliest_purge") {
		t.Fatalf("GCP show printed an AWS-only earliest_purge line: %q", stdout)
	}

	_, stderr, code = runSecret("restore", "SCRATCH_TEST")
	if code != 0 {
		t.Fatalf("restore: exit %d, stderr %q", code, stderr)
	}

	stdout, stderr, code = runSecret("show", "SCRATCH_TEST")
	if code != 0 {
		t.Fatalf("show after restore: exit %d, stderr %q", code, stderr)
	}
	if strings.Contains(stdout, "deleted:") {
		t.Fatalf("show after restore still shows deleted: %q", stdout)
	}
}

// ownerOf is the owner annotation of the secret f holds under slug, "" when it holds none.
func (f *fakeSecretManager) ownerOf(slug string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.secrets[testGCPPrefix+slug]
	if !ok {
		return ""
	}
	return entry.secret.GetAnnotations()[policy.TagOwner]
}

// requireNothingWritten fails t unless Secret Manager holds no secret and the broker was asked
// for no reread: what a refused write leaves.
func requireNothingWritten(t *testing.T, fake *fakeSecretManager, broker *secretBroker) {
	t.Helper()
	if n := fake.written(); n != 0 {
		t.Fatalf("Secret Manager holds %d secrets after a refused write", n)
	}
	if rereads, _ := broker.recorded(); len(rereads) != 0 {
		t.Fatalf("rereads = %v after a refused write, want none", rereads)
	}
}

// requireRunUnder fails t unless every Secret Manager call so far, and at least one, carried the
// token the stub gcloud lends account, and gcloud lent tokens for account alone.
func requireRunUnder(t *testing.T, fake *fakeSecretManager, gcloud gcloudStub, account string) {
	t.Helper()
	if got, calls := fake.calledUnder(); got != "Bearer "+stubTokenPrefix+account || calls == 0 {
		t.Fatalf("Secret Manager calls ran under %q (%d calls), want only %s's gcloud token", got, calls, account)
	}
	lent := gcloud.lent(t)
	if len(lent) == 0 || slices.ContainsFunc(lent, func(a string) bool { return a != account }) {
		t.Fatalf("gcloud lent tokens for %v, want %s's alone", lent, account)
	}
}

// TestGCPSecretWriteUnderAMachineIdentityUsesThePersonsGcloudLogin: a write under Application
// Default Credentials that are a machine's own identity - a service account key, a workload
// identity federation credential (a dev box's own), or the metadata server's - runs under the
// person's own gcloud login, and never the machine's own gcloud account: every Secret Manager
// call carries the token gcloud lends that login, --owner me is the email Google answers for
// that token, and the broker is asked to reread. gcloud lends one token, which the command reuses
// for every call (each gcloud run costs seconds).
func TestGCPSecretWriteUnderAMachineIdentityUsesThePersonsGcloudLogin(t *testing.T) {
	for _, kind := range machineADCKinds {
		t.Run(cmp.Or(kind, "metadata server"), func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			useGCPIdentity(t, kind, machineEmail)
			gcloud := useGcloud(t, machineEmail, "ada@example.com")
			broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			useStdin(t, "shh")

			stdout, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent")
			if code != 0 {
				t.Fatalf("exit %d, stderr %q", code, stderr)
			}
			if owner := fake.ownerOf("scratch-test"); owner != "ada@example.com" {
				t.Fatalf("owner annotation %q, want ada@example.com; stdout %q", owner, stdout)
			}
			requireRunUnder(t, fake, gcloud, "ada@example.com")
			if lent := gcloud.lent(t); len(lent) != 1 {
				t.Fatalf("gcloud lent %d tokens, want one reused for every call", len(lent))
			}
			if rereads, answers := broker.recorded(); !slices.Equal(rereads, []string{"SCRATCH_TEST"}) || !answers[0].Served {
				t.Fatalf("rereads %v answered %v, want SCRATCH_TEST served", rereads, answers)
			}
		})
	}
}

// TestGCPSecretRetagUnderAMachineIdentityUsesThePersonsGcloudLogin: retag's read of the tags and
// its etag-bound write run under the person's gcloud login too, like every write: every Secret
// Manager call carries the token gcloud lends that login. A retag another writer races between its
// read and its write is refused (errTagsChanged) under that same login, and writes nothing.
func TestGCPSecretRetagUnderAMachineIdentityUsesThePersonsGcloudLogin(t *testing.T) {
	for _, raced := range []bool{false, true} {
		t.Run(map[bool]string{false: "alone", true: "raced"}[raced], func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			useGCPIdentity(t, "external_account", machineEmail)
			gcloud := useGcloud(t, machineEmail, "ada@example.com")
			startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			id := testGCPPrefix + "scratch-test"
			held := map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierAgent}
			fake.secrets[id] = &fakeSecret{
				secret:      &secretmanagerpb.Secret{Name: id, Annotations: maps.Clone(held), Etag: "1"},
				versions:    []*secretmanagerpb.SecretVersion{{Name: id + "/versions/1", State: secretmanagerpb.SecretVersion_ENABLED}},
				etagCounter: 1,
			}
			if raced {
				fake.race = func(entry *fakeSecret) {
					entry.secret.Annotations = map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierHuman}
					entry.etagCounter++
					entry.secret.Etag = "2"
				}
			}
			_, stderr, code := runSecret("retag", "SCRATCH_TEST", "--max-grant", "1h")
			fake.mu.Lock()
			annotations := maps.Clone(fake.secrets[id].secret.GetAnnotations())
			fake.mu.Unlock()
			if raced {
				if code != 1 || !strings.Contains(stderr, "another write changed the secret after this retag read it") {
					t.Fatalf("raced retag: exit %d stderr %q; want 1 naming the change", code, stderr)
				}
				if _, ok := annotations[policy.TagMaxGrant]; ok {
					t.Fatalf("annotations after a refused retag = %v; want the other writer's, with no max-grant", annotations)
				}
			} else if code != 0 || annotations[policy.TagMaxGrant] != "1h" {
				t.Fatalf("retag: exit %d stderr %q, annotations %v; want max-grant 1h", code, stderr, annotations)
			}
			requireRunUnder(t, fake, gcloud, "ada@example.com")
		})
	}
}

// TestGCPSecretWriteUnderAMachineIdentityWithoutAPersonsGcloudLoginIsRefused: where gcloud holds
// no person's login - none at all, or only the machine's own service account - or there is no
// gcloud on PATH, a write under a machine's own Application Default Credentials is refused (exit
// 1) before the value is read, before anything is written to Secret Manager, and before the
// broker is asked to reread, and gcloud lends no token.
func TestGCPSecretWriteUnderAMachineIdentityWithoutAPersonsGcloudLoginIsRefused(t *testing.T) {
	for _, kind := range machineADCKinds {
		for _, row := range []struct {
			name   string
			gcloud func(t *testing.T) *gcloudStub
		}{
			{"gcloud holds no login", func(t *testing.T) *gcloudStub { g := useGcloud(t); return &g }},
			{"gcloud holds the machine's own login alone", func(t *testing.T) *gcloudStub { g := useGcloud(t, machineEmail); return &g }},
			{"no gcloud on PATH", func(t *testing.T) *gcloudStub { useNoGcloud(t); return nil }},
		} {
			t.Run(cmp.Or(kind, "metadata server")+"/"+row.name, func(t *testing.T) {
				fake := newFakeSecretManager()
				startFakeSecretManager(t, fake)
				useGCPIdentity(t, kind, machineEmail)
				gcloud := row.gcloud(t)
				broker := startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
				refuseValueReads(t)

				_, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent")
				if code != 1 {
					t.Fatalf("exit %d, stderr %q", code, stderr)
				}
				requireNothingWritten(t, fake, broker)
				if gcloud != nil {
					if lent := gcloud.lent(t); len(lent) != 0 {
						t.Fatalf("gcloud lent tokens for %v, want none", lent)
					}
				}
			})
		}
	}
}

// TestGCPSecretGcloudLoginIsAPlainEmailLogin: a workload or workforce identity federation login,
// which gcloud names by a principal:// identifier (a workforce one ending in an email), is no
// person's login, nor a principalSet:// group: beside the person's own login, a write runs under
// that login with no several-logins refusal, and where one of them is gcloud's only login beside
// the machine's own, a write is refused (exit 1) before the value is read, writing nothing and
// with no token lent.
func TestGCPSecretGcloudLoginIsAPlainEmailLogin(t *testing.T) {
	federation := []string{workloadPrincipal, workforcePrincipal, workforceSet}
	t.Run("beside a person's login", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		useGCPIdentity(t, "external_account", machineEmail)
		gcloud := useGcloud(t, append(append([]string{machineEmail}, federation...), "ada@example.com")...)
		startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
		useStdin(t, "shh")

		if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		if owner := fake.ownerOf("scratch-test"); owner != "ada@example.com" {
			t.Fatalf("owner annotation %q, want ada@example.com", owner)
		}
		requireRunUnder(t, fake, gcloud, "ada@example.com")
	})
	for _, login := range federation {
		t.Run("alone: "+login, func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			useGCPIdentity(t, "external_account", machineEmail)
			gcloud := useGcloud(t, machineEmail, login)
			broker := startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
			refuseValueReads(t)

			if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent"); code != 1 {
				t.Fatalf("exit %d, stderr %q", code, stderr)
			}
			requireNothingWritten(t, fake, broker)
			if lent := gcloud.lent(t); len(lent) != 0 {
				t.Fatalf("gcloud lent tokens for %v, want none", lent)
			}
		})
	}
}

// TestGCPSecretGcloudRunsWithoutIdentityOverridesOrLogFiles: gcloud never runs with the variables
// that make it lend another identity's token, though this command's environment sets each of them,
// and always runs with file logging, HTTP logging and prompts off, though this command's
// environment turns file logging and HTTP logging on, its token redaction off; its token command
// prints the token in the private format, which bypasses gcloud's log file, and clears
// impersonation, an access token file and a credential file override with their empty flags,
// which outrank a configuration file that sets them.
func TestGCPSecretGcloudRunsWithoutIdentityOverridesOrLogFiles(t *testing.T) {
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useGCPIdentity(t, "external_account", machineEmail)
	gcloud := useGcloud(t, "ada@example.com")
	t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", "someone-elses-token")
	t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN_FILE", "/tokens/robot")
	t.Setenv("CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE", "/keys/robot.json")
	t.Setenv("CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT", machineEmail)
	t.Setenv("CLOUDSDK_CORE_DISABLE_FILE_LOGGING", "false")
	t.Setenv("CLOUDSDK_CORE_LOG_HTTP", "true")
	t.Setenv("CLOUDSDK_CORE_LOG_HTTP_REDACT_TOKEN", "false")
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	useStdin(t, "shh")

	if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	runs := gcloud.environments(t)
	if len(runs) == 0 {
		t.Fatal("gcloud never ran")
	}
	want := map[string]string{
		"CLOUDSDK_AUTH_ACCESS_TOKEN":                "unset",
		"CLOUDSDK_AUTH_ACCESS_TOKEN_FILE":           "unset",
		"CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE":    "unset",
		"CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT": "unset",
		"CLOUDSDK_CORE_DISABLE_FILE_LOGGING":        "1",
		"CLOUDSDK_CORE_DISABLE_PROMPTS":             "1",
		"CLOUDSDK_CORE_LOG_HTTP":                    "false",
	}
	for i, run := range runs {
		for name, value := range want {
			if run[name] != value {
				t.Errorf("gcloud run %d (%s): %s=%q, want %q", i, gcloud.commands(t)[i], name, run[name], value)
			}
		}
	}
	var token []string
	for _, command := range gcloud.commands(t) {
		if words := strings.Fields(command); len(words) > 1 && words[1] == "print-access-token" {
			token = words
		}
	}
	for _, word := range []string{"--format=value[private](token)", "--impersonate-service-account=", "--access-token-file=", "--credential-file-override="} {
		if !slices.Contains(token, word) {
			t.Errorf("gcloud's token command %q lacks %s", token, word)
		}
	}
	if len(token) == 0 || token[len(token)-1] != "ada@example.com" {
		t.Errorf("gcloud's token command %q does not end with the account", token)
	}
}

// TestGCPSecretWriteRefusesAGcloudTokenOfAnotherAccount: when gcloud, asked for the person's
// login, lends a token Google answers as another account's - a service account's or another
// person's - a write is refused (exit 1) before the value is read, writing nothing, under a
// machine's Application Default Credentials and with none at all. A token Google answers as the
// same account in another case writes, owned by that account in lowercase.
func TestGCPSecretWriteRefusesAGcloudTokenOfAnotherAccount(t *testing.T) {
	for _, adc := range []struct {
		name string
		use  func(t *testing.T)
	}{
		{"under a machine's ADC", func(t *testing.T) { useGCPIdentity(t, "external_account", machineEmail) }},
		{"with no ADC", useNoADC},
	} {
		for _, lendsAs := range []string{machineEmail, "grace@example.com"} {
			t.Run(adc.name+"/lent as "+lendsAs, func(t *testing.T) {
				fake := newFakeSecretManager()
				startFakeSecretManager(t, fake)
				adc.use(t)
				useGcloud(t, "ada@example.com")
				t.Setenv("STUB_GCLOUD_LENDS_AS", lendsAs)
				broker := startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
				refuseValueReads(t)

				_, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent")
				if code != 1 {
					t.Fatalf("exit %d, stderr %q", code, stderr)
				}
				requireNothingWritten(t, fake, broker)
			})
		}
	}
	t.Run("lent as the same account in another case", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		useGCPIdentity(t, "external_account", machineEmail)
		useGcloud(t, "ada@example.com")
		t.Setenv("STUB_GCLOUD_LENDS_AS", "Ada@Example.com")
		startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
		useStdin(t, "shh")

		if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		if owner := fake.ownerOf("scratch-test"); owner != "ada@example.com" {
			t.Fatalf("owner annotation %q, want ada@example.com", owner)
		}
	})
}

// TestGCPSecretPipeCommandKeepsTheGcloudAccountChoice: the command a write prints for piping a
// value in, after refusing a value of more than one line at the prompt, starts with the
// CLOUDSDK_CORE_ACCOUNT that chose the gcloud login, shell-quoted, so running it picks the same
// login among several; with a single login it chose none, and the command carries none.
func TestGCPSecretPipeCommandKeepsTheGcloudAccountChoice(t *testing.T) {
	for _, row := range []struct {
		name     string
		accounts []string
		named    string
		want     string
	}{
		{"several logins, one named", []string{machineEmail, "ada@example.com", "grace@example.com"}, "grace@example.com",
			"CLOUDSDK_CORE_ACCOUNT=grace@example.com agent-secrets secret create SCRATCH_TEST --owner me --tier agent < FILE"},
		{"a named login that needs quoting", []string{"ada@example.com", "grace+ops@example.com"}, "grace+ops@example.com",
			"CLOUDSDK_CORE_ACCOUNT='grace+ops@example.com' agent-secrets secret create SCRATCH_TEST --owner me --tier agent < FILE"},
		{"one login, none named", []string{machineEmail, "ada@example.com"}, "",
			": agent-secrets secret create SCRATCH_TEST --owner me --tier agent < FILE"},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			useGCPIdentity(t, "external_account", machineEmail)
			useGcloud(t, row.accounts...)
			t.Setenv("CLOUDSDK_CORE_ACCOUNT", row.named)
			broker := startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
			useTerminal(t)
			readHidden = func(int, func(), func()) ([]byte, error) { return nil, errMoreThanOneLine }

			_, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent")
			if code != exitUsageError || !strings.Contains(stderr, row.want) {
				t.Fatalf("exit %d, stderr %q; want %d naming %q", code, stderr, exitUsageError, row.want)
			}
			requireNothingWritten(t, fake, broker)
		})
	}
}

// TestGCPSecretWriteWithSeveralPeoplesGcloudLogins: where gcloud holds more than one person's
// login, a write runs under the one gcloud's own CLOUDSDK_CORE_ACCOUNT names. With none of them
// named it is refused (exit 1), naming each, before the value is read, writing nothing and with no
// token lent.
func TestGCPSecretWriteWithSeveralPeoplesGcloudLogins(t *testing.T) {
	for _, row := range []struct{ name, named string }{
		{"no account named", ""},
		{"the machine's own account named", machineEmail},
		{"an account gcloud holds no login for named", "linus@example.com"},
	} {
		t.Run("refused with "+row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			useGCPIdentity(t, "external_account", machineEmail)
			gcloud := useGcloud(t, machineEmail, "ada@example.com", "grace@example.com")
			t.Setenv("CLOUDSDK_CORE_ACCOUNT", row.named)
			broker := startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
			refuseValueReads(t)

			_, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent")
			if code != 1 {
				t.Fatalf("exit %d, stderr %q", code, stderr)
			}
			if !strings.Contains(stderr, "ada@example.com") || !strings.Contains(stderr, "grace@example.com") {
				t.Fatalf("the refusal does not name each person's login: %q", stderr)
			}
			requireNothingWritten(t, fake, broker)
			if lent := gcloud.lent(t); len(lent) != 0 {
				t.Fatalf("gcloud lent tokens for %v, want none", lent)
			}
		})
	}
	t.Run("the one CLOUDSDK_CORE_ACCOUNT names", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		useGCPIdentity(t, "external_account", machineEmail)
		gcloud := useGcloud(t, machineEmail, "ada@example.com", "grace@example.com")
		t.Setenv("CLOUDSDK_CORE_ACCOUNT", "grace@example.com")
		startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
		useStdin(t, "shh")

		if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		if owner := fake.ownerOf("scratch-test"); owner != "grace@example.com" {
			t.Fatalf("owner annotation %q, want grace@example.com", owner)
		}
		requireRunUnder(t, fake, gcloud, "grace@example.com")
	})
}

// TestGCPSecretWithNoApplicationDefaultCredentialsUsesThePersonsGcloudLogin: where there are no
// Application Default Credentials and GOOGLE_APPLICATION_CREDENTIALS is unset, every form, a read
// included, runs under the person's gcloud login.
func TestGCPSecretWithNoApplicationDefaultCredentialsUsesThePersonsGcloudLogin(t *testing.T) {
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useNoADC(t)
	gcloud := useGcloud(t, machineEmail, "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	held, err := fake.CreateSecret(context.Background(), &secretmanagerpb.CreateSecretRequest{
		Parent: "projects/" + testGCPProject, SecretId: "agent-secrets-demo-deploy-token",
		Secret: &secretmanagerpb.Secret{Annotations: map[string]string{policy.TagOwner: "grace@example.com", policy.TagTier: "agent"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fake.AddSecretVersion(context.Background(), &secretmanagerpb.AddSecretVersionRequest{Parent: held.GetName()}); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runSecret("list")
	if code != 0 {
		t.Fatalf("list: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "DEMO_DEPLOY_TOKEN") {
		t.Fatalf("list stdout: %q", stdout)
	}
	requireRunUnder(t, fake, gcloud, "ada@example.com")

	useStdin(t, "shh")
	if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	if owner := fake.ownerOf("scratch-test"); owner != "ada@example.com" {
		t.Fatalf("owner annotation %q, want ada@example.com", owner)
	}
	requireRunUnder(t, fake, gcloud, "ada@example.com")
}

// TestGCPSecretWithNoApplicationDefaultCredentialsAndNoGcloudLoginIsRefused: with no Application
// Default Credentials, and no person's gcloud login either, a read is refused (exit 1) before any
// Secret Manager call.
func TestGCPSecretWithNoApplicationDefaultCredentialsAndNoGcloudLoginIsRefused(t *testing.T) {
	for _, row := range []struct {
		name   string
		gcloud func(t *testing.T)
	}{
		{"gcloud holds the machine's own login alone", func(t *testing.T) { useGcloud(t, machineEmail) }},
		{"no gcloud on PATH", useNoGcloud},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			useNoADC(t)
			row.gcloud(t)
			startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)

			_, stderr, code := runSecret("list")
			if code != 1 {
				t.Fatalf("exit %d, stderr %q", code, stderr)
			}
			if _, calls := fake.calledUnder(); calls != 0 {
				t.Fatalf("Secret Manager got %d calls, want none", calls)
			}
		})
	}
}

// TestGCPSecretUnderGoogleApplicationCredentialsNeverAsksGcloud: GOOGLE_APPLICATION_CREDENTIALS
// is the person's own choice of credentials, so the gcloud login never stands in for it, though
// gcloud holds a person's login: a file there that cannot be loaded is refused, and so is a write
// under a machine's credentials named there, each before any Secret Manager call and before the
// value is read, with gcloud never run.
func TestGCPSecretUnderGoogleApplicationCredentialsNeverAsksGcloud(t *testing.T) {
	t.Run("a file that cannot be loaded", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		useNoADC(t)
		gcloud := useGcloud(t, "ada@example.com")
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "absent.json"))
		startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)

		_, stderr, code := runSecret("list")
		if code != 1 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		if _, calls := fake.calledUnder(); calls != 0 {
			t.Fatalf("Secret Manager got %d calls, want none", calls)
		}
		if commands := gcloud.commands(t); len(commands) != 0 {
			t.Fatalf("gcloud ran %q, want nothing", commands)
		}
	})
	t.Run("a machine's credentials, for a write", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		useGCPIdentity(t, "service_account", machineEmail)
		gcloud := useGcloud(t, "ada@example.com")
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/keys/robot.json")
		broker := startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
		refuseValueReads(t)

		_, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent")
		if code != 1 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		requireNothingWritten(t, fake, broker)
		if commands := gcloud.commands(t); len(commands) != 0 {
			t.Fatalf("gcloud ran %q, want nothing", commands)
		}
	})
}

// TestGCPSecretWriteUnderAPersonsADCNeverAsksGcloud: a person's own Application Default
// Credentials (a laptop's) write as they always have, every call under them, with gcloud never
// run, though it holds another person's login.
func TestGCPSecretWriteUnderAPersonsADCNeverAsksGcloud(t *testing.T) {
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	gcloud := useGcloud(t, "grace@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	useStdin(t, "shh")

	if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if owner := fake.ownerOf("scratch-test"); owner != "ada@example.com" {
		t.Fatalf("owner annotation %q, want ada@example.com", owner)
	}
	if got, calls := fake.calledUnder(); got != "Bearer fake" || calls == 0 {
		t.Fatalf("Secret Manager calls ran under %q (%d calls), want only the person's own ADC", got, calls)
	}
	if commands := gcloud.commands(t); len(commands) != 0 {
		t.Fatalf("gcloud ran %q, want nothing", commands)
	}
}

// TestGCPSecretListSucceedsWithoutUserinfoScope: a read makes no userinfo call, so it succeeds
// even under a token that lacks the scope that call needs (a machine identity's commonly does),
// and it runs under the machine's own identity, which can read, with gcloud never run.
func TestGCPSecretListSucceedsWithoutUserinfoScope(t *testing.T) {
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	restoreCreds, restoreUserinfo := gcpCredentials, gcpUserinfo
	gcpCredentials = func(context.Context) (*google.Credentials, error) {
		return &google.Credentials{TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake"})}, nil
	}
	gcpUserinfo = func(context.Context, *google.Credentials) (string, error) {
		return "", errors.New("insufficient authentication scopes")
	}
	t.Cleanup(func() { gcpCredentials, gcpUserinfo = restoreCreds, restoreUserinfo })
	gcloud := useGcloud(t, "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)

	_, stderr, code := runSecret("list")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if got, calls := fake.calledUnder(); got != "Bearer fake" || calls == 0 {
		t.Fatalf("Secret Manager calls ran under %q (%d calls), want only the machine's own ADC", got, calls)
	}
	if commands := gcloud.commands(t); len(commands) != 0 {
		t.Fatalf("gcloud ran %q, want nothing", commands)
	}
}

// TestGCPSecretRetagKeepsUnrelatedAnnotations: retag changes only owner and tier, never an
// annotation it does not know about (an UpdateSecret whose mask names "annotations" replaces the
// whole map unless retag reads and merges the current one first).
func TestGCPSecretRetagKeepsUnrelatedAnnotations(t *testing.T) {
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	useStdin(t, "shh")
	if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}

	id := testGCPPrefix + "scratch-test"
	fake.mu.Lock()
	fake.secrets[id].secret.Annotations["unrelated-note"] = "do-not-delete"
	fake.mu.Unlock()

	if _, stderr, code := runSecret("retag", "SCRATCH_TEST", "--tier", "human"); code != 0 {
		t.Fatalf("retag: exit %d, stderr %q", code, stderr)
	}

	fake.mu.Lock()
	got := fake.secrets[id].secret.Annotations["unrelated-note"]
	fake.mu.Unlock()
	if got != "do-not-delete" {
		t.Fatalf("retag dropped an unrelated annotation: got %q", got)
	}
}

// TestGCPSecretOwnerMeIsTheSignInEmailLowercased: --owner me is the userinfo email, lowercased.
func TestGCPSecretOwnerMeIsTheSignInEmailLowercased(t *testing.T) {
	startFakeSecretManager(t, newFakeSecretManager())
	useGCPIdentity(t, "authorized_user", "Ada@Example.com")
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	useStdin(t, "shh")

	stdout, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "owner=ada@example.com") {
		t.Fatalf("stdout: %q", stdout)
	}
}

// TestGCPSecretRetagOnlyNamedFieldReplacesHeldOne: retag with only --tier keeps the held owner,
// sending both as one UpdateSecret call (the annotations map is written whole).
func TestGCPSecretRetagOnlyNamedFieldReplacesHeldOne(t *testing.T) {
	startFakeSecretManager(t, newFakeSecretManager())
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	useStdin(t, "shh")

	if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "shared", "--tier", "agent"); code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	stdout, stderr, code := runSecret("retag", "SCRATCH_TEST", "--tier", "human")
	if code != 0 {
		t.Fatalf("retag: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "owner=shared, tier=human") {
		t.Fatalf("stdout: %q", stdout)
	}
}

// TestGCPSecretRetagOfASharedSecretSaysAnAdministratorChangesIt: IAM (PermissionDenied) refuses a
// person's change to a shared secret's annotations; the CLI prints GCP's refusal and says whose
// change it is.
func TestGCPSecretRetagOfASharedSecretSaysAnAdministratorChangesIt(t *testing.T) {
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	useStdin(t, "shh")

	if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "shared", "--tier", "agent"); code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	fake.mu.Lock()
	fake.refuseAnnotationUpdates = true
	fake.mu.Unlock()

	_, stderr, code := runSecret("retag", "SCRATCH_TEST", "--tier", "human")
	if code != 1 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, "PermissionDenied") {
		t.Fatalf("stderr does not show GCP's refusal: %q", stderr)
	}
	if !strings.Contains(stderr, "a shared secret's owner and tier are an administrator's to change") {
		t.Fatalf("stderr does not say whose change it is: %q", stderr)
	}
}

// TestGCPSecretListShowsOwnerTierAndValue: list shows every secret under the prefix, its owner,
// tier and whether it has a value, never the value.
func TestGCPSecretListShowsOwnerTierAndValue(t *testing.T) {
	startFakeSecretManager(t, newFakeSecretManager())
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	useStdin(t, "shh")

	if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	stdout, stderr, code := runSecret("list")
	if code != 0 {
		t.Fatalf("list: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "SCRATCH_TEST") || !strings.Contains(stdout, "ada@example.com") || !strings.Contains(stdout, "agent") || !strings.Contains(stdout, "yes") {
		t.Fatalf("list stdout: %q", stdout)
	}
	if strings.Contains(stdout, "shh") {
		t.Fatalf("list printed the value: %q", stdout)
	}
}

// TestGCPSecretReadsRefuseBadSettings: an invalid kms_key_name is refused before any GCP call.
// TestCheckSettingsKMSKeyName covers the other checkSettingsKMSKeyName refusals directly.
func TestGCPSecretReadsRefuseBadSettings(t *testing.T) {
	startFakeSecretManager(t, newFakeSecretManager())
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, Settings{
		Store: secrets.StoreGCP, SecretsPrefix: testGCPPrefix, GCPProject: testGCPProject,
		KMSKeyName: "not-a-key-name",
	}, alwaysServed)

	_, stderr, code := runSecret("list")
	if code != 1 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, "invalid KMS key name") {
		t.Fatalf("stderr: %q", stderr)
	}
}

func TestCheckSettingsKMSKeyName(t *testing.T) {
	if err := checkSettingsKMSKeyName(gcpTestSettings()); err != nil {
		t.Fatalf("valid settings refused: %v", err)
	}
	bad := gcpTestSettings()
	bad.SecretsPrefix = "not-a-prefix"
	if err := checkSettingsKMSKeyName(bad); err == nil {
		t.Fatal("an invalid secrets_prefix was accepted")
	}
	mismatched := gcpTestSettings()
	mismatched.GCPProject = "999999999999"
	if err := checkSettingsKMSKeyName(mismatched); err == nil {
		t.Fatal("a gcp_project not matching secrets_prefix's project number was accepted")
	} else if !strings.Contains(err.Error(), "999999999999") || !strings.Contains(err.Error(), testGCPProject) {
		t.Fatalf("error does not name both projects: %v", err)
	}
	badKey := gcpTestSettings()
	badKey.KMSKeyName = "bad"
	if err := checkSettingsKMSKeyName(badKey); err == nil {
		t.Fatal("an invalid key name was accepted")
	}
}

// TestOpenSecretBackendRefusesUnknownStore: a store name neither backend names is refused before
// any cloud call.
func TestOpenSecretBackendRefusesUnknownStore(t *testing.T) {
	startSecretBrokerWith(t, Settings{Store: "azure-key-vault"}, alwaysServed)
	_, stderr, code := runSecret("list")
	if code != 1 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, `unknown secret store "azure-key-vault"`) {
		t.Fatalf("stderr: %q", stderr)
	}
}

// TestOpenSecretBackendRefusesProfileOnGCP: --profile names an AWS profile; a GCP broker refuses
// it as a usage error rather than silently ignoring it.
func TestOpenSecretBackendRefusesProfileOnGCP(t *testing.T) {
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	_, stderr, code := runSecret("list", "--profile", "some-aws-profile")
	if code != exitUsageError {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, "--profile names an AWS profile") || !strings.Contains(stderr, secrets.StoreGCP) {
		t.Fatalf("stderr: %q", stderr)
	}
}
