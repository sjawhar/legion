package webhook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
)

// A request that cannot get room holds nothing the budget has not charged: its next buffer is
// allocated only once the charge succeeds, so requests waiting for room add nothing to memory.
func TestABodyReadWaitingForRoomAllocatesNothing(t *testing.T) {
	bodies := newBodyBudget(bodyReadStep)
	longest := bodies.begin()
	defer longest.release()
	if _, err := longest.grow(context.Background(), nil, bodyReadStep); err != nil {
		t.Fatalf("the longest holder's first buffer: %v", err)
	}
	waiting := bodies.begin()
	defer waiting.release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var err error
	allocations := testing.AllocsPerRun(10, func() {
		_, err = waiting.grow(ctx, nil, githubMaxBody)
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("grow with a full budget and an ended context = %v, want context.Canceled", err)
	}
	if allocations != 0 {
		t.Fatalf("a read that could not get room allocated %.0f times, want none", allocations)
	}
}

// GitHub delivers nothing over 25 MB, so the handler reads a body up to that size and refuses a
// larger one as too large, before verifying or parsing any of it, whether or not the request
// declares its length. The body returns its end together with its last bytes, as net/http's
// chunked reader does.
func TestGitHubHandlerReadsABodyUpToGitHubsPayloadCap(t *testing.T) {
	const secret = "s"
	for _, tc := range []struct {
		name       string
		size       int
		undeclared bool
		want       int
	}{
		// A body of exactly the cap is read whole and reaches the JSON decode, which refuses it.
		{name: "at the cap", size: 25 << 20, want: http.StatusBadRequest},
		{name: "one byte over the cap", size: 25<<20 + 1, want: http.StatusRequestEntityTooLarge},
		{name: "at the cap, length undeclared", size: 25 << 20, undeclared: true, want: http.StatusBadRequest},
		{name: "one byte over the cap, length undeclared", size: 25<<20 + 1, undeclared: true, want: http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := bytes.Repeat([]byte("x"), tc.size)
			pub := &mockPublisher{}
			recorder := &mockRecorder{}
			handler := GitHubHandler(secret, "@legion", "", pub, recorder)
			req := httptest.NewRequest(http.MethodPost, "/webhook/github", iotest.DataErrReader(bytes.NewReader(body)))
			req.ContentLength = int64(tc.size)
			if tc.undeclared {
				req.ContentLength = -1
			}
			req.Header.Set("X-GitHub-Delivery", "delivery-oversized")
			req.Header.Set("X-GitHub-Event", "push")
			req.Header.Set("X-Hub-Signature-256", githubSign(secret, body))
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", rr.Code, tc.want, rr.Body.String())
			}
			if tc.want == http.StatusBadRequest && !strings.Contains(rr.Body.String(), "invalid json") {
				t.Fatalf("body = %q, want the JSON decode's refusal: a body at the cap must be read whole", rr.Body.String())
			}
			if len(pub.published) != 0 || len(recorder.calls) != 0 || len(recorder.headCalls) != 0 {
				t.Fatalf("published %d, recorded %d checks and %d heads; want nothing", len(pub.published), len(recorder.calls), len(recorder.headCalls))
			}
		})
	}
}

// testBody is a request body of remaining bytes. It yields the first stallAfter bytes as fast as
// they are read and then waits for stall to close before failing, as a sender that stops sending
// does; a negative stallAfter never stalls. wait, when set, is closed before the first byte is
// yielded.
type testBody struct {
	remaining  int
	stallAfter int
	stall      <-chan struct{}
	wait       <-chan struct{}
	read       int
}

func (b *testBody) Read(p []byte) (int, error) {
	if b.read == 0 && b.wait != nil {
		<-b.wait
	}
	if b.stallAfter >= 0 && b.read >= b.stallAfter {
		<-b.stall
		return 0, io.ErrUnexpectedEOF
	}
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), b.remaining)
	if b.stallAfter >= 0 {
		n = min(n, b.stallAfter-b.read)
	}
	b.remaining -= n
	b.read += n
	return n, nil
}

// A body declared larger than GitHub delivers is refused before a byte of it is read.
func TestGitHubHandlerRefusesADeclaredOversizeBodyUnread(t *testing.T) {
	body := &testBody{remaining: githubMaxBody + 1, stallAfter: -1}
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", body)
	req.ContentLength = githubMaxBody + 1
	req.Header.Set("X-GitHub-Delivery", "delivery-declared-oversize")
	req.Header.Set("X-GitHub-Event", "push")
	rr := httptest.NewRecorder()

	GitHubHandler("s", "@legion", "", &mockPublisher{}, &mockRecorder{}).ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body = %s", rr.Code, rr.Body.String())
	}
	if body.read != 0 {
		t.Fatalf("read %d bytes of a body declared over the cap, want none", body.read)
	}
}

