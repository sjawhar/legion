package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

type testAdviceAsk struct {
	ID       string `json:"id"`
	Question string `json:"question"`
}

type testWriteAdvice struct {
	IssueStatus             string          `json:"issue_status"`
	SessionWritesSinceHuman int             `json:"session_writes_since_human"`
	YourOpenAsks            []testAdviceAsk `json:"your_open_asks"`
	DecisionBlocks          *int            `json:"decision_blocks"`
	DecisionBlocksAdded     *int            `json:"decision_blocks_added"`
	UnparsedOpeners         *struct {
		Count    int      `json:"count"`
		Examples []string `json:"examples"`
	} `json:"unparsed_openers"`
}

type testAdviceEnvelope struct {
	Advice *testWriteAdvice `json:"advice"`
}

func createAdviceProject(t *testing.T, handler http.Handler, key string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": key, "name": key + " project",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
}

func createAdviceIssue(t *testing.T, handler http.Handler, project, title string, spec *string) struct {
	Key               string           `json:"key"`
	PrimaryArtifactID string           `json:"primary_artifact_id"`
	Advice            *testWriteAdvice `json:"advice"`
} {
	t.Helper()
	body := map[string]any{"project": project, "title": title, "force": true}
	if spec != nil {
		body["spec"] = *spec
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Key               string           `json:"key"`
		PrimaryArtifactID string           `json:"primary_artifact_id"`
		Advice            *testWriteAdvice `json:"advice"`
	}](t, response)
}

func adviceFromResponse(t *testing.T, responseCode int, responseBody string) *testWriteAdvice {
	t.Helper()
	if responseCode != http.StatusCreated && responseCode != http.StatusOK {
		t.Fatalf("write: status=%d body=%s", responseCode, responseBody)
	}
	var envelope testAdviceEnvelope
	if err := json.Unmarshal([]byte(responseBody), &envelope); err != nil {
		t.Fatalf("decode advice response %q: %v", responseBody, err)
	}
	if envelope.Advice == nil {
		t.Fatalf("response omitted advice: %s", responseBody)
	}
	return envelope.Advice
}

func adviceSessionActor(id string) map[string]any {
	return map[string]any{"kind": "session", "id": id}
}

func TestWriteAdviceCountsDecisionBlocksAtDocumentWriteTime(t *testing.T) {
	handler := newTestHandler(t)
	createAdviceProject(t, handler, "ADVICE")

	withoutBlocks := "# Proposal\n\nNo decision here.\n"
	without := createAdviceIssue(t, handler, "ADVICE", "No decisions", &withoutBlocks)
	if without.Advice == nil || without.Advice.DecisionBlocks == nil || *without.Advice.DecisionBlocks != 0 {
		t.Fatalf("zero-block issue advice = %#v", without.Advice)
	}

	withBlocks := `# Proposal

:::ask{#first urgency="med" multiple="false" state="open"}
Which path?
:::

:::ask{#second urgency="high" multiple="false" state="answered" answered_by="alice" answered_at="2026-09-12T13:20:00Z" selected="[&#x22;REST&#x22;]" answer="Ship REST first."}
Which transport?

* REST: Existing platform
* gRPC: Adds streaming
:::
`
	with := createAdviceIssue(t, handler, "ADVICE", "Two decisions", &withBlocks)
	if with.Advice == nil || with.Advice.DecisionBlocks == nil || *with.Advice.DecisionBlocks != 2 {
		t.Fatalf("two-block issue advice = %#v", with.Advice)
	}

	withoutSpec := createAdviceIssue(t, handler, "ADVICE", "No spec", nil)
	if withoutSpec.Advice == nil {
		t.Fatal("issue created without a spec omitted advice")
	}
	if withoutSpec.Advice.DecisionBlocks != nil {
		t.Fatalf("decision_blocks of an issue created without a spec = %d, want absent", *withoutSpec.Advice.DecisionBlocks)
	}
	if withoutSpec.Advice.YourOpenAsks == nil {
		t.Fatal("human issue-create your_open_asks decoded as nil, want []")
	}

	projectUpload := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/ADVICE/artifacts", map[string]string{
		"name": "project-spec.md", "content": withBlocks,
	}, "alice")
	if projectUpload.Code != http.StatusCreated {
		t.Fatalf("project upload: status=%d body=%s", projectUpload.Code, projectUpload.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(projectUpload.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode project upload: %v", err)
	}
	var projectAdvice map[string]json.RawMessage
	if err := json.Unmarshal(raw["advice"], &projectAdvice); err != nil {
		t.Fatalf("decode project advice: %v body=%s", err, projectUpload.Body.String())
	}
	if len(projectAdvice) != 1 {
		t.Fatalf("project advice keys = %v, want decision_blocks only", projectAdvice)
	}
	var projectCount int
	if err := json.Unmarshal(projectAdvice["decision_blocks"], &projectCount); err != nil || projectCount != 2 {
		t.Fatalf("project decision_blocks = %d err=%v", projectCount, err)
	}
	issueUpload := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+with.Key+"/artifacts", map[string]string{
		"name": "decisions.md", "content": withBlocks,
	}, "alice")
	issueUploadAdvice := adviceFromResponse(t, issueUpload.Code, issueUpload.Body.String())
	if issueUploadAdvice.DecisionBlocks == nil || *issueUploadAdvice.DecisionBlocks != 2 {
		t.Fatalf("issue artifact decision_blocks = %#v, want 2", issueUploadAdvice.DecisionBlocks)
	}
	if issueUploadAdvice.IssueStatus != "triage" {
		t.Fatalf("issue artifact status = %q, want triage", issueUploadAdvice.IssueStatus)
	}
}

