package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// TestAPersonsCommentOnADocumentReachesTheSessionsThatWroteIt writes documents through the API as
// sessions and people, comments on them, and runs the real outbox over each comment's event to read
// which sessions' own topics it reached.
func TestAPersonsCommentOnADocumentReachesTheSessionsThatWroteIt(t *testing.T) {
	writer := map[string]any{"kind": "session", "id": "session-writer"}
	editor := map[string]any{"kind": "session", "id": "session-editor"}
	reviewer := map[string]any{"kind": "session", "id": "session-reviewer"}

	var sendsMu sync.Mutex
	sends := map[string]int{}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"session_id": "session-writer", "title": "writer", "capabilities": []string{"btw", "steer"}},
				{"session_id": "session-editor", "title": "editor", "capabilities": []string{"btw", "steer"}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var input struct {
				TargetSession string `json:"target_session"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode mention send: %v", err)
			}
			sendsMu.Lock()
			sends[input.TargetSession]++
			sendsMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"event_id": "mention-envelope", "recipient": input.TargetSession})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	issue := createInteractionIssue(t, handler, "DOCS", "Document comments", "The issue's own spec.")

	// upload writes a document as actor, a session's actor map or a person's login, and returns
	// its id; a second upload of the same name is a new version crediting its uploader.
	upload := func(t *testing.T, target, name, content string, actor any) string {
		t.Helper()
		body := map[string]any{"name": name, "content": content}
		var response *httptest.ResponseRecorder
		if login, person := actor.(string); person {
			response = dispatchRequest(t, handler, http.MethodPost, target, body, login)
		} else {
			body["actor"] = actor
			response = sessionRequest(t, handler, http.MethodPost, target, body)
		}
		if response.Code != http.StatusCreated {
			t.Fatalf("upload %s: status=%d body=%s", name, response.Code, response.Body.String())
		}
		return decodeBody[struct {
			Artifact struct {
				ID string `json:"id"`
			} `json:"artifact"`
		}](t, response).Artifact.ID
	}
	// comment posts body to target as actor and returns the comment's id.
	comment := func(t *testing.T, target string, body map[string]any, actor any) string {
		t.Helper()
		var response *httptest.ResponseRecorder
		if login, person := actor.(string); person {
			response = dispatchRequest(t, handler, http.MethodPost, target, body, login)
		} else {
			body["actor"] = actor
			response = sessionRequest(t, handler, http.MethodPost, target, body)
		}
		if response.Code != http.StatusCreated {
			t.Fatalf("comment on %s: status=%d body=%s", target, response.Code, response.Body.String())
		}
		return decodeBody[struct {
			ID string `json:"id"`
		}](t, response).ID
	}
	passage := func(artifactID, quote string) map[string]any {
		return map[string]any{"artifact": artifactID, "quote": quote}
	}
	// reached runs the outbox over the comment's event and returns whether it notified and the
	// sessions' own topics it reached, each as often as it was published there.
	reached := func(t *testing.T, commentID string) (bool, []string) {
		t.Helper()
		deliveries := eventDeliveries(t, database, deps.Docs, "comment.created", commentID)
		if len(deliveries) != 1 {
			t.Fatalf("comment.created events for %s = %#v, want one", commentID, deliveries)
		}
		agents := []string{}
		for _, topic := range deliveries[0].Destinations {
			if strings.HasPrefix(topic, contracts.AgentTopicPrefix) {
				agents = append(agents, topic)
			}
		}
		return deliveries[0].Notify, agents
	}
	expect := func(t *testing.T, commentID string, notify bool, want ...string) {
		t.Helper()
		if want == nil {
			want = []string{}
		}
		gotNotify, got := reached(t, commentID)
		if gotNotify != notify || !slices.Equal(got, want) {
			t.Fatalf("comment %s: notify=%t reached %v, want notify=%t reaching %v", commentID, gotNotify, got, notify, want)
		}
	}

	issueDocument := upload(t, "/api/v1/issues/"+issue.Key+"/artifacts", "design.md", "We ship the design on Friday.\n", writer)
	projectDocument := upload(t, "/api/v1/projects/DOCS/artifacts", "plan.md", "The plan ships on Friday.\n", writer)
	upload(t, "/api/v1/projects/DOCS/artifacts", "plan.md", "The plan ships on Friday, after the review.\n", editor)
	humanDocument := upload(t, "/api/v1/projects/DOCS/artifacts", "notes.md", "Notes only people wrote.\n", "alice")

	t.Run("a person's comment on a passage of an issue's document reaches the session that wrote it", func(t *testing.T) {
		id := comment(t, "/api/v1/issues/"+issue.Key+"/comments",
			map[string]any{"body": "Why Friday?", "anchor": passage(issueDocument, "on Friday")}, "alice")
		expect(t, id, true, contracts.AgentSubject("session-writer"))
	})
	t.Run("a person's comment on a project document reaches each session that wrote it once", func(t *testing.T) {
		id := comment(t, "/api/v1/artifacts/"+projectDocument+"/comments",
			map[string]any{"body": "Which review?", "anchor": passage(projectDocument, "after the review")}, "alice")
		expect(t, id, true, contracts.AgentSubject("session-writer"), contracts.AgentSubject("session-editor"))
	})
	t.Run("a person's suggestion on a project document reaches each session that wrote it", func(t *testing.T) {
		id := comment(t, "/api/v1/artifacts/"+projectDocument+"/comments", map[string]any{
			"body": "Name the day.", "anchor": passage(projectDocument, "on Friday"),
			"suggestion": map[string]any{"replace_with": "on Thursday"},
		}, "alice")
		expect(t, id, true, contracts.AgentSubject("session-writer"), contracts.AgentSubject("session-editor"))
	})
	t.Run("a person's comment on a whole project document reaches each session that wrote it", func(t *testing.T) {
		id := comment(t, "/api/v1/artifacts/"+projectDocument+"/comments", map[string]any{"body": "Looks right."}, "alice")
		expect(t, id, true, contracts.AgentSubject("session-writer"), contracts.AgentSubject("session-editor"))
	})
	t.Run("a document only people wrote reaches no session", func(t *testing.T) {
		id := comment(t, "/api/v1/artifacts/"+humanDocument+"/comments",
			map[string]any{"body": "Agreed.", "anchor": passage(humanDocument, "people wrote")}, "bob")
		expect(t, id, true)
	})
	t.Run("an agent's comment reaches no author, the commenting author included", func(t *testing.T) {
		own := comment(t, "/api/v1/artifacts/"+projectDocument+"/comments",
			map[string]any{"body": "I will name the day.", "anchor": passage(projectDocument, "on Friday")}, writer)
		expect(t, own, false)
		other := comment(t, "/api/v1/artifacts/"+projectDocument+"/comments",
			map[string]any{"body": "A second look.", "anchor": passage(projectDocument, "the review")}, reviewer)
		expect(t, other, false)
	})
	t.Run("a reply reaches its thread's author and not the document's writers", func(t *testing.T) {
		root := comment(t, "/api/v1/artifacts/"+projectDocument+"/comments",
			map[string]any{"body": "Is Friday firm?", "anchor": passage(projectDocument, "ships on Friday")}, reviewer)
		expect(t, root, false)
		reply := comment(t, "/api/v1/artifacts/"+projectDocument+"/comments",
			map[string]any{"body": "Yes, Friday.", "reply_to": root}, "alice")
		expect(t, reply, true, contracts.AgentSubject("session-reviewer"))
	})
	t.Run("an author the comment mentions hears it once, from the mention", func(t *testing.T) {
		id := comment(t, "/api/v1/artifacts/"+projectDocument+"/comments", map[string]any{
			"body": "@writer, is the day firm?", "anchor": passage(projectDocument, "The plan"),
			"mentions": []map[string]any{{"target": "session:session-writer"}},
		}, "alice")
		var suppressed []string
		if err := database.Pool.QueryRow(context.Background(), `
			select payload->'suppressed_authors' from events where type = 'comment.created' and payload->>'id' = $1
		`, id).Scan(&suppressed); err != nil {
			t.Fatalf("read the comment's suppressed authors: %v", err)
		}
		if !slices.Equal(suppressed, []string{"session-writer"}) {
			t.Fatalf("suppressed_authors = %v, want the mentioned author alone", suppressed)
		}
		expect(t, id, true, contracts.AgentSubject("session-editor"))
		sendsMu.Lock()
		defer sendsMu.Unlock()
		if sends["session-writer"] != 1 || sends["session-editor"] != 0 {
			t.Fatalf("mention sends = %v, want one to session-writer", sends)
		}
	})
}
