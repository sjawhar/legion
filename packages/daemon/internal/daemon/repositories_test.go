package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// installations answers each App's installation on each repository, by "<role> <owner/name>";
// a repository it does not name is installed for both Apps, fail answers an error for every read
// and failing an error for the reads it names. delay is how long each read takes, as a GitHub
// that answers slowly; reads wait it out side by side.
type installations struct {
	mu      sync.Mutex
	missing map[string]bool
	fail    error
	failing map[string]error
	asked   []string
	delay   time.Duration
}

func (i *installations) InstalledOn(ctx context.Context, role appauth.AppRole, repo ghrepo.Repository) (bool, error) {
	if i.delay > 0 {
		select {
		case <-time.After(i.delay):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	key := string(role) + " " + repo.String()
	i.asked = append(i.asked, key)
	if i.fail != nil {
		return false, i.fail
	}
	if err := i.failing[key]; err != nil {
		return false, err
	}
	return !i.missing[key], nil
}

// setFailing makes the reads keys names fail with err, or answer again when err is nil.
func (i *installations) setFailing(err error, keys ...string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.failing == nil {
		i.failing = map[string]error{}
	}
	for _, key := range keys {
		i.failing[key] = err
	}
}

// reads is how many installation reads were asked of key ("<role> <owner/name>").
func (i *installations) reads(key string) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	count := 0
	for _, asked := range i.asked {
		if asked == key {
			count++
		}
	}
	return count
}

// mappingDispatch is a Dispatch whose repository mapping is mappings, whose project listing is
// listed (counted in listings, its next listFailures reads failing), and whose issue detail is
// issues' entry for a key it names, the embedded issue for any other.
type mappingDispatch struct {
	outboxDispatch
	mu           sync.Mutex
	mappings     []dispatch.RepoProject
	listed       []dispatch.IssueSummary
	listings     int
	listFailures int
	issues       map[string]dispatch.Issue
	err          error
}

func (d *mappingDispatch) ListRepoProjects(context.Context) ([]dispatch.RepoProject, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]dispatch.RepoProject{}, d.mappings...), d.err
}

func (d *mappingDispatch) ListIssues(context.Context, string, []string) ([]dispatch.IssueSummary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.listings++
	if d.listFailures > 0 {
		d.listFailures--
		return nil, &dispatch.TransientError{Err: errors.New("Dispatch answered 502 to the listing")}
	}
	return append([]dispatch.IssueSummary{}, d.listed...), nil
}

// listingCount is how many project listings were asked of d.
func (d *mappingDispatch) listingCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.listings
}

func (d *mappingDispatch) GetIssue(ctx context.Context, key string) (dispatch.Issue, error) {
	d.mu.Lock()
	issue, named := d.issues[key]
	d.mu.Unlock()
	if named {
		return issue, nil
	}
	return d.outboxDispatch.GetIssue(ctx, key)
}

func (d *mappingDispatch) set(mappings ...dispatch.RepoProject) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mappings = mappings
}

