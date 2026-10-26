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
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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

// repackTimeout bounds one repack, which follows the repository's size, in place of the runner's
// five-minute slow-command budget. A full `git repack -a -d` of a 3.2 GB, 5.4 million-object clone
// took 361 s at one core on a development host, and a gVisor cache pod ran the same git work four
// to six times slower per core on 2026-10-26, so the five minutes killed it every time, at every
// refresh once it was due. Nothing waits on a repack but the next fetch (refresh): it runs outside
// the clone's write lock, so a tar is served throughout.
var repackTimeout = 2 * time.Hour

// maxStreams is how many /tar streams the cache sends at once; a request past it waits for a stream
// to end, first come first served, within its own deadline. The streams share the pod's sending
// rate evenly, so with no bound every stream slows as the next pod launches, and a large launch
// leaves none of them done: on 2026-10-26 a 2.1 GB clone's cache sent about 400 MB/s in all over
// 290 streams at once, about 29 minutes before any reached its last byte, past the 30 minutes an
// issue pod's fetch gives the stream and its verification (workspace.FetchTimeout), so every fetch
// failed and started over. Bounded, a stream takes its share of the rate among maxStreams, and the
// queue drains at the same total rate.
var maxStreams = 32

// Serve listens, then clones the repository onto the volume — or, when the volume holds it, fetches
// into it — then serves until ctx ends:
//
//	GET /readyz   the kubelet's readiness probe, which carries no token: 200 once the clone is
//	              there to serve, 503 before, and nothing else.
//	GET /healthz  200 {"ready":true,"repo":"acme/widgets","head":"<default branch sha>","lastFetchAt":"…","bytes":N}
//	              once the clone is there to serve; 503 {"ready":false,…} before.
//	GET /tar      a tar stream of the bare clone's refs and packs (workspace.FeedEntry), taken under
//	              the read lock, so no fetch moves refs or packs while it streams, at most
//	              maxStreams at once, a request past them waiting its turn; 503 before the clone
//	              is there.
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
// off when it cloned. Once repackEvery has passed, the fetch is followed by a repack
// (`git repack -a -d`, within repackTimeout) that holds no lock /tar takes: a tar is served while
// it runs (snapshot), and only the next fetch waits for it. /readyz and /healthz take no lock, so
// neither the readiness probe nor a health read ever waits on a fetch or a repack. A fetch that
// fails is logged and tried at the next tick; the cache stays ready on its last good state, a
// restart's clone on the volume included. A first clone that fails ends Serve with its failure:
// there is nothing to serve, and the kubelet restarts the container. As its container's PID 1,
// Serve reaps every orphan git leaves it after each refresh (reapOrphans).
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
	c := newCache(opts, dir)
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
	if present {
		c.removePartialPacks()
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

// cache is Serve's state: lock is the clone's refs and packs as a tar reads them, written by every
// fetch and read by every tar; maint keeps a fetch and a repack from running at once, and is held
// by refresh alone; health is what /healthz answers, swapped whole after each fetch and repack;
// streams holds one token per /tar stream being sent, maxStreams at most.
type cache struct {
	opts       Options
	dir        string
	lock       sync.RWMutex
	maint      sync.Mutex
	health     atomic.Pointer[Health]
	lastRepack time.Time
	streams    chan struct{}
}

// newCache is the state Serve keeps for opts' clone at dir.
func newCache(opts Options, dir string) *cache {
	return &cache{opts: opts, dir: dir, streams: make(chan struct{}, maxStreams)}
}

// refresh fetches the clone from GitHub under the write lock (cloning it the first time) and
// publishes what it then holds; then, when a repack is due, repacks it (repack). The token is read
// from its file again before every transfer (workspace.Fetch), as the file stands then. The repack
// runs outside the write lock: it changes no ref and adds no object, and an open pack stays readable
// after the repack removes it, so a tar is served while it runs (snapshot takes a pack that vanished
// between its listing and its opening again). maint keeps the next fetch from starting until the
// repack ends. A fetch that fails has the partial packs its `git index-pack` left removed, as a
// failed repack does. Once both have ended, and no git of this process runs, the orphans git left
// are reaped (reapOrphans).
func (c *cache) refresh(ctx context.Context) error {
	c.maint.Lock()
	defer c.maint.Unlock()
	defer reapOrphans(c.opts.Log)
	fetched, err := c.fetch(ctx)
	if err != nil {
		c.removePartialPacks()
		return err
	}
	if c.lastRepack.IsZero() {
		at, ok := c.recordedRepack()
		if !ok {
			c.lastRepack = fetched
			c.recordRepack(fetched)
			return nil
		}
		c.lastRepack = at
	}
	if fetched.Sub(c.lastRepack) < repackEvery {
		return nil
	}
	c.repack(ctx, fetched)
	if err := c.publish(ctx, fetched); err != nil {
		return fmt.Errorf("publish the clone after its repack: %w", err)
	}
	return nil
}

// lastRepackFile is where the cache keeps, on its own volume beside the clone, when it last
// repacked or tried to: a cache pod is replaced at every image change and on eviction, more often
// than repackEvery, and a time kept in memory alone would start the wait again with every new
// process, so the clone would never be repacked.
const lastRepackFile = ".legion-last-repack"

// recordedRepack is when the clone was last repacked, or a repack last tried, as lastRepackFile
// records it, and whether it records one that reads as an RFC 3339 time. With none, the process's
// first fetch starts the record and repacks nothing: a clone just cloned needs no repack.
func (c *cache) recordedRepack() (time.Time, bool) {
	body, err := os.ReadFile(filepath.Join(c.opts.Root, lastRepackFile))
	if err != nil {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(string(body)))
	return at, err == nil
}

// recordRepack writes at to lastRepackFile, logging a failure: a lost record costs one repack too
// early, nothing more.
func (c *cache) recordRepack(at time.Time) {
	if err := os.WriteFile(filepath.Join(c.opts.Root, lastRepackFile), []byte(at.UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		c.opts.Log("record the last repack: " + oneLine(err))
	}
}

// fetch fetches the clone under the write lock and publishes what it holds, answering when. The
// first clone gets workspace.CacheFetchTimeout rather than an issue pod's FetchTimeout, since it is
// the repository's one full download; a fetch into the clone keeps FetchTimeout, since it holds the
// lock every tar waits on and brings only what GitHub gained.
func (c *cache) fetch(ctx context.Context) (time.Time, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	cloned, err := pathExists(c.dir)
	if err != nil {
		return time.Time{}, err
	}
	bound := workspace.CacheFetchTimeout
	if cloned {
		bound = workspace.FetchTimeout
	}
	if _, err := workspace.Fetch(ctx, c.opts.Run, workspace.FetchRequest{
		Repo: c.opts.Repo, CredentialDir: c.opts.CredentialDir, Feed: c.opts.Root,
		Token: func() (string, error) { return config.ReadSecretPointer(tokenFileEnv, c.opts.TokenFile) },
		Log:   c.opts.Log, Clock: workspace.WallClock{}, Bound: bound,
	}); err != nil {
		return time.Time{}, err
	}
	fetched := time.Now()
	return fetched, c.publishLocked(ctx, fetched)
}

// repackMemory bounds the repack's memory, which git leaves unbounded by default: one thread, a
// 256 MiB delta window, 64 MiB of delta and delta-base caches, and pack data mapped 64 MiB at a
// time up to 512 MiB. A full repack of a 3.2 GB, 5.4 million-object clone peaked at 4.35 GiB
// resident without it, past the cache container's 4Gi default memory limit, and at 1.78 GiB with
// it, taking 7m04s rather than 6m07s at one core (sjawhar/legion#2240). It writes no bitmap
// either: the cache serves no git protocol and streams no `.bitmap`, so one costs time and memory
// for nothing.
var repackMemory = []string{
	"-c", "pack.threads=1", "-c", "pack.windowMemory=256m", "-c", "pack.deltaCacheSize=64m",
	"-c", "core.deltaBaseCacheLimit=64m", "-c", "core.packedGitLimit=512m", "-c", "core.packedGitWindowSize=64m",
	"-c", "repack.writeBitmaps=false",
}

// repack runs `git repack -a -d` on the clone within repackTimeout and repackMemory. One that fails
// is logged, its partial packs are removed at once, and it is tried again a repackEvery later
// rather than at the next fetch, so one that cannot fit never holds maint, and the cache's CPU,
// from fetch to fetch. Either way the attempt's time is recorded (recordRepack). Removing the
// partial packs (removePartialPacks) is what keeps a repack that keeps failing from filling the
// cache's volume: on 2026-10-26 one deployment's cache's 20Gi volume filled with two killed
// repacks' 13 GB of `tmp_pack_*` files, after which every fetch failed `No space left on device`
// (sjawhar/legion#2240).
func (c *cache) repack(ctx context.Context, fetched time.Time) {
	c.lastRepack = fetched
	c.recordRepack(fetched)
	if _, err := workspace.FeedGitWithin(ctx, c.opts.Run, c.dir, repackTimeout, append(slices.Clone(repackMemory), "repack", "-a", "-d", "--quiet")...); err != nil {
		c.opts.Log(fmt.Sprintf("repack failed; tried again in %s: %s", repackEvery, oneLine(err)))
		c.removePartialPacks()
		return
	}
	c.opts.Log("repacked " + c.dir)
}

// removePartialPacks removes, and logs, each partial pack a killed git left in the clone's
// objects/pack: every `tmp_*` git writes there (a pack, its index, reverse index, bitmap and mtimes
// as it receives or writes them), and the `.tmp-*` pack pack-objects writes for a repack before
// renaming it. Its callers run it only while no git of the cache runs: before the
// first fetch of a process (a pod killed mid-repack leaves its partial packs on the volume), and
// after a fetch or a repack that failed, under maint. It looks only in objects/pack, and only when neither
// objects nor objects/pack is a link.
func (c *cache) removePartialPacks() {
	pack := filepath.Join(c.dir, "objects", "pack")
	for _, dir := range []string{filepath.Dir(pack), pack} {
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
			return
		}
	}
	entries, err := os.ReadDir(pack)
	if err != nil {
		c.opts.Log("read " + pack + " for partial packs: " + oneLine(err))
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !(strings.HasPrefix(name, "tmp_") || strings.HasPrefix(name, ".tmp-")) {
			continue
		}
		if err := os.Remove(filepath.Join(pack, name)); err != nil {
			c.opts.Log("remove the partial pack " + name + ": " + oneLine(err))
			continue
		}
		c.opts.Log("removed the partial pack " + name + ", which a killed git left")
	}
}

// reapOrphans reaps, when this process is its container's PID 1, every exited process it was
// handed as their parent died: git's helpers (`git remote-https`, `git index-pack`) outlive a git
// that dies on an error, and the kernel gives them to PID 1, which nothing else waits for. On
// 2026-10-26 one deployment's cache held 56 such zombies, two more at every refresh. It runs only between
// refreshes, with no git of this process running, so every process left to it is such an orphan
// and none is a child an exec.Cmd still waits for. As launcher.reapNamespace does, it waits on
// each process by id with WNOHANG, so it never blocks on one still running.
func reapOrphans(log func(string)) {
	if os.Getpid() != 1 {
		return
	}
	if _, err := reapExited(); err != nil {
		log("reap the orphans git left: " + oneLine(err))
	}
}

// reapExited waits, with WNOHANG, on every process /proc lists but this one and PID 1, and answers
// how many it reaped. A process that is not this one's child answers ECHILD and is passed over.
func reapExited() (int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	reaped := 0
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 || pid == os.Getpid() {
			continue
		}
		got, err := syscall.Wait4(pid, nil, syscall.WNOHANG, nil)
		if err != nil && !errors.Is(err, syscall.ECHILD) && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EINTR) {
			return reaped, fmt.Errorf("reap process %d: %w", pid, err)
		}
		if got == pid {
			reaped++
		}
	}
	return reaped, nil
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

