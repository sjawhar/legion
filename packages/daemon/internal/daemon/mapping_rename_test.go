package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
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

// mintingTokens is roleTokens that keeps the owner of every token it is asked for and refuses
// each owner whose installation does not cover the repository, as a transfer leaves GitHub: an
// App is installed on an owner, so the installation holding a moved repository is the owner it
// moved to, and a mint for the owner it left reaches nothing, however the address redirects. A
// source that answered whatever owner it was asked for would hide which owner the boot's read of
// GitHub's spelling (nameAccepted) is made for.
type mintingTokens struct {
	roleTokens
	// installed are the owners whose installation covers the repositories GitHub holds.
	installed []string

	mu     sync.Mutex
	minted []string
}

func (t *mintingTokens) Token(ctx context.Context, role appauth.AppRole, repo ghrepo.Repository) (appauth.Lease, error) {
	owner := repo.Owner()
	t.mu.Lock()
	t.minted = append(t.minted, strings.ToLower(owner))
	t.mu.Unlock()
	if !slices.ContainsFunc(t.installed, func(held string) bool { return strings.EqualFold(held, owner) }) {
		return appauth.Lease{}, fmt.Errorf("no installation of the %s App on %s covers the repository", role, owner)
	}
	return t.roleTokens.Token(ctx, role, repo)
}

// owners is every owner a token was minted for, lowercased, sorted and once each.
func (t *mintingTokens) owners() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	owners := slices.Clone(t.minted)
	slices.Sort(owners)
	return slices.Compact(owners)
}

// servedDefaults is each served repository that is the project's default, by name: the one entry
// carrying it, or none where the project serves no default.
func servedDefaults(repos *repositories) []string {
	var carried []string
	for _, mapped := range repos.Mapped() {
		if mapped.Default {
			carried = append(carried, mapped.Repo.String())
		}
	}
	return carried
}