// The project's repositories are Dispatch's mapping of the project, each served only once both
// Apps are installed on it: a repository missing an App is left out with the App named, the
// owners are the served repositories', lowercased and sorted, and a served repository is held as
// GitHub names it, at its default branch.
func TestTheMappingServesOnlyRepositoriesBothAppsAreInstalledOn(t *testing.T) {
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "Acme/Widgets@release/v7.1", "zeta/tools": "zeta/tools@main"})
	repos := newRepositories("LEGION")
	installs := &installations{missing: map[string]bool{"review acme/gadgets": true}}
	client := &mappingDispatch{mappings: []dispatch.RepoProject{
		{Repo: "acme/widgets", Project: "LEGION", Default: true},
		{Repo: "acme/gadgets", Project: "LEGION"},
		{Repo: "zeta/tools", Project: "LEGION"},
		{Repo: "other/thing", Project: "OTHER", Default: true},
	}}
	branches := newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now)

	accepted, refused, err := readMapping(context.Background(), client, "LEGION", installs)
	if err != nil {
		t.Fatalf("readMapping: %v", err)
	}
	if accepted, err = nameAccepted(context.Background(), accepted, branches); err != nil {
		t.Fatalf("nameAccepted: %v", err)
	}
	repos.set(accepted, refused)
	if got, want := repos.Owners(), []string{"acme", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Owners = %v, want %v", got, want)
	}
	if got := repos.Accepted(); len(got) != 2 || got[0] != ghrepo.MustParse("Acme/Widgets") || got[1] != ghrepo.MustParse("zeta/tools") {
		t.Fatalf("Accepted = %v, want Acme/Widgets and zeta/tools", got)
	}
	if got, want := repos.Refused(), []refusedRepository{{Repo: "acme/gadgets", Reason: "the review App is not installed on acme/gadgets"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Refused = %#v, want %#v", got, want)
	}
	if mapped := repos.Mapped(); len(mapped) != 2 || !mapped[0].Default || mapped[1].Default {
		t.Fatalf("Mapped = %#v, want Acme/Widgets the default and zeta/tools not", mapped)
	}

	for _, tc := range []struct {
		resolved string
		want     workflow.Resolution
	}{
		{"acme/widgets", workflow.Resolution{Repo: "Acme/Widgets", DefaultBranch: "release/v7.1", GitHubRepo: "Acme/Widgets"}},
		{"Acme/Widgets", workflow.Resolution{Repo: "Acme/Widgets", DefaultBranch: "release/v7.1", GitHubRepo: "Acme/Widgets"}},
		{"acme/gadgets", workflow.Resolution{Refusal: "the review App is not installed on acme/gadgets", LeftOut: true}},
		{"other/thing", workflow.Resolution{Refusal: "other/thing is not mapped to project LEGION in Dispatch"}},
	} {
		got, err := (&workflowRuntime{repositories: repos, defaultBranches: branches}).ResolveRepository(tc.resolved)
		if err != nil || got != tc.want {
			t.Errorf("ResolveRepository(%s) = %#v, %v; want %#v", tc.resolved, got, err, tc.want)
		}
	}
}

// A repository both Apps lack names both. An installation read that fails, transient or not, is no
// answer: it leaves only its own repository out for now, naming the App, the repository and the
// failure, and marked as a failed read so it is read again, and the rest of the mapping is checked
// as it stands.
func TestTheMappingNamesEveryMissingAppAndLeavesOutOnlyARepositoryWhoseReadFailed(t *testing.T) {
	client := &mappingDispatch{mappings: []dispatch.RepoProject{{Repo: "acme/gadgets", Project: "LEGION"}}}
	installs := &installations{missing: map[string]bool{"implement acme/gadgets": true, "review acme/gadgets": true}}
	accepted, refused, err := readMapping(context.Background(), client, "LEGION", installs)
	if err != nil || len(accepted) != 0 || len(refused) != 1 || refused[0].Reason != "the implement and review Apps are not installed on acme/gadgets" || refused[0].readErr != nil {
		t.Fatalf("readMapping = %#v, %#v, %v; want acme/gadgets left out naming both Apps", accepted, refused, err)
	}

	for _, failure := range []error{&appauth.TransientError{Err: errors.New("GitHub answered 502")}, errors.New("GitHub answered 403")} {
		client := &mappingDispatch{mappings: []dispatch.RepoProject{{Repo: "acme/widgets", Project: "LEGION", Default: true}, {Repo: "acme/gadgets", Project: "LEGION"}}}
		installs := &installations{}
		installs.setFailing(failure, "review acme/gadgets")
		accepted, refused, err := readMapping(context.Background(), client, "LEGION", installs)
		if err != nil {
			t.Fatalf("readMapping with %v on acme/gadgets = %v, want acme/gadgets left out and the rest checked", failure, err)
		}
		if len(accepted) != 1 || accepted[0].Repo != ghrepo.MustParse("acme/widgets") {
			t.Fatalf("accepted = %#v, want acme/widgets", accepted)
		}
		want := "the review App's installation on acme/gadgets could not be read: " + failure.Error() + "; Legion reads it again"
		if len(refused) != 1 || refused[0].Repo != "acme/gadgets" || refused[0].Reason != want || !errors.Is(refused[0].readErr, failure) {
			t.Fatalf("refused = %#v, want acme/gadgets refused for now with %q", refused, want)
		}
	}

	// An App GitHub answered is missing leaves the repository out as today, whatever the other
	// App's read says: no read again of its own.
	installs = &installations{missing: map[string]bool{"review acme/gadgets": true}}
	installs.setFailing(errors.New("GitHub answered 502"), "implement acme/gadgets")
	_, refused, err = readMapping(context.Background(), client, "LEGION", installs)
	want := "the review App is not installed on acme/gadgets; the implement App's installation on acme/gadgets could not be read: GitHub answered 502"
	if err != nil || len(refused) != 1 || refused[0].Reason != want || refused[0].readErr != nil {
		t.Fatalf("readMapping = %#v, %v; want acme/gadgets refused with %q and no read again", refused, err, want)
	}
}

