package workspace

import (
	"archive/tar"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// isolatedGitConfig is the configuration Fetch's git reads besides its own: none. Not the system's,
// not the image user's; and git's `-c`, GIT_CONFIG_PARAMETERS, no process provisioning starts
// inherits (config.go).
var isolatedGitConfig = []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}

// FetchRequest is a pod's first init container's: the repository, how to read the provisioning
// token (Token), the directory its one-shot credential goes under, the feed directory, the log each
// retried transfer and each wait on the cache are told to, the clock the retries and that wait read
// and wait on, and the repository cache's address and the bearer token it takes (an issue pod's,
// under the Kubernetes runtime), both empty for the cache's own process, which fills its clone from
// GitHub.
type FetchRequest struct {
	Repo ghrepo.Repository
	// Token answers the provisioning token as it stands when called, never a copy kept from an
	// earlier call: the file a pod mounts it at, which the daemon keeps fresh while the fetch runs.
	Token         func() (string, error)
	CredentialDir string
	Feed          string
	Log           func(line string)
	Clock         Clock
	CacheURL      string
	CacheToken    string
}

// CacheRepositoryHeader names, on every answer the repository cache gives a request its token
// authorizes, the repository it caches (owner/name), which Fetch checks is its own before it takes
// anything the answer carries.
const CacheRepositoryHeader = "Legion-Repository"

// FetchReport is where Fetch filled the feed from and what each source brought: the feed
// repository; whether it was cloned whole from GitHub; the repository cache's tar, its length, how
// long it streamed and how long its history took to verify, when the feed was unpacked from it;
// and how much the feed's objects grew across the transfer from GitHub, with how many refs that
// transfer made, moved or deleted.
type FetchReport struct {
	Feed           string
	Cloned         bool
	CacheBytes     int64
	CacheDuration  time.Duration
	VerifyDuration time.Duration
	GitHubBytes    int64
	Refs           int
}

