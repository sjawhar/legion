package policy_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
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

// TestAReloadThatCatchesAChangeKeepsItForListLag pins that a reload which, rereading a recent
// name, finds an answer the live policy did not hold re-anchors that name: a secret deleted after
// a reread served it, with no reread of its own, is dropped by the next reload's re-describe, and a
// reload listLag after the reread, from a listing that still predates the delete, keeps it out
// rather than serving it again.
func TestAReloadThatCatchesAChangeKeepsItForListLag(t *testing.T) {
	store := secrets.NewLocal(policytest.Secret("OLD_KEY", "ada@example.com", policy.TierAgent, "v1"))
	stale, err := store.ListSecrets(context.Background(), &secretsmanager.ListSecretsInput{})
	if err != nil {
		t.Fatal(err)
	}
	lister := &slowLister{page: stale} // lists OLD_KEY throughout
	loader := policytest.Loader(store)
	loader.Secrets = lister
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reread := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	now := reread
	policy.SetNow(cur, func() time.Time { return now })
	if lk, _ := cur.RefreshOne(context.Background(), "OLD_KEY"); !lk.Served {
		t.Fatal("reread must serve OLD_KEY")
	}

	// Deleted (from the console, say) while the next reload lists: its listing still shows OLD_KEY,
	// and its own re-describe finds it gone.
	now = reread.Add(policy.ListLag / 2)
	lister.fetched = func() { store.Delete(policytest.ID("OLD_KEY")) }
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := cur.Get().Secrets["OLD_KEY"]; ok {
		t.Fatal("the reload whose own re-describe found OLD_KEY deleted still served it")
	}

	lister.fetched = nil
	now = reread.Add(policy.ListLag + time.Second)
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := cur.Get().Secrets["OLD_KEY"]; ok {
		t.Fatal("a reload listLag after the reread, but not after the reload that found OLD_KEY deleted, served it again from a lagging listing")
	}
}

// TestARereadOfANameThatExistsNowhereCostsNoReload pins that a reread finding absent a name the
// live policy did not serve is not kept for the next reload: anyone may ask for a reread, so
// otherwise every name a caller invents would cost each reload in the next listLag one more
// DescribeSecret under the writer lock. TestRefreshKeepsWhatARecentRereadSettled pins the other
// side: a served name a reread finds gone is still kept, so a lagging listing cannot bring it back.
func TestARereadOfANameThatExistsNowhereCostsNoReload(t *testing.T) {
	store := secrets.NewLocal(policytest.Secret("OLD_KEY", "ada@example.com", policy.TierAgent, "v1"))
	count := &countingDescriber{DescribeSecretAPIClient: store}
	loader := policytest.Loader(store)
	loader.Describer = count
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if lk, err := cur.RefreshOne(context.Background(), "NO_SUCH_KEY"); err != nil || lk.Served || lk.Reason != policy.ReasonAbsent {
		t.Fatalf("RefreshOne(NO_SUCH_KEY) = %+v, %v; want absent", lk, err)
	}
	before := count.calls.Load()
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := count.calls.Load() - before; got != 0 {
		t.Fatalf("the reload after a reread of a name that exists nowhere made %d DescribeSecret calls; want 0", got)
	}
}

// countingDescriber counts the DescribeSecret calls a reread or a reload makes.
type countingDescriber struct {
	policy.DescribeSecretAPIClient
	calls atomic.Int64
}

func (c *countingDescriber) DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	c.calls.Add(1)
	return c.DescribeSecretAPIClient.DescribeSecret(ctx, in, opts...)
}

// heldCurrent loads an empty namespace's policy and starts a reload whose listing waits until the
// test ends, returning once that reload holds the writer lock: a reload stuck on a slow Secrets
// Manager.
func heldCurrent(t *testing.T) *policy.Current {
	t.Helper()
	store := secrets.NewLocal()
	page, err := store.ListSecrets(context.Background(), &secretsmanager.ListSecretsInput{})
	if err != nil {
		t.Fatal(err)
	}
	lister := &slowLister{page: page}
	loader := policytest.Loader(store)
	loader.Secrets = lister
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	listing, release := make(chan struct{}), make(chan struct{})
	lister.fetched = func() { close(listing); <-release }
	refreshed := make(chan error, 1)
	go func() { refreshed <- cur.Refresh(context.Background()) }()
	<-listing
	t.Cleanup(func() { close(release); <-refreshed })
	return cur
}

// rereadWithin runs RefreshOne(ctx, name) and answers its error, failing t when it has not
// returned within a second: long enough for any answer that waits on nothing, far shorter than a
// held reload.
func rereadWithin(t *testing.T, cur *policy.Current, ctx context.Context, name string) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := cur.RefreshOne(ctx, name)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		t.Fatalf("RefreshOne(%s) waited on the writer lock a reload holds", name)
		return nil
	}
}

// TestARereadOfANameNoSecretCanCarryWaitsOnNoReload pins that RefreshOne refuses such a name
// before it takes the writer lock: free text, or a name past Secrets Manager's name limit, is
// answered ErrNameInvalid at once while a reload holds the lock.
func TestARereadOfANameNoSecretCanCarryWaitsOnNoReload(t *testing.T) {
	cur := heldCurrent(t)
	for _, name := range []string{"not a secret name", "A" + strings.Repeat("B", 512-len(policytest.Prefix))} {
		if err := rereadWithin(t, cur, context.Background(), name); !errors.Is(err, policy.ErrNameInvalid) {
			t.Fatalf("RefreshOne(%.20s...) = %v, want ErrNameInvalid", name, err)
		}
	}
}