// checkMapping bounds each installation read by installReadAttempt and nothing else: twenty slow
// reads, more than two rounds of mappingReads, are each answered and every repository served,
// however long the whole check takes, and a read longer than installReadAttempt is a failed read,
// refused for now. This guards checkMapping's own bound, which the code before this check already
// had; the removed bound on a refresh as a whole, which cut such a check off, is
// TestASlowButAnsweringGitHubIsServedOnARefresh's.
func TestEachInstallationReadIsBoundedOnItsOwn(t *testing.T) {
	read := installReadAttempt
	t.Cleanup(func() { installReadAttempt = read })
	var mappings []dispatch.RepoProject
	for i := range 10 {
		mappings = append(mappings, dispatch.RepoProject{Repo: fmt.Sprintf("acme/repo-%d", i), Project: "LEGION", Default: i == 0})
	}
	installReadAttempt = 400 * time.Millisecond
	accepted, refused, err := checkMapping(context.Background(), mappings, "LEGION", &installations{delay: 150 * time.Millisecond})
	if err != nil || len(accepted) != 10 || len(refused) != 0 {
		t.Fatalf("checkMapping of 20 slow reads = %d accepted, %#v, %v; want all 10 served", len(accepted), refused, err)
	}

	installReadAttempt = 50 * time.Millisecond
	accepted, refused, err = checkMapping(context.Background(), mappings[:1], "LEGION", &installations{delay: 150 * time.Millisecond})
	if err != nil || len(accepted) != 0 || len(refused) != 1 || !errors.Is(refused[0].readErr, context.DeadlineExceeded) {
		t.Fatalf("checkMapping of a read longer than installReadAttempt = %#v, %#v, %v; want it refused for now as a failed read", accepted, refused, err)
	}
}

