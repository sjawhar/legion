package architecture

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// fakeSource is the GitHub App surface a sync walks: installation lookup,
// token mint, branch commit, the architecture directory's subtree, and blob
// reads. files is keyed by the file name inside SourceDir; the subtree sha is
// derived from the files, so identical files under a new commit answer the
// same sha exactly like git would.
type fakeSource struct {
	t              *testing.T
	installationID int64
	contents       string
	commit         string
	files          map[string]string
	branchMissing  bool
	treeFailure    bool
	// treeShaFollowsCommit makes every commit answer a distinct subtree sha
	// even for identical files, so a test can force the full projection path.
	treeShaFollowsCommit bool
	// blobHold makes every blob read block until the request is cancelled: a
	// GitHub that never answers, for the sync deadline.
	blobHold bool

	mu        sync.Mutex
	treeReads int
	blobReads int
}

func (f *fakeSource) reads() (trees, blobs int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.treeReads, f.blobReads
}

func (f *fakeSource) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		contents := f.contents
		if contents == "" {
			contents = "read"
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"id": f.installationID, "app_slug": "dispatch-test",
			"permissions": map[string]string{"contents": contents},
		}); err != nil {
			f.t.Errorf("encode installation: %v", err)
		}
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"token": "ghs_fake_source", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		}); err != nil {
			f.t.Errorf("encode token: %v", err)
		}
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{branch}", func(w http.ResponseWriter, _ *http.Request) {
		if f.branchMissing {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"sha":%q}`, f.commit)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/trees/{ref}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.treeReads++
		f.mu.Unlock()
		commit, dir, ok := strings.Cut(r.PathValue("ref"), ":")
		if !ok || dir != SourceDir {
			f.t.Errorf("tree read %q is not the %s subtree", r.PathValue("ref"), SourceDir)
		}
		if f.treeFailure {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message":"upstream is sad"}`)
			return
		}
		names := make([]string, 0, len(f.files))
		for name := range f.files {
			names = append(names, name)
		}
		sort.Strings(names)
		digest := sha256.New()
		if f.treeShaFollowsCommit {
			fmt.Fprintf(digest, "%s\x00", commit)
		}
		entries := []map[string]any{}
		for _, name := range names {
			fmt.Fprintf(digest, "%s\x00%s\x00", name, f.files[name])
			entries = append(entries, map[string]any{
				"path": name, "mode": "100644", "type": "blob", "sha": "blob-" + name, "size": len(f.files[name]),
			})
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"sha": "tree-" + hex.EncodeToString(digest.Sum(nil))[:12], "truncated": false, "tree": entries,
		}); err != nil {
			f.t.Errorf("encode tree: %v", err)
		}
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/blobs/{sha}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.blobReads++
		f.mu.Unlock()
		if f.blobHold {
			<-r.Context().Done()
			return
		}
		content, ok := f.files[strings.TrimPrefix(r.PathValue("sha"), "blob-")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"content": base64.StdEncoding.EncodeToString([]byte(content)), "encoding": "base64",
		}); err != nil {
			f.t.Errorf("encode blob: %v", err)
		}
	})
	return mux
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	baseURL, err := url.Parse(os.Getenv("DISPATCH_TEST_DATABASE_URL"))
	if err != nil || baseURL.String() == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL must be set to run Postgres architecture tests")
	}
	adminURL := *baseURL
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(context.Background(), adminURL.String())
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	t.Cleanup(admin.Close)
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("random database name: %v", err)
	}
	name := "dispatch_arch_test_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(context.Background(), "create database "+name); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
	})
	testURL := *baseURL
	testURL.Path = "/" + name
	database, err := store.Open(context.Background(), testURL.String())
	if err != nil {
		t.Fatalf("open isolated database: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate isolated database: %v", err)
	}
	return database
}

// newGitHubClient serves handler as the GitHub API and returns an App client
// pointed at it.
func newGitHubClient(t *testing.T, handler http.Handler) *githubapp.Client {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.test", ClientSecret: "secret", PEM: pemText}, server.URL)
	if err != nil {
		t.Fatalf("new github client: %v", err)
	}
	return client
}

