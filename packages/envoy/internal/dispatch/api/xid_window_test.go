package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The xid window tests (INFRA-636, plan test T1) drive each write route through the real handler
// on a fixture the size of the load harness's large specs, and judge the transaction the request
// ran with storetest's xid rig: from the statement that gave the transaction its xid to its
// COMMIT, no statement-free gap over 5 ms, and no more time in gaps than in statements (plan D6).
// A route that runs an advisory read in its transaction (write advice, the claim's Legion-managed
// walk) is also held to running none inside the window (refuseAdvisoryReadsInXidWindow), and a
// route whose writes are one queued batch to holding nothing else in it
// (refuseStatementsBesideTheWriteBatch): those reads are queries, back to back, which D6 alone
// would pass. Each request runs under a marked context (storetest.MarkXid), so only the
// transactions the request itself ran are judged; the server's background work shares the pool
// but not the verdict. The server settles documents after an hour, so no settlement races the
// measured request, except where a route needs settlement to have run first (the block ask).

// xidProject is the fixture's project key.
const xidProject = "XID"

// xidAnchoredComments and xidAnchoredAsks are the fixture's open anchors: twenty, as on the
// harness's large specs once their comments have accumulated.
const (
	xidAnchoredComments = 12
	xidAnchoredAsks     = 8
)

// xidSpec is the fixture spec, storetest.XidSpec on the fixture's project: about 40 KB of tables
// and prose as the harness's large specs, and its requirement cells.
func xidSpec(seed uint64) (string, []string) { return storetest.XidSpec(xidProject, seed) }

// xidFixture is a server on a traced pool holding one issue whose spec is xidSpec with twenty open
// anchors on it, written by a session through the API as the harness's seed writes them.
type xidFixture struct {
	handler  http.Handler
	database *store.Store
	deps     Deps
	tracer   *storetest.XidTracer
	issueKey string
	specID   string
	spec     string
	quotes   []string
	// spare are requirement cells no fixture anchor quotes, for the measured request's own.
	spare    []string
	comments []string
	asks     []string
	// mayHoldNoXid passes a measured request that holds no xid at all
	// (storetest.XidReport.MayHoldNoXid): one that may write nothing.
	mayHoldNoXid bool
}

// newXidFixture builds the fixture on a server with options, whose tracer it sets, and with spec
// as the issue's spec (xidSpec(1) when empty).
func newXidFixture(t *testing.T, options testServerOptions, spec string) *xidFixture {
	t.Helper()
	fixture := &xidFixture{tracer: storetest.NewXidTracer()}
	if options.settle == 0 {
		options.settle = time.Hour
	}
	options.tracer = fixture.tracer
	fixture.handler, fixture.database, fixture.deps = newTestServer(t, options)
	_, fixture.quotes = xidSpec(1)
	if spec == "" {
		spec, _ = xidSpec(1)
	}
	fixture.spec = spec
	started := time.Now()
	issue := createInteractionIssue(t, fixture.handler, xidProject, "Large spec under review", spec)
	fixture.issueKey, fixture.specID = issue.Key, issue.PrimaryArtifactID

	anchors := xidAnchoredComments + xidAnchoredAsks
	step := len(fixture.quotes) / (anchors + 4)
	anchored := map[int]bool{}
	for i := range anchors {
		index := i * step
		anchored[index] = true
		quote := fixture.quotes[index]
		if i < xidAnchoredComments {
			created := sessionRequest(t, fixture.handler, http.MethodPost, "/api/v1/issues/"+fixture.issueKey+"/comments", map[string]any{
				"body":   fmt.Sprintf("Seed comment %d: does %q still hold after the migration?", i, quote),
				"anchor": map[string]any{"artifact": "spec", "quote": quote}, "actor": sessionActor(),
			})
			if created.Code != http.StatusCreated {
				t.Fatalf("seed anchored comment %d: status=%d body=%s", i, created.Code, created.Body.String())
			}
			fixture.comments = append(fixture.comments, decodeBody[model.Comment](t, created).ID)
			continue
		}
		created := sessionRequest(t, fixture.handler, http.MethodPost, "/api/v1/issues/"+fixture.issueKey+"/asks", map[string]any{
			"question": fmt.Sprintf("Seed ask %d: who owns %q?", i, quote),
			"options":  []map[string]string{{"label": "Platform"}, {"label": "Product"}},
			"anchor":   map[string]any{"artifact": "spec", "quote": quote}, "actor": sessionActor(),
		})
		if created.Code != http.StatusCreated {
			t.Fatalf("seed anchored ask %d: status=%d body=%s", i, created.Code, created.Body.String())
		}
		fixture.asks = append(fixture.asks, decodeBody[model.Ask](t, created).ID)
	}
	for index := len(fixture.quotes) - 1; index >= 0 && len(fixture.spare) < 8; index-- {
		if !anchored[index] {
			fixture.spare = append(fixture.spare, fixture.quotes[index])
		}
	}
	t.Logf("xid fixture: spec of %d bytes, %d requirement cells, %d open anchored comments and %d open anchored asks, seeded in %s",
		len(spec), len(fixture.quotes), len(fixture.comments), len(fixture.asks), time.Since(started).Round(time.Millisecond))
	return fixture
}

