// Package repocache is the Kubernetes runtime's per-repository cache: one process per mapped
// repository holding a bare clone on its own volume, fetched from GitHub with the implement App's
// token on a schedule, and streaming that clone to issue pods, whose fetch container unpacks it as
// its feed and asks GitHub only for what the cache lacks.
package repocache

import (
	"archive/tar"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// Port is the port a cache pod serves on: its readiness probe's, and the one an issue pod's fetch
// container reaches it at.
const Port = 8418

// tokenFileEnv names the variable a cache pod's container points at the implement App's token for
// the repository's owner, projected from the cache's Secret: what a token file's refusal names.
const tokenFileEnv = "LEGION_PROVISION_TOKEN_FILE"

// CacheTokenFileEnv names the variable that points at the cache's bearer token: in the cache's
// container the token every request but /readyz must present, and in an issue pod's fetch container
// the token it presents. Both are projected from Secrets the daemon writes, never an environment
// literal.
const CacheTokenFileEnv = "LEGION_REPOSITORY_CACHE_TOKEN_FILE"

// Options is one cache's: the repository, the volume its bare clone lives on (at
// <Root>/<owner>/<repo>.git, workspace.FeedRepository's layout), the provisioning token's file, the
// file holding the bearer token its clients present (read again for every request, so a token the
// Secret's projection rewrote is the one taken), where each fetch's one-shot credential goes (the
// container's TMPDIR), how long between fetches, the address it serves on, its log, and the runner
// its git runs under.
type Options struct {
	Repo           ghrepo.Repository
	Root           string
	TokenFile      string
	CacheTokenFile string
	CredentialDir  string
	Refresh        time.Duration
	Listen         string
	Log            func(line string)
	Run            workspace.Runner
}

// Health is what GET /healthz answers: whether the cache has a clone to serve, its repository, the
// default branch's head, when its last fetch in this process succeeded (zero before one has), and
// how many bytes /tar streams.
type Health struct {
	Ready       bool      `json:"ready"`
	Repo        string    `json:"repo"`
	Head        string    `json:"head,omitempty"`
	LastFetchAt time.Time `json:"lastFetchAt,omitzero"`
	Bytes       int64     `json:"bytes"`
}

// repackEvery is how long after the clone, or its last repack, a fetch repacks it.
var repackEvery = 24 * time.Hour

// Serve listens, then clones the repository onto the volume — or, when the volume holds it, fetches
// into it — then serves until ctx ends:
//
//	GET /readyz   the kubelet's readiness probe, which carries no token: 200 once the clone is
//	              there to serve, 503 before, and nothing else.
//	GET /healthz  200 {"ready":true,"repo":"acme/widgets","head":"<default branch sha>","lastFetchAt":"…","bytes":N}
//	              once the clone is there to serve; 503 {"ready":false,…} before.
//	GET /tar      a tar stream of the bare clone's refs and packs (workspace.FeedEntry), taken under
//	              the read lock, so no fetch moves refs or packs while it streams; 503 before the
//	              clone is there.
//
// /tar answers only a request whose `Authorization: Bearer` is the token CacheTokenFile holds now,
// compared in constant time; any other is 401 and names nothing. /healthz answers any request: its
// health holds no secret and nothing of the repository's content, the same class of information as
// /readyz, so the daemon reads it with no credential at all. Both name the repository in
// workspace.CacheRepositoryHeader on every answer they give, which a reader checks is the
// repository it asked for before it takes anything.
//
// Every fetch holds the write lock for workspace.Fetch, which reaches GitHub with the one-shot
// credential, the isolated git configuration and the pins every holder of a provisioning token
// uses, fetching every branch and pruning the deleted ones, and turned the clone's automatic gc
// off when it cloned; a repack (`git repack -a -d`) runs under the same lock once repackEvery has
// passed. /readyz and /healthz take no lock, so neither the readiness probe nor a health read ever
// waits on a fetch. A fetch that fails
// is logged and tried at the next tick; the cache stays ready on its last good state, a restart's
// clone on the volume included. A first clone that fails ends Serve with its failure: there is
// nothing to serve, and the kubelet restarts the container.
func Serve(ctx context.Context, opts Options) error {
	if err := opts.check(); err != nil {
		return err
	}
	if _, err := config.ReadSecretPointer(CacheTokenFileEnv, opts.CacheTokenFile); err != nil {
		return err
	}
	dir, err := workspace.FeedRepository(opts.Root, opts.Repo)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", opts.Listen, err)
	}
	c := &cache{opts: opts, dir: dir}
	c.health.Store(&Health{Repo: opts.Repo.String()})
	server := &http.Server{Handler: c.handler(), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	opts.Log(fmt.Sprintf("serving %s on %s", opts.Repo, listener.Addr()))

	present, err := pathExists(dir)
	if err != nil {
		return err
	}
	if err := c.refresh(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		if !present {
			return err
		}
		opts.Log("fetch failed; serving the clone as it was: " + oneLine(err))
		if err := c.publish(ctx, time.Time{}); err != nil {
			return err
		}
	}
	ticker := time.NewTicker(opts.Refresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-served:
			return fmt.Errorf("serve %s: %w", opts.Repo, err)
		case <-ticker.C:
			if err := c.refresh(ctx); err != nil && ctx.Err() == nil {
				opts.Log("fetch failed; serving the clone as it was: " + oneLine(err))
			}
		}
	}
}

