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
}

func (f *fakeSecretManager) ListSecrets(_ context.Context, req *secretmanagerpb.ListSecretsRequest) (*secretmanagerpb.ListSecretsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	s, secret, err := f.latest(req.GetName())
	if err != nil {
		return nil, err
	}
	if s.state != secretmanagerpb.SecretVersion_ENABLED {
		return nil, status.Errorf(codes.FailedPrecondition, "Secret Version [%s] is in %s state", req.GetName(), s.state)
	}
	return &secretmanagerpb.AccessSecretVersionResponse{Name: secret + "/versions/1", Payload: &secretmanagerpb.SecretPayload{Data: s.value}}, nil
}

// startFake serves f over TLS on a loopback port and answers a Secret Manager client dialled to it
// with no credentials, as the broker's client would be built against the real service.
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
	client, err := secretmanager.NewClient(context.Background(), option.WithGRPCConn(conn), option.WithoutAuthentication())
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
	store := GCP{Client: startFake(t, f), KeyName: fakeKey}
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
	store := GCP{Client: startFake(t, f), KeyName: fakeKey}
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
	store := GCP{Client: startFake(t, f), KeyName: fakeKey}
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
