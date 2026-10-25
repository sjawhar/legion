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
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// archRepo is the repository newImporterFixture's source of CORE reads.
const archRepo = "legion/arch"

// fakeSource is the GitHub App surface a sync walks: installation lookup,
// token mint, branch commit, the architecture directory's subtree, and blob
// reads. Each repository answers its own head, files and failures: archRepo,
// the fixture's source, answers commit, files, branchMissing and treeFailure,
// and every other repository answers its entry in repos (a repository in
// neither has no installation). files is keyed by the file name inside
// SourceDir; the subtree sha is derived from the files, so identical files
// under a new commit answer the same sha exactly like git would.
type fakeSource struct {
	t              *testing.T
	installationID int64
	contents       string
	commit         string
	files          map[string]string
	branchMissing  bool
	treeFailure    bool
	repos          map[string]*fakeRepo
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

// fakeRepo is one repository of the fake GitHub other than archRepo: its
// branch head, its architecture files and its failures.
type fakeRepo struct {
	commit        string
	files         map[string]string
	branchMissing bool
	treeFailure   bool
}

// repo answers the repository the request's route names, as it stands now.
func (f *fakeSource) repo(r *http.Request) (fakeRepo, bool) {
	name := r.PathValue("owner") + "/" + r.PathValue("repo")
	if name == archRepo {
		return fakeRepo{commit: f.commit, files: f.files, branchMissing: f.branchMissing, treeFailure: f.treeFailure}, true
	}
	repo, ok := f.repos[name]
	if !ok {
		return fakeRepo{}, false
	}
	return *repo, true
}

func (f *fakeSource) reads() (trees, blobs int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.treeReads, f.blobReads
}

func (f *fakeSource) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := f.repo(r); !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
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
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{branch}", func(w http.ResponseWriter, r *http.Request) {
		repo, ok := f.repo(r)
		if !ok || repo.branchMissing {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"sha":%q}`, repo.commit)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/trees/{ref}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.treeReads++
		f.mu.Unlock()
		commit, dir, ok := strings.Cut(r.PathValue("ref"), ":")
		if !ok || dir != SourceDir {
			f.t.Errorf("tree read %q is not the %s subtree", r.PathValue("ref"), SourceDir)
		}
		repo, ok := f.repo(r)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if repo.treeFailure {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message":"upstream is sad"}`)
			return
		}
		names := make([]string, 0, len(repo.files))
		for name := range repo.files {
			names = append(names, name)
		}
		sort.Strings(names)
		digest := sha256.New()
		if f.treeShaFollowsCommit {
			fmt.Fprintf(digest, "%s\x00", commit)
		}
		entries := []map[string]any{}
		for _, name := range names {
			fmt.Fprintf(digest, "%s\x00%s\x00", name, repo.files[name])
			entries = append(entries, map[string]any{
				"path": name, "mode": "100644", "type": "blob", "sha": "blob-" + name, "size": len(repo.files[name]),
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
		repo, _ := f.repo(r)
		content, ok := repo.files[strings.TrimPrefix(r.PathValue("sha"), "blob-")]
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

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

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
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.test", PEM: pemText}, server.URL)
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

	database := storetest.Open(t)
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

	source, err := importer.Sync(ctx, "CORE", archRepo)
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

	if _, err := importer.Sync(ctx, "CORE", archRepo); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	before := componentRows(t, database)
	treesBefore, blobsBefore := fake.reads()
	var firstSync time.Time
	if err := database.Pool.QueryRow(ctx, `select last_sync_at from architecture_sources where project_key = 'CORE'`).Scan(&firstSync); err != nil {
		t.Fatalf("read last_sync_at: %v", err)
	}

	for range 5 {
		source, err := importer.Sync(ctx, "CORE", archRepo)
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
	if _, err := importer.Sync(ctx, "CORE", archRepo); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	before := componentRows(t, database)
	_, blobsBefore := fake.reads()

	fake.commit = "commit-two"
	source, err := importer.Sync(ctx, "CORE", archRepo)
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
	if _, err := importer.Sync(ctx, "CORE", archRepo); err != nil {
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
	if _, err := importer.Sync(ctx, "CORE", archRepo); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	before := componentRows(t, database)

	fake.commit = "commit-broken"
	fake.files = map[string]string{"api.md": "x\n", "API.md": "duplicate\n"}
	source, err := importer.Sync(ctx, "CORE", archRepo)
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
		if _, err := importer.Sync(ctx, "CORE", archRepo); err == nil {
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
	if _, err := importer.Sync(ctx, "CORE", archRepo); err != nil {
		t.Fatalf("recovery sync: %v", err)
	}
	if got := projectEvents(t, database); len(got) != 2 || got[1] != "architecture.synced commit-one" {
		t.Fatalf("recovery events = %#v", got)
	}

	// And the recovered source stays silent while nothing changes.
	if _, err := importer.Sync(ctx, "CORE", archRepo); err != nil {
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
	if _, err := importer.Sync(ctx, "CORE", archRepo); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	fake.commit = "commit-two"
	fake.files = map[string]string{"server.md": "---\ntitle: Server\n---\nThe server.\n"}
	if _, err := importer.Sync(ctx, "CORE", archRepo); err != nil {
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
			if _, err := importer.Sync(ctx, "CORE", archRepo); err != nil {
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
	database := storetest.Open(t)
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
				_, failures[n] = process.Sync(ctx, "CORE", archRepo)
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
	database = storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('CORE', 'legion/arch', 'main', 11, '{"kind":"user","id":"alice"}');
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	importer := NewImporter(database, client, events.NewBroker())

	source, err := importer.Sync(ctx, "CORE", archRepo)
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
			source, err := importer.Sync(ctx, "CORE", archRepo)
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

// YAML spells U+0000 in plain ASCII ("\0", "\x00", "\u0000"), so a file the byte check passes can
// still decode to front matter PostgreSQL cannot store. Each decoded string is checked as well, and
// the problem is named on the row as a binary file's is, not an internal error escaping the
// projection. An unknown key reaches KnownFields before that check, so recordFailure writes its
// error through text.StorableReplacement before it reaches last_error or the sync_failed event.
func TestSyncRejectsAFrontMatterNulOnTheRow(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		field, matter, want string
		replacement         bool
	}{
		{"title", `title: "Ser\0ver"`, "server.md: front matter title must be valid UTF-8 text without NUL characters", false},
		{"parent", `parent: "a\x00pi"`, "server.md: front matter parent must be valid UTF-8 text without NUL characters", false},
		{"depends_on", `depends_on: ["st\u0000ore"]`, "server.md: front matter depends_on must be valid UTF-8 text without NUL characters", false},
		{"paths", `paths: ["src/\0x"]`, "server.md: front matter paths must be valid UTF-8 text without NUL characters", false},
		{"key", `"\0": value`, "server.md: front matter", true},
	} {
		t.Run(test.field, func(t *testing.T) {
			files := validFiles()
			files["server.md"] = "---\n" + test.matter + "\n---\nThe server.\n"
			importer, database := newImporterFixture(t, &fakeSource{t: t, files: files})
			source, err := importer.Sync(ctx, "CORE", archRepo)
			if err == nil {
				t.Fatal("front matter holding a NUL was accepted")
			}
			if source.Project == "" || source.LastError == nil || !strings.Contains(*source.LastError, test.want) {
				t.Fatalf("row after sync: %+v err=%v, want last_error naming %q", source, err, test.want)
			}
			if strings.IndexByte(*source.LastError, 0) >= 0 {
				t.Fatalf("last_error %q holds a raw U+0000", *source.LastError)
			}
			if test.replacement && !strings.Contains(*source.LastError, "\uFFFD") {
				t.Fatalf("last_error %q does not write the front-matter key's U+0000 as U+FFFD", *source.LastError)
			}
			if got := componentRows(t, database); len(got) != 0 {
				t.Fatalf("a rejected set was projected: %#v", got)
			}
			if got := projectEvents(t, database); len(got) != 1 || !strings.HasPrefix(got[0], "architecture.sync_failed ") || strings.IndexByte(got[0], 0) >= 0 || test.replacement && !strings.Contains(got[0], "\uFFFD") {
				t.Fatalf("events = %#v", got)
			}
		})
	}
}

// A projection PostgreSQL refuses for any reason is a failure on the row as a model problem is: the
// source says why the import stopped, one sync_failed event says so, and the previous projection
// stays, rather than an error that records nothing.
func TestAProjectionFailureIsRecordedOnTheRow(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles()}
	importer, database := newImporterFixture(t, fake)
	if _, err := importer.Sync(ctx, "CORE", archRepo); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	before := componentRows(t, database)
	if _, err := database.Pool.Exec(ctx, `alter table components add constraint test_refuses_boom check (title <> 'Boom')`); err != nil {
		t.Fatalf("add the refusing constraint: %v", err)
	}
	fake.commit = "commit-two"
	fake.files = validFiles()
	fake.files["server.md"] = "---\ntitle: Boom\n---\nThe server.\n"
	source, err := importer.Sync(ctx, "CORE", archRepo)
	if err == nil {
		t.Fatal("a projection the database refused reported success")
	}
	if source.Project == "" || source.LastError == nil || !strings.Contains(*source.LastError, "project component server") {
		t.Fatalf("row after sync: %+v err=%v, want last_error naming the refused component", source, err)
	}
	if got := componentRows(t, database); !reflect.DeepEqual(got, before) {
		t.Fatalf("components after the refused projection = %#v, want the previous %#v", got, before)
	}
	if got := projectEvents(t, database); len(got) != 2 || !strings.HasPrefix(got[1], "architecture.sync_failed ") {
		t.Fatalf("events = %#v", got)
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

	source, err := importer.Sync(ctx, "CORE", archRepo)
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

	source, err := importer.Sync(ctx, "CORE", archRepo)
	if err == nil || IsAccessFailure(err) {
		t.Fatalf("upstream 500: err=%v, want a non-access failure", err)
	}
	if source.LastError == nil || !strings.Contains(*source.LastError, "status 500") {
		t.Fatalf("last_error = %v", source.LastError)
	}

	if _, err := importer.Sync(ctx, "NOPE", archRepo); err == nil || !strings.Contains(err.Error(), ErrNoSource.Error()) {
		t.Fatalf("unconfigured project: err=%v, want ErrNoSource", err)
	}
	_ = database
}

func TestSyncMissingBranchIsAnAccessFailure(t *testing.T) {
	fake := &fakeSource{t: t, branchMissing: true, files: validFiles()}
	importer, _ := newImporterFixture(t, fake)

	if _, err := importer.Sync(context.Background(), "CORE", archRepo); !IsAccessFailure(err) {
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

	if _, err := importer.Sync(ctx, "CORE", archRepo); err != nil {
		t.Fatalf("sync: %v", err)
	}
	var stored int64
	if err := database.Pool.QueryRow(ctx,
		`select installation_id from architecture_sources where project_key = 'CORE'`).Scan(&stored); err != nil || stored != 99 {
		t.Fatalf("stored installation id = %d err %v, want 99", stored, err)
	}
}

// ListEnabled names every enabled source as its project and repository, ordered by project and
// then by the order the project's sources were added (not by repository name).
func TestListEnabledNamesEveryEnabledSource(t *testing.T) {
	ctx := context.Background()
	importer, database := newImporterFixture(t, &fakeSource{t: t, files: validFiles()})
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('OPS', 'Operations'), ('OLD', 'Retired');
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('OPS', 'legion/ops', 'main', 1, '{"kind":"user","id":"alice"}'),
		       ('OLD', 'legion/old', 'main', 1, '{"kind":"user","id":"alice"}'),
		       ('CORE', 'acme/infra', 'main', 1, '{"kind":"user","id":"alice"}');
		update architecture_sources set enabled = false where project_key = 'OLD';
	`); err != nil {
		t.Fatalf("seed sources: %v", err)
	}
	sources, err := importer.ListEnabled(ctx)
	if err != nil {
		t.Fatalf("list enabled: %v", err)
	}
	want := []EnabledSource{{"CORE", archRepo}, {"CORE", "acme/infra"}, {"OPS", "legion/ops"}}
	if !reflect.DeepEqual(sources, want) {
		t.Fatalf("enabled sources = %#v\nwant %#v", sources, want)
	}
}

// graph_edges carries the component arms: part_of from components.parent,
// depends_on from component_depends, both with node kind 'component' addressed
// '<project>/<id>' — and never a row in refs.
func TestGraphEdgesCarriesTheComponentArms(t *testing.T) {
	ctx := context.Background()
	importer, database := newImporterFixture(t, &fakeSource{t: t, files: validFiles()})
	if _, err := importer.Sync(ctx, "CORE", archRepo); err != nil {
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

	source, err := importer.Sync(context.Background(), "CORE", archRepo)
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
	if _, err := importer2.Sync(ctx, "CORE", archRepo); err == nil {
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
	database := storetest.Open(t)
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
			_, failures[n] = process.Sync(ctx, "CORE", archRepo)
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

// infraRepo is the second source of CORE in the multi-source tests.
const infraRepo = "acme/infra"

// infraFiles is acme/infra's model: infra > infra-db, and infra-dns depending on infra-db. None of
// its ids is one of validFiles'.
func infraFiles() map[string]string {
	return map[string]string{
		"infra.md":     "---\ntitle: Infrastructure\n---\nThe cluster.\n",
		"infra-db.md":  "---\ntitle: Database\nparent: infra\n---\nThe database.\n",
		"infra-dns.md": "---\ntitle: DNS\ndepends_on: [infra-db]\n---\nNames.\n",
	}
}

// addSource gives CORE a source from repo on main, which fake serves as files at commit.
func addSource(t *testing.T, database *store.Store, fake *fakeSource, repo, commit string, files map[string]string) {
	t.Helper()
	if fake.repos == nil {
		fake.repos = map[string]*fakeRepo{}
	}
	fake.repos[repo] = &fakeRepo{commit: commit, files: files}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('CORE', $1, 'main', $2, '{"kind":"user","id":"alice"}')
	`, repo, fake.installationID); err != nil {
		t.Fatalf("add source %s: %v", repo, err)
	}
}

// sourceComponents names every component of CORE with the repository of the source projecting
// it, by source and then id.
func sourceComponents(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select s.repo || ' ' || c.id
		from components c join architecture_sources s on s.project_key = c.project_key and s.id = c.source_id
		where c.project_key = 'CORE' order by s.id, c.id
	`)
	if err != nil {
		t.Fatalf("query components by source: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read components by source: %v", err)
	}
	return got
}

// attachments names each component CORE's issues are attached to with the repository of the
// source projecting it now, or "retired" when no source does.
func attachments(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select m.issue_key || ' ' || m.component_id || ' ' || coalesce(s.repo, 'retired')
		from issue_component_members m
		left join components c on c.project_key = m.project_key and c.id = m.component_id
		left join architecture_sources s on s.project_key = c.project_key and s.id = c.source_id
		where m.project_key = 'CORE' order by m.issue_key, m.component_id
	`)
	if err != nil {
		t.Fatalf("query attachments: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read attachments: %v", err)
	}
	return got
}

// projectionState reads CORE's whole projection as text: every component row with its source's
// repository, every component arm of graph_edges (part_of, depends_on, and an issue's affects),
// and every attachment.
func projectionState(t *testing.T, database *store.Store) string {
	t.Helper()
	var state string
	if err := database.Pool.QueryRow(context.Background(), `
		select concat_ws(E'\n',
			(select string_agg(s.repo || ' ' || to_jsonb(c)::text, E'\n' order by s.id, c.id)
			   from components c join architecture_sources s on s.project_key = c.project_key and s.id = c.source_id),
			(select string_agg(concat_ws(' ', g.from_id, g.kind, g.to_id), E'\n' order by g.from_id, g.kind, g.to_id)
			   from graph_edges g where g.to_kind = 'component'))
	`).Scan(&state); err != nil {
		t.Fatalf("read the projection: %v", err)
	}
	return state + "\n" + strings.Join(attachments(t, database), "\n")
}

// attachToCore creates CORE-1 attached to each of ids.
func attachToCore(t *testing.T, database *store.Store, ids ...string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
		insert into issue_components (issue_key, mode) values ('CORE-1', 'explicit');
	`); err != nil {
		t.Fatalf("create CORE-1: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into issue_component_members (issue_key, project_key, component_id) select 'CORE-1', 'CORE', unnest($1::text[])
	`, ids); err != nil {
		t.Fatalf("attach CORE-1 to %v: %v", ids, err)
	}
}

// sourceEvents reads every architecture event as its type, the source's repo@branch it names, and
// its commit and component count or its error.
func sourceEvents(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select concat_ws(' ', type, (payload->>'repo') || '@' || (payload->>'branch'),
		                 coalesce(payload->>'commit', payload->>'error'), payload->>'components')
		from events where type like 'architecture.%' order by id
	`)
	if err != nil {
		t.Fatalf("query architecture events: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read architecture events: %v", err)
	}
	return got
}

// A project with two sources imports each from its own repository's head: each row records its
// own commit and how many components it projects, each component belongs to the source whose files
// define it, each source's edges stand, and each source's synced event names its repository and
// branch. A second import of both records the same heads, fetches no file and changes nothing.
func TestEverySourceOfAProjectImportsItsOwnHeadAndComponents(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles()}
	importer, database := newImporterFixture(t, fake)
	addSource(t, database, fake, infraRepo, "infra-one", infraFiles())

	results, err := importer.SyncProject(ctx, "CORE")
	if err != nil || len(results) != 2 {
		t.Fatalf("sync CORE: %d results, err %v; want both sources", len(results), err)
	}
	for n, want := range []struct {
		repo, commit string
		components   int
	}{{archRepo, "commit-one", 3}, {infraRepo, "infra-one", 3}} {
		got := results[n]
		if got.Err != nil || got.Source.Repo != want.repo || got.Source.LastCommit == nil || *got.Source.LastCommit != want.commit ||
			got.Source.LastError != nil || got.Source.Components != want.components {
			t.Fatalf("result %d = %+v (err %v), want %s at %s projecting %d", n, got.Source, got.Err, want.repo, want.commit, want.components)
		}
	}
	wantComponents := []string{
		"legion/arch api", "legion/arch server", "legion/arch store",
		"acme/infra infra", "acme/infra infra-db", "acme/infra infra-dns",
	}
	if got := sourceComponents(t, database); !reflect.DeepEqual(got, wantComponents) {
		t.Fatalf("components = %#v\nwant %#v", got, wantComponents)
	}
	before := projectionState(t, database)
	for _, edge := range []string{"CORE/api depends_on CORE/store", "CORE/api part_of CORE/server", "CORE/infra-db part_of CORE/infra", "CORE/infra-dns depends_on CORE/infra-db"} {
		if !strings.Contains(before, edge) {
			t.Fatalf("projection lacks %q:\n%s", edge, before)
		}
	}
	wantEvents := []string{"architecture.synced legion/arch@main commit-one 3", "architecture.synced acme/infra@main infra-one 3"}
	if got := sourceEvents(t, database); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("events = %#v\nwant %#v", got, wantEvents)
	}

	trees, blobs := fake.reads()
	results, err = importer.SyncProject(ctx, "CORE")
	if err != nil || len(results) != 2 {
		t.Fatalf("second sync of CORE: %d results, err %v", len(results), err)
	}
	for n, commit := range []string{"commit-one", "infra-one"} {
		if got := results[n]; got.Err != nil || got.Source.LastCommit == nil || *got.Source.LastCommit != commit || got.Source.Components != 3 {
			t.Fatalf("second sync result %d = %+v (err %v), want %s still", n, got.Source, got.Err, commit)
		}
	}
	if gotTrees, gotBlobs := fake.reads(); gotTrees != trees || gotBlobs != blobs {
		t.Fatalf("unchanged heads re-fetched: trees %d -> %d, blobs %d -> %d", trees, gotTrees, blobs, gotBlobs)
	}
	if after := projectionState(t, database); after != before {
		t.Fatalf("a second import changed the projection:\nbefore\n%s\nafter\n%s", before, after)
	}
	if got := sourceEvents(t, database); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("a second import emitted events: %#v", got)
	}
}

// One source's model failure is that source's alone: its row records the error and keeps its
// commit, its previous components stay, and every component, edge and attachment of the other
// source is untouched, the other source importing as usual. The failed event names the failing
// source's repository and branch, and the same failure on the next import emits nothing more.
func TestOneSourcesFailedImportKeepsEverySourcesProjection(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles()}
	importer, database := newImporterFixture(t, fake)
	addSource(t, database, fake, infraRepo, "infra-one", infraFiles())
	if _, err := importer.SyncProject(ctx, "CORE"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	attachToCore(t, database, "api", "infra-db")
	before := projectionState(t, database)

	fake.commit = "commit-broken"
	fake.files = map[string]string{"api.md": "x\n", "API.md": "duplicate\n"}
	for range 2 {
		results, err := importer.SyncProject(ctx, "CORE")
		if err != nil || len(results) != 2 {
			t.Fatalf("sync CORE: %d results, err %v", len(results), err)
		}
		failed, other := results[0], results[1]
		if failed.Err == nil || failed.Source.LastError == nil || !strings.Contains(*failed.Source.LastError, "duplicate component id") ||
			failed.Source.LastCommit == nil || *failed.Source.LastCommit != "commit-one" || failed.Source.Components != 3 {
			t.Fatalf("legion/arch after its broken model = %+v (err %v), want the error recorded over commit-one's 3 components", failed.Source, failed.Err)
		}
		if other.Err != nil || other.Source.Repo != infraRepo || other.Source.LastError != nil {
			t.Fatalf("acme/infra beside legion/arch's failure = %+v (err %v), want a clean import", other.Source, other.Err)
		}
		if after := projectionState(t, database); after != before {
			t.Fatalf("a failed import changed the projection:\nbefore\n%s\nafter\n%s", before, after)
		}
	}
	got := sourceEvents(t, database)
	if len(got) != 3 || !strings.HasPrefix(got[2], "architecture.sync_failed legion/arch@main ") || !strings.Contains(got[2], "duplicate component id") {
		t.Fatalf("events = %#v, want the two syncs and one sync_failed naming legion/arch@main", got)
	}
}

// An import whose model holds an id another source of the project projects fails for the
// importing source alone, its error naming each id and the repository holding it; both sources'
// previous projections stay. Once the holder drops the ids, the failed source's next import (a
// full one, as its row carries an error) takes them.
func TestACollidingComponentIDFailsTheImportingSourceAndNamesTheHolder(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles()}
	importer, database := newImporterFixture(t, fake)
	addSource(t, database, fake, infraRepo, "infra-one", infraFiles())
	if _, err := importer.SyncProject(ctx, "CORE"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	before := projectionState(t, database)

	colliding := infraFiles()
	colliding["server.md"] = "---\ntitle: Another server\n---\nMine.\n"
	colliding["store.md"] = "---\ntitle: Another store\n---\nMine.\n"
	fake.repos[infraRepo].commit, fake.repos[infraRepo].files = "infra-two", colliding
	source, err := importer.Sync(ctx, "CORE", infraRepo)
	wants := []string{
		`component id "server" is already a component of CORE from legion/arch`,
		`component id "store" is already a component of CORE from legion/arch`,
	}
	for _, want := range wants {
		if err == nil || !strings.Contains(err.Error(), want) || source.LastError == nil || !strings.Contains(*source.LastError, want) {
			t.Fatalf("colliding import: err %v, row %+v; want both to say %q", err, source, want)
		}
	}
	if source.LastCommit == nil || *source.LastCommit != "infra-one" || source.Components != 3 {
		t.Fatalf("acme/infra after the collision = %+v, want infra-one's 3 components kept", source)
	}
	if after := projectionState(t, database); after != before {
		t.Fatalf("a colliding import changed the projection:\nbefore\n%s\nafter\n%s", before, after)
	}
	if got := sourceEvents(t, database); len(got) != 3 || !strings.HasPrefix(got[2], "architecture.sync_failed acme/infra@main ") {
		t.Fatalf("events = %#v, want a sync_failed naming acme/infra@main", got)
	}

	fake.commit = "commit-two"
	fake.files = map[string]string{"api.md": "---\ntitle: HTTP API\n---\nThe API.\n"}
	if _, err := importer.Sync(ctx, "CORE", archRepo); err != nil {
		t.Fatalf("legion/arch dropping server and store: %v", err)
	}
	if _, err := importer.Sync(ctx, "CORE", infraRepo); err != nil {
		t.Fatalf("acme/infra once the ids are free: %v", err)
	}
	want := []string{
		"legion/arch api",
		"acme/infra infra", "acme/infra infra-db", "acme/infra infra-dns", "acme/infra server", "acme/infra store",
	}
	if got := sourceComponents(t, database); !reflect.DeepEqual(got, want) {
		t.Fatalf("components = %#v\nwant %#v", got, want)
	}
}

// Two server processes (two Importer instances, no shared mutex) importing two sources of one
// project whose models share an id at once: the projections serialize on the project's source
// rows, so one import succeeds and the other fails naming the winner's repository - never a
// primary-key error from both inserting the id.
func TestACollisionRacedAcrossProcessesFailsOneSourceByName(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{
		t: t, installationID: 11, commit: "arch-one",
		files: map[string]string{"shared.md": "---\ntitle: Arch's\n---\n", "arch-only.md": "---\ntitle: Arch\n---\n"},
		repos: map[string]*fakeRepo{infraRepo: {
			commit: "infra-one",
			files:  map[string]string{"shared.md": "---\ntitle: Infra's\n---\n", "infra-only.md": "---\ntitle: Infra\n---\n"},
		}},
	}
	// Both processes look up their heads before either projects.
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
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by) values
			('CORE', 'legion/arch', 'main', 11, '{"kind":"user","id":"alice"}'),
			('CORE', 'acme/infra', 'main', 11, '{"kind":"user","id":"alice"}');
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	repos := []string{archRepo, infraRepo}
	var wait sync.WaitGroup
	failures := make([]error, len(repos))
	for n, repo := range repos {
		process := NewImporter(database, client, events.NewBroker())
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, failures[n] = process.Sync(ctx, "CORE", repo)
		}()
	}
	wait.Wait()
	if (failures[0] == nil) == (failures[1] == nil) {
		t.Fatalf("legion/arch: %v; acme/infra: %v; want exactly one to fail", failures[0], failures[1])
	}
	winner, loser := 0, 1
	if failures[0] != nil {
		winner, loser = 1, 0
	}
	want := `component id "shared" is already a component of CORE from ` + repos[winner]
	if !strings.Contains(failures[loser].Error(), want) || strings.Contains(failures[loser].Error(), "23505") {
		t.Fatalf("%s failed with %v, want %q and no primary-key error", repos[loser], failures[loser], want)
	}
	var lastError *string
	if err := database.Pool.QueryRow(ctx, `select last_error from architecture_sources where repo = $1`, repos[loser]).Scan(&lastError); err != nil ||
		lastError == nil || !strings.Contains(*lastError, want) {
		t.Fatalf("%s's row: last_error %v (err %v), want %q", repos[loser], lastError, err, want)
	}
	only := map[string]string{archRepo: "arch-only", infraRepo: "infra-only"}[repos[winner]]
	wantComponents := []string{repos[winner] + " " + only, repos[winner] + " shared"}
	if got := sourceComponents(t, database); !reflect.DeepEqual(got, wantComponents) {
		t.Fatalf("components = %#v\nwant %#v", got, wantComponents)
	}
}

// A component id is the project's, whichever source supplies it: an issue attached to a component
// of a deleted source reads it as retired, and once another source of the project imports that id
// the attachment is live again, now on that source.
func TestAnIDAnotherSourceImportsRevivesItsAttachment(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles()}
	importer, database := newImporterFixture(t, fake)
	addSource(t, database, fake, infraRepo, "infra-one", infraFiles())
	if _, err := importer.SyncProject(ctx, "CORE"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	attachToCore(t, database, "server")
	if got := attachments(t, database); !reflect.DeepEqual(got, []string{"CORE-1 server legion/arch"}) {
		t.Fatalf("attachments = %#v", got)
	}

	if _, err := database.Pool.Exec(ctx, `delete from architecture_sources where project_key = 'CORE' and repo = $1`, archRepo); err != nil {
		t.Fatalf("delete legion/arch: %v", err)
	}
	if got := attachments(t, database); !reflect.DeepEqual(got, []string{"CORE-1 server retired"}) {
		t.Fatalf("attachments after deleting legion/arch = %#v, want server retired", got)
	}
	if got := sourceComponents(t, database); !reflect.DeepEqual(got, []string{"acme/infra infra", "acme/infra infra-db", "acme/infra infra-dns"}) {
		t.Fatalf("components after deleting legion/arch = %#v, want acme/infra's alone", got)
	}

	files := infraFiles()
	files["server.md"] = "---\ntitle: Server\n---\nNow infra's.\n"
	fake.repos[infraRepo].commit, fake.repos[infraRepo].files = "infra-two", files
	if _, err := importer.Sync(ctx, "CORE", infraRepo); err != nil {
		t.Fatalf("acme/infra importing server: %v", err)
	}
	if got := attachments(t, database); !reflect.DeepEqual(got, []string{"CORE-1 server acme/infra"}) {
		t.Fatalf("attachments after acme/infra imported server = %#v, want it live on acme/infra", got)
	}
}

// A project's one source from before sources were per repository reads id 0, and its next sync
// takes the unchanged-head shortcut like any healthy source: the freshness stamp moves, no tree or
// blob is read, its components stay as they were, and no event is emitted.
func TestTheLegacySourceResyncsThroughTheUnchangedHeadShortcut(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles(), commit: "commit-one", installationID: 11}
	client := newGitHubClient(t, fake.handler())
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into architecture_sources (project_key, id, repo, branch, installation_id, created_by, last_sync_at, last_commit, last_tree_sha)
			values ('CORE', 0, 'legion/arch', 'main', 11, '{"kind":"user","id":"alice"}', now() - interval '1 hour', 'commit-one', 'tree-legacy');
		insert into architecture_snapshots (id, project_key, commit, files) values (700, 'CORE', 'commit-one', '{}');
		insert into components (project_key, id, title, prose, snapshot_id, source_id) values
			('CORE', 'legacy', 'Legacy', 'From before.', 700, 0), ('CORE', 'other', 'Other', 'Also.', 700, 0);
	`); err != nil {
		t.Fatalf("seed the legacy source: %v", err)
	}
	importer := NewImporter(database, client, events.NewBroker())
	before := componentRows(t, database)

	source, err := importer.Sync(ctx, "CORE", archRepo)
	if err != nil {
		t.Fatalf("sync the legacy source: %v", err)
	}
	if source.ID != 0 || source.LastCommit == nil || *source.LastCommit != "commit-one" || source.LastError != nil ||
		source.LastSyncAt == nil || time.Since(*source.LastSyncAt) > time.Minute || source.Components != 2 {
		t.Fatalf("legacy source after sync = %+v, want id 0 at commit-one, freshly stamped, projecting 2", source)
	}
	if trees, blobs := fake.reads(); trees != 0 || blobs != 0 {
		t.Fatalf("the shortcut read %d trees and %d blobs, want none", trees, blobs)
	}
	if got := componentRows(t, database); !reflect.DeepEqual(got, before) {
		t.Fatalf("components = %#v, want the legacy projection %#v", got, before)
	}
	if got := sourceEvents(t, database); len(got) != 0 {
		t.Fatalf("the shortcut emitted %#v", got)
	}
}

// SyncProject leaves out a source deleted while it runs, and a project with no source is
// ErrNoSource.
func TestSyncProjectLeavesOutASourceDeletedMidCall(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles(), commit: "commit-one", installationID: 11,
		repos: map[string]*fakeRepo{infraRepo: {commit: "infra-one", files: infraFiles()}}}
	var database *store.Store
	mux := http.NewServeMux()
	fetches := fake.handler()
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{branch}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("owner")+"/"+r.PathValue("repo") == archRepo {
			if _, err := database.Pool.Exec(ctx, `delete from architecture_sources where repo = $1`, infraRepo); err != nil {
				t.Errorf("delete acme/infra: %v", err)
			}
		}
		fetches.ServeHTTP(w, r)
	})
	mux.Handle("/", fetches)
	client := newGitHubClient(t, mux)
	database = storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core'), ('BARE', 'Bare');
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by) values
			('CORE', 'legion/arch', 'main', 11, '{"kind":"user","id":"alice"}'),
			('CORE', 'acme/infra', 'main', 11, '{"kind":"user","id":"alice"}');
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	importer := NewImporter(database, client, events.NewBroker())

	results, err := importer.SyncProject(ctx, "CORE")
	if err != nil || len(results) != 1 || results[0].Err != nil || results[0].Source.Repo != archRepo {
		t.Fatalf("sync CORE = %+v, err %v; want legion/arch's import alone", results, err)
	}
	if _, err := importer.SyncProject(ctx, "BARE"); !errors.Is(err, ErrNoSource) {
		t.Fatalf("sync a project with no source: err %v, want ErrNoSource", err)
	}
}
