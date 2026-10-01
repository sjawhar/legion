package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func TestBlockPathReadsPlaceAnAnchoredCellByRowAndColumn(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Table position",
		"Intro.\n\n| # | Line | Due |\n| --- | --- | --- |\n| 1 | GDM backfill | Oct 1 |\n| 2 | Red-teamer loop | Today |\n")
	comment := decodeBody[struct {
		ID     string       `json:"id"`
		Anchor model.Anchor `json:"anchor"`
	}](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"anchor": map[string]any{"artifact": "spec", "quote": "Today"},
		"body":   "I want this done today",
	}, "alice"))
	ask := decodeBody[struct {
		ID string `json:"id"`
	}](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"anchor":   map[string]any{"artifact": "spec", "quote": "Today"},
		"question": "Which day is meant?",
	}, "alice"))
	if comment.Anchor.BlockID == nil {
		t.Fatal("the comment anchor records no block")
	}

	routed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks/"+*comment.Anchor.BlockID, nil, "alice")
	if routed.Code != http.StatusOK {
		t.Fatalf("block path: status=%d body=%s", routed.Code, routed.Body.String())
	}
	path := decodeBody[model.BlockPath](t, routed)
	types, indexes := make([]string, len(path.Path)), make([]int, len(path.Path))
	for i, entry := range path.Path {
		types[i], indexes[i] = entry.Type, entry.Index
		if entry.ID == "" {
			t.Fatalf("path entry %d has no id: %#v", i, path)
		}
	}
	if path.ID != *comment.Anchor.BlockID || path.Type != "paragraph" ||
		!reflect.DeepEqual(types, []string{"table", "table_row", "table_cell", "paragraph"}) || !reflect.DeepEqual(indexes, []int{1, 2, 2, 0}) {
		t.Fatalf("path = %#v, want table[1] › table_row[2] › table_cell[2] › paragraph[0]", path)
	}
	table := path.Table
	if table == nil || table.ID != path.Path[0].ID || table.Row == nil || *table.Row != 2 || table.Column == nil || *table.Column != 2 ||
		table.Header == nil || *table.Header != "Due" || !reflect.DeepEqual(table.Cells, []string{"2", "Red-teamer loop", "Today"}) {
		t.Fatalf("table position = %#v, want row 2, column 2 headed Due", table)
	}

	read := decodeBody[struct {
		Comment model.Comment `json:"comment"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, "alice"))
	if !reflect.DeepEqual(read.Comment.AnchorBlock, &path) {
		t.Fatalf("comment anchor_block = %#v, want the route's %#v", read.Comment.AnchorBlock, path)
	}
	askRead := decodeBody[struct {
		Ask model.Ask `json:"ask"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+ask.ID, nil, "alice"))
	if !reflect.DeepEqual(askRead.Ask.AnchorBlock, &path) {
		t.Fatalf("ask anchor_block = %#v, want the route's %#v", askRead.Ask.AnchorBlock, path)
	}

	// Negative controls: the block list did not grow, lists carry no position, an unknown block
	// is the usual 404, a floating comment has no position, a top-level paragraph has a
	// one-entry path with no table.
	if blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice")); len(blocks) != 2 {
		t.Fatalf("GET /blocks lists %d blocks, want the paragraph and the table alone: %#v", len(blocks), blocks)
	}
	if list := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/comments", nil, "alice"); strings.Contains(list.Body.String(), "anchor_block") {
		t.Fatalf("the comment list carries anchor_block: %s", list.Body.String())
	}
	missing := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks/missing", nil, "alice")
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), `"code":"TARGET_NOT_FOUND"`) {
		t.Fatalf("unknown block: status=%d body=%s", missing.Code, missing.Body.String())
	}
	floating := decodeBody[struct {
		ID string `json:"id"`
	}](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "No anchor."}, "alice"))
	if read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+floating.ID, nil, "alice"); read.Code != http.StatusOK || strings.Contains(read.Body.String(), "anchor_block") {
		t.Fatalf("a floating comment read: status=%d body=%s, want 200 without anchor_block", read.Code, read.Body.String())
	}
	intro := decodeBody[struct {
		ID string `json:"id"`
	}](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"anchor": map[string]any{"artifact": "spec", "quote": "Intro."}, "body": "Top-level.",
	}, "alice"))
	introRead := decodeBody[struct {
		Comment model.Comment `json:"comment"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+intro.ID, nil, "alice"))
	if got := introRead.Comment.AnchorBlock; got == nil || got.Table != nil || len(got.Path) != 1 || got.Path[0].Type != "paragraph" || got.Path[0].Index != 0 {
		t.Fatalf("top-level paragraph anchor_block = %#v, want paragraph[0] and no table", got)
	}

	// The block leaves the document (its row is deleted once nothing open anchors there): the
	// position leaves the read with it; the anchor row keeps its stale block_id.
	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]string{"text": "Today."}, "alice"); answered.Code != http.StatusOK {
		t.Fatalf("answer ask: status=%d body=%s", answered.Code, answered.Body.String())
	}
	if resolved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/resolve", nil, "alice"); resolved.Code != http.StatusOK {
		t.Fatalf("resolve comment: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	if edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "delete_row", "block": table.ID, "index": 2}},
	}, "alice"); edited.Code != http.StatusOK {
		t.Fatalf("delete row: status=%d body=%s", edited.Code, edited.Body.String())
	}
	stale := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, "alice")
	if stale.Code != http.StatusOK || strings.Contains(stale.Body.String(), "anchor_block") {
		t.Fatalf("a comment on a deleted row: status=%d body=%s, want 200 without anchor_block", stale.Code, stale.Body.String())
	}
	if staleRead := decodeBody[struct {
		Comment model.Comment `json:"comment"`
	}](t, stale); staleRead.Comment.Anchor == nil || staleRead.Comment.Anchor.BlockID == nil || *staleRead.Comment.Anchor.BlockID != *comment.Anchor.BlockID {
		t.Fatalf("the deleted row's comment anchor = %#v, want it to keep block_id %q", staleRead.Comment.Anchor, *comment.Anchor.BlockID)
	}
}

// The position is one derived field of a comment's or ask's read, so a document Dispatch cannot
// read leaves the read standing: 200, no anchor_block, and anchor_block_error saying why. Each
// case reads through a second server on the same database, where the document's room is not
// resident, so the read goes to its store: one that cannot load it (a persistence outage), or one
// whose live tree leaves the schema.
func TestCommentAndAskReadsStandWhenTheAnchorDocumentCannotBeRead(t *testing.T) {
	writer, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, writer, "TEST", "Table position",
		"Intro.\n\n| # | Line | Due |\n| --- | --- | --- |\n| 1 | GDM backfill | Oct 1 |\n| 2 | Red-teamer loop | Today |\n")
	created := func(path string, body map[string]any) string {
		t.Helper()
		return decodeBody[struct {
			ID string `json:"id"`
		}](t, dispatchRequest(t, writer, http.MethodPost, path, body, "alice")).ID
	}
	anchor := map[string]any{"artifact": "spec", "quote": "Today"}
	comment := created("/api/v1/issues/"+issue.Key+"/comments", map[string]any{"anchor": anchor, "body": "I want this done today"})
	ask := created("/api/v1/issues/"+issue.Key+"/asks", map[string]any{"anchor": anchor, "question": "Which day is meant?"})
	floating := created("/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "No anchor."})

	for _, test := range []struct {
		name  string
		store docs.VersionedStore
		want  string
	}{
		{"the store cannot load the document", failingLoadStore{VersionedStore: docs.NewPgVersioned(database)}, codeDocServiceUnavailable},
		{"the live tree leaves the schema", outsideSchemaStore{VersionedStore: docs.NewPgVersioned(database)}, codeDocSchema},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := readingServer(t, database, test.store, nil)
			for _, read := range []struct{ path, record string }{
				{"/api/v1/comments/" + comment, "comment"},
				{"/api/v1/asks/" + ask, "ask"},
			} {
				response := dispatchRequest(t, reader, http.MethodGet, read.path, nil, "alice")
				body := response.Body.String()
				if response.Code != http.StatusOK {
					t.Fatalf("GET %s: status=%d body=%s, want 200", read.path, response.Code, body)
				}
				var record map[string]json.RawMessage
				if err := json.Unmarshal(decodeBody[map[string]json.RawMessage](t, response)[read.record], &record); err != nil {
					t.Fatalf("GET %s: decode %s: %v", read.path, read.record, err)
				}
				if _, carried := record["anchor_block"]; carried || string(record["anchor_block_error"]) != `"`+test.want+`"` || record["anchor"] == nil {
					t.Fatalf("GET %s: body=%s, want its anchor, no anchor_block and anchor_block_error %q", read.path, body, test.want)
				}
			}
			// Negative control: a comment with no anchor reads no document and carries neither field.
			plain := dispatchRequest(t, reader, http.MethodGet, "/api/v1/comments/"+floating, nil, "alice")
			if plain.Code != http.StatusOK || strings.Contains(plain.Body.String(), "anchor_block") {
				t.Fatalf("a floating comment read: status=%d body=%s, want 200 without either field", plain.Code, plain.Body.String())
			}
		})
	}
}

// A room fails with the cause that failed it, and a writer whose client went away during its
// commit fails the room with a cause that holds context.Canceled. That cancellation is the
// writer's, so a live read of the same document still answers 200 without the position, while a
// read whose own request has gone away fails. The two reads meet the same error; only the request
// differs.
func TestAnAnchoredReadDecidesCancellationByItsOwnRequest(t *testing.T) {
	writer, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, writer, "TEST", "Cancelled writer", "Intro.\n\nThe quoted line.\n")
	created := func(path string, body map[string]any) string {
		t.Helper()
		return decodeBody[struct {
			ID string `json:"id"`
		}](t, dispatchRequest(t, writer, http.MethodPost, path, body, "alice")).ID
	}
	anchor := map[string]any{"artifact": "spec", "quote": "The quoted line."}
	reads := []struct{ path, record string }{
		{"/api/v1/comments/" + created("/api/v1/issues/"+issue.Key+"/comments", map[string]any{"anchor": anchor, "body": "Quoted."}), "comment"},
		{"/api/v1/asks/" + created("/api/v1/issues/"+issue.Key+"/asks", map[string]any{"anchor": anchor, "question": "Keep it?"}), "ask"},
	}
	// What a room failed by a writer's cancelled commit answers a read that may not wait for it.
	failedRoom := fmt.Errorf("%w: %w", docs.ErrServiceUnavailable, fmt.Errorf("commit live document write: %w", context.Canceled))

	for _, read := range reads {
		live := readingServer(t, database, docs.NewPgVersioned(database), func(documents docs.API) docs.API {
			return failingBlockPath{API: documents, err: failedRoom}
		})
		response := dispatchRequest(t, live, http.MethodGet, read.path, nil, "alice")
		body := response.Body.String()
		if response.Code != http.StatusOK {
			t.Fatalf("live GET %s: status=%d body=%s, want 200", read.path, response.Code, body)
		}
		var record map[string]json.RawMessage
		if err := json.Unmarshal(decodeBody[map[string]json.RawMessage](t, response)[read.record], &record); err != nil {
			t.Fatalf("live GET %s: decode %s: %v", read.path, read.record, err)
		}
		if _, carried := record["anchor_block"]; carried || string(record["anchor_block_error"]) != `"`+codeDocServiceUnavailable+`"` {
			t.Fatalf("live GET %s: body=%s, want no anchor_block and anchor_block_error %q", read.path, body, codeDocServiceUnavailable)
		}

		ctx, cancel := context.WithCancel(context.Background())
		gone := readingServer(t, database, docs.NewPgVersioned(database), func(documents docs.API) docs.API {
			return failingBlockPath{API: documents, err: failedRoom, during: cancel}
		})
		request := httptest.NewRequest(http.MethodGet, read.path, nil).WithContext(ctx)
		request.Header.Set("X-Dispatch-User", "alice")
		cancelled := httptest.NewRecorder()
		gone.ServeHTTP(cancelled, request)
		if cancelled.Code != http.StatusInternalServerError || !strings.Contains(cancelled.Body.String(), `"code":"`+codeInternal+`"`) {
			t.Fatalf("cancelled GET %s: status=%d body=%s, want 500 %s", read.path, cancelled.Code, cancelled.Body.String(), codeInternal)
		}
	}
}

// A comment's or ask's anchor_block_error names a document error as the block route answers the
// same error, since both reads place one block and writeHandlerError serves the route's error. A
// failed room is DOC_SERVICE_UNAVAILABLE on both, whatever failed it: settlement's schema refusal
// and a publish refused because the issue closed are causes another operation met, and the room
// serves the document again once it is evicted. A live tree outside the schema is DOC_SCHEMA on
// both, and any other failure INTERNAL.
func TestAnAnchoredReadNamesADocumentErrorAsTheBlockRouteDoes(t *testing.T) {
	writer, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, writer, "TEST", "One code", "Intro.\n\nThe quoted line.\n")
	created := func(path string, body map[string]any) (string, string) {
		t.Helper()
		record := decodeBody[struct {
			ID     string       `json:"id"`
			Anchor model.Anchor `json:"anchor"`
		}](t, dispatchRequest(t, writer, http.MethodPost, path, body, "alice"))
		if record.Anchor.BlockID == nil {
			t.Fatalf("POST %s: anchor pinned no block", path)
		}
		return record.ID, *record.Anchor.BlockID
	}
	anchor := map[string]any{"artifact": "spec", "quote": "The quoted line."}
	comment, blockID := created("/api/v1/issues/"+issue.Key+"/comments", map[string]any{"anchor": anchor, "body": "Quoted."})
	ask, _ := created("/api/v1/issues/"+issue.Key+"/asks", map[string]any{"anchor": anchor, "question": "Keep it?"})
	// What awaitRoomRecovery answers a read that may not wait for a room failed with cause.
	failedRoom := func(cause error) error { return fmt.Errorf("%w: %w", docs.ErrServiceUnavailable, cause) }
	outsideSchema := fmt.Errorf("%w: callout needs at least one block", docs.ErrDocSchema)

	for _, test := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"a room settlement's schema refusal failed", failedRoom(outsideSchema), http.StatusServiceUnavailable, codeDocServiceUnavailable},
		{"a room a publish refused for a closed issue failed", failedRoom(fmt.Errorf("apply committed live document write: %w", docs.ErrIssueClosed)), http.StatusServiceUnavailable, codeDocServiceUnavailable},
		{"a live tree outside the schema", outsideSchema, http.StatusInternalServerError, codeDocSchema},
		{"an unclassified failure", errors.New("unexpected document failure"), http.StatusInternalServerError, codeInternal},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := readingServer(t, database, docs.NewPgVersioned(database), func(documents docs.API) docs.API {
				return failingBlockPath{API: documents, err: test.err}
			})
			route := dispatchRequest(t, reader, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks/"+blockID, nil, "alice")
			if route.Code != test.status || !strings.Contains(route.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("block route: status=%d body=%s, want %d %s", route.Code, route.Body.String(), test.status, test.code)
			}
			for _, read := range []struct{ path, record string }{
				{"/api/v1/comments/" + comment, "comment"},
				{"/api/v1/asks/" + ask, "ask"},
			} {
				response := dispatchRequest(t, reader, http.MethodGet, read.path, nil, "alice")
				if response.Code != http.StatusOK {
					t.Fatalf("GET %s: status=%d body=%s, want 200", read.path, response.Code, response.Body.String())
				}
				var record map[string]json.RawMessage
				if err := json.Unmarshal(decodeBody[map[string]json.RawMessage](t, response)[read.record], &record); err != nil {
					t.Fatalf("GET %s: decode %s: %v", read.path, read.record, err)
				}
				if string(record["anchor_block_error"]) != `"`+test.code+`"` {
					t.Fatalf("GET %s: anchor_block_error=%s, want %q, the block route's code", read.path, record["anchor_block_error"], test.code)
				}
			}
		})
	}
}

// failingBlockPath is a document service whose block placement fails with err, running during
// first when it is set, as the reading request's own cancellation would land mid-read.
type failingBlockPath struct {
	docs.API
	err    error
	during func()
}

func (f failingBlockPath) BlockPath(context.Context, string, string) (model.BlockPath, error) {
	if f.during != nil {
		f.during()
	}
	return model.BlockPath{}, f.err
}

// readingServer is a second server on database whose document service reads through persist,
// behind wrap's document service when wrap is set.
func readingServer(t *testing.T, database *store.Store, persist docs.VersionedStore, wrap func(docs.API) docs.API) http.Handler {
	t.Helper()
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Persistence: persist, Events: broker})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	var service docs.API = documents
	if wrap != nil {
		service = wrap(documents)
	}
	allowed := map[string]struct{}{"alice": {}}
	deps, err := NewDeps(DepsInput{
		Store:         database,
		Identity:      identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: allowed},
		AllowedLogins: allowed,
		Docs:          service,
		Events:        broker,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	Register(mux, deps)
	return mux
}

// outsideSchemaStore loads every document as a live tree the Proof schema refuses: an empty
// callout, which needs at least one block.
type outsideSchemaStore struct {
	docs.VersionedStore
}

func (outsideSchemaStore) Load(context.Context, string) (persistence.LoadResult, error) {
	doc := crdt.New()
	fragment := doc.GetXmlFragment("prosemirror")
	doc.Transact(func(txn *crdt.Transaction) {
		fragment.InsertElement(txn, 0, crdt.NewYXmlElement("callout"))
	})
	return persistence.LoadResult{Update: crdt.EncodeStateAsUpdateV1(doc, nil)}, nil
}
