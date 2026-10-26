package workspace

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
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
// clone at each refresh — has the delta fetched into it from GitHub, once the locks a killed fetch
// left are gone (RemoveStaleFeedLocks). A feed that does not is filled in a temporary sibling, its
// garbage collection turned off (feedConfiguration), and renamed into place once what fills it
// succeeded, so the feed's repository is either whole or absent. With a cache named, it comes from
// the cache alone: Fetch waits for the cache to be ready (waitForCache), unpacks its tar
// (unpackCache), verifies it (verifyCache), moves it into the feed, and only then fetches from
// GitHub the default branch and the Legion issue branches, which is what the issue's clone is
// made from, after deleting every branch GitHub no longer has (pruneToGitHub, fetchInto); a
// cache that is not ready within CacheWaitTimeout, or whose tar a feed does not take, fails the
// fetch, and nothing is cloned from GitHub instead, so the cache's own first clone is the one full
// download of the repository. With none, the feed is cloned bare from GitHub, or has every branch
// fetched into it and the ones GitHub deleted pruned: what the cache's own process does. Provision
// then clones the shared clone from the feed with no credential.
//
// The feed moves into place before the delta so that a fetch killed in the delta leaves the
// cache's stream on the volume: the pod the daemon makes in its place finds the repository, and
// asks GitHub for the delta alone. Nine LINUX issue pods died in the delta on 2026-10-26, and each
// replacement threw away a verified 2 GB stream and began again (sjawhar/legion#2235). The feed
// it leaves is a whole repository, no further behind GitHub than the cache is, and provisioning
// never sees it: `workspace-init provision` runs only after this container exits 0.
//
// Each size-following stage of a fetch gets FetchTimeout of its own, and none starts past the one
// it is in: from the cache, the stream and its verification share one and the delta from GitHub
// has another; without a cache, the clone or fetch from GitHub has one. So the fetch's waits, transfers and checks end
// within FetchBudget, however long it waited on the cache; the few local
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
		narrowed, heads, err := deltaBranches(ctx, run, request, feed, held, deadline)
		if err != nil {
			return FetchReport{}, err
		}
		if err := pruneToGitHub(ctx, run, request, feed, narrowed, heads); err != nil {
			return FetchReport{}, err
		}
		if err := report.fromGitHub(ctx, run, request, fetchInto(feed, request.Repo, narrowed), feed, held, deadline); err != nil {
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
	if request.CacheURL == "" {
		clone := feedTransfer{verb: "clone", argv: []string{"git", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "clone", "--bare", "--quiet", GitHubURL(request.Repo), staging}, destination: staging}
		report.Cloned = true
		deadline := request.Clock.Now().Add(FetchTimeout)
		if err := report.fromGitHub(ctx, run, request, clone, staging, held, deadline); err != nil {
			return FetchReport{}, err
		}
		if err := settleFeed(ctx, run, staging, feed); err != nil {
			return FetchReport{}, err
		}
		return report, credential.remove()
	}

	if err := waitForCache(ctx, request); err != nil {
		return FetchReport{}, err
	}
	cached := request.Clock.Now().Add(FetchTimeout)
	started := request.Clock.Now()
	streamed, err := unpackCache(ctx, run, request, staging, cached)
	verifying := request.Clock.Now()
	if err == nil {
		err = verifyCache(ctx, run, request, staging, cached)
	}
	if err != nil {
		return FetchReport{}, fmt.Errorf("fill the feed from the repository cache %s: %w", request.CacheURL, err)
	}
	report.CacheBytes, report.CacheDuration, report.VerifyDuration = streamed, verifying.Sub(started), request.Clock.Now().Sub(verifying)
	if err := settleFeed(ctx, run, staging, feed); err != nil {
		return FetchReport{}, err
	}
	deadline := request.Clock.Now().Add(FetchTimeout)
	narrowed, heads, err := deltaBranches(ctx, run, request, feed, held, deadline)
	if err != nil {
		return FetchReport{}, err
	}
	if err := pruneToGitHub(ctx, run, request, feed, narrowed, heads); err != nil {
		return FetchReport{}, err
	}
	if err := report.fromGitHub(ctx, run, request, fetchInto(feed, request.Repo, narrowed), feed, held, deadline); err != nil {
		return FetchReport{}, err
	}
	return report, credential.remove()
}

