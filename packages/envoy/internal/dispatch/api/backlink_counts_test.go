package api

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type askBacklinkCount struct {
	ID                string `json:"id"`
	ReferencedByCount *int   `json:"referenced_by_count"`
}

func countsByAskID(asks []askBacklinkCount) map[string]*int {
	counts := make(map[string]*int, len(asks))
	for _, ask := range asks {
		counts[ask.ID] = ask.ReferencedByCount
	}
	return counts
}

func requireAskBacklinkCount(t *testing.T, counts map[string]*int, askID string, want int) {
	t.Helper()
	got, found := counts[askID]
	if !found || got == nil || *got != want {
		t.Fatalf("ask %q backlink count = %v, want %d", askID, got, want)
	}
}

func TestAskListsCarryBatchedBacklinkCounts(t *testing.T) {
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	target := createReferenceAPIIssue(t, handler, "CORE")
	source := createReferenceAPIIssue(t, handler, "OPS")

	citedResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+target.Key+"/asks", map[string]any{
		"question": "Which release should ship?",
	}, "alice")
	if citedResponse.Code != http.StatusCreated {
		t.Fatalf("create cited issue ask: status=%d body=%s", citedResponse.Code, citedResponse.Body.String())
	}
	cited := decodeBody[askBacklinkCount](t, citedResponse)
	uncitedResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+target.Key+"/asks", map[string]any{
		"question": "Which rollout should wait?",
	}, "alice")
	if uncitedResponse.Code != http.StatusCreated {
		t.Fatalf("create uncited issue ask: status=%d body=%s", uncitedResponse.Code, uncitedResponse.Body.String())
	}
	uncited := decodeBody[askBacklinkCount](t, uncitedResponse)

	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+source.Key+"/messages", map[string]any{
		"actor": sessionActor(),
		"body":  "The deployment depends on dispatch://" + target.Key + "/ask/" + cited.ID + ".",
	}); response.Code != http.StatusCreated {
		t.Fatalf("create cross-project message: status=%d body=%s", response.Code, response.Body.String())
	}

	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if inbox.Code != http.StatusOK {
		t.Fatalf("read inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	inboxCounts := countsByAskID(decodeBody[[]askBacklinkCount](t, inbox))
	requireAskBacklinkCount(t, inboxCounts, cited.ID, 1)
	requireAskBacklinkCount(t, inboxCounts, uncited.ID, 0)

	issueAsks := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+target.Key+"/asks", nil, "alice")
	if issueAsks.Code != http.StatusOK {
		t.Fatalf("list issue asks: status=%d body=%s", issueAsks.Code, issueAsks.Body.String())
	}
	requireAskBacklinkCount(t, countsByAskID(decodeBody[[]askBacklinkCount](t, issueAsks)), cited.ID, 1)

	askRead := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+cited.ID, nil, "alice")
	if askRead.Code != http.StatusOK {
		t.Fatalf("read ask: status=%d body=%s", askRead.Code, askRead.Body.String())
	}
	read := decodeBody[struct {
		Ask askBacklinkCount `json:"ask"`
	}](t, askRead)
	requireAskBacklinkCount(t, map[string]*int{read.Ask.ID: read.Ask.ReferencedByCount}, cited.ID, 1)

	document := createProjectDocument(t, handler, "CORE", "Decision record", "# Decision record\n")
	documentAskResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/asks", map[string]any{
		"question": "Approve the decision record?",
	}, "alice")
	if documentAskResponse.Code != http.StatusCreated {
		t.Fatalf("create document ask: status=%d body=%s", documentAskResponse.Code, documentAskResponse.Body.String())
	}
	documentAsk := decodeBody[askBacklinkCount](t, documentAskResponse)
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+source.Key+"/messages", map[string]any{
		"actor": sessionActor(),
		"body":  "The decision is dispatch://CORE/artifact/decision-record/ask/" + documentAsk.ID + ".",
	}); response.Code != http.StatusCreated {
		t.Fatalf("create document backlink message: status=%d body=%s", response.Code, response.Body.String())
	}
	artifactAsks := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+document.ID+"/asks", nil, "alice")
	if artifactAsks.Code != http.StatusOK {
		t.Fatalf("list document asks: status=%d body=%s", artifactAsks.Code, artifactAsks.Body.String())
	}
	requireAskBacklinkCount(t, countsByAskID(decodeBody[[]askBacklinkCount](t, artifactAsks)), documentAsk.ID, 1)
}