func TestWriteAdviceOmitsDecisionBlocksWhenMarkdownCannotBeParsed(t *testing.T) {
	blocks := readDocumentBlocks(":::callout{#broken}\nUnclosed\n")
	if blocks != nil {
		t.Fatalf("invalid markdown document blocks = %#v, want absent", blocks)
	}
	encoded, err := json.Marshal(withAdvice(map[string]bool{"ok": true}, &writeAdvice{
		IssueStatus: "triage", YourOpenAsks: []adviceAsk{}, documentBlocks: blocks,
	}))
	if err != nil {
		t.Fatalf("marshal advice: %v", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var rawAdvice map[string]json.RawMessage
	if err := json.Unmarshal(envelope["advice"], &rawAdvice); err != nil {
		t.Fatalf("decode advice: %v", err)
	}
	for _, field := range []string{"decision_blocks", "unparsed_openers"} {
		if _, exists := rawAdvice[field]; exists {
			t.Fatalf("parse failure emitted %s: %s", field, encoded)
		}
	}
}

// A code block before an ask - a spec that shows a snippet, then asks about it - neither hides the
// ask from the count nor adds the opening quoted in the code to the openings stored as text.
func TestReadDocumentBlocksReadsPastCode(t *testing.T) {
	blocks := readDocumentBlocks("Context\n\n```md\n:::ask{urgency=\"med\"}\n```\n\n:::ask{urgency=\"med\"}\nShip it?\n:::\n\nThen write :::callout{kind=\"note\"} as text.\n")
	if blocks == nil || blocks.DecisionBlocks != 1 || blocks.UnparsedOpeners == nil || blocks.UnparsedOpeners.Count != 1 {
		t.Fatalf("blocks = %#v (unparsed %#v), want one ask block and the one opening after it", blocks, blocks.UnparsedOpeners)
	}
}

// An opening longer than the stretch an example quotes - a long name, or a long run of colons - is
// quoted whole rather than taking the write's response down after the write committed.
func TestReadDocumentBlocksQuotesLongOpenings(t *testing.T) {
	name := strings.Repeat("n", 90)
	colons := strings.Repeat(":", 90)
	blocks := readDocumentBlocks("Write :::" + name + "{a=\"b\"} and " + colons + "ask{} as text.\n")
	if blocks == nil || blocks.UnparsedOpeners == nil || blocks.UnparsedOpeners.Count != 2 ||
		!strings.Contains(blocks.UnparsedOpeners.Examples[0], ":::"+name+"{") || !strings.Contains(blocks.UnparsedOpeners.Examples[1], colons+"ask{") {
		t.Fatalf("blocks = %#v, want both long openings quoted whole", blocks)
	}
}

// A typed block opening a document stores as text - inside a line, where the parser cannot make
// it a block - is reported beside the decision-block count, so a writer who meant a block hears
// that it is text rather than only that the document holds no decisions (LEGION-416). A mention
// in code is not an opening, and a document whose openings all became blocks reports none.
func TestWriteAdviceReportsTypedBlockOpeningsStoredAsText(t *testing.T) {
	handler := newTestHandler(t)
	createAdviceProject(t, handler, "OPENERS")

	asText := "Intro 27::::ask{urgency=\"med\"} Ship it? 33:::: 57::::ask{urgency=\"low\"} Later?\n\nThe syntax is `:::ask{urgency=\"med\"}`.\n"
	created := createAdviceIssue(t, handler, "OPENERS", "Openings as text", &asText)
	if created.Advice == nil || created.Advice.DecisionBlocks == nil || *created.Advice.DecisionBlocks != 0 {
		t.Fatalf("advice = %#v, want decision_blocks 0", created.Advice)
	}
	openers := created.Advice.UnparsedOpeners
	if openers == nil || openers.Count != 2 || len(openers.Examples) != 2 ||
		!strings.Contains(openers.Examples[0], `::::ask{urgency="med"}`) || !strings.Contains(openers.Examples[1], `::::ask{urgency="low"}`) {
		t.Fatalf("unparsed_openers = %#v, want the two openings inside the line and not the one in code", openers)
	}

	asBlock := "Context\n\n:::ask{urgency=\"med\"}\nShip it?\n:::\n\nThe syntax is `:::ask{urgency=\"med\"}`.\n"
	parsed := createAdviceIssue(t, handler, "OPENERS", "Opening as a block", &asBlock)
	if parsed.Advice == nil || parsed.Advice.DecisionBlocks == nil || *parsed.Advice.DecisionBlocks != 1 || parsed.Advice.UnparsedOpeners != nil {
		t.Fatalf("advice = %#v, want one decision block and no unparsed_openers", parsed.Advice)
	}

	projectUpload := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/OPENERS/artifacts", map[string]string{
		"name": "notes.md", "content": asText,
	}, "alice")
	if projectAdvice := adviceFromResponse(t, projectUpload.Code, projectUpload.Body.String()); projectAdvice.UnparsedOpeners == nil || projectAdvice.UnparsedOpeners.Count != 2 {
		t.Fatalf("project document advice = %#v, want two unparsed openers", projectAdvice)
	}
}

