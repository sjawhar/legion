package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// listReadOptions is how one read of a list is made: as the dashboard makes it (trees), as a
// browser makes it (gzip), holding a validator it was given (etag), and counted by a roundTrips
// when the test counts the statements the read runs (counted).
type listReadOptions struct {
	trees   bool
	gzip    bool
	etag    string
	counted bool
}

// listRead is one GET of a list as alice under those options, answered however the server answers
// it: a test of a 304 asserts the status itself.
func listRead(t *testing.T, handler http.Handler, target string, options listReadOptions) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	if options.counted {
		ctx = context.WithValue(ctx, countedRead{}, true)
	}
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	if options.trees {
		request.Header.Set(displayTreesHeader, "1")
	}
	if options.gzip {
		request.Header.Set("Accept-Encoding", "gzip")
	}
	if options.etag != "" {
		request.Header.Set("If-None-Match", options.etag)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// listETagOf is the validator of a list answered whole, which is what a reader holds to ask again
// with. It fails unless the answer is a 200 carrying a body and an ETag.
func listETagOf(t *testing.T, response *httptest.ResponseRecorder, what string) string {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s, want 200", what, response.Code, response.Body.String())
	}
	if response.Body.Len() == 0 {
		t.Fatalf("%s carries no body", what)
	}
	etag := response.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("%s carries no ETag", what)
	}
	return etag
}

// requireNotModified fails unless the answer is the 304 a reader holding etag is owed: that same
// ETag again, and not a byte of body.
func requireNotModified(t *testing.T, response *httptest.ResponseRecorder, etag, what string) {
	t.Helper()
	if response.Code != http.StatusNotModified {
		t.Fatalf("%s: status=%d body=%s, want 304", what, response.Code, response.Body.String())
	}
	if got := response.Header().Get("ETag"); got != etag {
		t.Errorf("%s: ETag = %q, want the %s it was asked with", what, got, etag)
	}
	if response.Body.Len() != 0 {
		t.Errorf("%s carries %d bytes of body, want none", what, response.Body.Len())
	}
}

// requireListVary fails unless the answer tells a cache that both the trees header and the
// accepted encoding choose its bytes, however many field lines it spells them over.
func requireListVary(t *testing.T, response *httptest.ResponseRecorder, what string) {
	t.Helper()
	named := map[string]bool{}
	for _, line := range response.Header().Values("Vary") {
		for _, field := range strings.Split(line, ",") {
			named[http.CanonicalHeaderKey(strings.TrimSpace(field))] = true
		}
	}
	for _, want := range []string{displayTreesHeader, "Accept-Encoding"} {
		if !named[http.CanonicalHeaderKey(want)] {
			t.Errorf("%s: Vary = %q, want %s named in it", what, response.Header().Values("Vary"), want)
		}
	}
}

// listTargets is one issue with comments comments on it and the two lists this file is about: its
// comment list and its activity feed, each read under an ETag and each keeping what it answered.
func listTargets(t *testing.T, handler http.Handler, project string, comments int) (key, commentList, activityFeed string) {
	t.Helper()
	key = issueWithComments(t, handler, project, comments)
	return key, "/api/v1/issues/" + key + "/comments", "/api/v1/issues/" + key + "/events"
}

// issueLastSeq is the issue's newest event sequence: what the activity feed's validator is built
// on, and what the comment list's deliberately is not.
func issueLastSeq(t *testing.T, database *store.Store, key string) int {
	t.Helper()
	var lastSeq int
	if err := database.Pool.QueryRow(context.Background(),
		`select last_seq from issues where key = $1`, key).Scan(&lastSeq); err != nil {
		t.Fatalf("read %s's event sequence: %v", key, err)
	}
	return lastSeq
}

// An unchanged list answers a reader holding its own ETag a 304 with no body, and a reader holding
// a stale one the whole answer again, under the ETag it had all along. Both lists, both readings.
func TestAnUnchangedListAnswersNotModifiedToItsOwnETag(t *testing.T) {
	handler := newTestHandler(t)
	_, comments, feed := listTargets(t, handler, "ETAG", 3)
	for _, list := range []struct{ name, target string }{
		{"the comment list", comments},
		{"the activity feed", feed},
	} {
		for _, read := range []struct {
			name  string
			trees bool
		}{
			{"with trees", true},
			{"without trees", false},
		} {
			t.Run(list.name+" "+read.name, func(t *testing.T) {
				what := "GET " + list.target + " " + read.name
				first := listRead(t, handler, list.target, listReadOptions{trees: read.trees})
				etag := listETagOf(t, first, what)

				again := listRead(t, handler, list.target, listReadOptions{trees: read.trees, etag: etag})
				requireNotModified(t, again, etag, what+" holding its own ETag")

				stale := listRead(t, handler, list.target, listReadOptions{trees: read.trees, etag: `"a validator of another answer"`})
				if got := listETagOf(t, stale, what+" holding a stale ETag"); got != etag {
					t.Errorf("%s holding a stale ETag answers %s, want the %s the list stands at", what, got, etag)
				}
				if !bytes.Equal(stale.Body.Bytes(), first.Body.Bytes()) {
					t.Errorf("%s holding a stale ETag answers %d bytes, want the %d the whole answer carries",
						what, stale.Body.Len(), first.Body.Len())
				}
			})
		}
	}
}

