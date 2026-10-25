package main

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/smithy-go"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

func TestSecretKindAndMaxGrantOnAWS(t *testing.T) {
	local := secrets.NewLocal()
	startSecretBroker(t, servedBy(local))
	useAWS(t, local, testAccount, adaSignIn)
	useStdin(t, "stored-value")

	stdout, stderr, code := runSecret("create", "APP_KEY", "--owner", "me", "--tier", "agent", "--kind", policy.KindGitHubApp, "--github-owners", "Acme-Org", "--max-grant", "1h")
	if code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	tags := secrets.TagMap(describe(t, local, "APP_KEY").Tags)
	if tags[policy.TagKind] != policy.KindGitHubApp || tags[policy.TagMaxGrant] != "1h" || tags[policy.TagGitHubOwners] != "acme-org" {
		t.Fatalf("create tags = %v, want kind, github-owners as the broker reads them, and max-grant", tags)
	}
	if tags[policy.TagRollbackGuard] != policy.RollbackGuardValue {
		t.Fatalf("create tags = %v, want the rollback guard beside the kind", tags)
	}
	if !strings.Contains(stdout, "broker: serving APP_KEY") {
		t.Fatalf("create stdout %q does not name the broker result", stdout)
	}

	stdout, stderr, code = runSecret("show", "APP_KEY")
	if code != 0 || !strings.Contains(stdout, "kind: github-app") || !strings.Contains(stdout, "github_owners: acme-org") || !strings.Contains(stdout, "max_grant: 1h") {
		t.Fatalf("show: exit %d stdout %q stderr %q; want kind, github owners and max-grant", code, stdout, stderr)
	}
	stdout, stderr, code = runSecret("list")
	if code != 0 || !strings.Contains(stdout, "KIND") || !strings.Contains(stdout, "MAX_GRANT") || !strings.Contains(stdout, "github-app") || !strings.Contains(stdout, "1h") {
		t.Fatalf("list: exit %d stdout %q stderr %q; want kind and max-grant", code, stdout, stderr)
	}
	stdout, stderr, code = runSecret("list", "--json")
	var listed []map[string]any
	if code != 0 || json.Unmarshal([]byte(stdout), &listed) != nil || len(listed) != 1 {
		t.Fatalf("list --json: exit %d stdout %q stderr %q; want one secret", code, stdout, stderr)
	}
	if got := listed[0]; got["kind"] != policy.KindGitHubApp || got["github_owners"] != "acme-org" || got["max_grant"] != "1h" {
		t.Fatalf("list --json = %v; want kind github-app, github_owners acme-org and max_grant 1h", got)
	}

	for _, kind := range []string{policy.KindGoogleLogin, policy.KindGitHubApp} {
		_, stderr, code = runSecret("retag", "APP_KEY", "--kind", kind)
		if code != 0 {
			t.Fatalf("retag %s: exit %d, stderr %q", kind, code, stderr)
		}
		tags = secrets.TagMap(describe(t, local, "APP_KEY").Tags)
		if tags[policy.TagKind] != kind || tags[policy.TagMaxGrant] != "1h" || tags[policy.TagOwner] != "ada@example.com" || tags[policy.TagTier] != policy.TierAgent || tags[policy.TagRollbackGuard] != policy.RollbackGuardValue {
			t.Fatalf("retag %s tags = %v, want new kind, the rollback guard and retained tags", kind, tags)
		}
	}
	_, stderr, code = runSecret("retag", "APP_KEY", "--kind", "none")
	if code != 0 {
		t.Fatalf("retag none: exit %d, stderr %q", code, stderr)
	}
	tags = secrets.TagMap(describe(t, local, "APP_KEY").Tags)
	if _, ok := tags[policy.TagKind]; ok || tags[policy.TagMaxGrant] != "1h" {
		t.Fatalf("retag none tags = %v, want removed kind and retained max-grant", tags)
	}
	if _, ok := tags[policy.TagRollbackGuard]; ok {
		t.Fatalf("retag none tags = %v, want the rollback guard removed with the kind", tags)
	}
}

