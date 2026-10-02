package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A rebuild is only for a history ygo cannot load. A healthy, idle document can carry updates
// newer than its latest version, so the human-only route refuses it without deleting any history.
func TestRebuildArtifactRouteIsHumanOnlyAndRefusesADocumentThatLoads(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	issue := createArtifactIssue(t, handler)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "healthy.md", "content": "before\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create healthy document: status=%d body=%s", created.Code, created.Body.String())
	}
	artifact := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, created).Artifact
	documentService := deps.Docs.(*docs.Service)
	if _, err := documentService.ReplaceText(context.Background(), artifact.ID, "unsettled\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write unsettled document update: %v", err)
	}
	if err := documentService.Evict(context.Background(), artifact.ID); err != nil {
		t.Fatalf("evict healthy document: %v", err)
	}
	var before int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from doc_updates where artifact_id = $1`, artifact.ID).Scan(&before); err != nil {
		t.Fatalf("count document updates before rebuild: %v", err)
	}

	path := "/api/v1/artifacts/" + artifact.ID + "/rebuild"
	bearer := sessionRequest(t, handler, http.MethodPost, path, map[string]any{"actor": sessionActor()})
	if bearer.Code != http.StatusForbidden || !strings.Contains(bearer.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("bearer rebuild: status=%d body=%s, want 403 HUMAN_ONLY", bearer.Code, bearer.Body.String())
	}
	human := dispatchRequest(t, handler, http.MethodPost, path, map[string]any{}, "alice")
	if human.Code != http.StatusConflict || !strings.Contains(human.Body.String(), `"code":"DOCUMENT_LOADS"`) {
		t.Fatalf("healthy document rebuild: status=%d body=%s, want 409 DOCUMENT_LOADS", human.Code, human.Body.String())
	}
	var after int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from doc_updates where artifact_id = $1`, artifact.ID).Scan(&after); err != nil {
		t.Fatalf("count document updates after refused rebuild: %v", err)
	}
	if after != before {
		t.Fatalf("refused rebuild changed doc_updates from %d to %d", before, after)
	}
	if text, err := documentService.Text(context.Background(), artifact.ID); err != nil || text != "unsettled\n" {
		t.Fatalf("healthy document after refused rebuild: %q (%v), want its unsettled update", text, err)
	}
}

type invalidLoadOnceStore struct {
	docs.VersionedStore
	mu      sync.Mutex
	invalid map[string]bool
}

func (s *invalidLoadOnceStore) failNextLoad(room string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalid[room] = true
}

func (s *invalidLoadOnceStore) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	s.mu.Lock()
	invalid := s.invalid[room]
	delete(s.invalid, room)
	s.mu.Unlock()
	if invalid {
		return persistence.LoadResult{Update: []byte{0xff}}, nil
	}
	return s.VersionedStore.Load(ctx, room)
}

// A caller-supplied rebuild source is a document change: it writes the next immutable version,
// emits its artifact.version event, and retracts the approval ask pinned to the prior version.
// Omitting markdown intentionally keeps the latest-version rebuild behavior, including its
// existing version and approval state.
func TestRebuildArtifactVersionsSuppliedMarkdownButNotTheLatestVersionSource(t *testing.T) {
	var broken *invalidLoadOnceStore
	handler, database, _ := newTestServer(t, testServerOptions{
		settle: time.Hour,
		persistence: func(database *store.Store) docs.VersionedStore {
			broken = &invalidLoadOnceStore{
				VersionedStore: docs.NewPgVersioned(database),
				invalid:        make(map[string]bool),
			}
			return broken
		},
	})
	issue := createArtifactIssue(t, handler)
	createBrokenDocument := func(t *testing.T, name, markdown string) model.Artifact {
		t.Helper()
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
			"name": name, "content": markdown,
		}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create document %q: status=%d body=%s", name, created.Code, created.Body.String())
		}
		artifact := decodeBody[struct {
			Artifact model.Artifact `json:"artifact"`
		}](t, created).Artifact
		broken.failNextLoad(artifact.ID)
		return artifact
	}
	requestApproval := func(t *testing.T, artifactID string) {
		t.Helper()
		response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/approval-requests", map[string]any{
			"actor": sessionActor(),
		})
		if response.Code != http.StatusCreated {
			t.Fatalf("request approval: status=%d body=%s", response.Code, response.Body.String())
		}
	}
	count := func(t *testing.T, query, artifactID string) int {
		t.Helper()
		var value int
		if err := database.Pool.QueryRow(context.Background(), query, artifactID).Scan(&value); err != nil {
			t.Fatalf("count rows: %v", err)
		}
		return value
	}

	latest := createBrokenDocument(t, "latest.md", "latest source\n")
	requestApproval(t, latest.ID)
	rebuilt := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+latest.ID+"/rebuild", map[string]any{}, "alice")
	if rebuilt.Code != http.StatusOK || !strings.Contains(rebuilt.Body.String(), `"source_version":1`) {
		t.Fatalf("rebuild from latest version: status=%d body=%s", rebuilt.Code, rebuilt.Body.String())
	}
	if versions := count(t, `select count(*) from artifact_versions where artifact_id = $1`, latest.ID); versions != 1 {
		t.Fatalf("latest-source rebuild versions = %d, want its original version only", versions)
	}
	if events := count(t, `select count(*) from events where type = 'artifact.version' and payload->>'artifact_id' = $1`, latest.ID); events != 0 {
		t.Fatalf("latest-source rebuild events = %d, want no content-change event", events)
	}
	if approval := readApproval(t, handler, latest.ID).Approval; approval == nil || approval.State != "awaiting" || approval.LatestVersion != 1 {
		t.Fatalf("latest-source rebuild approval = %#v, want its original awaiting approval", approval)
	}

	supplied := createBrokenDocument(t, "supplied.md", "before\n")
	requestApproval(t, supplied.ID)
	rebuilt = dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+supplied.ID+"/rebuild", map[string]any{
		"markdown": "",
	}, "alice")
	if rebuilt.Code != http.StatusOK || !strings.Contains(rebuilt.Body.String(), `"source_version":0`) {
		t.Fatalf("rebuild from supplied empty markdown: status=%d body=%s", rebuilt.Code, rebuilt.Body.String())
	}
	if versions := count(t, `select count(*) from artifact_versions where artifact_id = $1`, supplied.ID); versions != 2 {
		t.Fatalf("supplied rebuild versions = %d, want a new immutable version", versions)
	}
	if events := count(t, `select count(*) from events where type = 'artifact.version' and payload->>'artifact_id' = $1`, supplied.ID); events != 1 {
		t.Fatalf("supplied rebuild events = %d, want one artifact.version event", events)
	}
	if approval := readApproval(t, handler, supplied.ID).Approval; approval == nil || approval.State != "draft" || approval.LatestVersion != 2 {
		t.Fatalf("supplied rebuild approval = %#v, want stale approval invalidated at version 2", approval)
	}
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+supplied.ID+"/text", nil, "alice")
	if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), `"markdown":""`) {
		t.Fatalf("read supplied empty rebuild: status=%d body=%s", text.Code, text.Body.String())
	}
}

