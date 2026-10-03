package api

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func createIssueRequest(t *testing.T, handler http.Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
}

func createDuplicateTestProject(t *testing.T, handler http.Handler, key string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": key, "name": key + " project",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create project %q: status=%d body=%s", key, response.Code, response.Body.String())
	}
}

func requireCreatedIssue(t *testing.T, response *httptest.ResponseRecorder) model.Issue {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

func TestCreateIssueRejectsNearDuplicateTitleWithCandidates(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	first := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Global search across issues and documents",
	}))
	second := createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Global search for issues and documents",
	})
	if second.Code != http.StatusConflict {
		t.Fatalf("near-duplicate issue: status=%d body=%s", second.Code, second.Body.String())
	}
	body := decodeBody[struct {
		Code       string                     `json:"code"`
		Candidates []model.DuplicateCandidate `json:"candidates"`
	}](t, second)
	if body.Code != "POSSIBLE_DUPLICATE" {
		t.Fatalf("duplicate code = %q, want POSSIBLE_DUPLICATE", body.Code)
	}
	if len(body.Candidates) != 1 {
		t.Fatalf("candidate count = %d, want 1: %#v", len(body.Candidates), body.Candidates)
	}
	candidate := body.Candidates[0]
	if candidate.Key != first.Key || candidate.Title != "Global search across issues and documents" ||
		candidate.Status != "triage" || candidate.SharedTerms != 4 ||
		candidate.Snippet != "<mark>Global</mark> <mark>search</mark> across <mark>issues</mark> and <mark>documents</mark>" ||
		candidate.Href != "/issues/"+first.Key {
		t.Fatalf("candidate = %#v", candidate)
	}

	third := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Unrelated",
	}))
	if third.Key != "TEST-2" {
		t.Fatalf("issue created after refusal = %q, want TEST-2", third.Key)
	}
}

func TestCreateIssueForceBypassesTheGate(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Global search across issues and documents",
	}))
	forced := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Global search for issues and documents", "force": true,
	}))
	if forced.Key != "TEST-2" {
		t.Fatalf("forced issue key = %q, want TEST-2", forced.Key)
	}
}

func TestCreateIssueGateIgnoresParentTitleTerms(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	parent := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Dispatch global search",
	}))
	childA := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "parent": parent.Key, "title": "Dispatch global search: server",
	}))
	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "parent": parent.Key, "title": "Dispatch global search: SPA",
	}))
	duplicate := createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "parent": parent.Key, "title": "Dispatch global search: server",
	})
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate child: status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	body := decodeBody[struct {
		Candidates []model.DuplicateCandidate `json:"candidates"`
	}](t, duplicate)
	if len(body.Candidates) != 1 || body.Candidates[0].Key != childA.Key {
		t.Fatalf("duplicate child candidates = %#v, want %q", body.Candidates, childA.Key)
	}
}

func TestCreateIssueGateIsScopedToProjectAndSkipsExternal(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")
	createDuplicateTestProject(t, handler, "OTHER")

	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Global search across issues and documents",
	}))
	other := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "OTHER", "title": "Global search across issues and documents",
	}))
	if other.Key != "OTHER-1" {
		t.Fatalf("cross-project issue key = %q, want OTHER-1", other.Key)
	}
	mapping := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": "TEST",
	}, "alice")
	if mapping.Code != http.StatusOK {
		t.Fatalf("map external repository: status=%d body=%s", mapping.Code, mapping.Body.String())
	}
	external := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"external": "owner/repo#41", "title": "Global search across issues and documents",
	}))
	if external.Key != "TEST-2" {
		t.Fatalf("external issue key = %q, want TEST-2", external.Key)
	}
}

func TestCreateIssueGateIncludesClosedIssues(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	first := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration notes",
	}))
	closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+first.Key, map[string]string{
		"status": "done",
	}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", closed.Code, closed.Body.String())
	}
	duplicate := createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe",
	})
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate closed issue: status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	body := decodeBody[struct {
		Candidates []model.DuplicateCandidate `json:"candidates"`
	}](t, duplicate)
	if len(body.Candidates) != 1 || body.Candidates[0].Key != first.Key || body.Candidates[0].Status != "done" {
		t.Fatalf("closed duplicate candidates = %#v", body.Candidates)
	}
}