// Fetch fills the feed with the provisioning token and no configuration but the one-shot
// credential's and the pins: the process of a pod's first init container, the only one that holds
// the token, whose feed is its own and which mounts nothing a tree agent can write, and the
// repository cache's process (repocache), whose volume is its own. It reaches GitHub as the tmux
// runtime's Source does, so the token's holders share one credential rule.
//
// A feed that holds the repository already — the issue volume's feed on a relaunch, the cache's
// clone at each refresh — has every branch fetched into it from GitHub, the ones GitHub deleted
// pruned, once the locks a killed fetch left are gone (RemoveStaleFeedLocks). A feed that does not
// is filled in a temporary sibling, its garbage collection turned off (feedConfiguration), and
// renamed into place once all of that succeeded, so the feed's repository is either whole or
// absent. With a cache named, it comes from the cache alone: Fetch waits for the cache to be ready
// (waitForCache), unpacks its tar (unpackCache), then fetches from GitHub what the cache lacks; a
// cache that is not ready within CacheWaitTimeout, or whose tar a feed does not take, fails the
// fetch, and nothing is cloned from GitHub instead, so the cache's own first clone is the one full
// download of the repository. With none, the feed is cloned bare from GitHub: what the cache's own
// process does. Provision then clones the shared clone from the feed with no credential.
//
// What the fetch does once it may start — at once, or once the cache is ready — shares one
// FetchTimeout: the cache's stream, its verification and the delta from GitHub, or a clone or fetch
// from GitHub, each bounded by what remains of it, and none started past it. So the fetch's wait,
// transfers and checks end within FetchBudget, however long it waited on the cache; the few local
// git commands around them (reading refs, writing the feed's configuration) keep CommandTimeout
// each and take moments. Every attempt of a transfer from GitHub reads
// the provisioning token anew as it starts (FetchRequest.Token, fetchCredential.renew), so it
// presents the token as its file stands then, not as it stood when the fetch began. A transfer
// from GitHub that fails transiently (transientCloneFailure) is run again after the next of
// fetchBackoff's waits, once per wait, its failure logged and whatever a failed clone left removed
// first, and no retry starts past the bound. A transfer still failing after the last wait, and
// every other failure, is the answer at once. Each attempt's stderr has every token the fetch has
// read replaced by *** as soon as the attempt returns, so nothing Fetch logs or returns carries
// one, wherever git echoed it.
func Fetch(ctx context.Context, run Runner, request FetchRequest) (FetchReport, error) {
	feed, err := FeedRepository(request.Feed, request.Repo)
	if err != nil {
		return FetchReport{}, err
	}
	if request.Token == nil {
		return FetchReport{}, errors.New("workspace fetch token is required")
	}
	token, err := request.Token()
	if err != nil {
		return FetchReport{}, err
	}
	github := gitHubSource{token: token, credentialDir: request.CredentialDir}
	if err := github.check(request.Repo); err != nil {
		return FetchReport{}, err
	}
	if request.Log == nil {
		return FetchReport{}, errors.New("workspace fetch log is required")
	}
	if request.Clock == nil {
		return FetchReport{}, errors.New("workspace fetch clock is required")
	}
	if request.CacheURL != "" && request.CacheToken == "" {
		return FetchReport{}, errors.New("workspace fetch: a repository cache needs its token")
	}
	credential, err := github.open(request.Repo, "", "")
	if err != nil {
		return FetchReport{}, err
	}
	defer func() {
		_ = credential.remove()
	}()
	present, err := ownFeedRepository(feed, request.Repo, request.Clock.Now(), request.Log)
	if err != nil {
		return FetchReport{}, err
	}
	if err := os.MkdirAll(filepath.Dir(feed), 0o700); err != nil {
		return FetchReport{}, fmt.Errorf("create feed parent: %w", err)
	}
	held := &fetchCredential{remote: credential, env: merge(credential.env, isolatedGitConfig), read: request.Token, held: []string{token}}
	report := FetchReport{Feed: feed}
	if present {
		deadline := request.Clock.Now().Add(FetchTimeout)
		if err := report.fromGitHub(ctx, run, request, fetchInto(feed, request.Repo), feed, held, deadline); err != nil {
			return FetchReport{}, err
		}
		return report, credential.remove()
	}

	staging, err := stageBeside(feed, request.Log)
	if err != nil {
		return FetchReport{}, err
	}
	defer func() {
		_ = os.RemoveAll(staging)
	}()
	transfer := feedTransfer{verb: "clone", argv: []string{"git", "clone", "--bare", "--quiet", GitHubURL(request.Repo), staging}, destination: staging}
	if request.CacheURL != "" {
		if err := waitForCache(ctx, request); err != nil {
			return FetchReport{}, err
		}
	}
	deadline := request.Clock.Now().Add(FetchTimeout)
	if request.CacheURL != "" {
		started := request.Clock.Now()
		streamed, err := unpackCache(ctx, run, request, staging, deadline)
		verifying := request.Clock.Now()
		if err == nil {
			err = verifyCache(ctx, run, request, staging, deadline)
		}
		if err != nil {
			return FetchReport{}, fmt.Errorf("fill the feed from the repository cache %s: %w", request.CacheURL, err)
		}
		report.CacheBytes, report.CacheDuration, report.VerifyDuration = streamed, verifying.Sub(started), request.Clock.Now().Sub(verifying)
		transfer = fetchInto(staging, request.Repo)
	}
	report.Cloned = transfer.verb == "clone"
	if err := report.fromGitHub(ctx, run, request, transfer, staging, held, deadline); err != nil {
		return FetchReport{}, err
	}
	if err := feedConfiguration(ctx, run, staging); err != nil {
		return FetchReport{}, err
	}
	if err := os.Rename(staging, feed); err != nil {
		return FetchReport{}, fmt.Errorf("move the fetched repository into the feed at %s: %w", feed, err)
	}
	return report, credential.remove()
}

// fromGitHub runs transfer into the bare repository gitDir under the one-shot credential held,
// with Fetch's retries, all before deadline, and records how much the repository's objects grew
// across it and how many refs it made, moved or deleted. A clone's gitDir holds no repository
// before it runs: nothing, and no ref.
func (r *FetchReport) fromGitHub(ctx context.Context, run Runner, request FetchRequest, transfer feedTransfer, gitDir string, held *fetchCredential, deadline time.Time) error {
	refsBefore, err := feedRefs(ctx, run, gitDir, transfer.verb == "fetch")
	if err != nil {
		return err
	}
	sizeBefore, err := objectsSize(gitDir)
	if err != nil {
		return err
	}
	if _, err := transferWithRetries(ctx, run, request, transfer, held, deadline); err != nil {
		return err
	}
	refsAfter, err := feedRefs(ctx, run, gitDir, true)
	if err != nil {
		return err
	}
	if transfer.verb == "fetch" {
		if err := followHead(ctx, run, request, gitDir, refsAfter, held, deadline); err != nil {
			return err
		}
	}
	sizeAfter, err := objectsSize(gitDir)
	if err != nil {
		return err
	}
	r.GitHubBytes = sizeAfter - sizeBefore
	for ref, id := range refsAfter {
		if refsBefore[ref] != id {
			r.Refs++
		}
	}
	for ref := range refsBefore {
		if _, kept := refsAfter[ref]; !kept {
			r.Refs++
		}
	}
	return nil
}

