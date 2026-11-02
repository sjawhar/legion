package bus_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// deniedRequests is how many Requests the test below has in flight on one window at once, each to
// a denied subject of its own. The outbox forwards one role route per event and sends a whole
// batch through one window, so a grant that withholds several holders' subjects has this shape.
const deniedRequests = 8

// Every Request of one window the server denied is answered ErrPublishDenied naming the subject it
// sent, within its own timeout.
//
// The flush cannot name them all: nats.go keeps one last error per connection, so a violation of
// another subject recorded between a request's publish and its flush's read leaves that flush
// unable to confirm the publish (confirmPublished), and every request but the one the connection
// kept the violation of ended as a send nobody acknowledged, carrying another subject's violation
// - which its caller reports as a bus that did not answer rather than a grant that refused it.
// The request reads the violation the connection recorded under its own subject instead
// (deniedPublish), on the goroutine that waits for its answer: when the flush comes back unable
// to confirm the publish, and once more before the timeout is answered, since the connection
// records the violation on nats.go's error callback, which often runs after the flush returns.
// So a denial found at the first read answers at once, and one found at the second answers when
// the request's timeout runs out, each naming the subject it sent.
func TestEveryDeniedRequestOfOneWindowNamesItsOwnSubject(t *testing.T) {
	const timeout = 3 * time.Second
	client, _ := grantedClient(t)
	window := client.WindowUntil(nil)

	type answered struct {
		subject string
		err     error
		elapsed time.Duration
	}
	answers := make(chan answered, deniedRequests)
	var wg sync.WaitGroup
	for index := range deniedRequests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			subject := fmt.Sprintf("%s-%d", deniedRole, index)
			started := time.Now()
			err := <-window.Request(subject, roleEnvelope(subject), bus.RequestOptions{Timeout: timeout})
			answers <- answered{subject: subject, err: err, elapsed: time.Since(started)}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("only %d of %d denied Requests were answered", len(answers), deniedRequests)
	}
	close(answers)

	for got := range answers {
		if !errors.Is(got.err, bus.ErrPublishDenied) || !errors.Is(got.err, natsgo.ErrPermissionViolation) {
			t.Errorf("the Request to %s answered %v, want ErrPublishDenied wrapping the permissions violation", got.subject, got.err)
			continue
		}
		if errors.Is(got.err, bus.ErrNotAcknowledged) {
			t.Errorf("the Request to %s answered %v, which reads as a send nobody acknowledged", got.subject, got.err)
		}
		if !strings.Contains(got.err.Error(), fmt.Sprintf("Publish to %q", got.subject)) {
			t.Errorf("the Request to %s answered %v, which carries another subject's violation", got.subject, got.err)
		}
		if got.elapsed > timeout+2*time.Second {
			t.Errorf("the Request to %s was answered after %s, past its %s timeout", got.subject, got.elapsed, timeout)
		}
	}
}