// newImporterFixture seeds project CORE with an architecture source pointing
// at the fake GitHub, and returns the importer plus the database.
func newImporterFixture(t *testing.T, fake *fakeSource) (*Importer, *store.Store) {
	t.Helper()
	if fake.installationID == 0 {
		fake.installationID = 11
	}
	if fake.commit == "" {
		fake.commit = "commit-one"
	}
	client := newGitHubClient(t, fake.handler())

	database := openTestStore(t)
	if _, err := database.Pool.Exec(context.Background(),
		`insert into projects (key, name) values ('CORE', 'Core')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('CORE', 'legion/arch', 'main', $1, '{"kind":"user","id":"alice"}')
	`, fake.installationID); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	return NewImporter(database, client, events.NewBroker()), database
}

func projectEvents(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select type, coalesce(payload->>'commit', payload->>'error'), notify
		from events where type like 'architecture.%' order by id
	`)
	if err != nil {
		t.Fatalf("query architecture events: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var eventType, detail string
		var notify bool
		if err := rows.Scan(&eventType, &detail, &notify); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		if notify {
			t.Errorf("%s woke agents; importer events are retained, never waking", eventType)
		}
		got = append(got, eventType+" "+detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate events: %v", err)
	}
	return got
}

func componentRows(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select id, title, prose, coalesce(parent, ''), external, array_to_string(paths, ',')
		from components where project_key = 'CORE' order by id
	`)
	if err != nil {
		t.Fatalf("query components: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id, title, prose, parent, paths string
		var external bool
		if err := rows.Scan(&id, &title, &prose, &parent, &external, &paths); err != nil {
			t.Fatalf("scan component: %v", err)
		}
		got = append(got, fmt.Sprintf("%s|%s|%s|%s|%t|%s", id, title, strings.TrimSpace(prose), parent, external, paths))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate components: %v", err)
	}
	return got
}

const validAPI = "---\ntitle: HTTP API\nparent: server\ndepends_on: [store]\npaths: [internal/api]\n---\nThe API.\n"

func validFiles() map[string]string {
	return map[string]string{
		"api.md":    validAPI,
		"server.md": "---\ntitle: Server\n---\nThe server.\n",
		"store.md":  "---\ntitle: Store\nexternal: true\n---\nThe store.\n",
	}
}

func TestSyncProjectsComponentsDependenciesAndSnapshot(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles()}
	importer, database := newImporterFixture(t, fake)

	source, err := importer.Sync(ctx, "CORE")
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if source.LastCommit == nil || *source.LastCommit != "commit-one" || source.LastError != nil || source.LastSyncAt == nil {
		t.Fatalf("source after sync: %+v", source)
	}
	want := []string{
		"api|HTTP API|The API.|server|false|internal/api",
		"server|Server|The server.||false|",
		"store|Store|The store.||true|",
	}
	if got := componentRows(t, database); !reflect.DeepEqual(got, want) {
		t.Fatalf("components = %#v\nwant %#v", got, want)
	}

	var dependencies string
	if err := database.Pool.QueryRow(ctx, `
		select string_agg(from_id || '->' || to_id, ',' order by from_id, to_id) from component_depends
	`).Scan(&dependencies); err != nil || dependencies != "api->store" {
		t.Fatalf("dependencies = %q err %v", dependencies, err)
	}

	var files map[string]string
	var commit string
	if err := database.Pool.QueryRow(ctx,
		`select commit, files from architecture_snapshots where project_key = 'CORE'`).Scan(&commit, &files); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if commit != "commit-one" || files["api.md"] != validAPI {
		t.Fatalf("snapshot: commit=%q files=%#v", commit, files)
	}

	if got := projectEvents(t, database); !reflect.DeepEqual(got, []string{"architecture.synced commit-one"}) {
		t.Fatalf("events = %#v", got)
	}
}