// The mapping load serves a mapped repository under the name GitHub has it, in each state a
// configured pair leaves a mapping in. Every case drives the boot's own path — loadMapping, which
// reads Dispatch's mapping and both Apps' installations (checkMapping) and hands the result to
// set, then nameMapping, which reads GitHub's spelling of each served repository — and pins what
// the project serves, which served entry is its default, what it refuses, the owners the mapping
// holds, the owner each read of GitHub's spelling was minted for, the installation reads GitHub
// was asked for, and the repository each webhook App gap is recorded against.
//
// The states are a mapping's over one pair, sjawhar/legion -> cybermonklabs/legion:
//
//   - M1, the mapping names the repository the pair moved to: the mapping as it reads once
//     somebody changed it after the transfer.
//   - M2, it names the repository the pair moved from and GitHub answers that neither App is
//     installed there: the state right after the transfer, and the failure this change is about.
//     The load that read the mapped name alone left the entry out as "the implement and review
//     Apps are not installed on sjawhar/legion", because after a transfer no installation of the
//     owner the repository left covers it.
//   - M3, the same mapping with GitHub still answering for the mapped name: the state before the
//     transfer, with the pair configured early. The mapped name answered, so no second
//     installation read is made at all and the project serves the repository GitHub still holds.
//   - M4, the mapping names both names, which are one repository: the two entries are served as
//     one, and that one is the project's default because one of the two is, whichever of them
//     Dispatch lists first.
//   - M5, the mapping names both and GitHub answers for both: somebody made a repository at the
//     old location, which deletes GitHub's redirect, so the two names are two repositories.
//
// The sixth case is M2 with the implement App installed on the mapped name while the review App
// answers that it is not: a transfer takes a repository out of every installation of the owner
// it left, so a mapped name that holds one App and not the other is the operator's own gap
// instead. The pair's other name is read for neither App there, and the entry is left out naming
// the App that is missing, rather than a project being moved onto the repository a pair names
// on the strength of one missing installation.
func TestTheMappingLoadServesEachMappedRepositoryUnderTheNameGitHubHasIt(t *testing.T) {
	const old, moved = "sjawhar/legion", "cybermonklabs/legion"
	// transferred is GitHub after the transfer, which answers the old address with the redirect
	// GitHub's own answers; untransferred is GitHub before it; separate is the pair's two names
	// as two repositories.
	transferred := map[string]string{old: "301 " + moved, moved: moved + "@main"}
	untransferred := map[string]string{old: old + "@main"}
	separate := map[string]string{old: old + "@main", moved: moved + "@main"}
	// reads is both Apps' installation reads of each of repos, sorted as the fake records them,
	// a name given twice being a name read twice.
	reads := func(repos ...string) []string {
		asked := make([]string, 0, 2*len(repos))
		for _, repo := range repos {
			asked = append(asked, "implement "+repo, "review "+repo)
		}
		slices.Sort(asked)
		return asked
	}
	const gapReason = "Dispatch's GitHub App is not installed on " + old + ": GitHub sends Legion none of its events and nothing redelivers them; the pass heals its pull requests every 2 minutes"
	for _, tc := range []struct {
		name string
		// mappings is Dispatch's repository mapping, and missing each installation read GitHub
		// answers not installed, by "<role> <owner/name>".
		mappings []dispatch.RepoProject
		missing  map[string]bool
		// github is what a repository read of each name answers, and installed the owners whose
		// installation a token can be minted on.
		github    map[string]string
		installed []string
		// webhookMissing are the repositories Dispatch answers its webhook App check not
		// installed for, under the mapped name it is asked under.
		webhookMissing map[string]bool
		// asks is every installation read the load makes, sorted, and unread a name no
		// installation read may be made of at all.
		asks   []string
		unread string
		// served is what the project serves, by name; serves the served entry Dispatch maps as
		// the project's default, "" where the project serves none; owners the owner set the
		// mapping holds; refuses the refusal each left-out entry carries, by mapped name.
		served  []string
		serves  string
		owners  []string
		refuses []refusedRepository
		// minted is each owner a read of GitHub's spelling was minted for, and gaps each webhook
		// App gap with the repository it is recorded against.
		minted []string
		gaps   []webhookAppGap
	}{
		{
			name:      "M1: the mapping names the repository the pair moved to",
			mappings:  []dispatch.RepoProject{{Repo: moved, Project: "LEGION", Default: true}},
			github:    transferred,
			installed: []string{"cybermonklabs"},
			asks:      reads(moved),
			unread:    old,
			served:    []string{moved},
			serves:    moved,
			owners:    []string{"cybermonklabs"},
			minted:    []string{"cybermonklabs"},
		},
		{
			name:           "M2: the mapping names the repository the pair moved from, which GitHub has transferred",
			mappings:       []dispatch.RepoProject{{Repo: old, Project: "LEGION", Default: true}},
			missing:        map[string]bool{"implement " + old: true, "review " + old: true},
			github:         transferred,
			installed:      []string{"cybermonklabs"},
			webhookMissing: map[string]bool{old: true},
			asks:           reads(old, moved),
			served:         []string{moved},
			serves:         moved,
			owners:         []string{"cybermonklabs"},
			minted:         []string{"cybermonklabs"},
			gaps:           []webhookAppGap{{Repo: moved, Reason: gapReason}},
		},
		{
			name:      "M3: the mapping names the repository the pair moved from, which GitHub still holds",
			mappings:  []dispatch.RepoProject{{Repo: old, Project: "LEGION", Default: true}},
			github:    untransferred,
			installed: []string{"sjawhar"},
			asks:      reads(old),
			unread:    moved,
			served:    []string{old},
			serves:    old,
			owners:    []string{"sjawhar"},
			minted:    []string{"sjawhar"},
		},
		{
			name:      "M4: the mapping names both, the repository it moved to its default",
			mappings:  []dispatch.RepoProject{{Repo: old, Project: "LEGION"}, {Repo: moved, Project: "LEGION", Default: true}},
			missing:   map[string]bool{"implement " + old: true, "review " + old: true},
			github:    transferred,
			installed: []string{"cybermonklabs"},
			asks:      reads(old, moved, moved),
			served:    []string{moved},
			serves:    moved,
			owners:    []string{"cybermonklabs"},
			minted:    []string{"cybermonklabs"},
		},
		{
			name:      "M4 with the default entry listed first, which the merge keeps all the same",
			mappings:  []dispatch.RepoProject{{Repo: moved, Project: "LEGION", Default: true}, {Repo: old, Project: "LEGION"}},
			missing:   map[string]bool{"implement " + old: true, "review " + old: true},
			github:    transferred,
			installed: []string{"cybermonklabs"},
			asks:      reads(old, moved, moved),
			served:    []string{moved},
			serves:    moved,
			owners:    []string{"cybermonklabs"},
			minted:    []string{"cybermonklabs"},
		},
		{
			name:      "M5: the mapping names both and GitHub answers for both",
			mappings:  []dispatch.RepoProject{{Repo: old, Project: "LEGION", Default: true}, {Repo: moved, Project: "LEGION"}},
			github:    separate,
			installed: []string{"cybermonklabs", "sjawhar"},
			asks:      reads(old, moved),
			served:    []string{moved, old},
			serves:    old,
			owners:    []string{"cybermonklabs", "sjawhar"},
			minted:    []string{"cybermonklabs", "sjawhar"},
		},
		{
			// The mapped name holds the implement App and not the review App, which is the
			// operator's own gap: no read of the pair's other name is made for either App, and
			// the entry is left out naming the review App alone. Nothing is served, so the
			// project has no default and no owner, and no read of GitHub's spelling is minted.
			name:      "M2 with the review App alone answering that it is not installed on the mapped name",
			mappings:  []dispatch.RepoProject{{Repo: old, Project: "LEGION", Default: true}},
			missing:   map[string]bool{"review " + old: true},
			github:    transferred,
			installed: []string{"cybermonklabs"},
			asks:      reads(old),
			unread:    moved,
			refuses:   []refusedRepository{{Repo: old, Reason: "the review App is not installed on " + old, mappedDefault: true}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newNamedGitHub(t, tc.github)
			installs := &installations{missing: tc.missing}
			tokens := &mintingTokens{installed: tc.installed}
			client := &mappingDispatch{mappings: tc.mappings, webhookMissing: tc.webhookMissing}
			repos := newRepositories("LEGION", transferredLegion)
			w := &workflowRuntime{dispatch: client, dispatchProject: "LEGION", installs: installs, repositories: repos,
				defaultBranches: newDefaultBranches(tokens, repos, github.url, defaultBranchTTL, time.Now), log: quietLogger()}
			if err := w.loadMapping(context.Background()); err != nil {
				t.Fatalf("loadMapping: %v", err)
			}
			if err := w.nameMapping(context.Background()); err != nil {
				t.Fatalf("nameMapping: %v; want GitHub's spelling read on the owner whose installation answered", err)
			}
			if got := repositoryNames(repos.Accepted()); !slices.Equal(got, tc.served) {
				t.Errorf("the project serves %v, want %v", got, tc.served)
			}
			wantDefault := []string{tc.serves}
			if tc.serves == "" {
				wantDefault = nil
			}
			if got := servedDefaults(repos); !slices.Equal(got, wantDefault) {
				t.Errorf("the served entries carrying the project's default are %v, want %v", got, wantDefault)
			}
			if got := repos.Refused(); !slices.Equal(got, tc.refuses) {
				t.Errorf("the load refuses %#v, want %#v: an entry is served under the name both Apps answer for, and left out naming the App that is missing", got, tc.refuses)
			}
			if got := repos.Owners(); !slices.Equal(got, tc.owners) {
				t.Errorf("the owner set is %v, want %v: the owner of each served name", got, tc.owners)
			}
			if got := tokens.owners(); !slices.Equal(got, tc.minted) {
				t.Errorf("the read of GitHub's spelling was minted for %v, want %v", got, tc.minted)
			}
			if got := installs.asks(); !slices.Equal(got, tc.asks) {
				t.Errorf("GitHub was asked for the installation reads %v, want %v", got, tc.asks)
			}
			if second := installs.reads("implement "+tc.unread) + installs.reads("review "+tc.unread); tc.unread != "" && second != 0 {
				t.Errorf("the load read %s's installations %d times, want none: the mapped name answered, so no second installation read is made", tc.unread, second)
			}
			if got := repos.WithoutWebhookApp(); !slices.Equal(got, tc.gaps) {
				t.Errorf("the webhook App gaps are %#v, want %#v: a gap is recorded against the name its entry is served under", got, tc.gaps)
			}
			mapped := make([]string, 0, len(tc.mappings))
			for _, entry := range tc.mappings {
				mapped = append(mapped, entry.Repo)
			}
			slices.Sort(mapped)
			if got := client.webhookChecks(); !slices.Equal(got, mapped) {
				t.Errorf("Dispatch was asked the webhook App check for %v, want %v: the mapped name, the only one known while an entry's reads run at once", got, mapped)
			}
		})
	}
}

