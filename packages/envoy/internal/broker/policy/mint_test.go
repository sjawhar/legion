package policy_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

const mintTarget = "agent-admin@example-project.iam.gserviceaccount.com"

// minted is a human-tier secret carrying the mint tag target and no value.
func minted(name, target string) secrets.LocalSecret {
	s := policytest.Secret(name, owner, policy.TierHuman, "")
	s.Tags[policy.TagMint] = target
	return s
}

// TestMintKindServedWithoutValue pins the minted kind: a secret under the prefix carrying a
// well-formed mint-service-account tag and no version is served, its Source naming the target,
// while the same secret without the tag is still refused for having no value.
func TestMintKindServedWithoutValue(t *testing.T) {
	logged := policytest.CaptureLog(t)
	store := secrets.NewLocal(minted("ADMIN_GCP_TOKEN", mintTarget), policytest.Secret("UNSEEDED", owner, policy.TierHuman, ""))
	set, err := policytest.Loader(store).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := set.Secrets["ADMIN_GCP_TOKEN"]
	if !ok || got.Source != policy.MintSourcePrefix+mintTarget || got.MintTarget != mintTarget || got.Owner != owner || got.Tier != policy.TierHuman {
		t.Fatalf("ADMIN_GCP_TOKEN = %+v (served %v); want served, human, Source %q", got, ok, policy.MintSourcePrefix+mintTarget)
	}
	if _, ok := set.Secrets["UNSEEDED"]; ok {
		t.Fatal("a secret with no value and no mint tag was served")
	}
	if logged.String() != refusedLine("UNSEEDED", policy.ReasonNoCurrentValue) {
		t.Fatalf("logged %q, want only UNSEEDED's no-current-value refusal", logged.String())
	}
	lk, err := policytest.Loader(store).LoadOne(context.Background(), "ADMIN_GCP_TOKEN")
	if err != nil || !lk.Served || lk.Secret.Source != policy.MintSourcePrefix+mintTarget {
		t.Fatalf("LoadOne(ADMIN_GCP_TOKEN) = %+v, %v; want it served as Load serves it", lk, err)
	}
}

// TestMintKindMalformedTargetRefused pins the refusal of a mint tag that names no service
// account, and that the name and tag reasons still come first.
func TestMintKindMalformedTargetRefused(t *testing.T) {
	logged := policytest.CaptureLog(t)
	untiered := minted("UNTIERED", mintTarget)
	delete(untiered.Tags, policy.TagTier)
	store := secrets.NewLocal(
		minted("NOT_AN_EMAIL", "not-an-email"),
		minted("A_PERSON", "sami@example.com"),
		untiered,
	)
	set, err := policytest.Loader(store).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Secrets) != 0 {
		t.Fatalf("served %+v, want nothing", set.Secrets)
	}
	for _, line := range []string{
		refusedLine("NOT_AN_EMAIL", policy.ReasonMintTargetMalformed),
		refusedLine("A_PERSON", policy.ReasonMintTargetMalformed),
		refusedLine("UNTIERED", policy.ReasonTierTagMissing),
	} {
		if !strings.Contains(logged.String(), line) {
			t.Errorf("log %q lacks %q", logged.String(), line)
		}
	}
}
