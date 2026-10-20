package policy_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

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
// service account; a name BROKER_MINT_TARGETS does not list; under the listed name, another
// account, another owner, a shared owner, the agent tier, or a stored value; and no setting at
// all. The name and tag reasons still come first.
func TestMintKindMalformedTargetRefused(t *testing.T) {
	logged := policytest.CaptureLog(t)
	untiered := minted("UNTIERED", policytest.MintOwner, policy.TierHuman, mintTarget)
	delete(untiered.Tags, policy.TagTier)
	store := secrets.NewLocal(
		minted("NOT_AN_EMAIL", policytest.MintOwner, policy.TierHuman, "not-an-email"),
		minted("A_PERSON", policytest.MintOwner, policy.TierHuman, "sami@example.com"),
		minted("BUILD_CACHE_KEY", policytest.MintOwner, policy.TierHuman, mintTarget),
		untiered,
	)
	set, err := policytest.Loader(store).Load(context.Background())
	if err != nil || len(set.Secrets) != 0 {
		t.Fatalf("Load = %+v, %v; want nothing served", set, err)
	}
	for _, line := range []string{
		refusedLine("NOT_AN_EMAIL", policy.ReasonMintTargetMalformed),
		refusedLine("A_PERSON", policy.ReasonMintTargetMalformed),
		refusedLine("BUILD_CACHE_KEY", policy.ReasonMintTargetNotListed),
		refusedLine("UNTIERED", policy.ReasonTierTagMissing),
	} {
		if !strings.Contains(logged.String(), line) {
			t.Errorf("log %q lacks %q", logged.String(), line)
		}
	}
	withValue := minted(policytest.MintName, policytest.MintOwner, policy.TierHuman, mintTarget)
	withValue.Value = "stored"
	for reason, s := range map[string]secrets.LocalSecret{
		"another account": minted(policytest.MintName, policytest.MintOwner, policy.TierHuman, "other-admin@example-project.iam.gserviceaccount.com"),
		"another owner":   minted(policytest.MintName, other, policy.TierHuman, mintTarget),
		"a shared owner":  minted(policytest.MintName, policy.OwnerShared, policy.TierHuman, mintTarget),
		"the agent tier":  minted(policytest.MintName, policytest.MintOwner, policy.TierAgent, mintTarget),
		"a stored value":  withValue,
	} {
		one := secrets.NewLocal(s)
		if set, err := policytest.Loader(one).Load(context.Background()); err != nil || len(set.Secrets) != 0 {
			t.Errorf("Load of the listed name with %s = %+v, %v; want nothing served", reason, set, err)
		}
	}
	if !strings.Contains(logged.String(), refusedLine(policytest.MintName, policy.ReasonMintedHasValue)) {
		t.Errorf("log lacks the minted-has-value refusal")
	}
	loader := policytest.Loader(secrets.NewLocal(minted(policytest.MintName, policytest.MintOwner, policy.TierHuman, mintTarget)))
	loader.MintTargets = nil
	if set, err := loader.Load(context.Background()); err != nil || len(set.Secrets) != 0 {
		t.Fatalf("Load with no mint targets = %+v, %v; want nothing served", set, err)
	}
}

// TestStillMintedHoldsOneReadPerNamePerWindow pins the release reread's hold: fifty releases at
// once, and more within the window, cost one store read; once the window passes, a deleted secret
// reads as gone and a retagged one as another source.
func TestStillMintedHoldsOneReadPerNamePerWindow(t *testing.T) {
	store := secrets.NewLocal(minted(policytest.MintName, policytest.MintOwner, policy.TierHuman, mintTarget))
	count := &policytest.CountingDescriber{SecretsManagerAPI: store}
	loader := policytest.Loader(store)
	loader.Store = policytest.StoreThrough(store, count)
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	clock := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	policy.SetNow(cur, func() time.Time { mu.Lock(); defer mu.Unlock(); return clock })
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }
	base := count.Calls.Load()
	source := policy.MintSourcePrefix + mintTarget
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if ok, err := cur.StillMinted(context.Background(), policytest.MintName, source); !ok || err != nil {
				t.Errorf("StillMinted = %v, %v; want served", ok, err)
			}
		})
	}
	wg.Wait()
	advance(29 * time.Second)
	if ok, _ := cur.StillMinted(context.Background(), policytest.MintName, source); !ok || count.Calls.Load()-base != 1 {
		t.Fatalf("51 releases within the window made %d store reads, served %v; want 1 and served", count.Calls.Load()-base, ok)
	}
	gone := minted(policytest.MintName, policytest.MintOwner, policy.TierHuman, mintTarget)
	gone.DeletedAt = new(time.Now())
	store.Put(gone)
	advance(2 * time.Second)
	if ok, err := cur.StillMinted(context.Background(), policytest.MintName, source); ok || err != nil || count.Calls.Load()-base != 2 {
		t.Fatalf("after a delete and the window = %v, %v after %d reads; want refused after 2", ok, err, count.Calls.Load()-base)
	}
	store.Put(minted(policytest.MintName, policytest.MintOwner, policy.TierHuman, "other-admin@example-project.iam.gserviceaccount.com"))
	advance(31 * time.Second)
	if ok, _ := cur.StillMinted(context.Background(), policytest.MintName, source); ok {
		t.Fatal("a retagged secret still reads as this grant's minted secret")
	}
}
