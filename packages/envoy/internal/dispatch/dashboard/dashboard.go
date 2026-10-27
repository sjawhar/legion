// Package dashboard keeps the dashboard builds Dispatch's server processes run in Dispatch's own
// database, so that every process can answer every build's assets. A rolling update or a rollback
// runs two builds behind one load balancer, and each image carries one build's web dist alone: a
// tab holding one build's page asks for that build's content-hashed chunks, and whichever process
// answers must have them. Every process publishes its build at boot, before it listens (Publish),
// prunes the builds no process needs (Prune), and answers a local miss from the database (Store,
// the router's routes.AssetStore). The tables are migration 0121's.
package dashboard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// KeepNewest is how many of the most recently started builds Prune keeps beside the live ones: a
// tab may hold the page of a build no process runs any more, for as long as it stays open, and a
// rollback starts an earlier build again.
const KeepNewest = 20

// lockName names the advisory lock every publish takes shared and every prune exclusive, so a prune
// never deletes an asset between a publish's check that it is stored and the link that keeps it.
const lockName = "dispatch-dashboard-assets"

// ErrNoBuild is the answer for a web dist with no index.html: there is no build to publish. The Go
// test rigs serve an empty directory.
var ErrNoBuild = errors.New("no build: the web dist holds no index.html")

var (
	scriptSource = regexp.MustCompile(`(?i)<script\b[^>]*\bsrc=["']([^"']+)["'][^>]*>`)
	entryPath    = regexp.MustCompile(`^/assets/index-[^/]+\.js$`)
)

