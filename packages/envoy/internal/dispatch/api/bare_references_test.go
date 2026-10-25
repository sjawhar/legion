package api

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// questionBlock is an ask block declaring id and asking question, as an author writes one.
func questionBlock(id, question string) string {
	return ":::ask{#" + id + ` urgency="med" multiple="false"}` + "\n" + question + "\n\n- Yes: Do it.\n- No: Wait.\n:::\n"
}

// blockQuestion is the id of the ask settlement made for the block id on document, once it has.
func blockQuestion(t *testing.T, database *store.Store, document, id string) string {
	t.Helper()
	var ask string
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		err := database.Pool.QueryRow(context.Background(), `
			select id::text from asks where block_artifact_id = $1 and block_id = $2
		`, document, id).Scan(&ask)
		if err == nil {
			return ask
		}
		if time.Now().After(deadline) {
			t.Fatalf("settlement never made the ask of block %q on %s: %v", id, document, err)
		}
	}
}

// citers is the dispatch:// address of every item GET /api/v1/references lists as mentioning
// target: its "Referenced by".
func citers(t *testing.T, handler http.Handler, target string) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	for _, edge := range graphEdges(t, handler, url.Values{"to": {target}}).Edges {
		if edge.Kind == "mentions" && edge.Direction == "in" {
			found[edge.Node.Ref] = true
		}
	}
	return found
}

// awaitCiter waits until target's "Referenced by" lists citer, for a citation a later version of a
// document makes once settlement writes it.
func awaitCiter(t *testing.T, handler http.Handler, target, citer string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !citers(t, handler, target)[citer]; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s never listed %s; it lists %v", target, citer, citers(t, handler, target))
		}
	}
}

func uploadIssueDocument(t *testing.T, handler http.Handler, issueKey, name, content string) model.Artifact {
	t.Helper()
	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": name, "content": content,
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload %s: status=%d body=%s", name, uploaded.Code, uploaded.Body.String())
	}
	return decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact
}

func postIssueComment(t *testing.T, handler http.Handler, issueKey string, body map[string]any) model.Comment {
	t.Helper()
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments", body, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("comment %v: status=%d body=%s", body, created.Code, created.Body.String())
	}
	return decodeBody[model.Comment](t, created)
}

