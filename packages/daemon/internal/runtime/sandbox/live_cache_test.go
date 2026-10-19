//go:build e2e

// The Stage 4a harness's repository cache checks: the cache the run's first issue pod made at
// root-ready serves its repository only with its token, a second pod fills its feed from it alone,
// asking GitHub only for the delta, and an issue whose volume is lost is provisioned again through
// the cache onto a commit of its pushed branch that neither the default branch nor the cache holds.
// The rig is live_test.go.

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

	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// cacheStreamLine is the fetch container's last line when it filled the feed from the repository
// cache: the tar's bytes, the cache's address, and the bytes and refs GitHub's delta brought.
var cacheStreamLine = regexp.MustCompile(`workspace-init fetch: ([0-9,]+) bytes from the repository cache (http://\S+) in \S+, its history verified in \S+; ([0-9,]+) bytes from GitHub for the delta \(([0-9]+) refs?\)`)

// repository-cache: the cache the root's launch made (root-ready) is Ready as the runtime's stores
// show it, at its Service, which the cluster holds as the controller made it — controlled by the
// cache's Sandbox, selecting the hash of its name, and headless with no external name, the shape
// the launch checked before telling
// the root's pod the cache's address. Asked inside the cache's own pod, its /healthz answers with
// no token, ready with a fetch no older than its refresh and two minutes, at the head GitHub named
// for the repository's default branch at that fetch (headAtFetch), and its /tar refuses a request
// without its token, 401 naming nothing, and answers its own token, which reaches curl on its
// standard input (`-H @-`) from the file the pod mounts, never in a process's argv.
func (r *liveRig) checkRepositoryCache() error {
	name := CacheSandboxName(r.env.project, r.env.repo)
	status, err := r.awaitCacheReady()
	if err != nil {
		return err
	}
	if want := r.rt.cacheURL(name); status.Address != want {
		return fmt.Errorf("the repository cache %s is Ready at %q, want its Service %s", name, status.Address, want)
	}
	service, err := r.kubectl("get", "service", name, "-o", `jsonpath={.metadata.ownerReferences[?(@.controller==true)].kind}/{.metadata.ownerReferences[?(@.controller==true)].name} {.spec.selector} clusterIP={.spec.clusterIP} externalName={.spec.externalName}`)
	if err != nil {
		return err
	}
	if want := "Sandbox/" + name + ` {"` + sandboxNameHashLabel + `":"` + sandboxNameHash(name) + `"} clusterIP=None externalName=`; strings.TrimSpace(service) != want {
		return fmt.Errorf("the cluster holds the cache's Service as %q, want %q", strings.TrimSpace(service), want)
	}
	health, body, err := r.cacheHealth(name)
	if err != nil {
		return err
	}
	curl := fmt.Sprintf(`curl -s -w ' %%{http_code}' localhost:%d`, repocache.Port)
	head, err := r.githubHead(r.defaultBranch)
	if err != nil {
		return err
	}
	if !health.Ready || health.Repo != r.env.repo.String() || health.Bytes <= 0 || health.LastFetchAt.IsZero() {
		return fmt.Errorf("the cache's /healthz answered %+v, want ready for %s", health, r.env.repo)
	}
	if refresh := liveRepositoryCache().Refresh; time.Since(health.LastFetchAt) > refresh+2*time.Minute {
		return fmt.Errorf("the cache's last fetch was at %s, more than its %s refresh and two minutes ago", health.LastFetchAt.Format(time.RFC3339), refresh)
	}
	if err := r.headAtFetch(r.defaultBranch, head, health); err != nil {
		return err
	}
	tar := strings.Replace(curl, "curl -s", "curl -s -o /dev/null", 1) + "/tar"
	refused, err := r.kubectl("exec", name, "-c", cacheContainer, "--", "sh", "-c", strings.Replace(curl, "curl -s", "curl -s -D -", 1)+"/tar")
	if err != nil {
		return err
	}
	if !strings.HasSuffix(strings.TrimSpace(refused), " 401") || strings.Contains(refused, r.env.repo.String()) {
		return fmt.Errorf("the cache's /tar without its token answered %q, want 401 naming nothing", strings.TrimSpace(refused))
	}
	withToken := `{ printf 'Authorization: Bearer '; tr -d '\r\n' <"$` + repocache.CacheTokenFileEnv + `"; echo; } | ` + strings.Replace(tar, "curl -s", "curl -s -H @-", 1)
	served, err := r.kubectl("exec", name, "-c", cacheContainer, "--", "sh", "-c", withToken)
	if err != nil {
		return err
	}
	if strings.TrimSpace(served) != "200" {
		return fmt.Errorf("the cache's /tar with its token answered %q, want 200", strings.TrimSpace(served))
	}
	note("runtime", "RepositoryCache(%s): %s at %s; its Service %s", r.env.repo, status.State, status.Address, strings.TrimSpace(service))
	note("operator", "exec %s -c %s -- curl localhost:%d: /healthz with no token %s; /tar 401 without its token, %s with it on stdin; GitHub's %s is %s", name, cacheContainer, repocache.Port, body, strings.TrimSpace(served), r.defaultBranch, head)
	return nil
}

