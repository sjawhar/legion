//go:build e2e

// The Stage 4a harness's repository cache checks: the cache of the run's repository serves, an issue
// pod launched while it is ready fills its feed from it and asks GitHub only for the delta, and an
// issue whose volume is lost is provisioned again from the cache and its pushed branch. The rig is
// live_test.go.

package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// cacheStreamLine is the fetch container's last line when it filled the feed from the repository
// cache: the tar's bytes, the cache's address, and the bytes and refs GitHub's delta brought.
var cacheStreamLine = regexp.MustCompile(`workspace-init fetch: ([0-9,]+) bytes from the repository cache (http://\S+) in \S+; ([0-9,]+) bytes from GitHub for the delta \(([0-9]+) refs?\)`)

// repository-cache: before the run's repository is mapped no cache exists, so an issue pod launched
// then (cache-a) clones its feed from GitHub and says so — the negative control of
// cache-second-pod. Once the repository is mapped, the runtime's cache for it runs and is Ready,
// and its /healthz, asked inside its own pod, answers ready at the head GitHub names for the
// repository's default branch.
func (r *liveRig) checkRepositoryCache() error {
	before := r.claim("cache-a")
	if err := r.ensureRunning(before); err != nil {
		return err
	}
	log, err := r.fetchLog(SandboxName(before.token))
	if err != nil {
		return err
	}
	if want := "workspace-init fetch: no ready repository cache for " + r.env.repo.String() + ": cloning from GitHub ("; !strings.Contains(log, want) {
		return fmt.Errorf("cache-a, launched before any cache, logged %q, want %q", strings.TrimSpace(log), want)
	}
	note("operator", "cache-a's workspace-fetch log, launched with no cache: %s", oneLine(log))

	name := CacheSandboxName(r.env.project, r.env.repo)
	if err := r.recordSandbox(name); err != nil {
		return err
	}
	r.mappedMu.Lock()
	r.mapped = []ghrepo.Repository{r.env.repo}
	r.mappedMu.Unlock()
	if err := r.rt.EnsureRepositoryCache(r.ctx, r.env.repo); err != nil {
		return err
	}
	note("runtime", "EnsureRepositoryCache(%s): Sandbox %s", r.env.repo, name)
	status, err := r.awaitCacheReady()
	if err != nil {
		return err
	}
	out, err := r.kubectl("exec", name, "-c", cacheContainer, "--", "curl", "-s", fmt.Sprintf("localhost:%d/healthz", repocache.Port))
	if err != nil {
		return err
	}
	var health repocache.Health
	if err := json.Unmarshal([]byte(out), &health); err != nil {
		return fmt.Errorf("the cache's /healthz answered %q, not its JSON: %w", strings.TrimSpace(out), err)
	}
	head, err := r.githubHead(r.defaultBranch)
	if err != nil {
		return err
	}
	if !health.Ready || health.Repo != r.env.repo.String() || health.Head != head || health.Bytes <= 0 || health.LastFetchAt.IsZero() {
		return fmt.Errorf("the cache's /healthz answered %+v, want ready for %s at %s's head %s", health, r.env.repo, r.defaultBranch, head)
	}
	note("runtime", "RepositoryCache(%s): ready at %s", r.env.repo, status.Address)
	note("operator", "exec %s -c %s -- curl -s localhost:%d/healthz: %s; GitHub's %s is %s", name, cacheContainer, repocache.Port, strings.TrimSpace(out), r.defaultBranch, head)
	return nil
}

// cache-second-pod: an issue pod (cache-b) launched while the cache is Ready fills its feed from
// the cache's tar and fetches from GitHub only the delta, under 1 MiB; its clone's objects are the
// feed's (objects/info/alternates names the feed on the issue volume), and git checks the clone's
// connectivity through them.
func (r *liveRig) checkCacheSecondPod() error {
	status, err := r.awaitCacheReady()
	if err != nil {
		return err
	}
	c := r.claim("cache-b")
	if err := r.ensureRunning(c); err != nil {
		return err
	}
	if err := r.assertCacheFed(c, status); err != nil {
		return err
	}
	clone := TreeRoot + "/repos/github.com/" + r.env.repo.Owner() + "/" + r.env.repo.Name()
	alternates, err := r.exec(c, "cat", clone+"/.git/objects/info/alternates")
	if err != nil {
		return err
	}
	if want := FeedDir + "/" + r.env.repo.Owner() + "/" + r.env.repo.Name() + ".git/objects"; alternates != want {
		return fmt.Errorf("cache-b's clone's alternates is %q, want %q", alternates, want)
	}
	fsck, err := r.exec(c, "git", "--git-dir="+clone+"/.git", "fsck", "--connectivity-only", "--no-progress")
	if err != nil {
		return fmt.Errorf("git fsck of cache-b's clone: %w", err)
	}
	note("operator", "exec %s -- cat %s/.git/objects/info/alternates: %s; git fsck --connectivity-only: ok %s", SandboxName(c.token), clone, alternates, oneLine(fsck))
	return nil
}

