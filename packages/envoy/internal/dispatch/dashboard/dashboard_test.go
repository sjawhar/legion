package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// page is an index.html as Vite writes it, naming entry as its module script and a stylesheet
// after it, with marker making two pages of one entry differ.
func page(entry, marker string) []byte {
	return []byte(`<!doctype html><html><head><title>Dispatch ` + marker + `</title>` +
		`<script type="module" crossorigin src="` + entry + `"></script>` +
		`<link rel="stylesheet" crossorigin href="/assets/index-styles.css"></head><body><div id="root"></div></body></html>`)
}

// writeDist writes files, by slash-separated path below the dist, into a new dist directory.
func writeDist(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dist := t.TempDir()
	for name, content := range files {
		file := filepath.Join(dist, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dist
}

// captureLogs sends slog's records to a JSON buffer until the test ends.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

// errorRecords is every ERROR record of logs, each by its path attribute.
func errorRecords(t *testing.T, logs *bytes.Buffer) []string {
	t.Helper()
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record struct {
			Level string `json:"level"`
			Path  string `json:"path"`
		}
		if json.Unmarshal([]byte(line), &record) == nil && record.Level == "ERROR" {
			paths = append(paths, record.Path)
		}
	}
	return paths
}

// requireAsset fails the test unless the store answers key with want.
func requireAsset(t *testing.T, assets *Store, key string, want []byte) {
	t.Helper()
	got, err := assets.GetAsset(context.Background(), key)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("GetAsset(%q) = %d bytes, %v; want the %d bytes published", key, len(got), err, len(want))
	}
}

// requireNoAsset fails the test unless the store holds nothing under key.
func requireNoAsset(t *testing.T, assets *Store, key string) {
	t.Helper()
	if got, err := assets.GetAsset(context.Background(), key); !errors.Is(err, routes.ErrAssetNotFound) {
		t.Fatalf("GetAsset(%q) = %d bytes, %v; want ErrAssetNotFound", key, len(got), err)
	}
}

func count(t *testing.T, pool *store.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// A build's every file is stored under the key the router asks for, and read back byte for byte;
// the build is recorded with the entry its page names and the commit it was given.
func TestPublishStoresEveryFileOfTheBuild(t *testing.T) {
	pool := storetest.Open(t).Pool
	files := map[string][]byte{
		"index.html":               page("/assets/index-a1.js", "a"),
		"assets/index-a1.js":       []byte("import './lib-a1.js'"),
		"assets/lib-a1.js":         bytes.Repeat([]byte("export const x = 1;\n"), 50_000),
		"assets/index-styles.css":  []byte("body{margin:0}"),
		"assets/fonts/inter.woff2": {0x77, 0x4f, 0x46, 0x32, 0x00, 0xff, 0x00},
		"favicon.svg":              []byte("<svg/>"),
	}
	dist := writeDist(t, files)

	published, err := Publish(context.Background(), pool, dist, "0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}

	build, entry, err := BuildID(dist)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Published{Build: build, Entry: "/assets/index-a1.js", Assets: 4, Written: 4}); published != want || entry != want.Entry {
		t.Fatalf("Publish = %+v (BuildID entry %q), want %+v", published, entry, want)
	}
	if len(build) != 32 {
		t.Errorf("build id %q, want 32 hex digits", build)
	}
	assets := NewStore(pool)
	for name, content := range files {
		if strings.HasPrefix(name, "assets/") {
			requireAsset(t, assets, name, content)
		}
	}
	requireNoAsset(t, assets, "index.html")
	requireNoAsset(t, assets, "favicon.svg")
	requireNoAsset(t, assets, "assets/never-built.js")
	var storedEntry, commit string
	if err := pool.QueryRow(context.Background(), `select entry, commit from dashboard_builds where build = $1`, build).Scan(&storedEntry, &commit); err != nil {
		t.Fatal(err)
	}
	if storedEntry != "/assets/index-a1.js" || commit != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("build row: entry %q commit %q", storedEntry, commit)
	}
	if n := count(t, pool, `select count(*) from dashboard_build_assets where build = $1`, build); n != 4 {
		t.Errorf("the build links %d assets, want 4", n)
	}
}