func (opts Options) check() error {
	switch {
	case opts.Repo.IsZero():
		return errors.New("repository cache repository is required")
	case !filepath.IsAbs(opts.Root):
		return fmt.Errorf("repository cache root must be an absolute path (got %q)", opts.Root)
	case opts.TokenFile == "":
		return errors.New("repository cache token file is required")
	case opts.CacheTokenFile == "":
		return errors.New("repository cache bearer token file is required")
	case opts.CredentialDir == "":
		return errors.New("repository cache credential directory is required")
	case opts.Refresh <= 0:
		return fmt.Errorf("repository cache refresh must be positive (got %s)", opts.Refresh)
	case opts.Listen == "":
		return errors.New("repository cache listen address is required")
	case opts.Log == nil:
		return errors.New("repository cache log is required")
	case opts.Run == nil:
		return errors.New("repository cache runner is required")
	}
	return nil
}

// cache is Serve's state: lock is the clone's, written by every fetch and repack and read by every
// tar; health is what /healthz answers, swapped whole after each fetch.
type cache struct {
	opts       Options
	dir        string
	lock       sync.RWMutex
	health     atomic.Pointer[Health]
	lastRepack time.Time
}

// refresh fetches the clone from GitHub under the write lock (cloning it the first time), repacks
// it when a repack is due, and publishes what it then holds. The token is read from its file again
// before every transfer (workspace.Fetch), as the file stands then. A repack that fails is logged
// and tried again at the next fetch; the fetch it followed still counts.
func (c *cache) refresh(ctx context.Context) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	if _, err := workspace.Fetch(ctx, c.opts.Run, workspace.FetchRequest{
		Repo: c.opts.Repo, CredentialDir: c.opts.CredentialDir, Feed: c.opts.Root,
		Token: func() (string, error) { return config.ReadSecretPointer(tokenFileEnv, c.opts.TokenFile) },
		Log:   c.opts.Log, Clock: workspace.WallClock{},
	}); err != nil {
		return err
	}
	fetched := time.Now()
	if c.lastRepack.IsZero() {
		c.lastRepack = fetched
	} else if fetched.Sub(c.lastRepack) >= repackEvery {
		if _, err := workspace.FeedGit(ctx, c.opts.Run, c.dir, "repack", "-a", "-d", "--quiet"); err != nil {
			c.opts.Log("repack failed; the next fetch tries again: " + oneLine(err))
		} else {
			c.lastRepack = fetched
			c.opts.Log("repacked " + c.dir)
		}
	}
	return c.publishLocked(ctx, fetched)
}

// publish makes /healthz answer ready with what the clone holds, fetched when it was last fetched
// (zero when this process has not fetched it).
func (c *cache) publish(ctx context.Context, fetched time.Time) error {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.publishLocked(ctx, fetched)
}

func (c *cache) publishLocked(ctx context.Context, fetched time.Time) error {
	head, err := workspace.FeedGit(ctx, c.opts.Run, c.dir, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("read the default branch's head: %w", err)
	}
	var size int64
	if err := c.walk(func(_ string, _ fs.DirEntry, info fs.FileInfo) error {
		if info.Mode().IsRegular() {
			size += info.Size()
		}
		return nil
	}); err != nil {
		return err
	}
	c.health.Store(&Health{Ready: true, Repo: c.opts.Repo.String(), Head: strings.TrimSpace(head.Stdout), LastFetchAt: fetched, Bytes: size})
	return nil
}

// walk calls visit, in lexical order, for every regular file and directory of the clone the tar
// streams, with its slash-separated path relative to the clone: the clone's history and refs
// (workspace.FeedEntry), nothing of its own configuration, and no temporary or lock file git is
// writing.
func (c *cache) walk(visit func(name string, entry fs.DirEntry, info fs.FileInfo) error) error {
	return filepath.WalkDir(c.dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == c.dir {
			return nil
		}
		relative, err := filepath.Rel(c.dir, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if (!entry.IsDir() && !entry.Type().IsRegular()) || !workspace.FeedEntry(name, entry.IsDir()) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return visit(name, entry, info)
	})
}

