package secrets

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	fakeProject = "projects/000000000000"
	fakePrefix  = fakeProject + "/secrets/agent-secrets-"
	fakeKey     = "projects/example-project/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets"
)

// fakeSecret is one secret the fake Secret Manager holds: its metadata, its latest version's state
// and key, and its value.
type fakeSecret struct {
	annotations map[string]string
	// replicationKey is the key the secret's replication policy names; "" is Google's default.
	replicationKey string
	// versionKey is the key the latest version was encrypted with; "" is Google's default.
	versionKey string
	// noVersion is a secret with no version at all.
	noVersion bool
	state     secretmanagerpb.SecretVersion_State
	// expires, when set, is the secret's expire_time.
	expires time.Time
	// denyVersion makes GetSecretVersion on the secret answer PERMISSION_DENIED, as for a secret
	// outside the namespace the broker's condition grants it.
	denyVersion bool
	// keyUnusable makes AccessSecretVersion answer FAILED_PRECONDITION on an ENABLED version, as
	// an unusable CMEK key might.
	keyUnusable bool
	value       []byte
}

// fakeSecretManager is the four Secret Manager calls the store makes, over gRPC with TLS, as the
// real service answers them: resource names with the project number, NOT_FOUND for a missing
// secret or version, FAILED_PRECONDITION for a disabled or destroyed version's access.
type fakeSecretManager struct {
	secretmanagerpb.UnimplementedSecretManagerServiceServer
	mu      sync.Mutex
	secrets map[string]*fakeSecret // keyed by the secret's resource name
	// failList, when set, is what ListSecrets answers.
	failList error
	// calls counts each method's calls, and unavailable is how many calls of each method still
	// answer UNAVAILABLE first, as a transient outage does.
	calls, unavailable map[string]int
}

// call counts one call of method and answers UNAVAILABLE while unavailable holds some for it.
// f.mu is held.
func (f *fakeSecretManager) call(method string) error {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[method]++
	if f.unavailable[method] > 0 {
		f.unavailable[method]--
		return status.Error(codes.Unavailable, "transient")
	}
	return nil
}

// reads is the calls made so far that Secret Manager counts against its 600-per-minute read
// quota: every call but AccessSecretVersion.
func (f *fakeSecretManager) reads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls["ListSecrets"] + f.calls["GetSecret"] + f.calls["GetSecretVersion"]
}

func (f *fakeSecretManager) ListSecrets(_ context.Context, req *secretmanagerpb.ListSecretsRequest) (*secretmanagerpb.ListSecretsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("ListSecrets"); err != nil {
		return nil, err
	}
	if f.failList != nil {
		return nil, f.failList
	}
	if req.GetParent() != fakeProject {
		return nil, status.Errorf(codes.NotFound, "project %q", req.GetParent())
	}
	needle, ok := strings.CutPrefix(req.GetFilter(), "name:")
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "filter %q", req.GetFilter())
	}
	resp := &secretmanagerpb.ListSecretsResponse{}
	for name, s := range f.secrets {
		id := name[strings.LastIndex(name, "/")+1:]
		// Secret Manager's ":" is a case-insensitive substring match.
		if !strings.Contains(strings.ToLower(id), strings.ToLower(needle)) {
			continue
		}
		resp.Secrets = append(resp.Secrets, f.secretProto(name, s))
	}
	resp.TotalSize = int32(len(resp.Secrets))
	return resp, nil
}

func (f *fakeSecretManager) secretProto(name string, s *fakeSecret) *secretmanagerpb.Secret {
	p := &secretmanagerpb.Secret{Name: name, Annotations: s.annotations}
	auto := &secretmanagerpb.Replication_Automatic{}
	if s.replicationKey != "" {
		auto.CustomerManagedEncryption = &secretmanagerpb.CustomerManagedEncryption{KmsKeyName: s.replicationKey}
	}
	p.Replication = &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: auto}}
	if !s.expires.IsZero() {
		p.Expiration = &secretmanagerpb.Secret_ExpireTime{ExpireTime: timestamppb.New(s.expires)}
	}
	return p
}