// A refusal over a configured pair is for now whenever any read of either name failed to answer,
// and for good only once every read of both names answered and at least one said not installed.
// The rule before the partner read, where an App answering not installed settled the entry
// whatever else failed, cannot survive it: after a transfer the mapped name's "not installed" is
// the honest answer, so a transient failure on the name the repository moved to would leave the
// entry out for good behind that reason, and no read again would ever revisit it.
func TestARefusalOverATransferredPairIsForNowWhileAReadDidNotAnswer(t *testing.T) {
	const old, moved = "sjawhar/legion", "cybermonklabs/legion"
	failure := errors.New("GitHub answered 502")
	for _, tc := range []struct {
		name string
		// missing are the reads GitHub answers not installed, failing the reads it does not
		// answer at all, each by "<role> <owner/name>".
		missing map[string]bool
		failing []string
		// reason is the refusal the entry carries, and forNow whether Legion reads it again.
		reason string
		forNow bool
	}{
		{
			name:    "the mapped name answers not installed and the partner's read fails",
			missing: map[string]bool{"implement " + old: true, "review " + old: true},
			failing: []string{"implement " + moved},
			reason: "the implement and review Apps are not installed on " + old +
				"; the implement App's installation on " + moved + " could not be read: " + failure.Error() + "; Legion reads it again",
			forNow: true,
		},
		{
			name:    "both names answer not installed",
			missing: map[string]bool{"implement " + old: true, "review " + old: true, "implement " + moved: true, "review " + moved: true},
			reason:  "the implement and review Apps are not installed on " + old + "; the implement and review Apps are not installed on " + moved,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installs := &installations{missing: tc.missing}
			installs.setFailing(failure, tc.failing...)
			repos := newRepositories("LEGION", transferredLegion)
			w := &workflowRuntime{dispatch: &mappingDispatch{mappings: []dispatch.RepoProject{{Repo: old, Project: "LEGION", Default: true}}},
				dispatchProject: "LEGION", installs: installs, repositories: repos, log: quietLogger()}
			if err := w.loadMapping(context.Background()); err != nil {
				t.Fatalf("loadMapping = %v, want a boot serving none with the entry left out", err)
			}
			if got := repositoryNames(repos.Accepted()); len(got) != 0 {
				t.Fatalf("the project serves %v, want none: neither name answered for both Apps", got)
			}
			refused := repos.Refused()
			if len(refused) != 1 || refused[0].Repo != old || refused[0].Reason != tc.reason {
				t.Fatalf("the load refuses %#v, want %s left out as %q", refused, old, tc.reason)
			}
			if forNow := refused[0].readErr != nil; forNow != tc.forNow {
				t.Fatalf("the refusal of %s carries the read failure %v, want a refusal for now = %t", old, refused[0].readErr, tc.forNow)
			}
			waiting := repos.awaitingRead()
			switch {
			case tc.forNow && !errors.Is(refused[0].readErr, failure):
				t.Fatalf("the refusal's read failure = %v, want the failed read of %s", refused[0].readErr, moved)
			case tc.forNow && !slices.Equal(waiting, []string{tc.reason}):
				t.Fatalf("awaiting a read again = %q, want the entry, which Legion reads again until GitHub answers every read of both names", waiting)
			case !tc.forNow && len(waiting) != 0:
				t.Fatalf("awaiting a read again = %q, want none: every read of both names answered and an App is not installed", waiting)
			}
		})
	}
}