// followHead points the bare repository gitDir's HEAD at the branch GitHub's HEAD names now
// (`git ls-remote --symref`), under the fetch's credential, retries and deadline, and logs the
// move when there is one. A fetch never moves HEAD and prunes the branches GitHub deleted, so a
// repository whose default branch GitHub renamed would keep a HEAD naming a branch that is gone:
// the cache could publish no head, and an issue's clone would check nothing out. HEAD moves only to
// a branch among fetched, the refs the fetch left: GitHub renaming its branch between the fetch and
// this read names one the fetch did not bring, so HEAD stays, logged, until a fetch brings it. A
// clone takes GitHub's HEAD itself. A repository GitHub answers with no default branch (nothing
// pushed yet) keeps its HEAD.
func followHead(ctx context.Context, run Runner, request FetchRequest, gitDir string, fetched map[string]string, held *fetchCredential, deadline time.Time) error {
	read := feedTransfer{verb: "ls-remote", argv: []string{"git", "--git-dir=" + gitDir, "ls-remote", "--symref", GitHubURL(request.Repo), "HEAD"}}
	result, err := transferWithRetries(ctx, run, request, read, held, deadline)
	if err != nil {
		return fmt.Errorf("read GitHub's default branch of %s: %w", request.Repo, err)
	}
	target := ""
	for _, line := range strings.Split(result.Stdout, "\n") {
		if ref, rest, ok := strings.Cut(strings.TrimPrefix(line, "ref: "), "\t"); ok && strings.HasPrefix(line, "ref: ") && rest == "HEAD" {
			target = ref
		}
	}
	if target == "" {
		return nil
	}
	if !strings.HasPrefix(target, "refs/heads/") {
		return fmt.Errorf("GitHub's HEAD of %s names %q, not a branch", request.Repo, target)
	}
	current, err := runCommandTimeout(ctx, run, []string{"git", "--git-dir=" + gitDir, "symbolic-ref", "--quiet", "HEAD"}, isolatedGitConfig, "", run.Timeout())
	if err != nil {
		return err
	}
	was := strings.TrimSpace(current.Stdout)
	if was == target {
		return nil
	}
	if _, ok := fetched[target]; !ok {
		request.Log(fmt.Sprintf("GitHub's default branch of %s is now %s, which this fetch did not bring: HEAD stays at %s until a fetch brings it", request.Repo, strings.TrimPrefix(target, "refs/heads/"), cmp.Or(was, "nothing")))
		return nil
	}
	if _, err := FeedGit(ctx, run, gitDir, "symbolic-ref", "HEAD", target); err != nil {
		return err
	}
	request.Log(fmt.Sprintf("the default branch of %s is now %s (HEAD named %s)", request.Repo, strings.TrimPrefix(target, "refs/heads/"), cmp.Or(was, "nothing")))
	return nil
}

// feedRefs is every ref of the bare repository gitDir with the object it names, or none when held
// is unset: a repository a clone is yet to make.
func feedRefs(ctx context.Context, run Runner, gitDir string, held bool) (map[string]string, error) {
	refs := map[string]string{}
	if !held {
		return refs, nil
	}
	listed, err := FeedGit(ctx, run, gitDir, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(listed.Stdout), "\n") {
		if ref, id, ok := strings.Cut(line, " "); ok {
			refs[ref] = id
		}
	}
	return refs, nil
}

// objectsSize is how many bytes the regular files under gitDir's objects directory hold; none when
// there is no such directory yet.
func objectsSize(gitDir string) (int64, error) {
	var size int64
	err := filepath.WalkDir(filepath.Join(gitDir, "objects"), func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		size += info.Size()
		return nil
	})
	return size, err
}

// cacheClient reaches the repository cache directly, at the address it is given, the cache's
// Service: no proxy the container's environment names stands between an issue pod and a pod of its
// own namespace.
var cacheClient = &http.Client{Transport: &http.Transport{Proxy: nil, ForceAttemptHTTP2: false}}

// CachePoll is how long Fetch waits between two asks of a repository cache that is not ready.
const CachePoll = 10 * time.Second