func TestWriteAdviceCountsSessionWritesSinceLastHumanEvent(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createAdviceProject(t, handler, "COUNT")
	spec := "# Count\n"
	issue := createAdviceIssue(t, handler, "COUNT", "Count writes", &spec)
	actor := adviceSessionActor("session-counter")
	path := "/api/v1/issues/" + issue.Key + "/messages"

	for want := 1; want <= 3; want++ {
		response := sessionRequest(t, handler, http.MethodPost, path, map[string]any{
			"body": "session message", "actor": actor,
		})
		advice := adviceFromResponse(t, response.Code, response.Body.String())
		if advice.SessionWritesSinceHuman != want {
			t.Fatalf("message %d count = %d, want %d", want, advice.SessionWritesSinceHuman, want)
		}
	}

	humanComment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]string{
		"body": "human response",
	}, "alice")
	if advice := adviceFromResponse(t, humanComment.Code, humanComment.Body.String()); advice.SessionWritesSinceHuman != 0 {
		t.Fatalf("human comment count = %d, want 0", advice.SessionWritesSinceHuman)
	}
	afterComment := sessionRequest(t, handler, http.MethodPost, path, map[string]any{"body": "after comment", "actor": actor})
	if advice := adviceFromResponse(t, afterComment.Code, afterComment.Body.String()); advice.SessionWritesSinceHuman != 1 {
		t.Fatalf("count after human comment = %d, want 1", advice.SessionWritesSinceHuman)
	}

	patch := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice")
	if advice := adviceFromResponse(t, patch.Code, patch.Body.String()); advice.SessionWritesSinceHuman != 0 {
		t.Fatalf("human patch count = %d, want 0", advice.SessionWritesSinceHuman)
	}
	afterPatch := sessionRequest(t, handler, http.MethodPost, path, map[string]any{"body": "after patch", "actor": actor})
	if advice := adviceFromResponse(t, afterPatch.Code, afterPatch.Body.String()); advice.SessionWritesSinceHuman != 1 {
		t.Fatalf("count after human patch = %d, want 1", advice.SessionWritesSinceHuman)
	}

	reset := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]string{"body": "reset before ask"}, "alice")
	if reset.Code != http.StatusCreated {
		t.Fatalf("reset comment: status=%d body=%s", reset.Code, reset.Body.String())
	}
	ask := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Does an ask count?", "actor": actor,
	})
	if advice := adviceFromResponse(t, ask.Code, ask.Body.String()); advice.SessionWritesSinceHuman != 1 {
		t.Fatalf("ask.opened count = %d, want 1", advice.SessionWritesSinceHuman)
	}

	reset = dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]string{"body": "reset before system"}, "alice")
	if reset.Code != http.StatusCreated {
		t.Fatalf("second reset comment: status=%d body=%s", reset.Code, reset.Body.String())
	}
	beforeSystem := sessionRequest(t, handler, http.MethodPost, path, map[string]any{"body": "before system", "actor": actor})
	if advice := adviceFromResponse(t, beforeSystem.Code, beforeSystem.Body.String()); advice.SessionWritesSinceHuman != 1 {
		t.Fatalf("count before system event = %d, want 1", advice.SessionWritesSinceHuman)
	}
	appendAdviceSystemEvent(t, database, issue.Key)
	afterSystem := sessionRequest(t, handler, http.MethodPost, path, map[string]any{"body": "after system", "actor": actor})
	if advice := adviceFromResponse(t, afterSystem.Code, afterSystem.Body.String()); advice.SessionWritesSinceHuman != 2 {
		t.Fatalf("system event changed count: got %d, want 2", advice.SessionWritesSinceHuman)
	}
}

