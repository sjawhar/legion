package repocache

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const testTimeout = 30 * time.Second

// gitHub is how the tests' git reaches https://github.com/acme/widgets: the URL in a command's argv
// is replaced with the local bare remote, over git's file transport, which the tools' wrappers add
// to the runner's https-only allow-list.
const gitHub = "https://github.com/acme/widgets"

// testRunner runs every command through workspace.NewRunner with the real git, the GitHub URL in
// its argv replaced by the local bare remote. It records, for every clone and fetch, the token its
// one-shot credential held while it ran. A test that arms hold has the next clone or fetch signal
// started and wait for release before it runs.
type testRunner struct {
	remote string
	others map[string]string
	runner workspace.Runner

	mu      sync.Mutex
	argv    [][]string
	tokens  []string
	held    chan struct{}
	release chan struct{}
	// on, when set, runs in place of the next command whose argv holds its word (onWord).
	onWord string
	on     func(ctx context.Context, command workspace.Command) (workspace.Result, error)
}

// instead arms the next command whose argv holds word to run do in place of git; do may run the
// command itself (real).
func (r *testRunner) instead(word string, do func(ctx context.Context, command workspace.Command) (workspace.Result, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onWord, r.on = word, do
}

func newTestRunner(t *testing.T, remote string) *testRunner {
	t.Helper()
	tools := map[string]string{}
	dir := t.TempDir()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("the cache's tests drive a real git: %v", err)
	}
	wrapper := filepath.Join(dir, "git")
	script := "#!/bin/sh\n[ -z \"${GIT_ALLOW_PROTOCOL+set}\" ] || export GIT_ALLOW_PROTOCOL=\"$GIT_ALLOW_PROTOCOL:file\"\nexec '" + path + "' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	tools["git"] = wrapper
	return &testRunner{remote: remote, others: map[string]string{}, runner: workspace.NewRunner(testTimeout, tools)}
}

// serves has the runner's git reach repo's GitHub URL at the local bare remote too.
func (r *testRunner) serves(repo, remote string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.others[workspace.GitHubURL(ghrepo.MustParse(repo))] = remote
}

func (r *testRunner) Timeout() time.Duration { return testTimeout }

func (r *testRunner) Run(ctx context.Context, command workspace.Command) (workspace.Result, error) {
	transfer := false
	actual := command
	actual.Argv = append([]string(nil), command.Argv...)
	r.mu.Lock()
	for i, arg := range actual.Argv {
		if arg == gitHub {
			actual.Argv[i] = "file://" + r.remote
			transfer = true
		} else if other, ok := r.others[arg]; ok {
			actual.Argv[i] = "file://" + other
			transfer = true
		}
	}
	r.mu.Unlock()
	r.mu.Lock()
	r.argv = append(r.argv, command.Argv)
	if r.on != nil && slices.Contains(command.Argv, r.onWord) {
		do := r.on
		r.on, r.onWord = nil, ""
		r.mu.Unlock()
		return do(ctx, actual)
	}
	var held, release chan struct{}
	if transfer {
		token := ""
		for _, entry := range command.Env {
			if file, ok := strings.CutPrefix(entry, "LEGION_PROVISIONING_TOKEN_FILE="); ok {
				if body, err := os.ReadFile(file); err == nil {
					token = string(body)
				}
			}
		}
		r.tokens = append(r.tokens, token)
		// A fetch's read of GitHub's HEAD is a transfer too, but hold is for the next clone or fetch.
		if !slices.Contains(command.Argv, "ls-remote") {
			held, release = r.held, r.release
			r.held, r.release = nil, nil
		}
	}
	r.mu.Unlock()
	if held != nil {
		close(held)
		select {
		case <-release:
		case <-ctx.Done():
			return workspace.Result{}, ctx.Err()
		}
	}
	return r.runner.Run(ctx, actual)
}

// hold arms the next clone or fetch to wait: started is closed once it is reached, and it runs once
// the returned release is called.
func (r *testRunner) hold() (started <-chan struct{}, release func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.held, r.release = make(chan struct{}), make(chan struct{})
	started, gate := r.held, r.release
	var once sync.Once
	return started, func() { once.Do(func() { close(gate) }) }
}

// transfers is every clone and fetch the runner saw, its argv's subcommand each.
func (r *testRunner) transfers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var seen []string
	for _, argv := range r.argv {
		for _, arg := range argv {
			if arg != gitHub {
				continue
			}
			args := argv[1:]
			for len(args) > 2 && args[0] == "-c" {
				args = args[2:]
			}
			seen = append(seen, args[0]+" "+args[1])
			break
		}
	}
	return seen
}

func (r *testRunner) seenTokens() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.tokens...)
}

// git runs a fixture git command in dir and returns its output, trimmed.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Legion test", "GIT_AUTHOR_EMAIL=legion-test@example.invalid",
		"GIT_COMMITTER_NAME=Legion test", "GIT_COMMITTER_EMAIL=legion-test@example.invalid")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

// newRemote is a bare remote whose default branch is main, holding one commit.
func newRemote(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	git(t, root, "init", "--bare", "--quiet", "--initial-branch=main", remote)
	advance(t, remote, "seed")
	return remote
}

// advance pushes one more commit, adding a file named for message, to the remote's main and returns
// its id.
func advance(t *testing.T, remote, message string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	git(t, filepath.Dir(work), "clone", "--quiet", remote, work)
	if err := os.WriteFile(filepath.Join(work, message+".txt"), []byte(message+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "--quiet", "-m", message)
	git(t, work, "push", "--quiet", "origin", "HEAD:main")
	return git(t, work, "rev-parse", "HEAD")
}

// syncLog collects the cache's log lines, safe for the test to read while Serve writes.
type syncLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *syncLog) log(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
}

