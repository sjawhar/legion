package main

import (
	"context"
	"maps"
	"strings"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// racingTagWrites is a secrets.Local whose first tag write lets another writer's write (race) land
// first, as a second retag of the same secret between this one's read and its write does.
type racingTagWrites struct {
	*secrets.Local
	race  func()
	raced bool
}

func (r *racingTagWrites) TagResource(ctx context.Context, in *secretsmanager.TagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.TagResourceOutput, error) {
	r.first()
	return r.Local.TagResource(ctx, in, o...)
}

func (r *racingTagWrites) UntagResource(ctx context.Context, in *secretsmanager.UntagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.UntagResourceOutput, error) {
	r.first()
	return r.Local.UntagResource(ctx, in, o...)
}

func (r *racingTagWrites) first() {
	if !r.raced {
		r.raced = true
		r.race()
	}
}

// loginWithOwnersLeft is a google-login secret that still holds the github-owners tag a github-app
// kind left on it, as retagging a GitHub App secret to a Google login leaves it.
func loginWithOwnersLeft(name string) map[string]string {
	tags := policytest.KindSecret(name, "ada@example.com", policy.TierAgent, policy.KindGoogleLogin, "stored-value").Tags
	tags[policy.TagGitHubOwners] = "ada"
	return tags
}

// TestAConcurrentRetagNeverLeavesAGitHubAppSecretWithoutOwners: `--github-owners none` on a
// google-login secret is checked against that kind, which reads no owners. When another writer
// retags the secret github-app, keeping its owners, between that check and the write, the removal
// would leave a github-app secret with no owners, which the broker refuses. On GCP the write is
// made against the read it was checked on (its etag), so the store refuses it, naming the change,
// and the other writer's tags stand. AWS has no conditional tag write, so the retag reads the tags
// back, finds the pair the broker refuses, restores the tags it read and exits 1 naming the race.
func TestAConcurrentRetagNeverLeavesAGitHubAppSecretWithoutOwners(t *testing.T) {
	t.Run("aws", func(t *testing.T) {
		secret := policytest.KindSecret("APP_KEY", "ada@example.com", policy.TierAgent, policy.KindGoogleLogin, "stored-value")
		secret.Tags = loginWithOwnersLeft("APP_KEY")
		local := secrets.NewLocal(secret)
		startSecretBroker(t, servedBy(local))
		writes := &racingTagWrites{Local: local}
		writes.race = func() {
			if _, err := local.TagResource(context.Background(), &secretsmanager.TagResourceInput{
				SecretId: aws.String(policytest.ID("APP_KEY")), Tags: awsTags(map[string]string{policy.TagKind: policy.KindGitHubApp}),
			}); err != nil {
				t.Errorf("the other writer's retag: %v", err)
			}
		}
		useAWS(t, writes, testAccount, adaSignIn)
		_, stderr, code := runSecret("retag", "APP_KEY", "--github-owners", "none")
		if code != 1 || !strings.Contains(stderr, "another write retagged APP_KEY") || !strings.Contains(stderr, "one writer at a time") || !strings.Contains(stderr, "restored") {
			t.Fatalf("racing retag: exit %d stderr %q; want 1 naming the race and the restore", code, stderr)
		}
		tags := secrets.TagMap(describe(t, local, "APP_KEY").Tags)
		if !maps.Equal(tags, secret.Tags) {
			t.Fatalf("tags after the race = %v; want the tags the retag read, %v", tags, secret.Tags)
		}
		if _, err := policy.GitHubOwnersFor(tags[policy.TagKind], tags[policy.TagGitHubOwners]); err != nil {
			t.Fatalf("tags after the race are a pair the broker refuses: %v", err)
		}
	})
	t.Run("gcp", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		useGCPIdentity(t, "authorized_user", "ada@example.com")
		settings := gcpTestSettings()
		broker := startSecretBrokerWith(t, settings, gcpServedBy(fake, settings))
		id := testGCPPrefix + "app-key"
		fake.secrets[id] = &fakeSecret{
			secret:      &secretmanagerpb.Secret{Name: id, Annotations: loginWithOwnersLeft("APP_KEY"), Etag: "1"},
			versions:    []*secretmanagerpb.SecretVersion{{Name: id + "/versions/1", State: secretmanagerpb.SecretVersion_ENABLED}},
			etagCounter: 1,
		}
		fake.race = func(entry *fakeSecret) {
			annotations := maps.Clone(entry.secret.Annotations)
			annotations[policy.TagKind] = policy.KindGitHubApp
			entry.secret.Annotations = annotations
			entry.etagCounter++
			entry.secret.Etag = "2"
		}
		_, stderr, code := runSecret("retag", "APP_KEY", "--github-owners", "none")
		if code != 1 || !strings.Contains(stderr, "another write changed the secret after this retag read it") {
			t.Fatalf("racing retag: exit %d stderr %q; want 1 naming the change", code, stderr)
		}
		fake.mu.Lock()
		annotations, gets := fake.secrets[id].secret.GetAnnotations(), fake.gets
		fake.mu.Unlock()
		if annotations[policy.TagKind] != policy.KindGitHubApp || annotations[policy.TagGitHubOwners] != "ada" {
			t.Fatalf("annotations after the race = %v; want the other writer's github-app with its owners", annotations)
		}
		if gets != 1 {
			t.Fatalf("the retag read the secret %d times; want once, the read its write was made against", gets)
		}
		if rereads, _ := broker.recorded(); len(rereads) != 0 {
			t.Fatalf("rereads = %v; want none after a refused write", rereads)
		}
	})
}
