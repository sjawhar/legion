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

// A `#` heading after a decision ends it, as the block walk ends it, so text under that heading
// is no part of the decision and changing it marks nothing; and a heading the document carries
// twice - a decision made again after a replaced one of the same name - still names each of its
// decisions, so the live one's links go out of date with its own text.
func TestTheVersionCutReadsTheSameDecisionsAsTheBlockWalk(t *testing.T) {
	fixture := newDecisionsFixture(t)
	handler, key := fixture.handler, fixture.issue.Key

	// A `#` heading and a paragraph written after the agent decision, before the next one.
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "insert", "markdown": "# Notes\n\nA note.", "before": "## " + nameDecision},
	})
	table := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision)
	if strings.Contains(table.Text, "A note.") {
		t.Fatalf("the `#` heading did not end the decision: %q", table.Text)
	}
	if stale := staleTargets(table); len(stale) != 0 {
		t.Fatalf("a `#` heading written after the decision marks %v out of date", stale)
	}
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "replace", "find": "A note.", "with": "A changed note."},
	})
	if table = decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision); len(staleTargets(table)) != 0 {
		t.Fatalf("text under the `#` heading marks %v out of date", staleTargets(table))
	}

	// The same heading under `## Replaced`: the document now carries it twice, and the live
	// decision's own change still marks the blocks it links.
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "insert", "markdown": "### " + tableDecision + "\n\nThe file store we tried first.", "after": "## Replaced"},
	})
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "replace", "find": tableParagraph, "with": changedTableSay},
	})
	view := readIssueDecisions(t, handler, key)
	live := view.Decisions[0]
	if live.Heading != tableDecision || live.Replaced {
		t.Fatalf("the document's first decision = %#v, want the live %q", live, tableDecision)
	}
	if got := len(staleTargets(live)); got != 2 {
		t.Fatalf("beside a replaced decision of its own heading the live one marks %v out of date, want both spec blocks",
			staleTargets(live))
	}
}

// Keep records a person's agreement with the text they read, which stops the decision waiting for
// review; a later change of the decision asks for review again, and a Keep that carries the text
// before that change is refused. A bearer may not keep, and a block that is no decision heading
// is not a target.
func TestKeepRecordsAgreementUntilTheDecisionChanges(t *testing.T) {
	fixture := newDecisionsFixture(t)
	handler, key := fixture.handler, fixture.issue.Key
	table := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision)

	keepPath := "/api/v1/artifacts/" + fixture.decisions.ID + "/blocks/" + table.BlockID + "/keep"
	kept := dispatchRequest(t, handler, http.MethodPost, keepPath,
		map[string]any{"text_sha256": table.TextSHA256}, "alice")
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

	// The Keep of the text the person read before that change is refused, and writes nothing: a
	// Keep that stood would record an agreement with words nobody read.
	stale := dispatchRequest(t, handler, http.MethodPost, keepPath,
		map[string]any{"text_sha256": table.TextSHA256}, "alice")
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), "DECISION_CHANGED") {
		t.Fatalf("a Keep of the text before the change: status=%d body=%s, want 409 DECISION_CHANGED",
			stale.Code, stale.Body.String())
	}
	if again := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision); !again.WaitingReview {
		t.Fatalf("the refused Keep was recorded: %#v", again)
	}
	keepBlock(t, handler, fixture.decisions.ID, changed.BlockID, changed.TextSHA256)
	if now := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision); now.WaitingReview {
		t.Fatalf("after a Keep of the text as it stands the decision = %#v, want it kept", now)
	}

	// A Keep is the person's own agreement, so the route needs the person scope: the deployment's
	// shared token holds none and is refused, naming it.
	bearer := sessionRequest(t, handler, http.MethodPost, keepPath, map[string]any{"actor": sessionActor()})
	if bearer.Code != http.StatusForbidden || !strings.Contains(bearer.Body.String(), codeScopeRequired) {
		t.Fatalf("a bearer's keep: status=%d body=%s, want 403 %s", bearer.Code, bearer.Body.String(), codeScopeRequired)
	}

	blocks := blockIDsByText(t, handler, fixture.decisions.ID)
	paragraph := blocks[changedTableSay+" Shapes dispatch://"+key+"/spec#b-ship%5Fit and dispatch://"+key+"/spec#b-hold-it."]
	if paragraph == "" {
		t.Fatalf("the decision's paragraph has no block id; blocks = %v", blocks)
	}
	notHeading := dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+fixture.decisions.ID+"/blocks/"+paragraph+"/keep",
		map[string]any{"text_sha256": table.TextSHA256}, "alice")
	if notHeading.Code != http.StatusNotFound || !strings.Contains(notHeading.Body.String(), "TARGET_NOT_FOUND") {
		t.Fatalf("keep on a block that is no decision heading: status=%d body=%s, want 404 TARGET_NOT_FOUND",
			notHeading.Code, notHeading.Body.String())
	}
}