func (f *fakeSecretManager) GetSecret(_ context.Context, req *secretmanagerpb.GetSecretRequest) (*secretmanagerpb.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("GetSecret"); err != nil {
		return nil, err
	}
	s, ok := f.secrets[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "Secret [%s] not found", req.GetName())
	}
	return f.secretProto(req.GetName(), s), nil
}

// latest splits "<secret>/versions/latest" and answers the secret, or NOT_FOUND.
func (f *fakeSecretManager) latest(name string) (*fakeSecret, string, error) {
	secret, version, ok := strings.Cut(name, "/versions/")
	if !ok || version != "latest" {
		return nil, "", status.Errorf(codes.InvalidArgument, "version name %q", name)
	}
	s, found := f.secrets[secret]
	if !found || s.noVersion {
		return nil, "", status.Errorf(codes.NotFound, "Secret Version [%s] not found", name)
	}
	return s, secret, nil
}

func (f *fakeSecretManager) GetSecretVersion(_ context.Context, req *secretmanagerpb.GetSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("GetSecretVersion"); err != nil {
		return nil, err
	}
	s, secret, err := f.latest(req.GetName())
	if err != nil {
		return nil, err
	}
	if s.denyVersion {
		return nil, status.Errorf(codes.PermissionDenied, "Permission denied on %s", req.GetName())
	}
	v := &secretmanagerpb.SecretVersion{Name: secret + "/versions/1", State: s.state}
	auto := &secretmanagerpb.ReplicationStatus_AutomaticStatus{}
	if s.versionKey != "" {
		auto.CustomerManagedEncryption = &secretmanagerpb.CustomerManagedEncryptionStatus{KmsKeyVersionName: s.versionKey + "/cryptoKeyVersions/3"}
	}
	v.ReplicationStatus = &secretmanagerpb.ReplicationStatus{ReplicationStatus: &secretmanagerpb.ReplicationStatus_Automatic{Automatic: auto}}
	return v, nil
}

func (f *fakeSecretManager) AccessSecretVersion(_ context.Context, req *secretmanagerpb.AccessSecretVersionRequest) (*secretmanagerpb.AccessSecretVersionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("AccessSecretVersion"); err != nil {
		return nil, err
	}
	s, secret, err := f.latest(req.GetName())
	if err != nil {
		return nil, err
	}
	if s.state != secretmanagerpb.SecretVersion_ENABLED || s.keyUnusable {
		return nil, status.Errorf(codes.FailedPrecondition, "Secret Version [%s] is in %s state", req.GetName(), s.state)
	}
	return &secretmanagerpb.AccessSecretVersionResponse{Name: secret + "/versions/1", Payload: &secretmanagerpb.SecretPayload{Data: s.value}}, nil
}

