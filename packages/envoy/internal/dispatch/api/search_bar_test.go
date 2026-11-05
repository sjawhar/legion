package api

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// searchComment writes one comment on an issue as login and answers its id: the corpus of every
// filter test below is comments, since a comment carries an author of its own and an issue of
// its own, which is what the filters read.
func searchComment(t *testing.T, handler http.Handler, issueKey, login, body string) string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments",
		map[string]any{"body": body}, login)
	if response.Code != http.StatusCreated {
		t.Fatalf("create %s's comment: status=%d body=%s", login, response.Code, response.Body.String())
	}
	return decodeBody[model.Comment](t, response).ID
}

// A person writes an issue key the way they remember it, and every one of those ways must put
// that issue first: the key, the key in any case, the key with a space where the hyphen is, and
// the number alone. Only the first two match the issue's own search vector, which holds the key
// as one hyphenated lexeme ('srch' <-> '-1042'); the other forms match no vector at all and
// answer the issue only through the keys the statement is given (searchOwnIssueKeys). With no
// project in the query the number alone names one issue of every project, and all of them head
// the list, in project order, before any other row.
func TestSearchLeadsWithTheIssueTheQueryNamesInEveryForm(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	for _, project := range []string{"SRCH", "OTHER"} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
			"key": project, "name": project + " project",
		}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project, response.Code, response.Body.String())
		}
	}
	// Both projects are moved to the same four-digit number: an issue's number is its project's
	// next, and a one-digit query is below the route's own two-character minimum.
	if _, err := database.Pool.Exec(ctx,
		"update projects set next_number = 1042 where key in ('SRCH', 'OTHER')"); err != nil {
		t.Fatalf("move both projects to number 1042: %v", err)
	}
	named := createSearchIssue(t, handler, "SRCH", "Navigation instruments", "")
	twin := createSearchIssue(t, handler, "OTHER", "Harbour logbook", "")
	if named != "SRCH-1042" || twin != "OTHER-1042" {
		t.Fatalf("seeded issues are %q and %q, want SRCH-1042 and OTHER-1042", named, twin)
	}
	// An issue citing the key, which ts_rank_cd scores no lower than the issue's own key, and a
	// comment naming the number as a word of its own, which is the only other row a bare number
	// matches: both must sit behind the issues the query names.
	citing := createSearchIssue(t, handler, "SRCH", named+" follow-up", "")
	comment := searchComment(t, handler, citing, "alice", "Budget line 1042 is closed.")

	for _, form := range []string{named, strings.ToLower(named), "SRCH 1042", "srch 1042"} {
		t.Run(form, func(t *testing.T) {
			body := searchResponse(t, handler, url.Values{"q": {form}}.Encode())
			if len(body.Results) == 0 || body.Results[0].ID != named {
				t.Fatalf("results = %#v, want %s first", body.Results, named)
			}
		})
	}

	t.Run("the number alone within a project", func(t *testing.T) {
		body := searchResponse(t, handler, url.Values{"q": {"1042"}, "project": {"SRCH"}}.Encode())
		if len(body.Results) == 0 || body.Results[0].ID != named {
			t.Fatalf("results = %#v, want %s first", body.Results, named)
		}
		if searchResultOf(body, "issue", twin) != nil {
			t.Fatalf("results = %#v, want nothing of %s, which another project holds", body.Results, twin)
		}
	})

	t.Run("the number alone across every project", func(t *testing.T) {
		body := searchResponse(t, handler, url.Values{"q": {"1042"}}.Encode())
		if len(body.Results) < 3 {
			t.Fatalf("results = %#v, want both issues of number 1042 and the comment naming it", body.Results)
		}
		if body.Results[0].ID != twin || body.Results[1].ID != named {
			t.Fatalf("results = %s then %s, want %s then %s: every matching issue heads the list, in project order",
				body.Results[0].ID, body.Results[1].ID, twin, named)
		}
		if body.Results[2].Kind != "comment" || body.Results[2].ID != comment {
			t.Fatalf("results[2] = %#v, want the comment %s, which both issues outrank", body.Results[2], comment)
		}
	})
}