// CacheWaitTimeout bounds Fetch's wait on a repository cache that is not ready: the cache's own
// first clone is bounded by FetchTimeout, and the rest is its pod's scheduling and volume.
const CacheWaitTimeout = FetchTimeout + 10*time.Minute

// FetchBudget bounds a fetch's waits, transfers and checks, end to end: at most CacheWaitTimeout
// waiting for the repository cache, then FetchTimeout for the cache's stream, its verification and
// the delta from GitHub together. The few local git commands around them keep CommandTimeout each
// and take moments, which the launch's boot timeout past ProvisionBound covers. The Sandbox runtime
// allows a launching pod's provisioning this much and the shared clone's (sandbox.Runtime.
// ProvisionBound), so a fetch that runs out of it fails, naming why, before the launch would give
// up on it.
const FetchBudget = CacheWaitTimeout + FetchTimeout

// waitForCache asks the repository cache's GET <cache>/healthz every CachePoll until it answers
// 200 ready, logging what it answered whenever that changes and at least once a minute, and fails
// with the last answer once a further ask would start past CacheWaitTimeout. Each ask is bounded
// by cacheAsk, and by what remains of the wait, so the wait ends within it. An answer that names
// another repository (askCache) fails it at once: no wait makes that right. The asks carry no
// credential (cacheNotReady).
func waitForCache(ctx context.Context, request FetchRequest) error {
	deadline := request.Clock.Now().Add(CacheWaitTimeout)
	var logged string
	var loggedAt time.Time
	for {
		why, err := cacheNotReady(ctx, request, min(cacheAsk, deadline.Sub(request.Clock.Now())))
		if err != nil {
			return err
		}
		if why == "" {
			return nil
		}
		now := request.Clock.Now()
		if !now.Add(CachePoll).Before(deadline) {
			return fmt.Errorf("the repository cache %s was not ready within %s: %s", request.CacheURL, CacheWaitTimeout, why)
		}
		if why != logged || now.Sub(loggedAt) >= time.Minute {
			request.Log(fmt.Sprintf("waiting for the repository cache %s to be ready: %s", request.CacheURL, why))
			logged, loggedAt = why, now
		}
		if err := request.Clock.Sleep(ctx, CachePoll); err != nil {
			return fmt.Errorf("the wait for the repository cache %s ended: %w", request.CacheURL, err)
		}
	}
}

// cacheAsk bounds one ask of a repository cache's /healthz.
const cacheAsk = 30 * time.Second

