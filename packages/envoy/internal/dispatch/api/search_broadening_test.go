package api

import (
	"context"
	"net/url"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// searchRowsOf is a page as each row's kind and id, which is what a failure here has to show:
// the rows that came back, without the snippet and the score of every one of them.
func searchRowsOf(body model.SearchResponse) []string {
	rows := make([]string, 0, len(body.Results))
	for _, result := range body.Results {
		rows = append(rows, result.Kind+" "+result.ID)
	}
	return rows
}

// searchStatementsSince is the SQL of every statement recorder saw after mark, which is a
// len(recorder.Statements()) taken before the request under test: the statements that one
// request made, with the seeding before it left out.
func searchStatementsSince(recorder *storetest.StatementRecorder, mark int) []string {
	return recorder.Statements()[mark:]
}

// searchStatementCount is how many of statements are sql exactly: searchQuery is one search, and
// searchRarestWordsSQL is the one extra read the fallback costs, so counting each by its own
// text says how many searches a request ran and whether it looked up a word's rarity at all.
func searchStatementCount(statements []string, sql string) int {
	count := 0
	for _, statement := range statements {
		if statement == sql {
			count++
		}
	}
	return count
}

// Broadening is the fallback for a query nothing answers, never the way a query is answered.
// A query whose every word one item holds is answered by the AND of those words, in one search,
// and that answer holds only rows holding all of them; the same words with one no item holds are
// answered by a second search over the query's rarest words, and that answer holds the rows
// holding some of them. The aggregate recall floors (scripts/search-measure/analyze.py) cannot
// tell those apart - a build that ORed every query's words would still find most right answers
// somewhere on the page - so this pins the two directly, and the rows the broadened form returns
// are what the narrow one is checked against: a build that broadens everything answers the first
// query with the decoys below and fails here rather than passing on a count.
func TestSearchBroadensOnlyTheQueryNoItemAnswers(t *testing.T) {
	database := storetest.Open(t)
	recorder := &storetest.StatementRecorder{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, recorder, nil)}
	// The caller's identity is resolved on the untraced pool, so what the recorder holds is the
	// search's own statements and not the sign-in lookup every request makes.
	handler, _, _ := newTestServer(t, testServerOptions{store: traced, identity: headerIdentity(database)})
	ctx := context.Background()

	issue := createInteractionIssue(t, handler, "SRCH", "Harbour pilot roster", "")
	answer := searchComment(t, handler, issue.Key, "alice", "The lighthouse keeper logs the tide at dawn.")
	// Each decoy holds some of the answer's words and no decoy holds them all: rows the AND of
	// the query leaves out and the OR of its rarest words returns.
	decoys := map[string]string{
		"lighthouse": searchComment(t, handler, issue.Key, "alice", "The lighthouse glazing was replaced."),
		"keeper":     searchComment(t, handler, issue.Key, "alice", "A keeper signed for the paraffin."),
		"tide":       searchComment(t, handler, issue.Key, "alice", "Spring tide floods the slipway."),
		"dawn":       searchComment(t, handler, issue.Key, "alice", "Fog cleared well before dawn."),
	}
	RefreshEverySearchTermStats(ctx, database.Pool)

	// Seven words, every one of them the answer comment's: the AND of them matches that comment
	// and nothing else.
	const held = "lighthouse keeper logs the tide at dawn"
	mark := len(recorder.Statements())
	narrow := searchResponse(t, handler, url.Values{"q": {held}}.Encode())
	statements := searchStatementsSince(recorder, mark)

	if narrow.Query.Broadened {
		t.Fatalf("query reading = %#v, want the query as typed: one comment holds every word", narrow.Query)
	}
	if searchResultOf(narrow, "comment", answer) == nil {
		t.Fatalf("results = %v, want the comment %s, which holds every word", searchRowsOf(narrow), answer)
	}
	for word, decoy := range decoys {
		if searchResultOf(narrow, "comment", decoy) != nil {
			t.Fatalf("the comment %s is on the page and holds %q and not the rest of the query:"+
				" this answer must be the query's words ANDed, not broadened. results = %v",
				decoy, word, searchRowsOf(narrow))
		}
	}
	if len(narrow.Results) != 1 {
		t.Fatalf("results = %v, want the comment %s alone", searchRowsOf(narrow), answer)
	}
	if searched := searchStatementCount(statements, searchQuery); searched != 1 {
		t.Fatalf("the query as typed ran %d searches, want 1, of the %d statements the request made",
			searched, len(statements))
	}
	if rarity := searchStatementCount(statements, searchRarestWordsSQL); rarity != 0 {
		t.Fatalf("the query as typed read word rarity %d times, want none: it never reached the fallback", rarity)
	}

	// The same words and one no item holds: the AND matches nothing, so the fallback runs a
	// second search over the query's rarest words and stamps that answer broadened.
	mark = len(recorder.Statements())
	broadened := searchResponse(t, handler, url.Values{"q": {held + " zzzabsent"}}.Encode())
	statements = searchStatementsSince(recorder, mark)

	if !broadened.Query.Broadened {
		t.Fatalf("query reading = %#v, want the fallback: no item holds every word", broadened.Query)
	}
	if searchResultOf(broadened, "comment", answer) == nil {
		t.Fatalf("results = %v, want the comment %s, which holds five of the eight words",
			searchRowsOf(broadened), answer)
	}
	for word, decoy := range decoys {
		if searchResultOf(broadened, "comment", decoy) == nil {
			t.Fatalf("the comment %s, which holds %q, is not on the page: these are the rows broadening"+
				" returns and the query as typed refuses. results = %v",
				decoy, word, searchRowsOf(broadened))
		}
	}
	if searched := searchStatementCount(statements, searchQuery); searched != 2 {
		t.Fatalf("the query nothing answers ran %d searches, want 2 - the query as typed, then its rarest"+
			" words - of the %d statements the request made", searched, len(statements))
	}
	if rarity := searchStatementCount(statements, searchRarestWordsSQL); rarity != 1 {
		t.Fatalf("the query nothing answers read word rarity %d times, want 1", rarity)
	}
}