// The boot refuses only a project with no repository mapped. A mapping whose every repository
// lacks an App boots serving none, each left out and reported; a mapping with one served
// repository and one left out boots serving the one.
func TestTheBootRefusesOnlyAnEmptyMapping(t *testing.T) {
	for _, tc := range []struct {
		name            string
		mappings        []dispatch.RepoProject
		missing         map[string]bool
		want            string
		served, leftOut int
	}{
		{name: "no repository mapped", mappings: []dispatch.RepoProject{{Repo: "other/thing", Project: "OTHER"}},
			want: "project LEGION has no repository mapped in Dispatch (Settings → Repositories → Projects)"},
		{name: "no repository with both Apps", mappings: []dispatch.RepoProject{{Repo: "acme/gadgets", Project: "LEGION"}},
			missing: map[string]bool{"review acme/gadgets": true}, leftOut: 1},
		{name: "one served, one left out", mappings: []dispatch.RepoProject{{Repo: "acme/widgets", Project: "LEGION"}, {Repo: "acme/gadgets", Project: "LEGION"}},
			missing: map[string]bool{"review acme/gadgets": true}, served: 1, leftOut: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &workflowRuntime{dispatch: &mappingDispatch{mappings: tc.mappings}, dispatchProject: "LEGION", installs: &installations{missing: tc.missing},
				repositories: newRepositories("LEGION"), log: quietLogger()}
			err := w.loadMapping(context.Background())
			if tc.want != "" {
				if err == nil || err.Error() != tc.want {
					t.Fatalf("loadMapping = %v, want %q", err, tc.want)
				}
				return
			}
			if err != nil || len(w.repositories.Accepted()) != tc.served || len(w.repositories.Refused()) != tc.leftOut {
				t.Fatalf("loadMapping = %v, served %v, left out %v; want it to boot serving %d with %d left out",
					err, w.repositories.Accepted(), w.repositories.Refused(), tc.served, tc.leftOut)
			}
		})
	}
}

// unreadyDispatch is a Dispatch with no ready endpoint for its first refusals reads of the
// repository mapping: each is refused at the dial, as a Service with no ready endpoint refuses it,
// and reported as the real client reports a refused dial (a TransientError around it). Every read
// after them answers the mapping.
type unreadyDispatch struct {
	mappingDispatch
	refusals int64
	reads    atomic.Int64
}

func (d *unreadyDispatch) ListRepoProjects(ctx context.Context) ([]dispatch.RepoProject, error) {
	if d.reads.Add(1) <= d.refusals {
		return nil, &dispatch.TransientError{Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}}
	}
	return d.mappingDispatch.ListRepoProjects(ctx)
}

// A Dispatch with no ready endpoint refuses every dial for as long as it has none. The boot's read
// of the mapping waits that out without limit (readinessRetry), logging each wait, as the boot's
// listing for admission does: bounded at appMintRetry's attempts, it exited the daemon, whose
// supervisor restarted it straight back into the same refusal. Here Dispatch refuses two reads more
// than appMintRetry's budget allows and then answers, and the boot serves the mapped repository.
// GitHub's installation reads never refuse the boot (TestABootServesTheRepositoriesWhoseInstallationReadsAnswer).
func TestTheBootWaitsForTheMappingWhileDispatchRefusesEveryDial(t *testing.T) {
	quickAppMints(t)
	fastReadiness(t, bootprobe.Retry{Initial: time.Millisecond, Max: 4 * time.Millisecond})
	mapped := []dispatch.RepoProject{{Repo: "acme/widgets", Project: "LEGION", Default: true}}
	refusals := appMintRetry.Attempts + 2
	client := &unreadyDispatch{mappingDispatch: mappingDispatch{mappings: mapped}, refusals: int64(refusals)}
	var logged bytes.Buffer
	w := &workflowRuntime{dispatch: client, dispatchProject: "LEGION", installs: &installations{},
		repositories: newRepositories("LEGION"), log: slog.New(slog.NewTextHandler(&logged, nil))}
	if err := w.loadMapping(context.Background()); err != nil {
		t.Fatalf("loadMapping = %v, want it to wait out %d refused dials and serve acme/widgets", err, refusals)
	}
	if got := w.repositories.Accepted(); len(got) != 1 || got[0] != ghrepo.MustParse("acme/widgets") {
		t.Fatalf("Accepted = %v, want acme/widgets", got)
	}
	if got, want := client.reads.Load(), int64(refusals)+1; got != want {
		t.Fatalf("Dispatch saw %d mapping reads, want %d: the %d refused and the one that answered", got, want, refusals)
	}
	if got := strings.Count(logged.String(), `probe="repository mapping read"`); got != refusals {
		t.Fatalf("the boot logged %d waits on the mapping read, want one per refused dial (%d):\n%s", got, refusals, logged.String())
	}
	if !strings.Contains(logged.String(), "connect: connection refused") {
		t.Fatalf("the waits do not name the refused dial:\n%s", logged.String())
	}
}

