package contracts_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/webhook"
)

// TestMain removes the NATS server the package's tests share (testnats.Main).
func TestMain(m *testing.M) { os.Exit(testnats.Main(m)) }

type object = map[string]any

// noCI is the CI store of a test whose events record no CI observation.
type noCI struct{ t *testing.T }

func (c noCI) Record(context.Context, contracts.CIObservation) error {
	c.t.Error("a pull request, review, push or workflow run recorded a CI observation")
	return nil
}

func (c noCI) RecordSuite(context.Context, contracts.CIObservation) error {
	c.t.Error("a pull request, review, push or workflow run recorded a CI suite")
	return nil
}

// The listener publishes each envelope to NATS whole, and the server refuses a message past its max
// payload, 1 MiB by default, which would lose the event on every delivery. The normalizer bounds
// every text an envelope copies from its webhook at MaxEnvelopeTextRunes, and a push's changed paths
// as a list (MaxPushChangedPaths, MaxPushChangedPathRunes). Each kind of event the daemon reads is
// posted to the listener's handler here with every text it copies past those bounds, under the
// longest owner (39 characters) and repository name (100) GitHub allows, so its envelope is the
// largest the normalizer makes: in 4-byte runes, and in `<`, which costs the most bytes a rune once
// the payload is a JSON string inside the envelope's JSON (`\\u003c`, 7 bytes). Each is answered
// 200 and stored by a real NATS server at the default max payload. A workflow run's file name,
// a segment of its subject, is the longest a filesystem keeps (255 bytes); a checks settlement has
// its own bound, which the CI store's tests publish at.
func TestTheLargestEnvelopeOfEachKindTheDaemonReadsIsStored(t *testing.T) {
	owner, repo := strings.Repeat("o", 39), strings.Repeat("r", 100)
	repository := object{"name": repo, "full_name": owner + "/" + repo, "owner": object{"login": owner}}
	for _, filler := range []struct{ name, rune string }{{"4-byte runes", "𝔸"}, {"<", "<"}} {
		over := strings.Repeat(filler.rune, contracts.MaxEnvelopeTextRunes+500)
		for _, tc := range []struct {
			event  string
			body   object
			capped []string
		}{
			{
				event: "pull_request",
				body: object{"action": over, "repository": repository, "pull_request": object{
					"number": 42, "title": over, "body": over, "html_url": over, "user": object{"login": over},
					"head": object{"sha": over, "ref": over}, "base": object{"ref": over},
					"merged": true, "merge_commit_sha": over, "merged_by": object{"login": over}, "updated_at": over,
				}},
				capped: []string{"action", "title", "body", "author", "url", "head_sha", "head_ref", "base_ref", "merge_commit_sha", "merged_by", "updated_at"},
			},
			{
				event: "pull_request_review",
				body: object{"action": over, "repository": repository,
					"pull_request": object{"number": 42, "title": over, "head": object{"sha": over}},
					"review": object{
						"id": 77, "body": over, "user": object{"login": over}, "html_url": over, "state": over,
						"submitted_at": over, "commit_id": over,
					},
				},
				capped: []string{"action", "title", "body", "author", "url", "state", "submitted_at", "commit_id", "head_sha"},
			},
			{
				event:  "push",
				body:   largestPush(repository, filler.rune, over),
				capped: []string{"ref", "before", "after", "pusher", "head_subject", "compare_url"},
			},
			{
				event: "workflow_run",
				body: object{"action": "completed", "repository": repository, "workflow_run": object{
					"id": 900, "run_attempt": 1, "name": over,
					"path":        ".github/workflows/" + strings.Repeat(filler.rune, (255-len(".yml"))/len(filler.rune)) + ".yml",
					"head_branch": over, "status": over, "conclusion": over, "head_sha": over,
					"run_started_at": over, "updated_at": over, "html_url": over, "pull_requests": []any{},
				}},
				capped: []string{"workflow", "branch", "head_branch", "status", "conclusion", "head_sha", "run_started_at", "updated_at", "url"},
			},
		} {
			t.Run(filler.name+"/"+tc.event, func(t *testing.T) {
				client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
				if err != nil {
					t.Fatalf("connect the bus: %v", err)
				}
				defer client.Close()
				if got := client.Conn.MaxPayload(); got != 1<<20 {
					t.Fatalf("the server's max payload is %d, want the 1 MiB default", got)
				}

				body, err := json.Marshal(tc.body)
				if err != nil {
					t.Fatalf("marshal %s: %v", tc.event, err)
				}
				const secret = "s"
				mac := hmac.New(sha256.New, []byte(secret))
				mac.Write(body)
				req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
				req.Header.Set("X-GitHub-Delivery", "delivery-largest-"+tc.event)
				req.Header.Set("X-GitHub-Event", tc.event)
				req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
				rr := httptest.NewRecorder()
				webhook.GitHubHandler(secret, "@legion", "", client, noCI{t}).ServeHTTP(rr, req)
				if rr.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
				}

				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				stored, err := client.NextStoredMessage(ctx, contracts.GithubSubject(owner, repo, ">"), 0)
				if err != nil {
					t.Fatalf("read the stored %s: %v", tc.event, err)
				}
				var envelope contracts.Envelope
				if err := json.Unmarshal(stored.Data, &envelope); err != nil {
					t.Fatalf("decode the stored %s: %v", tc.event, err)
				}
				var payload map[string]string
				if err := json.Unmarshal([]byte(envelope.Payload), &payload); err != nil {
					t.Fatalf("decode the stored %s's payload: %v", tc.event, err)
				}
				// Every text the test made too long reaches the envelope at its bound, so the envelope
				// stored is the largest the normalizer makes.
				for _, field := range tc.capped {
					if runes := utf8.RuneCountInString(payload[field]); runes < contracts.MaxEnvelopeTextRunes {
						t.Errorf("payload %s is %d runes, want it at the %d-rune bound", field, runes, contracts.MaxEnvelopeTextRunes)
					}
				}
				if tc.event == "push" {
					paths := payload["changed_paths"]
					if lines := strings.Count(paths, "\n") + 1; lines != contracts.MaxPushChangedPaths ||
						utf8.RuneCountInString(paths) != contracts.MaxPushChangedPathRunes || payload["changed_paths_truncated"] != "true" {
						t.Errorf("changed paths are %d paths in %d runes (truncated %q), want %d paths in %d runes, truncated",
							lines, utf8.RuneCountInString(paths), payload["changed_paths_truncated"],
							contracts.MaxPushChangedPaths, contracts.MaxPushChangedPathRunes)
					}
				}
				t.Logf("the stored %s is %d bytes on a %d-byte subject", tc.event, len(stored.Data), len(stored.Subject))
			})
		}
	}
}

// largestPush is a push whose every copied text is over, whose ref is 5,000 runes, and whose
// commits list MaxPushChangedPaths paths of filler that, with the newlines joining them, are
// exactly MaxPushChangedPathRunes, and one path more.
func largestPush(repository object, filler, over string) object {
	const n = contracts.MaxPushChangedPaths
	text := contracts.MaxPushChangedPathRunes - (n - 1)
	paths := make([]any, n+1)
	for i := range paths {
		runes := text / n
		if i < text%n {
			runes++
		}
		prefix := fmt.Sprintf("%03d/", i)
		paths[i] = prefix + strings.Repeat(filler, runes-len(prefix))
	}
	return object{
		"ref": "refs/heads/" + strings.Repeat(filler, 5000), "before": over, "after": over,
		"compare": over, "forced": true, "repository": repository, "pusher": object{"name": over},
		"head_commit": object{"message": over}, "commits": []any{object{
			"added": paths[:n/2], "modified": paths[n/2:], "removed": []any{},
		}},
	}
}
