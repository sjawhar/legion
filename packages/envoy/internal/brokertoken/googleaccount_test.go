// googleaccount_test.go drives the reader of this machine's own Google identity token against a
// stub metadata server: how long it reuses what it read, and what concurrent readers of a cold
// cache cost, which on a listener is the per-request authentication path (introspect.go).
package brokertoken_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/googleidentity"
)

// metadataAudience is the broker audience these reads ask the metadata server to mint for.
const metadataAudience = "https://broker.test"

// stubMetadata stands up a metadata server at the host GCE_METADATA_HOST names, answering
// handler, and returns how many requests it was asked.
func stubMetadata(t *testing.T, handler http.HandlerFunc) *atomic.Int64 {
	t.Helper()
	var asked atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	t.Setenv(googleidentity.MetadataHostVar, strings.TrimPrefix(server.URL, "http://"))
	return &asked
}

// TestGoogleAccountReadsAgainOnceItsCacheLapses: a token read once is answered to every later
// reader without reaching the metadata server while it is younger than TTL, and a reader past the
// TTL reads afresh.
func TestGoogleAccountReadsAgainOnceItsCacheLapses(t *testing.T) {
	var minted atomic.Int64
	asked := stubMetadata(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "token-%d\n", minted.Add(1))
	})
	account := brokertoken.NewGoogleAccount(metadataAudience)

	first, err := account.Read()
	if err != nil || first != "token-1" {
		t.Fatalf("the first read = %q, %v; want the stub's first token", first, err)
	}
	if cached, err := account.Read(); err != nil || cached != first {
		t.Fatalf("the read inside the TTL = %q, %v; want the cached %q", cached, err, first)
	}
	if got := asked.Load(); got != 1 {
		t.Fatalf("the metadata server was asked %d times inside the TTL, want 1", got)
	}

	account.TTL = 0
	if second, err := account.Read(); err != nil || second != "token-2" {
		t.Fatalf("the read past the TTL = %q, %v; want the stub's second token", second, err)
	}
	if got := asked.Load(); got != 2 {
		t.Errorf("the metadata server was asked %d times across a lapsed cache, want 2", got)
	}
}

// TestGoogleAccountSharesOneMetadataReadBetweenConcurrentReaders: readers that arrive while a read
// is in flight take that read's answer rather than each asking the metadata server in turn. The
// metadata server here refuses, which is the case the rule is for: nothing is cached on a failure,
// so before this each queued reader paid internal/googleidentity's whole timeout after the one
// before it, while the lock was held across the read. Every reader is answered the one refusal,
// which names the audience and no token, and the next read asks again because no failure is kept.
func TestGoogleAccountSharesOneMetadataReadBetweenConcurrentReaders(t *testing.T) {
	const readers = 8
	release := make(chan struct{})
	asked := stubMetadata(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	account := brokertoken.NewGoogleAccount(metadataAudience)

	var started, done sync.WaitGroup
	started.Add(readers)
	done.Add(readers)
	errs := make([]error, readers)
	for i := range readers {
		go func() {
			defer done.Done()
			started.Done()
			_, errs[i] = account.Read()
		}()
	}
	// The whole point is what the readers behind the first one do, so the stub is held until
	// every reader has been scheduled and given a moment to reach the read; nothing a reader
	// blocked inside the shared read is observable from here.
	started.Wait()
	time.Sleep(50 * time.Millisecond)
	close(release)
	done.Wait()

	for i := range readers {
		if errs[i] == nil || !strings.Contains(errs[i].Error(), metadataAudience) {
			t.Fatalf("reader %d = %v, want a refusal naming the audience", i, errs[i])
		}
	}
	if got := asked.Load(); got != 1 {
		t.Errorf("the metadata server was asked %d times for %d readers of a cold cache, want the one read they share", got, readers)
	}
	if _, err := account.Read(); err == nil {
		t.Fatal("the read after the shared one succeeded; a failure is never cached, so it asks again")
	}
	if got := asked.Load(); got != 2 {
		t.Errorf("the metadata server was asked %d times over the later read, want 2: no failure is cached", got)
	}
}
