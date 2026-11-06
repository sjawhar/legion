package githubapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

// heldLookupTransport answers the client's GitHub calls in memory from a limitGitHub, holding
// each repository installation lookup until release closes or the lookup's own context ends, as
// a real transport gives up on a request whose caller went away. Every lookup it holds is
// announced on held.
type heldLookupTransport struct {
	fake    *limitGitHub
	held    chan struct{}
	release chan struct{}
}

func (h *heldLookupTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodGet && r.URL.Path == "/repos/acme/web/installation" {
		h.held <- struct{}{}
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-h.release:
		}
	}
	recorder := httptest.NewRecorder()
	h.fake.serve(recorder, r)
	return recorder.Result(), nil
}

// Two requests for one repository's token arrive together, as the GitHub proxy serves two
// browser reads of one repository. The first caller goes away (its browser tab closed) while
// GitHub is still answering the installation lookup. The second caller, still waiting, gets its
// token: one caller's cancellation is never another caller's failure.
func TestOneCallersCancellationDoesNotFailAnotherCallersRepositoryToken(t *testing.T) {
	_, pemText := testKeyPEM(t)
	synctest.Test(t, func(t *testing.T) {
		fake := &limitGitHub{installationID: 7}
		client, err := New(&auth.AppConfig{ClientID: "Iv1.cancel", PEM: pemText}, "https://github.test")
		if err != nil || client == nil {
			t.Fatalf("new client: %v", err)
		}
		transport := &heldLookupTransport{fake: fake, held: make(chan struct{}, 4), release: make(chan struct{})}
		client.http = &http.Client{Transport: transport}

		firstCtx, cancelFirst := context.WithCancel(context.Background())
		defer cancelFirst()
		var callers sync.WaitGroup
		var secondErr error
		var secondToken string
		// The first caller's own answer is not the contract here: once cancelled, it may fail.
		callers.Go(func() { _, _ = client.RepositoryToken(firstCtx, "acme", "web") })
		<-transport.held
		callers.Go(func() { secondToken, secondErr = client.RepositoryToken(context.Background(), "acme", "web") })
		// Both callers are now waiting on GitHub's answer to the lookup.
		synctest.Wait()

		cancelFirst()
		synctest.Wait()
		close(transport.release)
		callers.Wait()

		if secondErr != nil || secondToken == "" {
			t.Fatalf("the second caller's RepositoryToken(acme/web) = (%q, %v), want its token: the first caller's "+
				"context was cancelled mid-lookup and failed the second caller's live request too, because the "+
				"shared installation lookup runs on the first caller's context", secondToken, secondErr)
		}
	})
}