// An unchanged head of a healthy source is the short-circuit: the freshness
// stamp moves, nothing is fetched past the head lookup, nothing is
// re-projected, no event — which is also what bounds a caller looping the
// sync route.
func TestSyncSameCommitIsIdempotentEventlessAndFetchesNothing(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles()}
	importer, database := newImporterFixture(t, fake)

	if _, err := importer.Sync(ctx, "CORE"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	before := componentRows(t, database)
	treesBefore, blobsBefore := fake.reads()
	var firstSync time.Time
	if err := database.Pool.QueryRow(ctx, `select last_sync_at from architecture_sources where project_key = 'CORE'`).Scan(&firstSync); err != nil {
		t.Fatalf("read last_sync_at: %v", err)
	}

	for range 5 {
		source, err := importer.Sync(ctx, "CORE")
		if err != nil {
			t.Fatalf("re-sync: %v", err)
		}
		if source.LastSyncAt == nil || !source.LastSyncAt.After(firstSync) {
			t.Fatalf("same-commit re-import did not refresh last_sync_at: %v -> %v", firstSync, source.LastSyncAt)
		}
	}
	if trees, blobs := fake.reads(); trees != treesBefore || blobs != blobsBefore {
		t.Fatalf("unchanged head re-fetched: tree reads %d -> %d, blob reads %d -> %d", treesBefore, trees, blobsBefore, blobs)
	}
	if got := componentRows(t, database); !reflect.DeepEqual(got, before) {
		t.Fatalf("re-import changed the projection: %#v", got)
	}
	var snapshots int
	if err := database.Pool.QueryRow(ctx, `select count(*) from architecture_snapshots`).Scan(&snapshots); err != nil || snapshots != 1 {
		t.Fatalf("snapshots = %d err %v, want 1", snapshots, err)
	}
	if got := projectEvents(t, database); !reflect.DeepEqual(got, []string{"architecture.synced commit-one"}) {
		t.Fatalf("same-commit re-import emitted an event: %#v", got)
	}
}

// The head moved but the architecture directory did not (a push elsewhere in
// the repository): the subtree sha matches, so the new commit is recorded
// without a blob read, a snapshot, a re-projection, or an event.
func TestSyncMovedHeadWithUnchangedTreeRecordsTheCommitOnly(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles()}
	importer, database := newImporterFixture(t, fake)
	if _, err := importer.Sync(ctx, "CORE"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	before := componentRows(t, database)
	_, blobsBefore := fake.reads()

	fake.commit = "commit-two"
	source, err := importer.Sync(ctx, "CORE")
	if err != nil {
		t.Fatalf("moved-head sync: %v", err)
	}
	if source.LastCommit == nil || *source.LastCommit != "commit-two" || source.LastError != nil {
		t.Fatalf("source after moved head: %+v", source)
	}
	if _, blobs := fake.reads(); blobs != blobsBefore {
		t.Fatalf("unchanged tree re-read blobs: %d -> %d", blobsBefore, blobs)
	}
	if got := componentRows(t, database); !reflect.DeepEqual(got, before) {
		t.Fatalf("unchanged tree changed the projection: %#v", got)
	}
	var snapshots int
	if err := database.Pool.QueryRow(ctx, `select count(*) from architecture_snapshots`).Scan(&snapshots); err != nil || snapshots != 1 {
		t.Fatalf("snapshots = %d err %v, want 1 (the model did not change)", snapshots, err)
	}
	if got := projectEvents(t, database); !reflect.DeepEqual(got, []string{"architecture.synced commit-one"}) {
		t.Fatalf("unchanged tree emitted an event: %#v", got)
	}

	// A real model change under the next head goes the whole way again.
	fake.commit = "commit-three"
	fake.files = map[string]string{"server.md": "---\ntitle: Server\n---\nThe server.\n"}
	if _, err := importer.Sync(ctx, "CORE"); err != nil {
		t.Fatalf("changed-tree sync: %v", err)
	}
	if got := componentRows(t, database); !reflect.DeepEqual(got, []string{"server|Server|The server.||false|"}) {
		t.Fatalf("components after the tree changed = %#v", got)
	}
	if got := projectEvents(t, database); !reflect.DeepEqual(got, []string{
		"architecture.synced commit-one", "architecture.synced commit-three",
	}) {
		t.Fatalf("events = %#v", got)
	}
}