// cacheNotReady is why the repository cache is not ready to stream, or "" when its /healthz
// answers 200 with ready set, within timeout; an answer askCache refuses is the error. The ask
// carries no credential: the cache answers its health to anyone, and its token goes with the tar
// alone (unpackCache).
func cacheNotReady(ctx context.Context, request FetchRequest, timeout time.Duration) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := askCache(bounded, request, "/healthz", false)
	if response == nil {
		return err.Error(), nil
	}
	defer response.Body.Close()
	if err != nil {
		return "", err
	}
	cache := strings.TrimSuffix(request.CacheURL, "/")
	if response.StatusCode != http.StatusOK {
		return fmt.Sprintf("GET %s/healthz answered %s", cache, response.Status), nil
	}
	var health struct {
		Ready bool `json:"ready"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<16)).Decode(&health); err != nil {
		return fmt.Sprintf("GET %s/healthz answered a body that is not the cache's health: %v", cache, err), nil
	}
	if !health.Ready {
		return fmt.Sprintf("GET %s/healthz answered not ready", cache), nil
	}
	return "", nil
}

// askCache is GET <cache><path>, with the cache's bearer token when withToken is set. It answers
// (nil, the transport's failure) when the cache could not be reached, which is worth another ask.
// An answer that refuses the token (401) to a request that carried it, or whose
// CacheRepositoryHeader is not the request's repository, comes back with the refusal as its error:
// the caller closes the body and takes nothing it carries.
func askCache(ctx context.Context, request FetchRequest, path string, withToken bool) (*http.Response, error) {
	cache := strings.TrimSuffix(request.CacheURL, "/")
	ask, err := http.NewRequestWithContext(ctx, http.MethodGet, cache+path, nil)
	if err != nil {
		return nil, err
	}
	if withToken {
		ask.Header.Set("Authorization", "Bearer "+request.CacheToken)
	}
	response, err := cacheClient.Do(ask)
	if err != nil {
		return nil, err
	}
	if withToken && response.StatusCode == http.StatusUnauthorized {
		return response, fmt.Errorf("ask the repository cache %s: GET %s%s answered %s: it is not the cache whose token this pod holds", request.CacheURL, cache, path, response.Status)
	}
	if named := strings.TrimSpace(response.Header.Get(CacheRepositoryHeader)); !strings.EqualFold(named, request.Repo.String()) {
		shown := named
		if shown == "" {
			shown = `""`
		}
		return response, fmt.Errorf("the repository cache %s is the repository cache of %s, not of %s", request.CacheURL, shown, request.Repo)
	}
	return response, nil
}

// unpackCache streams the repository cache's GET <cache>/tar into staging, a bare repository it
// makes first (`git init --bare`, whose configuration, hooks and info are the feed's own), before
// deadline, and returns the tar's length; Fetch then verifies what it wrote (verifyCache). The
// answer must be the request's repository's (askCache) before any of it is read. Every entry must
// be a regular file or a directory whose cleaned name stays inside staging and is one of a
// repository's refs or packs (FeedEntry); the first that is not ends the stream, refused by name,
// and the caller removes what was written. The stream is read to its end, so a transfer the cache
// broke off is a failure, not a short repository.
func unpackCache(ctx context.Context, run Runner, request FetchRequest, staging string, deadline time.Time) (int64, error) {
	if _, err := RunChecked(ctx, run, []string{"git", "init", "--bare", "--quiet", staging}, isolatedGitConfig, ""); err != nil {
		return 0, err
	}
	left, err := remaining(request, deadline, "the stream from the repository cache")
	if err != nil {
		return 0, err
	}
	bounded, cancel := context.WithTimeout(ctx, left)
	defer cancel()
	response, err := askCache(bounded, request, "/tar", true)
	if response != nil {
		defer response.Body.Close()
	}
	if err != nil {
		return 0, err
	}
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("GET %s answered %s", response.Request.URL, response.Status)
	}
	stream := &countingReader{reader: response.Body}
	archive := tar.NewReader(stream)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("read the repository cache's tar: %w", err)
		}
		if err := unpackEntry(staging, header, archive); err != nil {
			return 0, err
		}
	}
	if _, err := io.Copy(io.Discard, stream); err != nil {
		return 0, fmt.Errorf("read the repository cache's tar: %w", err)
	}
	return stream.read, nil
}

// verifyCache holds the history the cache streamed into staging to what git takes from a remote:
// each pack is indexed afresh (`git index-pack`, which checks the pack's checksum and inflates and
// hashes every object in it, writing the index git then reads, so no index of the cache's is
// trusted), and every object the refs reach must then be present (`git fsck --connectivity-only`).
// A pack that does not index and a history that is not whole are refused, naming git's answer.
// Each runs within what remains before deadline, the bound the stream and the delta share, since
// both follow the repository's size; one with nothing left fails the fetch, naming the bound.
func verifyCache(ctx context.Context, run Runner, request FetchRequest, staging string, deadline time.Time) error {
	packs, err := filepath.Glob(filepath.Join(staging, "objects", "pack", "pack-*.pack"))
	if err != nil {
		return err
	}
	verify := func(argv []string, left time.Duration) error {
		result, err := runCommandTimeout(ctx, run, argv, isolatedGitConfig, "", left)
		_, err = checkedResult(argv, result, err)
		return err
	}
	for _, pack := range packs {
		left, err := remaining(request, deadline, "indexing the repository cache's pack "+filepath.Base(pack))
		if err != nil {
			return err
		}
		if err := verify([]string{"git", "--git-dir=" + staging, "index-pack", pack}, left); err != nil {
			return fmt.Errorf("the repository cache's pack %s, which does not verify: %w", filepath.Base(pack), err)
		}
	}
	left, err := remaining(request, deadline, "checking the repository cache's history is whole")
	if err != nil {
		return err
	}
	if err := verify([]string{"git", "--git-dir=" + staging, "fsck", "--connectivity-only", "--no-dangling", "--no-progress"}, left); err != nil {
		return fmt.Errorf("the repository cache's history is not whole: %w", err)
	}
	return nil
}

// remaining is what is left before deadline, by request's clock, for what runs next, or the
// fetch's failure naming it and the bound once nothing is.
func remaining(request FetchRequest, deadline time.Time, what string) (time.Duration, error) {
	left := deadline.Sub(request.Clock.Now())
	if left <= 0 {
		return 0, fmt.Errorf("the fetch's %s bound ran out before %s", FetchTimeout, what)
	}
	return left, nil
}

// unpackEntry writes one entry of the repository cache's tar into staging, refusing one a feed does
// not take.
func unpackEntry(staging string, header *tar.Header, body io.Reader) error {
	name := strings.TrimSuffix(header.Name, "/")
	if !filepath.IsLocal(filepath.FromSlash(name)) || path.Clean(name) != name {
		return fmt.Errorf("the repository cache's tar holds %q, which is not a path inside the feed", name)
	}
	dir := header.Typeflag == tar.TypeDir
	if !dir && header.Typeflag != tar.TypeReg {
		return fmt.Errorf("the repository cache's tar holds %q, which is neither a regular file nor a directory", name)
	}
	if !FeedEntry(name, dir) {
		return fmt.Errorf("the repository cache's tar holds %q, which is not one of a repository's refs or packs", name)
	}
	target := filepath.Join(staging, filepath.FromSlash(name))
	if dir {
		return os.MkdirAll(target, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, body); err != nil {
		_ = file.Close()
		return fmt.Errorf("read %s from the repository cache's tar: %w", name, err)
	}
	return file.Close()
}

// countingReader counts what it reads.
type countingReader struct {
	reader io.Reader
	read   int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += int64(n)
	return n, err
}

// feedTransfer is how Fetch brings the repository from GitHub: verb names each attempt in the
// log and in a failure, argv is the attempt, and destination, for a clone, is the temporary
// directory the attempt clones into, removed before a retry.
type feedTransfer struct {
	verb        string
	argv        []string
	destination string
}

// fetchInto is the transfer into a feed repository that exists: every branch of GitHub's, named by
// its URL in the argv rather than read from the feed's configuration, pruned of those GitHub
// deleted.
func fetchInto(feed string, repo ghrepo.Repository) feedTransfer {
	return feedTransfer{verb: "fetch", argv: []string{"git", "--git-dir=" + feed, "fetch", "--prune", "--quiet", GitHubURL(repo), "+refs/heads/*:refs/heads/*"}}
}

// stageBeside is the temporary sibling of feed a feed that lacks the repository is filled in. A
// sibling an earlier Fetch left — killed before it renamed or removed it — is removed first and
// logged: only Fetch writes beside the feed's repository, and a killed clone of a large repository
// left on a volume of fixed size would fill it.
func stageBeside(feed string, log func(string)) (string, error) {
	stale, err := filepath.Glob(feed + cloneSuffix + "*")
	if err != nil {
		return "", err
	}
	for _, left := range stale {
		if err := os.RemoveAll(left); err != nil {
			return "", fmt.Errorf("remove the clone an earlier fetch left at %s: %w", left, err)
		}
		log("removed the clone an earlier fetch left at " + left)
	}
	staging, err := os.MkdirTemp(filepath.Dir(feed), filepath.Base(feed)+cloneSuffix)
	if err != nil {
		return "", fmt.Errorf("create the clone's temporary directory beside %s: %w", feed, err)
	}
	return staging, nil
}

// cloneSuffix follows the feed repository's name in the temporary directory it is filled in.
const cloneSuffix = ".clone-"

// feedConfiguration is what the feed repository's own configuration sets: no automatic garbage
// collection or maintenance, ever, and every fetch's objects kept as the pack they arrived in
// (`fetch.unpackLimit=1`). The issue's clone keeps its objects in the feed
// (objects/info/alternates), so an object a gc dropped from the feed — one reachable only from a
// branch GitHub deleted and the fetch pruned — would be gone from the clone that still names it.
// And the repository cache streams its clone's packs alone (FeedEntry), so a fetch that exploded a
// small pack into loose objects would leave the history it brought out of every issue's feed.
func feedConfiguration(ctx context.Context, run Runner, gitDir string) error {
	for _, pair := range [][2]string{{"gc.auto", "0"}, {"maintenance.auto", "false"}, {"fetch.unpackLimit", "1"}} {
		if _, err := FeedGit(ctx, run, gitDir, "config", pair[0], pair[1]); err != nil {
			return err
		}
	}
	return nil
}

// FeedGit runs git on the bare repository gitDir — a feed's, or the repository cache's — under
// the runner's slow-command budget, holding no credential and reading no configuration but that
// repository's own: what reads or tends such a repository outside Fetch's transfer.
func FeedGit(ctx context.Context, run Runner, gitDir string, args ...string) (Result, error) {
	return RunChecked(ctx, run, append([]string{"git", "--git-dir=" + gitDir}, args...), isolatedGitConfig, "")
}

// transferWithRetries runs transfer under the one-shot credential held until it succeeds, retrying
// a transient failure as Fetch says, each attempt bounded by what remains before deadline and run
// on the provisioning token as it stands when the attempt starts (renew). It answers the attempt
// that succeeded.
func transferWithRetries(ctx context.Context, run Runner, request FetchRequest, transfer feedTransfer, held *fetchCredential, deadline time.Time) (Result, error) {
	now := request.Clock.Now()
	if _, err := remaining(request, deadline, "the "+transfer.verb+" from GitHub"); err != nil {
		return Result{}, err
	}
	for attempt := 1; ; attempt++ {
		if err := held.renew(); err != nil {
			return Result{}, fmt.Errorf("%s attempt %d: %w", transfer.verb, attempt, err)
		}
		result, runErr := runCommandTimeout(ctx, run, transfer.argv, held.env, "", deadline.Sub(now))
		result.Stderr = held.scrub(result.Stderr)
		_, failed := checkedResult(transfer.argv, result, runErr)
		if failed == nil {
			return result, nil
		}
		if runErr != nil || !transientCloneFailure(result) {
			return Result{}, failed
		}
		if attempt > len(fetchBackoff) {
			return Result{}, fmt.Errorf("%w\n%s attempt %d failed, and the fetch retries a %s at most %d times", failed, transfer.verb, attempt, transfer.verb, len(fetchBackoff))
		}
		wait := fetchBackoff[attempt-1]
		if !request.Clock.Now().Add(wait).Before(deadline) {
			return Result{}, fmt.Errorf("%w\n%s attempt %d failed, and a retry in %s would start past the fetch's %s bound", failed, transfer.verb, attempt, wait, FetchTimeout)
		}
		request.Log(fmt.Sprintf("%s attempt %d failed; retrying in %s: %s", transfer.verb, attempt, wait,
			strings.ReplaceAll(strings.TrimSpace(result.Stderr), "\n", " | ")))
		if transfer.destination != "" {
			if err := os.RemoveAll(transfer.destination); err != nil {
				return Result{}, fmt.Errorf("remove what %s attempt %d left at %s: %w", transfer.verb, attempt, transfer.destination, err)
			}
		}
		if err := request.Clock.Sleep(ctx, wait); err != nil {
			return Result{}, fmt.Errorf("%w\nthe wait before %s attempt %d ended: %w", failed, transfer.verb, attempt+1, err)
		}
		if now = request.Clock.Now(); !now.Before(deadline) {
			return Result{}, fmt.Errorf("%w\n%s attempt %d failed, and its retry after the %s wait would start past the fetch's %s bound", failed, transfer.verb, attempt, wait, FetchTimeout)
		}
	}
}

// fetchCredential is the one-shot credential a fetch's transfers from GitHub run under (remote,
// with env the environment that names it), the way to read the provisioning token
// (FetchRequest.Token), and every token it has held, none of which anything Fetch logs or returns
// carries.
type fetchCredential struct {
	remote remote
	env    []string
	read   func() (string, error)
	held   []string
}

// renew reads the provisioning token as it stands now and writes it over the credential's, so the
// transfer about to start presents it: a token file the daemon refreshed during a long wait on the
// repository cache, or a retry's backoff, is the one git is given, never the token the fetch began
// with.
func (c *fetchCredential) renew() error {
	token, err := c.read()
	if err != nil {
		return fmt.Errorf("read the provisioning token: %w", err)
	}
	if err := c.remote.setToken(token); err != nil {
		return err
	}
	if !slices.Contains(c.held, token) {
		c.held = append(c.held, token)
	}
	return nil
}

// scrub is text with every token the credential has held replaced by ***.
func (c *fetchCredential) scrub(text string) string {
	for _, token := range c.held {
		text = strings.ReplaceAll(text, token, "***")
	}
	return text
}

// Clock is the time Fetch's retries read and wait on: WallClock, or a test's own.
type Clock interface {
	Now() time.Time
	// Sleep waits d, or returns ctx's error once ctx ends first.
	Sleep(ctx context.Context, d time.Duration) error
}

// WallClock is the real time.
type WallClock struct{}

func (WallClock) Now() time.Time { return time.Now() }

func (WallClock) Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// fetchBackoff is the wait before each retry of the fetch's clone, one retry per entry: a first
// retry soon after a stream GitHub cut, the last a minute later. A clone still failing after the
// last fails the fetch, so a GitHub outage holds a launch for a few attempts, not the whole
// FetchTimeout, which bounds a slow transfer instead.
var fetchBackoff = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute}

// gitFatalExit is the status git's die() exits with, which every failure below ends in. Any other
// is not git reporting a failed transfer: a signal, a usage error, or the attempt's own timeout.
const gitFatalExit = 128

var (
	// causeLine is git's line saying why a request of the clone failed, in either of its forms:
	// remote-curl's `RPC failed; …` for a request of the transfer, once the repository was found,
	// and `unable to access '<url>': …` for the first request. What follows is the HTTP status the
	// server answered with, or curl's own error.
	causeLine = regexp.MustCompile(`(?:RPC failed; |unable to access '[^']*': )(.*)`)
	// statusFailure is the HTTP status a cause names, in either form: `HTTP <status> curl …` after
	// `RPC failed;`, or curl's `The requested URL returned error: <status>`.
	statusFailure = regexp.MustCompile(`(?:^HTTP |The requested URL returned error: )(\d+)`)
)

