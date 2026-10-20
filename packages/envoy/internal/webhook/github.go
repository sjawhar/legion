package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/verify"
)

// githubEvent returns true for event types where sender logging and bot filtering apply.
func githubEvent(event string) bool {
	switch event {
	case "issue_comment", "pull_request_review_comment", "pull_request_review":
		return true
	}
	return false
}

// githubSkip returns true for event types the handler acknowledges with 200 and neither
// publishes nor records. merge_group is GitHub's merge queue building (`checks_requested`) or
// tearing down (`destroyed`) a temporary merge commit: it names no pull request head, and
// nothing in Legion routes on it (LEGION-99). It runs after signature verification.
func githubSkip(event string) bool {
	return event == "merge_group"
}

// githubSenderField extracts a string field from the sender map.
func githubSenderField(payload map[string]any, field string) string {
	sender, ok := payload["sender"].(map[string]any)
	if !ok {
		return fmt.Sprintf("<no sender map: %T>", payload["sender"])
	}
	v, ok := sender[field]
	if !ok {
		return fmt.Sprintf("<missing %s>", field)
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("<non-string %s: %T=%v>", field, v, v)
	}
	return s
}

// CIRecorder folds check-run and check-suite observations into CI state.
type CIRecorder interface {
	Record(contracts.CIObservation) error
	RecordSuite(contracts.CIObservation) error
}

func reviewerVerdict(name string) bool {
	return name == "tester" || name == "architect"
}

// githubMaxBody is the largest webhook body the handler reads: GitHub's documented payload cap,
// 25 MB, read as MiB so no delivery GitHub sends is refused. A push of a thousand commits is
// several megabytes, and a refused delivery is lost for good, since a redelivery is the same body.
const githubMaxBody = 25 << 20

// githubBodyBudget bounds the webhook body bytes the handler holds at once, across all requests
// (bodyBudget). The body is buffered whole before its signature can be checked, so without it
// anyone who can reach the route could make the listener hold 25 MiB per connection. A request
// charges its buffer as its body arrives and holds it until the handler returns; one whose next
// piece does not fit waits, except the one request at a time allowed past the limit. A signed body
// decodes to about two and a half times its size again. It is at least githubMaxBody, so any body
// GitHub sends fits.
const githubBodyBudget = 64 << 20

// githubBodyDeadline bounds how long, from the handler's start, the route reads a delivery's body,
// and githubHandleDeadline how long after that the handler has to verify, record, publish and
// answer. The listener's server gives every route 10 s from the request's start to read it, and
// 10 s from the end of its header to answer it, which a body of several megabytes sent through a
// proxy can need more than: the read fails, its refusal cannot be written, and the delivery is
// lost. At these bounds a body read in 60 s is kept: 10 MB at about 170 KB/s, the whole 25 MB cap
// at about 440 KB/s.
//
// The bounds are absolute, so they also cap what a sender that stalls can hold: its one connection
// for up to about 70 s (the server's 10 s for the header, then githubBodyDeadline), at most twice
// the bytes it has sent (bodyBudget) for githubBodyDeadline, and, when it holds the budget's right
// to go past the limit, the other deliveries waiting for room for that long. A delivery waiting for
// room reads its body under the same deadline, so its wait ends there too and it answers 400.
// They are variables so a test can shorten them.
var (
	githubBodyDeadline   = 60 * time.Second
	githubHandleDeadline = 15 * time.Second
)

// GitHubHandler returns the HTTP handler for GitHub webhook events.
func GitHubHandler(secret, mentionTrigger, reviewerAppID string, publisher Publisher, ci CIRecorder) http.HandlerFunc {
	return githubHandler(secret, mentionTrigger, reviewerAppID, publisher, ci, newBodyBudget(githubBodyBudget))
}