// A2: with ACME-1's spec holding the settled question blocks q1 and q2, `#q2` written later lists
// under q2's ask from every text in the spec's scope - a later version of the spec itself, a reply
// to q1, a comment anchored to the spec and a reply to it, and an issue comment and an issue
// message, which belong to no document and resolve against the spec. A record copy of the spec
// shows q2 as a copy, and `#q2` there lists under the original q2. A second document of the issue
// declaring its own q2 is the negative control: each citation lists under its own document's
// question only.
func TestQuestionIDsListUnderTheQuestionOfTheirOwnDocument(t *testing.T) {
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	issue := createInteractionIssue(t, handler, "ACME", "Questions",
		"Context\n\n"+questionBlock("q1", "Ship on Monday?")+"\n"+questionBlock("q2", "Ship the docs too?"))
	spec := issue.PrimaryArtifactID
	q1 := blockQuestion(t, database, spec, "q1")
	q2 := blockQuestion(t, database, spec, "q2")
	q2Ref := "dispatch://" + issue.Key + "/ask/" + q2
	commentRef := func(comment model.Comment) string { return "dispatch://" + issue.Key + "/comment/" + comment.ID }

	if edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+spec+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops":   []map[string]string{{"op": "replace", "find": "Context", "with": "Context, decided by #q2."}},
	}); edited.Code != http.StatusOK {
		t.Fatalf("cite q2 in the spec: status=%d body=%s", edited.Code, edited.Body.String())
	}
	reply := postIssueComment(t, handler, issue.Key, map[string]any{"body": "Only after #q2.", "ask_id": q1})
	anchored := postIssueComment(t, handler, issue.Key, map[string]any{
		"body": "This hangs on #q2.", "anchor": map[string]any{"artifact": "spec", "quote": "Context"},
	})
	anchoredReply := postIssueComment(t, handler, issue.Key, map[string]any{"body": "Agreed, see #q2.", "reply_to": anchored.ID})
	issueComment := postIssueComment(t, handler, issue.Key, map[string]any{"body": "Settled by #q2."})
	messaged := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Per #q2, go ahead.", "actor": sessionActor(),
	})
	if messaged.Code != http.StatusCreated {
		t.Fatalf("message the issue: status=%d body=%s", messaged.Code, messaged.Body.String())
	}
	message := decodeBody[model.Message](t, messaged)

	record := uploadIssueDocument(t, handler, issue.Key, "spec-record.md", documentMarkdown(t, handler, spec))
	awaitDocumentText(t, handler, record.ID, "q2 as a copy", contains(`copied_from="`+q2+`"`), nil)
	recordComment := postIssueComment(t, handler, issue.Key, map[string]any{
		"body": "The record keeps #q2.", "anchor": map[string]any{"artifact": record.ID, "quote": "Context"},
	})

	other := uploadIssueDocument(t, handler, issue.Key, "rollout.md", "Rollout.\n\n"+questionBlock("q2", "Roll out by region?"))
	otherQ2 := blockQuestion(t, database, other.ID, "q2")
	otherQ2Ref := "dispatch://" + issue.Key + "/ask/" + otherQ2
	if edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+other.ID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops":   []map[string]string{{"op": "replace", "find": "Rollout.", "with": "Rollout, decided by #q2."}},
	}); edited.Code != http.StatusOK {
		t.Fatalf("cite q2 in the rollout: status=%d body=%s", edited.Code, edited.Body.String())
	}
	otherComment := postIssueComment(t, handler, issue.Key, map[string]any{
		"body": "Here #q2 is the rollout's.", "anchor": map[string]any{"artifact": other.ID, "quote": "Rollout"},
	})

	specRef := "dispatch://" + issue.Key + "/spec"
	recordRef := "dispatch://" + issue.Key + "/artifact/" + record.Slug
	otherRef := "dispatch://" + issue.Key + "/artifact/" + other.Slug
	awaitCiter(t, handler, q2Ref, recordRef)
	awaitCiter(t, handler, otherQ2Ref, otherRef)

	underQ2 := citers(t, handler, q2Ref)
	for name, citer := range map[string]string{
		"the spec's later version":         specRef,
		"a reply to q1":                    commentRef(reply),
		"a comment anchored to the spec":   commentRef(anchored),
		"a reply to the anchored comment":  commentRef(anchoredReply),
		"an issue comment":                 commentRef(issueComment),
		"an issue message":                 "dispatch://" + issue.Key + "/message/" + message.ID,
		"the record copy's text":           recordRef,
		"a comment anchored to the record": commentRef(recordComment),
	} {
		if !underQ2[citer] {
			t.Errorf("q2 does not list %s (%s); it lists %v", name, citer, underQ2)
		}
	}
	for name, citer := range map[string]string{
		"the rollout's text":                otherRef,
		"a comment anchored to the rollout": commentRef(otherComment),
	} {
		if underQ2[citer] {
			t.Errorf("q2 lists %s (%s), which cites the rollout's own q2", name, citer)
		}
	}
	underOther := citers(t, handler, otherQ2Ref)
	if len(underOther) != 2 || !underOther[otherRef] || !underOther[commentRef(otherComment)] {
		t.Errorf("the rollout's q2 lists %v; want the rollout's text and the comment anchored to it only", underOther)
	}
	// The spec's mention is excerpted at the block the `#q2` is written in, as a dispatch://
	// address's would be.
	for _, edge := range graphEdges(t, handler, url.Values{"to": {q2Ref}}).Edges {
		if edge.Node.Ref == specRef && (edge.Excerpt == nil || edge.Excerpt.BlockID == "" || !strings.Contains(edge.Excerpt.Text, "Context, decided by #q2.")) {
			t.Errorf("the spec's mention of q2 has excerpt %#v; want the paragraph citing it", edge.Excerpt)
		}
	}

	// A rebuild of the whole index from the stored text reads `#q2` by the same rule.
	if _, err := database.Pool.Exec(context.Background(), `delete from refs`); err != nil {
		t.Fatalf("empty the index: %v", err)
	}
	if _, err := refs.RebuildAll(context.Background(), database.Pool, "https://dispatch.example"); err != nil {
		t.Fatalf("rebuild the index: %v", err)
	}
	if rebuilt := citers(t, handler, q2Ref); !reflect.DeepEqual(rebuilt, underQ2) {
		t.Errorf("after a rebuild q2 lists %v; want %v", rebuilt, underQ2)
	}
	if rebuilt := citers(t, handler, otherQ2Ref); !reflect.DeepEqual(rebuilt, underOther) {
		t.Errorf("after a rebuild the rollout's q2 lists %v; want %v", rebuilt, underOther)
	}
}