func (l *syncLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// running is one running Serve: its options, its address, its log, and Serve's answer once it ends.
type running struct {
	opts   Options
	run    *testRunner
	url    string
	log    *syncLog
	cancel context.CancelFunc
	done   chan error
}

// testCacheToken is the bearer a test cache of acme/widgets takes.
const testCacheToken = "cache-token-of-acme-widgets"

// testOptions are a cache of acme/widgets's options on a fresh volume: its provisioning token file
// holding ghs_first, and its bearer token file testCacheToken.
func testOptions(t *testing.T, run *testRunner) Options {
	t.Helper()
	return repoOptions(t, run, "acme/widgets", testCacheToken)
}

// repoOptions are the options of a cache of repo on a fresh volume, taking bearer.
func repoOptions(t *testing.T, run *testRunner, repo, bearer string) Options {
	t.Helper()
	dir := t.TempDir()
	tokenFile, bearerFile := filepath.Join(dir, "token"), filepath.Join(dir, "cache-token")
	if err := os.WriteFile(tokenFile, []byte("ghs_first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bearerFile, []byte(bearer+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Options{
		Repo: ghrepo.MustParse(repo), Root: t.TempDir(), TokenFile: tokenFile, CacheTokenFile: bearerFile, CredentialDir: t.TempDir(),
		Refresh: time.Hour, Listen: "127.0.0.1:0", Run: run,
	}
}

// get is GET path of the cache, presenting bearer when it is not empty.
func (c *running) get(t *testing.T, path, bearer string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, c.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// bearer is the token the cache takes, as its file holds it.
func (c *running) bearer(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(c.opts.CacheTokenFile)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(body))
}

// start runs Serve with opts until the test ends, and returns once it serves.
func start(t *testing.T, opts Options) *running {
	t.Helper()
	log := &syncLog{}
	opts.Log = log.log
	ctx, cancel := context.WithCancel(context.Background())
	c := &running{opts: opts, run: opts.Run.(*testRunner), log: log, cancel: cancel, done: make(chan error, 1)}
	go func() { c.done <- Serve(ctx, opts) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-c.done:
			if err != nil {
				t.Errorf("Serve ended with %v", err)
			}
		case <-time.After(testTimeout):
			t.Error("Serve did not end with its context")
		}
	})
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		for _, line := range log.all() {
			if address, ok := strings.CutPrefix(line, "serving "+opts.Repo.String()+" on "); ok {
				c.url = "http://" + address
				return c
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Serve never said where it serves: %q", log.all())
	return nil
}

// health is GET /healthz, with the cache's bearer: its status and its body.
func (c *running) health(t *testing.T) (int, Health) {
	t.Helper()
	response := c.get(t, "/healthz", c.bearer(t))
	defer response.Body.Close()
	var health Health
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("/healthz answered %d with a body that is not its JSON: %v", response.StatusCode, err)
	}
	return response.StatusCode, health
}

// ready waits until /healthz answers 200 and returns what it said.
func (c *running) ready(t *testing.T) Health {
	t.Helper()
	return c.waitFor(t, "ready", func(status int, _ Health) bool { return status == http.StatusOK })
}

// waitFor polls /healthz until done holds and returns what it said then.
func (c *running) waitFor(t *testing.T, what string, done func(int, Health) bool) Health {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		status, health := c.health(t)
		if done(status, health) {
			return health
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cache never became %s: /healthz answers %d %+v; log %q", what, status, health, c.log.all())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// dir is the cache's bare clone.
func (c *running) dir() string {
	return filepath.Join(c.opts.Root, "acme", "widgets.git")
}

// unpack writes a tar stream into a new directory and returns it with every entry name it held.
func unpack(t *testing.T, stream io.Reader) (string, []string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "widgets.git")
	var names []string
	reader := tar.NewReader(stream)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return dir, names
		}
		if err != nil {
			t.Fatalf("read the tar: %v", err)
		}
		names = append(names, header.Name)
		path := filepath.Join(dir, filepath.FromSlash(header.Name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("the tar holds %s, of type %q: neither a regular file nor a directory", header.Name, header.Typeflag)
		}
	}
}

// getTar is GET /tar with the cache's bearer, read whole.
func (c *running) getTar(t *testing.T) (int, []byte) {
	t.Helper()
	response := c.get(t, "/tar", c.bearer(t))
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read /tar: %v", err)
	}
	return response.StatusCode, body
}

// /healthz answers 503, not ready, until the first clone finished, and /tar answers 503 too; then
// 200 with the repository, the default branch's head, when that clone finished and how many bytes
// the clone holds.
func TestHealthzIsUnavailableUntilTheFirstCloneFinishes(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	started, release := run.hold()
	defer release()
	before := time.Now()
	c := start(t, testOptions(t, run))
	<-started

	status, health := c.health(t)
	if status != http.StatusServiceUnavailable || health.Ready || health.Repo != "acme/widgets" {
		t.Fatalf("/healthz during the clone answers %d %+v, want 503, not ready, acme/widgets", status, health)
	}
	if status, _ := c.getTar(t); status != http.StatusServiceUnavailable {
		t.Fatalf("/tar during the clone answers %d, want 503", status)
	}
	release()
	health = c.ready(t)
	if want := git(t, remote, "--git-dir="+remote, "rev-parse", "main"); health.Head != want {
		t.Errorf("/healthz head = %s, want the remote's main %s", health.Head, want)
	}
	if health.Bytes <= 0 || health.LastFetchAt.Before(before) || health.LastFetchAt.After(time.Now()) {
		t.Errorf("/healthz = %+v, want the clone's bytes and the time it finished", health)
	}
	if transfers := run.transfers(); len(transfers) != 1 || transfers[0] != "clone --bare" {
		t.Errorf("the cache ran %q, want one clone", transfers)
	}
}

// /tar streams the bare clone's refs and packs alone — HEAD, packed-refs, refs/,
// objects/pack/pack-<id>.pack and the index git reads each pack through — and no reverse index,
// loose object, or anything of the clone's own configuration, hooks or descriptions: unpacked, it
// is a repository whose connectivity git checks and whose default branch is the remote's, holding
// regular files and directories alone and no temporary pack or lock file git was writing.
func TestTarStreamsTheBareRepositorysRefsAndPacks(t *testing.T) {
	remote := newRemote(t)
	c := start(t, testOptions(t, newTestRunner(t, remote)))
	c.ready(t)
	for _, planted := range []string{"objects/pack/tmp_pack_abc", "refs/heads/main.lock", "packed-refs.lock"} {
		if err := os.WriteFile(filepath.Join(c.dir(), planted), []byte("being written\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, own := range []string{"config", "description", "hooks", "info"} {
		if _, err := os.Stat(filepath.Join(c.dir(), own)); err != nil {
			t.Fatalf("the clone holds no %s to leave out: %v", own, err)
		}
	}

	status, body := c.getTar(t)
	if status != http.StatusOK {
		t.Fatalf("/tar answers %d", status)
	}
	unpacked, names := unpack(t, bytes.NewReader(body))
	packs, indexes := 0, 0
	for _, name := range names {
		switch {
		case name == "HEAD" || name == "packed-refs" || strings.HasPrefix(name, "refs/") || name == "objects/" || name == "objects/pack/":
		case regexp.MustCompile(`^objects/pack/pack-[0-9a-f]{40}\.pack$`).MatchString(name):
			packs++
		case regexp.MustCompile(`^objects/pack/pack-[0-9a-f]{40}\.idx$`).MatchString(name):
			indexes++
		default:
			t.Errorf("the tar holds %s, which is neither a ref nor a pack", name)
		}
	}
	if packs == 0 || indexes != packs {
		t.Fatalf("the tar holds %d packs and %d indexes: %q; want every pack with the index git reads it through", packs, indexes, names)
	}
	indexed(t, unpacked)
	if got, want := git(t, unpacked, "--git-dir="+unpacked, "rev-parse", "refs/heads/main"), git(t, remote, "--git-dir="+remote, "rev-parse", "main"); got != want {
		t.Errorf("the unpacked main is %s, want the remote's %s", got, want)
	}
}

// indexed checks the unpacked bare repository dir as an issue pod's fetch does: each pack the
// cache streamed came with the index git reads it through, so none is indexed again, and the
// history is whole.
func indexed(t *testing.T, dir string) {
	t.Helper()
	packs, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "pack-*.pack"))
	if err != nil {
		t.Fatal(err)
	}
	for _, pack := range packs {
		index := strings.TrimSuffix(pack, ".pack") + ".idx"
		if _, err := os.Stat(index); err != nil {
			t.Fatalf("the cache streamed %s without its index: %v", filepath.Base(pack), err)
		}
	}
	git(t, dir, "--git-dir="+dir, "fsck", "--connectivity-only")
}