// curlNetworkFailures are curl's errors for a connection that could not be made or broke: never
// an answer from GitHub, so the next attempt may not meet them. Any other curl error a cause names
// is not here — a certificate that fails to verify (curl 60), a write to the clone that failed
// (curl 23) — since it fails the same way every time.
var curlNetworkFailures = []string{
	"Failed to connect to",     // CURLE_COULDNT_CONNECT (7): refused, or the connect timed out
	"Could not resolve host",   // CURLE_COULDNT_RESOLVE_HOST (6)
	"timed out",                // CURLE_OPERATION_TIMEDOUT (28): "Connection timed out after …", "Operation timed out after …"
	"Connection reset by peer", // a reset under any of curl's errors
	"Recv failure",             // CURLE_RECV_ERROR (56)
	"Send failure",             // CURLE_SEND_ERROR (55)
	"Empty reply from server",  // CURLE_GOT_NOTHING (52)
	"transfer closed with outstanding read data remaining", // CURLE_PARTIAL_FILE (18)
	"was not closed cleanly",                               // CURLE_HTTP2_STREAM (92)
	"HTTP2 framing layer",                                  // CURLE_HTTP2 (16)
	"SSL_ERROR_SYSCALL",                                    // OpenSSL: the connection closed under TLS
	"unexpected eof while reading",                         // OpenSSL 3: the same
	"TLS connection was non-properly terminated",           // GnuTLS, Debian's git: the same
	"GnuTLS recv error",                                    // GnuTLS: a read that failed
	"Error in the pull function",                           // GnuTLS: the same, under the handshake
	"Error in the push function",                           // GnuTLS: a write that failed
}