func TestClosedIssueRebuildRefusesBeforeChangingTheDocument(t *testing.T) {
	var broken *invalidLoadOnceStore
	handler, _, _ := newTestServer(t, testServerOptions{
		settle: time.Hour,
		persistence: func(database *store.Store) docs.VersionedStore {
			broken = &invalidLoadOnceStore{VersionedStore: docs.NewPgVersioned(database), invalid: make(map[string]bool)}
			return broken
		},
	})
	issue := createArtifactIssue(t, handler)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "closed.md", "content": "before\n",
	}, "alice")
	artifact := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, created).Artifact
	if closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", closed.Code, closed.Body.String())
	}
	broken.failNextLoad(artifact.ID)
	rebuilt := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifact.ID+"/rebuild", map[string]any{"markdown": "changed\n"}, "alice")
	if rebuilt.Code != http.StatusConflict || !strings.Contains(rebuilt.Body.String(), `"code":"ISSUE_CLOSED"`) {
		t.Fatalf("closed issue rebuild: status=%d body=%s, want 409 ISSUE_CLOSED", rebuilt.Code, rebuilt.Body.String())
	}
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifact.ID+"/text", nil, "alice")
	if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), `"markdown":"before\n"`) {
		t.Fatalf("closed document after refused rebuild: status=%d body=%s, want unchanged before text", text.Code, text.Body.String())
	}
}

// A rebuild from supplied markdown commits with its version: a failure after its destructive write
// and before the version is written leaves the document's history, versions and events as they
// were.
func TestSuppliedRebuildThatFailsBeforeItsVersionLeavesTheDocument(t *testing.T) {
	var broken *invalidLoadOnceStore
	handler, database, _ := newTestServer(t, testServerOptions{
		settle: time.Hour,
		persistence: func(database *store.Store) docs.VersionedStore {
			broken = &invalidLoadOnceStore{VersionedStore: docs.NewPgVersioned(database), invalid: make(map[string]bool)}
			return broken
		},
	})
	issue := createArtifactIssue(t, handler)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "failing.md", "content": "before\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create document: status=%d body=%s", created.Code, created.Body.String())
	}
	artifact := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, created).Artifact
	history := func(t *testing.T) string {
		t.Helper()
		var rows string
		if err := database.Pool.QueryRow(context.Background(), `
			select coalesce(string_agg(version::text || ':' || md5(update), ',' order by version), '')
			from doc_updates where artifact_id = $1
		`, artifact.ID).Scan(&rows); err != nil {
			t.Fatalf("read document history: %v", err)
		}
		return rows
	}
	before := history(t)
	if _, err := database.Pool.Exec(context.Background(), `
		create function dispatch_test_reject_rebuild_version() returns trigger language plpgsql as $$
		begin
			raise exception 'reject the rebuild version';
		end $$;
		create trigger dispatch_test_reject_rebuild_version
		before insert on artifact_versions for each row
		execute function dispatch_test_reject_rebuild_version();
	`); err != nil {
		t.Fatalf("make the version write fail: %v", err)
	}

	broken.failNextLoad(artifact.ID)
	rebuilt := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifact.ID+"/rebuild", map[string]any{"markdown": "changed\n"}, "alice")
	if rebuilt.Code != http.StatusInternalServerError {
		t.Fatalf("rebuild whose version fails: status=%d body=%s, want 500", rebuilt.Code, rebuilt.Body.String())
	}
	if after := history(t); after != before {
		t.Fatalf("failed rebuild changed the document history from %q to %q", before, after)
	}
	var versions, events int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id = $1`, artifact.ID).Scan(&versions); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from events where type = 'artifact.version' and payload->>'artifact_id' = $1`, artifact.ID).Scan(&events); err != nil {
		t.Fatalf("count version events: %v", err)
	}
	if versions != 1 || events != 0 {
		t.Fatalf("failed rebuild left %d versions and %d artifact.version events, want 1 and 0", versions, events)
	}
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifact.ID+"/text", nil, "alice")
	if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), `"markdown":"before\n"`) {
		t.Fatalf("document after failed rebuild: status=%d body=%s, want unchanged before text", text.Code, text.Body.String())
	}
}
