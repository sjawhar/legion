package dashboard

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// twoBuildsSigningKey is what the routers below sign cookies with. Nothing here signs in: it is
// there because BuildAppContext requires one.
var twoBuildsSigningKey = auth.SingleSigningKey("two-builds-asset-test-key")

// newAssetRouter is a Dispatch process serving the build at distDir, with assets behind it for a
// file that build does not hold: routes.AppContext's WebDistDir and AssetStore, as
// cmd/dispatch/main.go wires them after it publishes its own build.
func newAssetRouter(t *testing.T, pool *store.Pool, distDir string, assets routes.AssetStore) http.Handler {
	t.Helper()
	people := store.NewPgPeopleStore(pool, twoBuildsSigningKey, nil)
	ctx, err := routes.BuildAppContext(routes.AppContextOptions{
		SigningKey: twoBuildsSigningKey,
		WebDistDir: distDir,
		AssetStore: assets,
		People:     people,
		Identity:   identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
		Lifetime:   t.Context(),
	})
	if err != nil {
		t.Fatalf("build app context for %s: %v", distDir, err)
	}
	handler, _ := routes.New(ctx)
	return handler
}

// A file of either build answers 200 with its own bytes from either process, whichever build that
// process serves: the one that holds the file on disk reads it there, and the one that does not
// reads it from the store both published into. That is what a rolling update or a rollback leaves
// a reader with - a page one process served, asking another for the chunks of its own build - and
// it is the claim the browser suite's two-process row used to make over two live servers
// (two-builds.multiprocess.e2e.ts), made here at the seam that answers it: routes.staticHandler
// over WebDistDir, falling back to the AssetStore (routes/assets.go) that dashboard.Publish fills
// at boot (INFRA-1404).
func TestEveryAssetOfBothBuildsAnswersFromEitherProcess(t *testing.T) {
	pool := storetest.Open(t).Pool
	ctx := context.Background()
	// Vite hashes every asset name, so two builds share a name only where the bytes are the same;
	// each name below carries its build's marker, and the shared runtime chunk carries neither,
	// which is the file both builds publish under one path.
	shared := []byte("export const shared = true")
	firstFiles := map[string][]byte{
		"index.html":               page("/assets/index-a1.js", "a1"),
		"assets/index-a1.js":       []byte("import './lib-a1.js'"),
		"assets/lib-a1.js":         bytes.Repeat([]byte("export const a = 1;\n"), 1_000),
		"assets/styles-a1.css":     []byte("body{margin:0}"),
		"assets/fonts/inter.woff2": {0x77, 0x4f, 0x46, 0x32, 0x00, 0xff, 0x00},
		"assets/shared-0.js":       shared,
		"favicon.svg":              []byte("<svg/>"),
	}
	secondFiles := map[string][]byte{
		"index.html":           page("/assets/index-b2.js", "b2"),
		"assets/index-b2.js":   []byte("import './lib-b2.js'"),
		"assets/lib-b2.js":     bytes.Repeat([]byte("export const b = 2;\n"), 1_000),
		"assets/styles-b2.css": []byte("body{margin:1px}"),
		"assets/shared-0.js":   shared,
		"favicon.svg":          []byte("<svg/>"),
	}
	firstDist, secondDist := writeDist(t, firstFiles), writeDist(t, secondFiles)
	for _, dist := range []string{firstDist, secondDist} {
		if _, err := Publish(ctx, pool, dist, ""); err != nil {
			t.Fatalf("publish %s: %v", dist, err)
		}
	}

	assets := NewStore(pool)
	processes := []struct {
		name    string
		dist    string
		files   map[string][]byte
		handler http.Handler
	}{
		{
			name:    "the process serving the first build",
			dist:    firstDist,
			files:   firstFiles,
			handler: newAssetRouter(t, pool, firstDist, assets),
		},
		{
			name:    "the process serving the second build",
			dist:    secondDist,
			files:   secondFiles,
			handler: newAssetRouter(t, pool, secondDist, assets),
		},
	}
	builds := []struct {
		name  string
		files map[string][]byte
	}{
		{name: "the first build", files: firstFiles},
		{name: "the second build", files: secondFiles},
	}

	for _, build := range builds {
		for path, want := range build.files {
			if !strings.HasPrefix(path, "assets/") {
				continue
			}
			for _, process := range processes {
				t.Run(build.name+"'s "+path+" from "+process.name, func(t *testing.T) {
					// What the row is for: where the file is not the serving build's own, that
					// process's dist must not hold it, so the 200 below can only have come from
					// the asset store - routes.staticHandler reads WebDistDir first and falls
					// back to the store for what it lacks (routes/assets.go). The shared runtime
					// chunk is in both dists, and both processes read it from their own.
					if _, own := process.files[path]; !own {
						local := filepath.Join(process.dist, filepath.FromSlash(path))
						if _, err := os.Stat(local); !errors.Is(err, fs.ErrNotExist) {
							t.Fatalf("%s holds %s in its own dist (stat %s: %v), so this row would answer from the file and prove nothing of the asset store",
								process.name, path, local, err)
						}
					}
					server := httptest.NewServer(process.handler)
					defer server.Close()
					response, err := server.Client().Get(server.URL + "/" + path)
					if err != nil {
						t.Fatalf("GET /%s: %v", path, err)
					}
					defer response.Body.Close()
					got, err := io.ReadAll(response.Body)
					if err != nil {
						t.Fatalf("read /%s: %v", path, err)
					}
					if response.StatusCode != http.StatusOK || !bytes.Equal(got, want) {
						t.Fatalf("GET /%s: status %d, %d bytes; want 200 and the %d bytes %s holds",
							path, response.StatusCode, len(got), len(want), build.name)
					}
				})
			}
		}
	}
}