// measure runs one request through the fixture's handler under a marked context and judges the
// transactions it ran. A request that does not answer want is the rig's failure, not a finding,
// and stops the test before any verdict.
func (f *xidFixture) measure(t *testing.T, want int, request func(http.Handler) *httptest.ResponseRecorder) *storetest.XidReport {
	t.Helper()
	marked := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.handler.ServeHTTP(w, r.WithContext(storetest.MarkXid(r.Context())))
	})
	var response *httptest.ResponseRecorder
	report := f.tracer.Measure(t, f.database.Pool, func() { response = request(marked) })
	if response.Code != want {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): the measured request answered %d, want %d: %s", response.Code, want, response.Body.String())
	}
	report.MayHoldNoXid = f.mayHoldNoXid
	storetest.AssertXidWindows(t, report)
	return report
}

// measureAdvised is measure for a write that runs an advisory read (advisoryStatement), which also
// refuses that read inside the xid window.
func (f *xidFixture) measureAdvised(t *testing.T, want int, request func(http.Handler) *httptest.ResponseRecorder) {
	t.Helper()
	refuseAdvisoryReadsInXidWindow(t, f.measure(t, want, request))
}

// measureBatched is measure for a write whose route runs the two phases as one batch
// (write_queue.go): it also refuses every statement inside the window but that batch and the
// COMMIT ending it.
func (f *xidFixture) measureBatched(t *testing.T, want int, request func(http.Handler) *httptest.ResponseRecorder) {
	t.Helper()
	refuseStatementsBesideTheWriteBatch(t, f.measure(t, want, request))
}

// advisoryStatement opens the savepoint of each advisory read a write runs in its transaction: a
// session's write advice (writeAdvice, write_advice.go) and a claim's Legion-managed walk
// (claimLegionManagedStatus, issue_claim.go). Each bounds its queries with it.
const advisoryStatement = "set local statement_timeout = '500ms'"

// refuseAdvisoryReadsInXidWindow fails t when a write ran an advisory read while it held its xid.
// An advisory read is queries, so D6 cannot see it: each runs back to back with the next. The plan
// moves write advice to the read phase of every caller (M3) and makes the claim's walk xid-free
// (M1), so the queries a write runs to advise its caller hold back no reader's snapshot.
func refuseAdvisoryReadsInXidWindow(t *testing.T, report *storetest.XidReport) {
	t.Helper()
	for _, window := range report.Windows {
		for index, statement := range window.Statements {
			if strings.TrimSpace(statement.SQL) == advisoryStatement {
				t.Errorf("XID WINDOW VIOLATION (INFRA-636 M3/M1) %s\n  an advisory read (write advice or the claim's Legion-managed walk) ran inside the xid window, from statement %d at %s after the window opened\n%s",
					window.Summary(), index, storetest.XidMillis(statement.Start.Sub(window.Start)), window.Detail())
				break
			}
		}
	}
}

// refuseStatementsBesideTheWriteBatch fails t when a route that queues its writes ran anything
// inside its xid window but the one batch its write phase sends and the COMMIT that ends it. A
// read a route runs after its first write is what D6 cannot see on this fixture: each is a fast
// query back to back with the next, so the window passes on timing while every reader's snapshot
// waits for those reads all the same. A route whose writes are queued on a ledger sends them as
// one batch, with the settlement credit's statements after them, and commits (Ledger.Commit), so
// two statements are all such a window may hold.
func refuseStatementsBesideTheWriteBatch(t *testing.T, report *storetest.XidReport) {
	t.Helper()
	for _, window := range report.Windows {
		if len(window.Statements) <= 2 {
			continue
		}
		t.Errorf("XID WINDOW VIOLATION (INFRA-636) %s\n  the transaction ran %d statements inside its xid window, want the write phase's one batch and its COMMIT: a read it runs there holds back every reader's snapshot\n%s",
			window.Summary(), len(window.Statements), window.Detail())
	}
}