// cacheHealth is the cache name's /healthz, asked inside its own pod with no token, decoded, with
// the body as it answered: an error unless it answered 200 and its JSON.
func (r *liveRig) cacheHealth(name string) (repocache.Health, string, error) {
	out, err := r.kubectl("exec", name, "-c", cacheContainer, "--", "sh", "-c", fmt.Sprintf(`curl -s -w ' %%{http_code}' localhost:%d/healthz`, repocache.Port))
	if err != nil {
		return repocache.Health{}, "", err
	}
	body, code, _ := strings.Cut(strings.TrimSpace(out), " ")
	var health repocache.Health
	if err := json.Unmarshal([]byte(body), &health); err != nil || code != "200" {
		return repocache.Health{}, "", fmt.Errorf("the cache %s's /healthz with no token answered %q, not 200 and its JSON: %v", name, strings.TrimSpace(out), err)
	}
	return health, body, nil
}

// cache-second-pod: another issue pod of the run's repository (cache-b), launched while the cache
// is Ready, fills its feed from the cache's tar and fetches from GitHub only the delta, under 1 MiB;
// its clone's objects are the feed's (objects/info/alternates names the feed on the issue volume),
// and git checks the clone's connectivity through them.
func (r *liveRig) checkCacheSecondPod() error {
	if _, err := r.awaitCacheReady(); err != nil {
		return err
	}
	c := r.claim("cache-b")
	if err := r.ensureRunning(c); err != nil {
		return err
	}
	if err := r.assertCacheFed(c); err != nil {
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

// cache-lost-volume: with the cache warm, the harness pushes legion/S4A-9 onto the repository as a
// commit of its own — a file only that commit adds, on top of the default branch's head — and
// checks inside the cache's pod that its clone lacks that commit (deleted when the harness ends).
// cache-b's Sandbox and volume are then deleted while no runtime runs. Relaunched with the branch as
// its recovery source, the new pod fills its new feed from the cache again, and, while the cache
// still lacks the commit, GitHub's delta brings it (more than no bytes); the workspace is
// provisioned on the branch: its @- is that commit, and the file that commit added is in it.
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
	base, err := r.githubHead(r.defaultBranch)
	if err != nil {
		return err
	}
	health, _, err := r.cacheHealth(status.Sandbox)
	if err != nil {
		return err
	}
	cached := health.Head
	if cached == "" {
		return fmt.Errorf("the cache %s's /healthz names no head: %+v", status.Sandbox, health)
	}
	marker, content := ".legion-e2e/"+c.issue+".txt", fmt.Sprintf("the commit only %s holds, pushed at %s\n", dir.Bookmark, time.Now().UTC().Format(time.RFC3339Nano))
	branchHead, err := r.pushRunCommit(dir.Bookmark, base, marker, content)
	if err != nil {
		return err
	}
	if branchHead == base || branchHead == cached {
		return fmt.Errorf("the pushed %s is %s, which the default branch (%s) or the cache (%s) already holds", dir.Bookmark, branchHead, base, cached)
	}
	if held, err := r.cacheHolds(status.Sandbox, branchHead); err != nil || held {
		return fmt.Errorf("the cache %s holds the just-pushed %s already (%v), so the recovery would prove nothing of GitHub's delta", status.Sandbox, branchHead, err)
	}
	note("operator", "pushed %s on %s at %s, adding %s on the default branch's %s (the cache serves %s and lacks %s); deleted when the run ends", dir.Bookmark, r.env.repo, branchHead, marker, base, cached, branchHead)

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
	if _, err := r.awaitCacheReady(); err != nil {
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
	if err := r.assertCacheFed(c); err != nil {
		return err
	}
	if held, err := r.cacheHolds(status.Sandbox, branchHead); err != nil {
		return err
	} else if !held {
		fetched, err := r.fetchLog(name)
		if err != nil {
			return err
		}
		if match := cacheStreamLine.FindStringSubmatch(fetched); match == nil || printedCount(match[3]) <= 0 {
			return fmt.Errorf("the cache still lacks %s, yet the recovered pod's fetch brought nothing from GitHub: %s", branchHead, strings.TrimSpace(fetched))
		}
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
		return fmt.Errorf("the recovered workspace's @- is %q, want %s's commit %s (the default branch is %s)", parent, dir.Bookmark, branchHead, base)
	}
	held, err := r.exec(c, "cat", dir.Dir+"/"+marker)
	if err != nil || held != strings.TrimSpace(content) {
		return fmt.Errorf("the recovered workspace's %s holds %q (%v), want %q", marker, held, err, strings.TrimSpace(content))
	}
	note("runtime", "Spawn(%s) with WorkspaceRecoveredFrom %s into a new Sandbox and volume", c.token, dir.Bookmark)
	note("operator", "init log: %s; workspace @- %s = %s, not the default branch's %s; %s holds the pushed line", oneLine(log), parent, dir.Bookmark, base, marker)
	return nil
}

// cacheHolds is whether the cache name's clone, asked inside its own pod, holds the object id.
func (r *liveRig) cacheHolds(name, id string) (bool, error) {
	clone, err := workspace.FeedRepository(CacheRoot, r.env.repo)
	if err != nil {
		return false, err
	}
	out, err := r.kubectl("exec", name, "-c", cacheContainer, "--", "sh", "-c", `if git --git-dir="$1" cat-file -e "$2"; then echo held; else echo absent; fi`, "sh", clone, id)
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(out) {
	case "held":
		return true, nil
	case "absent":
		return false, nil
	}
	return false, fmt.Errorf("asking the cache %s whether it holds %s answered %q", name, id, strings.TrimSpace(out))
}

// pushRunCommit puts branch on the run's repository at a new commit on parent that adds path
// holding content — built with GitHub's git data API, so nothing reaches the default branch — and
// deletes the branch when the run ends, whatever happens in between. It returns the commit.
func (r *liveRig) pushRunCommit(branch, parent, path, content string) (string, error) {
	github, err := r.github()
	if err != nil {
		return "", err
	}
	var commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := github.Get(r.ctx, "/git/commits/"+parent, &commit); err != nil {
		return "", fmt.Errorf("read the commit %s: %w", parent, err)
	}
	var tree, created struct {
		SHA string `json:"sha"`
	}
	if err := github.Post(r.ctx, "/git/trees", map[string]any{
		"base_tree": commit.Tree.SHA,
		"tree":      []map[string]string{{"path": path, "mode": "100644", "type": "blob", "content": content}},
	}, &tree); err != nil {
		return "", fmt.Errorf("write the tree adding %s: %w", path, err)
	}
	if err := github.Post(r.ctx, "/git/commits", map[string]any{
		"message": "Stage 4a: " + branch + "'s own commit", "tree": tree.SHA, "parents": []string{parent},
	}, &created); err != nil {
		return "", fmt.Errorf("write the commit adding %s: %w", path, err)
	}
	r.t.Cleanup(func() {
		if err := r.deleteBranch(branch); err != nil {
			fmt.Printf("teardown: could not delete branch %s of %s: %v\n", branch, r.env.repo, err)
		}
	})
	if err := github.Post(r.ctx, "/git/refs", map[string]string{"ref": "refs/heads/" + branch, "sha": created.SHA}, nil); err != nil {
		return "", fmt.Errorf("create refs/heads/%s at %s: %w", branch, created.SHA, err)
	}
	return created.SHA, nil
}

// assertCacheFed is cache-stream's evidence for c's pod: its workspace-fetch log's last line names
// the bytes it streamed from its repository's cache at the address the runtime gave it and the
// GitHub delta, under 1 MiB, and nothing was cloned from GitHub.
func (r *liveRig) assertCacheFed(c *liveClaim) error {
	log, err := r.fetchLog(SandboxName(c.token))
	if err != nil {
		return err
	}
	repo := r.claimRepository(c)
	address := r.rt.cacheURL(CacheSandboxName(r.env.project, repo))
	match := cacheStreamLine.FindStringSubmatch(log)
	if match == nil || match[2] != address || strings.Contains(log, "cloned "+repo.String()+" from GitHub") {
		return fmt.Errorf("%s's workspace-fetch log does not name the stream from the cache at %s alone: %s", c.name, address, strings.TrimSpace(log))
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

// headAtFetch is nil when health's head is the one GitHub named for branch when the cache last
// fetched (health.LastFetchAt, which checkRepositoryCache holds to the cache's refresh): branch's
// head now, or a commit on branch's first-parent line below it whose next commit landed after that
// fetch, less ten seconds, since GitHub writes a commit a moment before it moves the branch to it.
// The run's repository is shared with other live runs, which merge into its default branch, so the
// branch may move between the cache's refresh and this read; the cache takes the move at its next
// refresh.
func (r *liveRig) headAtFetch(branch, now string, health repocache.Health) error {
	if health.Head == now {
		return nil
	}
	github, err := r.github()
	if err != nil {
		return err
	}
	for child, steps := now, 0; steps < 50; steps++ {
		var commit struct {
			Commit struct {
				Committer struct {
					Date time.Time `json:"date"`
				} `json:"committer"`
			} `json:"commit"`
			Parents []struct {
				SHA string `json:"sha"`
			} `json:"parents"`
		}
		if err := github.Get(r.ctx, "/commits/"+child, &commit); err != nil {
			return fmt.Errorf("read commit %s of %s: %w", child, r.env.repo, err)
		}
		if len(commit.Parents) == 0 {
			return fmt.Errorf("the cache serves %s, which is not on %s's first-parent line below its head %s", health.Head, branch, now)
		}
		if commit.Parents[0].SHA == health.Head {
			landed := commit.Commit.Committer.Date
			if landed.Before(health.LastFetchAt.Add(-10 * time.Second)) {
				return fmt.Errorf("the cache serves %s from its fetch at %s, but %s's next commit on %s, %s, landed at %s, more than ten seconds before that fetch", health.Head, health.LastFetchAt, health.Head, branch, child, landed)
			}
			note("operator", "the cache serves %s, %s's head at its fetch at %s; %s has moved to %s since, its next commit %s landing at %s, which the cache's next refresh takes", health.Head, branch, health.LastFetchAt.Format(time.RFC3339), branch, now, child, landed.Format(time.RFC3339))
			return nil
		}
		child = commit.Parents[0].SHA
	}
	return fmt.Errorf("the cache serves %s, which is not within 50 commits below %s's head %s on its first-parent line", health.Head, branch, now)
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
	request.Header.Set("Authorization", "Bearer "+github.Token)
	request.Header.Set("Accept", "application/vnd.github+json")
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