// The handler buffers a body whole before it can check the signature, so anyone who can reach the
// route can make it hold a body. The budget it reads bodies against holds at most githubBodyBudget
// plus what the one request that has held its place longest charges past it, at most twice one
// cap-sized buffer (bodyBudget); the rest wait for room and are then served. Here sixteen cap-sized
// bodies with a wrong signature arrive together, sent as fast as they are read, and each is
// answered 401.
func TestGitHubHandlerBoundsTheBodyBytesItHoldsInABurst(t *testing.T) {
	const requests = 16
	bound := int64(githubBodyBudget + 2*githubMaxBody)
	bodies := newBodyBudget(githubBodyBudget)
	handler := githubHandler("s", "@legion", "", &mockPublisher{}, &mockRecorder{}, bodies)
	var mu sync.Mutex
	entered := 0
	start := make(chan struct{})
	codes := make([]int, requests)
	var wg sync.WaitGroup
	for i := range requests {
		body := &testBody{remaining: githubMaxBody, stallAfter: -1, wait: start}
		req := httptest.NewRequest(http.MethodPost, "/webhook/github", body)
		req.ContentLength = githubMaxBody
		req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("delivery-budget-%d", i))
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-Hub-Signature-256", "sha256=0000")
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			entered++
			mu.Unlock()
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			codes[i] = rr.Code
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		ready := entered == requests
		mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d requests entered the handler", entered, requests)
		}
		time.Sleep(time.Millisecond)
	}
	// Let every request reach its body before any byte is sent.
	time.Sleep(20 * time.Millisecond)
	close(start)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the burst never drained: requests waited on each other's half-read bodies")
	}

	most := bodies.mostHeld()
	if most > bound {
		t.Fatalf("the budget held %d MiB at once, want at most %d MiB (a %d MiB budget plus twice one cap-sized buffer)", most>>20, bound>>20, githubBodyBudget>>20)
	}
	t.Logf("the budget held at most %d MiB", most>>20)
	for i, code := range codes {
		if code != http.StatusUnauthorized {
			t.Fatalf("request %d: status = %d, want 401 once its turn came", i, code)
		}
	}
}

// A sender without the secret can declare a cap-sized body and then send it a few bytes at a time,
// or send only its headers, holding its connection until the server's read timeout. It must not
// hold room for bytes it has not sent: GitHub gives a delivery 10 s, and one that waits behind such
// senders is lost. Here three trickling senders and one that sent only headers are mid-body when a
// signed 3.6 MB push arrives, and the push is served at once.
func TestGitHubHandlerServesADeliveryBehindSlowSenders(t *testing.T) {
	const secret = "s"
	handler := GitHubHandler(secret, "@legion", "", &mockPublisher{}, &mockRecorder{})
	stall := make(chan struct{})
	var mu sync.Mutex
	entered := 0
	var slow sync.WaitGroup
	for i, sent := range []int{4096, 4096, 4096, 0} {
		body := &testBody{remaining: githubMaxBody, stallAfter: sent, stall: stall}
		req := httptest.NewRequest(http.MethodPost, "/webhook/github", body)
		req.ContentLength = githubMaxBody
		req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("delivery-slow-%d", i))
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-Hub-Signature-256", "sha256=0000")
		slow.Add(1)
		go func() {
			defer slow.Done()
			mu.Lock()
			entered++
			mu.Unlock()
			handler.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	defer func() {
		close(stall)
		slow.Wait()
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		ready := entered == 4
		mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the slow senders never entered the handler")
		}
		time.Sleep(time.Millisecond)
	}
	// Let every slow sender reach its body, or the wait for room ahead of it.
	time.Sleep(20 * time.Millisecond)

	push := largePushPayload(t, 1000)
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(push))
	req.Header.Set("X-GitHub-Delivery", "delivery-behind-slow-senders")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", githubSign(secret, push))
	rr := httptest.NewRecorder()
	served := make(chan struct{})
	began := time.Now()
	go func() {
		handler.ServeHTTP(rr, req)
		close(served)
	}()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("a signed push behind three trickling senders and one that sent only headers was not served within 5 s; GitHub gives up at 10 s")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	t.Logf("served in %s", time.Since(began).Round(time.Millisecond))
}