// Both lists tell every cache between a browser and Dispatch that the trees header and the
// accepted encoding each choose their bytes, on the answer and on the 304 that revalidates it: a
// 304 that named neither would let a cache hand a caller the answer written for the other.
func TestBothListsVaryOnTheTreesHeaderAndTheAcceptedEncoding(t *testing.T) {
	handler := newTestHandler(t)
	_, comments, feed := listTargets(t, handler, "VARY", 2)
	for _, target := range []string{comments, feed} {
		answered := listRead(t, handler, target, listReadOptions{trees: true})
		etag := listETagOf(t, answered, "GET "+target)
		requireListVary(t, answered, "the 200 of GET "+target)

		notModified := listRead(t, handler, target, listReadOptions{trees: true, etag: etag})
		requireNotModified(t, notModified, etag, "GET "+target+" holding its own ETag")
		requireListVary(t, notModified, "the 304 of GET "+target)
	}
}

// An edited comment moves the comment list's ETag and the answer carries the new wording: the
// negative control of a warm read that is served from what was kept.
func TestAnEditedCommentChangesTheCommentListsETag(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "EDITL", "An edited comment", "A spec")
	comment := createReferenceAPIComment(t, handler, issue.Key, "The **first** wording of this comment.")
	target := "/api/v1/issues/" + issue.Key + "/comments"

	first := listRead(t, handler, target, listReadOptions{trees: true})
	etag := listETagOf(t, first, "GET "+target)

	const edited = "The **second** wording, which the list has to answer."
	requireEdit(t, "edit the comment", editCommentAs(t, handler, comment.ID, edited, nil, nil))

	again := listRead(t, handler, target, listReadOptions{trees: true, etag: etag})
	what := "GET " + target + " after the edit, holding the ETag from before it"
	if got := listETagOf(t, again, what); got == etag {
		t.Fatalf("%s answers the same ETag %s, so a reader of the edited comment would be told nothing changed", what, etag)
	}
	requireAnsweredTree(t, again, "0.body_tree", edited)
}

// A delivery attempt opened on a mentioned comment moves the comment list's ETag although it
// appends no event and leaves the issue's last_seq where it was. This is the case the strong
// validator exists for: an ETag over that column would answer 304 to a reader whose list now
// shows an attempt it has never seen.
func TestANewDeliveryAttemptChangesTheCommentListsETagWithNoEventAppended(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "DLVRL", "A delivery attempt", "A spec")
	comment := createReferenceAPIComment(t, handler, issue.Key, "A comment a session is to be told about.")
	target := "/api/v1/issues/" + issue.Key + "/comments"

	first := listRead(t, handler, target, listReadOptions{trees: true})
	etag := listETagOf(t, first, "GET "+target)
	before := issueLastSeq(t, database, issue.Key)

	// The row a claim leaves behind, written as the delivery's own transactions write it: pending,
	// claimed, and with no event of its own (comment_delivery.go).
	if _, err := database.Pool.Exec(context.Background(), `
		insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, state, claimed_at)
		values ($1, 'session:s1', 1, 'steer', 's1', 'pending', now())
	`, comment.ID); err != nil {
		t.Fatalf("open the delivery attempt: %v", err)
	}
	if after := issueLastSeq(t, database, issue.Key); after != before {
		t.Fatalf("the issue's last_seq moved from %d to %d; this case needs a write that appends no event", before, after)
	}

	again := listRead(t, handler, target, listReadOptions{trees: true, etag: etag})
	what := "GET " + target + " after the attempt was opened, holding the ETag from before it"
	if got := listETagOf(t, again, what); got == etag {
		t.Fatalf("%s answers the same ETag %s though the list now carries a delivery attempt", what, etag)
	}
	if _, held := valueAt(t, again, "0.deliveries.0"); !held {
		t.Errorf("the answer carries no delivery attempt:\n\t%s", again.Body.String())
	}
}

