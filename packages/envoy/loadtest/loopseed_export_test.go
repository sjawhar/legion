package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// --- a loop seed a test can run a step against ------------------------------------------------

// loopTestSeedProposals are three proposals of the shape the loop holds: one the driver is live on,
// with part documents, spare quote rows, review pages, a review index and threads of both kinds;
// one with a part document and threads but no review; and one with no requirement id at all, which
// 336 of the loop's 891 rows have. The requirement ids are the weights a read or a write lands by.
func loopTestSeedProposals() []loopSeedProposal {
	return []loopSeedProposal{
		{
			Index: 0, Name: "alpha", Key: "LOOP-1", LiveInDriver: true, IDs: 300,
			Parts: []loopSeedPart{
				{Index: 0, ArtifactID: "part-a0", Slug: "part-001.md"},
				{Index: 1, ArtifactID: "part-a1", Slug: "part-002.md"},
			},
			LastPart:    1,
			SpareQuotes: []string{loopSpareRow(0, 0), loopSpareRow(0, 1), loopSpareRow(0, 2)},
			ReviewPages: []loopSeedPage{
				{Index: 0, ArtifactID: "page-a0", Bytes: loopPageMedian},
				{Index: 1, ArtifactID: "page-a1", Bytes: loopPageMean},
			},
			ReviewIndex:     "index-a",
			OpenThreads:     []string{"open-a0", "open-a1"},
			ResolvedThreads: []string{"done-a0", "done-a1"},
		},
		{
			Index: 1, Name: "beta", Key: "LOOP-2", IDs: 100,
			Parts:           []loopSeedPart{{Index: 0, ArtifactID: "part-b0", Slug: "part-001.md"}},
			LastPart:        0,
			SpareQuotes:     []string{loopSpareRow(1, 0)},
			OpenThreads:     []string{"open-b0"},
			ResolvedThreads: []string{"done-b0"},
		},
		{Index: 2, Name: "gamma", Key: "LOOP-3", LastPart: -1},
	}
}

// loopTestSeed is that seed, prepared: the weighted draws are built and the proposals with no
// issue key are gone, as a step's own seed is when it starts.
func loopTestSeed(t *testing.T) *loopSeed {
	t.Helper()
	ls := &loopSeed{
		Plan: "loopseed.plan.json", PlanID: strings.Repeat("ab", 32) + ".7", Project: "LOOP",
		ThreadsPerProposal: loopThreadSample,
		SearchTerms:        []string{"alpha", "beta", "gamma"},
		StatusTerms:        loopStatusNames[:],
		Proposals:          loopTestSeedProposals(),
	}
	if dropped := ls.prepare(); dropped != 0 {
		t.Fatalf("prepare dropped %d proposals of a seed whose proposals all have a key", dropped)
	}
	return ls
}

// TestLoopSeedServesWhatEachKindNeeds: the weighted draws are over the proposals that hold the
// record a kind reads, so a verdict never lands where there is no review page.
func TestLoopSeedServesWhatEachKindNeeds(t *testing.T) {
	ls := loopTestSeed(t)
	for need, want := range map[loopNeed]int{
		loopNeedAny:            3,
		loopNeedPart:           2,
		loopNeedLastPart:       2,
		loopNeedReviewPage:     1,
		loopNeedReviewIndex:    1,
		loopNeedOpenThread:     2,
		loopNeedResolvedThread: 2,
	} {
		if got := ls.servingProposals(need); got != want {
			t.Errorf("%s is served by %d proposals, want %d", loopNeedNames[need], got, want)
		}
	}
	for range 200 {
		if at := ls.pick(loopNeedReviewPage, 0.99); ls.Proposals[at].ReviewIndex == "" {
			t.Fatalf("a review-page draw landed on proposal %d, which holds none", at)
		}
	}
}

