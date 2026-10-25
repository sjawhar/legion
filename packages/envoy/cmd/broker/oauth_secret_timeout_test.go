package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// TestOAuthProviderSecretUnderATwoPodBootIsFarInsideItsTimeout measures the headroom
// oauthSecretTimeout has, rather than pinning the call's duration: a row asserting that a call
// takes about so long is a flake waiting for a slow runner, so what is asserted here is the
// margin — that the slower of two contending calls is comfortably inside the bound — and the
// measurements go in the log, where a reader can see the figure the bound was chosen against.
//
// The contention is the real one. A cold two-pod boot runs oauthstore.Store.Secret on both pods
// at the same moment, and the insert ... on conflict do nothing on the one-row
// oauth_provider_secret table makes one of them wait on the other's row lock; that is the only
// contention this call ever meets, and the whole reason it has a bound of its own. Two
// goroutines released together against a migrated schema of this test's own reproduce it, driving
// the production store rather than a statement of the test's making.
//
// The expectation is single-digit milliseconds of contention against a ten-second bound
// (oauthSecretTimeout's own comment carries that measured range). What this row measures is
// larger and mostly not the contention: each goroutine's first call also opens its pooled
// connection, so it reads 14.7 ms and 6.3 ms in this sandbox where the contention alone, with
// both connections already open, is 0.7-1.6 ms. Either figure is three orders of magnitude
// inside the bound, and the larger one is the honest thing to compare against, since a cold
// boot pays the connection too. So a quarter of the bound is a margin no scheduling hiccup
// or loaded runner closes: this row goes red when the margin collapses by two orders of
// magnitude, and for nothing else.
func TestOAuthProviderSecretUnderATwoPodBootIsFarInsideItsTimeout(t *testing.T) {
	const pods = 2
	oauthStore := oauthstore.New(storetest.Open(t).Pool)

	var (
		wg      sync.WaitGroup
		release = make(chan struct{})
		took    [pods]time.Duration
		errs    [pods]error
	)
	wg.Add(pods)
	for pod := range pods {
		go func() {
			defer wg.Done()
			<-release
			began := time.Now()
			_, err := oauthStore.Secret(context.Background())
			took[pod], errs[pod] = time.Since(began), err
		}()
	}
	close(release)
	wg.Wait()

	slower := took[0]
	for pod := range pods {
		if errs[pod] != nil {
			t.Fatalf("pod %d's boot read of the OAuth provider secret, with %d pods reading it at once: %v", pod, pods, errs[pod])
		}
		slower = max(slower, took[pod])
	}
	t.Logf("%d pods read the OAuth provider secret at the same moment: %s and %s, against oauthSecretTimeout of %s", pods, took[0], took[1], oauthSecretTimeout)
	if margin := oauthSecretTimeout / 4; slower >= margin {
		t.Fatalf("the slower of %d simultaneous boot reads of the OAuth provider secret took %s, which is no longer inside %s (a quarter of oauthSecretTimeout, %s); the bound is sized for single-digit milliseconds, so a call this close to it means the contention has changed rather than the runner being slow",
			pods, slower, margin, oauthSecretTimeout)
	}
}