// startFake serves f over TLS on a loopback port and answers the broker's own Secret Manager
// client (NewGCPClient, its retries included) dialled to it with no credentials.
func startFake(t *testing.T, f *fakeSecretManager) *secretmanager.Client {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake-secretmanager"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})))
	secretmanagerpb.RegisterSecretManagerServiceServer(srv, f)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "127.0.0.1"})))
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewGCPClient(context.Background(), option.WithGRPCConn(conn), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func servedSecret(value string) *fakeSecret {
	return &fakeSecret{
		annotations:    map[string]string{"owner": "ada@example.com", "tier": "human"},
		replicationKey: fakeKey, versionKey: fakeKey,
		state: secretmanagerpb.SecretVersion_ENABLED, value: []byte(value),
	}
}

func TestGCPListReadsAnnotationsKeyStateAndExpiry(t *testing.T) {
	f := &fakeSecretManager{secrets: map[string]*fakeSecret{
		fakePrefix + "served":      servedSecret("v1"),
		fakePrefix + "default-key": {annotations: map[string]string{"owner": "shared", "tier": "agent"}, state: secretmanagerpb.SecretVersion_ENABLED, value: []byte("v")},
		fakePrefix + "disabled":    {annotations: map[string]string{"owner": "shared", "tier": "agent"}, replicationKey: fakeKey, versionKey: fakeKey, state: secretmanagerpb.SecretVersion_DISABLED, value: []byte("v")},
		fakePrefix + "no-version":  {annotations: map[string]string{"owner": "shared", "tier": "agent"}, replicationKey: fakeKey, noVersion: true},
		fakePrefix + "expiring":    {annotations: map[string]string{"owner": "shared", "tier": "agent"}, replicationKey: fakeKey, versionKey: fakeKey, state: secretmanagerpb.SecretVersion_ENABLED, expires: time.Now().Add(30 * 24 * time.Hour), value: []byte("v")},
		fakePrefix + "expired":     {annotations: map[string]string{"owner": "shared", "tier": "agent"}, replicationKey: fakeKey, versionKey: fakeKey, state: secretmanagerpb.SecretVersion_ENABLED, expires: time.Now().Add(-time.Minute), value: []byte("v")},
		fakePrefix + "no-tags":     {replicationKey: fakeKey, versionKey: fakeKey, state: secretmanagerpb.SecretVersion_ENABLED, value: []byte("v")},
		// The name filter is a case-insensitive substring match, so these two are listed too;
		// neither is the broker's, and a version lookup on either is denied.
		fakeProject + "/secrets/other-agent-secrets-key": {denyVersion: true},
		fakeProject + "/secrets/AGENT-SECRETS-upper":     {denyVersion: true},
	}}
	store := NewGCP(startFake(t, f), fakeKey)
	ctx := context.Background()
	listings, err := store.List(ctx, fakePrefix)
	if err != nil {
		t.Fatalf("List = %v; want the secrets outside the prefix dropped before any call for them", err)
	}
	byID := map[string]Listing{}
	for _, ls := range listings {
		byID[ls.ID] = ls
	}
	if len(byID) != 5 {
		t.Fatalf("List answered %d secrets, want the 5 under the prefix with no expire_time: %+v", len(byID), listings)
	}
	onKey := func(ls Listing) bool {
		t.Helper()
		on, err := ls.OnKey(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return on
	}
	served := byID[fakePrefix+"served"]
	if served.Source != served.ID || served.Tags["owner"] != "ada@example.com" || served.Tags["tier"] != "human" || !onKey(served) || !served.HasCurrentValue {
		t.Fatalf("served = %+v; want on the key, with a value, tagged from its annotations", served)
	}
	if ls := byID[fakePrefix+"default-key"]; onKey(ls) || !ls.HasCurrentValue {
		t.Fatalf("default-key = %+v; want not on the key", ls)
	}
	if ls := byID[fakePrefix+"disabled"]; !onKey(ls) || ls.HasCurrentValue {
		t.Fatalf("disabled = %+v; want on the key with no current value", ls)
	}
	if ls := byID[fakePrefix+"no-version"]; !onKey(ls) || ls.HasCurrentValue {
		t.Fatalf("no-version = %+v; want on the key (its replication policy's) with no current value", ls)
	}
	for _, name := range []string{"expiring", "expired"} {
		if ls, listed := byID[fakePrefix+name]; listed {
			t.Fatalf("%s = %+v; want it left out from the moment it has an expire_time, as delete sets one", name, ls)
		}
	}
	if ls := byID[fakePrefix+"no-tags"]; len(ls.Tags) != 0 || ls.Tags == nil {
		t.Fatalf("no-tags = %+v; want an empty, non-nil tag map", ls)
	}
}

func TestGCPDescribeAndRead(t *testing.T) {
	f := &fakeSecretManager{secrets: map[string]*fakeSecret{
		fakePrefix + "served":   servedSecret("hello"),
		fakePrefix + "disabled": {annotations: map[string]string{}, replicationKey: fakeKey, versionKey: fakeKey, state: secretmanagerpb.SecretVersion_DISABLED, value: []byte("v")},
		fakePrefix + "empty":    {annotations: map[string]string{}, replicationKey: fakeKey, versionKey: fakeKey, state: secretmanagerpb.SecretVersion_ENABLED, value: nil},
		fakePrefix + "binary":   {annotations: map[string]string{}, replicationKey: fakeKey, versionKey: fakeKey, state: secretmanagerpb.SecretVersion_ENABLED, value: []byte{0xff, 0xfe}},
		fakePrefix + "expiring": {annotations: map[string]string{}, replicationKey: fakeKey, versionKey: fakeKey, state: secretmanagerpb.SecretVersion_ENABLED, expires: time.Now().Add(30 * 24 * time.Hour), value: []byte("v")},
	}}
	store := NewGCP(startFake(t, f), fakeKey)
	ctx := context.Background()

	ls, err := store.Describe(ctx, fakePrefix+"served")
	if err != nil || ls.ID != fakePrefix+"served" || !ls.HasCurrentValue {
		t.Fatalf("Describe(served) = %+v, %v", ls, err)
	}
	if on, err := ls.OnKey(ctx); err != nil || !on {
		t.Fatalf("Describe(served).OnKey = %v, %v; want on the key", on, err)
	}
	for _, name := range []string{"missing", "expiring"} {
		if _, err := store.Describe(ctx, fakePrefix+name); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Describe(%s) = %v, want ErrNotFound", name, err)
		}
	}
	if v, err := store.Read(ctx, fakePrefix+"served"); err != nil || v != "hello" {
		t.Fatalf("Read(served) = %q, %v", strings.ReplaceAll(v, "hello", "[ok]"), err)
	}
	if _, err := store.Read(ctx, fakePrefix+"missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read(missing) = %v, want ErrNotFound", err)
	}
	if _, err := store.Read(ctx, fakePrefix+"disabled"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read(disabled) = %v, want ErrNotFound (FAILED_PRECONDITION)", err)
	}
	// Secret Manager would still answer its value; the store refuses it as Secrets Manager refuses
	// a secret in its recovery window, so a grant made before the delete reads nothing after it.
	if _, err := store.Read(ctx, fakePrefix+"expiring"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read(expiring) = %v, want ErrNotFound: a secret scheduled for deletion is not readable", err)
	}
	for _, name := range []string{"empty", "binary"} {
		_, err := store.Read(ctx, fakePrefix+name)
		if err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "non-empty UTF-8") || strings.Contains(err.Error(), "\xff") {
			t.Fatalf("Read(%s) = %v; want a refusal naming the rule and never the bytes", name, err)
		}
	}
}

