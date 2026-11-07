package api

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/texttrees"
)

// requireStoredTree fails unless the table holds the display tree of text, in the mode a read
// serves, as the bytes pmdoc.DisplayJSON answers: what the write stored is what a read would have
// built, so a read after a restart serves it in place of reading the markdown again.
func requireStoredTree(t *testing.T, database *store.Store, what, text string) {
	t.Helper()
	if text == "" {
		t.Fatalf("%s: the test's text is empty, which has no tree and stores no row", what)
	}
	stored, err := texttrees.New(database.Pool).Read(
		context.Background(), pmdoc.DisplayLatest, [][32]byte{texttrees.Sum(text)},
	)
	if err != nil {
		t.Fatalf("%s: read the stored tree: %v", what, err)
	}
	tree, held := stored[texttrees.Sum(text)]
	if !held {
		t.Fatalf("%s: the table holds no display tree of %q", what, text)
	}
	if want := pmdoc.DisplayJSON(text, pmdoc.DisplayLatest); !bytes.Equal(tree, want) {
		t.Errorf("%s: the stored tree of %q is\n\t%s\nwant\n\t%s", what, text, tree, want)
	}
}

// Every write of a stored text stores that text's display tree, in the one request that wrote it:
// a comment, its edit, a message, an ask with its options, an ask's edit, an answer, a
// resolution's reason, a review's reason and a document version's markdown. Each is driven through
// the routes a client calls, so what the row is compared with is what the write stored.
func TestEveryWriteOfAStoredTextStoresItsDisplayTree(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	const (
		commentBody   = "Check **this** before the deploy."
		editedBody    = "Check **that** before the deploy, and the *rollback* too."
		replyBody     = "Checked: the deploy is `green`."
		messageBody   = "The *staging* run is green."
		askQuestion   = "Ship the ~~draft~~ **release** today?"
		optionLabel   = "Yes, `ship it`"
		optionDetail  = "Ships the release *today*."
		editedOption  = "Yes, ship it **now**"
		editedAsk     = "Ship the **release** today, or wait?"
		answerText    = "Shipping **now**."
		resolvedAsk   = "Should the *rollback* be rehearsed?"
		resolveReason = "Rehearsed in the **last** deploy."
		reviewReason  = "The *rollout* section needs the rollback step."
		spec          = "# Spec\n\nThe **plan** is written here.\n"
		namedSummary  = "the plan as it stands"
	)
	issue := createInteractionIssue(t, handler, "TREES", "Stored display trees", spec)

	t.Run("a comment create", func(t *testing.T) {
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
			map[string]any{"body": commentBody}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create the comment: status=%d body=%s", created.Code, created.Body.String())
		}
		comment := decodeBody[model.Comment](t, created)
		requireStoredTree(t, database, "a comment create", comment.Body)

		t.Run("its edit", func(t *testing.T) {
			edited := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID,
				map[string]any{"body": editedBody}, "alice")
			if edited.Code != http.StatusOK {
				t.Fatalf("edit the comment: status=%d body=%s", edited.Code, edited.Body.String())
			}
			requireStoredTree(t, database, "a comment edit", decodeBody[model.Comment](t, edited).Body)
		})

		t.Run("a reply to it", func(t *testing.T) {
			replied := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
				map[string]any{"body": replyBody, "reply_to": comment.ID}, "bob")
			if replied.Code != http.StatusCreated {
				t.Fatalf("reply to the comment: status=%d body=%s", replied.Code, replied.Body.String())
			}
			requireStoredTree(t, database, "a comment reply", decodeBody[model.Comment](t, replied).Body)
		})
	})

	t.Run("a message create", func(t *testing.T) {
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages",
			map[string]any{"body": messageBody}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create the message: status=%d body=%s", created.Code, created.Body.String())
		}
		requireStoredTree(t, database, "a message create", decodeBody[model.Message](t, created).Body)
	})

	t.Run("an ask create, its edit and its answer", func(t *testing.T) {
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
			"question": askQuestion,
			"options":  []map[string]string{{"label": optionLabel, "description": optionDetail}},
		}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create the ask: status=%d body=%s", created.Code, created.Body.String())
		}
		ask := decodeBody[model.Ask](t, created)
		requireStoredTree(t, database, "an ask's question", ask.Question)
		requireStoredTree(t, database, "an ask option's label", ask.Options[0].Label)
		requireStoredTree(t, database, "an ask option's description", ask.Options[0].Description)

		edited := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+ask.ID, map[string]any{
			"question": editedAsk,
			"options":  []map[string]string{{"label": editedOption}},
		}, "alice")
		if edited.Code != http.StatusOK {
			t.Fatalf("edit the ask: status=%d body=%s", edited.Code, edited.Body.String())
		}
		reworded := decodeBody[model.Ask](t, edited)
		requireStoredTree(t, database, "an edited ask's question", reworded.Question)
		requireStoredTree(t, database, "an edited ask option's label", reworded.Options[0].Label)
		// The question and options the edit replaced hold no row of their own, and every read of
		// the ask's history is shown them.
		requireStoredTree(t, database, "the question an ask edit replaced", askQuestion)
		requireStoredTree(t, database, "the option label an ask edit replaced", optionLabel)

		// The answer names the wording it was given, as a person's browser does: an ask edited
		// since the reader read it is refused (ASK_EDITED).
		answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
			"selected": []string{reworded.Options[0].Label}, "text": answerText,
			"expected_edited_at": reworded.EditedAt,
		}, "alice")
		if answered.Code != http.StatusOK {
			t.Fatalf("answer the ask: status=%d body=%s", answered.Code, answered.Body.String())
		}
		requireStoredTree(t, database, "an ask answer's text", answerText)
	})

	t.Run("an ask resolve", func(t *testing.T) {
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks",
			map[string]any{"question": resolvedAsk}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create the ask to resolve: status=%d body=%s", created.Code, created.Body.String())
		}
		ask := decodeBody[model.Ask](t, created)
		resolved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/resolve",
			map[string]any{"kind": "resolved", "reason": resolveReason}, "alice")
		if resolved.Code != http.StatusOK {
			t.Fatalf("resolve the ask: status=%d body=%s", resolved.Code, resolved.Body.String())
		}
		requireStoredTree(t, database, "a resolution's reason", resolveReason)
	})

	t.Run("a review with a reason", func(t *testing.T) {
		reviewed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/reviews",
			map[string]any{"state": "changes_requested", "reason": reviewReason}, "alice")
		if reviewed.Code != http.StatusCreated {
			t.Fatalf("review the document: status=%d body=%s", reviewed.Code, reviewed.Body.String())
		}
		requireStoredTree(t, database, "a review's reason", reviewReason)
	})

	t.Run("a document version write", func(t *testing.T) {
		// The issue's own spec document, written when the issue was created, and the named
		// version this writes over it: both are version writes of the same markdown, and the row
		// is the markdown the document stores, not the text the request sent.
		markdown := documentMarkdown(t, handler, issue.PrimaryArtifactID)
		requireStoredTree(t, database, "the spec a new issue stored", markdown)
		named := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions",
			map[string]string{"summary": namedSummary}, "alice")
		if named.Code != http.StatusCreated {
			t.Fatalf("write a named version: status=%d body=%s", named.Code, named.Body.String())
		}
		requireStoredTree(t, database, "a named version's markdown", documentMarkdown(t, handler, issue.PrimaryArtifactID))
	})

	t.Run("a document upload", func(t *testing.T) {
		uploaded := uploadIssueDocument(t, handler, issue.Key, "Design notes.md", "# Design\n\nThe **notes** are here.\n")
		requireStoredTree(t, database, "an uploaded document's markdown", documentMarkdown(t, handler, uploaded.ID))
	})
}

