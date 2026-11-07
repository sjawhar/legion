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

// The twelve cases of the resolution: the six states a project's mapping can be in over one
// configured pair, each in the two owner-set states, with what the mapping answers for a record
// naming the repository the pair moved from and for one naming the repository it moved to.
//
// The served set each case is built from is the set the mapping load leaves, written as the
// accepted list that load hands `set`, so the test reads the rule over the mapping the daemon
// really holds rather than over a fixture of its own: M4's two entries are the two names one
// mapping gives, which `set` merges into the one repository both Apps answered for, and M5's two
// entries are two repositories, which it keeps apart. State B is one more repository of the
// owner the pair moved the repository away from, which is the one thing production changed on
// 2026-11-06: it must change no answer here.
//
// Three rules that are not this one pass most of this table, and each case below that one of
// them fails is named where it sits:
//
//   - The owner rule this replaced — resolve while the project owns the pair's to owner and not
//     its from owner — answers M1, M2 and M4 wrong in state B alone, where it leaves a record on
//     sjawhar/legion unresolved. That is the production failure: adding sjawhar/oh-my-pi to the
//     project's mapping made sjawhar an owner of the project, the pair switched off, and 47
//     trees could not push for 42 minutes.
//   - "Resolve whenever the pair's to is served" answers ten of the twelve and fails M5's two
//     rows, where the two names are two repositories and each must keep its own.
//   - A symmetric rule, which answers from either side as ghrepo.Renames.Partner does, fails
//     M3's second row, where it would address the repository the transfer has not yet moved the
//     name away from.
func TestTheMappingResolvesARecordedRepositoryOverTheSixMappingStates(t *testing.T) {
	const recordedOld, recordedNew = "sjawhar/legion", "cybermonklabs/legion"
	pair := ghrepo.MustRenames(ghrepo.MustRename(recordedOld, recordedNew))
	accepted := func(repos ...string) []mappedRepository {
		mapped := make([]mappedRepository, 0, len(repos))
		for i, repo := range repos {
			mapped = append(mapped, mappedRepository{Repo: ghrepo.MustParse(repo), Default: i == 0})
		}
		return mapped
	}
	for _, tc := range []struct {
		name string
		// accepted is the list the mapping load hands set, one entry per repository the mapping
		// names, each under the name both GitHub Apps answered for it (checkMapping).
		accepted []mappedRepository
		// served is the set that leaves, by name: the issue's served-set column.
		served []string
		// old and new are what the mapping answers for a record naming sjawhar/legion and for
		// one naming cybermonklabs/legion.
		old, new string
	}{
		{
			name:     "M1: the mapping names the repository the pair moved to",
			accepted: accepted(recordedNew), served: []string{recordedNew},
			old: recordedNew, new: recordedNew,
		},
		{
			// The mapping names sjawhar/legion, which GitHub answers 404 and cybermonklabs/legion
			// 200: the state right after the transfer, so the mapping load serves it under the
			// name both Apps answered for.
			name:     "M2: the mapping names the repository the pair moved from, which GitHub has transferred",
			accepted: accepted(recordedNew), served: []string{recordedNew},
			old: recordedNew, new: recordedNew,
		},
		{
			// The pair configured before the transfer: GitHub still answers sjawhar/legion, so
			// that is what the project serves and what a record of it is addressed at. A
			// symmetric rule fails the second answer, resolving a record of cybermonklabs/legion
			// back onto the name the repository has not left.
			name:     "M3: the mapping names the repository the pair moved from, which GitHub still holds",
			accepted: accepted(recordedOld), served: []string{recordedOld},
			old: recordedOld, new: recordedNew,
		},
		{
			name:     "M4: the mapping names both, and the load merges them onto the one GitHub holds",
			accepted: accepted(recordedNew, recordedNew), served: []string{recordedNew},
			old: recordedNew, new: recordedNew,
		},
		{
			// Somebody made a repository at the old location, which deletes GitHub's redirect:
			// the two names are two repositories, and each record keeps its own. A rule that
			// resolved whenever the pair's to is served fails the first answer here, addressing
			// the new repository for a record of the old one.
			name:     "M5: the mapping names both and GitHub answers for both",
			accepted: accepted(recordedOld, recordedNew), served: []string{recordedOld, recordedNew},
			old: recordedOld, new: recordedNew,
		},
		{
			name:     "M0: the mapping names neither name of the pair",
			accepted: accepted("cybermonklabs/other"), served: []string{"cybermonklabs/other"},
			old: recordedOld, new: recordedNew,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, state := range []struct {
				name     string
				accepted []mappedRepository
				served   []string
			}{
				{name: "state A: the project serves no other repository of the owner the pair moved from",
					accepted: tc.accepted, served: tc.served},
				{name: "state B: it serves sjawhar/oh-my-pi beside them",
					accepted: append(slices.Clone(tc.accepted), mappedRepository{Repo: ghrepo.MustParse("sjawhar/oh-my-pi")}),
					served:   append(slices.Clone(tc.served), "sjawhar/oh-my-pi")},
			} {
				t.Run(state.name, func(t *testing.T) {
					repos := newRepositories("LEGION", pair)
					repos.set(state.accepted, nil)
					wantServed := slices.Clone(state.served)
					slices.Sort(wantServed)
					if got := repositoryNames(repos.Accepted()); !slices.Equal(got, wantServed) {
						t.Fatalf("the mapping serves %v, want %v: the case's served set is not the one this mapping leaves", got, wantServed)
					}
					for recorded, want := range map[string]string{recordedOld: tc.old, recordedNew: tc.new} {
						if got := repos.serving(ghrepo.MustParse(recorded)); got.String() != want {
							t.Errorf("a record naming %s is served as %s over %v, want %s", recorded, got, state.served, want)
						}
					}
				})
			}
		})
	}
}
