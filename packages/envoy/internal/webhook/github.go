package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/verify"
	"golang.org/x/sync/semaphore"
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

// CIRecorder folds check-run, check-suite, and PR-head observations into CI state.
type CIRecorder interface {
	Record(contracts.CIObservation) error
	RecordSuite(contracts.CIObservation) error
	RecordHead(owner, repo, number, sha, updatedAt string) error
}

func reviewerVerdict(name string) bool {
	return name == "tester" || name == "architect"
}

func githubPullRequestHead(event string, payload map[string]any) (owner, repo, number, sha, updatedAt string, ok bool) {
	if event != "pull_request" {
		return "", "", "", "", "", false
	}
	switch payload["action"] {
	case "opened", "synchronize", "reopened":
	default:
		return "", "", "", "", "", false
	}
	repository, ok := payload["repository"].(map[string]any)
	if !ok {
		return "", "", "", "", "", false
	}
	repositoryOwner, ok := repository["owner"].(map[string]any)
	if !ok {
		return "", "", "", "", "", false
	}
	owner, _ = repositoryOwner["login"].(string)
	repo, _ = repository["name"].(string)
	pullRequest, ok := payload["pull_request"].(map[string]any)
	if !ok {
		return "", "", "", "", "", false
	}
	head, ok := pullRequest["head"].(map[string]any)
	if !ok {
		return "", "", "", "", "", false
	}
	sha, _ = head["sha"].(string)
	updatedAt, _ = pullRequest["updated_at"].(string)
	if updatedAt != "" {
		if _, err := time.Parse(time.RFC3339, updatedAt); err != nil {
			updatedAt = ""
		}
	}
	number = contracts.GithubPRNumber(payload["number"])
	return owner, repo, number, sha, updatedAt, owner != "" && repo != "" && number != "" && sha != ""
}

// githubMaxBody is the largest webhook body the handler reads: GitHub's documented payload cap,
// 25 MB, read as MiB so no delivery GitHub sends is refused. A push of a thousand commits is
// several megabytes, and a refused delivery is lost for good, since a redelivery is the same body.
const githubMaxBody = 25 << 20

// githubBodyBudget bounds the webhook body bytes the handler holds at once, across all requests.
// The body is buffered whole before its signature can be checked, so without it anyone who can
// reach the route could make the listener hold 25 MiB per connection; a request waits for room
// before it reads its body, and holds it until the handler returns. A signed body decodes to
// about two and a half times its size again, so the process stays near 200 MB even when every
// request is a signed cap-sized push (measured with eight at once). It is at least githubMaxBody,
// so any body GitHub sends gets its turn.
const githubBodyBudget = 64 << 20

// readGitHubBody reads the request body into a buffer of the declared length, so the read never
// grows the buffer past the body; a body of undeclared length is read up to githubMaxBody.
func readGitHubBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.ContentLength < 0 {
		return io.ReadAll(http.MaxBytesReader(w, r.Body, githubMaxBody))
	}
	body := make([]byte, r.ContentLength)
	if _, err := io.ReadFull(r.Body, body); err != nil {
		return nil, err
	}
	return body, nil
}

// GitHubHandler returns the HTTP handler for GitHub webhook events.
func GitHubHandler(secret, mentionTrigger, reviewerAppID string, publisher Publisher, ci CIRecorder) http.HandlerFunc {
	bodies := semaphore.NewWeighted(githubBodyBudget)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.ContentLength > githubMaxBody {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		// A body of undeclared length is charged the whole cap it may grow to.
		held := r.ContentLength
		if held < 0 {
			held = githubMaxBody
		}
		if err := bodies.Acquire(r.Context(), held); err != nil {
			// The caller went away while it waited for room.
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		defer bodies.Release(held)
		body, err := readGitHubBody(w, r)
		if err != nil {
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
		if owner, repo, number, sha, updatedAt, ok := githubPullRequestHead(event, payload); ok {
			if err := ci.RecordHead(owner, repo, number, sha, updatedAt); err != nil {
				if errors.Is(err, cistore.ErrInvalidHeadSHA) {
					log.Printf("github ci head skipped: invalid sha=%q pr=%s", sha, number)
				} else {
					log.Printf("github ci head record failed: %v", err)
					http.Error(w, "service unavailable", http.StatusServiceUnavailable)
					return
				}
			}
		}
		// CI events only update durable state. The summary loop publishes one
		// settled checks envelope when the head's suites and check runs are done.
		if obs := contracts.GithubCIObservations(event, payload); len(obs) > 0 {
			for _, o := range obs {
				if o.CheckName == "" {
					if err := ci.RecordSuite(o); err != nil {
						log.Printf("github ci suite record failed: %v", err)
						http.Error(w, "service unavailable", http.StatusServiceUnavailable)
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
					log.Printf("github ci record failed: %v", err)
					http.Error(w, "service unavailable", http.StatusServiceUnavailable)
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
				log.Printf("github publish failed: %v", err)
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
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
