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
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"
)

// A request that cannot get room holds nothing the budget has not charged: its next buffer is
// allocated only once the charge succeeds, so requests waiting for room add nothing to memory.
func TestABodyReadWaitingForRoomAllocatesNothing(t *testing.T) {
	const step = 16 << 10
	bodies := newBodyBudget(step)
	// A read larger than the budget takes the one right to go past it, so the next read must wait.
	over := bodies.begin()
	defer over.release()
	if _, err := over.grow(context.Background(), nil, 2*step); err != nil {
		t.Fatalf("the first read's buffer: %v", err)
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
// plus what the one request allowed past it charges beyond it, at most twice one cap-sized buffer
// (bodyBudget); the rest wait for room and are then served. Here sixteen cap-sized
// bodies with a wrong signature arrive together, sent as fast as they are read, and each is
// answered 401.
func TestGitHubHandlerBoundsTheBodyBytesItHoldsInABurst(t *testing.T) {
	const requests = 16
	bound := int64(githubBodyBudget + 2*githubMaxBody)
	bodies := newBodyBudget(githubBodyBudget)
	handler := githubHandler("s", "@legion", "", &mockPublisher{}, &mockRecorder{}, bodies)
	start := make(chan struct{})
	senders := make([]*testBody, requests)
	for i := range senders {
		senders[i] = &testBody{remaining: githubMaxBody, stallAfter: -1, wait: start}
	}
	// Let every request reach its body before any byte is sent.
	codes, done := serveUnsigned(t, handler, senders, 20*time.Millisecond)
	close(start)
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
	var senders []*testBody
	for _, sent := range []int{4096, 4096, 4096, 0} {
		senders = append(senders, &testBody{remaining: githubMaxBody, stallAfter: sent, stall: stall})
	}
	// Let every slow sender reach its body, or the wait for room ahead of it.
	_, done := serveUnsigned(t, handler, senders, 20*time.Millisecond)
	defer func() {
		close(stall)
		<-done
	}()
	took := requireServedPushes(t, handler, secret, "three trickling senders and one that sent only headers", 1)
	t.Logf("served in %s", took.Round(time.Millisecond))
}

// Connections that send only headers, or one byte of body, cost a sender almost nothing, so there
// can be thousands of them, each held until the server's read timeout. They may hold room only for
// what they have sent: here enough of them to spend a whole budget at 16 KiB each, half with no
// body byte yet and half with one, are waiting when a signed 3.6 MB push arrives, and the push is
// served at once.
func TestGitHubHandlerServesADeliveryBehindManyHeaderOnlySenders(t *testing.T) {
	const (
		secret = "s"
		budget = 8 << 20
	)
	handler := githubHandler(secret, "@legion", "", &mockPublisher{}, &mockRecorder{}, newBodyBudget(budget))
	stall := make(chan struct{})
	senders := make([]*testBody, budget/(16<<10))
	for i := range senders {
		senders[i] = &testBody{remaining: githubMaxBody, stallAfter: i % 2, stall: stall}
	}
	// Let every sender reach its body.
	_, done := serveUnsigned(t, handler, senders, 50*time.Millisecond)
	defer func() {
		close(stall)
		<-done
	}()
	requireServedPushes(t, handler, secret, fmt.Sprintf("%d senders holding headers or one byte", len(senders)), 1)
}

// Pushes that together need more than the budget are served only because one of them at a time
// may go past it. A connection that has sent only its headers, or a byte and then nothing, and was
// there first must not keep that right from them, or the pushes wait for room until its read
// timeout. Here such a connection is waiting when four signed 3.6 MB pushes arrive together at a
// 2 MiB budget, so that each can be read only past it.
func TestGitHubHandlerServesPushesPastTheBudgetBehindAStalledConnection(t *testing.T) {
	const (
		secret = "s"
		budget = 2 << 20
	)
	for _, tc := range []struct {
		name string
		sent int
	}{
		{"only headers", 0},
		{"one byte", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := githubHandler(secret, "@legion", "", &mockPublisher{}, &mockRecorder{}, newBodyBudget(budget))
			stall := make(chan struct{})
			_, done := serveUnsigned(t, handler, []*testBody{{remaining: githubMaxBody, stallAfter: tc.sent, stall: stall}}, 20*time.Millisecond)
			defer func() {
				close(stall)
				<-done
			}()
			took := requireServedPushes(t, handler, secret, "a connection that sent "+tc.name, 4)
			t.Logf("served in %s", took.Round(time.Millisecond))
		})
	}
}

// The right to go past the budget goes to a request whose next piece does not fit, and its sender
// can stop sending right after taking it. It must not keep other requests from going past the
// budget once the total is back within it: GitHub gives a delivery 10 s, and one that waits for the
// stalled sender's read timeout is lost. Here three signed 3.6 MB pushes on a slow link have read
// into 2 MiB buffers, holding 6 MiB of an 8 MiB budget, when a sender sends 1 MiB and stops. Its
// full buffer grows to 2 MiB, which does not fit, so it takes the right, and giving back its old
// buffer leaves the total at the budget. The pushes then each need to grow past it, and all three
// are served.
func TestGitHubHandlerServesDeliveriesInProgressBehindASenderThatTookTheRightAndStopped(t *testing.T) {
	const (
		secret = "s"
		budget = 8 << 20
		paused = 3 << 19 // each push pauses after 1.5 MiB, having charged a 2 MiB buffer
		buffer = 2 << 20
	)
	bodies := newBodyBudget(budget)
	handler := githubHandler(secret, "@legion", "", &mockPublisher{}, &mockRecorder{}, bodies)
	push := largePushPayload(t, 1000)
	resume := make(chan struct{})
	resumePushes := sync.OnceFunc(func() { close(resume) })
	defer resumePushes()
	var answered sync.WaitGroup
	recorders := make([]*httptest.ResponseRecorder, 3)
	for i := range recorders {
		recorders[i] = startPush(handler, secret, push, &pausedBody{data: push, pauseAt: paused, resume: resume}, i, &answered)
		// One at a time, so that no push takes the right while the others' buffers grow.
		awaitBudget(t, bodies, int64(i+1)*buffer, false)
	}
	stall := make(chan struct{})
	_, done := serveUnsigned(t, handler, []*testBody{{remaining: githubMaxBody, stallAfter: 1 << 20, stall: stall}}, 0)
	defer func() {
		close(stall)
		<-done
	}()
	awaitBudget(t, bodies, budget, true)
	began := time.Now()
	resumePushes()
	took := requireServed(t, recorders, &answered, began, "3 signed pushes in progress behind a sender that took the right and stopped")
	t.Logf("served in %s", took.Round(time.Millisecond))
}

// A request asks for room as soon as its buffer is full, before its next byte arrives, so a sender
// that stops right after filling its buffer takes the right to go past the budget, when its growth
// does not fit, for bytes it never sends. Once the requests that filled the budget finish, it must
// not keep a delivery from going past the budget until its read timeout. Here the other reads hold
// all but a few hundred bytes of a 2 MiB budget when a sender sends its bytes and stops, holding
// 1 KiB and the right. One of the other reads then finishes, leaving 1 MiB held, and a signed
// 3.6 MB push, which can be read only past the budget, is served.
func TestGitHubHandlerServesADeliveryBehindASenderThatTookTheRightAtABufferBoundary(t *testing.T) {
	const (
		secret = "s"
		budget = 2 << 20
	)
	for _, tc := range []struct {
		name string
		free int64 // what the other reads leave of the budget
		sent int
	}{
		// Its first read, 512 bytes, fits; the 1 KiB buffer it then needs for the next 88 does not.
		{"600 bytes with 700 free", 700, 600},
		// It stops exactly on its first buffer and asks for a 1 KiB one it never fills.
		{"512 bytes with 1000 free", 1000, 512},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			bodies := newBodyBudget(budget)
			handler := githubHandler(secret, "@legion", "", &mockPublisher{}, &mockRecorder{}, bodies)
			staying, finishing := bodies.begin(), bodies.begin()
			defer staying.release()
			if err := staying.charge(ctx, budget/2); err != nil {
				t.Fatalf("the read that stays: %v", err)
			}
			if err := finishing.charge(ctx, budget/2-tc.free); err != nil {
				t.Fatalf("the read that finishes: %v", err)
			}
			stall := make(chan struct{})
			_, done := serveUnsigned(t, handler, []*testBody{{remaining: githubMaxBody, stallAfter: tc.sent, stall: stall}}, 0)
			defer func() {
				close(stall)
				<-done
			}()
			awaitBudget(t, bodies, budget-tc.free+1<<10, true)
			finishing.release()
			took := requireServedPushes(t, handler, secret, "a sender that took the right after "+tc.name+" and stopped", 1)
			t.Logf("served in %s", took.Round(time.Millisecond))
		})
	}
}