// BuildID returns the build id of the web dist at distDir — the hex of the first 16 bytes of the
// sha256 of its index.html, the bytes the page's ETag hashes (routes.servePage) — and the entry
// chunk its page names (`/assets/index-*.js`, the path a tab compares to tell builds apart, read
// as DeploymentResilience.tsx reads it), or an empty entry when the page names none. It returns
// ErrNoBuild when there is no index.html.
func BuildID(distDir string) (build, entry string, err error) {
	page, err := os.ReadFile(filepath.Join(distDir, "index.html"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", ErrNoBuild
	}
	if err != nil {
		return "", "", fmt.Errorf("read the dashboard's index.html: %w", err)
	}
	sum := sha256.Sum256(page)
	for _, match := range scriptSource.FindAllSubmatch(page, -1) {
		source, _, _ := strings.Cut(string(match[1]), "?")
		source, _, _ = strings.Cut(source, "#")
		if entryPath.MatchString(source) {
			entry = source
			break
		}
	}
	return hex.EncodeToString(sum[:16]), entry, nil
}

// Published is what a Publish stored.
type Published struct {
	Build, Entry string
	// Assets is how many files the build's assets directory holds.
	Assets int
	// Written is how many of them this publish wrote the bytes of: none for a build already
	// stored, and none for a file another build already stored under its path.
	Written int
}

// asset is one file of a build's assets directory: its key (`assets/<relative path>`, what the
// router asks the store for) and where it is on disk.
type asset struct {
	key, file string
}

// Publish stores the build at distDir: every file below its assets directory under its key, the
// build's row with commit (the image's stamped commit, or empty), and the build's link to each
// file, in one transaction that holds lockName shared, so publishes run together and a prune waits
// for them. A build already stored with all its files only has its last start renewed, and no
// file of it is read or sent, so a restart writes no bytes. A file another build stored first is
// linked, not written; one stored with other bytes than this build's is a content-hash collision,
// logged at ERROR naming its path, and the stored bytes are kept. A file over
// routes.MaxRetainedAssetSize refuses the whole publish, naming it, since that is the most memory
// the router reserves for one store read. Every time is the database's clock. A dist with no
// index.html is ErrNoBuild and stores nothing.
func Publish(ctx context.Context, pool *store.Pool, distDir, commit string) (Published, error) {
	build, entry, err := BuildID(distDir)
	if err != nil {
		return Published{}, err
	}
	assets, err := listAssets(distDir)
	if err != nil {
		return Published{}, err
	}
	published := Published{Build: build, Entry: entry, Assets: len(assets)}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return Published{}, fmt.Errorf("begin the dashboard publish: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock_shared(hashtext($1))`, lockName); err != nil {
		return Published{}, fmt.Errorf("lock the dashboard assets: %w", err)
	}
	rows, err := tx.Query(ctx, `select path from dashboard_build_assets where build = $1`, build)
	if err != nil {
		return Published{}, fmt.Errorf("read build %s's assets: %w", build, err)
	}
	linked := map[string]bool{}
	var linkedPath string
	if _, err := pgx.ForEachRow(rows, []any{&linkedPath}, func() error {
		linked[linkedPath] = true
		return nil
	}); err != nil {
		return Published{}, fmt.Errorf("read build %s's assets: %w", build, err)
	}
	if _, err := tx.Exec(ctx, `
		insert into dashboard_builds (build, entry, commit) values ($1, $2, $3)
		on conflict (build) do update set last_started_at = now()`, build, entry, commit); err != nil {
		return Published{}, fmt.Errorf("record build %s: %w", build, err)
	}
	var missing []asset
	for _, file := range assets {
		if !linked[file.key] {
			missing = append(missing, file)
		}
	}
	if len(missing) > 0 {
		if published.Written, err = storeAssets(ctx, tx, build, missing); err != nil {
			return Published{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Published{}, fmt.Errorf("commit the dashboard publish: %w", err)
	}
	return published, nil
}

// listAssets lists every file below distDir's assets directory in key order, and refuses one over
// routes.MaxRetainedAssetSize by its size on disk, before anything is read or stored. A dist with
// no assets directory has none.
func listAssets(distDir string) ([]asset, error) {
	root := filepath.Join(distDir, "assets")
	var assets []asset
	err := filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && file == root {
			return fs.SkipDir
		}
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := os.Stat(file)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(distDir, file)
		if err != nil {
			return err
		}
		if info.Size() > routes.MaxRetainedAssetSize {
			return fmt.Errorf("dashboard asset %s is %d bytes, over the %d-byte limit the server serves", file, info.Size(), routes.MaxRetainedAssetSize)
		}
		assets = append(assets, asset{key: filepath.ToSlash(relative), file: file})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list the dashboard's assets: %w", err)
	}
	// Every publish inserts its paths in this one order, so two publishes of builds sharing paths
	// wait on each other's inserts in turn and never each hold a path the other waits for.
	slices.SortFunc(assets, func(a, b asset) int { return strings.Compare(a.key, b.key) })
	return assets, nil
}

// storeAssets stores each file of missing whose path no build has stored yet, checks the hash of
// each one stored before, and links them all to build. It returns how many files it wrote.
func storeAssets(ctx context.Context, tx pgx.Tx, build string, missing []asset) (int, error) {
	keys := make([]string, len(missing))
	for i, file := range missing {
		keys[i] = file.key
	}
	stored := map[string][]byte{}
	rows, err := tx.Query(ctx, `select path, sha256 from dashboard_assets where path = any($1)`, keys)
	if err != nil {
		return 0, fmt.Errorf("read the stored dashboard assets: %w", err)
	}
	var key string
	var sum []byte
	if _, err := pgx.ForEachRow(rows, []any{&key, &sum}, func() error {
		stored[key] = sum
		return nil
	}); err != nil {
		return 0, fmt.Errorf("read the stored dashboard assets: %w", err)
	}

	written := 0
	for _, file := range missing {
		content, err := os.ReadFile(file.file)
		if err != nil {
			return 0, fmt.Errorf("read dashboard asset %s: %w", file.file, err)
		}
		if int64(len(content)) > routes.MaxRetainedAssetSize {
			return 0, fmt.Errorf("dashboard asset %s is %d bytes, over the %d-byte limit the server serves", file.file, len(content), routes.MaxRetainedAssetSize)
		}
		hash := sha256.Sum256(content)
		storedHash, found := stored[file.key]
		if !found {
			tag, err := tx.Exec(ctx, `
				insert into dashboard_assets (path, content, size, sha256) values ($1, $2, $3, $4)
				on conflict (path) do nothing`, file.key, content, len(content), hash[:])
			if err != nil {
				return 0, fmt.Errorf("store dashboard asset %s: %w", file.key, err)
			}
			if tag.RowsAffected() == 1 {
				written++
				continue
			}
			// A publish of another build stored it between the read above and this insert.
			if err := tx.QueryRow(ctx, `select sha256 from dashboard_assets where path = $1`, file.key).Scan(&storedHash); err != nil {
				return 0, fmt.Errorf("read stored dashboard asset %s: %w", file.key, err)
			}
		}
		if !bytes.Equal(storedHash, hash[:]) {
			slog.Error("dispatch: dashboard asset differs from the one stored under its path; keeping the stored bytes",
				"path", file.key, "build", build, "sha256", hex.EncodeToString(hash[:]), "stored_sha256", hex.EncodeToString(storedHash))
		}
	}
	if _, err := tx.Exec(ctx, `
		insert into dashboard_build_assets (build, path) select $1, unnest($2::text[])
		on conflict do nothing`, build, keys); err != nil {
		return 0, fmt.Errorf("link build %s to its assets: %w", build, err)
	}
	return written, nil
}

// Pruned is what a Prune deleted.
type Pruned struct {
	Builds, Assets int64
}

// Prune deletes every build that is not in live and not among the KeepNewest started most
// recently, and then every asset no build still links, in one transaction holding lockName
// exclusive, so it waits for every publish in flight and none starts until it ends: an asset is
// deleted only once no build links it, and a publish that links one after the prune sees it gone
// and stores it again.
func Prune(ctx context.Context, pool *store.Pool, live []string) (Pruned, error) {
	// A nil slice is SQL null, and `<> all(null)` is never true, which would keep every build.
	if live == nil {
		live = []string{}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Pruned{}, fmt.Errorf("begin the dashboard prune: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext($1))`, lockName); err != nil {
		return Pruned{}, fmt.Errorf("lock the dashboard assets: %w", err)
	}
	builds, err := tx.Exec(ctx, `
		delete from dashboard_builds
		where build <> all($1::text[])
		  and build not in (select build from dashboard_builds order by last_started_at desc, build limit $2)`,
		live, KeepNewest)
	if err != nil {
		return Pruned{}, fmt.Errorf("prune dashboard builds: %w", err)
	}
	assets, err := tx.Exec(ctx, `
		delete from dashboard_assets a
		where not exists (select from dashboard_build_assets l where l.path = a.path)`)
	if err != nil {
		return Pruned{}, fmt.Errorf("prune dashboard assets: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Pruned{}, fmt.Errorf("commit the dashboard prune: %w", err)
	}
	return Pruned{Builds: builds.RowsAffected(), Assets: assets.RowsAffected()}, nil
}

// Store is the router's asset store (routes.AssetStore) over the published assets.
type Store struct {
	pool *store.Pool
}

var _ routes.AssetStore = (*Store)(nil)

// NewStore reads published assets through pool.
func NewStore(pool *store.Pool) *Store {
	return &Store{pool: pool}
}

// GetAsset returns the bytes stored under key, or routes.ErrAssetNotFound when no build published
// that path.
func (s *Store) GetAsset(ctx context.Context, key string) ([]byte, error) {
	var content []byte
	err := s.pool.QueryRow(ctx, `select content from dashboard_assets where path = $1`, key).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, routes.ErrAssetNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read dashboard asset %q: %w", key, err)
	}
	return content, nil
}