// A boot whose installation read fails for one repository, transient or not, serves the
// repositories GitHub answered for and leaves that one out for now, naming the App, the repository
// and the failure, where it refused the whole boot and its supervisor restarted it straight back
// into GitHub's same failure. The state shows the refusal.
func TestABootServesTheRepositoriesWhoseInstallationReadsAnswer(t *testing.T) {
	for _, failure := range []error{&appauth.TransientError{Err: errors.New("GitHub answered 502")}, errors.New("read whether the implement App is installed on acme/gadgets failed (403)")} {
		installs := &installations{}
		installs.setFailing(failure, "implement acme/gadgets")
		var logged bytes.Buffer
		w := &workflowRuntime{dispatch: &mappingDispatch{mappings: []dispatch.RepoProject{{Repo: "acme/widgets", Project: "LEGION", Default: true}, {Repo: "acme/gadgets", Project: "LEGION"}}},
			dispatchProject: "LEGION", installs: installs, repositories: newRepositories("LEGION"), log: slog.New(slog.NewTextHandler(&logged, nil))}
		if err := w.loadMapping(context.Background()); err != nil {
			t.Fatalf("loadMapping with %v on acme/gadgets = %v, want the boot to go on", failure, err)
		}
		if got := w.repositories.Accepted(); len(got) != 1 || got[0] != ghrepo.MustParse("acme/widgets") {
			t.Fatalf("Accepted = %v, want acme/widgets", got)
		}
		view := w.repositories.view(nil)
		want := "the implement App's installation on acme/gadgets could not be read: " + failure.Error() + "; Legion reads it again"
		if len(view.Refused) != 1 || view.Refused[0] != (api.RefusedRepositoryView{Repo: "acme/gadgets", Reason: want}) {
			t.Fatalf("state's refused = %#v, want acme/gadgets with %q", view.Refused, want)
		}
		if !strings.Contains(logged.String(), "level=WARN") || strings.Contains(logged.String(), "level=ERROR") {
			t.Fatalf("boot log = %s, want one warning and no error while acme/widgets is served", logged.String())
		}
	}
}

// A boot whose every repository's installation read fails still comes up, serving none, and logs
// one ERROR line naming the first failure: its API and recovery stay up, and the read again heals it.
func TestABootWhoseEveryInstallationReadFailsServesNoneAndLogsOneError(t *testing.T) {
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 403: Resource not accessible by integration"), "implement acme/widgets", "review acme/widgets", "implement acme/gadgets", "review acme/gadgets")
	var logged bytes.Buffer
	w := &workflowRuntime{dispatch: &mappingDispatch{mappings: []dispatch.RepoProject{{Repo: "acme/widgets", Project: "LEGION", Default: true}, {Repo: "acme/gadgets", Project: "LEGION"}}},
		dispatchProject: "LEGION", installs: installs, repositories: newRepositories("LEGION"), log: slog.New(slog.NewTextHandler(&logged, nil))}
	if err := w.loadMapping(context.Background()); err != nil {
		t.Fatalf("loadMapping with every installation read failing = %v, want a boot serving none", err)
	}
	if got := w.repositories.Accepted(); len(got) != 0 {
		t.Fatalf("Accepted = %v, want none", got)
	}
	if got := readFailures(w.repositories.Refused()); len(got) != 2 {
		t.Fatalf("refused for a failed read = %#v, want both repositories", got)
	}
	var errorLines []string
	for line := range strings.SplitSeq(logged.String(), "\n") {
		if strings.Contains(line, "level=ERROR") {
			errorLines = append(errorLines, line)
		}
	}
	if len(errorLines) != 1 || !strings.Contains(errorLines[0], "every mapped repository's GitHub App installation read failed") ||
		!strings.Contains(errorLines[0], "check the implement App's installation on acme/widgets: GitHub answered 403: Resource not accessible by integration") {
		t.Fatalf("ERROR lines = %q, want one naming the first failure (acme/widgets' implement read)", errorLines)
	}
}