// Every fetch the cache makes keeps what it brings as a pack: its clone never holds a loose
// object, which its tar would leave out of every issue's feed.
func TestTheCachesClonesHistoryStaysPacked(t *testing.T) {
	remote := newRemote(t)
	opts := testOptions(t, newTestRunner(t, remote))
	opts.Refresh = 50 * time.Millisecond
	c := start(t, opts)
	c.ready(t)
	moved := advance(t, remote, "moved")
	c.waitFor(t, "at the moved head", func(_ int, h Health) bool { return h.Head == moved })
	if loose := git(t, c.dir(), "--git-dir="+c.dir(), "count-objects"); !strings.HasPrefix(loose, "0 objects") {
		t.Errorf("the cache's clone holds %s after a fetch", loose)
	}
}

// A tar asked for while a fetch runs waits for the fetch and streams the state it left, and
// /healthz keeps answering ready meanwhile: the kubelet's readiness probe never waits on a fetch.
func TestATarAskedForDuringAFetchWaitsForIt(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	opts.Refresh = 50 * time.Millisecond
	c := start(t, opts)
	c.ready(t)
	moved := advance(t, remote, "moved")
	started, release := run.hold()
	defer release()
	<-started

	streamed := make(chan []byte, 1)
	request, err := http.NewRequest(http.MethodGet, c.url+"/tar", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+c.bearer(t))
	go func() {
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Error(err)
			streamed <- nil
			return
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		streamed <- body
	}()
	select {
	case <-streamed:
		t.Fatal("the tar streamed while a fetch ran")
	case <-time.After(300 * time.Millisecond):
	}
	if status, health := c.health(t); status != http.StatusOK || !health.Ready {
		t.Fatalf("/healthz during the fetch answers %d %+v, want ready", status, health)
	}
	release()
	var body []byte
	select {
	case body = <-streamed:
	case <-time.After(testTimeout):
		t.Fatal("the tar never streamed once the fetch ended")
	}
	unpacked, _ := unpack(t, bytes.NewReader(body))
	indexed(t, unpacked)
	if got := git(t, unpacked, "--git-dir="+unpacked, "rev-parse", "refs/heads/main"); got != moved {
		t.Errorf("the tar's main is %s, want what the fetch brought, %s", got, moved)
	}
}

// A tar's reader that stops reading partway holds nothing a refresh waits on: the stream takes the
// clone's refs and packs under the read lock and sends them with no lock held, so the next tick's
// fetch goes through while the reader stalls, /healthz moves to its head, and the stalled reader,
// once it reads on, gets the whole tar it began, as it stood when it began. The pack is large
// enough that the cache blocks writing it to a reader that stopped.
func TestAStalledTarReaderDoesNotHoldTheNextFetch(t *testing.T) {
	remote := newRemote(t)
	large := make([]byte, 24<<20)
	if _, err := rand.Read(large); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "work")
	git(t, filepath.Dir(work), "clone", "--quiet", remote, work)
	if err := os.WriteFile(filepath.Join(work, "large.bin"), large, 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "--quiet", "-m", "large")
	git(t, work, "push", "--quiet", "origin", "HEAD:main")
	began := git(t, work, "rev-parse", "HEAD")
	opts := testOptions(t, newTestRunner(t, remote))
	opts.Refresh = 50 * time.Millisecond
	c := start(t, opts)
	c.waitFor(t, "at the large commit", func(_ int, h Health) bool { return h.Head == began })

	stalled := c.get(t, "/tar", c.bearer(t))
	defer stalled.Body.Close()
	first := make([]byte, 4096)
	if _, err := io.ReadFull(stalled.Body, first); err != nil {
		t.Fatalf("read the tar's first bytes: %v", err)
	}
	moved := advance(t, remote, "moved")
	c.waitFor(t, "at the moved head while a tar's reader stalls", func(_ int, h Health) bool { return h.Head == moved })

	unpacked, _ := unpack(t, io.MultiReader(bytes.NewReader(first), stalled.Body))
	indexed(t, unpacked)
	if got := git(t, unpacked, "--git-dir="+unpacked, "rev-parse", "refs/heads/main"); got != began {
		t.Errorf("the stalled tar's main is %s, want the head it began at, %s", got, began)
	}
}

// Past maxStreams streams a tar waits for one to end: it gets nothing while the stream ahead of it
// is sent, a waiter whose request ends first gives up having sent nothing and holding no turn, and
// the next waiter gets the whole tar once the stream ahead ends.
func TestATarPastTheStreamBoundWaitsForAStreamToEnd(t *testing.T) {
	defer func(was int) { maxStreams = was }(maxStreams)
	maxStreams = 1
	remote := newRemote(t)
	large := make([]byte, 24<<20)
	if _, err := rand.Read(large); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "work")
	git(t, filepath.Dir(work), "clone", "--quiet", remote, work)
	if err := os.WriteFile(filepath.Join(work, "large.bin"), large, 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "--quiet", "-m", "large")
	git(t, work, "push", "--quiet", "origin", "HEAD:main")
	head := git(t, work, "rev-parse", "HEAD")
	c := start(t, testOptions(t, newTestRunner(t, remote)))
	c.waitFor(t, "at the large commit", func(_ int, h Health) bool { return h.Head == head })

	ahead := c.get(t, "/tar", c.bearer(t))
	defer ahead.Body.Close()
	first := make([]byte, 4096)
	if _, err := io.ReadFull(ahead.Body, first); err != nil {
		t.Fatalf("read the first tar's first bytes: %v", err)
	}

	ask := func(ctx context.Context) (*http.Response, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/tar", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+c.bearer(t))
		return http.DefaultClient.Do(request)
	}
	impatient, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if response, err := ask(impatient); err == nil {
		response.Body.Close()
		t.Fatalf("a tar past the bound was answered %s while the stream ahead of it was sent", response.Status)
	}

	answered := make(chan *http.Response, 1)
	go func() {
		response, err := ask(context.Background())
		if err != nil {
			t.Errorf("the waiting tar: %v", err)
			close(answered)
			return
		}
		answered <- response
	}()
	select {
	case response := <-answered:
		if response != nil {
			response.Body.Close()
		}
		t.Fatal("a tar past the bound was answered while the stream ahead of it was sent")
	case <-time.After(300 * time.Millisecond):
	}
	if _, health := c.health(t); health.Streams == nil || *health.Streams != 2 {
		t.Errorf("/healthz counts streams %v, want 2: one sent and one waiting", health.Streams)
	}
	unpacked, _ := unpack(t, io.MultiReader(bytes.NewReader(first), ahead.Body))
	indexed(t, unpacked)
	ahead.Body.Close()
	select {
	case response := <-answered:
		if response == nil {
			t.FailNow()
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("the waiting tar was answered %s, want 200 once the stream ahead ended", response.Status)
		}
		next, _ := unpack(t, response.Body)
		if got := git(t, next, "--git-dir="+next, "rev-parse", "refs/heads/main"); got != head {
			t.Errorf("the waiting tar's main is %s, want %s", got, head)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the waiting tar was not answered within 30s of the stream ahead ending")
	}
}