// streamCuts are git's own lines for a pack stream that ended before the pack did, which a
// connection closed mid-transfer leaves when curl itself reported nothing.
var streamCuts = []string{
	"early EOF",
	"unexpected disconnect while reading sideband packet",
	"the remote end hung up unexpectedly",
}

// transientCloneFailure is whether a clone of GitHub that failed may succeed if run again: git's
// fatal exit, with a cause that names a status GitHub may not answer again — 408 (it timed the
// request out), 429 (it is limiting the rate) or a 5xx (it failed) — or a connection that failed
// or broke (curlNetworkFailures), or, with no cause line at all, a pack stream that ended early
// (streamCuts). The cause line decides, never the follow-on line git prints after it: a 403 on a
// request of the transfer ends in the same `expected flush after ref listing` a 502 does. A
// credential refused (`Authentication failed`), a repository not found or not granted (404, 403),
// a certificate that does not verify, a full disk, and anything this does not recognise fail at
// once, as every clone failure did before the fetch retried.
func transientCloneFailure(result Result) bool {
	if result.TimedOut || result.ExitCode != gitFatalExit {
		return false
	}
	cause := causeLine.FindStringSubmatch(result.Stderr)
	if cause == nil {
		return containsAny(result.Stderr, streamCuts)
	}
	if status := statusFailure.FindStringSubmatch(cause[1]); status != nil {
		return transientStatus(status[1])
	}
	return containsAny(cause[1], curlNetworkFailures)
}

// transientStatus is whether an HTTP status is one a server may not answer again: 408, 429, 5xx.
func transientStatus(status string) bool {
	code, err := strconv.Atoi(status)
	return err == nil && (code == 408 || code == 429 || code >= 500 && code <= 599)
}

// containsAny is whether text holds any of needles.
func containsAny(text string, needles []string) bool {
	return slices.ContainsFunc(needles, func(needle string) bool { return strings.Contains(text, needle) })
}