// A process restarted on a build already stored writes no asset: it neither reads nor sends a file
// (a file changed on disk since is never compared, so nothing is logged) and leaves every asset
// row as it was, and it renews only the build's last start, on the database's clock.
func TestPublishOfAStoredBuildWritesNoAsset(t *testing.T) {
	pool := storetest.Open(t).Pool
	ctx := context.Background()
	dist := writeDist(t, map[string][]byte{
		"index.html":         page("/assets/index-b1.js", "b"),
		"assets/index-b1.js": []byte("console.log('b')"),
		"assets/chunk-b1.js": []byte("export default 1"),
	})
	first, err := Publish(ctx, pool, dist, "")
	if err != nil {
		t.Fatal(err)
	}
	rowVersions := func() string {
		var versions string
		if err := pool.QueryRow(ctx, `select string_agg(path || ':' || xmin::text || ':' || created_at::text, ',' order by path) from dashboard_assets`).Scan(&versions); err != nil {
			t.Fatal(err)
		}
		return versions
	}
	starts := func() (firstStarted, lastStarted time.Time) {
		if err := pool.QueryRow(ctx, `select first_started_at, last_started_at from dashboard_builds where build = $1`, first.Build).Scan(&firstStarted, &lastStarted); err != nil {
			t.Fatal(err)
		}
		return firstStarted, lastStarted
	}
	before := rowVersions()
	firstStart, lastStart := starts()
	logs := captureLogs(t)
	if err := os.WriteFile(filepath.Join(dist, "assets", "chunk-b1.js"), []byte("changed on disk"), 0o600); err != nil {
		t.Fatal(err)
	}

	again, err := Publish(ctx, pool, dist, "")
	if err != nil {
		t.Fatal(err)
	}

	if again.Written != 0 || again.Assets != 2 || again.Build != first.Build {
		t.Errorf("second publish = %+v, want build %s with 2 assets and none written", again, first.Build)
	}
	if after := rowVersions(); after != before {
		t.Errorf("asset rows changed: before %s, after %s", before, after)
	}
	if paths := errorRecords(t, logs); len(paths) != 0 {
		t.Errorf("the second publish compared files it has no reason to read: ERROR records for %q", paths)
	}
	renewedFirst, renewedLast := starts()
	if !renewedFirst.Equal(firstStart) || !renewedLast.After(lastStart) {
		t.Errorf("starts: first %s then %s, last %s then %s; want first unchanged and last later", firstStart, renewedFirst, lastStart, renewedLast)
	}
	requireAsset(t, NewStore(pool), "assets/chunk-b1.js", []byte("export default 1"))
}

// A file larger than the router serves refuses the whole publish, naming the file, and stores
// nothing of the build.
func TestPublishRefusesAFileOverTheLimit(t *testing.T) {
	pool := storetest.Open(t).Pool
	dist := writeDist(t, map[string][]byte{
		"index.html":         page("/assets/index-c1.js", "c"),
		"assets/index-c1.js": []byte("console.log('c')"),
		"assets/huge-c1.js":  bytes.Repeat([]byte("/"), 9<<20),
	})

	published, err := Publish(context.Background(), pool, dist, "")

	if err == nil || !strings.Contains(err.Error(), filepath.Join("assets", "huge-c1.js")) {
		t.Fatalf("Publish = %+v, %v; want an error naming assets/huge-c1.js", published, err)
	}
	if n := count(t, pool, `select (select count(*) from dashboard_builds) + (select count(*) from dashboard_assets)`); n != 0 {
		t.Errorf("a refused publish stored %d rows, want none", n)
	}
}

// A dist with no index.html is no build (the Go test rigs serve an empty directory): nothing is
// stored, whatever its assets directory holds.
func TestPublishWithoutAnIndexPageStoresNothing(t *testing.T) {
	pool := storetest.Open(t).Pool
	dist := writeDist(t, map[string][]byte{"assets/orphan.js": []byte("console.log('orphan')")})

	published, err := Publish(context.Background(), pool, dist, "")

	if !errors.Is(err, ErrNoBuild) {
		t.Fatalf("Publish = %+v, %v; want ErrNoBuild", published, err)
	}
	if _, _, err := BuildID(dist); !errors.Is(err, ErrNoBuild) {
		t.Errorf("BuildID: %v, want ErrNoBuild", err)
	}
	if n := count(t, pool, `select (select count(*) from dashboard_builds) + (select count(*) from dashboard_assets)`); n != 0 {
		t.Errorf("a dist with no index.html stored %d rows, want none", n)
	}
}