// A broadcast is one text however many recipients it reaches: every copy holds the body the send
// was made with, and one row holds its tree.
func TestABroadcastStoresTheOneTreeOfItsBody(t *testing.T) {
	const body = "Status, please: the **deploy** is held."
	listener, _ := newBroadcastListener(t, broadcastSessions)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	sent := postBroadcast(t, handler, map[string]any{
		"body": body, "delivery": "steer", "session_ids": []string{"planner", "tester"},
	}, "alice")
	if sent.Code != http.StatusCreated {
		t.Fatalf("send the broadcast: status=%d body=%s", sent.Code, sent.Body.String())
	}
	created := decodeBody[broadcastResponse](t, sent)
	awaitBroadcastDeliveries(t, handler, created.ID)
	requireStoredTree(t, database, "a broadcast's body", body)
	sum := texttrees.Sum(body)
	var rows int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from text_trees where sum = $1`, sum[:]).Scan(&rows); err != nil {
		t.Fatalf("count the broadcast's rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("a broadcast to two sessions stored %d rows for one body, want 1", rows)
	}
}

// The publish path stores the tree of every text an event's payload carries, under the keys a
// reader reads them by (eventDisplayTrees): an edit's previous question and its option labels
// have no row of their own, and a reader of the ask's history is shown them.
//
// It calls the publish path's store rather than a route, because a route's own store writes the
// same rows: this is the reading of the payload, and the shape it answers for is the shape a read
// of that event answers.
func TestThePublishPathStoresTheTreesOfAnEventsPayloadTexts(t *testing.T) {
	_, database, deps := newTestServer(t, testServerOptions{})
	const (
		question    = "Ship the **release** today, as the event carries it?"
		label       = "Yes, `ship it`"
		previous    = "Ship the ~~draft~~ today?"
		previousTwo = "Wait for the *rehearsal*"
		answer      = "Shipping, as the event says."
	)
	directServer(deps).storeEventDisplayTrees(context.Background(), []model.Event{{
		Type: "ask.edited",
		Payload: map[string]any{
			"question": question,
			"options":  []map[string]string{{"label": label}},
			"previous": map[string]any{
				"question": previous,
				"options":  []map[string]string{{"label": previousTwo}},
			},
			"previous_answer": map[string]string{"text": answer},
		},
	}})
	for what, text := range map[string]string{
		"the question an event carries":         question,
		"an option label an event carries":      label,
		"the question an edit replaced":         previous,
		"the option label an edit replaced":     previousTwo,
		"the answer a second answer superseded": answer,
	} {
		requireStoredTree(t, database, what, text)
	}
}

// A write whose tree cannot be stored still succeeds: the store runs after the commit, and a
// failure is logged and changes no answer. The table is dropped, which is every failure the store
// can meet - a refused statement - as a caller sees it.
func TestAWriteWhoseTreeCannotBeStoredStillSucceeds(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "NOTREE", "A store that fails", "# Spec\n\nWritten here.\n")
	if _, err := database.Pool.Exec(context.Background(), `drop table text_trees`); err != nil {
		t.Fatalf("drop the table the store writes: %v", err)
	}
	const body = "The deploy is **held** until the rollback is rehearsed."
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
		map[string]any{"body": body}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create a comment with no table to store its tree: status=%d body=%s", created.Code, created.Body.String())
	}
	if stored := decodeBody[model.Comment](t, created).Body; stored != body {
		t.Errorf("the comment stored %q, want %q", stored, body)
	}
	// And the read answers the tree all the same, built from the markdown as it always was.
	requireDisplayTree(t, handler, "/api/v1/issues/"+issue.Key+"/comments", "0.body", "0.body_tree")
}