// namedGitHub answers GET /repos/{owner}/{repo} for each repository it names, as
// "<GitHub's spelling>@<default branch>", or as "301 <owner>/<name>" for one GitHub renamed, which
// it answers as GitHub does: a 301 to the new name's path.
type namedGitHub struct{ url string }

func newNamedGitHub(t *testing.T, repos map[string]string) *namedGitHub {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/repos/")
		named, ok := repos[strings.ToLower(path)]
		if r.Method != http.MethodGet || !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		if renamed, moved := strings.CutPrefix(named, "301 "); moved {
			http.Redirect(w, r, "/repos/"+renamed, http.StatusMovedPermanently)
			return
		}
		name, branch, _ := strings.Cut(named, "@")
		_, _ = w.Write([]byte(`{"full_name":"` + name + `","default_branch":"` + branch + `"}`))
	}))
	t.Cleanup(server.Close)
	return &namedGitHub{url: server.URL}
}

// The state's mapping carries each mapped repository's cache, matched by repository whatever its
// case, with its last health read; a cache whose repository the mapping no longer names is listed
// apart with when the sweep retires it; a mapped repository with no cache carries none.
func TestTheMappingsViewCarriesTheRepositoryCaches(t *testing.T) {
	repos := newRepositories("LEGION")
	repos.set([]mappedRepository{
		{Repo: ghrepo.MustParse("Acme/Widgets"), Default: true},
		{Repo: ghrepo.MustParse("zeta/tools")},
	}, nil)
	fetched := time.Date(2026, 10, 16, 19, 20, 30, 0, time.UTC)
	retained := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	view := repos.view([]sandbox.CacheStatus{
		{Repo: ghrepo.MustParse("acme/legacy"), Sandbox: "legion-legion-cache-acme-legacy-0a1b2c3d", State: "failing",
			Reason: "container cache ImagePullBackOff: pull access denied (restarted 0 times)", RetainedUntil: retained},
		{Repo: ghrepo.MustParse("acme/widgets"), Sandbox: "legion-legion-cache-acme-widgets-d782c874", State: "ready", Ready: true, Address: "http://legion-legion-cache-acme-widgets-d782c874.legion.svc:8418",
			Health: &repocache.Health{Ready: true, Repo: "acme/widgets", Head: "34a7ac80", LastFetchAt: fetched, Bytes: 6612345678}},
	})
	want := api.RepositoriesView{
		Owners: []string{"acme", "zeta"},
		Mapped: []api.MappedRepositoryView{
			{Repo: "Acme/Widgets", Default: true, Cache: &api.CacheView{
				Repo: "acme/widgets", Sandbox: "legion-legion-cache-acme-widgets-d782c874", State: "ready", Ready: true, Address: "http://legion-legion-cache-acme-widgets-d782c874.legion.svc:8418",
				Head: "34a7ac80", LastFetchAt: fetched, Bytes: 6612345678,
			}},
			{Repo: "zeta/tools"},
		},
		Caches: []api.CacheView{{Repo: "acme/legacy", Sandbox: "legion-legion-cache-acme-legacy-0a1b2c3d", State: "failing", Reason: "container cache ImagePullBackOff: pull access denied (restarted 0 times)", RetainedUntil: retained}},
	}
	if !reflect.DeepEqual(view, want) {
		t.Fatalf("view = %+v\nwant %+v", view, want)
	}
}

