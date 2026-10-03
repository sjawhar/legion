package api

import (
	"context"
	"fmt"
	"html"
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

// duplicateQueryFromTitles parses every stored title and parent from their title text, rather than
// the stored lexemes 0069 introduced.
const duplicateQueryFromTitles = `
with parent as (select coalesce((select title from issues where key = $3), '') as title),
new_title as (
  select array(select unnest(tsvector_to_array(to_tsvector('english', search_text($2))))
               except select unnest(tsvector_to_array(to_tsvector('english', search_text(p.title))))) as lex
    from parent p),
cand as (
  select i.key, i.title, i.status, i.updated_at,
         array(select unnest(tsvector_to_array(to_tsvector('english', search_text(i.title))))
               except select unnest(tsvector_to_array(to_tsvector('english', search_text(p.title))))) as lex
    from issues i, parent p
   where i.project_key = $1 and i.key <> $3),
scored as (
  select c.key, c.title, c.status, c.updated_at, n.lex as new_lex,
         (select count(*) from (select unnest(c.lex) intersect select unnest(n.lex)) s)::int as shared,
         least(cardinality(c.lex), cardinality(n.lex)) as shorter
    from cand c, new_title n
   where c.lex && n.lex)
select key, title, status, shared,
       ts_headline('english', search_text(title),
         to_tsquery('simple', (select string_agg(quote_literal(x), ' | ') from unnest(new_lex) x)), $4) as headline
  from scored
 where (shared >= 3 and 2 * shared >= shorter) or (shared >= 1 and shared = shorter)
 order by shared desc, updated_at desc
 limit 5
`

// The duplicate check reads every stored title's lexemes from issues.title_lexemes, which the issues
// trigger writes on every insert and retitle, and answers what the from-titles query answers:
// the same candidates in the same order, with the same shared counts. Its snippets differ only when
// a parent contains a word that is also a part of a hyphenated word of the new title. The titles
// share words with each other and with their own keys (DUP-12's key holds `dup` and `12`), repeat a
// stem, hold the parent's words, a hyphenated word, an underscore run search_text breaks, accented
// letters, or only stop words, and some were retitled after they were created.
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
		"Café résumé launch window", "Model-routing withdrawn credentials", "Launch window dispatch"} {
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
		"Dispatch global search: parser ranking", "Dispatch global search", "model-routing credentials",
		"dispatch-search launch window"}
	for range 80 {
		probes = append(probes, randomTitle())
	}
	for n := 0; n < len(stored); n += 9 {
		probes = append(probes, stored[n])
	}
	var parentLexemes []string
	if err := database.Pool.QueryRow(ctx, `select title_lexemes from issues where key = 'DUP-1'`).Scan(&parentLexemes); err != nil {
		t.Fatalf("read DUP-1's lexemes: %v", err)
	}
	// Beside the parent, the from-titles query marked `dispatch`, a part of the probe's
	// `dispatch-search` and a word of the parent's, in this candidate; the check marks shared words.
	const hyphenProbe, hyphenCandidate = "dispatch-search launch window", "Launch window dispatch"
	answered, full, parentMatters, pinned := 0, 0, false, false
	for _, probe := range probes {
		var withoutParent []model.DuplicateCandidate
		for _, parent := range []string{"", "DUP-1"} {
			got, err := (&server{}).duplicateCandidates(ctx, database.Pool, "DUP", probe, parent)
			if err != nil {
				t.Fatalf("duplicate check of %q under parent %q: %v", probe, parent, err)
			}
			want := duplicateCandidatesFromTitles(t, ctx, database.Pool, probe, parent)
			if len(got) != len(want) {
				t.Errorf("%q under parent %q:\n stored lexemes %#v\n from titles   %#v", probe, parent, got, want)
				continue
			}
			for n := range want {
				if parent != "" && probe == hyphenProbe && want[n].Title == hyphenCandidate {
					pinned = true
					if got[n].Snippet != "<mark>Launch</mark> <mark>window</mark> dispatch" ||
						want[n].Snippet != "<mark>Launch</mark> <mark>window</mark> <mark>dispatch</mark>" {
						t.Errorf("%q under parent %q, %s: snippet %q, from-titles %q", probe, parent, want[n].Key, got[n].Snippet, want[n].Snippet)
					}
				}
				if got[n].Snippet != want[n].Snippet {
					if parent == "" {
						t.Errorf("%q, no parent, %s: snippet %q, from-titles %q", probe, want[n].Key, got[n].Snippet, want[n].Snippet)
					} else {
						unmarkedPartsOfTheParent(t, ctx, database.Pool, parentLexemes, got[n].Snippet, want[n].Snippet)
					}
				}
				got[n].Snippet, want[n].Snippet = "", ""
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%q under parent %q:\n stored lexemes %#v\n from titles   %#v", probe, parent, got, want)
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
	if answered < 20 || full == 0 || !parentMatters || !pinned {
		t.Fatalf("the fixture is too weak to compare: %d answers held a candidate, %d held five, the parent changed one: %v, %q named %q: %v",
			answered, full, parentMatters, hyphenProbe, hyphenCandidate, pinned)
	}
}

// unmarkedPartsOfTheParent fails t unless snippet is the from-titles result with some words
// unmarked, each of which holds only the parent's lexemes.
func unmarkedPartsOfTheParent(t *testing.T, ctx context.Context, q queryer, parent []string, snippet, fromTitles string) {
	t.Helper()
	text, marked := snippetMarks(snippet)
	fromTitlesText, fromTitlesMarked := snippetMarks(fromTitles)
	if text != fromTitlesText {
		t.Errorf("snippet %q reads %q, from-titles %q reads %q", snippet, text, fromTitles, fromTitlesText)
		return
	}
	for at, word := range marked {
		if fromTitlesMarked[at] != word {
			t.Errorf("snippet %q marks %q, which from-titles %q does not", snippet, word, fromTitles)
		}
	}
	for at, word := range fromTitlesMarked {
		if marked[at] == word {
			continue
		}
		var parents bool
		if err := q.QueryRow(ctx, `select tsvector_to_array(to_tsvector('english', $1)) <@ $2::text[]`, html.UnescapeString(word), parent).Scan(&parents); err != nil {
			t.Fatalf("read the lexemes of %q: %v", word, err)
		}
		if !parents {
			t.Errorf("snippet %q leaves %q unmarked, which from-titles %q marks and the parent's lexemes do not hold", snippet, word, fromTitles)
		}
	}
}

// snippetMarks reads a snippet as its text without marks and each marked word by its offset in it.
func snippetMarks(snippet string) (string, map[int]string) {
	var text strings.Builder
	marked := map[int]string{}
	for {
		open := strings.Index(snippet, "<mark>")
		if open < 0 {
			text.WriteString(snippet)
			return text.String(), marked
		}
		text.WriteString(snippet[:open])
		snippet = snippet[open+len("<mark>"):]
		end := strings.Index(snippet, "</mark>")
		marked[text.Len()] = snippet[:end]
		text.WriteString(snippet[:end])
		snippet = snippet[end+len("</mark>"):]
	}
}

// duplicateCandidatesFromTitles runs duplicateQueryFromTitles and reads its rows as
// duplicateCandidates reads the check's.
func duplicateCandidatesFromTitles(t *testing.T, ctx context.Context, q queryer, title, parentKey string) []model.DuplicateCandidate {
	t.Helper()
	rows, err := q.Query(ctx, duplicateQueryFromTitles, "DUP", title, parentKey, duplicateHeadlineOptions)
	if err != nil {
		t.Fatalf("from-titles duplicate check of %q: %v", title, err)
	}
	candidates, err := scanDuplicateCandidates(rows)
	if err != nil {
		t.Fatalf("read from-titles duplicate check of %q: %v", title, err)
	}
	return candidates
}