// The next tick's fetch brings what the remote gained, and /healthz says so; every fetch reads the
// token file again, so a token the Secret's projection rotated is the one the next fetch presents.
func TestEachTickFetchesWithTheTokenFileAsItIsThen(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	opts.Refresh = 50 * time.Millisecond
	c := start(t, opts)
	first := c.ready(t)
	if err := os.WriteFile(opts.TokenFile, []byte("ghs_rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := advance(t, remote, "moved")

	health := c.waitFor(t, "at the moved head", func(_ int, h Health) bool { return h.Head == moved })
	if !health.LastFetchAt.After(first.LastFetchAt) {
		t.Errorf("lastFetchAt stayed %s after a fetch that moved the head", health.LastFetchAt)
	}
	tokens := run.seenTokens()
	if tokens[0] != "ghs_first" || tokens[len(tokens)-1] != "ghs_rotated" {
		t.Errorf("the transfers presented %q, want ghs_first first and the rotated ghs_rotated last", tokens)
	}
	for i, transfer := range run.transfers()[1:] {
		want := "--git-dir=" + c.dir() + " fetch"
		if i%2 == 1 {
			want = "--git-dir=" + c.dir() + " ls-remote"
		}
		if transfer != want {
			t.Errorf("a later tick's transfer %d is %q, want %q: each tick fetches into the cache's clone, then reads GitHub's HEAD", i+1, transfer, want)
		}
	}
}

// A fetch that fails is logged, and the cache stays ready on its last good state: the head it had.
func TestAFailedFetchKeepsTheCacheReady(t *testing.T) {
	remote := newRemote(t)
	opts := testOptions(t, newTestRunner(t, remote))
	opts.Refresh = 50 * time.Millisecond
	c := start(t, opts)
	before := c.ready(t)
	if err := os.RemoveAll(remote); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(testTimeout)
	for !logged(c.log.all(), "fetch failed; serving the clone as it was: ") {
		if time.Now().After(deadline) {
			t.Fatalf("no failed fetch was logged: %q", c.log.all())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status, health := c.health(t); status != http.StatusOK || !health.Ready || health.Head != before.Head || health.LastFetchAt != before.LastFetchAt {
		t.Fatalf("/healthz after a failed fetch answers %d %+v, want ready on %+v", status, health, before)
	}
	if status, _ := c.getTar(t); status != http.StatusOK {
		t.Fatalf("/tar after a failed fetch answers %d, want 200", status)
	}
}

func logged(lines []string, prefix string) bool {
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// A cache restarted on a volume that holds its clone fetches into it rather than cloning again, and
// when that first fetch fails it serves the clone as the volume holds it.
func TestARestartOnAVolumeThatHoldsTheCloneFetchesIntoIt(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	first := start(t, opts)
	first.ready(t)
	first.cancel()
	if err := <-first.done; err != nil {
		t.Fatalf("the first Serve ended with %v", err)
	}
	first.done <- nil

	moved := advance(t, remote, "moved")
	again := start(t, opts)
	if health := again.ready(t); health.Head != moved {
		t.Errorf("the restarted cache serves %s, want what its fetch brought, %s", health.Head, moved)
	}
	if transfers := run.transfers(); !slices.Equal(transfers, []string{"clone --bare", "--git-dir=" + again.dir() + " fetch", "--git-dir=" + again.dir() + " ls-remote"}) {
		t.Errorf("the cache ran %q, want a clone, then the restart's fetch and its read of GitHub's HEAD", transfers)
	}
	again.cancel()
	if err := <-again.done; err != nil {
		t.Fatalf("the second Serve ended with %v", err)
	}
	again.done <- nil

	if err := os.RemoveAll(remote); err != nil {
		t.Fatal(err)
	}
	offline := start(t, opts)
	if health := offline.ready(t); health.Head != moved || !health.LastFetchAt.IsZero() {
		t.Errorf("the cache restarted with GitHub unreachable serves %+v, want the volume's %s and no fetch time", health, moved)
	}
	if !logged(offline.log.all(), "fetch failed; serving the clone as it was: ") {
		t.Errorf("the failed first fetch was not logged: %q", offline.log.all())
	}
}

// A cache killed mid-fetch leaves git's lock files in its clone. Restarted on that volume, it is
// the clone's only writer, so its first fetch removes them and brings what the remote gained.
func TestACacheRestartedAfterAnInterruptedFetchClearsItsLocks(t *testing.T) {
	remote := newRemote(t)
	opts := testOptions(t, newTestRunner(t, remote))
	first := start(t, opts)
	first.ready(t)
	first.cancel()
	if err := <-first.done; err != nil {
		t.Fatalf("the first Serve ended with %v", err)
	}
	first.done <- nil
	for _, lock := range []string{"refs/heads/main.lock", "packed-refs.lock"} {
		if err := os.WriteFile(filepath.Join(first.dir(), lock), []byte("interrupted\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	moved := advance(t, remote, "moved")

	again := start(t, opts)
	if health := again.ready(t); health.Head != moved || health.LastFetchAt.IsZero() {
		t.Fatalf("the restarted cache serves %+v, want what its fetch brought, %s", health, moved)
	}
	if !logged(again.log.all(), "removed the stale git lock "+filepath.Join(again.dir(), "refs/heads/main.lock")) {
		t.Errorf("the restart did not log the lock it removed: %q", again.log.all())
	}
}

// GitHub renaming the repository's default branch moves the cache's HEAD with it at the next fetch.
// The fetch's prune removes the old branch, so a HEAD left naming it would name nothing: every
// publish would fail, and a restart would end Serve, the cache never ready again. Restarted on its
// volume after the rename, the cache is ready at the renamed branch's head, its HEAD names that
// branch, and the old branch is gone.
func TestACacheFollowsARenamedDefaultBranch(t *testing.T) {
	remote := newRemote(t)
	opts := testOptions(t, newTestRunner(t, remote))
	first := start(t, opts)
	first.ready(t)
	first.cancel()
	if err := <-first.done; err != nil {
		t.Fatalf("the first Serve ended with %v", err)
	}
	first.done <- nil
	moved := advance(t, remote, "moved")
	git(t, remote, "branch", "-m", "main", "trunk")

	again := start(t, opts)
	if health := again.ready(t); health.Head != moved {
		t.Fatalf("the restarted cache serves %+v, want the renamed branch's head %s", health, moved)
	}
	head, err := os.ReadFile(filepath.Join(again.dir(), "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	if string(head) != "ref: refs/heads/trunk\n" {
		t.Errorf("the cache's HEAD is %q, want the renamed branch", head)
	}
	if refs := git(t, again.dir(), "for-each-ref", "--format=%(refname)", "refs/heads"); refs != "refs/heads/trunk" {
		t.Errorf("the cache's branches are %q, want trunk alone", refs)
	}
}

// A cache whose volume holds no clone and whose first clone fails has nothing to serve: Serve ends
// with the clone's failure, so the container exits and the kubelet restarts it.
func TestServeEndsWhenTheFirstCloneFails(t *testing.T) {
	remote := newRemote(t)
	opts := testOptions(t, newTestRunner(t, remote))
	if err := os.RemoveAll(remote); err != nil {
		t.Fatal(err)
	}
	opts.Log = func(string) {}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := Serve(ctx, opts); err == nil || !strings.Contains(err.Error(), "git -c gc.auto=0") {
		t.Fatalf("Serve = %v, want the clone's failure", err)
	}
}

// Once a repack is due, the fetch that finds it so is followed by a repack of the clone into one
// pack with no loose object; until then a fetch leaves what it brought as git stored it.
func TestAFetchRepacksTheCloneOnceARepackIsDue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		every  time.Duration
		repack bool
	}{
		{"not due", 24 * time.Hour, false},
		{"due", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := repackEvery
			repackEvery = tc.every
			t.Cleanup(func() { repackEvery = saved })
			remote := newRemote(t)
			opts := testOptions(t, newTestRunner(t, remote))
			opts.Refresh = 50 * time.Millisecond
			c := start(t, opts)
			c.ready(t)
			moved := advance(t, remote, "moved")
			c.waitFor(t, "at the moved head", func(_ int, h Health) bool { return h.Head == moved })
			packed := func() (bool, string) {
				counts := map[string]string{}
				for _, line := range strings.Split(git(t, c.dir(), "--git-dir="+c.dir(), "count-objects", "-v"), "\n") {
					if key, value, ok := strings.Cut(line, ": "); ok {
						counts[key] = value
					}
				}
				return counts["count"] == "0" && counts["packs"] == "1", "count=" + counts["count"] + " packs=" + counts["packs"]
			}
			// The repack follows the fetch that brought the moved head, so a due one is waited for.
			deadline := time.Now().Add(testTimeout)
			got, held := packed()
			for tc.repack && !got && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
				got, held = packed()
			}
			if got != tc.repack {
				t.Errorf("the clone holds %s, want repacked %t", held, tc.repack)
			}
		})
	}
}

// Serve refuses options it cannot run on before it listens or touches the volume.
func TestServeRefusesIncompleteOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Options)
		want string
	}{
		{"no repository", func(o *Options) { o.Repo = ghrepo.Repository{} }, "repository cache repository is required"},
		{"a relative root", func(o *Options) { o.Root = "cache" }, `repository cache root must be an absolute path (got "cache")`},
		{"no token file", func(o *Options) { o.TokenFile = "" }, "repository cache token file is required"},
		{"no bearer token file", func(o *Options) { o.CacheTokenFile = "" }, "repository cache bearer token file is required"},
		{"a blank bearer token file", func(o *Options) {
			if err := os.WriteFile(o.CacheTokenFile, []byte(" \n"), 0o600); err != nil {
				panic(err)
			}
		}, "LEGION_REPOSITORY_CACHE_TOKEN_FILE names "},
		{"no credential directory", func(o *Options) { o.CredentialDir = "" }, "repository cache credential directory is required"},
		{"no refresh", func(o *Options) { o.Refresh = 0 }, "repository cache refresh must be positive (got 0s)"},
		{"no listen address", func(o *Options) { o.Listen = "" }, "repository cache listen address is required"},
		{"no log", func(o *Options) { o.Log = nil }, "repository cache log is required"},
		{"no runner", func(o *Options) { o.Run = nil }, "repository cache runner is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newTestRunner(t, newRemote(t))
			opts := testOptions(t, run)
			opts.Log = func(string) {}
			tc.edit(&opts)
			if err := Serve(context.Background(), opts); err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("Serve = %v, want %q", err, tc.want)
			}
			if transfers := run.transfers(); len(transfers) != 0 {
				t.Errorf("Serve ran %q before refusing", transfers)
			}
			if entries, err := os.ReadDir(opts.Root); tc.name != "a relative root" && (err != nil || len(entries) != 0) {
				t.Errorf("Serve wrote %v (%v) into its root before refusing", entries, err)
			}
		})
	}
}