func TestCreateIssueGateRule(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Global search across issues and documents",
	}))
	threeShared := createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Global search issues title",
	})
	if threeShared.Code != http.StatusConflict {
		t.Fatalf("three shared terms issue: status=%d body=%s", threeShared.Code, threeShared.Body.String())
	}
	oneShared := createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Search palette loses focus on phone",
	})
	if oneShared.Code != http.StatusCreated {
		t.Fatalf("one shared term issue: status=%d body=%s", oneShared.Code, oneShared.Body.String())
	}
	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Red-teamer hiring test on the platform: candidate attack authoring and live results",
	}))
	fourShared := createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Chief of staff: red-teamer ops, onboarding, weekly check-ins, contractor comms, platform access",
	})
	if fourShared.Code != http.StatusCreated {
		t.Fatalf("four shared terms in long title: status=%d body=%s", fourShared.Code, fourShared.Body.String())
	}
}

func TestCreateIssueOrdersDuplicateCandidates(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "ORDER")

	older := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "ORDER", "title": "Global search issues documents alpha", "force": true,
	}))
	newer := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "ORDER", "title": "Global search issues documents beta", "force": true,
	}))
	highest := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "ORDER", "title": "Global search issues documents alpha beta", "force": true,
	}))
	duplicate := createIssueRequest(t, handler, map[string]any{
		"project": "ORDER", "title": "Global search issues documents alpha beta gamma",
	})
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("ordered candidates: status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	body := decodeBody[struct {
		Candidates []model.DuplicateCandidate `json:"candidates"`
	}](t, duplicate)
	if len(body.Candidates) != 3 || body.Candidates[0].Key != highest.Key || body.Candidates[1].Key != newer.Key || body.Candidates[2].Key != older.Key {
		t.Fatalf("candidate ordering = %#v, want %q, %q, then %q", body.Candidates, highest.Key, newer.Key, older.Key)
	}
}

// duplicateQueryFromTitles is the duplicate check as it ran before migration 0069 stored each
// title's lexemes: every title's, the parent's included, built from its text on each creation.
// issues.title_lexemes holds what this built, so the check must answer from it what this answers
// from the titles.
const duplicateQueryFromTitles = `
with parent as (select coalesce((select title from issues where key = $3), '') as title),
new_title as (
  select array(select unnest(tsvector_to_array(search_vector('', search_text($2))))
               except select unnest(tsvector_to_array(search_vector('', search_text(p.title))))) as lex
    from parent p),
cand as (
  select i.key, i.title, i.status, i.updated_at,
         array(select unnest(tsvector_to_array(search_vector('', search_text(i.title))))
               except select unnest(tsvector_to_array(search_vector('', search_text(p.title))))) as lex
    from issues i, parent p
   where i.project_key = $1 and i.key <> $3),
scored as (
  select c.key, c.title, c.status, c.updated_at,
         array(select unnest(c.lex) intersect select unnest(n.lex)) as shared_lex,
         least(cardinality(c.lex), cardinality(n.lex)) as shorter
    from cand c, new_title n
   where c.lex && n.lex)
select key, title, status, cardinality(shared_lex) as shared,
       ts_headline('english', search_text(title),
         to_tsquery('simple', (select string_agg(quote_literal(x), ' | ') from unnest(shared_lex[1:$5]) x)), $4) as headline
  from scored
 where (cardinality(shared_lex) >= 3 and 2 * cardinality(shared_lex) >= shorter)
    or (cardinality(shared_lex) >= 1 and cardinality(shared_lex) = shorter)
 order by shared desc, updated_at desc
 limit 5
`