// Two builds of one source share the files whose bytes did not change, under one path: the second
// build links the stored file and writes nothing for it. A path stored with other bytes is a
// content-hash collision: one ERROR names it, and the stored bytes stay.
func TestPublishSharesAStoredPathAndKeepsItsBytes(t *testing.T) {
	pool := storetest.Open(t).Pool
	ctx := context.Background()
	shared := []byte("export const yjs = true")
	first := writeDist(t, map[string][]byte{
		"index.html":         page("/assets/index-d1.js", "d1"),
		"assets/index-d1.js": []byte("console.log('d1')"),
		"assets/yjs-0.js":    shared,
		"assets/clash-0.js":  []byte("the first build's bytes"),
	})
	if _, err := Publish(ctx, pool, first, ""); err != nil {
		t.Fatal(err)
	}
	logs := captureLogs(t)
	second := writeDist(t, map[string][]byte{
		"index.html":         page("/assets/index-d2.js", "d2"),
		"assets/index-d2.js": []byte("console.log('d2')"),
		"assets/yjs-0.js":    shared,
		"assets/clash-0.js":  []byte("the second build's bytes"),
	})

	published, err := Publish(ctx, pool, second, "")
	if err != nil {
		t.Fatal(err)
	}

	if published.Written != 1 || published.Assets != 3 {
		t.Errorf("second build: %+v, want 3 assets and only its entry written", published)
	}
	if paths := errorRecords(t, logs); len(paths) != 1 || paths[0] != "assets/clash-0.js" {
		t.Errorf("ERROR records for %q, want one for assets/clash-0.js", paths)
	}
	assets := NewStore(pool)
	requireAsset(t, assets, "assets/clash-0.js", []byte("the first build's bytes"))
	requireAsset(t, assets, "assets/yjs-0.js", shared)
	if n := count(t, pool, `select count(*) from dashboard_build_assets where build = $1`, published.Build); n != 3 {
		t.Errorf("the second build links %d assets, want 3", n)
	}
}

