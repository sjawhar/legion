package policy_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// captureLog points the default slog handler, which the broker logs through, at a buffer with
// no timestamp, so a test reads the exact lines the broker writes.
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	logged := &lockedBuffer{}
	flags, output := log.Flags(), log.Writer()
	log.SetFlags(0)
	log.SetOutput(logged)
	t.Cleanup(func() { log.SetFlags(flags); log.SetOutput(output) })
	return logged
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// refusedLine is the exact line the broker logs for a refused secret: the deployment's alarm
// filters on it.
func refusedLine(name, reason string) string {
	return "ERROR agent secret policy refused name=" + policytest.ID(name) + " reason=" + reason + "\n"
}

// TestLoadServesEveryWellTaggedSecretOnTheKey pins what the loader reads from Secrets Manager: a
// secret's request name from its name under the prefix, owner and tier from its tags, and its ARN
// as the source, for every form Secrets Manager reports the agent-secrets key in — its ARN, its
// key id, and an alias that points at it by name or by ARN.
func TestLoadServesEveryWellTaggedSecretOnTheKey(t *testing.T) {
	logged := captureLog(t)
	keyID := policytest.KeyARN[strings.LastIndex(policytest.KeyARN, "/")+1:]
	byForm := map[string]string{
		"BY_ARN":       policytest.KeyARN,
		"BY_KEY_ID":    keyID,
		"BY_ALIAS":     "alias/agent-secrets",
		"BY_ALIAS_ARN": "arn:aws:kms:us-east-1:111122223333:alias/agent-secrets",
	}
	store := secrets.NewLocal()
	store.Alias(policytest.KeyARN, "alias/agent-secrets")
	for name, keyForm := range byForm {
		s := policytest.Secret(name, owner, policy.TierAgent, "v")
		s.KmsKeyID = keyForm
		store.Put(s)
	}
	store.Put(policytest.Secret("DEEL_API_KEY", policy.OwnerShared, policy.TierHuman, "v"))

	set, err := policytest.Loader(store).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Secrets) != len(byForm)+1 {
		t.Fatalf("served %d secrets, want %d: %+v", len(set.Secrets), len(byForm)+1, set.Secrets)
	}
	deel := set.Secrets["DEEL_API_KEY"]
	if deel.Name != "DEEL_API_KEY" || deel.Owner != policy.OwnerShared || deel.Tier != policy.TierHuman || deel.ARN != secrets.LocalARN(policytest.ID("DEEL_API_KEY")) {
		t.Fatalf("DEEL_API_KEY = %+v, want shared, human, read by its ARN", deel)
	}
	if logged.String() != "" {
		t.Fatalf("a load refusing nothing logged:\n%s", logged)
	}
}