func TestSecretKindAndMaxGrantOnGCP(t *testing.T) {
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	settings := gcpTestSettings()
	startSecretBrokerWith(t, settings, gcpServedBy(fake, settings))
	useStdin(t, "stored-value")

	_, stderr, code := runSecret("create", "LOGIN_KEY", "--owner", "me", "--tier", "agent", "--kind", policy.KindGoogleLogin, "--max-grant", "30m")
	if code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	id := testGCPPrefix + "login-key"
	fake.mu.Lock()
	annotations := fake.secrets[id].secret.GetAnnotations()
	fake.mu.Unlock()
	if annotations[policy.TagKind] != policy.KindGoogleLogin || annotations[policy.TagMaxGrant] != "30m" || annotations[policy.TagRollbackGuard] != policy.RollbackGuardValue {
		t.Fatalf("create annotations = %v, want kind, max-grant and the rollback guard", annotations)
	}
	if _, ok := annotations[policy.TagGitHubOwners]; ok {
		t.Fatalf("create annotations = %v, want no github-owners on a google-login secret", annotations)
	}

	stdout, stderr, code := runSecret("show", "LOGIN_KEY")
	if code != 0 || !strings.Contains(stdout, "kind: google-login") || !strings.Contains(stdout, "max_grant: 30m") {
		t.Fatalf("show: exit %d stdout %q stderr %q; want kind and max-grant", code, stdout, stderr)
	}
	stdout, stderr, code = runSecret("list")
	if code != 0 || !strings.Contains(stdout, "KIND") || !strings.Contains(stdout, "MAX_GRANT") || !strings.Contains(stdout, "google-login") || !strings.Contains(stdout, "30m") {
		t.Fatalf("list: exit %d stdout %q stderr %q; want kind and max-grant", code, stdout, stderr)
	}
	for _, kind := range []string{policy.KindGitHubApp, policy.KindGoogleLogin} {
		args := []string{"retag", "LOGIN_KEY", "--kind", kind}
		if policy.KindNeedsGitHubOwners(kind) {
			args = append(args, "--github-owners", "Ada  acme-org")
		}
		if _, stderr, code = runSecret(args...); code != 0 {
			t.Fatalf("retag %s: exit %d, stderr %q", kind, code, stderr)
		}
		fake.mu.Lock()
		annotations = fake.secrets[id].secret.GetAnnotations()
		fake.mu.Unlock()
		if annotations[policy.TagKind] != kind || annotations[policy.TagMaxGrant] != "30m" || annotations[policy.TagOwner] != "ada@example.com" || annotations[policy.TagTier] != policy.TierAgent {
			t.Fatalf("retag %s annotations = %v, want new kind and retained annotations", kind, annotations)
		}
	}
	fake.mu.Lock()
	if got := fake.secrets[id].secret.GetAnnotations()[policy.TagGitHubOwners]; got != "acme-org ada" {
		t.Errorf("github-owners after the round trip = %q, want the canonical space-separated list: acme-org ada", got)
	}
	fake.mu.Unlock()
	if _, stderr, code = runSecret("retag", "LOGIN_KEY", "--kind", "none"); code != 0 {
		t.Fatalf("retag none: exit %d, stderr %q", code, stderr)
	}
	fake.mu.Lock()
	annotations = fake.secrets[id].secret.GetAnnotations()
	fake.mu.Unlock()
	if _, ok := annotations[policy.TagKind]; ok || annotations[policy.TagMaxGrant] != "30m" {
		t.Fatalf("retag none annotations = %v, want removed kind and retained max-grant", annotations)
	}
	if _, ok := annotations[policy.TagRollbackGuard]; ok {
		t.Fatalf("retag none annotations = %v, want the rollback guard removed with the kind", annotations)
	}
}