// publishNumbered publishes build n, holding its own chunk and the shared one, and dates its last
// start age before now on the database's clock.
func publishNumbered(t *testing.T, pool *store.Pool, n int, age time.Duration, shared []byte) Published {
	t.Helper()
	ctx := context.Background()
	entry := fmt.Sprintf("/assets/index-n%d.js", n)
	dist := writeDist(t, map[string][]byte{
		"index.html":                       page(entry, fmt.Sprint(n)),
		strings.TrimPrefix(entry, "/"):     []byte(fmt.Sprintf("console.log(%d)", n)),
		"assets/shared-0.js":               shared,
		fmt.Sprintf("assets/own-%d.js", n): []byte(fmt.Sprintf("export default %d", n)),
	})
	published, err := Publish(ctx, pool, dist, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update dashboard_builds set last_started_at = now() - make_interval(secs => $2) where build = $1`,
		published.Build, age.Seconds()); err != nil {
		t.Fatal(err)
	}
	return published
}

// The prune keeps every build a live process runs and the KeepNewest started most recently, and
// deletes every other build with each asset only it held; an asset a deleted build shared with a
// kept one stays.
func TestPruneKeepsLiveAndNewestBuildsAndTheirAssets(t *testing.T) {
	pool := storetest.Open(t).Pool
	shared := []byte("export const shared = true")
	const total = KeepNewest + 5
	builds := make([]Published, total)
	for n := range total {
		// Build 0 is the oldest, and build total-1 the newest.
		builds[n] = publishNumbered(t, pool, n, time.Duration(total-n)*time.Hour, shared)
	}
	live := []string{builds[1].Build}

	pruned, err := Prune(context.Background(), pool, live)
	if err != nil {
		t.Fatal(err)
	}

	// Kept: the live build 1 and the newest KeepNewest (5 to 24); dropped: 0, 2, 3 and 4.
	dropped := map[int]bool{0: true, 2: true, 3: true, 4: true}
	if pruned.Builds != int64(len(dropped)) || pruned.Assets != int64(2*len(dropped)) {
		t.Errorf("Prune = %+v, want %d builds and their %d own assets", pruned, len(dropped), 2*len(dropped))
	}
	assets := NewStore(pool)
	for n, build := range builds {
		stored := count(t, pool, `select count(*) from dashboard_builds where build = $1`, build.Build) == 1
		own := fmt.Sprintf("assets/own-%d.js", n)
		if dropped[n] {
			if stored {
				t.Errorf("build %d was kept, want it pruned", n)
			}
			requireNoAsset(t, assets, own)
			requireNoAsset(t, assets, strings.TrimPrefix(build.Entry, "/"))
			continue
		}
		if !stored {
			t.Errorf("build %d was pruned, want it kept", n)
		}
		requireAsset(t, assets, own, []byte(fmt.Sprintf("export default %d", n)))
	}
	requireAsset(t, assets, "assets/shared-0.js", shared)

	// With no live build, the newest KeepNewest alone stay: build 1 goes too.
	if pruned, err := Prune(context.Background(), pool, nil); err != nil || pruned.Builds != 1 {
		t.Errorf("Prune with no live build = %+v, %v; want build 1 pruned", pruned, err)
	}
	requireNoAsset(t, assets, "assets/own-1.js")
	requireAsset(t, assets, "assets/shared-0.js", shared)
}

// A publish and a prune started at the same moment, in either order, leave every file of the
// published build readable. Each round's build stores nothing of its own at first: every file it
// holds is already stored by an old build the prune deletes, so a prune that ran between the
// publish's check that a file is stored and its link to it would delete what the publish relies
// on.
func TestPublishAndPruneTogetherLeaveThePublishedBuildReadable(t *testing.T) {
	pool := storetest.Open(t).Pool
	ctx := context.Background()
	files := map[string][]byte{
		"assets/index-x1.js":      []byte("import './lib-x1.js'"),
		"assets/lib-x1.js":        []byte("export const lib = 1"),
		"assets/index-styles.css": []byte("body{margin:0}"),
	}
	withPage := func(marker string) map[string][]byte {
		dist := map[string][]byte{"index.html": page("/assets/index-x1.js", marker)}
		for name, content := range files {
			dist[name] = content
		}
		return dist
	}
	old := writeDist(t, withPage("old"))
	published := writeDist(t, withPage("published"))
	assets := NewStore(pool)

	for round := range 30 {
		if _, err := pool.Exec(ctx, `delete from dashboard_builds; delete from dashboard_assets`); err != nil {
			t.Fatal(err)
		}
		if _, err := Publish(ctx, pool, old, ""); err != nil {
			t.Fatal(err)
		}
		// KeepNewest builds newer than the old one and older than the publish: a prune deletes the
		// old build, and with it every file, unless the published build links them first.
		if _, err := pool.Exec(ctx, `update dashboard_builds set last_started_at = now() - interval '1 day'`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			insert into dashboard_builds (build, entry, last_started_at)
			select 'filler-' || n, '', now() - make_interval(mins => n) from generate_series(1, $1) n`, KeepNewest); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		var publishErr, pruneErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, publishErr = Publish(ctx, pool, published, "")
		}()
		go func() {
			defer wg.Done()
			<-start
			_, pruneErr = Prune(ctx, pool, nil)
		}()
		close(start)
		wg.Wait()

		if publishErr != nil || pruneErr != nil {
			t.Fatalf("round %d: publish %v, prune %v", round, publishErr, pruneErr)
		}
		for name, content := range files {
			got, err := assets.GetAsset(ctx, name)
			if err != nil || !bytes.Equal(got, content) {
				t.Fatalf("round %d: GetAsset(%q) = %q, %v; want %q", round, name, got, err, content)
			}
		}
	}
}