// referenceWriteEvent is the part of a message or comment event a reader holding batched
// backlink counts needs: which record was written, and which nodes' counts that write moved.
type referenceWriteEvent struct {
	Seq     int64  `json:"seq"`
	Type    string `json:"type"`
	Payload struct {
		ID                string                   `json:"id"`
		ReferencesChanged []model.ChangedReference `json:"references_changed"`
	} `json:"payload"`
}

// requireReferencesChanged reads the newest event of one type for one record, so a second edit
// of the same comment is judged on its own event rather than the first one's, and compares the
// targets it names with what the write actually moved.
func requireReferencesChanged(
	t *testing.T,
	handler http.Handler,
	issueKey, eventType, id string,
	want []model.ChangedReference,
) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/events", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read events: status=%d body=%s", response.Code, response.Body.String())
	}
	newest := referenceWriteEvent{}
	for _, event := range decodeBody[[]referenceWriteEvent](t, response) {
		if event.Type == eventType && event.Payload.ID == id && event.Seq >= newest.Seq {
			newest = event
		}
	}
	if newest.Type == "" {
		t.Fatalf("no %s event for %q in %s", eventType, id, response.Body.String())
	}
	if !reflect.DeepEqual(newest.Payload.ReferencesChanged, want) {
		t.Fatalf("%s %q references_changed = %#v, want %#v", eventType, id, newest.Payload.ReferencesChanged, want)
	}
}

// writtenRecord reads the id out of a create response.
type writtenRecord struct {
	ID string `json:"id"`
}

func requireIssueBacklinkCount(t *testing.T, handler http.Handler, key string, want int) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read issue: status=%d body=%s", response.Code, response.Body.String())
	}
	count := decodeBody[struct {
		ReferencedByCount int `json:"referenced_by_count"`
	}](t, response).ReferencedByCount
	if count != want {
		t.Fatalf("issue %q referenced_by_count = %d, want %d", key, count, want)
	}
}

// A reader carrying a batched backlink count refreshes the rows of the nodes a write cited, so
// the event has to name them. The write itself is the only place that knows: refs.ReplaceCounted
// reconciles the edges and resolves what moved in the same transaction that appends the event.
func TestReferenceWritesNameChangedTargets(t *testing.T) {
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	target := createReferenceAPIIssue(t, handler, "CORE")
	source := createReferenceAPIIssue(t, handler, "OPS")
	askResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+target.Key+"/asks", map[string]any{
		"question": "Should the rollout wait?",
	}, "alice")
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	askID := decodeBody[writtenRecord](t, askResponse).ID
	targetKey := target.Key

	plain := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+source.Key+"/messages", map[string]any{
		"actor": sessionActor(),
		"body":  "Starting the rollout now.",
	})
	if plain.Code != http.StatusCreated {
		t.Fatalf("create plain message: status=%d body=%s", plain.Code, plain.Body.String())
	}
	citing := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+source.Key+"/messages", map[string]any{
		"actor": sessionActor(),
		"body":  "Blocked on dispatch://" + target.Key + " and dispatch://" + target.Key + "/ask/" + askID + ".",
	})
	if citing.Code != http.StatusCreated {
		t.Fatalf("create citing message: status=%d body=%s", citing.Code, citing.Body.String())
	}
	requireReferencesChanged(t, handler, source.Key, "message.created", decodeBody[writtenRecord](t, plain).ID, nil)
	requireReferencesChanged(t, handler, source.Key, "message.created", decodeBody[writtenRecord](t, citing).ID, []model.ChangedReference{
		{Kind: "issue", ID: target.Key},
		{Kind: "ask", ID: askID, IssueKey: &targetKey},
	})

	comment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+source.Key+"/comments", map[string]any{
		"body": "Also waiting on dispatch://" + target.Key + ".",
	}, "alice")
	if comment.Code != http.StatusCreated {
		t.Fatalf("create citing comment: status=%d body=%s", comment.Code, comment.Body.String())
	}
	commentID := decodeBody[writtenRecord](t, comment).ID
	requireReferencesChanged(t, handler, source.Key, "comment.created", commentID, []model.ChangedReference{
		{Kind: "issue", ID: target.Key},
	})

	// The count the issue header names: this issue's own spec document is attached to it, so
	// the structural edge counts beside the message and the comment that cite it.
	requireIssueBacklinkCount(t, handler, target.Key, 3)

	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+commentID, map[string]any{
		"body": "Unblocked; the dependency is gone.",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("remove the mention: status=%d body=%s", response.Code, response.Body.String())
	}
	// The edit removed the edge, which moves the same count: the target is named either way.
	requireReferencesChanged(t, handler, source.Key, "comment.edited", commentID, []model.ChangedReference{
		{Kind: "issue", ID: target.Key},
	})
	requireIssueBacklinkCount(t, handler, target.Key, 2)

	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+commentID, map[string]any{
		"body": "Unblocked; nothing left to wait on.",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("edit without a mention: status=%d body=%s", response.Code, response.Body.String())
	}
	requireReferencesChanged(t, handler, source.Key, "comment.edited", commentID, nil)
	requireIssueBacklinkCount(t, handler, target.Key, 2)
}