// A2's same edit, A5's drain: one write of a spec declares q1, saying "see #q2", and q2. Settlement
// makes q1's row and indexes its question before it makes q2's, so q1's `#q2` names nothing yet;
// q2's row owes the spec as a scope, and one tick of the re-index lists q1 under q2, beside the
// spec's own text. The negative control: a second document declaring its own q2 in the upload
// that cites it lists under that q2 only after the tick, never under the spec's.
func TestQuestionIDsWrittenWithTheirQuestionListAfterOneReindexTick(t *testing.T) {
	handler, database, documents := settlingCarryDocuments(t, 20*time.Millisecond)
	service, ok := documents.(*docs.Service)
	if !ok {
		t.Fatalf("the document service is %T; want *docs.Service", documents)
	}
	issue := createInteractionIssue(t, handler, "ACME", "Questions in one edit",
		"Context.\n\n"+questionBlock("q1", "Ship on Monday? See #q2.")+"\n"+questionBlock("q2", "Ship the docs too?"))
	spec := issue.PrimaryArtifactID
	q1 := blockQuestion(t, database, spec, "q1")
	q2 := blockQuestion(t, database, spec, "q2")
	other := uploadIssueDocument(t, handler, issue.Key, "rollout.md", "Rollout, see #q2.\n\n"+questionBlock("q2", "Roll out by region?"))
	otherQ2 := blockQuestion(t, database, other.ID, "q2")

	q1Ref := "dispatch://" + issue.Key + "/ask/" + q1
	q2Ref := "dispatch://" + issue.Key + "/ask/" + q2
	otherQ2Ref := "dispatch://" + issue.Key + "/ask/" + otherQ2
	specRef := "dispatch://" + issue.Key + "/spec"
	otherRef := "dispatch://" + issue.Key + "/artifact/" + other.Slug
	if citers(t, handler, q2Ref)[q1Ref] {
		t.Fatalf("q2 lists q1 before the re-index; settlement indexed q1 after q2's row exists, so this test no longer shows the drain")
	}

	if _, _, err := service.ReindexReferences(context.Background()); err != nil {
		t.Fatalf("re-index tick: %v", err)
	}
	underQ2 := citers(t, handler, q2Ref)
	if !underQ2[q1Ref] || !underQ2[specRef] {
		t.Errorf("q2 lists %v after one tick; want q1 (%s) and the spec (%s)", underQ2, q1Ref, specRef)
	}
	if underQ2[otherRef] {
		t.Errorf("q2 lists the rollout (%s), which cites its own q2", otherRef)
	}
	if underOther := citers(t, handler, otherQ2Ref); len(underOther) != 1 || !underOther[otherRef] {
		t.Errorf("the rollout's q2 lists %v after one tick; want the rollout's text alone", underOther)
	}
}

// A3: a comment naming an issue key bare stores an edge to that issue, which lists it under
// "Referenced by"; a key whose issue does not exist, a key-shaped word such as UTF-8, and a key
// inside a dashboard address (stored once, as that address) store nothing more.
func TestBareIssueKeysStoreAnEdgeOnlyToAnIssueThatExists(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "ACME")
	source := createReferenceAPIIssue(t, handler, "ACME")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "ACME", "title": "Migrate the billing cluster",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the blocking issue: status=%d body=%s", created.Code, created.Body.String())
	}
	if blocker := decodeBody[model.Issue](t, created); blocker.Key != "ACME-2" {
		t.Fatalf("second issue = %s; want ACME-2", blocker.Key)
	}
	blocked := createReferenceAPIComment(t, handler, source.Key, "blocked on ACME-2")
	missing := createReferenceAPIComment(t, handler, source.Key, "blocked on ACME-999999")
	word := createReferenceAPIComment(t, handler, source.Key, "Encode it as UTF-8.")
	dashboard := createReferenceAPIComment(t, handler, source.Key, "See https://dispatch.example/issues/ACME-2.")

	edges := func(comment model.Comment) []string {
		t.Helper()
		rows, err := database.Pool.Query(context.Background(), `
			select to_kind || ' ' || to_id from refs where from_kind = 'comment' and from_id = $1 order by 1
		`, comment.ID)
		if err != nil {
			t.Fatalf("read the edges of %s: %v", comment.Body, err)
		}
		defer rows.Close()
		found := []string{}
		for rows.Next() {
			var edge string
			if err := rows.Scan(&edge); err != nil {
				t.Fatalf("scan an edge: %v", err)
			}
			found = append(found, edge)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate the edges of %s: %v", comment.Body, err)
		}
		return found
	}
	for _, tc := range []struct {
		comment model.Comment
		want    string
	}{
		{blocked, "issue ACME-2"},
		{missing, ""},
		{word, ""},
		{dashboard, "issue ACME-2"},
	} {
		if got := strings.Join(edges(tc.comment), ", "); got != tc.want {
			t.Errorf("edges of %q = %q; want %q", tc.comment.Body, got, tc.want)
		}
	}
	if listed := citers(t, handler, "dispatch://ACME-2"); !listed["dispatch://"+source.Key+"/comment/"+blocked.ID] {
		t.Errorf("ACME-2 lists %v; want the comment blocked on it", listed)
	}
}