// awaitBudget waits until bodies holds held bytes with the right to go past its limit taken, or
// free, as taken says, and fails the test if that takes longer than 5 s.
func awaitBudget(t *testing.T, bodies *bodyBudget, held int64, taken bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		bodies.mu.Lock()
		nowHeld, nowTaken := bodies.held, bodies.over != nil
		bodies.mu.Unlock()
		if nowHeld == held && nowTaken == taken {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the budget holds %d bytes with the right taken %t, want %d bytes and %t", nowHeld, nowTaken, held, taken)
		}
	}
}

// pausedBody is a request body of data. It yields its first pauseAt bytes as fast as they are read
// and the rest once resume is closed, as a delivery on a slow link does.
type pausedBody struct {
	data    []byte
	pauseAt int
	resume  <-chan struct{}
	read    int
}

func (b *pausedBody) Read(p []byte) (int, error) {
	if b.read == b.pauseAt {
		<-b.resume
	}
	if b.read == len(b.data) {
		return 0, io.EOF
	}
	end := len(b.data)
	if b.read < b.pauseAt {
		end = b.pauseAt
	}
	n := copy(p, b.data[b.read:end])
	b.read += n
	return n, nil
}

// serveUnsigned starts handler on one push delivery with a wrong signature per body, each declaring
// githubMaxBody, waits until every one has entered the handler, and then gives them settle to reach
// their bodies. done is closed once every one has been answered, and codes then holds each status.
func serveUnsigned(t *testing.T, handler http.Handler, bodies []*testBody, settle time.Duration) (codes []int, done <-chan struct{}) {
	t.Helper()
	codes = make([]int, len(bodies))
	var entered atomic.Int64
	var wg sync.WaitGroup
	for i, body := range bodies {
		req := httptest.NewRequest(http.MethodPost, "/webhook/github", body)
		req.ContentLength = githubMaxBody
		req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("delivery-unsigned-%d", i))
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-Hub-Signature-256", "sha256=0000")
		wg.Add(1)
		go func() {
			defer wg.Done()
			entered.Add(1)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			codes[i] = rr.Code
		}()
	}
	for deadline := time.Now().Add(10 * time.Second); entered.Load() != int64(len(bodies)); {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d requests entered the handler", entered.Load(), len(bodies))
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(settle)
	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()
	return codes, finished
}

