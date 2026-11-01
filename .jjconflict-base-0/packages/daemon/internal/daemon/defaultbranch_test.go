package daemon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// repositoryGitHub is GitHub's REST API answering GET /repos/acme/widgets with defaultBranch, or
// with status when it is not 200. It counts its reads and records how each was authorized; release,
// when set, holds every read until it is closed.
type repositoryGitHub struct {
	url           string
	reads         atomic.Int32
	mu            sync.Mutex
	status        int
	defaultBranch string
	fullName      string
	authorized    []string
	release       chan struct{}
}

func newRepositoryGitHub(t *testing.T, defaultBranch string) *repositoryGitHub {
	t.Helper()
	g := &repositoryGitHub{status: http.StatusOK, defaultBranch: defaultBranch}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/widgets" {
			t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		g.reads.Add(1)
		g.mu.Lock()
		g.authorized = append(g.authorized, r.Header.Get("Authorization"))
		status, branch, release, fullName := g.status, g.defaultBranch, g.release, g.fullName
		g.mu.Unlock()
		if release != nil {
			<-release
		}
		w.WriteHeader(status)
		if status != http.StatusOK {
			_, _ = io.WriteString(w, `{"message":"Server Error"}`)
			return
		}
		if fullName == "" {
			fullName = "acme/widgets"
		}
		_, _ = io.WriteString(w, `{"full_name":"`+fullName+`","default_branch":"`+branch+`"}`)
	}))
	t.Cleanup(server.Close)
	g.url = server.URL
	return g
}

func (g *repositoryGitHub) answer(status int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.status = status
}

// A repository's default branch is read from GitHub as the implement App, for the repository's
// owner, once per TTL: a second read within it asks GitHub nothing, and one after it reads again,
// seeing a branch GitHub renamed meanwhile.
func TestDefaultBranchesReadsGitHubOncePerTTL(t *testing.T) {
	github := newRepositoryGitHub(t, "release/v7.1")
	clock := time.Now()
	branches := newDefaultBranches(roleTokens{}, github.url, 10*time.Minute, func() time.Time { return clock })
	widgets := ghrepo.MustParse("acme/widgets")

	for range 2 {
		if got, err := branches.Named(context.Background(), widgets); err != nil || got.DefaultBranch != "release/v7.1" {
			t.Fatalf("Named = %+v, %v; want release/v7.1", got, err)
		}
	}
	if reads := github.reads.Load(); reads != 1 {
		t.Fatalf("two reads within the TTL asked GitHub %d times, want once", reads)
	}
	github.mu.Lock()
	github.defaultBranch = "trunk"
	github.mu.Unlock()
	clock = clock.Add(10 * time.Minute)
	if got, err := branches.Named(context.Background(), widgets); err != nil || got.DefaultBranch != "trunk" {
		t.Fatalf("Named after the TTL = %+v, %v; want GitHub's trunk", got, err)
	}
	if reads := github.reads.Load(); reads != 2 {
		t.Fatalf("a read after the TTL left GitHub asked %d times, want twice", reads)
	}
	for _, authorization := range github.authorized {
		if authorization != "Bearer implement-app-token-for-acme" {
			t.Fatalf("a read was authorized as %q, want the implement App's token for acme", authorization)
		}
	}
}

// A read GitHub refuses is returned, naming the repository, and nothing is kept: the next read asks
// GitHub again.
func TestDefaultBranchesKeepsNoRefusal(t *testing.T) {
	github := newRepositoryGitHub(t, "main")
	github.answer(http.StatusInternalServerError)
	branches := newDefaultBranches(roleTokens{}, github.url, 10*time.Minute, time.Now)
	widgets := ghrepo.MustParse("acme/widgets")

	if _, err := branches.Named(context.Background(), widgets); err == nil || !strings.Contains(err.Error(), "read the default branch of acme/widgets") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("Named on a 500 = %v, want GitHub's answer naming the repository", err)
	}
	github.answer(http.StatusOK)
	if got, err := branches.Named(context.Background(), widgets); err != nil || got.DefaultBranch != "main" {
		t.Fatalf("Named after the 500 = %+v, %v; want main", got, err)
	}
	if reads := github.reads.Load(); reads != 2 {
		t.Fatalf("GitHub was asked %d times, want twice: the refusal is not kept", reads)
	}
}

// Named is a repository as GitHub names it, read with its default branch and kept the same TTL,
// whatever spelling it was asked by: Dispatch's mapping is lowercased, and GitHub's events carry
// GitHub's spelling. Kept is the last read the daemon made, however old, and asks GitHub nothing:
// what the workflow records an issue with inside its transaction, once the read was made.
func TestDefaultBranchesNamesTheRepositoryAndKeepsTheLastRead(t *testing.T) {
	github := newRepositoryGitHub(t, "release/v7.1")
	github.fullName = "Acme/Widgets"
	clock := time.Now()
	branches := newDefaultBranches(roleTokens{}, github.url, 10*time.Minute, func() time.Time { return clock })

	if _, ok := branches.Kept("acme/widgets"); ok {
		t.Fatal("Kept answered before any read")
	}
	named, err := branches.Named(context.Background(), ghrepo.MustParse("acme/widgets"))
	if err != nil || named.Name != ghrepo.MustParse("Acme/Widgets") || named.DefaultBranch != "release/v7.1" {
		t.Fatalf("Named = %+v, %v; want Acme/Widgets at release/v7.1", named, err)
	}
	if got, err := branches.Named(context.Background(), ghrepo.MustParse("Acme/Widgets")); err != nil || got != named {
		t.Fatalf("Named in GitHub's spelling = %+v, %v; want the same read %+v", got, err, named)
	}
	if reads := github.reads.Load(); reads != 1 {
		t.Fatalf("two spellings of one repository asked GitHub %d times, want once", reads)
	}
	clock = clock.Add(time.Hour)
	if kept, ok := branches.Kept("ACME/widgets"); !ok || kept != named {
		t.Fatalf("Kept an hour later = %+v, %v; want the read %+v", kept, ok, named)
	}
	if reads := github.reads.Load(); reads != 1 {
		t.Fatalf("Kept asked GitHub: %d reads", reads)
	}
}

// Reads that overlap share one request to GitHub.
func TestDefaultBranchesSharesOneReadInFlight(t *testing.T) {
	github := newRepositoryGitHub(t, "develop")
	github.release = make(chan struct{})
	branches := newDefaultBranches(roleTokens{}, github.url, 10*time.Minute, time.Now)
	widgets := ghrepo.MustParse("acme/widgets")

	const readers = 5
	var wg sync.WaitGroup
	answers := make(chan string, readers)
	for range readers {
		wg.Go(func() {
			got, err := branches.Named(context.Background(), widgets)
			if err != nil {
				t.Errorf("Named: %v", err)
			}
			answers <- got.DefaultBranch
		})
	}
	deadline := time.After(10 * time.Second)
	for github.reads.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("no read reached GitHub")
		case <-time.After(time.Millisecond):
		}
	}
	close(github.release)
	wg.Wait()
	close(answers)
	for got := range answers {
		if got != "develop" {
			t.Errorf("a reader got %q, want develop", got)
		}
	}
	if reads := github.reads.Load(); reads != 1 {
		t.Fatalf("%d overlapping reads asked GitHub %d times, want once", readers, reads)
	}
}