func TestSyncInvalidSetKeepsThePreviousProjection(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles()}
	importer, database := newImporterFixture(t, fake)
	if _, err := importer.Sync(ctx, "CORE"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	before := componentRows(t, database)

	fake.commit = "commit-broken"
	fake.files = map[string]string{"api.md": "x\n", "API.md": "duplicate\n"}
	source, err := importer.Sync(ctx, "CORE")
	if err == nil {
		t.Fatal("invalid set was accepted")
	}
	if source.LastError == nil || !strings.Contains(*source.LastError, "duplicate component id") {
		t.Fatalf("last_error = %v", source.LastError)
	}
	if source.LastCommit == nil || *source.LastCommit != "commit-one" {
		t.Fatalf("last_commit moved to the rejected commit: %v", source.LastCommit)
	}
	if got := componentRows(t, database); !reflect.DeepEqual(got, before) {
		t.Fatalf("invalid set disturbed the projection: %#v", got)
	}
	if got := projectEvents(t, database); len(got) != 2 || !strings.HasPrefix(got[1], "architecture.sync_failed ") {
		t.Fatalf("events = %#v", got)
	}
}

func TestSyncRepeatedIdenticalFailureEmitsOneEventAndRecoveryEmitsSynced(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, branchMissing: true, files: validFiles()}
	importer, database := newImporterFixture(t, fake)

	for attempt := range 3 {
		if _, err := importer.Sync(ctx, "CORE"); err == nil {
			t.Fatalf("attempt %d: missing branch was accepted", attempt)
		}
	}
	got := projectEvents(t, database)
	if len(got) != 1 || !strings.HasPrefix(got[0], "architecture.sync_failed ") {
		t.Fatalf("a persistently broken source emitted %#v", got)
	}

	// Recovery: the same commit it never managed to import, so only the
	// cleared error can explain the event.
	fake.branchMissing = false
	if _, err := importer.Sync(ctx, "CORE"); err != nil {
		t.Fatalf("recovery sync: %v", err)
	}
	if got := projectEvents(t, database); len(got) != 2 || got[1] != "architecture.synced commit-one" {
		t.Fatalf("recovery events = %#v", got)
	}

	// And the recovered source stays silent while nothing changes.
	if _, err := importer.Sync(ctx, "CORE"); err != nil {
		t.Fatalf("steady sync: %v", err)
	}
	if got := projectEvents(t, database); len(got) != 2 {
		t.Fatalf("steady sync emitted an event: %#v", got)
	}
}

func TestSyncRemovedFileRetiresItsComponent(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles()}
	importer, database := newImporterFixture(t, fake)
	if _, err := importer.Sync(ctx, "CORE"); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	fake.commit = "commit-two"
	fake.files = map[string]string{"server.md": "---\ntitle: Server\n---\nThe server.\n"}
	if _, err := importer.Sync(ctx, "CORE"); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if got := componentRows(t, database); !reflect.DeepEqual(got, []string{"server|Server|The server.||false|"}) {
		t.Fatalf("components after removal = %#v", got)
	}
	var dependencies int
	if err := database.Pool.QueryRow(ctx, `select count(*) from component_depends`).Scan(&dependencies); err != nil || dependencies != 0 {
		t.Fatalf("dependency rows = %d err %v, want 0", dependencies, err)
	}
	if got := projectEvents(t, database); !reflect.DeepEqual(got, []string{
		"architecture.synced commit-one", "architecture.synced commit-two",
	}) {
		t.Fatalf("events = %#v", got)
	}
}

func TestSyncOfOneProjectIsSerialized(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles()}
	importer, database := newImporterFixture(t, fake)

	var wait sync.WaitGroup
	failures := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := importer.Sync(ctx, "CORE"); err != nil {
				failures <- err
			}
		}()
	}
	wait.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("concurrent sync: %v", err)
	}

	var snapshots int
	if err := database.Pool.QueryRow(ctx, `select count(*) from architecture_snapshots`).Scan(&snapshots); err != nil || snapshots != 1 {
		t.Fatalf("snapshots = %d err %v, want 1", snapshots, err)
	}
	if got := projectEvents(t, database); !reflect.DeepEqual(got, []string{"architecture.synced commit-one"}) {
		t.Fatalf("concurrent syncs emitted %#v", got)
	}
}