// requireServedPushes sends handler n signed 3.6 MB pushes at once and fails the test unless every
// one is answered 200 within 5 s, well inside the 10 s GitHub gives a delivery. It returns how
// long the slowest took.
func requireServedPushes(t *testing.T, handler http.Handler, secret, behind string, n int) time.Duration {
	t.Helper()
	push := largePushPayload(t, 1000)
	var answered sync.WaitGroup
	began := time.Now()
	recorders := make([]*httptest.ResponseRecorder, n)
	for i := range recorders {
		recorders[i] = startPush(handler, secret, push, bytes.NewReader(push), i, &answered)
	}
	return requireServed(t, recorders, &answered, began, fmt.Sprintf("%d signed pushes behind %s", n, behind))
}

// startPush starts handler on signed push delivery i, whose body is push read from body, adds it
// to answered, and returns the recorder that holds its answer once answered is done.
func startPush(handler http.Handler, secret string, push []byte, body io.Reader, i int, answered *sync.WaitGroup) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", body)
	req.ContentLength = int64(len(push))
	req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("delivery-behind-held-bodies-%d", i))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", githubSign(secret, push))
	rr := httptest.NewRecorder()
	answered.Add(1)
	go func() {
		defer answered.Done()
		handler.ServeHTTP(rr, req)
	}()
	return rr
}

// requireServed fails the test unless every delivery in answered has been answered 200 within 5 s
// of began, well inside the 10 s GitHub gives a delivery. It returns how long the slowest took.
func requireServed(t *testing.T, recorders []*httptest.ResponseRecorder, answered *sync.WaitGroup, began time.Time, what string) time.Duration {
	t.Helper()
	served := make(chan struct{})
	go func() {
		answered.Wait()
		close(served)
	}()
	select {
	case <-served:
	case <-time.After(5*time.Second - time.Since(began)):
		t.Fatalf("%s were not all served within 5 s; GitHub gives up at 10 s", what)
	}
	for i, rr := range recorders {
		if rr.Code != http.StatusOK {
			t.Fatalf("push %d: status = %d, want 200; body = %s", i, rr.Code, rr.Body.String())
		}
	}
	return time.Since(began)
}