func appendAdviceSystemEvent(t *testing.T, database *store.Store, issueKey string) {
	t.Helper()
	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin system event: %v", err)
	}
	defer tx.Rollback(context.Background())
	broker := events.NewBroker()
	if _, err := broker.Append(context.Background(), tx, issueOwner(issueKey).event(
		"system.test", model.Actor{Kind: "system", ID: "test-system"}, map[string]bool{"ok": true},
	)); err != nil {
		t.Fatalf("append system event: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit system event: %v", err)
	}
}

func TestWriteAdviceListsOnlyCallersOpenAsksAndExcludesCreatedAsk(t *testing.T) {
	handler := newTestHandler(t)
	createAdviceProject(t, handler, "ASKS")
	spec := "# Asks\n"
	issue := createAdviceIssue(t, handler, "ASKS", "Open asks", &spec)
	actorA := adviceSessionActor("session-a")
	actorB := adviceSessionActor("session-b")

	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which option?", "actor": actorA,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", created.Code, created.Body.String())
	}
	var askResponse struct {
		ID     string           `json:"id"`
		Advice *testWriteAdvice `json:"advice"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &askResponse); err != nil {
		t.Fatalf("decode created ask: %v", err)
	}
	if askResponse.Advice == nil || len(askResponse.Advice.YourOpenAsks) != 0 || askResponse.Advice.YourOpenAsks == nil {
		t.Fatalf("created ask advice = %#v, want non-nil empty your_open_asks", askResponse.Advice)
	}

	messagePath := "/api/v1/issues/" + issue.Key + "/messages"
	messageA := sessionRequest(t, handler, http.MethodPost, messagePath, map[string]any{"body": "from A", "actor": actorA})
	adviceA := adviceFromResponse(t, messageA.Code, messageA.Body.String())
	if len(adviceA.YourOpenAsks) != 1 || adviceA.YourOpenAsks[0].ID != askResponse.ID || adviceA.YourOpenAsks[0].Question != "Which option?" {
		t.Fatalf("session A open asks = %#v", adviceA.YourOpenAsks)
	}

	messageB := sessionRequest(t, handler, http.MethodPost, messagePath, map[string]any{"body": "from B", "actor": actorB})
	adviceB := adviceFromResponse(t, messageB.Code, messageB.Body.String())
	if len(adviceB.YourOpenAsks) != 0 || adviceB.YourOpenAsks == nil {
		t.Fatalf("session B open asks = %#v, want []", adviceB.YourOpenAsks)
	}

	resolved := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askResponse.ID+"/resolve", map[string]any{
		"kind": "resolved", "reason": "Decided elsewhere", "actor": actorA,
	})
	if resolved.Code != http.StatusOK {
		t.Fatalf("resolve ask: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	afterResolve := sessionRequest(t, handler, http.MethodPost, messagePath, map[string]any{"body": "after resolve", "actor": actorA})
	adviceAfterResolve := adviceFromResponse(t, afterResolve.Code, afterResolve.Body.String())
	if len(adviceAfterResolve.YourOpenAsks) != 0 || adviceAfterResolve.YourOpenAsks == nil {
		t.Fatalf("resolved ask remains in advice: %#v", adviceAfterResolve.YourOpenAsks)
	}
}

func TestWriteAdviceLimitsOpenAsksToTwoOldest(t *testing.T) {
	handler := newTestHandler(t)
	createAdviceProject(t, handler, "LIMIT")
	spec := "# Limited advice\n"
	issue := createAdviceIssue(t, handler, "LIMIT", "Limit open asks", &spec)
	actor := adviceSessionActor("session-limit")
	ids := make([]string, 0, 3)
	for _, question := range []string{"First?", "Second?", "Third?"} {
		response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
			"question": question, "actor": actor,
		})
		if response.Code != http.StatusCreated {
			t.Fatalf("create %q: status=%d body=%s", question, response.Code, response.Body.String())
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode %q: %v", question, err)
		}
		ids = append(ids, created.ID)
	}

	message := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "show blockers", "actor": actor,
	})
	advice := adviceFromResponse(t, message.Code, message.Body.String())
	if len(advice.YourOpenAsks) != 2 ||
		advice.YourOpenAsks[0].ID != ids[0] ||
		advice.YourOpenAsks[1].ID != ids[1] {
		t.Fatalf("open asks = %#v, want the two oldest ids %q, %q", advice.YourOpenAsks, ids[0], ids[1])
	}
}

func TestWriteAdviceReportsIssueStatusAtWriteTime(t *testing.T) {
	handler := newTestHandler(t)
	createAdviceProject(t, handler, "STATUS")
	spec := "# Status\n"
	issue := createAdviceIssue(t, handler, "STATUS", "Status advice", &spec)
	actor := adviceSessionActor("session-status")
	path := "/api/v1/issues/" + issue.Key + "/messages"

	triage := sessionRequest(t, handler, http.MethodPost, path, map[string]any{"body": "triage write", "actor": actor})
	if advice := adviceFromResponse(t, triage.Code, triage.Body.String()); advice.IssueStatus != "triage" {
		t.Fatalf("triage message status = %q", advice.IssueStatus)
	}
	patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "in_progress"}, "alice")
	if advice := adviceFromResponse(t, patched.Code, patched.Body.String()); advice.IssueStatus != "in_progress" {
		t.Fatalf("patch status = %q", advice.IssueStatus)
	}
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":   []map[string]string{{"op": "replace", "find": "Status", "with": "Progress"}},
		"actor": actor,
	})
	editAdvice := adviceFromResponse(t, edited.Code, edited.Body.String())
	if editAdvice.IssueStatus != "in_progress" || editAdvice.DecisionBlocks != nil {
		t.Fatalf("edit advice = %#v, want in-progress status without decision_blocks", editAdvice)
	}
	inProgress := sessionRequest(t, handler, http.MethodPost, path, map[string]any{"body": "in-progress write", "actor": actor})
	if advice := adviceFromResponse(t, inProgress.Code, inProgress.Body.String()); advice.IssueStatus != "in_progress" {
		t.Fatalf("post-patch message status = %q", advice.IssueStatus)
	}
}

// An edit's advice counts the ask blocks the edit added, as the server's parser reads the document
// it wrote: an opener is a block inside a blockquote or a list item, or behind more than three
// colons, and quoted in code it is text - in a fence at the top level, in a fence in a list item,
// or in indented code. A block the edit moved or reworded was already there.
func TestDocumentEditAdviceCountsTheAskBlocksTheEditAdded(t *testing.T) {
	handler := newTestHandler(t)
	createAdviceProject(t, handler, "ADDED")
	const spec = "# Spec\n\nPick a transport.\n\n:::ask{#decision urgency=\"high\"}\nWhich transport?\n:::\n\nContext ends with no drift.\n"
	insert := func(markdown string) func(t *testing.T, artifactID string) []map[string]any {
		return func(*testing.T, string) []map[string]any {
			return []map[string]any{{"op": "insert", "markdown": markdown, "after": "end"}}
		}
	}
	for _, test := range []struct {
		name  string
		ops   func(t *testing.T, artifactID string) []map[string]any
		added int
	}{
		{name: "a block at the top level", ops: insert(":::ask{#window}\nWhich deployment window?\n:::"), added: 1},
		{name: "a block in a blockquote", ops: insert("> :::ask{#window}\n> Which deployment window?\n> :::"), added: 1},
		{name: "a block on a list item's first line", ops: insert("- :::ask{#window}\n  Which deployment window?\n  :::"), added: 1},
		{name: "a block opened with four colons", ops: insert("::::ask{#window}\nWhich deployment window?\n::::"), added: 1},
		{name: "a block after a fence a list item's end closes", ops: insert("- Example:\n\n  ```md\n  code\n\n:::ask{#window}\nWhich deployment window?\n:::"), added: 1},
		{name: "a block after a tilde fence a longer run closes", ops: insert("~~~md\n:::ask{#example}\n:::\n~~~~\n\n:::ask{#window}\nWhich deployment window?\n:::"), added: 1},
		{name: "two blocks", ops: insert(":::ask{#window}\nWhich deployment window?\n:::\n\n> :::ask{#region}\n> Which region?\n> :::"), added: 2},
		{name: "a fenced example at the top level", ops: insert("Example:\n\n```md\n:::ask{#example}\nWhich?\n:::\n```"), added: 0},
		{name: "a fenced example four columns into a nested list item", ops: insert("- Format notes:\n  - Example:\n\n    ```md\n    :::ask{#example}\n    Which?\n    :::\n    ```"), added: 0},
		{name: "a fenced example four columns into item 10", ops: insert("10. Example:\n\n    ```md\n    :::ask{#example}\n    Which?\n    :::\n    ```"), added: 0},
		{name: "a fence opened on a list item's first line", ops: insert("- ```md\n  :::ask{#example}\n  Which?\n  :::\n  ```"), added: 0},
		{name: "an indented-code example", ops: insert("Example:\n\n    :::ask{#example}\n    Which?\n    :::"), added: 0},
		{name: "plain text", ops: insert("A plain revision."), added: 0},
		{name: "a retype into an ask block", ops: func(t *testing.T, artifactID string) []map[string]any {
			blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/blocks", nil, "alice"))
			for _, block := range blocks {
				if block.Type == "paragraph" {
					return []map[string]any{{"op": "retype", "block": block.ID, "type": "ask", "attributes": map[string]any{"urgency": "high"}}}
				}
			}
			t.Fatalf("blocks = %#v, want a paragraph", blocks)
			return nil
		}, added: 1},
		{name: "a move of an ask block", ops: func(*testing.T, string) []map[string]any {
			return []map[string]any{{"op": "move", "block": "decision", "after": "no drift."}}
		}, added: 0},
		{name: "a reworded ask block", ops: func(*testing.T, string) []map[string]any {
			return []map[string]any{{"op": "replace", "find": "Which transport?", "with": "Which wire transport?"}}
		}, added: 0},
	} {
		// Unguarded and under a document precondition, which applies the batch on its own path.
		for _, guarded := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, guarded=%t", test.name, guarded), func(t *testing.T) {
				specText := spec
				issue := createAdviceIssue(t, handler, "ADDED", test.name, &specText)
				body := map[string]any{"ops": test.ops(t, issue.PrimaryArtifactID)}
				if guarded {
					document := decodeBody[documentPreconditionRead](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
					body["precondition"] = map[string]string{"document": document.Token}
				}
				edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", body, "alice")
				advice := adviceFromResponse(t, edited.Code, edited.Body.String())
				if advice.DecisionBlocksAdded == nil || *advice.DecisionBlocksAdded != test.added {
					t.Fatalf("decision_blocks_added = %v, want %d: %s", advice.DecisionBlocksAdded, test.added, edited.Body.String())
				}
				if advice.DecisionBlocks != nil || advice.IssueStatus != "triage" {
					t.Fatalf("edit advice = %#v, want the issue's status and no decision_blocks", advice)
				}
			})
		}
	}

	// A project document has no issue state to report, so its edit's advice is the count alone.
	upload := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/ADDED/artifacts", map[string]string{
		"name": "project-notes.md", "content": "# Notes\n\nBody.\n",
	}, "alice")
	if upload.Code != http.StatusCreated {
		t.Fatalf("project upload: status=%d body=%s", upload.Code, upload.Body.String())
	}
	document := decodeBody[struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}](t, upload)
	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.Artifact.ID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "insert", "markdown": "> :::ask{#window}\n> Which deployment window?\n> :::", "after": "end"}},
	}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("project document edit: status=%d body=%s", edited.Code, edited.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(edited.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode project document edit: %v", err)
	}
	if advice := string(raw["advice"]); advice != `{"decision_blocks_added":1}` {
		t.Fatalf("project document edit advice = %s, want the count alone", advice)
	}
}