// keepBlock is a person's Keep of the decision that heading block opens, at the text sum the
// view served them.
func keepBlock(t *testing.T, handler http.Handler, artifactID, blockID, sum string) model.DecisionKeep {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+artifactID+"/blocks/"+blockID+"/keep",
		map[string]any{"text_sha256": sum}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("keep %s: status=%d body=%s", blockID, response.Code, response.Body.String())
	}
	return decodeBody[model.DecisionKeep](t, response)
}

// A kept decision whose text changes without its changed version moving waits for review again,
// and the spec blocks it linked are out of date. The version is the greatest of the blocks the
// decision holds now, so a block taken out of it, and one moved from it into another decision,
// leave both decisions' versions where they were while their text changed: the text, not the
// version, is what Keep records and what the out-of-date rule reads.
func TestAKeptDecisionWaitsForReviewAgainWhenABlockLeavesOrJoinsIt(t *testing.T) {
	fixture := newDecisionsFixture(t)
	handler, key, id := fixture.handler, fixture.issue.Key, fixture.decisions.ID

	// A paragraph written into the agent decision: its text changed after it linked both spec
	// blocks, so both are out of date, and a Keep agrees with the text as it now stands.
	const measured = "Measured against the file store."
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "insert", "markdown": measured, "before": "## " + nameDecision},
	})
	table := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision)
	if got := fmt.Sprint(staleTargets(table)); got != "[hold-it ship_it]" && got != "[ship_it hold-it]" {
		t.Fatalf("after a block joined it the decision marks %v out of date, want both spec blocks",
			staleTargets(table))
	}
	keepBlock(t, handler, id, table.BlockID, table.TextSHA256)
	if kept := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision); kept.WaitingReview {
		t.Fatalf("after the Keep the decision = %#v, want it waiting for no review", kept)
	}

	// The paragraph taken out again: the decision's text is the text it linked those blocks
	// with, so neither is out of date any more, while the Keep, which agreed with the
	// paragraph, no longer holds - and the decision's changed version moved for neither.
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "delete", "find": measured},
	})
	removed := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision)
	if strings.Contains(removed.Text, measured) {
		t.Fatalf("the paragraph is still in the decision: %q", removed.Text)
	}
	if removed.Kept == nil || removed.Kept.ChangedVersion < removed.ChangedVersion {
		t.Fatalf("the removal moved the decision's changed version: kept = %#v, decision = %#v",
			removed.Kept, removed)
	}
	if !removed.WaitingReview {
		t.Fatalf("after a block left it the decision = %#v, want it waiting for review", removed)
	}
	if stale := staleTargets(removed); len(stale) != 0 {
		t.Fatalf("the decision's text is what it linked those blocks with, and it marks %v", stale)
	}

	// Both decisions kept as they now stand, then the agent decision's paragraph moved into the
	// unsure one: no block of either changed, so neither changed version moves.
	view := readIssueDecisions(t, handler, key)
	table = decisionNamed(t, view, tableDecision)
	cache := decisionNamed(t, view, cacheDecision)
	keepBlock(t, handler, id, table.BlockID, table.TextSHA256)
	keepBlock(t, handler, id, cache.BlockID, cache.TextSHA256)
	blocks := blockIDsByText(t, handler, id)
	paragraph := blocks[tableParagraph+" Shapes dispatch://"+key+"/spec#b-ship%5Fit and dispatch://"+key+"/spec#b-hold-it."]
	if paragraph == "" {
		t.Fatalf("the decision's paragraph has no block id; blocks = %v", blocks)
	}
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "move", "block": paragraph, "before": "## Replaced"},
	})
	moved := readIssueDecisions(t, handler, key)
	left := decisionNamed(t, moved, tableDecision)
	joined := decisionNamed(t, moved, cacheDecision)
	if strings.Contains(left.Text, tableParagraph) || !strings.Contains(joined.Text, tableParagraph) {
		t.Fatalf("the move left the paragraph where it was: %q and %q", left.Text, joined.Text)
	}
	for _, decision := range []model.Decision{left, joined} {
		if decision.Kept == nil || decision.Kept.ChangedVersion < decision.ChangedVersion {
			t.Fatalf("the move moved %q's changed version: kept = %#v, decision = %#v",
				decision.Heading, decision.Kept, decision)
		}
		if !decision.WaitingReview {
			t.Fatalf("after the move %q = %#v, want it waiting for review", decision.Heading, decision)
		}
	}
	// The links moved with the paragraph, and both spec blocks are out of date under the
	// decision that holds them now: the decision they joined changed when they arrived.
	if got := fmt.Sprint(staleTargets(joined)); got != "[hold-it ship_it]" && got != "[ship_it hold-it]" {
		t.Fatalf("the decision the paragraph joined marks %v out of date, want both spec blocks",
			staleTargets(joined))
	}
	if len(left.Links) != 0 {
		t.Fatalf("the decision the paragraph left still holds %v", linkTargets(left))
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
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), codeScopeRequired) {
			t.Fatalf("a bearer's %s: status=%d body=%s, want 403 %s", probe.method, response.Code, response.Body.String(), codeScopeRequired)
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
