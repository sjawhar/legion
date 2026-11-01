package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// holdIt is the spec's second callout, written with its id, hold-it.
func holdIt(body string) string {
	return ":::callout{#hold-it kind=\"note\" title=\"\"}\n" + body + "\n:::"
}

// readIssueDecisions is GET /api/v1/issues/{key}/decisions as alice.
func readIssueDecisions(t *testing.T, handler http.Handler, key string) issueDecisions {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/decisions", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/issues/%s/decisions: status=%d body=%s", key, response.Code, response.Body.String())
	}
	return decodeBody[issueDecisions](t, response)
}

// decisionNamed is the decision whose heading is heading, or a failure naming what the view holds.
func decisionNamed(t *testing.T, view issueDecisions, heading string) model.Decision {
	t.Helper()
	for _, decision := range view.Decisions {
		if decision.Heading == heading {
			return decision
		}
	}
	headings := make([]string, 0, len(view.Decisions))
	for _, decision := range view.Decisions {
		headings = append(headings, decision.Heading)
	}
	t.Fatalf("no decision %q; the view holds %v", heading, headings)
	return model.Decision{}
}

// staleTargets is each spec block a decision's links mark out of date, by block id.
func staleTargets(decision model.Decision) []string {
	stale := []string{}
	for _, link := range decision.Links {
		if link.Stale {
			stale = append(stale, link.To.BlockID)
		}
	}
	return stale
}

// linkTargets is each spec block a decision links, by block id, in the order the view lists them.
func linkTargets(decision model.Decision) []string {
	targets := make([]string, 0, len(decision.Links))
	for _, link := range decision.Links {
		targets = append(targets, link.To.BlockID)
	}
	return targets
}

// decisionsFixture is one issue whose spec holds two linkable blocks and whose decisions.md holds
// an agent decision linking both, a person's decision linking an answered ask and one of them, an
// unsure decision, and a replaced one.
type decisionsFixture struct {
	handler   http.Handler
	database  *store.Store
	deps      Deps
	issue     model.Issue
	decisions model.Artifact
	askID     string
}

const (
	tableDecision   = "Store notices in a table"
	nameDecision    = "Use the name the person gave"
	cacheDecision   = "Keep the cache warm"
	fileDecision    = "Store notices in a file"
	tableParagraph  = "Chosen for the simplest storage."
	changedTableSay = "Chosen for the simplest storage, after measuring."
)

func newDecisionsFixture(t *testing.T) decisionsFixture {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE",
		shipIt("Ship it.")+"\n\n"+holdIt("Hold it.")+"\n\nOther paragraph.\n")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which name do we use?",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("open the ask: status=%d body=%s", created.Code, created.Body.String())
	}
	askID := decodeBody[struct {
		ID string `json:"id"`
	}](t, created).ID
	spec := "dispatch://" + issue.Key + "/spec"
	markdown := fmt.Sprintf(`## %s

%s Shapes %s#b-ship%%5Fit and %s#b-hold-it.

## %s

Decided in dispatch://%s/ask/%s. Shapes %s#b-hold-it.

## %s

Unsure: a measurement of the hit rate would change this.

## Replaced

### %s

Replaced by the table above.
`, tableDecision, tableParagraph, spec, spec, nameDecision, issue.Key, askID, spec, cacheDecision, fileDecision)
	decisions := uploadIssueDocument(t, handler, issue.Key, "decisions.md", markdown)
	return decisionsFixture{handler: handler, database: database, deps: deps, issue: issue, decisions: decisions, askID: askID}
}

// specVersionCount is how many versions the issue's spec has.
func specVersionCount(t *testing.T, database *store.Store, artifactID string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&count); err != nil {
		t.Fatalf("count the spec's versions: %v", err)
	}
	return count
}