// M4's two entries are one repository, and a refresh whose installation read of one of them fails
// keeps that one on its last answer: refreshMapping's kept branch appends it after the accepted
// list, and set merges the two onto the repository both name. The merge is what keeps the
// project's default here, since the kept entry carries Dispatch's mapping of the name it was
// mapped under, which is not the project's default: a write that let the last entry of the list
// win would clear the default the accepted entry carries, and a project that serves no default
// refuses the launch of an issue that names no repository (issueRepository).
func TestAKeptEntryOfATransferredPairKeepsTheProjectsDefault(t *testing.T) {
	ctx := context.Background()
	const old, moved = "sjawhar/legion", "cybermonklabs/legion"
	client := &mappingDispatch{mappings: []dispatch.RepoProject{{Repo: old, Project: "LEGION"}, {Repo: moved, Project: "LEGION", Default: true}}}
	github := newNamedGitHub(t, map[string]string{old: "301 " + moved, moved: moved + "@main"})
	installs := &installations{missing: map[string]bool{"implement " + old: true, "review " + old: true}}
	w, _, _, _ := mappingRigWith(t, installs, func(w *workflowRuntime) {
		w.dispatch = client
		w.repositories = newRepositories("LEGION", transferredLegion)
		w.defaultBranches = newDefaultBranches(roleTokens{}, w.repositories, github.url, defaultBranchTTL, time.Now)
	})
	if got, carried := repositoryNames(w.repositories.Accepted()), servedDefaults(w.repositories); !slices.Equal(got, []string{moved}) || !slices.Equal(carried, []string{moved}) {
		t.Fatalf("the boot serves %v with %v carrying the project's default, want %s alone, carrying it", got, carried, moved)
	}
	// The refresh answers for one entry and not the other: GitHub says the review App is
	// installed on the name the repository moved away from and does not answer the implement
	// App's read of it at all, so that entry asks for no read of the pair's other name and is
	// left out for now, which is the kept branch.
	installs.setMissing(false, "review "+old)
	failure := &appauth.TransientError{Err: errors.New("GitHub answered 502")}
	installs.setFailing(failure, "implement "+old)
	if err := w.refreshMapping(ctx); err != nil {
		t.Fatalf("refreshMapping: %v", err)
	}
	if got := repositoryNames(w.repositories.Accepted()); !slices.Equal(got, []string{moved}) {
		t.Fatalf("the refresh serves %v, want %s alone: the two entries are one repository", got, moved)
	}
	if got := servedDefaults(w.repositories); !slices.Equal(got, []string{moved}) {
		t.Fatalf("the served entries carrying the project's default are %v, want %s: the kept entry is appended last and must not clear it", got, moved)
	}
	if got := w.repositories.Refused(); len(got) != 0 {
		t.Fatalf("the refresh refuses %q, want none: the entry whose read failed is kept on its last answer", refusalReasons(got))
	}
	want := []string{moved + ": served on its last answer, its installation read having failed"}
	if !w.repositories.kept(old) || !slices.Equal(w.repositories.awaitingRead(), want) {
		t.Fatalf("kept(%s) = %t and awaiting a read again = %q, want the entry kept and read again as %q",
			old, w.repositories.kept(old), w.repositories.awaitingRead(), want)
	}
	if repo, branch, err := w.issueRepository(ctx, "LEGION-9"); err != nil || repo.String() != moved || branch != "main" {
		t.Fatalf("an unrecorded issue works %s@%s, %v; want the project's default, %s@main", repo, branch, err, moved)
	}
}

