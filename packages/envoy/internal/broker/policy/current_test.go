package policy_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// awsListLag is the AWS store's ListLag, which every Local-backed Current here runs on.
var awsListLag = policytest.Store(secrets.NewLocal()).ListLag()

// staleLister freezes ListSecrets at construction while DescribeSecret stays live: AWS's
// documented ListSecrets lag, reproduced.
type staleLister struct {
	*secrets.Local
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
	loader.Store = policytest.StoreThrough(store, staleLister{Local: store, page: stale}) // the listing predates everything below
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
	loader.Store = policytest.StoreThrough(store, staleLister{Local: store, page: stale}) // never lists NEW_KEY
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

	now = reread.Add(awsListLag - time.Second)
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := cur.Get().Secrets["NEW_KEY"]; !ok {
		t.Fatal("a full reload inside listLag of the reread dropped NEW_KEY")
	}

	now = reread.Add(awsListLag + time.Second)
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := cur.Get().Secrets["NEW_KEY"]; ok {
		t.Fatal("a full reload listLag after the reread still kept NEW_KEY over the listing")
	}
}

// slowStore is the AWS store with a longer documented lag, as Secret Manager's six hours.
type slowStore struct{ secrets.AWS }

func (slowStore) ListLag() time.Duration { return 6 * time.Hour }

// TestAnchorsLastTheStoresOwnLag pins that the window is the store's: on a store whose metadata
// may lag a write for hours, a reread that stopped serving a secret still holds past AWS's five
// minutes against a listing that has not caught up, so a stale listing cannot reinstate it.
func TestAnchorsLastTheStoresOwnLag(t *testing.T) {
	store := secrets.NewLocal(policytest.Secret("OLD_KEY", "ada@example.com", policy.TierAgent, "v1"))
	stale, err := store.ListSecrets(context.Background(), &secretsmanager.ListSecretsInput{})
	if err != nil {
		t.Fatal(err)
	}
	loader := policytest.Loader(store)
	loader.Store = slowStore{policytest.StoreThrough(store, staleLister{Local: store, page: stale})} // still lists OLD_KEY as agent tier
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reread := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	now := reread
	policy.SetNow(cur, func() time.Time { return now })
	store.Put(policytest.Secret("OLD_KEY", "ada@example.com", policy.TierHuman, "v1"))
	if lk, _ := cur.RefreshOne(context.Background(), "OLD_KEY"); lk.Secret.Tier != policy.TierHuman {
		t.Fatalf("reread = %+v; want OLD_KEY human tier", lk)
	}
	now = reread.Add(time.Hour)
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := cur.Get().Secrets["OLD_KEY"].Tier; got != policy.TierHuman {
		t.Fatalf("a reload an hour after the reread, inside the store's lag, served OLD_KEY as %s from the stale listing", got)
	}
}

// slowLister answers a frozen page, then runs fetched: a reload whose listing is fetched at one
// time and whose Load returns at a later one.
type slowLister struct {
	*secrets.Local
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
	lister := &slowLister{Local: store, page: stale} // lists OLD_KEY and never NEW_KEY
	loader := policytest.Loader(store)
	loader.Store = policytest.StoreThrough(store, lister)
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
	now = reread.Add(awsListLag - time.Second)
	lister.fetched = func() { now = reread.Add(awsListLag + time.Second) }
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
// a reread changed its answer, with no reread of its own, is dropped by the next reload's
// re-describe, and a reload listLag after the reread, from a listing that still predates the
// delete, keeps it out rather than serving it again.
func TestAReloadThatCatchesAChangeKeepsItForListLag(t *testing.T) {
	store := secrets.NewLocal(policytest.Secret("OLD_KEY", "ada@example.com", policy.TierAgent, "v1"))
	stale, err := store.ListSecrets(context.Background(), &secretsmanager.ListSecretsInput{})
	if err != nil {
		t.Fatal(err)
	}
	lister := &slowLister{Local: store, page: stale} // lists OLD_KEY, agent tier, throughout
	loader := policytest.Loader(store)
	loader.Store = policytest.StoreThrough(store, lister)
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reread := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	now := reread
	policy.SetNow(cur, func() time.Time { return now })
	store.Put(policytest.Secret("OLD_KEY", "ada@example.com", policy.TierHuman, "v1"))
	if lk, _ := cur.RefreshOne(context.Background(), "OLD_KEY"); lk.Secret.Tier != policy.TierHuman {
		t.Fatalf("reread = %+v; want OLD_KEY served at human tier", lk)
	}

	// Deleted (from the console, say) while the next reload lists: its listing still shows OLD_KEY,
	// and its own re-describe finds it gone.
	now = reread.Add(awsListLag / 2)
	lister.fetched = func() { store.Delete(policytest.ID("OLD_KEY")) }
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := cur.Get().Secrets["OLD_KEY"]; ok {
		t.Fatal("the reload whose own re-describe found OLD_KEY deleted still served it")
	}

	lister.fetched = nil
	now = reread.Add(awsListLag + time.Second)
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
	count := &policytest.CountingDescriber{SecretsManagerAPI: store}
	loader := policytest.Loader(store)
	loader.Store = policytest.StoreThrough(store, count)
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if lk, err := cur.RefreshOne(context.Background(), "NO_SUCH_KEY"); err != nil || lk.Served || lk.Reason != policy.ReasonAbsent {
		t.Fatalf("RefreshOne(NO_SUCH_KEY) = %+v, %v; want absent", lk, err)
	}
	before := count.Calls.Load()
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := count.Calls.Load() - before; got != 0 {
		t.Fatalf("the reload after a reread of a name that exists nowhere made %d DescribeSecret calls; want 0", got)
	}
}

// TestARereadThatConfirmsTheLiveAnswerCostsNoReload pins that a reread finding a served secret as
// the live policy already serves it records nothing: anyone may reread an existing name, and on a
// store whose ListLag is hours each anchor would cost every reload in it a describe.
func TestARereadThatConfirmsTheLiveAnswerCostsNoReload(t *testing.T) {
	store := secrets.NewLocal(policytest.Secret("OLD_KEY", "ada@example.com", policy.TierAgent, "v1"))
	count := &policytest.CountingDescriber{SecretsManagerAPI: store}
	loader := policytest.Loader(store)
	loader.Store = policytest.StoreThrough(store, count)
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if lk, err := cur.RefreshOne(context.Background(), "OLD_KEY"); err != nil || !lk.Served {
			t.Fatalf("RefreshOne(OLD_KEY) = %+v, %v; want served", lk, err)
		}
	}
	before := count.Calls.Load()
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := count.Calls.Load() - before; got != 0 {
		t.Fatalf("the reload after rereads that confirmed the live answer made %d DescribeSecret calls; want 0", got)
	}
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
	lister := &slowLister{Local: store, page: page}
	loader := policytest.Loader(store)
	loader.Store = policytest.StoreThrough(store, lister)
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

// TestARereadWaitsForTheWriterLockOnlyWhileItsCallerDoes pins that RefreshOne's wait for the lock
// a reload holds ends with its caller's context: one already done returns its error without
// waiting, and one whose deadline passes while it waits returns then.
func TestARereadWaitsForTheWriterLockOnlyWhileItsCallerDoes(t *testing.T) {
	cur := heldCurrent(t)
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rereadWithin(t, cur, done, "NEW_KEY"); !errors.Is(err, context.Canceled) {
		t.Fatalf("RefreshOne with its context done = %v, want context.Canceled", err)
	}
	expiring, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := rereadWithin(t, cur, expiring, "NEW_KEY"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RefreshOne whose deadline passed while it waited = %v, want context.DeadlineExceeded", err)
	}
}

// hangingLister answers page until hang; from then on it holds each call until unhang releases
// every held call with page, or until the call's context ends and it answers that context's error,
// as the SDK does: a Secrets Manager that stops answering, then answers again. heldCalls counts the
// calls it has held. hang and unhang pair: a repeated hang keeps holding, and an unhang with
// nothing held does nothing.
type hangingLister struct {
	*secrets.Local
	page      *secretsmanager.ListSecretsOutput
	heldCalls atomic.Int64
	mu        sync.Mutex
	held      chan struct{} // closed by unhang; nil while it answers
}

func (h *hangingLister) hang() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.held == nil {
		h.held = make(chan struct{})
	}
}