// The view reads decisions.md field by field: each `##` section in document order, the `###`
// sections under `## Replaced` marked replaced, a decision that mentions an ask the person's, a
// paragraph that starts `Unsure:` marking its decision unsure, and every link to a spec block.
// Right after the decisions are written no link is stale, and writing decisions.md wrote the spec
// no version of its own.
func TestIssueDecisionsReadsEachDecisionOfTheDocument(t *testing.T) {
	fixture := newDecisionsFixture(t)
	specVersions := specVersionCount(t, fixture.database, fixture.issue.PrimaryArtifactID)

	view := readIssueDecisions(t, fixture.handler, fixture.issue.Key)
	want := "dispatch://" + fixture.issue.Key + "/artifact/" + fixture.decisions.Slug
	if view.Document == nil || *view.Document != want {
		t.Fatalf("document = %v, want %q", view.Document, want)
	}
	headings := make([]string, 0, len(view.Decisions))
	for _, decision := range view.Decisions {
		headings = append(headings, decision.Heading)
	}
	wantHeadings := []string{tableDecision, nameDecision, cacheDecision, fileDecision}
	if fmt.Sprint(headings) != fmt.Sprint(wantHeadings) {
		t.Fatalf("headings = %v, want %v", headings, wantHeadings)
	}

	table := decisionNamed(t, view, tableDecision)
	if table.Kind != "agent" || table.Unsure || table.Replaced || len(table.Asks) != 0 {
		t.Fatalf("the agent decision = %#v, want an agent decision that is neither unsure nor replaced", table)
	}
	if table.Kept != nil || !table.WaitingReview {
		t.Fatalf("the agent decision's kept = %#v, waiting_review = %t, want null and true", table.Kept, table.WaitingReview)
	}
	if table.Ref != want+"#b-"+table.BlockID {
		t.Fatalf("ref = %q, want %q", table.Ref, want+"#b-"+table.BlockID)
	}
	if table.ChangedVersion != 1 || table.ChangedAt == nil {
		t.Fatalf("changed_version = %d, changed_at = %v, want 1 and the version's time", table.ChangedVersion, table.ChangedAt)
	}
	if got := fmt.Sprint(linkTargets(table)); got != "[hold-it ship_it]" && got != "[ship_it hold-it]" {
		t.Fatalf("the agent decision's links = %v, want both spec blocks", linkTargets(table))
	}
	if !strings.Contains(table.Text, tableParagraph) || !strings.Contains(table.Text, "## "+tableDecision) {
		t.Fatalf("text = %q, want the heading and its paragraph", table.Text)
	}

	name := decisionNamed(t, view, nameDecision)
	if name.Kind != "person" || len(name.Asks) != 1 || name.Asks[0] != fixture.askID {
		t.Fatalf("the person's decision = %#v, want kind person naming ask %s", name, fixture.askID)
	}
	if got := fmt.Sprint(linkTargets(name)); got != "[hold-it]" {
		t.Fatalf("the person's decision's links = %v, want hold-it", linkTargets(name))
	}
	if name.WaitingReview {
		t.Fatalf("a person's decision waits for no review: %#v", name)
	}

	cache := decisionNamed(t, view, cacheDecision)
	if !cache.Unsure || cache.Replaced || len(cache.Links) != 0 {
		t.Fatalf("the unsure decision = %#v, want unsure, not replaced, with no link", cache)
	}

	file := decisionNamed(t, view, fileDecision)
	if !file.Replaced || file.WaitingReview {
		t.Fatalf("the replaced decision = %#v, want replaced and waiting for no review", file)
	}

	for _, decision := range view.Decisions {
		if stale := staleTargets(decision); len(stale) != 0 {
			t.Fatalf("%q marks %v out of date right after the decisions were written", decision.Heading, stale)
		}
	}
	if got := specVersionCount(t, fixture.database, fixture.issue.PrimaryArtifactID); got != specVersions {
		t.Fatalf("the spec has %d versions, want %d: writing decisions.md writes the spec none", got, specVersions)
	}
}

// A link added to an existing decision is not out of date; a version that changes the decision's
// own text makes every link it already held out of date; and a later version of one linked spec
// block clears that link alone.
func TestDecisionLinksGoStaleWithTheDecisionAndClearWithTheirBlock(t *testing.T) {
	fixture := newDecisionsFixture(t)
	handler, key := fixture.handler, fixture.issue.Key

	// A link added to an existing decision: the paragraph that adds it is new, so the decision
	// changed in the version that wrote it, which is the version the new pair was created in.
	added := "Also shapes dispatch://" + key + "/spec#b-ship%5Fit."
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "insert", "markdown": added, "before": "## " + cacheDecision},
	})
	name := decisionNamed(t, readIssueDecisions(t, handler, key), nameDecision)
	for _, link := range name.Links {
		if link.To.BlockID == "ship_it" && link.Stale {
			t.Fatalf("the link a version added is out of date: %#v", link)
		}
	}

	// A version that changes the agent decision's own text: both links it already held are out of
	// date, the spec not having changed since.
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "replace", "find": tableParagraph, "with": changedTableSay},
	})
	table := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision)
	if got := len(staleTargets(table)); got != 2 {
		t.Fatalf("after the decision changed its links mark %v out of date, want both spec blocks", staleTargets(table))
	}

	// A later version of one of those spec blocks clears its link alone.
	editSpec(t, handler, key, []map[string]any{{"op": "replace", "find": "Ship it.", "with": "Ship it, soon."}})
	table = decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision)
	if got := fmt.Sprint(staleTargets(table)); got != "[hold-it]" {
		t.Fatalf("after the spec block changed the decision marks %v out of date, want hold-it alone", staleTargets(table))
	}
}