func TestSecretKindFlagsRejectInvalidValues(t *testing.T) {
	local := secrets.NewLocal(policytest.Secret("HELD_KEY", "ada@example.com", policy.TierAgent, "value"))
	startSecretBroker(t, servedBy(local))
	useAWS(t, local, testAccount, adaSignIn)
	useStdin(t, "stored-value")
	for _, args := range [][]string{
		{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--kind", "other"},
		{"retag", "HELD_KEY", "--kind", "other"},
		{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--max-grant", "soon"},
		{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--max-grant", "500ms"},
		{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--kind", policy.KindGitHubApp},
		{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--kind", policy.KindGitHubApp, "--github-owners", "  "},
		{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--kind", policy.KindGoogleLogin, "--github-owners", "ada"},
		{"retag", "HELD_KEY", "--github-owners", "  "},
		{"retag", "HELD_KEY", "--github-owners", "none", "--tier", "human"},
	} {
		_, stderr, code := runSecret(args...)
		if code != exitUsageError {
			t.Fatalf("%v: exit %d stderr %q; want usage error", args, code, stderr)
		}
	}
}

// TestANoneBesideALoginIsRefusedOnCreateAndRetag: none removes github-owners only alone on retag,
// so a list that names it beside a login, in any case, is refused before anything is read or
// written, on create and on retag, rather than written as a login named none.
func TestANoneBesideALoginIsRefusedOnCreateAndRetag(t *testing.T) {
	local := secrets.NewLocal(kindTaggedSecret("APP_KEY"))
	startSecretBroker(t, servedBy(local))
	writes := &countingTagWrites{Local: local}
	useAWS(t, writes, testAccount, adaSignIn)
	for _, owners := range []string{"none ada", "ada NONE", "None  acme-org"} {
		useStdin(t, "stored-value")
		_, stderr, code := runSecret("create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--kind", policy.KindGitHubApp, "--github-owners", owners)
		if code != exitUsageError || !strings.Contains(stderr, "none names no GitHub account") {
			t.Fatalf("create --github-owners %q: exit %d stderr %q; want a usage error naming none", owners, code, stderr)
		}
		_, stderr, code = runSecret("retag", "APP_KEY", "--github-owners", owners)
		if code != exitUsageError || !strings.Contains(stderr, "given alone") {
			t.Fatalf("retag --github-owners %q: exit %d stderr %q; want a usage error naming none alone", owners, code, stderr)
		}
	}
	if _, err := local.DescribeSecret(context.Background(), &secretsmanager.DescribeSecretInput{SecretId: aws.String(policytest.ID("NEW_KEY"))}); err == nil {
		t.Fatal("a refused create made NEW_KEY")
	}
	if writes.tags != 0 || writes.untags != 0 || writes.describes != 0 {
		t.Fatalf("refused commands made %d TagResource, %d UntagResource and %d DescribeSecret calls; want none", writes.tags, writes.untags, writes.describes)
	}
}

// TestARetagOfASecretWhoseStoredOwnersAreMalformedNamesTheMalformedTag: a github-app secret whose
// github-owners tag was written with a comma outside the CLI is refused by the broker as
// github-owners-malformed, and a retag that leaves the tag as it is says so, not that owners are
// missing, and writes nothing; naming the owners fixes it.
func TestARetagOfASecretWhoseStoredOwnersAreMalformedNamesTheMalformedTag(t *testing.T) {
	commas := kindTaggedSecret("APP_KEY")
	commas.Tags[policy.TagGitHubOwners] = "acme-org,ada"
	local := secrets.NewLocal(commas)
	startSecretBroker(t, servedBy(local))
	writes := &countingTagWrites{Local: local}
	useAWS(t, writes, testAccount, adaSignIn)
	_, stderr, code := runSecret("retag", "APP_KEY", "--tier", "human")
	if code != exitUsageError || !strings.Contains(stderr, policy.ReasonGitHubOwnersMalformed) || strings.Contains(stderr, policy.ReasonGitHubOwnersMissing) {
		t.Fatalf("retag --tier human: exit %d stderr %q; want a usage error naming %s, not %s", code, stderr, policy.ReasonGitHubOwnersMalformed, policy.ReasonGitHubOwnersMissing)
	}
	if writes.tags != 0 {
		t.Fatalf("the refused retag made %d TagResource calls, want none", writes.tags)
	}
	if _, stderr, code := runSecret("retag", "APP_KEY", "--github-owners", "acme-org ada"); code != 0 {
		t.Fatalf("retag naming the owners: exit %d stderr %q", code, stderr)
	}
	if tags := secrets.TagMap(describe(t, local, "APP_KEY").Tags); tags[policy.TagGitHubOwners] != "acme-org ada" {
		t.Fatalf("tags = %v, want github-owners %q", tags, "acme-org ada")
	}
}

// TestARetagThatLeavesAGitHubAppSecretWithoutOwnersWritesNothing: the github-owners rule
// (policy.GitHubOwnersFor) holds on retag as on create, against the kind and owners the secret
// will hold once the write lands, on both stores, so a retag never writes a github-app secret the
// broker then refuses as github-owners-missing.
func TestARetagThatLeavesAGitHubAppSecretWithoutOwnersWritesNothing(t *testing.T) {
	refusals := [][]string{
		{"retag", "PLAIN_KEY", "--kind", policy.KindGitHubApp},
		{"retag", "APP_KEY", "--github-owners", "none"},
		{"retag", "PLAIN_KEY", "--github-owners", "ada"},
	}
	t.Run("aws", func(t *testing.T) {
		local := secrets.NewLocal(policytest.Secret("PLAIN_KEY", "ada@example.com", policy.TierAgent, "value"), kindTaggedSecret("APP_KEY"))
		startSecretBroker(t, servedBy(local))
		writes := &countingTagWrites{Local: local}
		useAWS(t, writes, testAccount, adaSignIn)
		for _, args := range refusals {
			_, stderr, code := runSecret(args...)
			if code != exitUsageError || !strings.Contains(stderr, "github") {
				t.Fatalf("%v: exit %d stderr %q; want a usage error naming the github-owners rule", args, code, stderr)
			}
		}
		if writes.tags != 0 || writes.untags != 0 {
			t.Fatalf("refused retags made %d TagResource and %d UntagResource calls, want none", writes.tags, writes.untags)
		}
		if _, stderr, code := runSecret("retag", "PLAIN_KEY", "--kind", policy.KindGitHubApp, "--github-owners", "ada"); code != 0 {
			t.Fatalf("retag to github-app with owners: exit %d stderr %q", code, stderr)
		}
		if tags := secrets.TagMap(describe(t, local, "PLAIN_KEY").Tags); tags[policy.TagKind] != policy.KindGitHubApp || tags[policy.TagGitHubOwners] != "ada" {
			t.Fatalf("tags = %v, want github-app with owner ada", tags)
		}
		if _, stderr, code := runSecret("retag", "APP_KEY", "--kind", "none", "--github-owners", "none"); code != 0 {
			t.Fatalf("removing the kind and its owners together: exit %d stderr %q", code, stderr)
		}
	})
	t.Run("gcp", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		useGCPIdentity(t, "authorized_user", "ada@example.com")
		settings := gcpTestSettings()
		startSecretBrokerWith(t, settings, gcpServedBy(fake, settings))
		for _, args := range [][]string{
			{"create", "PLAIN_KEY", "--owner", "me", "--tier", "agent"},
			{"create", "APP_KEY", "--owner", "me", "--tier", "agent", "--kind", policy.KindGitHubApp, "--github-owners", "acme-org ada"},
		} {
			useStdin(t, "stored-value")
			if _, stderr, code := runSecret(args...); code != 0 {
				t.Fatalf("%v: exit %d stderr %q", args, code, stderr)
			}
		}
		annotations := func() map[string]map[string]string {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			held := map[string]map[string]string{}
			for id, secret := range fake.secrets {
				held[id] = maps.Clone(secret.secret.GetAnnotations())
			}
			return held
		}
		before := annotations()
		for _, args := range refusals {
			_, stderr, code := runSecret(args...)
			if code != exitUsageError || !strings.Contains(stderr, "github") {
				t.Fatalf("%v: exit %d stderr %q; want a usage error naming the github-owners rule", args, code, stderr)
			}
		}
		after := annotations()
		for id := range before {
			if !maps.Equal(after[id], before[id]) {
				t.Fatalf("annotations of %s after refused retags = %v, want unchanged %v", id, after[id], before[id])
			}
		}
	})
}

// TestAGitHubOwnersListIsWrittenSpaceSeparatedAndACommaIsRefused: on AWS, whose tag values take a
// space and no comma, a list of several accounts is written as the broker reads it, lowercased,
// sorted and single-spaced; a comma anywhere is refused before create or retag writes anything,
// naming the space-separated form.
func TestAGitHubOwnersListIsWrittenSpaceSeparatedAndACommaIsRefused(t *testing.T) {
	local := secrets.NewLocal(kindTaggedSecret("APP_KEY"))
	startSecretBroker(t, servedBy(local))
	writes := &countingTagWrites{Local: local}
	useAWS(t, writes, testAccount, adaSignIn)
	useStdin(t, "stored-value")
	for _, args := range [][]string{
		{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--kind", policy.KindGitHubApp, "--github-owners", "acme-org,ada"},
		{"retag", "APP_KEY", "--github-owners", "acme-org, ada"},
	} {
		_, stderr, code := runSecret(args...)
		if code != exitUsageError || !strings.Contains(stderr, "spaces, not commas") || !strings.Contains(stderr, `"<login> <login>"`) {
			t.Fatalf("%v: exit %d stderr %q; want a usage error naming the space-separated form", args, code, stderr)
		}
	}
	if _, err := local.DescribeSecret(context.Background(), &secretsmanager.DescribeSecretInput{SecretId: aws.String(policytest.ID("NEW_KEY"))}); err == nil {
		t.Fatal("the refused create made NEW_KEY")
	}
	if writes.tags != 0 {
		t.Fatalf("the refused retag made %d TagResource calls, want none", writes.tags)
	}
	if _, stderr, code := runSecret("retag", "APP_KEY", "--github-owners", "Widgets-Org  ada acme-org"); code != 0 {
		t.Fatalf("retag to three owners: exit %d stderr %q", code, stderr)
	}
	if tags := secrets.TagMap(describe(t, local, "APP_KEY").Tags); tags[policy.TagGitHubOwners] != "acme-org ada widgets-org" {
		t.Fatalf("tags = %v, want github-owners %q", tags, "acme-org ada widgets-org")
	}
	useStdin(t, "stored-value")
	if _, stderr, code := runSecret("create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--kind", policy.KindGitHubApp, "--github-owners", "ada acme-org"); code != 0 {
		t.Fatalf("create with two owners: exit %d stderr %q", code, stderr)
	}
	if tags := secrets.TagMap(describe(t, local, "NEW_KEY").Tags); tags[policy.TagGitHubOwners] != "acme-org ada" {
		t.Fatalf("tags = %v, want github-owners %q", tags, "acme-org ada")
	}
}

// TestACreateThatNamesNoneAsItsGitHubOwnersWritesNothing: on create, `--github-owners none` (in
// any case) names no GitHub account. It is not the removal it is on retag, and the github-app
// secret it would write has no github-owners tag. Such a create is refused with a usage error
// before anything is written, on both stores, as a create without --github-owners is.
func TestACreateThatNamesNoneAsItsGitHubOwnersWritesNothing(t *testing.T) {
	owners := []string{"none", "None", "NONE"}
	t.Run("aws", func(t *testing.T) {
		local := secrets.NewLocal()
		startSecretBroker(t, servedBy(local))
		useAWS(t, local, testAccount, adaSignIn)
		for _, owner := range owners {
			useStdin(t, "stored-value")
			_, stderr, code := runSecret("create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--kind", policy.KindGitHubApp, "--github-owners", owner)
			if code != exitUsageError {
				t.Fatalf("create --github-owners %q: exit %d stderr %q; want a usage error", owner, code, stderr)
			}
			if _, err := local.DescribeSecret(context.Background(), &secretsmanager.DescribeSecretInput{SecretId: aws.String(policytest.ID("NEW_KEY"))}); err == nil {
				t.Fatalf("create --github-owners %q wrote NEW_KEY", owner)
			}
		}
	})
	t.Run("gcp", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		useGCPIdentity(t, "authorized_user", "ada@example.com")
		settings := gcpTestSettings()
		startSecretBrokerWith(t, settings, gcpServedBy(fake, settings))
		for _, owner := range owners {
			useStdin(t, "stored-value")
			_, stderr, code := runSecret("create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--kind", policy.KindGitHubApp, "--github-owners", owner)
			if code != exitUsageError {
				t.Fatalf("create --github-owners %q: exit %d stderr %q; want a usage error", owner, code, stderr)
			}
			fake.mu.Lock()
			_, written := fake.secrets[testGCPPrefix+"new-key"]
			fake.mu.Unlock()
			if written {
				t.Fatalf("create --github-owners %q wrote NEW_KEY", owner)
			}
		}
	})
}

// countingTagWrites is a secrets.Local that counts DescribeSecret, TagResource and UntagResource
// calls and can refuse UntagResource as IAM refuses it.
type countingTagWrites struct {
	*secrets.Local
	describes, tags, untags int
	refuseUntag             bool
}

func (c *countingTagWrites) DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	c.describes++
	return c.Local.DescribeSecret(ctx, in, o...)
}

func (c *countingTagWrites) TagResource(ctx context.Context, in *secretsmanager.TagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.TagResourceOutput, error) {
	c.tags++
	return c.Local.TagResource(ctx, in, o...)
}

func (c *countingTagWrites) UntagResource(ctx context.Context, in *secretsmanager.UntagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.UntagResourceOutput, error) {
	c.untags++
	if c.refuseUntag {
		return nil, &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized to perform: secretsmanager:UntagResource"}
	}
	return c.Local.UntagResource(ctx, in, o...)
}

func kindTaggedSecret(name string) secrets.LocalSecret {
	secret := policytest.KindSecret(name, "ada@example.com", policy.TierAgent, policy.KindGitHubApp, "value")
	secret.Tags[policy.TagMaxGrant] = "1h"
	return secret
}

// TestSecretRetagRemovalIsOneCallThatChangesNothingElse: on AWS a tag removal is its own
// UntagResource and nothing else, so IAM refusing it leaves every tag as it was, and a retag that
// both removes and sets tags is refused before any read or write, since it would take two calls.
// Each retag reads the secret's tags once, to build the tags it will leave and check them, and a
// write that lands reads them once more, to find a concurrent writer's (awsBackend.raceCheck).
func TestSecretRetagRemovalIsOneCallThatChangesNothingElse(t *testing.T) {
	local := secrets.NewLocal(kindTaggedSecret("APP_KEY"))
	startSecretBroker(t, servedBy(local))
	writes := &countingTagWrites{Local: local}
	useAWS(t, writes, testAccount, adaSignIn)

	for _, args := range [][]string{
		{"retag", "APP_KEY", "--kind", "none", "--tier", "human"},
		{"retag", "APP_KEY", "--kind", "none", "--owner", "shared"},
		{"retag", "APP_KEY", "--max-grant", "none", "--kind", policy.KindGoogleLogin},
	} {
		_, stderr, code := runSecret(args...)
		if code != exitUsageError || !strings.Contains(stderr, "two retags") {
			t.Fatalf("%v: exit %d stderr %q; want a usage error naming two retags", args, code, stderr)
		}
	}
	if writes.tags != 0 || writes.untags != 0 || writes.describes != 0 {
		t.Fatalf("refused mixes made %d TagResource, %d UntagResource and %d DescribeSecret calls, want none", writes.tags, writes.untags, writes.describes)
	}

	writes.refuseUntag = true
	_, stderr, code := runSecret("retag", "APP_KEY", "--kind", "none")
	if code != 1 || !strings.Contains(stderr, "UntagResource") {
		t.Fatalf("refused removal: exit %d stderr %q; want 1 naming the refusal", code, stderr)
	}
	if writes.tags != 0 || writes.untags != 1 {
		t.Fatalf("refused removal made %d TagResource and %d UntagResource calls, want 0 and 1", writes.tags, writes.untags)
	}
	tags := secrets.TagMap(describe(t, local, "APP_KEY").Tags)
	if tags[policy.TagOwner] != "ada@example.com" || tags[policy.TagTier] != policy.TierAgent || tags[policy.TagKind] != policy.KindGitHubApp || tags[policy.TagMaxGrant] != "1h" {
		t.Fatalf("tags after a refused removal = %v, want every tag unchanged", tags)
	}

	writes.refuseUntag = false
	stdout, stderr, code := runSecret("retag", "APP_KEY", "--kind", "none", "--max-grant", "none")
	if code != 0 || !strings.Contains(stdout, "removed kind, mint-service-account, max-grant") {
		t.Fatalf("removal: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if writes.tags != 0 || writes.untags != 2 || writes.describes != 3 {
		t.Fatalf("removals made %d TagResource, %d UntagResource and %d DescribeSecret calls in all, want 0, 2 and 3: one read of the tags and one write each, and one read back after the write that landed", writes.tags, writes.untags, writes.describes)
	}
	tags = secrets.TagMap(describe(t, local, "APP_KEY").Tags)
	if _, ok := tags[policy.TagKind]; ok {
		t.Fatalf("tags after removal = %v, want no kind", tags)
	}
	if _, ok := tags[policy.TagMaxGrant]; ok {
		t.Fatalf("tags after removal = %v, want no max-grant", tags)
	}
	if tags[policy.TagOwner] != "ada@example.com" || tags[policy.TagTier] != policy.TierAgent {
		t.Fatalf("tags after removal = %v, want owner and tier unchanged", tags)
	}
}

// TestSecretRefusedRemovalOnASharedSecretSaysOnlyWhatTheCloudSaid: a removal sends no owner or
// tier, so the cloud refusing one on a shared secret does not print the owner-and-tier hint.
func TestSecretRefusedRemovalOnASharedSecretSaysOnlyWhatTheCloudSaid(t *testing.T) {
	shared := kindTaggedSecret("SHARED_APP_KEY")
	shared.Tags[policy.TagOwner] = policy.OwnerShared
	local := secrets.NewLocal(shared)
	startSecretBroker(t, servedBy(local))
	useAWS(t, &countingTagWrites{Local: local, refuseUntag: true}, testAccount, adaSignIn)
	_, stderr, code := runSecret("retag", "SHARED_APP_KEY", "--kind", "none")
	if code != 1 || !strings.Contains(stderr, "UntagResource") || strings.Contains(stderr, "administrator") {
		t.Fatalf("refused removal on a shared secret: exit %d stderr %q; want 1 with the cloud's refusal alone", code, stderr)
	}
}

// TestSecretRetagSetsAndRemovesMaxGrant: retag --max-grant sets the tag on both backends and keeps
// kind, and --max-grant none removes it and keeps kind.
func TestSecretRetagSetsAndRemovesMaxGrant(t *testing.T) {
	t.Run("aws", func(t *testing.T) {
		local := secrets.NewLocal(kindTaggedSecret("APP_KEY"))
		startSecretBroker(t, servedBy(local))
		useAWS(t, local, testAccount, adaSignIn)
		if _, stderr, code := runSecret("retag", "APP_KEY", "--max-grant", "2h"); code != 0 {
			t.Fatalf("retag --max-grant 2h: exit %d, stderr %q", code, stderr)
		}
		tags := secrets.TagMap(describe(t, local, "APP_KEY").Tags)
		if tags[policy.TagMaxGrant] != "2h" || tags[policy.TagKind] != policy.KindGitHubApp || tags[policy.TagOwner] != "ada@example.com" {
			t.Fatalf("tags = %v, want max-grant 2h beside the kept kind and owner", tags)
		}
		if _, stderr, code := runSecret("retag", "APP_KEY", "--max-grant", "none"); code != 0 {
			t.Fatalf("retag --max-grant none: exit %d, stderr %q", code, stderr)
		}
		tags = secrets.TagMap(describe(t, local, "APP_KEY").Tags)
		if _, ok := tags[policy.TagMaxGrant]; ok || tags[policy.TagKind] != policy.KindGitHubApp {
			t.Fatalf("tags = %v, want no max-grant and the kind kept", tags)
		}
		for _, value := range []string{"soon", "500ms"} {
			if _, stderr, code := runSecret("retag", "APP_KEY", "--max-grant", value); code != exitUsageError {
				t.Fatalf("retag --max-grant %s: exit %d stderr %q; want usage error", value, code, stderr)
			}
		}
	})
	t.Run("gcp", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		useGCPIdentity(t, "authorized_user", "ada@example.com")
		settings := gcpTestSettings()
		startSecretBrokerWith(t, settings, gcpServedBy(fake, settings))
		useStdin(t, "stored-value")
		if _, stderr, code := runSecret("create", "LOGIN_KEY", "--owner", "me", "--tier", "agent", "--kind", policy.KindGoogleLogin); code != 0 {
			t.Fatalf("create: exit %d, stderr %q", code, stderr)
		}
		id := testGCPPrefix + "login-key"
		annotations := func() map[string]string {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			return fake.secrets[id].secret.GetAnnotations()
		}
		if _, stderr, code := runSecret("retag", "LOGIN_KEY", "--max-grant", "45m"); code != 0 {
			t.Fatalf("retag --max-grant 45m: exit %d, stderr %q", code, stderr)
		}
		if got := annotations(); got[policy.TagMaxGrant] != "45m" || got[policy.TagKind] != policy.KindGoogleLogin || got[policy.TagOwner] != "ada@example.com" {
			t.Fatalf("annotations = %v, want max-grant 45m beside the kept kind and owner", got)
		}
		before := annotations()
		if _, stderr, code := runSecret("retag", "LOGIN_KEY", "--kind", "none", "--tier", "human"); code != exitUsageError || !strings.Contains(stderr, "two retags") {
			t.Fatalf("mixed retag: exit %d stderr %q; want a usage error naming two retags", code, stderr)
		}
		if got := annotations(); !maps.Equal(got, before) {
			t.Fatalf("annotations after a refused mix = %v, want unchanged %v", got, before)
		}
		if _, stderr, code := runSecret("retag", "LOGIN_KEY", "--max-grant", "none"); code != 0 {
			t.Fatalf("retag --max-grant none: exit %d, stderr %q", code, stderr)
		}
		if got := annotations(); got[policy.TagKind] != policy.KindGoogleLogin || got[policy.TagTier] != policy.TierAgent {
			t.Fatalf("annotations = %v, want kind and tier kept", got)
		} else if _, ok := got[policy.TagMaxGrant]; ok {
			t.Fatalf("annotations = %v, want no max-grant", got)
		}
	})
}