func TestGCPListFailureNamesTheCodeNotTheMessage(t *testing.T) {
	f := &fakeSecretManager{failList: status.Errorf(codes.PermissionDenied, "Permission denied on resource project 000000000000 (secret value: hunter2)")}
	store := NewGCP(startFake(t, f), fakeKey)
	_, err := store.List(context.Background(), fakePrefix)
	if err == nil || !strings.Contains(err.Error(), "PermissionDenied") || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("List under a denied identity = %v; want the gRPC code and never the status message", err)
	}
}

func TestGCPValidNameAndPrefix(t *testing.T) {
	store := GCP{KeyName: fakeKey}
	if err := store.ValidName(fakePrefix + "deel-api-key"); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidName(fakePrefix + strings.Repeat("a", 256)); err == nil {
		t.Fatal("ValidName accepted a 270-character id")
	}
	if err := store.ValidName(fakePrefix + "has.dot"); err == nil {
		t.Fatal("ValidName accepted a dot")
	}
	if err := ValidGCPPrefix(fakePrefix); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"projects/example-project/secrets/agent-secrets-", fakeProject + "/secrets/agent-secrets", fakeProject + "/agent-secrets-", "production/agent-secrets/"} {
		if err := ValidGCPPrefix(bad); err == nil {
			t.Fatalf("ValidGCPPrefix(%q) accepted", bad)
		}
	}
	if got := GCPProject(fakePrefix); got != "000000000000" {
		t.Fatalf("GCPProject = %q", got)
	}
	if err := ValidGCPKeyName(fakeKey); err != nil {
		t.Fatal(err)
	}
	if err := ValidGCPKeyName("projects/p/locations/us-west1/keyRings/r/cryptoKeys/k"); err == nil {
		t.Fatal("ValidGCPKeyName accepted a regional key")
	}
}