// cache-lost-volume: cache-b's Sandbox and volume are deleted while no runtime runs, and its issue
// branch exists on GitHub (created by the harness at the default branch, as a role's push would
// have left one). Relaunched with the branch as its recovery source, the new pod fills its new
// feed from the cache again — the branch the cache has not fetched yet arriving in GitHub's delta
// — and provisions the workspace on the branch, whose commit is the workspace's @-.
func (r *liveRig) checkCacheLostVolume() error {
	status, err := r.awaitCacheReady()
	if err != nil {
		return err
	}
	c := r.claim("cache-b")
	if err := r.ensureRunning(c); err != nil {
		return err
	}
	dir, err := workspace.Location(TreeRoot, r.env.repo, c.issue)
	if err != nil {
		return err
	}
	if err := r.createRunBranch(dir.Bookmark); err != nil {
		return err
	}
	branchHead, err := r.githubHead(dir.Bookmark)
	if err != nil {
		return err
	}
	note("operator", "branch %s on %s at %s (the default branch's head), deleted when the run ends", dir.Bookmark, r.env.repo, branchHead)

	r.stopRuntime()
	name := SandboxName(c.token)
	if _, err := r.kubectl("delete", "sandbox", name, "--wait=true", "--timeout=5m"); err != nil {
		return err
	}
	if _, err := r.kubectl("delete", "pvc", IssueClaimName(c.token), "--ignore-not-found", "--wait=true", "--timeout=5m"); err != nil {
		return err
	}
	if bound, err := r.issuePVCs(c.issue); err != nil || len(bound) != 0 {
		return fmt.Errorf("cache-b's volume outlived its deletion: %v (%v)", bound, err)
	}
	c.loc, c.last, c.state = nil, nil, stateNone
	note("operator", "deleted Sandbox %s and PVC %s while no runtime ran", name, IssueClaimName(c.token))

	if err := r.startRuntime(); err != nil {
		return err
	}
	if err := r.rt.ReconcileOrphans(r.ctx, r.known(c), time.Hour); err != nil {
		return err
	}
	if status, err = r.awaitCacheReady(); err != nil {
		return err
	}
	since := time.Now()
	if err := r.recordSandbox(name); err != nil {
		return err
	}
	c.bootToken, c.gen = r.reg.mint(c.token, true)
	spec := r.spec(c, "")
	spec.WorkspaceRecoveredFrom = dir.Bookmark
	loc, err := r.rt.Spawn(r.ctx, spec)
	if err != nil {
		return err
	}
	c.loc, c.last, c.state = &loc, &loc, stateRunning
	if _, err := r.awaitRunning(c, since); err != nil {
		return err
	}
	if err := r.assertCacheFed(c, status); err != nil {
		return err
	}
	log, err := r.initLog(name)
	if err != nil {
		return err
	}
	if want := "workspace-init: " + dir.Dir + " on " + dir.Bookmark + " from " + r.defaultBranch + "\n"; !strings.Contains(log, want) {
		return fmt.Errorf("the recovered pod's init log lacks %q: %s", strings.TrimSpace(want), strings.TrimSpace(log))
	}
	parent, err := r.exec(c, "jj", "-R", dir.Dir, "log", "-r", "@-", "--no-graph", "-T", "commit_id", "--ignore-working-copy")
	if err != nil {
		return err
	}
	if parent != branchHead {
		return fmt.Errorf("the recovered workspace's @- is %q, want %s's commit %s", parent, dir.Bookmark, branchHead)
	}
	note("runtime", "Spawn(%s) with WorkspaceRecoveredFrom %s into a new Sandbox and volume", c.token, dir.Bookmark)
	note("operator", "init log: %s; workspace @- %s = %s", oneLine(log), parent, dir.Bookmark)
	return nil
}