// An ask's own clarification replies are inbound `replies_to` edges, and its card already shows
// that thread: an ask nothing cites must read as zero and its panel must stay empty.
func TestAskBacklinksExcludeItsOwnReplies(t *testing.T) {
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	askResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Reply-counted?",
	}, "alice")
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	askID := decodeBody[writtenRecord](t, askResponse).ID
	for _, body := range []string{"First clarification.", "Second clarification."} {
		if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
			"actor": sessionActor(), "ask_id": askID, "body": body,
		}); response.Code != http.StatusCreated {
			t.Fatalf("reply to the ask: status=%d body=%s", response.Code, response.Body.String())
		}
	}

	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if inbox.Code != http.StatusOK {
		t.Fatalf("read inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	requireAskBacklinkCount(t, countsByAskID(decodeBody[[]askBacklinkCount](t, inbox)), askID, 0)

	panel := dispatchRequest(t, handler, http.MethodGet, "/api/v1/references?to="+url.QueryEscape("dispatch://"+issue.Key+"/ask/"+askID), nil, "alice")
	if panel.Code != http.StatusOK {
		t.Fatalf("read ask backlinks: status=%d body=%s", panel.Code, panel.Body.String())
	}
	if edges := decodeBody[model.GraphReferences](t, panel).Edges; len(edges) != 0 {
		t.Fatalf("ask backlink edges = %#v, want none", edges)
	}

	// The thread is still reachable as a graph query — it is excluded from "referenced by",
	// not from the graph.
	replies := dispatchRequest(t, handler, http.MethodGet, "/api/v1/references?to="+url.QueryEscape("dispatch://"+issue.Key+"/ask/"+askID)+"&kind=replies_to", nil, "alice")
	if replies.Code != http.StatusOK {
		t.Fatalf("read ask replies: status=%d body=%s", replies.Code, replies.Body.String())
	}
	if edges := decodeBody[model.GraphReferences](t, replies).Edges; len(edges) != 2 {
		t.Fatalf("ask reply edges = %d, want 2", len(edges))
	}
}

// The reference grammar accepts any segment as an id, so a body can cite an ask that is not a
// uuid, a document that does not exist, or an issue nobody created. Indexing such a body must
// succeed and name nothing: a write never fails because of what its text cites, and a row
// stored that way stays editable.
func TestMalformedReferencesNeverFailAWrite(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	malformed := strings.Join([]string{
		"dispatch://" + issue.Key + "/ask/hello",
		"dispatch://" + issue.Key + "/comment/hello",
		"dispatch://" + issue.Key + "/message/hello",
		"dispatch://CORE/artifact/not-a-document",
		"dispatch://CORE/component/not-a-component",
		"dispatch://CORE-9999",
	}, " and ")

	message := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"actor": sessionActor(),
		"body":  "Citing " + malformed + ".",
	})
	if message.Code != http.StatusCreated {
		t.Fatalf("message citing malformed references: status=%d body=%s", message.Code, message.Body.String())
	}
	requireReferencesChanged(t, handler, issue.Key, "message.created", decodeBody[writtenRecord](t, message).ID, nil)

	comment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Also citing " + malformed + ".",
	}, "alice")
	if comment.Code != http.StatusCreated {
		t.Fatalf("comment citing malformed references: status=%d body=%s", comment.Code, comment.Body.String())
	}
	commentID := decodeBody[writtenRecord](t, comment).ID
	requireReferencesChanged(t, handler, issue.Key, "comment.created", commentID, nil)

	// The malformed targets are stored exactly as an earlier server stored them, so editing the
	// comment reconciles them — the case that made a pre-deploy comment uneditable.
	var stored int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from refs where from_kind = 'comment' and from_id = $1 and to_id = 'hello'
	`, commentID).Scan(&stored); err != nil {
		t.Fatalf("count stored malformed references: %v", err)
	}
	if stored != 3 {
		t.Fatalf("stored malformed references = %d, want 3 (ask, comment and message)", stored)
	}
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+commentID, map[string]any{
		"body": "Still citing " + malformed + ", plus a word.",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("edit a comment with malformed stored references: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+commentID, map[string]any{
		"body": "Nothing cited any more.",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("clear malformed stored references: status=%d body=%s", response.Code, response.Body.String())
	}
}

// A project document's body cites the same nodes a message can, and its readers hold the same
// batched counts, so the version event has to name what the write moved.
func TestDocumentVersionsNameChangedTargets(t *testing.T) {
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	askResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Should the document decide this?",
	}, "alice")
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	askID := decodeBody[writtenRecord](t, askResponse).ID
	issueKey := issue.Key

	document := createProjectDocument(t, handler, "CORE", "Decision record",
		"# Decision record\n\nWaiting on dispatch://"+issue.Key+"/ask/"+askID+".\n")

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+document.ID+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("read document events: status=%d body=%s", events.Code, events.Body.String())
	}
	named := false
	for _, event := range decodeBody[[]referenceWriteEvent](t, events) {
		if event.Type != "artifact.created" && event.Type != "artifact.version" {
			continue
		}
		if reflect.DeepEqual(event.Payload.ReferencesChanged, []model.ChangedReference{
			{Kind: "ask", ID: askID, IssueKey: &issueKey},
		}) {
			named = true
		}
	}
	if !named {
		t.Fatalf("no document event named the cited ask in %s", events.Body.String())
	}
}

// requireArtifactEventNames reads the newest artifact.* event for one document and compares the
// targets it names, so each producer is judged on the event it actually appends.
func requireArtifactEventNames(t *testing.T, handler http.Handler, path, artifactID string, want []model.ChangedReference) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read events: status=%d body=%s", response.Code, response.Body.String())
	}
	newest := referenceWriteEvent{}
	for _, event := range decodeBody[[]referenceWriteEvent](t, response) {
		if (event.Type == "artifact.version" || event.Type == "artifact.created") && event.Seq >= newest.Seq {
			newest = event
		}
	}
	if newest.Type == "" {
		t.Fatalf("no artifact event for %q in %s", artifactID, response.Body.String())
	}
	if !reflect.DeepEqual(newest.Payload.ReferencesChanged, want) {
		t.Fatalf("%s references_changed = %#v, want %#v", newest.Type, newest.Payload.ReferencesChanged, want)
	}
}

// Every path that writes a document version appends its own artifact.version event, and each has
// to name what the new markdown moved: the edits endpoint (both its unnamed snapshot and its
// named version, the route dispatch_doc_edit calls) and accepting a suggestion.
func TestDocumentWritePathsNameChangedTargets(t *testing.T) {
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	issueKey := issue.Key
	askIDs := make([]string, 0, 3)
	for _, question := range []string{"Cited by an edit?", "Cited by a named version?", "Cited by a suggestion?"} {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
			"question": question,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create ask %q: status=%d body=%s", question, response.Code, response.Body.String())
		}
		askIDs = append(askIDs, decodeBody[writtenRecord](t, response).ID)
	}
	document := createProjectDocument(t, handler, "CORE", "Decision record", "# Decision record\n\nPending.\n")
	events := "/api/v1/artifacts/" + document.ID + "/events"

	// The unnamed snapshot branch: POST /artifacts/{id}/edits with no summary.
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/edits", map[string]any{
		"ops": []map[string]string{
			{"op": "replace", "find": "Pending.", "with": "Waiting on dispatch://" + issue.Key + "/ask/" + askIDs[0] + "."},
		},
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("edit the document: status=%d body=%s", response.Code, response.Body.String())
	}
	requireArtifactEventNames(t, handler, events, document.ID, []model.ChangedReference{
		{Kind: "ask", ID: askIDs[0], IssueKey: &issueKey},
	})

	// The named-version branch of the same endpoint.
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/edits", map[string]any{
		"ops": []map[string]string{
			{"op": "insert", "markdown": "Also dispatch://" + issue.Key + "/ask/" + askIDs[1] + ".", "after": "Waiting on"},
		},
		"summary": "Cite the rollout decision",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("edit and name the document: status=%d body=%s", response.Code, response.Body.String())
	}
	requireArtifactEventNames(t, handler, events, document.ID, []model.ChangedReference{
		{Kind: "ask", ID: askIDs[1], IssueKey: &issueKey},
	})

	// Accepting a suggestion writes a named version of its own.
	suggestion := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/comments", map[string]any{
		"anchor":     map[string]any{"artifact": document.ID, "quote": "Decision record"},
		"body":       "Cite the third decision too.",
		"suggestion": map[string]any{"replace_with": "Blocked on dispatch://" + issue.Key + "/ask/" + askIDs[2] + "."},
	}, "alice")
	if suggestion.Code != http.StatusCreated {
		t.Fatalf("suggest a citation: status=%d body=%s", suggestion.Code, suggestion.Body.String())
	}
	suggestionID := decodeBody[writtenRecord](t, suggestion).ID
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+suggestionID+"/accept", nil, "alice"); response.Code != http.StatusOK {
		t.Fatalf("accept the suggestion: status=%d body=%s", response.Code, response.Body.String())
	}
	requireArtifactEventNames(t, handler, events, document.ID, []model.ChangedReference{
		{Kind: "ask", ID: askIDs[2], IssueKey: &issueKey},
	})
}

// An ask's question is a body like any other: opening one that cites an issue, and editing the
// citation away again, each move that issue's own backlink count, so both events name it.
func TestAskWritesNameChangedTargets(t *testing.T) {
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	target := createReferenceAPIIssue(t, handler, "CORE")
	source := createReferenceAPIIssue(t, handler, "OPS")

	opened := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+source.Key+"/asks", map[string]any{
		"question": "Does dispatch://" + target.Key + " ship first?",
	}, "alice")
	if opened.Code != http.StatusCreated {
		t.Fatalf("open a citing ask: status=%d body=%s", opened.Code, opened.Body.String())
	}
	askID := decodeBody[writtenRecord](t, opened).ID
	want := []model.ChangedReference{{Kind: "issue", ID: target.Key}}
	requireReferencesChanged(t, handler, source.Key, "ask.opened", askID, want)
	requireIssueBacklinkCount(t, handler, target.Key, 2)

	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
		"question": "Does anything ship first?",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("edit the citation away: status=%d body=%s", response.Code, response.Body.String())
	}
	requireReferencesChanged(t, handler, source.Key, "ask.edited", askID, want)
	requireIssueBacklinkCount(t, handler, target.Key, 1)
}

// POST /api/v1/issues seeds the new issue's spec document from the request and indexes it in the
// same transaction, so that body's citations move counted nodes: issue.created is the event that
// has to name them, or the ask card and the ask lists the reader already holds stay one short.
func TestIssueCreateNamesWhatItsSpecCited(t *testing.T) {
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	target := createReferenceAPIIssue(t, handler, "CORE")
	targetKey := target.Key
	askResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+target.Key+"/asks", map[string]any{
		"question": "Which rollout does the new work wait on?",
	}, "alice")
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	askID := decodeBody[writtenRecord](t, askResponse).ID

	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "OPS",
		"title":   "Ship the rollout",
		"spec": "# Ship the rollout\n\nBlocked on dispatch://" + target.Key + "/ask/" + askID +
			" and dispatch://" + target.Key + ".\n",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create citing issue: status=%d body=%s", response.Code, response.Body.String())
	}
	source := decodeBody[model.Issue](t, response)

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+source.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("read events: status=%d body=%s", events.Code, events.Body.String())
	}
	created := referenceWriteEvent{}
	for _, event := range decodeBody[[]referenceWriteEvent](t, events) {
		if event.Type == "issue.created" && event.Seq >= created.Seq {
			created = event
		}
	}
	if created.Type == "" {
		t.Fatalf("no issue.created event in %s", events.Body.String())
	}
	want := []model.ChangedReference{
		{Kind: "ask", ID: askID, IssueKey: &targetKey},
		{Kind: "issue", ID: target.Key},
	}
	if !reflect.DeepEqual(created.Payload.ReferencesChanged, want) {
		t.Fatalf("issue.created references_changed = %#v, want %#v", created.Payload.ReferencesChanged, want)
	}

	// The move the event reports is real: the cited ask's own count carries it.
	asks := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+target.Key+"/asks", nil, "alice")
	if asks.Code != http.StatusOK {
		t.Fatalf("read asks: status=%d body=%s", asks.Code, asks.Body.String())
	}
	requireAskBacklinkCount(t, countsByAskID(decodeBody[[]askBacklinkCount](t, asks)), askID, 1)
}