func TestWriteAdviceOnIdempotentExternalIssueCreate(t *testing.T) {
	handler := newTestHandler(t)
	createAdviceProject(t, handler, "EXTERNAL")
	mapped := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": "EXTERNAL",
	}, "alice")
	if mapped.Code != http.StatusOK {
		t.Fatalf("map external repository: status=%d body=%s", mapped.Code, mapped.Body.String())
	}

	first := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"external": "owner/repo#777",
	}, "alice")
	if first.Code != http.StatusCreated {
		t.Fatalf("first external create: status=%d body=%s", first.Code, first.Body.String())
	}
	ignoredSpec := ":::ask{#ignored urgency=\"med\" multiple=\"false\" state=\"open\"}\nIgnored?\n:::\n"
	second := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"external": "owner/repo#777",
		"spec":     ignoredSpec,
	}, "alice")
	advice := adviceFromResponse(t, second.Code, second.Body.String())
	if second.Code != http.StatusOK || advice.IssueStatus != "triage" || advice.YourOpenAsks == nil {
		t.Fatalf("idempotent external advice = %#v status=%d body=%s", advice, second.Code, second.Body.String())
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(second.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode idempotent external response: %v", err)
	}
	var rawAdvice map[string]json.RawMessage
	if err := json.Unmarshal(envelope["advice"], &rawAdvice); err != nil {
		t.Fatalf("decode idempotent external advice: %v", err)
	}
	if _, exists := rawAdvice["decision_blocks"]; exists {
		t.Fatalf("ignored spec produced decision_blocks: %s", second.Body.String())
	}
}