// A webhook App gap is recorded against the name its entry is served under on the kept path too.
// checkMapping refused the entry, its installation read having failed, so it served no name for
// it and recorded the gap under Dispatch's mapped name, while refreshMapping serves it again
// under the name it was last served under: after a transfer those are two names, so without the
// relabel legion state would name the owner the repository left, for as long as the read fails.
func TestAKeptEntrysWebhookAppGapNamesTheRepositoryItIsServedUnder(t *testing.T) {
	ctx := context.Background()
	const old, moved = "sjawhar/legion", "cybermonklabs/legion"
	// Dispatch is asked the check under the name its own mapping holds, so the reason names the
	// mapped name while the gap is recorded against the served one.
	want := []webhookAppGap{{Repo: moved, Reason: "Dispatch's GitHub App is not installed on " + old + ": GitHub sends Legion none of its events and nothing redelivers them; the pass heals its pull requests every 2 minutes"}}
	client := &mappingDispatch{mappings: []dispatch.RepoProject{{Repo: old, Project: "LEGION", Default: true}}, webhookMissing: map[string]bool{old: true}}
	github := newNamedGitHub(t, map[string]string{old: "301 " + moved, moved: moved + "@main"})
	installs := &installations{missing: map[string]bool{"implement " + old: true, "review " + old: true}}
	w, _, _, _ := mappingRigWith(t, installs, func(w *workflowRuntime) {
		w.dispatch = client
		w.repositories = newRepositories("LEGION", transferredLegion)
		w.defaultBranches = newDefaultBranches(roleTokens{}, w.repositories, github.url, defaultBranchTTL, time.Now)
	})
	if got := w.repositories.WithoutWebhookApp(); !slices.Equal(got, want) {
		t.Fatalf("the boot's webhook App gaps are %#v, want %#v", got, want)
	}
	installs.setMissing(false, "review "+old)
	installs.setFailing(errors.New("GitHub answered 502"), "implement "+old)
	if err := w.refreshMapping(ctx); err != nil {
		t.Fatalf("refreshMapping: %v", err)
	}
	if got := repositoryNames(w.repositories.Accepted()); !slices.Equal(got, []string{moved}) {
		t.Fatalf("the refresh serves %v, want %s, kept on its last answer", got, moved)
	}
	if got := w.repositories.WithoutWebhookApp(); !slices.Equal(got, want) {
		t.Fatalf("the webhook App gaps after the kept refresh are %#v, want %#v: legion state names the repository the daemon serves", got, want)
	}
}
