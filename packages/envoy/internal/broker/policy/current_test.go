package policy_test

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// staleLister freezes ListSecrets at construction while DescribeSecret stays live: AWS's
// documented ListSecrets lag, reproduced.
type staleLister struct {
	page *secretsmanager.ListSecretsOutput
}

func (s staleLister) ListSecrets(context.Context, *secretsmanager.ListSecretsInput, ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	return s.page, nil
}

func TestRefreshKeepsWhatARecentRereadSettled(t *testing.T) {
	store := secrets.NewLocal(policytest.Secret("OLD_KEY", "ada@example.com", policy.TierAgent, "v1"))
	stale, err := store.ListSecrets(context.Background(), &secretsmanager.ListSecretsInput{})
	if err != nil {
		t.Fatal(err)
	}
	loader := policytest.Loader(store)
	loader.Secrets = staleLister{page: stale} // the listing predates everything below
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	store.Put(policytest.Secret("NEW_KEY", "ada@example.com", policy.TierAgent, "v1"))
	if lk, _ := cur.RefreshOne(context.Background(), "NEW_KEY"); !lk.Served {
		t.Fatal("reread must serve NEW_KEY")
	}
	store.Delete(policytest.ID("OLD_KEY"))
	if lk, _ := cur.RefreshOne(context.Background(), "OLD_KEY"); lk.Served {
		t.Fatal("reread must drop OLD_KEY")
	}
	if err := cur.Refresh(context.Background()); err != nil { // the stale listing still lists OLD_KEY and not NEW_KEY
		t.Fatal(err)
	}
	set := cur.Get()
	if _, ok := set.Secrets["NEW_KEY"]; !ok {
		t.Fatal("a full reload from a lagging listing dropped NEW_KEY")
	}
	if _, ok := set.Secrets["OLD_KEY"]; ok {
		t.Fatal("a full reload from a lagging listing resurrected deleted OLD_KEY")
	}
}

// TestRecentNamesExpireAfterListLag pins that the window is bounded: a reread outlives a lagging
// listing until listLag has passed since it, and after that the listing wins again.
func TestRecentNamesExpireAfterListLag(t *testing.T) {
	store := secrets.NewLocal()
	stale, err := store.ListSecrets(context.Background(), &secretsmanager.ListSecretsInput{})
	if err != nil {
		t.Fatal(err)
	}
	loader := policytest.Loader(store)
	loader.Secrets = staleLister{page: stale} // never lists NEW_KEY
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reread := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	now := reread
	policy.SetNow(cur, func() time.Time { return now })
	store.Put(policytest.Secret("NEW_KEY", "ada@example.com", policy.TierAgent, "v1"))
	if lk, _ := cur.RefreshOne(context.Background(), "NEW_KEY"); !lk.Served {
		t.Fatal("reread must serve NEW_KEY")
	}

	now = reread.Add(policy.ListLag - time.Second)
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := cur.Get().Secrets["NEW_KEY"]; !ok {
		t.Fatal("a full reload inside listLag of the reread dropped NEW_KEY")
	}

	now = reread.Add(policy.ListLag + time.Second)
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := cur.Get().Secrets["NEW_KEY"]; ok {
		t.Fatal("a full reload listLag after the reread still kept NEW_KEY over the listing")
	}
}

// slowLister answers a frozen page, then runs fetched: a reload whose listing is fetched at one
// time and whose Load returns at a later one.
type slowLister struct {
	page    *secretsmanager.ListSecretsOutput
	fetched func()
}

func (s *slowLister) ListSecrets(context.Context, *secretsmanager.ListSecretsInput, ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	if s.fetched != nil {
		s.fetched()
	}
	return s.page, nil
}

// TestARefreshStraddlingListLagStillRereadsTheName pins that the window is measured from when the
// listing was fetched: a reload whose lagging listing was read inside listLag of a reread, but
// whose Load returns after it ran out, still rereads that name rather than trusting the listing.
func TestARefreshStraddlingListLagStillRereadsTheName(t *testing.T) {
	store := secrets.NewLocal(policytest.Secret("OLD_KEY", "ada@example.com", policy.TierAgent, "v1"))
	stale, err := store.ListSecrets(context.Background(), &secretsmanager.ListSecretsInput{})
	if err != nil {
		t.Fatal(err)
	}
	lister := &slowLister{page: stale} // lists OLD_KEY and never NEW_KEY
	loader := policytest.Loader(store)
	loader.Secrets = lister
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reread := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	now := reread
	policy.SetNow(cur, func() time.Time { return now })
	store.Put(policytest.Secret("NEW_KEY", "ada@example.com", policy.TierAgent, "v1"))
	if lk, _ := cur.RefreshOne(context.Background(), "NEW_KEY"); !lk.Served {
		t.Fatal("reread must serve NEW_KEY")
	}
	store.Delete(policytest.ID("OLD_KEY"))
	if lk, _ := cur.RefreshOne(context.Background(), "OLD_KEY"); lk.Served {
		t.Fatal("reread must drop OLD_KEY")
	}

	// The listing is fetched inside listLag of the rereads, and Load returns after it ran out.
	now = reread.Add(policy.ListLag - time.Second)
	lister.fetched = func() { now = reread.Add(policy.ListLag + time.Second) }
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	set := cur.Get()
	if _, ok := set.Secrets["NEW_KEY"]; !ok {
		t.Fatal("a reload whose listing predates listLag's end dropped NEW_KEY")
	}
	if _, ok := set.Secrets["OLD_KEY"]; ok {
		t.Fatal("a reload whose listing predates listLag's end resurrected deleted OLD_KEY")
	}
}