// A deploy that reads markdown differently moves both validators, however the answers themselves
// come out: the display parser version is hashed into each, so no ETag the build before it gave is
// matched by this one.
func TestADeployOfANewDisplayParserVersionChangesBothListETags(t *testing.T) {
	handler := newTestHandler(t)
	_, comments, feed := listTargets(t, handler, "PRSRL", 2)
	for _, list := range []struct{ name, target string }{
		{"the comment list", comments},
		{"the activity feed", feed},
	} {
		t.Run(list.name, func(t *testing.T) {
			before := listETagOf(t, listRead(t, handler, list.target, listReadOptions{trees: true}), "GET "+list.target)

			restore := pmdoc.SetDisplayTreeVersionForTest(pmdoc.DisplayTreeVersion + 1)
			defer restore()
			answered := listRead(t, handler, list.target, listReadOptions{trees: true, etag: before})
			what := fmt.Sprintf("GET %s at display parser version %d", list.target, pmdoc.DisplayTreeVersionNow())
			if got := listETagOf(t, answered, what); got == before {
				t.Errorf("%s answers the ETag %s the version before it gave", what, before)
			}
		})
	}
}

// The feed's validator covers the query that chooses its page: two pages of one issue never share
// an ETag, and a reader holding one page's validator is answered the whole of another.
func TestTheActivityFeedsETagChangesWithItsPagingQuery(t *testing.T) {
	handler := newTestHandler(t)
	_, _, feed := listTargets(t, handler, "PAGEL", 3)
	etags := map[string]string{}
	for _, query := range []string{"", "?limit=1", "?limit=2", "?order=desc", "?after=1", "?ids=1"} {
		what := "GET " + feed + query
		etag := listETagOf(t, listRead(t, handler, feed+query, listReadOptions{trees: true}), what)
		for earlier, held := range etags {
			if held == etag {
				t.Errorf("%s answers the ETag of GET %s (%s): its paging query is not in the validator",
					what, feed+earlier, etag)
			}
		}
		etags[query] = etag
	}
	crossed := listRead(t, handler, feed+"?limit=1", listReadOptions{trees: true, etag: etags[""]})
	if crossed.Code != http.StatusOK {
		t.Errorf("GET %s?limit=1 holding the whole feed's ETag: status=%d, want 200", feed, crossed.Code)
	}
}

// A caller that asks for the display trees and one that sends no header never share a validator,
// for either list: the reading is hashed into the ETag, so neither can be answered 304 against the
// other's.
func TestATreesRequestAndOneWithoutNeverShareAListETag(t *testing.T) {
	handler := newTestHandler(t)
	_, comments, feed := listTargets(t, handler, "TREEL", 2)
	for _, list := range []struct{ name, target string }{
		{"the comment list", comments},
		{"the activity feed", feed},
	} {
		t.Run(list.name, func(t *testing.T) {
			with := listETagOf(t, listRead(t, handler, list.target, listReadOptions{trees: true}), "GET "+list.target+" with trees")
			without := listETagOf(t, listRead(t, handler, list.target, listReadOptions{}), "GET "+list.target+" without trees")
			if with == without {
				t.Fatalf("%s answers one ETag %s for both readings", list.name, with)
			}
			crossed := listRead(t, handler, list.target, listReadOptions{etag: with})
			if crossed.Code != http.StatusOK {
				t.Errorf("GET %s without trees, holding the trees reading's ETag: status=%d, want 200",
					list.target, crossed.Code)
			}
		})
	}
}

// An issue with no comments answers the same bytes to both readings, and still answers an ETag of
// its own to each: this is the list a validator over the bytes alone would hand to the wrong
// caller, which is why the reading is hashed in beside them.
func TestAnEmptyCommentListAnswersOneETagPerReading(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "EMPTYL", "No comments at all", "A spec")
	target := "/api/v1/issues/" + issue.Key + "/comments"

	with := listRead(t, handler, target, listReadOptions{trees: true})
	without := listRead(t, handler, target, listReadOptions{})
	withETag := listETagOf(t, with, "GET "+target+" with trees")
	withoutETag := listETagOf(t, without, "GET "+target+" without trees")
	if !bytes.Equal(with.Body.Bytes(), without.Body.Bytes()) {
		t.Fatalf("an empty comment list answers %q with trees and %q without; this case needs the one answer",
			with.Body.String(), without.Body.String())
	}
	if withETag == withoutETag {
		t.Errorf("both readings of the empty comment list answer the ETag %s", withETag)
	}
}