// websearch_to_tsquery ANDs a person's words, so a question of theirs answers nothing as soon as
// one of its words is in no item at all. Past searchBroadenWords words search ORs the words onto
// the query, which ranks a row by how many of them it holds, and the words the question excludes
// are ANDed onto that leg, so a -word still leaves its rows out.
func TestSearchBroadensALongQuestionToItsOwnWords(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "SRCH", "Controller restarts deploy", "")
	comment := searchComment(t, handler, issue.Key, "alice", "The agent pool is drained first.")

	// Six words, one of them ("zebraquill") in nothing: the issue holds three of them and the
	// comment two, and the AND of all six holds nowhere.
	body := searchResponse(t, handler,
		url.Values{"q": {"controller restarts deploy agent pool zebraquill"}}.Encode())
	if !body.Query.Broadened {
		t.Fatalf("query reading = %#v, want the any-word leg for a six-word question", body.Query)
	}
	if len(body.Results) == 0 || body.Results[0].Kind != "issue" || body.Results[0].ID != issue.Key {
		t.Fatalf("results = %#v, want the issue first: it holds three of the six words where the comment holds two",
			body.Results)
	}
	if searchResultOf(body, "comment", comment) == nil {
		t.Fatalf("results = %#v, want the comment %s, which holds two of the words", body.Results, comment)
	}

	// The same question, less what it excludes: the comment holds "drained" and goes, the issue
	// stays. Without the exclusion ANDed onto the any-word leg, OR-ing the words back in would
	// return the comment the question refused.
	less := searchResponse(t, handler,
		url.Values{"q": {"controller restarts deploy agent pool zebraquill -drained"}}.Encode())
	if !less.Query.Broadened || !slices.Equal(less.Query.Operators, []string{contracts.SearchOperatorExclude}) {
		t.Fatalf("query reading = %#v, want a broadened query read as excluding a word", less.Query)
	}
	if searchResultOf(less, "comment", comment) != nil {
		t.Fatalf("results = %#v, want nothing of the comment %s, which holds the excluded word",
			less.Results, comment)
	}
	if searchResultOf(less, "issue", issue.Key) == nil {
		t.Fatalf("results = %#v, want the issue %s, which holds none of the excluded word", less.Results, issue.Key)
	}
}

// A query of searchBroadenWords words or fewer is the AND it has always been: every word must
// hold, and a query naming one word nothing holds answers nothing, however many of the rest it
// gets right.
func TestSearchKeepsAShortQueryTheANDOfItsWords(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "SRCH", "Controller restarts deploy", "")
	searchComment(t, handler, issue.Key, "alice", "The agent pool is drained first.")

	// Five words, one of them in nothing.
	brief := searchResponse(t, handler, url.Values{"q": {"controller restarts deploy agent zebraquill"}}.Encode())
	if brief.Query.Broadened {
		t.Fatalf("query reading = %#v, want no any-word leg for a five-word query", brief.Query)
	}
	if len(brief.Results) != 0 {
		t.Fatalf("results = %#v, want none: one word of the five is in no item", brief.Results)
	}

	// The same length, every word held: the AND answers what it always answered.
	found := searchResponse(t, handler, url.Values{"q": {"controller restarts deploy"}}.Encode())
	if searchResultOf(found, "issue", issue.Key) == nil {
		t.Fatalf("results = %#v, want the issue %s, whose title holds every word", found.Results, issue.Key)
	}
}

