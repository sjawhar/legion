package webhook

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"

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

// githubSkip returns true for event types that should NOT be published.
// Currently all events are published.
func githubSkip(_ string) bool {
	return false
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
	Record(owner, repo, number, sha, checkName, checkRunID, url, status, conclusion string) error
	RecordSuite(owner, repo, number, sha, suiteID, status, conclusion string, appID ...string) error
	RecordHead(owner, repo, number, sha string) error
}

func reviewerVerdict(name string) bool {
	return name == "tester" || name == "architect"
}

func githubPullRequestHead(event string, payload map[string]any) (owner, repo, number, sha string, ok bool) {
	if event != "pull_request" {
		return "", "", "", "", false
	}
	switch payload["action"] {
	case "opened", "synchronize", "reopened":
	default:
		return "", "", "", "", false
	}
	repository, ok := payload["repository"].(map[string]any)
	if !ok {
		return "", "", "", "", false
	}
	repositoryOwner, ok := repository["owner"].(map[string]any)
	if !ok {
		return "", "", "", "", false
	}
	owner, _ = repositoryOwner["login"].(string)
	repo, _ = repository["name"].(string)
	pullRequest, ok := payload["pull_request"].(map[string]any)
	if !ok {
		return "", "", "", "", false
	}
	head, ok := pullRequest["head"].(map[string]any)
	if !ok {
		return "", "", "", "", false
	}
	sha, _ = head["sha"].(string)
	value, ok := payload["number"].(float64)
	if !ok || value != math.Trunc(value) {
		return "", "", "", "", false
	}
	number = strconv.FormatInt(int64(value), 10)
	return owner, repo, number, sha, owner != "" && repo != "" && number != "" && sha != ""
}

// GitHubHandler returns the HTTP handler for GitHub webhook events.
func GitHubHandler(secret, mentionTrigger, reviewerAppID string, publisher Publisher, ci CIRecorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
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
		if owner, repo, number, sha, ok := githubPullRequestHead(event, payload); ok {
			if err := ci.RecordHead(owner, repo, number, sha); err != nil {
				log.Printf("github ci head record failed: %v", err)
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		// CI events only update durable state. The summary loop publishes one
		// settled checks envelope when the head's suites and check runs are done.
		if obs := contracts.GithubCIObservations(event, payload); len(obs) > 0 {
			for _, o := range obs {
				if o.SuiteID != "" {
					if err := ci.RecordSuite(o.Owner, o.Repo, o.Number, o.SHA, o.SuiteID, o.Status, o.Conclusion, o.AppID); err != nil {
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
				if err := ci.Record(o.Owner, o.Repo, o.Number, o.SHA, o.CheckName, o.CheckRunID, o.URL, o.Status, o.Conclusion); err != nil {
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
