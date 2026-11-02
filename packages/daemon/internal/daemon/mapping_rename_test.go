package daemon

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// renamedWidgets is the pair a deployment configures on the day its key is deployed, before the
// transfer: acme/widgets, which Dispatch maps and GitHub still holds, and cybermonklabs/widgets,
// the name the repository moves to, which no mapping names and GitHub does not have yet.
var renamedWidgets = ghrepo.MustRename("acme/widgets", "cybermonklabs/widgets")

// A rename configured before the transfer names a repository Dispatch does not map: the boot's
// mapping read answers all the same, the project serves exactly what Dispatch maps, and the owners
// every role's gh bundle holds a directory for are the served repositories' alone — the pair never
// widens the served set, which is Dispatch's mapping and nothing else. What it does change is what
// GitHub intake hears: the consumer's filter subjects carry both names, so an event arrives
// whichever name GitHub sends it under on the day the repository moves, and an issue Dispatch
// resolves to the name it moved to is served by the one the mapping holds, under that one name.
// The boot says once that the pair names a repository this project does not serve. With no pair
// configured none of it happens: nothing is heard under the second name, and an issue resolved to
// it is not mapped to the project at all.
func TestARenameConfiguredBeforeTheTransferIsHeardWhileTheServedSetIsUnchanged(t *testing.T) {
	const unserved = "a configured repository rename names a repository this project does not serve"
	for _, tc := range []struct {
		name    string
		renames ghrepo.Renames
		// heard is what GitHub intake hears, which is also the GitHub consumer's filter subjects.
		heard []string
		// warnings is how often the boot says the pair names a repository it does not serve.
		warnings int
	}{
		{name: "the pair configured", renames: ghrepo.MustRenames(renamedWidgets), warnings: 1,
			heard: []string{"acme/widgets", "cybermonklabs/widgets", "zeta/tools"}},
		{name: "no pair configured", renames: ghrepo.Renames{}, heard: []string{"acme/widgets", "zeta/tools"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged := &syncBuffer{}
			w, _, _, stream := mappingRigWith(t, &installations{}, func(w *workflowRuntime) {
				w.repositories = newRepositories("LEGION", tc.renames)
				w.log = slog.New(slog.NewTextHandler(logged, nil))
			})
			if got := repositoryNames(w.repositories.Accepted()); !slices.Equal(got, []string{"acme/widgets", "zeta/tools"}) {
				t.Fatalf("served %v, want Dispatch's mapping alone: a configured pair widens nothing", got)
			}
			if got := w.repositories.Owners(); !slices.Equal(got, []string{"acme", "zeta"}) {
				t.Fatalf("owners = %v, want the served repositories' alone", got)
			}
			heard, err := w.heardAtBoot(context.Background())
			if err != nil {
				t.Fatalf("heardAtBoot: %v", err)
			}
			if got := repositoryNames(heard); !slices.Equal(got, tc.heard) {
				t.Fatalf("heard %v, want %v", got, tc.heard)
			}
			want := make([]string, 0, len(tc.heard))
			for _, repo := range tc.heard {
				want = append(want, "notifications.github."+strings.ReplaceAll(repo, "/", ".")+".>")
			}
			slices.Sort(want)
			got := githubFilterSubjects(t, stream)
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Fatalf("the GitHub consumer's filter subjects = %v, want %v", got, want)
			}
			resolved, err := w.ResolveRepository("cybermonklabs/widgets")
			if err != nil {
				t.Fatalf("ResolveRepository(cybermonklabs/widgets): %v", err)
			}
			switch {
			case tc.renames.IsZero():
				if resolved.Refusal != "cybermonklabs/widgets is not mapped to project LEGION in Dispatch" {
					t.Fatalf("ResolveRepository(cybermonklabs/widgets) = %#v, want it refused as a repository the project does not map", resolved)
				}
			case resolved.Repo != "acme/widgets" || resolved.GitHubRepo != "acme/widgets" || resolved.DefaultBranch != "main":
				t.Fatalf("ResolveRepository(cybermonklabs/widgets) = %#v, want acme/widgets@main under one name", resolved)
			}
			if got := strings.Count(logged.String(), unserved); got != tc.warnings {
				t.Fatalf("the boot says %d times that the pair names a repository it does not serve, want %d:\n%s", got, tc.warnings, logged.String())
			}
			if tc.warnings > 0 && !strings.Contains(logged.String(), "from=acme/widgets to=cybermonklabs/widgets project=LEGION") {
				t.Fatalf("the boot's line does not name the pair and the project:\n%s", logged.String())
			}
		})
	}
}

// What GitHub intake hears is spelled as GitHub spells it, whatever case the pair is configured
// in: NATS matches a subject case-sensitively and Envoy publishes under GitHub's own spelling, so
// a pair spelled CyberMonkLabs/legion against GitHub's cybermonklabs/legion would subscribe to
// subjects nothing publishes. A partner is therefore heard only where nothing else names it: the
// served repository keeps the spelling GitHub gave the mapping (nameAccepted), a record keeps the
// spelling it was written with (record.Issue.GitHubRepo), and the pair's own spelling is used for
// the side nothing else names, which is how a pair configured before the transfer is heard.
func TestHeardRepositoriesKeepGitHubsSpellingOverThePairs(t *testing.T) {
	ctx := context.Background()
	// The pair is written in the deployment's own case, which is neither name as GitHub spells it.
	pair := ghrepo.MustRenames(ghrepo.MustRename("SJAWHAR/Legion", "CyberMonkLabs/legion"))
	for _, tc := range []struct {
		name string
		// recorded is GitHub's spelling a record of the project keeps, none when "".
		recorded string
		heard    []string
	}{
		{name: "both names are heard already", recorded: "sjawhar/legion",
			heard: []string{"cybermonklabs/legion", "sjawhar/legion"}},
		{name: "the partner nothing names is heard as the pair spells it",
			heard: []string{"cybermonklabs/legion", "SJAWHAR/Legion"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			if tc.recorded != "" {
				if _, err := pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, default_branch, github_repo)
					values ('LEGION-1', 'LEGION-1', 'LEGION', 'root', 'implementing', 1, 'in_progress', 'A', 1, $1, 'main', $1)`, tc.recorded); err != nil {
					t.Fatal(err)
				}
			}
			w := &workflowRuntime{pool: pool, records: projectRecords{Store: record.NewStore(), project: "LEGION"},
				repositories: newRepositories("LEGION", pair), log: quietLogger()}
			// The served repository as GitHub names it, which nameAccepted reads and Envoy's
			// subjects carry.
			w.repositories.set([]mappedRepository{{Repo: ghrepo.MustParse("cybermonklabs/legion")}}, nil)
			heard, err := w.heardAtBoot(ctx)
			if err != nil {
				t.Fatalf("heardAtBoot: %v", err)
			}
			if got := repositoryNames(heard); !slices.Equal(got, tc.heard) {
				t.Fatalf("heard %v, want %v", got, tc.heard)
			}
		})
	}
}