func (c *cache) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !c.health.Load().Ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		health := c.health.Load()
		w.Header().Set(workspace.CacheRepositoryHeader, c.opts.Repo.String())
		w.Header().Set("Content-Type", "application/json")
		if !health.Ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(health)
	})
	mux.HandleFunc("GET /tar", c.authorized(c.serveTar))
	return mux
}

// authorized serves next only for a request whose bearer token is the one CacheTokenFile holds
// now, naming the repository in workspace.CacheRepositoryHeader; any other request is 401 and
// names nothing. A token file it cannot read is 500, logged.
func (c *cache) authorized(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want, err := config.ReadSecretPointer(CacheTokenFileEnv, c.opts.CacheTokenFile)
		if err != nil {
			c.opts.Log("read the bearer token: " + oneLine(err))
			http.Error(w, "the repository cache cannot read its token", http.StatusInternalServerError)
			return
		}
		presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(presented), []byte(want)) != 1 {
			http.Error(w, "this repository cache's token is required", http.StatusUnauthorized)
			return
		}
		w.Header().Set(workspace.CacheRepositoryHeader, c.opts.Repo.String())
		next(w, r)
	}
}

// serveTar streams the clone as it stood when the request came: its refs and packs taken under the
// read lock (snapshot), then sent with no lock held, so a slow or stalled reader holds nothing a
// refresh waits on. Before the first clone is there it answers 503 at once, without waiting on the
// lock the clone holds. A failure once the stream has begun aborts the connection, so the reader
// sees a broken transfer rather than a tar that merely ends early.
func (c *cache) serveTar(w http.ResponseWriter, r *http.Request) {
	if !c.health.Load().Ready {
		http.Error(w, "the repository cache holds no clone yet", http.StatusServiceUnavailable)
		return
	}
	entries, err := c.snapshot()
	if err != nil {
		c.opts.Log("take the clone's tar for " + r.RemoteAddr + ": " + oneLine(err))
		http.Error(w, "the repository cache cannot read its clone", http.StatusInternalServerError)
		return
	}
	defer closeEntries(entries)
	w.Header().Set("Content-Type", "application/x-tar")
	archive := tar.NewWriter(w)
	for _, entry := range entries {
		if err = archive.WriteHeader(entry.header); err != nil {
			break
		}
		switch {
		case entry.file != nil:
			_, err = io.CopyN(archive, entry.file, entry.header.Size)
		case entry.body != nil:
			_, err = archive.Write(entry.body)
		}
		if err != nil {
			err = fmt.Errorf("stream %s: %w", entry.header.Name, err)
			break
		}
	}
	if err == nil {
		err = archive.Close()
	}
	if err != nil {
		if r.Context().Err() == nil {
			c.opts.Log("tar stream to " + r.RemoteAddr + " failed: " + oneLine(err))
		}
		panic(http.ErrAbortHandler)
	}
}

// tarEntry is one entry of a snapshot of the clone: its tar header, and for a file either its bytes
// (HEAD, packed-refs and the refs) or, for a pack, the pack opened.
type tarEntry struct {
	header *tar.Header
	body   []byte
	file   *os.File
}

// snapshot takes, under the read lock, everything one tar stream sends (walk): each entry's header,
// the refs' bytes, and each pack opened. No fetch or repack runs while it holds the lock, so the
// refs it read name only objects in the packs it opened; a pack never changes once written, and an
// open pack stays readable after a repack removes it, so the stream needs no lock.
func (c *cache) snapshot() ([]tarEntry, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	var entries []tarEntry
	err := c.walk(func(name string, entry fs.DirEntry, info fs.FileInfo) error {
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name, header.Uid, header.Gid, header.Uname, header.Gname = name, 0, 0, "", ""
		taken := tarEntry{header: header}
		path := filepath.Join(c.dir, filepath.FromSlash(name))
		switch {
		case entry.IsDir():
			header.Name += "/"
		case strings.HasPrefix(name, "objects/pack/"):
			if taken.file, err = os.Open(path); err != nil {
				return err
			}
		default:
			if taken.body, err = os.ReadFile(path); err != nil {
				return err
			}
			header.Size = int64(len(taken.body))
		}
		entries = append(entries, taken)
		return nil
	})
	if err != nil {
		closeEntries(entries)
		return nil, err
	}
	return entries, nil
}

// closeEntries closes every pack a snapshot opened.
func closeEntries(entries []tarEntry) {
	for _, entry := range entries {
		if entry.file != nil {
			_ = entry.file.Close()
		}
	}
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// oneLine is err's text on one line, its lines joined by " | ", as the fetch logs a retried failure.
func oneLine(err error) string {
	return strings.ReplaceAll(strings.TrimSpace(err.Error()), "\n", " | ")
}