// The three filters the bar shows as chips, each narrowing the search rather than the page: a
// status is the status of the issue a row belongs to, so a row with no issue - a project
// document, and a comment or ask on one - matches no status at all; an author is who wrote the
// item the row names, a document's being whoever created it; and updated_since is when that item
// was itself last written, not when anything on its issue moved.
func TestSearchNarrowsEveryLegByStatusAuthorAndDate(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	survey := createInteractionIssue(t, handler, "SRCH", "Astrolabe survey", "")
	if patch := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+survey.Key,
		map[string]any{"status": "todo"}, "alice"); patch.Code != http.StatusOK {
		t.Fatalf("move the survey to todo: status=%d body=%s", patch.Code, patch.Body.String())
	}
	early := searchComment(t, handler, survey.Key, "alice", "The astrolabe needs a rete.")
	document := createProjectDocument(t, handler, "SRCH", "Astrolabe chart", "# Chart\n\nThe astrolabe on its own page.\n")

	// The watermark is the database's own clock, the clock every row's time is written by.
	var cut time.Time
	if err := database.Pool.QueryRow(ctx, "select now()").Scan(&cut); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	atlas := createSearchIssue(t, handler, "SRCH", "Astrolabe atlas", "")
	late := searchComment(t, handler, atlas, "bob", "An astrolabe reading from the atlas.")

	unfiltered := searchResponse(t, handler, "q=astrolabe")
	for _, want := range []struct{ kind, id string }{
		{"issue", survey.Key}, {"issue", atlas}, {"comment", early}, {"comment", late}, {"document", document.ID},
	} {
		if searchResultOf(unfiltered, want.kind, want.id) == nil {
			t.Fatalf("unfiltered results = %#v, want the %s %s, or the filters below decide nothing",
				unfiltered.Results, want.kind, want.id)
		}
	}

	t.Run("status", func(t *testing.T) {
		body := searchResponse(t, handler, url.Values{"q": {"astrolabe"}, "status": {"todo"}}.Encode())
		if len(body.Results) == 0 {
			t.Fatalf("results = %#v, want the todo issue and its comment", body.Results)
		}
		for _, result := range body.Results {
			if result.Owner.Kind != "issue" || result.Owner.Key != survey.Key {
				t.Fatalf("result = %#v, want only rows of %s, the one issue in todo", result, survey.Key)
			}
		}
		if searchResultOf(body, "issue", survey.Key) == nil || searchResultOf(body, "comment", early) == nil {
			t.Fatalf("results = %#v, want the issue %s and its comment %s", body.Results, survey.Key, early)
		}
		if searchResultOf(body, "document", document.ID) != nil {
			t.Fatalf("results = %#v, want nothing of the project document %s, which belongs to no issue",
				body.Results, document.ID)
		}
	})

	t.Run("author", func(t *testing.T) {
		mine := searchResponse(t, handler, url.Values{"q": {"astrolabe"}, "author": {"bob"}}.Encode())
		if len(mine.Results) != 1 || mine.Results[0].Kind != "comment" || mine.Results[0].ID != late {
			t.Fatalf("results = %#v, want bob's one comment %s alone", mine.Results, late)
		}
		if mine.Total != 1 {
			t.Fatalf("total = %d, want 1: the filter narrows the search, so it counts what the search matched", mine.Total)
		}
		// A document's author is whoever created it, the one actor its row names.
		hers := searchResponse(t, handler, url.Values{"q": {"astrolabe"}, "author": {"alice"}}.Encode())
		if searchResultOf(hers, "document", document.ID) == nil || searchResultOf(hers, "comment", early) == nil {
			t.Fatalf("results = %#v, want alice's document %s and comment %s", hers.Results, document.ID, early)
		}
		if searchResultOf(hers, "comment", late) != nil {
			t.Fatalf("results = %#v, want nothing of bob's comment %s", hers.Results, late)
		}
	})

	t.Run("updated_since", func(t *testing.T) {
		body := searchResponse(t, handler, url.Values{
			"q": {"astrolabe"}, "updated_since": {cut.Format(time.RFC3339Nano)},
		}.Encode())
		if searchResultOf(body, "issue", atlas) == nil || searchResultOf(body, "comment", late) == nil {
			t.Fatalf("results = %#v, want the issue %s and the comment %s, both written after the watermark",
				body.Results, atlas, late)
		}
		for _, gone := range []struct{ kind, id string }{
			{"comment", early}, {"document", document.ID},
		} {
			if searchResultOf(body, gone.kind, gone.id) != nil {
				t.Fatalf("results = %#v, want nothing of the %s %s, written before the watermark",
					body.Results, gone.kind, gone.id)
			}
		}
	})
}

// A filter narrows every leg of the search, not the page it is cut to: bob's comment is the
// weakest match of its kind, behind every one of the contracts.SearchKindDepth comments the kind
// is read to, so a filter applied to the rows the search had already chosen could never reach it.
func TestSearchFiltersNarrowEachLegRatherThanThePage(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "SRCH", "Astrolabe survey", "")
	weakest := searchComment(t, handler, issue.Key, "bob", "A quiet astrolabe.")
	for range contracts.SearchKindDepth {
		searchComment(t, handler, issue.Key, "alice", "The astrolabe astrolabe astrolabe.")
	}

	// The control: unfiltered, bob's comment is past the depth its kind is read to, so no page
	// of the answer holds it.
	for offset := 0; offset <= contracts.SearchKindDepth; offset += searchMaxLimit {
		page := searchResponse(t, handler, url.Values{
			"q": {"astrolabe"}, "limit": {"50"}, "offset": {strconv.Itoa(offset)},
		}.Encode())
		if page.Reachable != contracts.SearchKindDepth+1 {
			t.Fatalf("reachable = %d, want %d: one comment must be past the depth its kind is read to",
				page.Reachable, contracts.SearchKindDepth+1)
		}
		if searchResultOf(page, "comment", weakest) != nil {
			t.Fatalf("unfiltered page at offset %d holds bob's comment %s, so the filter below proves nothing",
				offset, weakest)
		}
	}

	filtered := searchResponse(t, handler, url.Values{"q": {"astrolabe"}, "author": {"bob"}}.Encode())
	if len(filtered.Results) != 1 || filtered.Results[0].ID != weakest {
		t.Fatalf("results = %#v, want bob's one comment %s, which the filtered leg reads", filtered.Results, weakest)
	}
}