// TestLoadRefusesEachUnservableSecretByNameWithTheExactLine pins every reason a secret under the
// prefix is refused, each logged as the one line the deployment's alarm filters on, while every
// other secret is still served: one person's malformed tag never freezes anyone else's policy.
func TestLoadRefusesEachUnservableSecretByNameWithTheExactLine(t *testing.T) {
	refusals := map[string]struct {
		secret secrets.LocalSecret
		reason string
	}{}
	refuse := func(name, reason string, edit func(*secrets.LocalSecret)) {
		s := policytest.Secret(name, owner, policy.TierAgent, "v")
		edit(&s)
		refusals[name] = struct {
			secret secrets.LocalSecret
			reason string
		}{s, reason}
	}
	refuse("NO_OWNER", policy.ReasonOwnerTagMissing, func(s *secrets.LocalSecret) { delete(s.Tags, policy.TagOwner) })
	refuse("GITHUB_LOGIN_OWNER", policy.ReasonOwnerTagMalformed, func(s *secrets.LocalSecret) { s.Tags[policy.TagOwner] = "sjawhar" })
	refuse("CASED_OWNER", policy.ReasonOwnerTagMalformed, func(s *secrets.LocalSecret) { s.Tags[policy.TagOwner] = "Sami@Example.com" })
	refuse("EMPTY_OWNER", policy.ReasonOwnerTagMalformed, func(s *secrets.LocalSecret) { s.Tags[policy.TagOwner] = "" })
	refuse("UNREGISTERED_SERVICE", policy.ReasonOwnerTagMalformed, func(s *secrets.LocalSecret) { s.Tags[policy.TagOwner] = "dispatch" })
	refuse("NO_TIER", policy.ReasonTierTagMissing, func(s *secrets.LocalSecret) { delete(s.Tags, policy.TagTier) })
	refuse("ADMIN_TIER", policy.ReasonTierTagMalformed, func(s *secrets.LocalSecret) { s.Tags[policy.TagTier] = "admin" })
	refuse("CASED_TAG_KEY", policy.ReasonOwnerTagMissing, func(s *secrets.LocalSecret) {
		s.Tags["Owner"] = s.Tags[policy.TagOwner]
		delete(s.Tags, policy.TagOwner)
	})
	refuse("SERVICE_HUMAN", policy.ReasonServiceOwnerHumanTier, func(s *secrets.LocalSecret) {
		s.Tags[policy.TagOwner], s.Tags[policy.TagTier] = legion, policy.TierHuman
	})
	refuse("MANAGED_KEY", policy.ReasonNotOnAgentSecretsKey, func(s *secrets.LocalSecret) { s.KmsKeyID = "" })
	refuse("ANOTHER_KEY", policy.ReasonNotOnAgentSecretsKey, func(s *secrets.LocalSecret) {
		s.KmsKeyID = "arn:aws:kms:us-east-1:111122223333:key/another-key"
	})
	refuse("ANOTHER_KEYS_ALIAS", policy.ReasonNotOnAgentSecretsKey, func(s *secrets.LocalSecret) { s.KmsKeyID = "alias/someone-elses" })

	store := secrets.NewLocal(policytest.Secret("WELL_TAGGED", owner, policy.TierAgent, "v"))
	store.Alias("arn:aws:kms:us-east-1:111122223333:key/another-key", "alias/someone-elses")
	for _, r := range refusals {
		store.Put(r.secret)
	}
	badName := policytest.Secret("X", owner, policy.TierAgent, "v")
	badName.Name = policytest.Prefix + "Deel_Api_Key"
	store.Put(badName)

	logged := captureLog(t)
	set, err := policytest.Loader(store, legion).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Secrets) != 1 || set.Secrets["WELL_TAGGED"].Name != "WELL_TAGGED" {
		t.Fatalf("served %+v, want only WELL_TAGGED", set.Secrets)
	}
	lines := logged.String()
	for name, r := range refusals {
		if want := refusedLine(name, r.reason); strings.Count(lines, want) != 1 {
			t.Errorf("log has %d of %q, want exactly one:\n%s", strings.Count(lines, want), want, lines)
		}
	}
	if want := "ERROR agent secret policy refused name=" + badName.Name + " reason=" + policy.ReasonNameMalformed + "\n"; !strings.Contains(lines, want) {
		t.Errorf("log has no %q:\n%s", want, lines)
	}
	if got := strings.Count(lines, "\n"); got != len(refusals)+1 {
		t.Errorf("logged %d lines, want one per refused secret (%d):\n%s", got, len(refusals)+1, lines)
	}
}

// pagedSecrets answers ListSecrets one secret per page, the shape a namespace bigger than one page
// comes back in, and counts the filters it was asked for.
type pagedSecrets struct {
	entries []smtypes.SecretListEntry
	filters [][]smtypes.Filter
}

func (p *pagedSecrets) ListSecrets(_ context.Context, in *secretsmanager.ListSecretsInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	p.filters = append(p.filters, in.Filters)
	i := 0
	if in.NextToken != nil {
		i = int((*in.NextToken)[0] - '0')
	}
	out := &secretsmanager.ListSecretsOutput{SecretList: p.entries[i : i+1]}
	if i+1 < len(p.entries) {
		out.NextToken = aws.String(string(rune('0' + i + 1)))
	}
	return out, nil
}

// noAliases fails a test that resolves an alias it never needed.
type noAliases struct{ t *testing.T }

func (n noAliases) ListAliases(context.Context, *kms.ListAliasesInput, ...func(*kms.Options)) (*kms.ListAliasesOutput, error) {
	n.t.Error("ListAliases called, but no secret names its key by an alias")
	return &kms.ListAliasesOutput{}, nil
}

// TestLoadReadsEveryPageOfThePrefixOnly pins that the loader asks Secrets Manager for the
// namespace prefix alone, reads every page, skips a name the prefix filter matched only
// case-insensitively, and never resolves aliases nobody used.
func TestLoadReadsEveryPageOfThePrefixOnly(t *testing.T) {
	entry := func(name string) smtypes.SecretListEntry {
		return smtypes.SecretListEntry{
			Name: aws.String(name), ARN: aws.String("arn:" + name), KmsKeyId: aws.String(policytest.KeyARN),
			Tags: []smtypes.Tag{
				{Key: aws.String(policy.TagOwner), Value: aws.String(policy.OwnerShared)},
				{Key: aws.String(policy.TagTier), Value: aws.String(policy.TierAgent)},
			},
		}
	}
	sm := &pagedSecrets{entries: []smtypes.SecretListEntry{
		entry(policytest.Prefix + "first"), entry(policytest.Prefix + "second"),
		entry(strings.ToUpper(policytest.Prefix) + "third"), entry(policytest.Prefix + "fourth"),
	}}
	loader := policy.Loader{Secrets: sm, Aliases: noAliases{t}, Prefix: policytest.Prefix, KeyARN: policytest.KeyARN}
	set, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Secrets) != 3 || set.Secrets["FIRST"].ARN != "arn:"+policytest.Prefix+"first" || set.Secrets["FOURTH"].Name != "FOURTH" {
		t.Fatalf("served %+v, want FIRST, SECOND and FOURTH", set.Secrets)
	}
	if len(sm.filters) != 4 {
		t.Fatalf("asked for %d pages, want 4", len(sm.filters))
	}
	for _, f := range sm.filters {
		if len(f) != 1 || f[0].Key != smtypes.FilterNameStringTypeName || len(f[0].Values) != 1 || f[0].Values[0] != policytest.Prefix {
			t.Fatalf("ListSecrets filters = %+v, want name=%s alone", f, policytest.Prefix)
		}
	}
}