// assertCacheFed is cache-stream's evidence for c's pod: its workspace-fetch log's last line names
// the bytes it streamed from the cache at status.Address and the GitHub delta, under 1 MiB.
func (r *liveRig) assertCacheFed(c *liveClaim, status CacheStatus) error {
	log, err := r.fetchLog(SandboxName(c.token))
	if err != nil {
		return err
	}
	match := cacheStreamLine.FindStringSubmatch(log)
	if match == nil || match[2] != status.Address {
		return fmt.Errorf("%s's workspace-fetch log does not name the stream from the cache at %s: %s", c.name, status.Address, strings.TrimSpace(log))
	}
	streamed, delta := printedCount(match[1]), printedCount(match[3])
	if streamed <= 0 || delta < 0 || delta >= 1<<20 {
		return fmt.Errorf("%s's feed came %d bytes from the cache and %d from GitHub, want some from the cache and under 1 MiB from GitHub: %s", c.name, streamed, delta, match[0])
	}
	note("operator", "%s's workspace-fetch log: %s", c.name, match[0])
	return nil
}

// printedCount is a byte count as workspace-init fetch prints it, digits grouped by commas; -1 when
// it is no such count.
func printedCount(printed string) int64 {
	n, err := strconv.ParseInt(strings.ReplaceAll(printed, ",", ""), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// awaitCacheReady waits for the run's repository's cache to be Ready as the runtime's stores show
// it, which is what an issue pod's launch reads.
func (r *liveRig) awaitCacheReady() (CacheStatus, error) {
	var status CacheStatus
	err := r.poll(liveRunningLimit, "the repository cache of "+r.env.repo.String()+" to be Ready", func() (bool, error) {
		var found bool
		status, found = r.rt.RepositoryCache(r.env.repo)
		return found && status.Ready, nil
	})
	return status, err
}

// fetchLog is the workspace-fetch container's log of the pod name.
func (r *liveRig) fetchLog(name string) (string, error) {
	out, err := r.kubectl("logs", name, "-c", fetchContainer)
	return out, err
}

// github is the run's repository's REST API under the implement App's installation token.
func (r *liveRig) github() (githubrest.Client, error) {
	token, err := r.tokens.Token(r.ctx, r.env.repo.Owner())
	if err != nil {
		return githubrest.Client{}, err
	}
	return githubrest.Client{Token: token, API: githubrest.RepositoryAPI("", r.env.repo)}, nil
}

// githubHead is the commit GitHub names for branch of the run's repository.
func (r *liveRig) githubHead(branch string) (string, error) {
	github, err := r.github()
	if err != nil {
		return "", err
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := github.Get(r.ctx, "/git/ref/heads/"+branchPath(branch), &ref); err != nil {
		return "", fmt.Errorf("read refs/heads/%s of %s: %w", branch, r.env.repo, err)
	}
	if ref.Object.SHA == "" {
		return "", fmt.Errorf("read refs/heads/%s of %s: GitHub named no commit", branch, r.env.repo)
	}
	return ref.Object.SHA, nil
}

// createRunBranch creates branch on the run's repository at the default branch's head, and deletes
// it when the run ends, whatever happens in between: the branch is the run's own.
func (r *liveRig) createRunBranch(branch string) error {
	github, err := r.github()
	if err != nil {
		return err
	}
	r.t.Cleanup(func() {
		if err := r.deleteBranch(branch); err != nil {
			fmt.Printf("teardown: could not delete branch %s of %s: %v\n", branch, r.env.repo, err)
		}
	})
	return ghbranch.Create(r.ctx, github, r.env.repo, branch, r.defaultBranch)
}

// deleteBranch deletes branch of the run's repository; one already gone is no failure.
func (r *liveRig) deleteBranch(branch string) error {
	github, err := r.github()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, github.API+"/git/refs/heads/"+branchPath(branch), nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	switch response.StatusCode {
	case http.StatusNoContent, http.StatusNotFound, http.StatusUnprocessableEntity:
		return nil
	}
	return fmt.Errorf("DELETE refs/heads/%s answered %s: %s", branch, response.Status, strings.TrimSpace(string(body)))
}

// branchPath is branch as the path of its ref: each character a URL path reads as its own syntax
// escaped, each `/` kept.
func branchPath(branch string) string {
	return strings.ReplaceAll(url.PathEscape(branch), "%2F", "/")
}