// settleFeed writes the feed repository's own configuration into staging and moves it to feed,
// where the next fetch of this pod, or of the pod a killed one is replaced by, finds the
// repository and asks GitHub for its delta alone.
func settleFeed(ctx context.Context, run Runner, staging, feed string) error {
	if err := feedConfiguration(ctx, run, staging); err != nil {
		return err
	}
	if err := os.Rename(staging, feed); err != nil {
		return fmt.Errorf("move the fetched repository into the feed at %s: %w", feed, err)
	}
	return nil
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

// deltaBranches reads what GitHub holds now, when the feed this fetch fills comes from the
// repository cache: the default branch its HEAD names, as a ref, and every branch it has with its
// tip. It is read with one `git ls-remote --symref <url> HEAD refs/heads/*` under the fetch's
// credential, retries and deadline. A feed with no cache behind it — the cache's own process —
// answers "" and no branches, and takes every branch, pruned, as before; so does a GitHub that
// names no default branch (nothing pushed yet).
//
// The default branch is what fetchInto narrows the delta to, beside the issue branches: a fetch of
// every branch pulls whatever anyone pushed to the repository since the cache's last refresh into
// every issue pod at once, which no pod reads (sjawhar/legion#2235). The branch list is what
// pruneToGitHub holds the rest of the feed to.
func deltaBranches(ctx context.Context, run Runner, request FetchRequest, gitDir string, held *fetchCredential, deadline time.Time) (string, map[string]string, error) {
	if request.CacheURL == "" {
		return "", nil, nil
	}
	target, heads, err := remoteRefs(ctx, run, request, gitDir, held, deadline, "refs/heads/*")
	if err != nil {
		return "", nil, err
	}
	if target == "" {
		return "", nil, nil
	}
	return target, heads, nil
}

// pruneToGitHub deletes from the bare repository gitDir, before the delta, every branch the delta
// will not bring current and that is not already current: one GitHub no longer lists, and one
// outside the delta's refspecs (the default branch defaultRef and refs/heads/legion/*) whose tip is
// not GitHub's (heads, read by deltaBranches). Each removal is logged. So every branch a feed holds
// after the fetch is GitHub's as it stood at this fetch, and nothing stale is left to start from.
// The narrowed delta prunes only within its own refspecs, and a feed kept on the volume is never
// streamed again, so without this such a feed would keep, for good, a branch GitHub deleted, the
// old name of a default branch GitHub renamed, and the tip it was first streamed at of a branch
// GitHub moved — among them an issue's recorded default after GitHub's default was switched to
// another branch, which a workspace provisioned again there with no pushed branch would start from
// without an error. Now provisioning finds that branch current, or missing and refuses (baseCommit).
// A branch created on GitHub after the read is not in heads and is not in the feed either, since
// nothing the fetch takes is deleted here: only branches the feed held before the delta are
// candidates.
func pruneToGitHub(ctx context.Context, run Runner, request FetchRequest, gitDir, defaultRef string, heads map[string]string) error {
	if heads == nil {
		return nil
	}
	held, err := feedRefs(ctx, run, gitDir, true)
	if err != nil {
		return err
	}
	gone := []string{}
	for ref, tip := range held {
		if !strings.HasPrefix(ref, "refs/heads/") || ref == defaultRef || strings.HasPrefix(ref, strings.TrimSuffix(issueBranches, "*")) {
			continue
		}
		if heads[ref] != tip {
			gone = append(gone, ref)
		}
	}
	slices.Sort(gone)
	for _, ref := range gone {
		if _, err := FeedGit(ctx, run, gitDir, "update-ref", "--no-deref", "-d", ref); err != nil {
			return err
		}
		why := "GitHub no longer has it"
		if _, listed := heads[ref]; listed {
			why = "GitHub has moved it, and this fetch brings only the default branch and the issue branches"
		}
		request.Log(fmt.Sprintf("removed %s from the feed: %s", strings.TrimPrefix(ref, "refs/heads/"), why))
	}
	return nil
}

// remoteHead is the ref GitHub's HEAD names, read as remoteRefs reads it; "" when GitHub names
// none.
func remoteHead(ctx context.Context, run Runner, request FetchRequest, gitDir string, held *fetchCredential, deadline time.Time) (string, error) {
	target, _, err := remoteRefs(ctx, run, request, gitDir, held, deadline)
	return target, err
}

// remoteRefs is the ref GitHub's HEAD names, and every ref GitHub lists matching patterns with the
// object it names (`git ls-remote --symref <url> HEAD <patterns…>`), run as a transfer under the
// fetch's credential, retries and deadline; "" when GitHub names no HEAD. A HEAD that names
// anything but a branch is the fetch's failure.
func remoteRefs(ctx context.Context, run Runner, request FetchRequest, gitDir string, held *fetchCredential, deadline time.Time, patterns ...string) (string, map[string]string, error) {
	argv := append([]string{"git", "--git-dir=" + gitDir, "ls-remote", "--symref", GitHubURL(request.Repo), "HEAD"}, patterns...)
	result, err := transferWithRetries(ctx, run, request, feedTransfer{verb: "ls-remote", argv: argv}, held, deadline)
	if err != nil {
		return "", nil, fmt.Errorf("read GitHub's default branch of %s: %w", request.Repo, err)
	}
	target := ""
	listed := map[string]string{}
	for _, line := range strings.Split(result.Stdout, "\n") {
		if ref, rest, ok := strings.Cut(strings.TrimPrefix(line, "ref: "), "\t"); ok && strings.HasPrefix(line, "ref: ") && rest == "HEAD" {
			target = ref
			continue
		}
		if id, ref, ok := strings.Cut(line, "\t"); ok && ref != "HEAD" {
			listed[ref] = id
		}
	}
	if target != "" && !strings.HasPrefix(target, "refs/heads/") {
		return "", nil, fmt.Errorf("GitHub's HEAD of %s names %q, not a branch", request.Repo, target)
	}
	return target, listed, nil
}

// followHead points the bare repository gitDir's HEAD at the branch GitHub's HEAD names now
// (`git ls-remote --symref`), under the fetch's credential, retries and deadline, and logs the
// move when there is one. A fetch never moves HEAD, and the cache's own fetch prunes the branches
// GitHub deleted, so a repository whose default branch GitHub renamed would leave the cache a HEAD
// naming a branch that is gone: it could publish no head, and an issue's clone would check nothing
// out. (An issue pod's narrowed fetch
// removes the old name too, before its delta: see pruneToGitHub.) HEAD moves only to
// a branch among fetched, the refs the fetch left: GitHub renaming its branch between the fetch and
// this read names one the fetch did not bring, so HEAD stays, logged, until a fetch brings it. A
// clone takes GitHub's HEAD itself. A repository GitHub answers with no default branch (nothing
// pushed yet) keeps its HEAD.
func followHead(ctx context.Context, run Runner, request FetchRequest, gitDir string, fetched map[string]string, held *fetchCredential, deadline time.Time) error {
	target, err := remoteHead(ctx, run, request, gitDir, held, deadline)
	if err != nil {
		return err
	}
	if target == "" {
		return nil
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
// waiting for the repository cache, then FetchTimeout for the cache's stream and its verification,
// then FetchTimeout for the delta from GitHub, which has a bound of its own so that a slow stream
// cannot leave the delta minutes to run in (sjawhar/legion#2235). A fetch with no cache holds one
// FetchTimeout for its clone or its fetch, well inside this. The few local git commands around
// them keep CommandTimeout each
// and take moments, which the launch's boot timeout past ProvisionBound covers. The Sandbox runtime
// allows a launching pod's provisioning this much and the shared clone's (sandbox.Runtime.
// ProvisionBound), so a fetch that runs out of it fails, naming why, before the launch would give
// up on it.
const FetchBudget = CacheWaitTimeout + FetchTimeout + FetchTimeout

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

// verifyCache holds the history the cache streamed into staging to what a feed takes: every pack's
// bytes must be the ones its checksum names, its index must carry its own checksum and name that
// pack, and every object the refs reach must be there (`git fsck --connectivity-only`, which walks
// the reachable commits and trees through the packs' indexes and reads no blob).
//
// A pack the cache streamed its index with (FeedEntry) is checked by the checksums git writes into
// both (packChecksums): the pack's own trailing hash over its bytes, the index's over its, and the
// pack hash the index records, which must be the pack's. That catches a pack or an index a
// transfer or a disk damaged, at a few seconds per gigabyte. It does not repeat `git index-pack`'s
// inflation and rehashing of every object, so it takes the index's names on the cache's word: an
// index whose checksums hold but which maps an object's name to another object's offset passes
// here and passes `fsck --connectivity-only`, and a checkout of a blob it misnames writes the other
// blob's content without an error, and the issue's jj workspace records no change for it. What
// this trusts is every writer of the cache's indexes: `git index-pack`, which named every object by
// its content when `git fetch` received the pack from GitHub; the cache's daily
// `git repack -a -d`, whose `pack-objects` writes new indexes from names it already holds and does
// not hash the objects again; the cache pod's volume; and whoever answers at the cache's address,
// since the bearer token proves the pod to the cache and nothing proves the cache to the pod. The
// daemon checks the cache's Service is the cache's own before it tells a pod the address, once
// (docs/kubernetes.md "The repository cache"). Rehashing in every pod caught a fault in any of
// those; it costs minutes of CPU per gigabyte in every pod instead — on the LINUX deployment's
// 3.2 GB cache, 8 to 12 minutes in every issue pod, the verification nine of them ran out of their
// bound in (sjawhar/legion#2235).
//
// A pack that arrived without its index is indexed here, as every pack was before: then the feed,
// and not the cache, writes the index git reads. A pack that does not verify and a history that is
// not whole are refused, naming what failed. Each git command runs within what remains before
// deadline, the bound the stream and its verification share; one with nothing left fails the
// fetch, naming the bound.
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
	hash, err := objectHash(ctx, run, staging)
	if err != nil {
		return err
	}
	for _, pack := range packs {
		index := strings.TrimSuffix(pack, ".pack") + ".idx"
		indexed, err := pathExists(index)
		if err != nil {
			return err
		}
		if indexed {
			if err := packChecksums(pack, index, hash); err != nil {
				return fmt.Errorf("the repository cache's pack %s, which does not verify: %w", filepath.Base(pack), err)
			}
			continue
		}
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

// objectHash is the hash function the repository at gitDir names its objects with, as git reports
// it (`git rev-parse --show-object-format`). Every checksum a pack and its index carry is that
// hash's, so its size is how those files end. Today it is always sha1: unpackCache makes the staging
// repository with `git init --bare` under no configuration, and a feed refuses any object format but
// sha1 (feedConfigKeys), so a SHA-256 cache is refused at its pack's checksum. The sha256 case is
// for a git whose default format has changed, and reads the same file layout with 32-byte hashes.
func objectHash(ctx context.Context, run Runner, gitDir string) (func() hash.Hash, error) {
	named, err := FeedGit(ctx, run, gitDir, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, err
	}
	switch format := strings.TrimSpace(named.Stdout); format {
	case "sha1":
		return sha1.New, nil
	case "sha256":
		return sha256.New, nil
	default:
		return nil, fmt.Errorf("the repository at %s names its objects with %q, which this fetch does not know", gitDir, format)
	}
}

// packChecksums holds a pack and the index git reads it through to the checksums git wrote into
// them: a pack ends with the hash of everything before it, an index with the hash of everything
// before it, and an index records before its own hash the hash of the pack it indexes. All three
// must hold, so neither file lost or gained a byte on the way and the index is this pack's.
func packChecksums(pack, index string, hash func() hash.Hash) error {
	packHash, err := trailingHash(pack, hash)
	if err != nil {
		return err
	}
	if _, err := trailingHash(index, hash); err != nil {
		return err
	}
	size := hash().Size()
	indexed := make([]byte, size)
	file, err := os.Open(index)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
	}()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() < int64(2*size) {
		return fmt.Errorf("its index %s is %d bytes, too short to hold the checksums git writes", filepath.Base(index), info.Size())
	}
	if _, err := file.ReadAt(indexed, info.Size()-int64(2*size)); err != nil {
		return fmt.Errorf("read the pack checksum its index %s records: %w", filepath.Base(index), err)
	}
	if !bytes.Equal(indexed, packHash) {
		return fmt.Errorf("its index %s indexes the pack %x, not this one", filepath.Base(index), indexed)
	}
	return nil
}

// trailingHash checks that the file at path ends with the hash of everything before it, git's own
// checksum of a pack and of a pack index, and answers that hash.
func trailingHash(path string, hash func() hash.Hash) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = file.Close()
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	sum := hash()
	size := int64(sum.Size())
	if info.Size() < size {
		return nil, fmt.Errorf("%s is %d bytes, too short to hold the checksum git writes", filepath.Base(path), info.Size())
	}
	if _, err := io.Copy(sum, io.LimitReader(file, info.Size()-size)); err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	written := make([]byte, size)
	if _, err := file.ReadAt(written, info.Size()-size); err != nil {
		return nil, fmt.Errorf("read the checksum of %s: %w", filepath.Base(path), err)
	}
	if computed := sum.Sum(nil); !bytes.Equal(computed, written) {
		return nil, fmt.Errorf("%s carries the checksum %x over bytes that hash to %x", filepath.Base(path), written, computed)
	}
	return written, nil
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

// fetchInto is the transfer into a feed repository that exists, with the remote named by its URL
// in the argv rather than read from the feed's configuration.
//
// An issue pod's feed (defaultRef set: the branch GitHub's HEAD names, read by deltaBranches)
// takes that branch and every Legion issue branch, and nothing else. Those are what the issue's
// clone is made from — feedRemote fetches the repository's default branch and the issue's own
// bookmark by name and no other — and everything else the feed holds is as the repository cache's
// stream left it: at most one refresh behind GitHub when the feed was streamed, older on a feed
// kept on the volume, which is never streamed again, or from a cache whose fetches have been
// failing. An agent that wants a branch newer than that fetches it from GitHub through its clone's
// origin. A branch outside those is then never pulled
// into every issue pod at once: the LINUX deployment's owners each fetched 1.27 GB of one
// upstream-sync branch pushed five minutes before they launched, which with the cache's stream and
// verification left nine of fourteen pods over FetchTimeout, each restarting the whole fetch
// (sjawhar/legion#2235). The issue branches' refspec is a pattern, so a fetch brings whichever
// exist and never fails on an issue whose branch is not pushed yet. The fetch prunes, and with
// refspecs on its command line git prunes only the refs those refspecs write: an issue branch
// GitHub deleted leaves the feed, as provisioning relies on (a bookmark with no row on the feed
// starts at the default branch, and the refusals that tell an operator to delete a branch on
// GitHub promise the next provisioning drops its row). A branch outside the two is kept only while
// it is exactly GitHub's: before the delta, Fetch deletes every other branch GitHub no longer lists
// or has moved (pruneToGitHub), since this prune reaches only its refspecs and a feed kept on the
// volume is never streamed again. So a feed holds no stale branch to start from: a default branch
// GitHub renamed, or one an issue recorded before GitHub's default was switched elsewhere and
// which has moved since, is current or absent, and provisioning refuses an absent one
// (baseCommit) rather than starting from an old tip.
//
// The repository cache's own process (defaultRef empty) takes every branch instead, pruned of
// those GitHub deleted: what it holds is what every issue pod of its repository starts from.
func fetchInto(feed string, repo ghrepo.Repository, defaultRef string) feedTransfer {
	argv := []string{"git", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "--git-dir=" + feed, "fetch", "--prune", "--quiet", GitHubURL(repo)}
	if defaultRef == "" {
		argv = append(argv, "+refs/heads/*:refs/heads/*")
	} else {
		argv = append(argv, "+"+defaultRef+":"+defaultRef, "+"+issueBranches+":"+issueBranches)
	}
	return feedTransfer{verb: "fetch", argv: argv}
}

// issueBranches is the pattern every Legion issue branch of a repository matches: Bookmark names
// an issue's branch `legion/<KEY>`.
const issueBranches = "refs/heads/legion/*"

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
// (`fetch.unpackLimit=1`). The clone owns entries for its history, so this keeps feed refreshes
// predictable and preserves the repository cache's pack-only stream; it is not the clone's only
// copy of history.
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