// Two server processes (two Importer instances, no shared mutex) syncing one
// project while its head keeps moving: each projection takes the source's
// row lock first, so delete+reinsert never deadlocks in Postgres, every sync
// succeeds, and the projection is always one whole snapshot's worth of
// components.
func TestSyncAcrossProcessesSerializesOnTheSourceRow(t *testing.T) {
	ctx := context.Background()
	files := map[string]string{}
	for n := range 60 {
		files[fmt.Sprintf("c%d.md", n)] = fmt.Sprintf("---\ntitle: C%d\n---\nprose", n)
	}
	fake := &fakeSource{t: t, files: files, treeShaFollowsCommit: true}
	var heads atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{branch}", func(w http.ResponseWriter, _ *http.Request) {
		// A push lands between the two processes' head lookups.
		fmt.Fprintf(w, `{"sha":"commit-%d"}`, heads.Add(1))
	})
	mux.Handle("/", fake.handler())
	client := newGitHubClient(t, mux)
	database := openTestStore(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('CORE', 'legion/arch', 'main', 11, '{"kind":"user","id":"alice"}');
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	processes := []*Importer{
		NewImporter(database, client, events.NewBroker()),
		NewImporter(database, client, events.NewBroker()),
	}

	for round := range 30 {
		var wait sync.WaitGroup
		failures := make([]error, len(processes))
		for n, process := range processes {
			wait.Add(1)
			go func() {
				defer wait.Done()
				_, failures[n] = process.Sync(ctx, "CORE")
			}()
		}
		wait.Wait()
		for n, err := range failures {
			if err != nil {
				t.Fatalf("round %d process %d: %v", round, n, err)
			}
		}
	}

	var components, snapshotsInUse int
	var lastCommit, projectedCommit string
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from components where project_key = 'CORE'),
		       (select count(distinct snapshot_id) from components where project_key = 'CORE'),
		       (select last_commit from architecture_sources where project_key = 'CORE'),
		       (select s.commit from architecture_snapshots s
		          where s.id = (select max(snapshot_id) from components where project_key = 'CORE'))
	`).Scan(&components, &snapshotsInUse, &lastCommit, &projectedCommit); err != nil {
		t.Fatalf("read projection: %v", err)
	}
	if components != 60 || snapshotsInUse != 1 || lastCommit != projectedCommit {
		t.Fatalf("components=%d snapshots in use=%d last_commit=%s projected=%s; want 60 components from one snapshot at last_commit",
			components, snapshotsInUse, lastCommit, projectedCommit)
	}
}

// A PUT that moves the source to another repository while a sync of the old
// one is mid-fetch: the sync finds the row changed under its lock and writes
// nothing — no old-repo commit on the new row, no error recorded against it.
func TestSyncAbortsWhenTheSourceMovedMidFetch(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles(), commit: "commit-one", installationID: 11}
	var moved atomic.Bool
	var database *store.Store
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/blobs/{sha}", func(w http.ResponseWriter, r *http.Request) {
		if moved.CompareAndSwap(false, true) {
			if _, err := database.Pool.Exec(ctx,
				`update architecture_sources set repo = 'legion/other' where project_key = 'CORE'`); err != nil {
				t.Errorf("move source: %v", err)
			}
		}
		fake.handler().ServeHTTP(w, r)
	})
	mux.Handle("/", fake.handler())
	client := newGitHubClient(t, mux)
	database = openTestStore(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('CORE', 'legion/arch', 'main', 11, '{"kind":"user","id":"alice"}');
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	importer := NewImporter(database, client, events.NewBroker())

	source, err := importer.Sync(ctx, "CORE")
	if !errors.Is(err, errSourceMoved) {
		t.Fatalf("sync over a moved source: err=%v, want errSourceMoved", err)
	}
	if source.Repo != "legion/other" || source.LastCommit != nil || source.LastError != nil || source.LastSyncAt != nil {
		t.Fatalf("the moved row was written to: %+v", source)
	}
	if got := componentRows(t, database); len(got) != 0 {
		t.Fatalf("the old repository's model was projected onto the new source: %#v", got)
	}
	if got := projectEvents(t, database); len(got) != 0 {
		t.Fatalf("an aborted sync emitted %#v", got)
	}
}

// Bytes Postgres cannot store as text or jsonb — invalid UTF-8, NUL — are a
// named model problem on the row (the drift rule), not an internal error
// escaping the projection transaction.
func TestSyncRejectsBinaryFilesOnTheRow(t *testing.T) {
	ctx := context.Background()
	for name, content := range map[string]string{
		"nul.md":  "---\ntitle: z\n---\nbody\x00here",
		"utf8.md": "---\ntitle: w\n---\nbody \xff\xfe here",
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeSource{t: t, files: map[string]string{name: content, "ok.md": "fine\n"}}
			importer, database := newImporterFixture(t, fake)
			source, err := importer.Sync(ctx, "CORE")
			if err == nil {
				t.Fatal("a binary file was accepted")
			}
			want := name + ": file must be valid UTF-8 text without NUL bytes"
			if source.Project == "" || source.LastError == nil || !strings.Contains(*source.LastError, want) {
				t.Fatalf("row after sync: %+v err=%v, want last_error naming %q", source, err, want)
			}
			if got := componentRows(t, database); len(got) != 0 {
				t.Fatalf("a rejected set was projected: %#v", got)
			}
			if got := projectEvents(t, database); len(got) != 1 || !strings.HasPrefix(got[0], "architecture.sync_failed ") {
				t.Fatalf("events = %#v", got)
			}
		})
	}
}

// What one failure writes — last_error, the sync_failed payload, the
// Settings row — is capped: the first problems and a count of the rest.
func TestRecordedFailureIsCapped(t *testing.T) {
	files := map[string][]byte{}
	const members = 5000
	for n := range members {
		files[fmt.Sprintf("c%d.md", n)] = []byte(fmt.Sprintf("---\nparent: c%d\n---\n", (n+1)%members))
	}
	_, err := Parse(files)
	if err == nil || len(err.Error()) <= maxFailureMessage {
		t.Fatalf("a %d-member cycle produced err=%v; want a problem longer than the cap", members, err)
	}
	message := capFailureMessage(err)
	if len(message) > maxFailureMessage {
		t.Fatalf("capped message is %d bytes, cap %d", len(message), maxFailureMessage)
	}
	if !strings.HasPrefix(message, "c0.md: containment cycle") || !strings.HasSuffix(message, "more problems") {
		t.Fatalf("capped message = %q", message)
	}

	// Many short problems: the first ones survive whole, the rest are counted.
	var problems []error
	for n := range 1000 {
		problems = append(problems, fmt.Errorf("f%d.md: unknown parent %q", n, "ghost"))
	}
	message = capFailureMessage(errors.Join(problems...))
	if len(message) > maxFailureMessage || !strings.HasPrefix(message, "f0.md: unknown parent") {
		t.Fatalf("capped message = %d bytes, starts %q", len(message), message[:40])
	}
	var kept, more int
	if _, err := fmt.Sscanf(message[strings.LastIndex(message, "\n")+1:], "… and %d more problems", &more); err != nil {
		t.Fatalf("capped message tail: %q", message[strings.LastIndex(message, "\n")+1:])
	}
	kept = strings.Count(message, "\n")
	if kept+more != 1000 {
		t.Fatalf("kept %d + more %d != 1000", kept, more)
	}
}

// A commit another process already snapshotted is a duplicate key on
// (project_key, commit): the import reuses that snapshot instead of recording
// an error, and the row keeps its clean state.
func TestSyncReusesAnExistingSnapshotForTheSameCommit(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles()}
	importer, database := newImporterFixture(t, fake)
	var seeded int64
	if err := database.Pool.QueryRow(ctx, `
		insert into architecture_snapshots (project_key, commit, files)
		values ('CORE', 'commit-one', '{"seeded": "by another process"}') returning id
	`).Scan(&seeded); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	source, err := importer.Sync(ctx, "CORE")
	if err != nil {
		t.Fatalf("sync over an existing snapshot: %v", err)
	}
	if source.LastError != nil {
		t.Fatalf("duplicate snapshot key recorded an error: %v", *source.LastError)
	}
	var snapshots int
	var files map[string]string
	if err := database.Pool.QueryRow(ctx, `select count(*) from architecture_snapshots`).Scan(&snapshots); err != nil || snapshots != 1 {
		t.Fatalf("snapshots = %d err %v, want 1", snapshots, err)
	}
	if err := database.Pool.QueryRow(ctx, `select files from architecture_snapshots where id = $1`, seeded).Scan(&files); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if files["seeded"] != "by another process" {
		t.Fatalf("the immutable snapshot was rewritten: %#v", files)
	}
	var snapshotIDs int64
	if err := database.Pool.QueryRow(ctx,
		`select distinct snapshot_id from components where project_key = 'CORE'`).Scan(&snapshotIDs); err != nil || snapshotIDs != seeded {
		t.Fatalf("components point at snapshot %d err %v, want %d", snapshotIDs, err, seeded)
	}
}

func TestSyncUpstreamFailureAndMissingSource(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, treeFailure: true, files: validFiles()}
	importer, database := newImporterFixture(t, fake)

	source, err := importer.Sync(ctx, "CORE")
	if err == nil || IsAccessFailure(err) {
		t.Fatalf("upstream 500: err=%v, want a non-access failure", err)
	}
	if source.LastError == nil || !strings.Contains(*source.LastError, "status 500") {
		t.Fatalf("last_error = %v", source.LastError)
	}

	if _, err := importer.Sync(ctx, "NOPE"); err == nil || !strings.Contains(err.Error(), ErrNoSource.Error()) {
		t.Fatalf("unconfigured project: err=%v, want ErrNoSource", err)
	}
	_ = database
}

func TestSyncMissingBranchIsAnAccessFailure(t *testing.T) {
	fake := &fakeSource{t: t, branchMissing: true, files: validFiles()}
	importer, _ := newImporterFixture(t, fake)

	if _, err := importer.Sync(context.Background(), "CORE"); !IsAccessFailure(err) {
		t.Fatalf("missing branch: err=%v, want an access failure", err)
	}
}

// The stored installation id is a cache: a sync re-resolves it and records the
// new one when GitHub moved the installation.
func TestSyncRecordsAReResolvedInstallation(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles()}
	importer, database := newImporterFixture(t, fake)
	fake.installationID = 99

	if _, err := importer.Sync(ctx, "CORE"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	var stored int64
	if err := database.Pool.QueryRow(ctx,
		`select installation_id from architecture_sources where project_key = 'CORE'`).Scan(&stored); err != nil || stored != 99 {
		t.Fatalf("stored installation id = %d err %v, want 99", stored, err)
	}
}

func TestListEnabledNamesEveryEnabledSource(t *testing.T) {
	ctx := context.Background()
	importer, database := newImporterFixture(t, &fakeSource{t: t, files: validFiles()})
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('OPS', 'Operations'), ('OLD', 'Retired');
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('OPS', 'legion/ops', 'main', 1, '{"kind":"user","id":"alice"}'),
		       ('OLD', 'legion/old', 'main', 1, '{"kind":"user","id":"alice"}');
		update architecture_sources set enabled = false where project_key = 'OLD';
	`); err != nil {
		t.Fatalf("seed sources: %v", err)
	}
	projects, err := importer.ListEnabled(ctx)
	if err != nil {
		t.Fatalf("list enabled: %v", err)
	}
	sort.Strings(projects)
	if !reflect.DeepEqual(projects, []string{"CORE", "OPS"}) {
		t.Fatalf("enabled projects = %#v", projects)
	}
}