// The duplicate check reads every stored title's lexemes from issues.title_lexemes, which the issues
// trigger writes on every insert and retitle, and answers what it answered when it built them from
// the titles: the same candidates in the same order, with the same shared counts and snippets, with
// and without a parent. The titles share words with each other and with their own keys (DUP-12's
// key holds `dup` and `12`), repeat a stem, hold the parent's words, a hyphenated word, an
// underscore run search_text breaks, accented letters, or only stop words, and some were retitled
// after they were created.
func TestDuplicateCheckFromStoredLexemesMatchesTheCheckFromTitles(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	vocabulary := strings.Fields(`dispatch global search searching searches issue issues document
		documents title parser ranking model-routing withdrawn credentials café résumé dup 3 12 the and
		of see/a_b_c_d_e_f_g_h_i_j_k_l_m_n_o_p_q_r.txt astrolabe calibration launch window settlement`)
	random := rand.New(rand.NewPCG(505, 1725))
	randomTitle := func() string {
		words := make([]string, 2+random.IntN(9))
		for i := range words {
			words[i] = vocabulary[random.IntN(len(vocabulary))]
		}
		return strings.Join(words, " ")
	}
	type seed struct{ project, key, parent, title string }
	seeds := []seed{{"DUP", "DUP-1", "", "Dispatch global search"}}
	for range 10 {
		seeds = append(seeds, seed{"DUP", "", "DUP-1", "Dispatch global search: " + randomTitle()})
	}
	for _, title := range []string{"the and of", "DUP 12 parser ranking", "Searching searches searched",
		"Café résumé launch window", "Model-routing withdrawn credentials"} {
		seeds = append(seeds, seed{"DUP", "", "", title})
	}
	for range 150 {
		seeds = append(seeds, seed{"DUP", "", "", randomTitle()})
	}
	for range 20 {
		seeds = append(seeds, seed{"OTHER", "", "", randomTitle()})
	}
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name) values ('DUP', 'Dup'), ('OTHER', 'Other')`); err != nil {
		t.Fatalf("seed projects: %v", err)
	}
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	numbers := map[string]int{}
	var stored []string
	for n, issue := range seeds {
		numbers[issue.project]++
		key := fmt.Sprintf("%s-%d", issue.project, numbers[issue.project])
		var parent *string
		if issue.parent != "" {
			parent = &issue.parent
		}
		status := "todo"
		if n%5 == 0 {
			status = "done"
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, parent_key, status, created_by, rank, updated_at)
			values ($1, $2, $3, $4, $5, $6, '{"kind":"user","id":"alice"}', $7, $8)`,
			key, issue.project, numbers[issue.project], issue.title, parent, status, fmt.Sprintf("%06d", n), base.Add(time.Duration(n)*time.Second)); err != nil {
			t.Fatalf("insert %s: %v", key, err)
		}
		if issue.project == "DUP" {
			stored = append(stored, issue.title)
		}
	}
	for n := 2; n <= numbers["DUP"]; n += 7 {
		retitle := randomTitle()
		if _, err := database.Pool.Exec(ctx, `update issues set title = $2, updated_at = $3 where key = $1`,
			fmt.Sprintf("DUP-%d", n), retitle, base.Add(time.Duration(len(seeds)+n)*time.Second)); err != nil {
			t.Fatalf("retitle DUP-%d: %v", n, err)
		}
		stored = append(stored, retitle)
	}

	probes := []string{"dup 12 parser", "DUP 12 parser ranking", "the and of", "searched search", "café résumé",
		"Dispatch global search: parser ranking", "Dispatch global search", "model-routing credentials"}
	for range 80 {
		probes = append(probes, randomTitle())
	}
	for n := 0; n < len(stored); n += 9 {
		probes = append(probes, stored[n])
	}
	answered, full, parentMatters := 0, 0, false
	for _, probe := range probes {
		var withoutParent []model.DuplicateCandidate
		for _, parent := range []string{"", "DUP-1"} {
			got, err := (&server{}).duplicateCandidates(ctx, database.Pool, "DUP", probe, parent)
			if err != nil {
				t.Fatalf("duplicate check of %q under parent %q: %v", probe, parent, err)
			}
			want := duplicateCandidatesFromTitles(t, ctx, database.Pool, probe, parent)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%q under parent %q:\n stored lexemes %#v\n titles         %#v", probe, parent, got, want)
			}
			if len(want) > 0 {
				answered++
			}
			if len(want) == 5 {
				full++
			}
			if parent == "" {
				withoutParent = want
			} else if !reflect.DeepEqual(withoutParent, want) {
				parentMatters = true
			}
		}
	}
	if answered < 20 || full == 0 || !parentMatters {
		t.Fatalf("the fixture is too weak to compare: %d answers held a candidate, %d held five, the parent changed one: %v", answered, full, parentMatters)
	}
}

// duplicateCandidatesFromTitles runs duplicateQueryFromTitles and reads its rows as
// duplicateCandidates reads the check's.
func duplicateCandidatesFromTitles(t *testing.T, ctx context.Context, q queryer, title, parentKey string) []model.DuplicateCandidate {
	t.Helper()
	rows, err := q.Query(ctx, duplicateQueryFromTitles, "DUP", title, parentKey, duplicateHeadlineOptions, duplicateHeadlineWords)
	if err != nil {
		t.Fatalf("duplicate check from titles of %q: %v", title, err)
	}
	defer rows.Close()
	candidates := []model.DuplicateCandidate{}
	for rows.Next() {
		var candidate model.DuplicateCandidate
		var headline string
		if err := rows.Scan(&candidate.Key, &candidate.Title, &candidate.Status, &candidate.SharedTerms, &headline); err != nil {
			t.Fatalf("scan: %v", err)
		}
		candidate.Snippet = markSnippet(headline)
		candidate.Href = "/issues/" + candidate.Key
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the check from titles of %q: %v", title, err)
	}
	return candidates
}