// What an issue pod's fetch container does with a running cache: workspace.Fetch, given the
// cache's address, unpacks its tar into a feed every entry of which it accepts, then fetches from
// GitHub only what the remote gained since the cache's last fetch, and clones nothing.
func TestAnIssuePodsFetchFillsItsFeedFromTheCache(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	c := start(t, testOptions(t, run))
	c.ready(t)
	moved := advance(t, remote, "moved")
	var logged []string
	report, err := workspace.Fetch(context.Background(), run, workspace.FetchRequest{
		Repo: ghrepo.MustParse("acme/widgets"), Token: func() (string, error) { return "ghs_pod", nil }, CredentialDir: t.TempDir(), Feed: filepath.Join(t.TempDir(), "feed"),
		Log: func(line string) { logged = append(logged, line) }, Clock: workspace.WallClock{}, CacheURL: c.url, CacheToken: testCacheToken,
	})
	if err != nil {
		t.Fatalf("Fetch from the cache: %v", err)
	}
	if report.Cloned || report.CacheBytes <= 0 || report.Refs != 1 || len(logged) != 0 {
		t.Fatalf("Fetch reported %+v and logged %q, want the cache's tar then one ref from GitHub", report, logged)
	}
	if got := git(t, report.Feed, "--git-dir="+report.Feed, "rev-parse", "refs/heads/main"); got != moved {
		t.Errorf("the feed's main is %s, want the remote's %s", got, moved)
	}
	git(t, report.Feed, "--git-dir="+report.Feed, "fsck", "--connectivity-only")
	if transfers := run.transfers(); !slices.ContainsFunc(transfers, func(transfer string) bool {
		return strings.HasPrefix(transfer, "--git-dir="+report.Feed+".clone-") && strings.HasSuffix(transfer, " fetch")
	}) {
		t.Errorf("the pod's transfers were %q, wanted a fetch of the delta into the feed it filled", transfers)
	}
	for _, transfer := range run.transfers()[1:] {
		if strings.HasPrefix(transfer, "clone ") {
			t.Errorf("the pod cloned from GitHub: %q", transfer)
		}
	}
}