// graph_edges carries the component arms: part_of from components.parent,
// depends_on from component_depends, both with node kind 'component' addressed
// '<project>/<id>' — and never a row in refs.
func TestGraphEdgesCarriesTheComponentArms(t *testing.T) {
	ctx := context.Background()
	importer, database := newImporterFixture(t, &fakeSource{t: t, files: validFiles()})
	if _, err := importer.Sync(ctx, "CORE"); err != nil {
		t.Fatalf("sync: %v", err)
	}

	rows, err := database.Pool.Query(ctx, `
		select from_kind, from_id, kind, to_kind, to_id, source_seq is null
		from graph_edges where from_kind = 'component' order by kind, from_id
	`)
	if err != nil {
		t.Fatalf("read graph_edges: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var fromKind, fromID, kind, toKind, toID string
		var structural bool
		if err := rows.Scan(&fromKind, &fromID, &kind, &toKind, &toID, &structural); err != nil {
			t.Fatalf("scan edge: %v", err)
		}
		if !structural {
			t.Errorf("%s edge carries a source_seq; component edges are projection rows", kind)
		}
		got = append(got, fmt.Sprintf("%s %s %s %s %s", fromKind, fromID, kind, toKind, toID))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate graph_edges: %v", err)
	}
	want := []string{
		"component CORE/api depends_on component CORE/store",
		"component CORE/api part_of component CORE/server",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("component edges = %#v\nwant %#v", got, want)
	}

	var refRows int
	if err := database.Pool.QueryRow(ctx, `select count(*) from refs`).Scan(&refRows); err != nil || refRows != 0 {
		t.Fatalf("refs rows = %d err %v, want 0 (component edges are never mentions)", refRows, err)
	}
}

// The sync's own deadline is an import failure like any other: it lands on
// the row (and emits one sync_failed) while the caller is still alive, so the
// Settings row and the ticker's next tick both see why nothing imported.
func TestSyncDeadlineIsRecordedOnTheRow(t *testing.T) {
	previous := syncTimeout
	syncTimeout = 300 * time.Millisecond
	t.Cleanup(func() { syncTimeout = previous })

	fake := &fakeSource{t: t, files: validFiles(), blobHold: true}
	importer, database := newImporterFixture(t, fake)

	source, err := importer.Sync(context.Background(), "CORE")
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("held blob: err=%v, want the deadline", err)
	}
	if source.LastError == nil || !strings.Contains(*source.LastError, "deadline") {
		t.Fatalf("last_error = %v, want the deadline on the row", source.LastError)
	}
	if got := projectEvents(t, database); len(got) != 1 || !strings.HasPrefix(got[0], "architecture.sync_failed") {
		t.Fatalf("events = %#v, want one sync_failed", got)
	}

	// A caller that has gone away records nothing: shutdown-cancel stays off the row.
	fake2 := &fakeSource{t: t, files: validFiles(), blobHold: true}
	importer2, database2 := newImporterFixture(t, fake2)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	if _, err := importer2.Sync(ctx, "CORE"); err == nil {
		t.Fatal("cancelled caller: expected an error")
	}
	var lastError *string
	if err := database2.Pool.QueryRow(context.Background(),
		`select last_error from architecture_sources where project_key = 'CORE'`).Scan(&lastError); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if lastError != nil {
		t.Fatalf("cancelled caller wrote last_error %q", *lastError)
	}
}