// failingSecrets fails ListSecrets once failing is set.
type failingSecrets struct {
	*secrets.Local
	mu      sync.Mutex
	failing bool
}

func (f *failingSecrets) ListSecrets(ctx context.Context, in *secretsmanager.ListSecretsInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	f.mu.Lock()
	failing := f.failing
	f.mu.Unlock()
	if failing {
		return nil, errors.New("ThrottlingException: Rate exceeded")
	}
	return f.Local.ListSecrets(ctx, in, opts...)
}

// TestReloadKeepsThePolicyWhenSecretsManagerFails pins the live policy across reloads: a tag edit
// takes effect on the next tick, a reload that cannot reach Secrets Manager logs the exact line
// the deployment's alarm filters on and keeps the previous policy, and a first load that fails
// fails the broker's boot.
func TestReloadKeepsThePolicyWhenSecretsManagerFails(t *testing.T) {
	store := &failingSecrets{Local: secrets.NewLocal(policytest.Secret("DEEL_API_KEY", owner, policy.TierAgent, "v"))}
	loader := policy.Loader{Secrets: store, Aliases: store, Prefix: policytest.Prefix, KeyARN: policytest.KeyARN}
	logged := captureLog(t)
	cur, err := policy.NewCurrent(t.Context(), loader, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("NewCurrent: %v", err)
	}
	first := cur.Get()

	store.Put(policytest.Secret("DEEL_API_KEY", owner, policy.TierHuman, "v"))
	await(t, func() bool { return cur.Get().Secrets["DEEL_API_KEY"].Tier == policy.TierHuman })
	if cur.Get().Version == first.Version {
		t.Fatalf("a tag edit left the version at %s", first.Version)
	}

	edited := cur.Get()
	store.mu.Lock()
	store.failing = true
	store.mu.Unlock()
	const failed = "ERROR agent secret policy load failed; previous policy kept error=\"list secrets under " + policytest.Prefix + ": ThrottlingException: Rate exceeded\"\n"
	await(t, func() bool { return strings.Contains(logged.String(), failed) })
	if cur.Get() != edited {
		t.Fatalf("a failed reload replaced the policy: %+v", cur.Get())
	}

	if _, err := policy.NewCurrent(t.Context(), loader, time.Hour); err == nil {
		t.Fatal("NewCurrent with Secrets Manager failing = nil error, want the first load's failure")
	}
}

func await(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestVersionNamesThePolicyNotItsListingOrder pins that the version is a function of the served
// policy alone: the same secrets listed in another order give the same version, and any owner,
// tier or ARN change gives another.
func TestVersionNamesThePolicyNotItsListingOrder(t *testing.T) {
	load := func(ss ...secrets.LocalSecret) string {
		t.Helper()
		set, err := policytest.Loader(secrets.NewLocal(ss...)).Load(context.Background())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return set.Version
	}
	a := policytest.Secret("A_KEY", owner, policy.TierAgent, "v")
	b := policytest.Secret("B_KEY", policy.OwnerShared, policy.TierHuman, "v")
	base := load(a, b)
	if load(b, a) != base {
		t.Fatal("the same secrets in another order gave another version")
	}
	value := b
	value.Value = "another value"
	if load(a, value) != base {
		t.Fatal("a value change gave another version; the version names the policy, not the values")
	}
	for name, changed := range map[string]secrets.LocalSecret{
		"owner": policytest.Secret("B_KEY", other, policy.TierHuman, "v"),
		"tier":  policytest.Secret("B_KEY", policy.OwnerShared, policy.TierAgent, "v"),
		"name":  policytest.Secret("C_KEY", policy.OwnerShared, policy.TierHuman, "v"),
	} {
		if load(a, changed) == base {
			t.Errorf("a changed %s left the version unchanged", name)
		}
	}
}