func (h *hangingLister) unhang() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.held != nil {
		close(h.held)
		h.held = nil
	}
}

func (h *hangingLister) ListSecrets(ctx context.Context, _ *secretsmanager.ListSecretsInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	h.mu.Lock()
	held := h.held
	h.mu.Unlock()
	if held != nil {
		h.heldCalls.Add(1)
		select {
		case <-held:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return h.page, nil
}

// hangingLoader is the loader of an empty namespace whose listing lister answers.
func hangingLoader(t *testing.T) (*hangingLister, policy.Loader) {
	t.Helper()
	store := secrets.NewLocal()
	page, err := store.ListSecrets(context.Background(), &secretsmanager.ListSecretsInput{})
	if err != nil {
		t.Fatal(err)
	}
	lister := &hangingLister{Local: store, page: page}
	loader := policytest.Loader(store)
	loader.Store = policytest.StoreThrough(store, lister)
	return lister, loader
}

// TestAReloadGivesUpWithinItsInterval pins that a periodic reload whose Secrets Manager stops
// answering fails once its interval has passed, logging LoadFailedMessage, rather than holding the
// writer lock every reread waits on for as long as the process lives. It runs in a synctest
// bubble, which waits for the reload goroutine to return, so none of its lines reach a later
// test's capture.
func TestAReloadGivesUpWithinItsInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lister, loader := hangingLoader(t)
		logged := policytest.CaptureLog(t)
		cur, err := policy.NewCurrent(t.Context(), loader, 50*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		lister.hang()
		const failed = "ERROR " + policy.LoadFailedMessage + " error=\"list secrets under " + policytest.Prefix + ": context deadline exceeded\"\n"
		await(t, func() bool { return strings.Contains(logged.String(), failed) })
		lister.unhang()
		if err := rereadWithin(t, cur, context.Background(), "NEW_KEY"); err != nil {
			t.Fatalf("RefreshOne after the hung reload gave up = %v", err)
		}
	})
}

// TestAReloadItsShutdownEndsLogsNothing pins that a reload cut short because NewCurrent's context
// ended, the broker shutting down, is no failed load: the deployment's alarm counts
// LoadFailedMessage, so a reload in flight when that context ends logs nothing.
func TestAReloadItsShutdownEndsLogsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lister, loader := hangingLoader(t)
		logged := policytest.CaptureLog(t)
		ctx, shutdown := context.WithCancel(t.Context())
		if _, err := policy.NewCurrent(ctx, loader, time.Minute); err != nil {
			t.Fatal(err)
		}
		lister.hang()
		time.Sleep(time.Minute + time.Second)
		synctest.Wait()
		if held := lister.heldCalls.Load(); held != 1 {
			t.Fatalf("%d reloads held on the listing a minute in, want the first", held)
		}
		shutdown()
		synctest.Wait() // the reload has returned, and its goroutine with it
		if logged.String() != "" {
			t.Fatalf("a reload the shutdown ended logged:\n%s", logged)
		}
	})
}
