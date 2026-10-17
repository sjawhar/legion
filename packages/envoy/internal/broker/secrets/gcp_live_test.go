//go:build gcplive

package secrets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestGCPLive runs the store against a real Secret Manager project under Application Default
// Credentials, the way the broker runs on GKE, and asserts what the fake asserts: a secret on the
// agent-secrets key with annotations is listed, described and read; one on Google's default key is
// not OnKey; one with no version has no value; a disabled one has no value and is not readable; one
// with an expire_time an hour ahead is left out and not described. Every secret it makes is under a random prefix of
// its own and deleted at the end, so the broker's namespace is untouched.
//
//	BROKER_GCP_LIVE_PROJECT=<project number> BROKER_GCP_LIVE_KMS_KEY=<key name> \
//	  go test -tags gcplive ./internal/broker/secrets/ -run Live -count=1 -v
func TestGCPLive(t *testing.T) {
	project, key := envOrSkip(t, "BROKER_GCP_LIVE_PROJECT"), envOrSkip(t, "BROKER_GCP_LIVE_KMS_KEY")
	if err := ValidGCPKeyName(key); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	client, err := NewGCPClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Registered before any secret's deletion, so it runs after every one of them: cleanups run
	// last in, first out, and a deferred close would run before them, on the client they need.
	t.Cleanup(func() { _ = client.Close() })

	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	idPrefix := "broker-live-" + hex.EncodeToString(nonce[:]) + "-"
	parent := "projects/" + project
	prefix := parent + "/secrets/" + idPrefix
	if err := ValidGCPPrefix(prefix); err != nil {
		t.Fatal(err)
	}
	store := NewGCP(client, key)

	create := func(name string, onKey bool, annotations map[string]string, value string, expires bool) string {
		t.Helper()
		auto := &secretmanagerpb.Replication_Automatic{}
		if onKey {
			auto.CustomerManagedEncryption = &secretmanagerpb.CustomerManagedEncryption{KmsKeyName: key}
		}
		req := &secretmanagerpb.CreateSecretRequest{
			Parent: parent, SecretId: idPrefix + name,
			Secret: &secretmanagerpb.Secret{
				Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: auto}},
				Annotations: annotations,
			},
		}
		if expires {
			req.Secret.Expiration = &secretmanagerpb.Secret_ExpireTime{ExpireTime: timestamppb.New(time.Now().Add(time.Hour))}
		}
		secret, err := client.CreateSecret(ctx, req)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		t.Cleanup(func() {
			if err := client.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: secret.GetName()}); err != nil {
				t.Errorf("delete %s: %v (delete it by hand)", secret.GetName(), err)
			}
		})
		if value != "" {
			if _, err := client.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: secret.GetName(), Payload: &secretmanagerpb.SecretPayload{Data: []byte(value)}}); err != nil {
				t.Fatalf("add version to %s: %v", name, err)
			}
		}
		return secret.GetName()
	}
	tags := map[string]string{"owner": "ada@example.com", "tier": "human"}
	served := create("served", true, tags, "live-value", false)
	defaultKey := create("default-key", false, tags, "v", false)
	noVersion := create("no-version", true, tags, "", false)
	disabled := create("disabled", true, tags, "v", false)
	if _, err := client.DisableSecretVersion(ctx, &secretmanagerpb.DisableSecretVersionRequest{Name: disabled + "/versions/1"}); err != nil {
		t.Fatal(err)
	}
	expiring := create("expiring", true, tags, "v", true)

	if !strings.HasPrefix(served, prefix) {
		t.Fatalf("the API answered %q for a secret created under %q: the prefix must name the project as the API does", served, prefix)
	}

	// The listing is eventually consistent; the broker's policy anchors on that too.
	var byID map[string]Listing
	deadline := time.Now().Add(90 * time.Second)
	for {
		listings, err := store.List(ctx, prefix)
		if err != nil {
			t.Fatal(err)
		}
		byID = map[string]Listing{}
		for _, ls := range listings {
			byID[ls.ID] = ls
		}
		if len(byID) == 4 || time.Now().After(deadline) {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if _, listed := byID[expiring]; len(byID) != 4 || listed {
		t.Fatalf("List answered %d secrets within 90s, want the 4 with no expire_time: %+v", len(byID), byID)
	}
	onKey := func(ls Listing) bool {
		t.Helper()
		on, err := ls.OnKey(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return on
	}
	if ls := byID[served]; !onKey(ls) || !ls.HasCurrentValue || ls.Tags["owner"] != "ada@example.com" || ls.Tags["tier"] != "human" {
		t.Fatalf("served = %+v", ls)
	}
	if ls := byID[defaultKey]; onKey(ls) || !ls.HasCurrentValue {
		t.Fatalf("default-key = %+v; want not on the key", ls)
	}
	if ls := byID[noVersion]; !onKey(ls) || ls.HasCurrentValue {
		t.Fatalf("no-version = %+v; want on the key with no value", ls)
	}
	if ls := byID[disabled]; !onKey(ls) || ls.HasCurrentValue {
		t.Fatalf("disabled = %+v; want on the key with no value", ls)
	}
	if _, err := store.Describe(ctx, expiring); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Describe(expiring) = %v, want ErrNotFound", err)
	}
	if _, err := store.Read(ctx, expiring); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read(expiring) = %v, want ErrNotFound: Secret Manager still holds its value, and the store must not release it", err)
	}

	if ls, err := store.Describe(ctx, served); err != nil || !onKey(ls) || !ls.HasCurrentValue {
		t.Fatalf("Describe(served) = %+v, %v", ls, err)
	}
	if _, err := store.Describe(ctx, prefix+"missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Describe(missing) = %v, want ErrNotFound", err)
	}
	if v, err := store.Read(ctx, served); err != nil || v != "live-value" {
		t.Fatalf("Read(served) = %d bytes, %v", len(v), err)
	}
	if _, err := store.Read(ctx, disabled); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read(disabled) = %v, want ErrNotFound", err)
	}
	if _, err := store.Read(ctx, noVersion); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read(no-version) = %v, want ErrNotFound", err)
	}
}

func envOrSkip(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is not set", name)
	}
	return v
}