// githubHandler is GitHubHandler reading bodies against the given budget.
func githubHandler(secret, mentionTrigger, reviewerAppID string, publisher Publisher, ci CIRecorder, bodies *bodyBudget) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}
		start := time.Now()
		extendDeadlines(w, start)
		read := bodies.begin()
		defer read.release()
		counted := &countingReader{r: r.Body}
		// The connection's read deadline ends a read that is waiting on the sender, and this one a
		// wait for room in the budget, which reads nothing from the connection.
		ctx, cancel := context.WithDeadline(r.Context(), start.Add(githubBodyDeadline))
		defer cancel()
		body, err := read.readAll(ctx, counted, r.ContentLength, githubMaxBody)
		if err != nil {
			log.Printf("github body read failed delivery=%s event=%s content_length=%d read=%d elapsed=%s: %v",
				r.Header.Get("X-GitHub-Delivery"), r.Header.Get("X-GitHub-Event"), r.ContentLength,
				counted.n, time.Since(start).Round(time.Millisecond), err)
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		delivery := r.Header.Get("X-GitHub-Delivery")
		event := r.Header.Get("X-GitHub-Event")
		signature := r.Header.Get("X-Hub-Signature-256")
		if delivery == "" || event == "" {
			http.Error(w, "missing github headers", http.StatusBadRequest)
			return
		}
		if !verify.Github(secret, body, signature) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if githubEvent(event) {
			log.Printf("github sender login=%s type=%s delivery=%s event=%s",
				githubSenderField(payload, "login"),
				githubSenderField(payload, "type"),
				delivery, event)
		}
		if githubSkip(event) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		// A pull request's CI events only update durable state. The summary loop publishes one
		// settled checks envelope for each commit whose suites and check runs are done. A finished
		// Actions job on the default branch, of a run with no pull request, records nothing and is
		// published below.
		if obs := contracts.GithubCIObservations(event, payload); len(obs) > 0 {
			for _, o := range obs {
				if o.CheckName == "" {
					if err := ci.RecordSuite(o); err != nil {
						deliveryFailed(w, "github ci suite record", err)
						return
					}
					continue
				}
				if reviewerVerdict(o.CheckName) {
					if reviewerAppID == "" {
						log.Printf("WARN github ci verdict dropped: ENVOY_REVIEWER_APP_ID is not configured check=%q app_id=%q pr=%s", o.CheckName, o.AppID, o.Number)
						continue
					}
					if o.AppID != reviewerAppID {
						log.Printf("WARN github ci verdict ignored: untrusted publisher check=%q app_id=%q expected_app_id=%q pr=%s", o.CheckName, o.AppID, reviewerAppID, o.Number)
						continue
					}
				}
				if err := ci.Record(o); err != nil {
					deliveryFailed(w, "github ci record", err)
					return
				}
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		trigger := mentionTrigger
		if trigger == "" {
			trigger = "@legion"
		}
		items := contracts.GithubEnvelopes(contracts.GithubEnvelopeInput{
			Event:    event,
			Delivery: delivery,
			Body:     payload,
			EventID:  id.New(),
			TraceID:  id.New(),
		}, trigger)
		for _, item := range items {
			if err := item.Validate(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := publisher.Publish(item); err != nil {
				deliveryFailed(w, "github publish", err)
				return
			}
		}
		if len(items) > 1 {
			log.Printf("github mention detected delivery=%s trigger=%s", delivery, strings.TrimSpace(trigger))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}

// extendDeadlines moves this request's connection deadlines from the server's to the route's own,
// counted from start. A ResponseWriter that has no connection (a test's recorder) has none to move.
func extendDeadlines(w http.ResponseWriter, start time.Time) {
	rc := http.NewResponseController(w)
	for _, set := range []struct {
		name string
		fn   func(time.Time) error
		at   time.Time
	}{
		{"read", rc.SetReadDeadline, start.Add(githubBodyDeadline)},
		{"write", rc.SetWriteDeadline, start.Add(githubBodyDeadline + githubHandleDeadline)},
	} {
		if err := set.fn(set.at); err != nil && !errors.Is(err, http.ErrNotSupported) {
			log.Printf("WARN github %s deadline not extended: %v", set.name, err)
		}
	}
}

// countingReader counts the bytes read through it, so a failed body read can say how far it got.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