// TestGCPReadQuotaCost pins what each operation costs against Secret Manager's 600-per-minute read
// quota, which the whole project shares (AccessSecretVersion has its own, 90,000 per minute): a
// List of N secrets under the prefix is one ListSecrets and N GetSecretVersion, a Describe is one
// GetSecret and one GetSecretVersion, and a Read is one GetSecret and one AccessSecretVersion.
func TestGCPReadQuotaCost(t *testing.T) {
	f := &fakeSecretManager{secrets: map[string]*fakeSecret{
		fakePrefix + "a": servedSecret("v"),
		fakePrefix + "b": servedSecret("v"),
		fakePrefix + "c": servedSecret("v"),
	}}
	store := NewGCP(startFake(t, f), fakeKey)
	ctx := context.Background()
	cost := func(op func() error) int {
		t.Helper()
		before := f.reads()
		if err := op(); err != nil {
			t.Fatal(err)
		}
		return f.reads() - before
	}
	if got := cost(func() error { _, err := store.List(ctx, fakePrefix); return err }); got != 1+3 {
		t.Fatalf("List of 3 secrets cost %d reads, want 4", got)
	}
	if got := cost(func() error { _, err := store.Describe(ctx, fakePrefix+"a"); return err }); got != 2 {
		t.Fatalf("Describe cost %d reads, want 2", got)
	}
	if got := cost(func() error { _, err := store.Read(ctx, fakePrefix+"a"); return err }); got != 1 {
		t.Fatalf("Read cost %d reads, want 1 (its value access is the separate quota)", got)
	}
}

// TestGCPRetriesAnUnavailableMetadataCall pins NewGCPClient's retry: a transient UNAVAILABLE on
// ListSecrets, GetSecret or GetSecretVersion is retried, so it fails neither a reload nor a reread.
func TestGCPRetriesAnUnavailableMetadataCall(t *testing.T) {
	f := &fakeSecretManager{
		secrets:     map[string]*fakeSecret{fakePrefix + "a": servedSecret("v")},
		unavailable: map[string]int{"ListSecrets": 1, "GetSecret": 1, "GetSecretVersion": 2},
	}
	store := NewGCP(startFake(t, f), fakeKey)
	ctx := context.Background()
	if ls, err := store.List(ctx, fakePrefix); err != nil || len(ls) != 1 {
		t.Fatalf("List through one UNAVAILABLE each = %d secrets, %v; want 1 after retries", len(ls), err)
	}
	if _, err := store.Describe(ctx, fakePrefix+"a"); err != nil {
		t.Fatalf("Describe through one UNAVAILABLE each = %v; want it to succeed after retries", err)
	}
	if f.calls["ListSecrets"] != 2 || f.calls["GetSecret"] != 2 || f.calls["GetSecretVersion"] != 4 {
		t.Fatalf("calls = %v; want each failed call made again once", f.calls)
	}
}