// /tar answers only a request carrying the cache's own token: none, or another, is 401, and says
// nothing of the repository. /healthz, its health, which carries no secret and nothing of the
// repository's content, answers any request, a token or none, the same way: 200 and the health,
// naming the repository in workspace.CacheRepositoryHeader. /readyz, the kubelet's readiness
// probe, answers 503 then 200 with nothing but its status.
func TestOnlyTheTarNeedsTheCachesToken(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	started, release := run.hold()
	defer release()
	c := start(t, testOptions(t, run))
	<-started
	if response := c.get(t, "/readyz", ""); response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/readyz during the first clone answers %d, want 503", response.StatusCode)
	}
	release()
	c.ready(t)
	for _, bearer := range []string{"", "cache-token-of-acme-gadgets"} {
		response := c.get(t, "/tar", bearer)
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized || response.Header.Get(workspace.CacheRepositoryHeader) != "" || strings.Contains(string(body), "acme/widgets") {
			t.Errorf("/tar with bearer %q answers %d %q naming %q, want 401 naming nothing", bearer, response.StatusCode, body, response.Header.Get(workspace.CacheRepositoryHeader))
		}
	}
	if response := c.get(t, "/tar", testCacheToken); response.StatusCode != http.StatusOK || response.Header.Get(workspace.CacheRepositoryHeader) != "acme/widgets" {
		t.Errorf("/tar with its token answers %d naming %q, want 200 naming acme/widgets", response.StatusCode, response.Header.Get(workspace.CacheRepositoryHeader))
	} else {
		response.Body.Close()
	}
	for _, bearer := range []string{"", "cache-token-of-acme-gadgets", testCacheToken} {
		response := c.get(t, "/healthz", bearer)
		var health Health
		err := json.NewDecoder(response.Body).Decode(&health)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || response.Header.Get(workspace.CacheRepositoryHeader) != "acme/widgets" || err != nil || !health.Ready || health.Repo != "acme/widgets" {
			t.Errorf("/healthz with bearer %q answers %d naming %q: %+v (%v), want 200 and the health naming acme/widgets", bearer, response.StatusCode, response.Header.Get(workspace.CacheRepositoryHeader), health, err)
		}
	}
	response := c.get(t, "/readyz", "")
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(body) != 0 || response.Header.Get(workspace.CacheRepositoryHeader) != "" {
		t.Errorf("/readyz once ready answers %d %q naming %q, want 200 and nothing else", response.StatusCode, body, response.Header.Get(workspace.CacheRepositoryHeader))
	}
}

// Two caches of two repositories, each with its own token: an issue pod of acme/widgets that is
// handed acme/gadgets's cache — a cache pod replaced and its address reused — takes nothing from
// it, whichever token it holds. The cache's health already names another repository, so the fetch
// is refused before it asks for the tar or presents a token: no ref, tag or object of acme/gadgets
// reaches its feed. Its own cache fills the feed.
func TestAnIssuePodTakesNothingFromAnotherRepositorysCache(t *testing.T) {
	widgets, gadgets := newRemote(t), newRemote(t)
	tagged := advance(t, gadgets, "gadgets-only")
	git(t, gadgets, "--git-dir="+gadgets, "tag", "gadgets-only", tagged)
	run := newTestRunner(t, widgets)
	run.serves("acme/gadgets", gadgets)
	ownCache := start(t, testOptions(t, run))
	otherCache := start(t, repoOptions(t, run, "acme/gadgets", "cache-token-of-acme-gadgets"))
	ownCache.ready(t)
	otherCache.ready(t)
	fetch := func(cache *running, bearer string) (workspace.FetchReport, string, error) {
		request := workspace.FetchRequest{
			Repo: ghrepo.MustParse("acme/widgets"), Token: func() (string, error) { return "ghs_pod", nil }, CredentialDir: t.TempDir(), Feed: filepath.Join(t.TempDir(), "feed"),
			Log: func(string) {}, Clock: workspace.WallClock{}, CacheURL: cache.url, CacheToken: bearer,
		}
		report, err := workspace.Fetch(context.Background(), run, request)
		return report, request.Feed, err
	}
	for _, tc := range []struct {
		bearer, says string
	}{
		{testCacheToken, "is the repository cache of acme/gadgets, not of acme/widgets"},
		{"cache-token-of-acme-gadgets", "is the repository cache of acme/gadgets, not of acme/widgets"},
	} {
		_, feed, err := fetch(otherCache, tc.bearer)
		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Fatalf("Fetch of acme/widgets from acme/gadgets's cache with %q = %v, want a refusal naming %q", tc.bearer, err, tc.says)
		}
		if entries, err := os.ReadDir(filepath.Join(feed, "acme")); err == nil && len(entries) != 0 {
			t.Errorf("the refused fetch left %d entries in the feed", len(entries))
		}
	}
	report, _, err := fetch(ownCache, testCacheToken)
	if err != nil {
		t.Fatalf("Fetch from its own cache: %v", err)
	}
	if tags := git(t, report.Feed, "--git-dir="+report.Feed, "tag", "--list"); tags != "" {
		t.Errorf("the feed holds tags %q", tags)
	}
	if err := exec.Command("git", "--git-dir="+report.Feed, "cat-file", "-e", tagged).Run(); err == nil {
		t.Errorf("the feed holds acme/gadgets's commit %s", tagged)
	}
}