// Keep records a person's agreement at the decision's current version, which stops it waiting for
// review; a later change of the decision asks for review again. A bearer may not keep, and a block
// that is no decision heading is not a target.
func TestKeepRecordsAgreementUntilTheDecisionChanges(t *testing.T) {
	fixture := newDecisionsFixture(t)
	handler, key := fixture.handler, fixture.issue.Key
	table := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision)

	keepPath := "/api/v1/artifacts/" + fixture.decisions.ID + "/blocks/" + table.BlockID + "/keep"
	kept := dispatchRequest(t, handler, http.MethodPost, keepPath, map[string]any{}, "alice")
	if kept.Code != http.StatusOK {
		t.Fatalf("keep: status=%d body=%s", kept.Code, kept.Body.String())
	}
	keep := decodeBody[model.DecisionKeep](t, kept)
	if keep.BlockID != table.BlockID || keep.ChangedVersion != table.ChangedVersion || keep.KeptBy != "alice" {
		t.Fatalf("keep = %#v, want %s at version %d by alice", keep, table.BlockID, table.ChangedVersion)
	}
	after := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision)
	if after.Kept == nil || after.Kept.ChangedVersion != table.ChangedVersion || after.WaitingReview {
		t.Fatalf("after Keep the decision = %#v, want it kept and waiting for no review", after)
	}

	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "replace", "find": tableParagraph, "with": changedTableSay},
	})
	changed := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision)
	if changed.Kept == nil || !changed.WaitingReview {
		t.Fatalf("after the decision changed it = %#v, want it kept at the older version and waiting for review", changed)
	}

	bearer := sessionRequest(t, handler, http.MethodPost, keepPath, map[string]any{"actor": sessionActor()})
	if bearer.Code != http.StatusForbidden || !strings.Contains(bearer.Body.String(), "HUMAN_ONLY") {
		t.Fatalf("a bearer's keep: status=%d body=%s, want 403 HUMAN_ONLY", bearer.Code, bearer.Body.String())
	}

	blocks := blockIDsByText(t, handler, fixture.decisions.ID)
	paragraph := blocks[changedTableSay+" Shapes dispatch://"+key+"/spec#b-ship%5Fit and dispatch://"+key+"/spec#b-hold-it."]
	if paragraph == "" {
		t.Fatalf("the decision's paragraph has no block id; blocks = %v", blocks)
	}
	notHeading := dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+fixture.decisions.ID+"/blocks/"+paragraph+"/keep", map[string]any{}, "alice")
	if notHeading.Code != http.StatusNotFound || !strings.Contains(notHeading.Body.String(), "TARGET_NOT_FOUND") {
		t.Fatalf("keep on a block that is no decision heading: status=%d body=%s, want 404 TARGET_NOT_FOUND",
			notHeading.Code, notHeading.Body.String())
	}
}

// Each person's last read version of a document: null before they read it, the version they
// recorded after, and a bearer may read neither route.
func TestDocumentReadRecordsEachPersonsLastReadVersion(t *testing.T) {
	fixture := newDecisionsFixture(t)
	handler := fixture.handler
	path := "/api/v1/me/documents/" + fixture.decisions.ID + "/read"

	first := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
	if first.Code != http.StatusOK {
		t.Fatalf("read the read mark: status=%d body=%s", first.Code, first.Body.String())
	}
	if read := decodeBody[model.DocumentRead](t, first); read.Version != nil || read.ReadAt != nil {
		t.Fatalf("before any read the mark = %#v, want a null version", read)
	}

	stored := dispatchRequest(t, handler, http.MethodPut, path, map[string]any{"version": 1}, "alice")
	if stored.Code != http.StatusOK {
		t.Fatalf("record the read mark: status=%d body=%s", stored.Code, stored.Body.String())
	}
	if read := decodeBody[model.DocumentRead](t, stored); read.Version == nil || *read.Version != 1 {
		t.Fatalf("the recorded mark = %#v, want version 1", read)
	}
	again := decodeBody[model.DocumentRead](t, dispatchRequest(t, handler, http.MethodGet, path, nil, "alice"))
	if again.Version == nil || *again.Version != 1 || again.ReadAt == nil {
		t.Fatalf("the stored mark = %#v, want version 1 with its time", again)
	}
	if bob := decodeBody[model.DocumentRead](t, dispatchRequest(t, handler, http.MethodGet, path, nil, "bob")); bob.Version != nil {
		t.Fatalf("another person's mark = %#v, want null: the mark is one person's", bob)
	}

	for _, probe := range []struct {
		method string
		body   any
	}{
		{http.MethodGet, nil},
		{http.MethodPut, map[string]any{"version": 2, "actor": sessionActor()}},
	} {
		response := sessionRequest(t, handler, probe.method, path, probe.body)
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "HUMAN_ONLY") {
			t.Fatalf("a bearer's %s: status=%d body=%s, want 403 HUMAN_ONLY", probe.method, response.Code, response.Body.String())
		}
	}
}