func TestWriteAdviceStaysBoundedOnLongIssueHistory(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createAdviceProject(t, handler, "PERF")
	spec := "# Performance\n"
	issue := createAdviceIssue(t, handler, "PERF", "Long event history", &spec)

	const seededEvents = 5000
	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin event seed: %v", err)
	}
	defer tx.Rollback(context.Background())
	var lastSeq int
	if err := tx.QueryRow(context.Background(), `
		select last_seq from issues where key = $1 for update
	`, issue.Key).Scan(&lastSeq); err != nil {
		t.Fatalf("lock issue for event seed: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `
		insert into events (issue_key, seq, type, actor, payload, notify)
		select $1, $2 + ordinal, 'message.created',
		       '{"kind":"session","id":"bulk-session"}'::jsonb, '{}'::jsonb, false
		from generate_series(1, $3) ordinal
	`, issue.Key, lastSeq, seededEvents); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `
		update issues set last_seq = $2 where key = $1
	`, issue.Key, lastSeq+seededEvents); err != nil {
		t.Fatalf("advance issue sequence: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit event seed: %v", err)
	}

	started := time.Now()
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "bounded advice", "actor": adviceSessionActor("session-performance"),
	})
	elapsed := time.Since(started)
	advice := adviceFromResponse(t, response.Code, response.Body.String())
	if advice.SessionWritesSinceHuman != seededEvents+1 {
		t.Fatalf("session write count = %d, want %d", advice.SessionWritesSinceHuman, seededEvents+1)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("advice round trip took %s, want under 2s", elapsed)
	}
}

func TestWriteAdviceFailureDoesNotAbortWrite(t *testing.T) {
	handler, database := newFailingAdviceHandler(t, "session_writes_since_human")
	createAdviceProject(t, handler, "FAIL")
	spec := "# Failure isolation\n"
	issue := createAdviceIssue(t, handler, "FAIL", "Advice failure", &spec)

	started := time.Now()
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "must still commit", "actor": adviceSessionActor("session-failure"),
	})
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("timed-out advice query held the write for %s, want under 2s", elapsed)
	}
	if response.Code != http.StatusCreated {
		t.Fatalf("message with failed advice: status=%d body=%s", response.Code, response.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode failure-isolated response: %v", err)
	}
	if _, exists := body["advice"]; exists {
		t.Fatalf("failed advice query still emitted advice: %s", response.Body.String())
	}
	var messages int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from messages where issue_key = $1 and body = 'must still commit'
	`, issue.Key).Scan(&messages); err != nil {
		t.Fatalf("count committed messages: %v", err)
	}
	if messages != 1 {
		t.Fatalf("committed messages = %d, want 1", messages)
	}
}
func TestDocumentEditAdviceKeepsCountWhenIssueAdviceFails(t *testing.T) {
	handler, _ := newFailingAdviceHandler(t, "session_writes_since_human")
	createAdviceProject(t, handler, "COUNT")
	spec := "# Count-only advice\n"
	issue := createAdviceIssue(t, handler, "COUNT", "Failed issue advice", &spec)

	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "insert", "markdown": ":::ask{#window}\nWhich deployment window?\n:::", "after": "end"}},
	}, "alice")
	advice := adviceFromResponse(t, edited.Code, edited.Body.String())
	if advice.DecisionBlocksAdded == nil || *advice.DecisionBlocksAdded != 1 {
		t.Fatalf("edit advice = %#v, want one added decision block: %s", advice, edited.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(edited.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode count-only edit response: %v", err)
	}
	var rawAdvice map[string]json.RawMessage
	if err := json.Unmarshal(body["advice"], &rawAdvice); err != nil {
		t.Fatalf("decode count-only advice: %v body=%s", err, edited.Body.String())
	}
	if len(rawAdvice) != 1 {
		t.Fatalf("count-only advice fields = %#v, want decision_blocks_added alone", rawAdvice)
	}
}

func newFailingAdviceHandler(t *testing.T, query string) (http.Handler, *store.Store) {
	t.Helper()
	database := storetest.Open(t)
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{
		Store: database, Events: broker, ServerURL: "https://dispatch.example",
	})
	t.Cleanup(func() {
		if err := documentService.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	allowed := map[string]struct{}{"alice": {}, "bob": {}}
	deps, err := NewDeps(DepsInput{
		Store:         database,
		Identity:      identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: allowed},
		AllowedLogins: allowed,
		AgentToken:    "agent-token",
		ServerURL:     "https://dispatch.example",
		Docs:          documentService,
		Events:        broker,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	s := &server{
		deps: deps,
		adviceQueryHook: func(ctx context.Context, tx pgx.Tx, got string) error {
			if got != query {
				return nil
			}
			_, err := tx.Exec(ctx, "select pg_sleep(2)")
			return err
		},
	}
	mux := http.NewServeMux()
	routes := s.routes()
	s.routeIndex = routeIndexEntries(routes)
	for _, route := range routes {
		mux.HandleFunc(route.Method+" "+route.Pattern, route.Handler)
	}
	return mux, database
}
