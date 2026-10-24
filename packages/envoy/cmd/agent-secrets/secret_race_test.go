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
				SecretId: aws.String(policytest.ID("APP_KEY")), Tags: secrets.TagList(map[string]string{policy.TagKind: policy.KindGitHubApp}),
			}); err != nil {
				t.Errorf("the other writer's retag: %v", err)
			}
		}
		useAWS(t, writes, testAccount, adaSignIn)
		_, stderr, code := runSecret("retag", "APP_KEY", "--github-owners", "none")
		if code != 1 || !strings.Contains(stderr, "another write changed the secret after this retag read it") || !strings.Contains(stderr, "one writer at a time") || !strings.Contains(stderr, "restored") {
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

// TestARetagExitsOneWhileTheBrokerServesTheOldKind: `broker: serving` alone does not say the
// broker reads the tags a retag wrote, since a reread can read them stale. A retag to a kind whose
// reread answers the secret served as stored (or as another kind) exits 1, saying the broker still
// sees the old tags, to run the retag again, and to store no value yet; the reread answering the
// kind the retag wrote exits 0. A retag that removes the kind is held to the same rule.
func TestARetagExitsOneWhileTheBrokerServesTheOldKind(t *testing.T) {
	for _, row := range []struct {
		name, served string
		args         []string
		code         int
	}{
		{"stale: still stored", "", []string{"--kind", policy.KindGoogleLogin}, 1},
		{"stale: the other kind", policy.KindGitHubApp, []string{"--kind", policy.KindGoogleLogin}, 1},
		{"read", policy.KindGoogleLogin, []string{"--kind", policy.KindGoogleLogin}, 0},
		{"removal stale", policy.KindGoogleLogin, []string{"--kind", "none"}, 1},
		{"removal read", "", []string{"--kind", "none"}, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			local := secrets.NewLocal(policytest.Secret("LOGIN_KEY", "ada@example.com", policy.TierAgent, "v1"))
			if row.args[1] == "none" {
				local = secrets.NewLocal(policytest.KindSecret("LOGIN_KEY", "ada@example.com", policy.TierAgent, policy.KindGoogleLogin, "v1"))
			}
			startSecretBroker(t, func(name string) Reread { return Reread{Name: name, Served: true, Kind: row.served} })
			useAWS(t, local, testAccount, adaSignIn)
			stdout, stderr, code := runSecret(append([]string{"retag", "LOGIN_KEY"}, row.args...)...)
			if code != row.code {
				t.Fatalf("retag %v with the broker serving kind %q: exit %d, stdout %q, stderr %q; want %d", row.args, row.served, code, stdout, stderr, row.code)
			}
			stale := strings.Contains(stderr, "it still sees the old tags") && strings.Contains(stderr, "Run this retag again") && strings.Contains(stderr, "store no value in LOGIN_KEY")
			if stale != (row.code == 1) {
				t.Fatalf("stderr %q; want the stale-tags message exactly when the kind is stale", stderr)
			}
			if row.served != "" && !strings.Contains(stdout, "broker: serving LOGIN_KEY (kind "+row.served+")") {
				t.Fatalf("stdout %q does not name the served kind", stdout)
			}
		})
	}
}
