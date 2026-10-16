// packages/envoy/cmd/agent-secrets/secret_gcp_test.go
//
// The GCP secret forms run in-process here, against a fake Secret Manager gRPC server
// (fakeSecretManager) standing in for the real API, with gcpCredentials and gcpUserinfo replaced
// so no form makes a network call for its identity, and the shared fake broker (secretBroker,
// secret_test.go) answering GET /v1/settings with the GCP store's shape and the reread.
package main

import (
	"context"
	"net"
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
	secret   *secretmanagerpb.Secret
	versions []*secretmanagerpb.SecretVersion
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
	f.secrets[name] = &fakeSecret{secret: secret}
	return secret, nil
}

func (f *fakeSecretManager) GetSecret(_ context.Context, req *secretmanagerpb.GetSecretRequest) (*secretmanagerpb.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.secrets[req.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return entry.secret, nil
}

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
	for _, path := range req.GetUpdateMask().GetPaths() {
		switch path {
		case "annotations":
			entry.secret.Annotations = req.GetSecret().GetAnnotations()
		case "expire_time":
			entry.secret.Expiration = req.GetSecret().GetExpiration()
		}
	}
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
// points gcpClientDial at it, ignoring whatever credentials a test supplies: the fake takes no
// authentication.
func startFakeSecretManager(t *testing.T, f *fakeSecretManager) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer()
	secretmanagerpb.RegisterSecretManagerServiceServer(server, f)
	go server.Serve(lis)
	t.Cleanup(server.Stop)

	restore := gcpClientDial
	gcpClientDial = func(ctx context.Context, _ *google.Credentials) (*secretmanager.Client, error) {
		conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, err
		}
		return secretmanager.NewClient(ctx, option.WithGRPCConn(conn))
	}
	t.Cleanup(func() { gcpClientDial = restore })
}

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
		return Reread{Name: name, Served: true}
	}
}

// useGCPIdentity makes gcpCredentials and gcpUserinfo answer kind/email/project for the rest of t,
// with no network call. kind is the ADC JSON's "type" ("authorized_user" for a person's sign-in,
// "service_account" for a key file); "" leaves the JSON empty, as a GCE/GKE metadata or workload
// identity carries none.
func useGCPIdentity(t *testing.T, kind, email, project string) {
	t.Helper()
	restoreCreds, restoreUserinfo := gcpCredentials, gcpUserinfo
	gcpCredentials = func(context.Context) (*google.Credentials, error) {
		var creds google.Credentials
		creds.ProjectID = project
		if kind != "" {
			creds.JSON = []byte(`{"type":"` + kind + `"}`)
		}
		creds.TokenSource = oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake"})
		return &creds, nil
	}
	gcpUserinfo = func(context.Context, *google.Credentials) (string, error) { return email, nil }
	t.Cleanup(func() { gcpCredentials, gcpUserinfo = restoreCreds, restoreUserinfo })
}

func alwaysServed(name string) Reread { return Reread{Name: name, Served: true} }

func TestGCPSecretCreateShowSetRetagDeleteRestoreLifecycle(t *testing.T) {
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useGCPIdentity(t, "authorized_user", "ada@example.com", testGCPProject)
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	useStdin(t, "shh")
	stdout, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent")
	if code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "created SCRATCH_TEST (owner=ada@example.com, tier=agent)") {
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
	if !strings.Contains(stdout, "retagged SCRATCH_TEST (owner=ada@example.com, tier=human)") {
		t.Fatalf("retag stdout: %q", stdout)
	}

	stdout, stderr, code = runSecret("delete", "SCRATCH_TEST")
	if code != 0 {
		t.Fatalf("delete: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "deleted SCRATCH_TEST") {
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

// TestGCPSecretCreateRefusesServiceAccount: a write under Application Default Credentials that
// are not a person's `gcloud auth application-default login` (a service account's) is refused
// before anything is written.
func TestGCPSecretCreateRefusesServiceAccount(t *testing.T) {
	startFakeSecretManager(t, newFakeSecretManager())
	useGCPIdentity(t, "service_account", "robot@acme-agent-infra.iam.gserviceaccount.com", testGCPProject)
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	useStdin(t, "shh")

	_, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent")
	if code != 1 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, "not a person's sign-in") {
		t.Fatalf("stderr: %q", stderr)
	}
}

// TestGCPSecretCreateRefusesMetadataIdentity: ADC with no JSON at all (a GCE/GKE metadata-server
// or workload identity) is refused the same way.
func TestGCPSecretCreateRefusesMetadataIdentity(t *testing.T) {
	startFakeSecretManager(t, newFakeSecretManager())
	useGCPIdentity(t, "", "robot@acme-agent-infra.iam.gserviceaccount.com", testGCPProject)
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	useStdin(t, "shh")

	_, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent")
	if code != 1 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, "not a person's sign-in") {
		t.Fatalf("stderr: %q", stderr)
	}
}

// TestGCPSecretReadRefusesAnotherProjectNumber: a read under Application Default Credentials
// whose quota project is a NUMBER other than the broker's is refused, naming both.
func TestGCPSecretReadRefusesAnotherProjectNumber(t *testing.T) {
	startFakeSecretManager(t, newFakeSecretManager())
	useGCPIdentity(t, "authorized_user", "ada@example.com", "999999999999")
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)

	_, stderr, code := runSecret("list")
	if code != 1 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, "999999999999") || !strings.Contains(stderr, testGCPProject) {
		t.Fatalf("stderr does not name both projects: %q", stderr)
	}
}

// TestGCPSecretReadAllowsNoQuotaProject: ADC naming no quota project (the common case for a plain
// `gcloud auth application-default login`) is not refused.
func TestGCPSecretReadAllowsNoQuotaProject(t *testing.T) {
	startFakeSecretManager(t, newFakeSecretManager())
	useGCPIdentity(t, "authorized_user", "ada@example.com", "")
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)

	_, stderr, code := runSecret("list")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

// TestGCPSecretReadAllowsAProjectIDQuotaProject: ADC naming a project-ID-shaped (non-numeric)
// quota project is not refused, even though it is textually unequal to the broker's project
// NUMBER: this CLI cannot resolve one from the other without another API call, so it does not try.
func TestGCPSecretReadAllowsAProjectIDQuotaProject(t *testing.T) {
	startFakeSecretManager(t, newFakeSecretManager())
	useGCPIdentity(t, "authorized_user", "ada@example.com", "acme-agent-infra")
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)

	_, stderr, code := runSecret("list")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

// TestGCPSecretOwnerMeIsTheSignInEmailLowercased: --owner me is the userinfo email, lowercased.
func TestGCPSecretOwnerMeIsTheSignInEmailLowercased(t *testing.T) {
	startFakeSecretManager(t, newFakeSecretManager())
	useGCPIdentity(t, "authorized_user", "Ada@Example.com", testGCPProject)
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
	useGCPIdentity(t, "authorized_user", "ada@example.com", testGCPProject)
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
	useGCPIdentity(t, "authorized_user", "ada@example.com", testGCPProject)
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
	useGCPIdentity(t, "authorized_user", "ada@example.com", testGCPProject)
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

// TestGCPSecretReadsRefuseBadSettings: an invalid kms_key_name or a missing gcp_project is
// refused before any GCP call.
func TestGCPSecretReadsRefuseBadSettings(t *testing.T) {
	startFakeSecretManager(t, newFakeSecretManager())
	useGCPIdentity(t, "authorized_user", "ada@example.com", testGCPProject)
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