// A filter is refused as the project and the page parameters beside it are: a repeated, blank or
// unreadable value answers 400 under a code of its own rather than an answer nobody asked for.
func TestSearchRefusesAFilterItCannotRead(t *testing.T) {
	handler := newTestHandler(t)
	cases := map[string]string{
		"q=astrolabe&status=":                                          "INVALID_STATUS",
		"q=astrolabe&status=almost":                                    "INVALID_STATUS",
		"q=astrolabe&status=todo&status=done":                          "INVALID_STATUS",
		"q=astrolabe&author=":                                          "INVALID_AUTHOR",
		"q=astrolabe&author=alice&author=bob":                          "INVALID_AUTHOR",
		"q=astrolabe&author=two+people":                                "INVALID_AUTHOR",
		"q=astrolabe&author=" + strings.Repeat("a", searchAuthorMax+1): "INVALID_AUTHOR",
		"q=astrolabe&updated_since=":                                   "INVALID_UPDATED_SINCE",
		"q=astrolabe&updated_since=yesterday":                          "INVALID_UPDATED_SINCE",
		"q=astrolabe&updated_since=2026-01-01T00:00:00Z&updated_since=2026-02-01T00:00:00Z": "INVALID_UPDATED_SINCE",
	}
	for query, code := range cases {
		t.Run(query, func(t *testing.T) {
			response := searchRequest(t, handler, query)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("search %q: status=%d body=%s", query, response.Code, response.Body.String())
			}
			body := decodeBody[struct {
				Code string `json:"code"`
			}](t, response)
			if body.Code != code {
				t.Fatalf("search %q code=%q, want %q", query, body.Code, code)
			}
		})
	}
	// A value every filter accepts is no refusal at all.
	whole := searchRequest(t, handler, url.Values{
		"q": {"astrolabe"}, "status": {"triage"}, "author": {"alice"},
		"updated_since": {"2026-01-01T00:00:00Z"},
	}.Encode())
	if whole.Code != http.StatusOK {
		t.Fatalf("every filter at a value it accepts: status=%d body=%s", whole.Code, whole.Body.String())
	}
}

// The answer says what search made of the words and what it narrowed by, so a caller can show a
// person what was done with their question instead of parsing it again itself.
func TestSearchAnswersHowItReadTheWordsAndWhatItNarrowed(t *testing.T) {
	handler := newTestHandler(t)
	createInteractionIssue(t, handler, "SRCH", "Astrolabe survey", "")

	read := searchResponse(t, handler, url.Values{"q": {`"navigation instruments" OR astrolabe -sextant`}}.Encode())
	want := []string{contracts.SearchOperatorPhrase, contracts.SearchOperatorOr, contracts.SearchOperatorExclude}
	if !slices.Equal(read.Query.Operators, want) {
		t.Fatalf("query operators = %v, want %v", read.Query.Operators, want)
	}
	if read.Query.Broadened {
		t.Fatalf("query reading = %#v, want no any-word leg for three words", read.Query)
	}

	plain := searchResponse(t, handler, "q=astrolabe")
	if len(plain.Query.Operators) != 0 || plain.Query.Broadened {
		t.Fatalf("query reading = %#v, want no operator and no any-word leg", plain.Query)
	}
	if plain.Filters != (model.SearchFilters{}) {
		t.Fatalf("filters = %#v, want none: the request named none", plain.Filters)
	}

	const since = "2026-01-01T00:00:00Z"
	narrowed := searchResponse(t, handler, url.Values{
		"q": {"astrolabe"}, "project": {"SRCH"}, "status": {"triage"}, "author": {"alice"}, "updated_since": {since},
	}.Encode())
	wantFilters := model.SearchFilters{Project: "SRCH", Status: "triage", Author: "alice", UpdatedSince: since}
	if narrowed.Filters != wantFilters {
		t.Fatalf("filters = %#v, want %#v", narrowed.Filters, wantFilters)
	}
}