// An issue the workflow does not record, such as an operator's claim, works the project's default
// repository, as Dispatch resolves an issue that names none, at its default branch as GitHub names
// it: a pod provisions its workspace from it, and the claim's commit identity is minted for its
// owner. A project serving no default repository refuses the launch, naming the fix.
func TestAnUnrecordedIssueWorksTheProjectsDefaultRepository(t *testing.T) {
	ctx := context.Background()
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "Acme/Widgets@release/v7.1", "zeta/tools": "zeta/tools@main"})
	w := &workflowRuntime{pool: isolatedOutboxPool(t), records: record.NewStore(), dispatchProject: "LEGION", repositories: newRepositories("LEGION"),
		defaultBranches: newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now)}
	w.repositories.set([]mappedRepository{{Repo: ghrepo.MustParse("acme/widgets"), Default: true}, {Repo: ghrepo.MustParse("zeta/tools")}}, nil)
	if repo, branch, err := w.issueRepository(ctx, "LEGION-9"); err != nil || repo.String() != "Acme/Widgets" || branch != "release/v7.1" {
		t.Fatalf("an unrecorded issue works %s@%s, %v; want the default Acme/Widgets@release/v7.1", repo, branch, err)
	}
	w.repositories.set([]mappedRepository{{Repo: ghrepo.MustParse("zeta/tools")}}, []refusedRepository{{Repo: "acme/widgets", Reason: "the review App is not installed on acme/widgets"}})
	want := "LEGION-9 is not recorded, so it works the project's default repository, and project LEGION serves none: set the project's default repository in Dispatch (Settings → Repositories → Projects) to one both GitHub Apps are installed on"
	if repo, _, err := w.issueRepository(ctx, "LEGION-9"); err == nil || err.Error() != want {
		t.Fatalf("an unrecorded issue with no default served works %s, %v; want %q", repo, err, want)
	}
}

// A tmux pane adopted after a restart holds its write token for the owner of the repository its
// claim's issue works, the owner its launch was minted for: its record's repository's, or, for an
// issue the workflow does not record, the project's default's. A token no claim holds is refused.
func TestAnAdoptedPanesWriteOwnerIsTheOwnerItsIssueWorks(t *testing.T) {
	ctx := context.Background()
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "Acme/Widgets@main"})
	w := &workflowRuntime{pool: isolatedOutboxPool(t), records: record.NewStore(), dispatchProject: "LEGION", repositories: newRepositories("LEGION"),
		defaultBranches: newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now)}
	w.repositories.set([]mappedRepository{{Repo: ghrepo.MustParse("acme/widgets"), Default: true}}, nil)
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		return w.records.PutIssue(ctx, tx, record.Issue{Key: "LEGION-2", Tree: "LEGION-2", Project: "LEGION", Title: "root", Phase: phase.Implementing,
			Generation: 1, Status: "in_progress", Rank: "A", Repo: "zeta/tools", DefaultBranch: "main", GitHubRepo: "zeta/tools"})
	}); err != nil {
		t.Fatal(err)
	}
	issues := map[claim.Token]string{}
	for _, issue := range []string{"LEGION-2", "LEGION-9"} {
		token, err := claim.NewToken("legion", issue, claim.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		issues[token] = issue
	}
	issueOf := func(_ context.Context, token claim.Token) (string, error) {
		issue, ok := issues[token]
		if !ok {
			return "", errors.New("no claim holds token " + string(token))
		}
		return issue, nil
	}
	owner := claimRepositoryOwner(issueOf, w.issueRepository)
	want := map[string]string{"LEGION-2": "zeta", "LEGION-9": "Acme"}
	for token, issue := range issues {
		if got, err := owner(ctx, token); err != nil || got != want[issue] {
			t.Errorf("the write owner of %s = %q, %v; want %q", token, got, err, want[issue])
		}
	}
	if got, err := owner(ctx, claim.ControllerToken("legion")); err == nil {
		t.Errorf("the write owner of a token no claim holds = %q, want a refusal", got)
	}
}