// Two server processes that both fetched the same head before either
// projected it: the second waits on the row lock, then finds the commit
// already recorded and stays silent — one snapshot, one architecture.synced.
// The state-change decision is made from the locked row, not from the row
// each process loaded before its fetch.
func TestSyncAcrossProcessesEmitsOneEventForOneHead(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles(), commit: "commit-one"}
	// Gate the head lookups so both processes are mid-fetch at once: neither may
	// project until both have loaded their (identical, pre-sync) source rows.
	var arrived sync.WaitGroup
	arrived.Add(2)
	mux := http.NewServeMux()
	fetches := fake.handler()
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{branch}", func(w http.ResponseWriter, r *http.Request) {
		arrived.Done()
		arrived.Wait()
		fetches.ServeHTTP(w, r)
	})
	mux.Handle("/", fetches)
	client := newGitHubClient(t, mux)
	database := openTestStore(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('CORE', 'legion/arch', 'main', 11, '{"kind":"user","id":"alice"}');
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	processes := []*Importer{
		NewImporter(database, client, events.NewBroker()),
		NewImporter(database, client, events.NewBroker()),
	}
	var wait sync.WaitGroup
	failures := make([]error, len(processes))
	for n, process := range processes {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, failures[n] = process.Sync(ctx, "CORE")
		}()
	}
	wait.Wait()
	for n, err := range failures {
		if err != nil {
			t.Fatalf("process %d: %v", n, err)
		}
	}
	if got := projectEvents(t, database); len(got) != 1 || !strings.HasPrefix(got[0], "architecture.synced") {
		t.Fatalf("events after two same-head syncs = %#v, want exactly one architecture.synced", got)
	}
	var snapshots int
	if err := database.Pool.QueryRow(ctx, `select count(*) from architecture_snapshots where project_key = 'CORE'`).Scan(&snapshots); err != nil || snapshots != 1 {
		t.Fatalf("snapshots = %d err %v, want 1", snapshots, err)
	}
}