// serveTar streams the clone as it stood when the stream began: its refs and packs taken under the
// read lock (snapshot), then sent with no lock held, so a slow or stalled reader holds nothing a
// refresh waits on. Before the first clone is there it answers 503 at once, without waiting on the
// lock the clone holds. Once maxStreams streams are being sent it waits, holding no lock and no
// open pack, for one of them to end, first come first served, and gives up with nothing sent when
// its request ends first. A failure once the stream has begun aborts the connection, so the reader
// sees a broken transfer rather than a tar that merely ends early.
func (c *cache) serveTar(w http.ResponseWriter, r *http.Request) {
	if !c.health.Load().Ready {
		http.Error(w, "the repository cache holds no clone yet", http.StatusServiceUnavailable)
		return
	}
	select {
	case c.streams <- struct{}{}:
		defer func() { <-c.streams }()
	case <-r.Context().Done():
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
// the refs' bytes, and each pack opened. No fetch runs while it holds the lock, so the refs it read
// name only objects in the packs the clone held; a pack never changes once written, and an open
// pack stays readable after a repack removes it, so the stream needs no lock. A repack may run
// meanwhile (refresh): it adds a pack and removes the ones it replaced, and the snapshot must have
// taken all of the old ones or the new one. So a snapshot whose pack was removed before it opened
// it, or that finds the clone's packs other than the ones it took once it has taken them, is taken
// again, up to snapshotTries times, snapshotPause apart: a repack moves its packs in moments at its
// very end.
func (c *cache) snapshot() ([]tarEntry, error) {
	var lastErr error
	for try := range snapshotTries {
		if try > 0 {
			time.Sleep(snapshotPause)
		}
		entries, err := c.snapshotOnce()
		if err == nil {
			return entries, nil
		}
		if !errors.Is(err, errPacksMoved) && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("the clone's packs kept changing under %d snapshots: %w", snapshotTries, lastErr)
}

// snapshotTries and snapshotPause bound snapshot's retakes while a repack moves its packs.
const (
	snapshotTries = 20
	snapshotPause = 250 * time.Millisecond
)

// snapshotWalked, when set, runs in each snapshot between taking the clone's entries and checking
// its packs are still those: a test's seam for a repack landing there. Nil outside tests.
var snapshotWalked func()

// errPacksMoved is a snapshot that found the clone's packs changed once it had taken them.
var errPacksMoved = errors.New("the clone's packs changed while the snapshot took them")

func (c *cache) snapshotOnce() ([]tarEntry, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	var entries []tarEntry
	taken := map[string]bool{}
	err := c.walk(func(name string, entry fs.DirEntry, info fs.FileInfo) error {
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name, header.Uid, header.Gid, header.Uname, header.Gname = name, 0, 0, "", ""
		one := tarEntry{header: header}
		path := filepath.Join(c.dir, filepath.FromSlash(name))
		switch {
		case entry.IsDir():
			header.Name += "/"
		case strings.HasPrefix(name, "objects/pack/"):
			if one.file, err = os.Open(path); err != nil {
				return err
			}
			taken[path] = true
		default:
			if one.body, err = os.ReadFile(path); err != nil {
				return err
			}
			header.Size = int64(len(one.body))
		}
		entries = append(entries, one)
		return nil
	})
	if err == nil && snapshotWalked != nil {
		snapshotWalked()
	}
	if err == nil {
		err = c.samePacks(taken)
	}
	if err != nil {
		closeEntries(entries)
		return nil, err
	}
	return entries, nil
}

// samePacks is errPacksMoved when the packs the clone holds now (the walk's entries under
// objects/pack) are not exactly taken.
func (c *cache) samePacks(taken map[string]bool) error {
	now := map[string]bool{}
	err := c.walk(func(name string, entry fs.DirEntry, _ fs.FileInfo) error {
		if !entry.IsDir() && strings.HasPrefix(name, "objects/pack/") {
			now[filepath.Join(c.dir, filepath.FromSlash(name))] = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !maps.Equal(now, taken) {
		return errPacksMoved
	}
	return nil
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