// resolveComment resolves a fixture comment, as a person does.
func (f *xidFixture) resolveComment(t *testing.T, commentID string) {
	t.Helper()
	if resolved := dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/comments/"+commentID+"/resolve", nil, "alice"); resolved.Code != http.StatusOK {
		t.Fatalf("resolve comment: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
}

// suggest opens a session's anchored suggestion on a spare requirement cell.
func (f *xidFixture) suggest(t *testing.T) model.Comment {
	t.Helper()
	quote := f.spare[0]
	created := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/comments", map[string]any{
		"body": "This requirement no longer holds as worded.", "anchor": map[string]any{"artifact": "spec", "quote": quote},
		"suggestion": map[string]string{"replace_with": strings.Replace(quote, "holds", "stands", 1)}, "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("open suggestion: status=%d body=%s", created.Code, created.Body.String())
	}
	return decodeBody[model.Comment](t, created)
}

// awaitSettledDocuments waits until no document owes a settlement and no backend has held an xid
// for a quiet spell, so a test that needed settlement to run first measures its request alone.
func awaitSettledDocuments(t *testing.T, database *store.Store) {
	t.Helper()
	const quiet = 300 * time.Millisecond
	deadline := time.Now().Add(30 * time.Second)
	var calm time.Time
	for time.Now().Before(deadline) {
		var busy bool
		if err := database.Pool.QueryRow(context.Background(), `
			select exists (select 1 from doc_settlements_pending)
			    or exists (select 1 from pg_stat_activity
			               where datname = current_database() and backend_xid is not null and pid <> pg_backend_pid())
		`).Scan(&busy); err != nil {
			t.Fatalf("read settlement state: %v", err)
		}
		switch {
		case busy:
			calm = time.Time{}
		case calm.IsZero():
			calm = time.Now()
		case time.Since(calm) >= quiet:
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("documents never finished settling")
}

func TestXidWindowCommentCreateAnchored(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	f.measureAdvised(t, http.StatusCreated, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/comments", map[string]any{
			"body": "Does this still hold once the outbox is sharded?", "anchor": map[string]any{"artifact": "spec", "quote": f.spare[0]},
			"actor": sessionActor(),
		})
	})
}

func TestXidWindowCommentCreateReplyOnAnchoredThread(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	f.measureAdvised(t, http.StatusCreated, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/comments", map[string]any{
			"body": "It does: the listener keeps its interests in the bucket.", "reply_to": f.comments[0], "actor": sessionActor(),
		})
	})
}

func TestXidWindowAskCreateAnchored(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	f.measureAdvised(t, http.StatusCreated, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/asks", map[string]any{
			"question": "Which team signs off on this requirement?",
			"options":  []map[string]string{{"label": "Platform"}, {"label": "Product"}},
			"anchor":   map[string]any{"artifact": "spec", "quote": f.spare[0]}, "actor": sessionActor(),
		})
	})
}

// newBlockAskFixture is the fixture with the transport `:::ask` block at the end of its spec, and
// the id of the ask settlement opened from it. A block ask's row is opened by settlement, so this
// fixture settles twenty milliseconds after each write, as the default server does, and waits for
// every settlement to finish before it answers, so the measured request runs alone. A block ask is
// the one whose transitions write its block back into the document (SetBlockAttributes,
// SetAskBlockText); an anchored ask's touch only its row.
func newBlockAskFixture(t *testing.T) (*xidFixture, string) {
	t.Helper()
	spec, _ := xidSpec(1)
	f := newXidFixture(t, testServerOptions{settle: 20 * time.Millisecond}, spec+"\n"+transportAsk)
	awaitIndexedAskBlock(t, f.handler, f.specID, "decision", "Which transport?")
	askID := blockAskID(t, f.handler, f.issueKey, "decision")
	awaitSettledDocuments(t, f.database)
	return f, askID
}