// TestLoopSeedRefusesEachMissingIngredient: checkSeedMix refuses a loop mix the seed cannot serve,
// naming the record no proposal holds, so a step is refused before it writes rather than refused
// 413 by 413.
func TestLoopSeedRefusesEachMissingIngredient(t *testing.T) {
	mix, err := parseEventMix("spec-loop-2026-11-03")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkSeedMix(loopTestSeed(t), mix, "loopseed.seed.json"); err != nil {
		t.Fatalf("a seed that holds every record is refused: %v", err)
	}
	for name, c := range map[string]struct {
		strip func(pr *loopSeedProposal)
		want  string
	}{
		"no review page":     {func(pr *loopSeedProposal) { pr.ReviewPages = nil }, "a review page"},
		"no spare quote":     {func(pr *loopSeedProposal) { pr.SpareQuotes = nil }, "a spare quote row"},
		"no last part":       {func(pr *loopSeedProposal) { pr.LastPart = -1 }, "a spare quote row"},
		"no open thread":     {func(pr *loopSeedProposal) { pr.OpenThreads = nil }, "an open thread"},
		"no resolved thread": {func(pr *loopSeedProposal) { pr.ResolvedThreads = nil }, "a resolved thread"},
	} {
		t.Run(name, func(t *testing.T) {
			ls := &loopSeed{Project: "LOOP", SearchTerms: []string{"alpha"}, StatusTerms: loopStatusNames[:],
				Proposals: loopTestSeedProposals()}
			for i := range ls.Proposals {
				c.strip(&ls.Proposals[i])
			}
			ls.prepare()
			err := checkSeedMix(ls, mix, "loopseed.seed.json")
			if err == nil {
				t.Fatalf("a seed with %s is accepted", name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("refusal %q does not name %q", err, c.want)
			}
		})
	}
	// seed.json cannot serve the loop's kinds at all, and says so by the flag that can.
	plain := seedFile{Issues: []seedIssue{{Key: "LGA-1", Quotes: []string{"q"}, PlainCommentIDs: []string{"c"}}}}
	err = checkSeedMix(plain, mix, "seed.json")
	if err == nil || !strings.Contains(err.Error(), "-loop-seed") {
		t.Fatalf("seed.json under the loop mix is refused with %v, want a refusal naming -loop-seed", err)
	}
}

// --- the export -------------------------------------------------------------------------------

// loopExportTestPlan is the plan the export tests write and then read back: the stub tests' rows,
// with three review pages on the live proposal.
func loopExportTestPlan(t *testing.T) *loopPlan {
	t.Helper()
	return loopTestPlanOf(t, 11, loopTestRows(), map[int]int{0: 3})
}

// loopWriteTestSeed writes a plan to a stub Dispatch over a real loadtest.loop_ids, as
// loop-seed-write does, and answers the id map the export then reads. refuse, when it is not nil,
// refuses the writes it names, as a deployment refuses what it cannot hold.
func loopWriteTestSeed(t *testing.T, p *loopPlan, refuse func(route, key string) (int, string)) (*loopStub, map[string]string) {
	t.Helper()
	ctx := context.Background()
	pool := loopTestDB(t)
	planID := loopPlanID(p)
	ids, err := loadLoopIDStore(ctx, pool, planID)
	if err != nil {
		t.Fatalf("load the id map: %v", err)
	}
	stub := newLoopStub(t)
	stub.refuse = refuse
	if err := loopTestWriter(t, p, stub, ids).run(ctx, 4); err != nil {
		t.Fatal(err)
	}
	read, err := readLoopIDs(ctx, pool, planID)
	if err != nil {
		t.Fatalf("read the id map: %v", err)
	}
	if len(read) == 0 {
		t.Fatal("the id map is empty after a run that wrote the plan")
	}
	return stub, read
}

// TestLoopSeedExportFromAWrittenSeed: the export of a seed written whole holds each proposal's own
// ids, its 64 spare quote rows, a sample of its open and resolved threads capped as the flag says,
// both term lists, and no missing record.
func TestLoopSeedExportFromAWrittenSeed(t *testing.T) {
	p := loopExportTestPlan(t)
	stub, ids := loopWriteTestSeed(t, p, nil)
	ls, err := buildLoopSeed("loopseed.plan.json", p, ids, loopThreadSample)
	if err != nil {
		t.Fatal(err)
	}
	if ls.Project != "LOOP" || ls.PlanID != loopPlanID(p) || ls.Seed != p.Seed {
		t.Fatalf("the export names project %q, plan %q, seed %d", ls.Project, ls.PlanID, ls.Seed)
	}
	if want := []string{"alpha", "beta", "gamma"}; !slices.Equal(ls.SearchTerms, want) {
		t.Errorf("search terms %v, want every proposal name %v", ls.SearchTerms, want)
	}
	if !slices.Equal(ls.StatusTerms, loopStatusNames[:]) {
		t.Errorf("status terms %v, want the planner's %v", ls.StatusTerms, loopStatusNames)
	}
	if (ls.Missing != loopSeedMissing{}) {
		t.Errorf("a whole seed exports with missing records %+v", ls.Missing)
	}
	if len(ls.Proposals) != len(p.Proposals) {
		t.Fatalf("%d proposals exported, want %d", len(ls.Proposals), len(p.Proposals))
	}

	// The logical path each local id was created under, which the stub kept with its write.
	pathOf := map[string]string{}
	for id, w := range stub.made {
		pathOf[id] = strings.TrimPrefix(w.key, loopPlanID(p)+".")
	}
	pr, sp := &p.Proposals[0], &ls.Proposals[0]
	if sp.Key == "" || pathOf[sp.Key] != loopIssuePath(0) {
		t.Fatalf("proposal 0's key %q was created as %q, want %q", sp.Key, pathOf[sp.Key], loopIssuePath(0))
	}
	if len(sp.Parts) != len(pr.Parts) || sp.LastPart != len(sp.Parts)-1 {
		t.Fatalf("proposal 0 exports %d part documents (last %d), want %d with the last marked",
			len(sp.Parts), sp.LastPart, len(pr.Parts))
	}
	for i, part := range sp.Parts {
		if pathOf[part.ArtifactID] != loopPartPath(0, i) || part.Slug != loopPartName(i) {
			t.Errorf("part %d is id %q (%q), slug %q", i, part.ArtifactID, pathOf[part.ArtifactID], part.Slug)
		}
	}
	if len(sp.SpareQuotes) != loopSpareQuotes || sp.SpareQuotes[0] != loopSpareRow(0, 0) {
		t.Fatalf("proposal 0 exports %d spare quote rows starting %q, want %d starting %q",
			len(sp.SpareQuotes), sp.SpareQuotes[0], loopSpareQuotes, loopSpareRow(0, 0))
	}
	if !strings.Contains(stub.docs[sp.Parts[sp.LastPart].ArtifactID], sp.SpareQuotes[0]) {
		t.Error("the first spare quote row is not a line of the last part document it anchors in")
	}
	if len(sp.ReviewPages) != 3 || sp.ReviewIndex == "" {
		t.Fatalf("proposal 0 exports %d review pages and index %q, want 3 and one", len(sp.ReviewPages), sp.ReviewIndex)
	}
	for i, page := range sp.ReviewPages {
		if pathOf[page.ArtifactID] != loopPagePath(0, i) || page.Index != i {
			t.Errorf("review page %d is id %q, created as %q, at index %d",
				i, page.ArtifactID, pathOf[page.ArtifactID], page.Index)
		}
		// The planned size travels with the id: a verdict draws the page's own text from it
		// without reading the page, and the page the seed wrote is that many bytes.
		if want := p.ReviewPages[i].Bytes; page.Bytes != want {
			t.Errorf("review page %d exports %d bytes, want the plan's %d", i, page.Bytes, want)
		}
		if got := int64(len(stub.docs[page.ArtifactID])); got != page.Bytes {
			t.Errorf("review page %d was written as %d bytes and exports %d", i, got, page.Bytes)
		}
	}
	if pathOf[sp.ReviewIndex] != loopIndexPath(0) {
		t.Errorf("the review index is id %q, created as %q", sp.ReviewIndex, pathOf[sp.ReviewIndex])
	}

	// Open and resolved are the planner's own statuses: a live id's thread is left open and every
	// other is resolved.
	statuses := loopStatuses(p.Seed, 0, loopPlanRow(pr))
	if len(sp.OpenThreads) != pr.LiveIDs || len(sp.ResolvedThreads) != pr.IDs-pr.LiveIDs {
		t.Fatalf("proposal 0 exports %d open and %d resolved threads, want %d and %d",
			len(sp.OpenThreads), len(sp.ResolvedThreads), pr.LiveIDs, pr.IDs-pr.LiveIDs)
	}
	for _, c := range []struct {
		ids  []string
		open bool
	}{{sp.OpenThreads, true}, {sp.ResolvedThreads, false}} {
		for _, id := range c.ids {
			path := pathOf[id]
			f := strings.Split(path, ".")
			if len(f) != 4 || f[0] != loopKindThread {
				t.Fatalf("thread id %q was created as %q, which is no thread", id, path)
			}
			j, err := strconv.Atoi(f[3])
			if err != nil {
				t.Fatal(err)
			}
			if (statuses[j] == loopStatusLive) != c.open {
				t.Errorf("thread %d is %s in the plan and is exported as open=%t",
					j, loopStatusNames[statuses[j]], c.open)
			}
		}
	}

	// The cap: a sample of one of each kind, spread over the threads rather than cut off the
	// front, and the sample is a sample of the same ids.
	capped, err := buildLoopSeed("loopseed.plan.json", p, ids, 1)
	if err != nil {
		t.Fatal(err)
	}
	one := &capped.Proposals[0]
	if len(one.OpenThreads) != 1 || len(one.ResolvedThreads) != 1 {
		t.Fatalf("-threads-per-proposal 1 exports %d open and %d resolved threads",
			len(one.OpenThreads), len(one.ResolvedThreads))
	}
	if !slices.Contains(sp.OpenThreads, one.OpenThreads[0]) || !slices.Contains(sp.ResolvedThreads, one.ResolvedThreads[0]) {
		t.Error("the capped sample holds a thread the whole export does not")
	}
	if n := len(capped.Proposals[2].Parts); n == 0 {
		t.Error("the proposal with no requirement id exports no part document to read")
	}
}

// TestLoopSeedExportCountsWhatItCouldNotFill: a seed whose review pages the deployment refused
// exports the proposal with what it has, and the file says how many pages it could not fill and
// that the proposal holds none - so a run that would read nothing is told rather than reading an
// empty id.
func TestLoopSeedExportCountsWhatItCouldNotFill(t *testing.T) {
	p := loopExportTestPlan(t)
	// A 413 is refused once: a 5xx would be retried six times, and what this test wants is the
	// refusal, not the retry (api.go callRetryKeyed).
	refusePages := func(route, key string) (int, string) {
		if strings.Contains(key, "."+loopKindPage+".") || strings.Contains(key, "."+loopKindIndex+".") {
			return http.StatusRequestEntityTooLarge, "CAP_EXCEEDED"
		}
		return 0, ""
	}
	_, ids := loopWriteTestSeed(t, p, refusePages)
	ls, err := buildLoopSeed("loopseed.plan.json", p, ids, loopThreadSample)
	if err != nil {
		t.Fatal(err)
	}
	sp := &ls.Proposals[0]
	if len(sp.ReviewPages) != 0 || sp.ReviewIndex != "" {
		t.Fatalf("proposal 0 exports %d review pages and index %q after they were all refused",
			len(sp.ReviewPages), sp.ReviewIndex)
	}
	if ls.Missing.ReviewPages != 3 || ls.Missing.ReviewIndexes != 1 {
		t.Errorf("missing counts %d review pages and %d review indexes, want 3 and 1",
			ls.Missing.ReviewPages, ls.Missing.ReviewIndexes)
	}
	if ls.Missing.ProposalsWithoutReviewPage != 1 || ls.Missing.ProposalsWithoutReviewIndex != 1 {
		t.Errorf("missing counts %d proposals without a review page and %d without an index, want 1 and 1",
			ls.Missing.ProposalsWithoutReviewPage, ls.Missing.ProposalsWithoutReviewIndex)
	}
	if ls.Missing.Issues != 0 || ls.Missing.Parts != 0 || ls.Missing.Threads != 0 {
		t.Errorf("missing counts records the refusal did not touch: %+v", ls.Missing)
	}
	if got := ls.Missing.summary(); !strings.Contains(got, "3 review pages") {
		t.Errorf("the missing line reads %q", got)
	}
	// A verdict cannot be drawn against this seed, and the refusal names the record.
	ls.prepare()
	mix, err := parseEventMix("verdict=1")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkSeedMix(ls, mix, "loopseed.seed.json"); err == nil || !strings.Contains(err.Error(), "a review page") {
		t.Fatalf("a seed with no review page is refused with %v, want a refusal naming it", err)
	}
}

// TestLoopSeedExportRefusesAMapWithNoProject: without the project row no read of the loop can name
// a project, so the export says so rather than writing a file a step cannot use.
func TestLoopSeedExportRefusesAMapWithNoProject(t *testing.T) {
	p := loopExportTestPlan(t)
	if _, err := buildLoopSeed("loopseed.plan.json", p, map[string]string{}, loopThreadSample); err == nil ||
		!strings.Contains(err.Error(), "loop-seed-write") {
		t.Fatalf("an id map with no project is accepted or refused without a remedy: %v", err)
	}
}

// TestLoopSeedFileRoundTrips: what the export writes is what a run reads, and a file that names no
// project or holds no proposal is refused by name.
func TestLoopSeedFileRoundTrips(t *testing.T) {
	ls := loopTestSeed(t)
	dir := t.TempDir()
	path := filepath.Join(dir, loopSeedFileName)
	b, err := json.Marshal(ls)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	read, err := readLoopSeed(path)
	if err != nil {
		t.Fatal(err)
	}
	if read.Project != ls.Project || len(read.Proposals) != len(ls.Proposals) {
		t.Fatalf("the file read back as project %q with %d proposals", read.Project, len(read.Proposals))
	}
	if read.Proposals[0].SpareQuotes[0] != ls.Proposals[0].SpareQuotes[0] {
		t.Error("the spare quote rows did not survive the file")
	}
	sf := read.seedFile()
	if len(sf.Issues) != len(read.Proposals) || sf.Issues[0].Key != read.Proposals[0].Key ||
		sf.Issues[0].ArtifactID != read.Proposals[0].Parts[0].ArtifactID {
		t.Fatalf("the step's issues are %+v, want one per proposal in the seed's order", sf.Issues)
	}
	if len(sf.Projects) != 1 || sf.Projects[0] != ls.Project {
		t.Fatalf("the step's projects are %v", sf.Projects)
	}

	for name, body := range map[string]string{
		"no project":  `{"proposals":[{"key":"LOOP-1"}],"search_terms":["a"],"status_terms":["live"]}`,
		"no proposal": `{"project":"LOOP","search_terms":["a"],"status_terms":["live"]}`,
		"no term":     `{"project":"LOOP","proposals":[{"key":"LOOP-1"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			bad := filepath.Join(t.TempDir(), loopSeedFileName)
			if err := os.WriteFile(bad, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := readLoopSeed(bad); err == nil {
				t.Fatalf("a loop seed with %s is accepted", name)
			}
		})
	}
}

// TestLoopSeedPrepareDropsProposalsWithNoIssue: a proposal whose issue the deployment refused has
// nothing to read or to write, so a step leaves it out and says how many it left.
func TestLoopSeedPrepareDropsProposalsWithNoIssue(t *testing.T) {
	ls := &loopSeed{Project: "LOOP", SearchTerms: []string{"alpha"}, StatusTerms: loopStatusNames[:],
		Proposals: loopTestSeedProposals()}
	ls.Proposals[1].Key = ""
	if dropped := ls.prepare(); dropped != 1 {
		t.Fatalf("prepare dropped %d proposals, want the one with no issue key", dropped)
	}
	if len(ls.Proposals) != 2 || ls.Proposals[1].Name != "gamma" {
		t.Fatalf("the proposals left are %+v", ls.Proposals)
	}
	if at := ls.pick(loopNeedAny, 0.999); ls.Proposals[at].Key == "" {
		t.Fatal("a draw landed on a proposal with no issue key")
	}
}

// TestLoopSampleSpansItsIDs: a capped sample is spread over the ids rather than their first n, so
// a step's status changes reach a proposal's later threads too.
func TestLoopSampleSpansItsIDs(t *testing.T) {
	ids := make([]string, 1000)
	for i := range ids {
		ids[i] = strconv.Itoa(i)
	}
	got := loopSample(ids, 4)
	if want := []string{"0", "250", "500", "750"}; !slices.Equal(got, want) {
		t.Fatalf("loopSample(1000 ids, 4) = %v, want %v", got, want)
	}
	if n := len(loopSample(ids[:3], 4)); n != 3 {
		t.Fatalf("a sample of fewer ids than the cap holds %d", n)
	}
	if n := len(loopSample(nil, 4)); n != 0 {
		t.Fatalf("a sample of no id holds %d", n)
	}
}

// TestLoopPickFollowsRequirementIDs: a read or a write lands on a proposal in proportion to its
// requirement ids, not evenly - the loop's load is its requirements - and a seed of one proposal
// always lands there.
func TestLoopPickFollowsRequirementIDs(t *testing.T) {
	ls := loopTestSeed(t)
	const draws = 200_000
	counts := map[string]int{}
	for i := range draws {
		counts[ls.Proposals[ls.pick(loopNeedAny, (float64(i)+0.5)/draws)].Name]++
	}
	var ids float64
	for i := range ls.Proposals {
		ids += float64(ls.Proposals[i].IDs)
	}
	// The draw is a cumulative-weight lookup, so evenly spaced draws land within one draw of the
	// exact share; 0.5% is far inside that and far outside an even draw's 33%.
	const tolerance = 0.005
	for i := range ls.Proposals {
		pr := &ls.Proposals[i]
		want := float64(pr.IDs) / ids
		got := float64(counts[pr.Name]) / draws
		if math.Abs(got-want) > tolerance {
			t.Errorf("proposal %s took %.4f of the draws, want %.4f of them (its %d of %g requirement ids), tolerance %.3f",
				pr.Name, got, want, pr.IDs, ids, tolerance)
		}
	}
	if counts["gamma"] != 0 {
		t.Errorf("a proposal with no requirement id took %d draws", counts["gamma"])
	}

	one := &loopSeed{Project: "LOOP", SearchTerms: []string{"alpha"}, StatusTerms: loopStatusNames[:],
		Proposals: []loopSeedProposal{{Index: 0, Name: "alpha", Key: "LOOP-1", IDs: 7, LastPart: -1}}}
	one.prepare()
	for i := range 1000 {
		if at := one.pick(loopNeedAny, float64(i)/1000); at != 0 {
			t.Fatalf("a seed of one proposal drew %d", at)
		}
	}
	// A seed whose proposals hold no requirement id at all still draws over them.
	none := &loopSeed{Project: "LOOP", SearchTerms: []string{"a"}, StatusTerms: loopStatusNames[:],
		Proposals: []loopSeedProposal{{Key: "LOOP-1", LastPart: -1}, {Key: "LOOP-2", LastPart: -1}}}
	none.prepare()
	seen := map[int]bool{}
	for i := range 100 {
		seen[none.pick(loopNeedAny, float64(i)/100)] = true
	}
	if len(seen) != 2 {
		t.Fatalf("a seed of proposals with no requirement id drew %v, want both", seen)
	}
}