// TestGCPReadChecksDeletionOncePerWindow pins Read's cache: releasing the same secret again within
// gcpHeldFor costs no metadata read, the first release after it reads again, and a secret
// scheduled for deletion since is refused from that read on.
func TestGCPReadChecksDeletionOncePerWindow(t *testing.T) {
	f := &fakeSecretManager{secrets: map[string]*fakeSecret{fakePrefix + "a": servedSecret("v")}}
	store := NewGCP(startFake(t, f), fakeKey)
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	store.held.now = func() time.Time { return now }
	ctx := context.Background()
	read := func() error { _, err := store.Read(ctx, fakePrefix+"a"); return err }
	if err := read(); err != nil {
		t.Fatal(err)
	}
	before := f.reads()
	for range 5 {
		if err := read(); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.reads() - before; got != 0 {
		t.Fatalf("five releases within the window cost %d metadata reads, want 0", got)
	}
	f.mu.Lock()
	f.secrets[fakePrefix+"a"].expires = now.Add(30 * 24 * time.Hour)
	f.mu.Unlock()
	now = now.Add(gcpHeldFor)
	if err := read(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read after the window of a secret scheduled for deletion since = %v, want ErrNotFound", err)
	}
	if got := f.reads() - before; got != 1 {
		t.Fatalf("the release after the window cost %d metadata reads, want 1", got)
	}
}

// TestGCPReadCachesARefusalAndCoalescesConcurrentChecks pins the rest of Read's bound: repeated
// releases of a secret scheduled for deletion cost one metadata read per window, as readable ones
// do, and concurrent releases of a cold secret wait for one check.
func TestGCPReadCachesARefusalAndCoalescesConcurrentChecks(t *testing.T) {
	gone := servedSecret("v")
	gone.expires = time.Now().Add(30 * 24 * time.Hour)
	f := &fakeSecretManager{secrets: map[string]*fakeSecret{fakePrefix + "gone": gone, fakePrefix + "cold": servedSecret("v")}}
	store := NewGCP(startFake(t, f), fakeKey)
	ctx := context.Background()
	before := f.reads()
	for range 5 {
		if _, err := store.Read(ctx, fakePrefix+"gone"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Read(gone) = %v, want ErrNotFound", err)
		}
	}
	if got := f.reads() - before; got != 1 {
		t.Fatalf("five releases of a secret scheduled for deletion cost %d metadata reads, want 1", got)
	}
	before = f.reads()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := store.Read(ctx, fakePrefix+"cold"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := f.reads() - before; got != 1 {
		t.Fatalf("eight concurrent releases of a cold secret cost %d metadata reads, want 1", got)
	}
}

// TestGCPBoundsItsRetriesAndKeepsAKeyFailureAnError pins two failure answers. A metadata call that
// keeps answering UNAVAILABLE is made gcpMetadataAttempts times and then fails with its own code
// under a request's deadline, within seconds. A FAILED_PRECONDITION on an enabled version (an
// unusable key, say) is an error the broker logs, not a secret with no value.
func TestGCPBoundsItsRetriesAndKeepsAKeyFailureAnError(t *testing.T) {
	broken := servedSecret("v")
	broken.keyUnusable = true
	f := &fakeSecretManager{
		secrets:     map[string]*fakeSecret{fakePrefix + "a": servedSecret("v"), fakePrefix + "broken": broken},
		unavailable: map[string]int{"GetSecret": 100},
	}
	store := NewGCP(startFake(t, f), fakeKey)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	start := time.Now()
	_, err := store.Describe(ctx, fakePrefix+"a")
	if err == nil || !strings.Contains(err.Error(), "Unavailable") || time.Since(start) > 10*time.Second {
		t.Fatalf("Describe through a lasting UNAVAILABLE = %v after %s; want Unavailable within seconds", err, time.Since(start))
	}
	f.mu.Lock()
	calls := f.calls["GetSecret"]
	f.unavailable = nil
	f.mu.Unlock()
	if calls != gcpMetadataAttempts {
		t.Fatalf("GetSecret calls = %d, want %d", calls, gcpMetadataAttempts)
	}
	if _, err := store.Read(context.Background(), fakePrefix+"broken"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Read of an enabled version that answers FAILED_PRECONDITION = %v; want a logged error, not ErrNotFound", err)
	}
}

// TestGCPReadDoesNotCacheAFailedCheck pins that a metadata check failing for another reason than
// not-found (a denied call, an outage past its retries) is not remembered: the next release
// checks again, so one caller's failure never refuses every release of the secret for 30 s.
func TestGCPReadDoesNotCacheAFailedCheck(t *testing.T) {
	f := &fakeSecretManager{
		secrets:     map[string]*fakeSecret{fakePrefix + "a": servedSecret("v")},
		unavailable: map[string]int{"GetSecret": gcpMetadataAttempts},
	}
	store := NewGCP(startFake(t, f), fakeKey)
	ctx := context.Background()
	if _, err := store.Read(ctx, fakePrefix+"a"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("first Read through an outage = %v, want its error", err)
	}
	if v, err := store.Read(ctx, fakePrefix+"a"); err != nil || v != "v" {
		t.Fatalf("second Read = %q, %v; want a fresh check that succeeds", v, err)
	}
}

// TestGCPReadCachesADisabledVersionsVerdict pins that a secret whose latest version is disabled
// costs one GetSecretVersion per window however often it is released, as Read's other check does.
func TestGCPReadCachesADisabledVersionsVerdict(t *testing.T) {
	off := servedSecret("v")
	off.state = secretmanagerpb.SecretVersion_DISABLED
	f := &fakeSecretManager{secrets: map[string]*fakeSecret{fakePrefix + "off": off}}
	store := NewGCP(startFake(t, f), fakeKey)
	for range 5 {
		if _, err := store.Read(context.Background(), fakePrefix+"off"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Read(off) = %v, want ErrNotFound", err)
		}
	}
	if got := f.calls["GetSecretVersion"]; got != 1 {
		t.Fatalf("five releases of a disabled secret made %d GetSecretVersion calls, want 1", got)
	}
}