// A decisions.md whose links the block index wrote at its first indexing marks nothing out of
// date, in both cases of a first indexing: the document's first version, written while it has no
// block rows, and the backfill over a document no block-aware build indexed.
func TestAFirstIndexingMarksNothingOutOfDate(t *testing.T) {
	fixture := newDecisionsFixture(t)
	handler, database, key := fixture.handler, fixture.database, fixture.issue.Key

	// The document's first version, written while it had no block rows: the fixture's own upload.
	for _, decision := range readIssueDecisions(t, handler, key).Decisions {
		if stale := staleTargets(decision); len(stale) != 0 {
			t.Fatalf("the first version's %q marks %v out of date", decision.Heading, stale)
		}
	}

	// The backfill: the state migration 0121 left a document no block-aware build indexed, then
	// one reconciliation tick, as the block index's own tests run it.
	ctx := context.Background()
	for _, statement := range []string{
		`delete from block_refs where artifact_id = $1`,
		`delete from document_blocks where artifact_id = $1`,
		`delete from document_block_texts where artifact_id = $1`,
		`delete from document_indexed_versions where artifact_id = $1`,
		`update document_block_versions set version = null where artifact_id = $1`,
	} {
		if _, err := database.Pool.Exec(ctx, statement, fixture.decisions.ID); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if _, err := fixture.deps.Docs.(*docs.Service).ReconcileBlockIndex(ctx, "", nil); err != nil {
		t.Fatalf("reconcile the block index: %v", err)
	}
	view := readIssueDecisions(t, handler, key)
	linked := 0
	for _, decision := range view.Decisions {
		linked += len(decision.Links)
		if stale := staleTargets(decision); len(stale) != 0 {
			t.Fatalf("after the backfill %q marks %v out of date", decision.Heading, stale)
		}
	}
	if linked == 0 {
		t.Fatalf("the backfill left no link to judge: %s", mustJSON(t, view))
	}
}

// The open questions are the issue's open asks whose block is in its spec, oldest first, and an
// issue with no decisions.md answers them with a null document and no decisions. The two asks are
// written in two versions, so one is older than the other: two that one settlement writes share
// their created_at and the order between them is their ids'.
func TestIssueDecisionsListTheSpecsOpenQuestions(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{settle: 20 * time.Millisecond})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", ":::ask{#decide_first}\nWhich store?\n:::\n\nA paragraph.\n")
	view, _ := waitForOpenQuestions(t, handler, issue.Key, 1)
	if view.Document != nil || len(view.Decisions) != 0 {
		t.Fatalf("an issue with no decisions.md = %#v, want a null document and no decisions", view)
	}

	editSpec(t, handler, issue.Key, []map[string]any{
		{"op": "insert", "after": "end", "markdown": ":::ask{#decide_second}\nWhich name?\n:::"},
	})
	_, questions := waitForOpenQuestions(t, handler, issue.Key, 2)
	if fmt.Sprint(questions) != "[Which store? Which name?]" {
		t.Fatalf("open_questions = %v, want the spec's two asks oldest first", questions)
	}
}

// waitForOpenQuestions reads the view until it holds want open questions: settlement indexes a
// spec's ask blocks, and until it has the issue holds none.
func waitForOpenQuestions(t *testing.T, handler http.Handler, key string, want int) (issueDecisions, []string) {
	t.Helper()
	var view issueDecisions
	questions := []string{}
	for range 200 {
		view = readIssueDecisions(t, handler, key)
		questions = questions[:0]
		for _, question := range view.OpenQuestions {
			questions = append(questions, question.Question)
		}
		if len(questions) == want {
			return view, questions
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("open_questions = %v, want %d of them", questions, want)
	return view, questions
}
func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(encoded)
}