// A second read of an unchanged list is written from what the first read kept. Each is answered
// before its rows are read at all - the feed from the issue's event sequence, the comment list
// from its watermark (answerCommentList) - so no statement of either touches the table its answer
// is built from, and both are written without encoding or compressing anything again.
func TestASecondReadOfEachListIsAnsweredFromTheKeptBytes(t *testing.T) {
	database := storetest.Open(t)
	counter := &roundTrips{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{
		store: traced,
		// The caller's identity is resolved on the untraced pool, so the count is the read's own
		// statements and not the sign-in lookup every request makes.
		identity: headerIdentity(database),
		// A settlement of the issue's spec is database work of its own on the same pool; a delay no
		// test run reaches keeps it out of the count.
		settle: time.Hour,
	})
	_, comments, feed := listTargets(t, handler, "WARML", 5)
	for _, list := range []struct{ name, target, forbidden string }{
		{"the comment list", comments, "from comments"},
		{"the activity feed", feed, "from events"},
	} {
		t.Run(list.name, func(t *testing.T) {
			first := listRead(t, handler, list.target, listReadOptions{trees: true})
			etag := listETagOf(t, first, "the first read of "+list.target)

			answered := served.listAnswers.answered()
			counter.take()
			second := listRead(t, handler, list.target, listReadOptions{trees: true, counted: true})
			counted := counter.take()

			if got := listETagOf(t, second, "the second read of "+list.target); got != etag {
				t.Fatalf("the second read of %s answers %s, want the %s the first gave", list.target, got, etag)
			}
			if !bytes.Equal(second.Body.Bytes(), first.Body.Bytes()) {
				t.Errorf("the second read of %s answers %d bytes, want the %d the first answered",
					list.target, second.Body.Len(), first.Body.Len())
			}
			if written := served.listAnswers.answered() - answered; written != 1 {
				t.Errorf("the cache wrote %d answers over the second read of %s, want the one it kept:\n%s",
					written, list.target, strings.Join(roundTripLines(counted), "\n"))
			}
			if list.forbidden == "" {
				return
			}
			for _, statement := range counted {
				if strings.Contains(statement, list.forbidden) {
					t.Errorf("the second read of %s ran %q, want an answer written without reading the page:\n%s",
						list.target, statement, strings.Join(roundTripLines(counted), "\n"))
				}
			}
		})
	}
}

// A warm read that takes gzip is written the kept compressed bytes: one gzip stream, not two, and
// what it decodes to is the JSON the same read is answered without the header. Serving the
// compressed form is why these answers set their own Content-Encoding, which is what keeps the
// compression layer from compressing them a second time (list_answers.go).
func TestAWarmGzipReadOfAListIsCompressedOnce(t *testing.T) {
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{})
	_, comments, feed := listTargets(t, handler, "GZWARM", 6)
	for _, list := range []struct{ name, target string }{
		{"the comment list", comments},
		{"the activity feed", feed},
	} {
		t.Run(list.name, func(t *testing.T) {
			plain := listRead(t, handler, list.target, listReadOptions{trees: true})
			listETagOf(t, plain, "GET "+list.target+" taking no encoding")
			if plain.Body.Len() <= compressibleMinimum {
				t.Fatalf("%s answers %d bytes, which is under the %d this case has to be above",
					list.name, plain.Body.Len(), compressibleMinimum)
			}

			first := listRead(t, handler, list.target, listReadOptions{trees: true, gzip: true})
			etag := listETagOf(t, first, "GET "+list.target+" taking gzip")
			if got := first.Header().Get("Content-Encoding"); got != compressedEncoding {
				t.Fatalf("GET %s taking gzip: Content-Encoding = %q, want %q", list.target, got, compressedEncoding)
			}

			answered := served.listAnswers.answered()
			second := listRead(t, handler, list.target, listReadOptions{trees: true, gzip: true, etag: `"a validator of another answer"`})
			if got := listETagOf(t, second, "the second gzip read of "+list.target); got != etag {
				t.Fatalf("the second gzip read of %s answers %s, want the %s the first gave", list.target, got, etag)
			}
			if written := served.listAnswers.answered() - answered; written != 1 {
				t.Errorf("the cache wrote %d answers over the second gzip read of %s, want the one it kept", written, list.target)
			}
			if got := second.Header().Get("Content-Encoding"); got != compressedEncoding {
				t.Errorf("the second gzip read of %s: Content-Encoding = %q, want %q", list.target, got, compressedEncoding)
			}
			if !bytes.Equal(second.Body.Bytes(), first.Body.Bytes()) {
				t.Errorf("the second gzip read of %s answers %d bytes, want the %d the first answered",
					list.target, second.Body.Len(), first.Body.Len())
			}
			if decoded := gunzip(t, second.Body.Bytes()); !bytes.Equal(decoded, plain.Body.Bytes()) {
				t.Errorf("the kept gzip answer of %s decodes to %d bytes, want the %d the same read answers without the header",
					list.target, len(decoded), plain.Body.Len())
			}
		})
	}
}