func TestXidWindowAskAnswerBlock(t *testing.T) {
	f, askID := newBlockAskFixture(t)
	f.measure(t, http.StatusOK, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
			"text": "REST first.",
		}, "alice")
	})
}

func TestXidWindowAskResolveBlock(t *testing.T) {
	f, askID := newBlockAskFixture(t)
	f.measure(t, http.StatusOK, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/asks/"+askID+"/resolve", map[string]any{
			"kind": "resolved", "reason": "Settled in the thread",
		}, "alice")
	})
}

func TestXidWindowAskEditBlockText(t *testing.T) {
	f, askID := newBlockAskFixture(t)
	f.measure(t, http.StatusOK, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
			"question": "Which transport ships first?",
		}, "alice")
	})
}

func TestXidWindowCommentReopen(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	f.resolveComment(t, f.comments[0])
	f.measure(t, http.StatusOK, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/comments/"+f.comments[0]+"/reopen", nil, "alice")
	})
}

func TestXidWindowCommentEdit(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	created := dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/comments", map[string]any{
		"body": "First wording.", "anchor": map[string]any{"artifact": "spec", "quote": f.spare[0]},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create alice's anchored comment: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[model.Comment](t, created)
	f.measure(t, http.StatusOK, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPatch, "/api/v1/comments/"+comment.ID, map[string]any{"body": "Second wording."}, "alice")
	})
}

func TestXidWindowCommentResolve(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	f.measure(t, http.StatusOK, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/comments/"+f.comments[0]+"/resolve", nil, "alice")
	})
}

func TestXidWindowSuggestionAccept(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	suggestion := f.suggest(t)
	f.measure(t, http.StatusOK, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/comments/"+suggestion.ID+"/accept", map[string]any{}, "alice")
	})
}

func TestXidWindowSuggestionReject(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	suggestion := f.suggest(t)
	f.measure(t, http.StatusOK, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/comments/"+suggestion.ID+"/reject", map[string]any{}, "alice")
	})
}

func TestXidWindowSpecEdit(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	f.measureAdvised(t, http.StatusOK, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/artifacts/"+f.specID+"/edits", map[string]any{
			"ops":   []map[string]string{{"op": "replace", "find": "Revision token r0", "with": "Revision token r1"}},
			"actor": sessionActor(),
		})
	})
}

// An upload under the spec's name replaces it: the whole document is re-read, its anchors re-found
// and a version written.
func TestXidWindowUploadReplace(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	revised := strings.Replace(f.spec, "Revision token r0", "Revision token r1", 1)
	f.measureAdvised(t, http.StatusCreated, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/artifacts", map[string]any{
			"name": "spec.md", "content": revised, "summary": "Bump the revision", "actor": sessionActor(),
		})
	})
}

// A named version of a document changed since its last version captures and renders the live
// document.
func TestXidWindowNamedVersion(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	if edited := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/artifacts/"+f.specID+"/edits", map[string]any{
		"ops":   []map[string]string{{"op": "replace", "find": "Revision token r0", "with": "Revision token r1"}},
		"actor": sessionActor(),
	}); edited.Code != http.StatusOK {
		t.Fatalf("edit the spec before naming a version: status=%d body=%s", edited.Code, edited.Body.String())
	}
	f.measure(t, http.StatusCreated, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/artifacts/"+f.specID+"/versions", map[string]any{
			"summary": "Revision one", "actor": sessionActor(),
		})
	})
}

// A rebuild repairs a history ygo cannot load: this server's document store hands the spec's next
// load a history that does not decode (invalidLoadOnceStore), and the rebuild writes the supplied
// markdown as the document's next version.
func TestXidWindowDocumentRebuild(t *testing.T) {
	var broken *invalidLoadOnceStore
	f := newXidFixture(t, testServerOptions{
		persistence: func(database *store.Store) docs.VersionedStore {
			broken = &invalidLoadOnceStore{VersionedStore: docs.NewPgVersioned(database), invalid: make(map[string]bool)}
			return broken
		},
	}, "")
	if err := f.deps.Docs.(*docs.Service).Evict(context.Background(), f.specID); err != nil {
		t.Fatalf("evict the spec: %v", err)
	}
	broken.failNextLoad(f.specID)
	revised := strings.Replace(f.spec, "Revision token r0", "Revision token r1", 1)
	f.measure(t, http.StatusOK, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/artifacts/"+f.specID+"/rebuild", map[string]any{"markdown": revised}, "alice")
	})
}