// A due repack runs outside the lock a tar takes: a tar asked for while the repack runs is served
// at once, and the repack then completes.
func TestATarIsServedWhileARepackRuns(t *testing.T) {
	saved := repackEvery
	repackEvery = 0
	t.Cleanup(func() { repackEvery = saved })
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	opts.Refresh = 50 * time.Millisecond
	c := start(t, opts)
	c.ready(t)
	started, release := make(chan struct{}), make(chan struct{})
	var bound time.Duration
	var argv []string
	run.instead("repack", func(ctx context.Context, command workspace.Command) (workspace.Result, error) {
		bound, argv = command.Timeout, command.Argv
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return workspace.Result{}, ctx.Err()
		}
		return run.runner.Run(ctx, command)
	})
	advance(t, remote, "moved")
	select {
	case <-started:
	case <-time.After(testTimeout):
		t.Fatal("no repack started")
	}
	request, err := http.NewRequest(http.MethodGet, c.url+"/tar", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+c.bearer(t))
	served := make(chan int, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			served <- 0
			return
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		served <- response.StatusCode
	}()
	select {
	case status := <-served:
		if status != http.StatusOK {
			t.Errorf("the tar asked for during the repack answered %d, want 200", status)
		}
	case <-time.After(5 * time.Second):
		t.Error("the tar waited on the repack")
	}
	if bound != repackTimeout {
		t.Errorf("the repack ran within %s, want repackTimeout %s, not the runner's own budget", bound, repackTimeout)
	}
	for _, setting := range []string{"pack.windowMemory=256m", "core.packedGitLimit=512m", "pack.threads=1"} {
		if !slices.Contains(argv, setting) {
			t.Errorf("the repack ran %q, want its memory bounded by %s", argv, setting)
		}
	}
	close(release)
	deadline := time.Now().Add(testTimeout)
	for !logged(c.log.all(), "repacked ") {
		if time.Now().After(deadline) {
			t.Fatalf("the repack never completed: %q", c.log.all())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A repack that fails is logged, and the partial packs it left are removed at once, each logged,
// so a repack that keeps failing never fills the cache's volume; the cache stays ready.
func TestAFailedRepackLeavesNoPartialPack(t *testing.T) {
	saved := repackEvery
	repackEvery = 0
	t.Cleanup(func() { repackEvery = saved })
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	opts.Refresh = 50 * time.Millisecond
	c := start(t, opts)
	c.ready(t)
	pack := filepath.Join(c.dir(), "objects", "pack")
	partial := filepath.Join(pack, "tmp_pack_Ab12Cd")
	others := []string{filepath.Join(pack, "tmp_rev_Ef34Gh"), filepath.Join(pack, ".tmp-123-pack-0a1b.pack")}
	kept, err := filepath.Glob(filepath.Join(pack, "pack-*"))
	if err != nil || len(kept) == 0 {
		t.Fatalf("the clone holds no pack to keep: %v", err)
	}
	keep := strings.TrimSuffix(kept[0], filepath.Ext(kept[0])) + ".keep"
	if err := os.WriteFile(keep, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	kept = append(kept, keep)
	run.instead("repack", func(context.Context, workspace.Command) (workspace.Result, error) {
		for _, path := range append([]string{partial}, others...) {
			if err := os.WriteFile(path, []byte("PACK half a pack\n"), 0o444); err != nil {
				return workspace.Result{}, err
			}
		}
		return workspace.Result{ExitCode: 128, Stderr: "fatal: killed\n"}, nil
	})
	advance(t, remote, "moved")
	deadline := time.Now().Add(testTimeout)
	for !logged(c.log.all(), "removed the partial pack tmp_pack_Ab12Cd, which a killed git left") {
		if time.Now().After(deadline) {
			t.Fatalf("the partial pack's removal was never logged: %q", c.log.all())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !logged(c.log.all(), "repack failed; tried again in ") {
		t.Errorf("the failed repack was not logged: %q", c.log.all())
	}
	for _, path := range append([]string{partial}, others...) {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the partial file %s is still there (%v)", filepath.Base(path), err)
		}
	}
	for _, path := range kept {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the removal took %s, which is no partial file: %v", filepath.Base(path), err)
		}
	}
	c.ready(t)
}

// A cache restarted on a volume where a killed repack left partial packs removes them before its
// first fetch, each logged.
func TestARestartedCacheRemovesThePartialPacksAKilledRepackLeft(t *testing.T) {
	remote := newRemote(t)
	opts := testOptions(t, newTestRunner(t, remote))
	first := start(t, opts)
	first.ready(t)
	first.cancel()
	if err := <-first.done; err != nil {
		t.Fatalf("the first Serve ended with %v", err)
	}
	first.done <- nil
	partial := filepath.Join(first.dir(), "objects", "pack", "tmp_pack_Zz99Yy")
	if err := os.WriteFile(partial, []byte("PACK half a pack\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	again := start(t, opts)
	again.ready(t)
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the partial pack outlived the restart (%v)", err)
	}
	if !logged(again.log.all(), "removed the partial pack tmp_pack_Zz99Yy, which a killed git left") {
		t.Errorf("the removal was not logged: %q", again.log.all())
	}
}

// The cache's own first clone gets the repository-sized bound, workspace.CacheFetchTimeout, not an
// issue pod's FetchTimeout, and a fetch into the clone keeps FetchTimeout.
func TestTheCachesCloneTakesTheRepositorysBound(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	bound := make(chan time.Duration, 1)
	run.instead("clone", func(ctx context.Context, command workspace.Command) (workspace.Result, error) {
		bound <- command.Timeout
		return run.runner.Run(ctx, command)
	})
	opts := testOptions(t, run)
	opts.Refresh = 50 * time.Millisecond
	c := start(t, opts)
	c.ready(t)
	select {
	case got := <-bound:
		if got <= workspace.FetchTimeout || got > workspace.CacheFetchTimeout {
			t.Errorf("the cache's clone ran within %s, want past FetchTimeout %s and within CacheFetchTimeout %s", got, workspace.FetchTimeout, workspace.CacheFetchTimeout)
		}
	default:
		t.Fatal("the cache cloned with no command the runner saw")
	}
	refreshed := make(chan time.Duration, 1)
	run.instead("fetch", func(ctx context.Context, command workspace.Command) (workspace.Result, error) {
		refreshed <- command.Timeout
		return run.runner.Run(ctx, command)
	})
	advance(t, remote, "moved")
	select {
	case got := <-refreshed:
		if got > workspace.FetchTimeout {
			t.Errorf("a fetch into the clone ran within %s, want FetchTimeout %s at most: it holds the lock every tar waits on", got, workspace.FetchTimeout)
		}
	case <-time.After(testTimeout):
		t.Fatal("the cache never fetched into its clone")
	}
}

// A snapshot taken while a repack swaps the clone's packs is taken again, and the tar it gives
// holds exactly the packs the clone holds then.
func TestASnapshotTakesThePacksARepackLeft(t *testing.T) {
	remote := newRemote(t)
	clone := filepath.Join(t.TempDir(), "widgets.git")
	git(t, filepath.Dir(clone), "clone", "--bare", "--quiet", remote, clone)
	git(t, clone, "--git-dir="+clone, "config", "fetch.unpackLimit", "1")
	advance(t, remote, "second pack")
	git(t, clone, "--git-dir="+clone, "fetch", "--quiet", remote, "+refs/heads/*:refs/heads/*")
	c := newCache(Options{Log: func(string) {}}, clone)
	calls := 0
	snapshotWalked = func() {
		calls++
		if calls == 1 {
			git(t, clone, "--git-dir="+clone, "repack", "-a", "-d", "--quiet")
		}
	}
	t.Cleanup(func() { snapshotWalked = nil })
	entries, err := c.snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer closeEntries(entries)
	if calls != 2 {
		t.Errorf("the snapshot was taken %d times, want twice", calls)
	}
	var taken []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.header.Name, "objects/pack/pack-") {
			taken = append(taken, filepath.Base(entry.header.Name))
		}
	}
	var held []string
	for _, entry := range mustReadDir(t, filepath.Join(clone, "objects", "pack")) {
		if workspace.FeedEntry("objects/pack/"+entry, false) {
			held = append(held, entry)
		}
	}
	slices.Sort(taken)
	slices.Sort(held)
	if len(taken) == 0 || !slices.Equal(taken, held) {
		t.Errorf("the snapshot took %q, want the clone's %q", taken, held)
	}
}

// mustReadDir is the names in dir.
func mustReadDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// cacheOn is a cache of opts that Serve has not started, for a test that drives refresh itself.
func cacheOn(t *testing.T, opts Options) *cache {
	t.Helper()
	dir, err := workspace.FeedRepository(opts.Root, opts.Repo)
	if err != nil {
		t.Fatal(err)
	}
	c := newCache(opts, dir)
	c.health.Store(&Health{Repo: opts.Repo.String()})
	return c
}

// repacks is how many repacks the runner was asked to run.
func (r *testRunner) repacks() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, argv := range r.argv {
		if slices.Contains(argv, "repack") {
			n++
		}
	}
	return n
}

// A repack that fails is not tried at the next fetch, only a repackEvery after it: one that cannot
// fit never holds the cache from fetch to fetch.
func TestAFailedRepackIsTriedAgainOnlyARepackEveryLater(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	log := &syncLog{}
	opts.Log = log.log
	c := cacheOn(t, opts)
	ctx := context.Background()
	if err := c.refresh(ctx); err != nil {
		t.Fatalf("the first refresh: %v", err)
	}
	c.lastRepack = time.Now().Add(-repackEvery - time.Hour)
	old := c.lastRepack.UTC().Format(time.RFC3339)
	if err := os.WriteFile(filepath.Join(opts.Root, lastRepackFile), []byte(old+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var held time.Time
	run.instead("repack", func(context.Context, workspace.Command) (workspace.Result, error) {
		held = recorded(t, opts.Root)
		return workspace.Result{ExitCode: 128, Stderr: "fatal: killed\n"}, nil
	})
	for i := range 2 {
		advance(t, remote, fmt.Sprintf("moved-%d", i))
		if err := c.refresh(ctx); err != nil {
			t.Fatalf("a refresh: %v", err)
		}
	}
	if got := run.repacks(); got != 1 {
		t.Errorf("the runner was asked for %d repacks, want the one that failed and none at the next fetch", got)
	}
	if held.Before(time.Now().Add(-time.Hour)) {
		t.Errorf("the record held %s while the repack ran, want the attempt's own time written before it: a repack that kills its pod must not run again at the next start", held)
	}
	if err := cacheOn(t, opts).refresh(ctx); err != nil {
		t.Fatalf("a new process's refresh: %v", err)
	}
	if got := run.repacks(); got != 1 {
		t.Errorf("a new process after the failed repack asked for %d repacks in all, want none of its own", got)
	}
	if !logged(log.all(), "repack failed; tried again in "+repackEvery.String()+": ") {
		t.Errorf("the failed repack was not logged: %q", log.all())
	}
}

// A fetch that fails has the partial files its git left in objects/pack removed, each logged.
func TestAFailedFetchLeavesNoPartialPack(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	log := &syncLog{}
	opts.Log = log.log
	c := cacheOn(t, opts)
	ctx := context.Background()
	if err := c.refresh(ctx); err != nil {
		t.Fatalf("the first refresh: %v", err)
	}
	partial := filepath.Join(c.dir, "objects", "pack", "tmp_pack_Fe7ch0")
	run.instead("fetch", func(context.Context, workspace.Command) (workspace.Result, error) {
		if err := os.WriteFile(partial, []byte("PACK half a pack\n"), 0o444); err != nil {
			return workspace.Result{}, err
		}
		return workspace.Result{ExitCode: 128, Stderr: "fatal: write error: No space left on device\n"}, nil
	})
	advance(t, remote, "moved")
	if err := c.refresh(ctx); err == nil {
		t.Fatal("a refresh whose fetch failed answered nil")
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the failed fetch's partial pack is still there (%v)", err)
	}
	if !logged(log.all(), "removed the partial pack tmp_pack_Fe7ch0, which a killed git left") {
		t.Errorf("the removal was not logged: %q", log.all())
	}
}

// The time of the last repack outlives the process: a cache restarted after a repack is not due
// again until a repackEvery after it, and one restarted a repackEvery after it repacks at once.
func TestTheLastRepackOutlivesTheProcess(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	opts.Log = func(string) {}
	ctx := context.Background()
	before := time.Now().Add(-time.Second)
	if err := cacheOn(t, opts).refresh(ctx); err != nil {
		t.Fatalf("the first process's refresh: %v", err)
	}
	if at := recorded(t, opts.Root); at.Before(before.Truncate(time.Second)) || at.After(time.Now()) {
		t.Fatalf("the first process recorded %s, want its first fetch's time, after %s", at, before)
	}
	if err := cacheOn(t, opts).refresh(ctx); err != nil {
		t.Fatalf("the second process's refresh: %v", err)
	}
	if got := run.repacks(); got != 0 {
		t.Fatalf("a process restarted inside repackEvery repacked %d times", got)
	}
	old := time.Now().Add(-repackEvery - time.Hour).UTC().Format(time.RFC3339)
	if err := os.WriteFile(filepath.Join(opts.Root, lastRepackFile), []byte(old+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cacheOn(t, opts).refresh(ctx); err != nil {
		t.Fatalf("the third process's refresh: %v", err)
	}
	if got := run.repacks(); got != 1 {
		t.Errorf("a process restarted a repackEvery after the last repack repacked %d times, want 1", got)
	}
}

// recorded is the time the cache's last-repack record under root holds.
func recorded(t *testing.T, root string) time.Time {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, lastRepackFile))
	if err != nil {
		t.Fatalf("read the last-repack record: %v", err)
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatalf("the last-repack record holds %q: %v", body, err)
	}
	return at
}
