package policy_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

const mintTarget = policytest.MintTarget

// minted is a secret of owner and tier carrying the mint tag target and no value.
func minted(name, owner, tier, target string) secrets.LocalSecret {
	s := policytest.Secret(name, owner, tier, "")
	s.Tags[policy.TagMint] = target
	return s
}

// TestMintKindServedWithoutValue pins the minted kind: a human-tier secret of the listed owner
// carrying a listed mint-service-account tag and no version is served, its Source naming the
// target, while a secret without the tag is still refused for having no value.
func TestMintKindServedWithoutValue(t *testing.T) {
	logged := policytest.CaptureLog(t)
	store := secrets.NewLocal(minted("ADMIN_GCP_TOKEN", policytest.MintOwner, policy.TierHuman, mintTarget), policytest.Secret("UNSEEDED", owner, policy.TierHuman, ""))
	set, err := policytest.Loader(store).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := set.Secrets["ADMIN_GCP_TOKEN"]
	if !ok || got.Source != policy.MintSourcePrefix+mintTarget || got.Owner != policytest.MintOwner || got.Tier != policy.TierHuman {
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

// TestMintKindMalformedTargetRefused pins the refusals of a minted secret: a tag that names no
// service account, an account BROKER_MINT_TARGETS does not list, another owner than the listed
// one, and the agent tier, each refused; the name and tag reasons still come first.
func TestMintKindMalformedTargetRefused(t *testing.T) {
	logged := policytest.CaptureLog(t)
	untiered := minted("UNTIERED", policytest.MintOwner, policy.TierHuman, mintTarget)
	delete(untiered.Tags, policy.TagTier)
	store := secrets.NewLocal(
		minted("NOT_AN_EMAIL", policytest.MintOwner, policy.TierHuman, "not-an-email"),
		minted("A_PERSON", policytest.MintOwner, policy.TierHuman, "sami@example.com"),
		minted("UNLISTED", policytest.MintOwner, policy.TierHuman, "other-admin@example-project.iam.gserviceaccount.com"),
		minted("OTHER_OWNER", other, policy.TierHuman, mintTarget),
		minted("SHARED_OWNER", policy.OwnerShared, policy.TierHuman, mintTarget),
		minted("AGENT_TIER", policytest.MintOwner, policy.TierAgent, mintTarget),
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
		refusedLine("UNLISTED", policy.ReasonMintTargetNotListed),
		refusedLine("OTHER_OWNER", policy.ReasonMintTargetNotListed),
		refusedLine("SHARED_OWNER", policy.ReasonMintTargetNotListed),
		refusedLine("AGENT_TIER", policy.ReasonMintTargetNotListed),
		refusedLine("UNTIERED", policy.ReasonTierTagMissing),
	} {
		if !strings.Contains(logged.String(), line) {
			t.Errorf("log %q lacks %q", logged.String(), line)
		}
	}
	// With no BROKER_MINT_TARGETS, no minted secret is served at all.
	loader := policytest.Loader(store)
	loader.MintTargets = nil
	store.Put(minted("ADMIN_GCP_TOKEN", policytest.MintOwner, policy.TierHuman, mintTarget))
	if set, err := loader.Load(context.Background()); err != nil || len(set.Secrets) != 0 {
		t.Fatalf("Load with no mint targets = %+v, %v; want nothing served", set, err)
	}
}