// An approval request on a document with no open request inserts the ask, follows its author,
// indexes its question and appends ask.opened. What it decides on - the document, its settled
// version and the approval it already carries - is read before the first of those writes.
func TestXidWindowApprovalRequestOpened(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	f.measureBatched(t, http.StatusCreated, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/artifacts/"+f.specID+"/approval-requests", map[string]any{
			"summary": "The migration section is rewritten; the rest of the spec stands.", "actor": sessionActor(),
		})
	})
}

// A version moved the open request, so it waits on its agent: a request naming a new summary
// rewords it (docs.RewriteApprovalAsk) and hands it back. The rewording plans the question's
// references and reads the row's edit time in the read phase, and the hand-back's update and both
// events follow in the write phase.
func TestXidWindowApprovalRequestHandedBack(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	if opened := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/artifacts/"+f.specID+"/approval-requests", map[string]any{
		"summary": "The first pass is ready to read.", "actor": sessionActor(),
	}); opened.Code != http.StatusCreated {
		t.Fatalf("open the approval request: status=%d body=%s", opened.Code, opened.Body.String())
	}
	revised := strings.Replace(f.spec, "Revision token r0", "Revision token r1", 1)
	if uploaded := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/artifacts", map[string]any{
		"name": "spec.md", "content": revised, "summary": "Bump the revision", "actor": sessionActor(),
	}); uploaded.Code != http.StatusCreated {
		t.Fatalf("version the spec under the open request: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	f.measureBatched(t, http.StatusCreated, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/artifacts/"+f.specID+"/approval-requests", map[string]any{
			"summary": "Only the revision token moved; the rest stands as you read it.", "actor": sessionActor(),
		})
	})
}

func TestXidWindowIssueCreateWithSpec(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	spec, _ := xidSpec(2)
	f.measureAdvised(t, http.StatusCreated, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": xidProject, "title": "Second large spec for the reviewers", "spec": spec, "force": true,
			"actor": sessionActor(),
		})
	})
}

// A create naming a GitHub issue Dispatch already tracks answers that issue
// (respondWithExistingExternalIssue), with its advice. It writes nothing: today its row lock gives
// it an xid, and once the owner lock is advisory (plan M1) it may hold none at all.
func TestXidWindowExistingExternalIssue(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	if mapped := dispatchRequest(t, f.handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": xidProject,
	}, "alice"); mapped.Code != http.StatusOK {
		t.Fatalf("map the repository: status=%d body=%s", mapped.Code, mapped.Body.String())
	}
	body := map[string]any{"external": "owner/repo#41", "title": "Tracked upstream", "actor": sessionActor()}
	if created := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues", body); created.Code != http.StatusCreated {
		t.Fatalf("create the external issue: status=%d body=%s", created.Code, created.Body.String())
	}
	f.mayHoldNoXid = true
	f.measureAdvised(t, http.StatusOK, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues", body)
	})
}

func TestXidWindowIssuePatch(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	f.measureAdvised(t, http.StatusOK, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPatch, "/api/v1/issues/"+f.issueKey, map[string]any{
			"title": "Large spec under review, second pass", "actor": sessionActor(),
		})
	})
}

func TestXidWindowMessageCreate(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	f.measureAdvised(t, http.StatusCreated, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/messages", map[string]any{
			"body":  "Status: the spec at dispatch://" + f.issueKey + " has twenty open anchors; replies welcome.",
			"actor": sessionActor(),
		})
	})
}

func TestXidWindowClaim(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	f.measureAdvised(t, http.StatusOK, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/claim", map[string]any{}, "alice")
	})
}

// A close withdraws the issue's open asks: the eight anchored asks on its spec and the block ask
// settlement opened from the spec's `:::ask` block, whose block and copies it writes resolved into
// the spec (docs.WriteIssueAskStates) before it records each ask withdrawn. That document work
// reads and rewrites the whole spec, so it is the close's read phase, before its first write.
func TestXidWindowIssueCloseWithOpenAsks(t *testing.T) {
	f, _ := newBlockAskFixture(t)
	f.measure(t, http.StatusOK, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPatch, "/api/v1/issues/"+f.issueKey, map[string]any{"status": "done"}, "alice")
	})
}
