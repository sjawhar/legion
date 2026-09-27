package classify

import (
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// observedRun is one GitHub check run on a head: its name, its id, the times (UTC, one day) it
// started and completed, and its conclusion.
type observedRun struct {
	name       string
	id         int64
	started    string
	completed  string
	conclusion string
}

// pr1084Head2257aaef is every check run GitHub holds for sjawhar/legion#1084 at head 2257aaef
// (`gh api repos/sjawhar/legion/commits/2257aaef462a56035ae860ba889fc4a6d2ebf6ae/check-runs`),
// 2026-09-14, in id order. pr-title ran in six check suites, one per pull_request event: two
// failures on the first title, then four passes after the title was corrected at 07:34. The other
// CI jobs are ten attempts of one workflow run, each attempt a fresh set of ids.
var pr1084Head2257aaef = []observedRun{
	{"pr-title", 103893898706, "07:32:39", "07:32:42", "failure"},
	{"test", 103893898747, "07:32:39", "07:33:39", "success"},
	{"lint", 103893898898, "07:32:39", "07:32:56", "success"},
	{"typecheck", 103893898945, "07:32:40", "07:33:05", "success"},
	{"changes", 103893899135, "07:32:40", "07:32:47", "success"},
	{"dispatch", 103893940833, "07:32:50", "07:40:48", "success"},
	{"envoy-plugin", 103893941871, "07:32:48", "07:32:48", "skipped"},
	{"envoy-go", 103893941911, "07:32:48", "07:32:48", "skipped"},
	{"envoy-client", 103893942005, "07:32:48", "07:32:48", "skipped"},
	{"claude-envoy-bridge", 103893942222, "07:32:48", "07:32:48", "skipped"},
	{"contracts", 103893942258, "07:32:48", "07:32:48", "skipped"},
	{"pi-envoy", 103893943165, "07:32:48", "07:32:48", "skipped"},
	{"pr-title", 103894181815, "07:33:48", "07:33:50", "failure"},
	{"pr-title", 103894268306, "07:34:09", "07:34:12", "success"},
	{"pr-title", 103894411582, "07:34:45", "07:34:49", "success"},
	{"changes", 103896119514, "07:41:50", "07:41:58", "success"},
	{"dispatch", 103896163258, "07:42:00", "07:49:21", "success"},
	{"envoy-plugin", 103896164268, "07:41:59", "07:41:58", "skipped"},
	{"pi-envoy", 103896164284, "07:41:59", "07:41:58", "skipped"},
	{"envoy-client", 103896164312, "07:41:59", "07:41:58", "skipped"},
	{"claude-envoy-bridge", 103896164363, "07:41:59", "07:41:58", "skipped"},
	{"envoy-go", 103896164586, "07:41:59", "07:41:58", "skipped"},
	{"contracts", 103896165099, "07:41:59", "07:41:58", "skipped"},
	{"changes", 103899568044, "07:55:33", "07:55:39", "success"},
	{"dispatch", 103899602152, "07:55:41", "08:03:46", "success"},
	{"envoy-plugin", 103899603277, "07:55:40", "07:55:40", "skipped"},
	{"claude-envoy-bridge", 103899603301, "07:55:40", "07:55:40", "skipped"},
	{"pi-envoy", 103899603601, "07:55:40", "07:55:40", "skipped"},
	{"envoy-client", 103899603646, "07:55:40", "07:55:40", "skipped"},
	{"contracts", 103899603697, "07:55:40", "07:55:40", "skipped"},
	{"envoy-go", 103899603766, "07:55:40", "07:55:40", "skipped"},
	{"changes", 103902189133, "08:05:48", "08:05:55", "success"},
	{"dispatch", 103902233091, "08:05:58", "08:14:25", "success"},
	{"envoy-go", 103902234380, "08:05:56", "08:05:55", "skipped"},
	{"envoy-client", 103902234398, "08:05:56", "08:05:55", "skipped"},
	{"contracts", 103902234479, "08:05:56", "08:05:55", "skipped"},
	{"pi-envoy", 103902234494, "08:05:56", "08:05:55", "skipped"},
	{"claude-envoy-bridge", 103902234797, "08:05:56", "08:05:55", "skipped"},
	{"envoy-plugin", 103902234823, "08:05:56", "08:05:55", "skipped"},
	{"changes", 103904933845, "08:15:55", "08:16:03", "success"},
	{"dispatch", 103904983355, "08:16:05", "08:24:12", "success"},
	{"envoy-client", 103904984351, "08:16:04", "08:16:03", "skipped"},
	{"contracts", 103904984397, "08:16:04", "08:16:03", "skipped"},
	{"envoy-go", 103904984623, "08:16:04", "08:16:03", "skipped"},
	{"envoy-plugin", 103904984753, "08:16:04", "08:16:03", "skipped"},
	{"pi-envoy", 103904984770, "08:16:04", "08:16:03", "skipped"},
	{"claude-envoy-bridge", 103904984937, "08:16:04", "08:16:03", "skipped"},
	{"changes", 103907294182, "08:24:49", "08:24:53", "success"},
	{"dispatch", 103907321441, "08:24:55", "08:32:41", "success"},
	{"envoy-client", 103907322542, "08:24:54", "08:24:53", "skipped"},
	{"envoy-plugin", 103907322634, "08:24:54", "08:24:53", "skipped"},
	{"claude-envoy-bridge", 103907322971, "08:24:54", "08:24:53", "skipped"},
	{"contracts", 103907322999, "08:24:54", "08:24:53", "skipped"},
	{"envoy-go", 103907323020, "08:24:54", "08:24:53", "skipped"},
	{"pi-envoy", 103907323104, "08:24:54", "08:24:53", "skipped"},
	{"changes", 103909676287, "08:33:37", "08:33:44", "success"},
	{"dispatch", 103909721259, "08:33:46", "08:41:37", "success"},
	{"contracts", 103909722188, "08:33:44", "08:33:44", "skipped"},
	{"envoy-client", 103909722246, "08:33:44", "08:33:44", "skipped"},
	{"pi-envoy", 103909722644, "08:33:44", "08:33:44", "skipped"},
	{"envoy-plugin", 103909722728, "08:33:44", "08:33:44", "skipped"},
	{"envoy-go", 103909722999, "08:33:44", "08:33:44", "skipped"},
	{"claude-envoy-bridge", 103909723087, "08:33:44", "08:33:44", "skipped"},
	{"changes", 103912061108, "08:42:30", "08:42:37", "success"},
	{"dispatch", 103912103565, "08:42:39", "08:51:00", "success"},
	{"envoy-client", 103912104565, "08:42:38", "08:42:38", "skipped"},
	{"pi-envoy", 103912104808, "08:42:38", "08:42:38", "skipped"},
	{"claude-envoy-bridge", 103912105049, "08:42:38", "08:42:38", "skipped"},
	{"envoy-plugin", 103912105191, "08:42:38", "08:42:38", "skipped"},
	{"contracts", 103912105320, "08:42:38", "08:42:38", "skipped"},
	{"envoy-go", 103912105467, "08:42:38", "08:42:38", "skipped"},
	{"changes", 103914444860, "08:51:17", "08:51:21", "success"},
	{"dispatch", 103914473621, "08:51:24", "08:59:48", "success"},
	{"envoy-go", 103914474742, "08:51:21", "08:51:21", "skipped"},
	{"envoy-client", 103914475049, "08:51:21", "08:51:21", "skipped"},
	{"pi-envoy", 103914475114, "08:51:21", "08:51:21", "skipped"},
	{"contracts", 103914475273, "08:51:22", "08:51:21", "skipped"},
	{"claude-envoy-bridge", 103914475347, "08:51:22", "08:51:21", "skipped"},
	{"envoy-plugin", 103914475918, "08:51:22", "08:51:21", "skipped"},
	{"changes", 103916858829, "09:00:13", "09:00:20", "success"},
	{"dispatch", 103916904492, "09:00:22", "09:09:13", "success"},
	{"envoy-client", 103916905412, "09:00:20", "09:00:20", "skipped"},
	{"pi-envoy", 103916905465, "09:00:20", "09:00:20", "skipped"},
	{"claude-envoy-bridge", 103916905828, "09:00:20", "09:00:20", "skipped"},
	{"contracts", 103916905911, "09:00:20", "09:00:20", "skipped"},
	{"envoy-go", 103916905966, "09:00:20", "09:00:20", "skipped"},
	{"envoy-plugin", 103916906108, "09:00:20", "09:00:20", "skipped"},
	{"pr-title", 103920681906, "09:14:18", "09:14:22", "success"},
	{"pr-title", 103924805268, "09:28:02", "09:28:04", "success"},
}

// listenerView is the head as an Envoy listener record holds it at instant at: for each check
// name, its highest-id run that has appeared by then (a record keeps one run per name and never
// lowers its id). complete is whether every one of those runs has concluded; the listener
// publishes a settlement only then. With onlyConcluded the view leaves out every name whose latest
// run is still going: an incomplete view, which the contract lets a settlement be.
func listenerView(runs []observedRun, at string, onlyConcluded bool) (SettlementCandidate, bool) {
	latest := map[string]observedRun{}
	for _, run := range runs {
		if min(run.started, run.completed) > at {
			continue
		}
		if known, found := latest[run.name]; !found || run.id > known.id {
			latest[run.name] = run
		}
	}
	candidate := SettlementCandidate{CheckRuns: []record.AttemptRun{}, Failing: []string{}, Verdict: "green"}
	complete := true
	for name, run := range latest {
		concluded := run.completed <= at
		complete = complete && concluded
		if onlyConcluded && !concluded {
			continue
		}
		candidate.CheckRuns = append(candidate.CheckRuns, record.AttemptRun{Name: name, ID: run.id})
		if concluded && run.conclusion == "failure" {
			candidate.Failing = append(candidate.Failing, name)
			candidate.Verdict = "red"
		}
	}
	sort.Slice(candidate.CheckRuns, func(i, j int) bool { return candidate.CheckRuns[i].Name < candidate.CheckRuns[j].Name })
	sort.Strings(candidate.Failing)
	return candidate, complete
}

// timedSettlement is one settlement and the instant it describes.
type timedSettlement struct {
	at        string
	candidate SettlementCandidate
}

// pr1084Settlements is one settlement at every instant a check run on the head completed, each
// with the next listener generation: only the complete views the listener publishes, or, with
// incomplete, every instant's view of the runs concluded by then.
func pr1084Settlements(incomplete bool) []timedSettlement {
	instants := make([]string, 0, len(pr1084Head2257aaef))
	for _, run := range pr1084Head2257aaef {
		instants = append(instants, run.completed)
	}
	slices.Sort(instants)
	instants = slices.Compact(instants)
	var settlements []timedSettlement
	for _, at := range instants {
		candidate, complete := listenerView(pr1084Head2257aaef, at, incomplete)
		if !incomplete && !complete {
			continue
		}
		candidate.Generation = int64(len(settlements) + 1)
		candidate.Snapshot = fmt.Sprintf("g%d", candidate.Generation)
		settlements = append(settlements, timedSettlement{at: at, candidate: candidate})
	}
	return settlements
}

// verdictChange is the head's verdict becoming verdict at the settlement describing instant at.
type verdictChange struct {
	at      string
	verdict string
}

// settleAll applies the settlements in order, as the workflow engine does, and returns the pull
// request and every change of its verdict.
func settleAll(pr record.PullRequest, settlements []timedSettlement) (record.PullRequest, []verdictChange) {
	var changes []verdictChange
	for _, settlement := range settlements {
		settled, applied := ApplySettlement(pr, settlement.candidate)
		if !applied {
			continue
		}
		if settled.Verdict != pr.Verdict {
			changes = append(changes, verdictChange{at: settlement.at, verdict: settled.Verdict})
		}
		pr = settled
	}
	return pr, changes
}

func freshHead(sha string) record.PullRequest {
	return record.PullRequest{HeadSHA: sha, Failing: []string{}, FailingStatuses: []string{}}
}

// LEGION-152 acceptance 1: the settlements GitHub's check runs on #1084's head produce take the
// head red at most once, at the first title failure, green once the passing title run is seen, and
// never red again - including when every earlier settlement is delivered a second time afterwards.
func TestPR1084HeadReplayGoesRedAtMostOnceAndNeverReturnsToRed(t *testing.T) {
	// The listener publishes only complete views. The first is at 07:40:48, when the CI suite's
	// first attempt finished, after the title fix; the tree's architect got that green at 07:40:55.
	// (The model also counts the instants between two attempts as complete, which the listener's
	// quiet period and suite tracking skip: more green views, never fewer.)
	published := pr1084Settlements(false)
	if published[0].at != "07:40:48" {
		t.Fatalf("first published settlement at %s, want 07:40:48", published[0].at)
	}
	_, changes := settleAll(freshHead("2257aaef"), published)
	if want := []verdictChange{{"07:40:48", "green"}}; !slices.Equal(changes, want) {
		t.Fatalf("published verdicts changed %v, want %v", changes, want)
	}

	// A settlement at every completion, each leaving out the checks still running, is the harshest
	// sequence the contract admits, and it holds the title failures.
	every := pr1084Settlements(true)
	pr, changes := settleAll(freshHead("2257aaef"), every)
	if want := []verdictChange{{"07:32:42", "red"}, {"07:34:12", "green"}}; !slices.Equal(changes, want) {
		t.Fatalf("verdicts changed %v, want %v", changes, want)
	}
	// At-least-once delivery: every settlement again, newest first, after the last.
	redelivered := slices.Clone(every)
	slices.Reverse(redelivered)
	if _, changes := settleAll(pr, redelivered); len(changes) != 0 {
		t.Fatalf("redelivered settlements changed the verdict %v, want no change from %s", changes, pr.Verdict)
	}
}

// LEGION-152 acceptance 2: once a head's only red check has passed on a newer run, no later
// settlement of another check on the head reports it red again - whether the settlement holds the
// whole record or, as a record recreated after the listener's TTL does, only the other check.
func TestAPassedCheckStaysRetiredWhileAnotherCheckSettlesRepeatedly(t *testing.T) {
	settlements := []timedSettlement{
		{"title fails", SettlementCandidate{CheckRuns: []record.AttemptRun{{Name: "ci", ID: 20}, {Name: "title", ID: 10}}, Verdict: "red", Failing: []string{"title"}}},
		{"title passes", SettlementCandidate{CheckRuns: []record.AttemptRun{{Name: "ci", ID: 20}, {Name: "title", ID: 11}}, Verdict: "green", Failing: []string{}}},
	}
	for id := int64(21); id <= 30; id++ {
		runs := []record.AttemptRun{{Name: "ci", ID: id}, {Name: "title", ID: 11}}
		if id%2 == 0 { // every other settlement comes from a recreated record holding only ci
			runs = runs[:1]
		}
		settlements = append(settlements, timedSettlement{fmt.Sprintf("ci %d", id), SettlementCandidate{CheckRuns: runs, Verdict: "green", Failing: []string{}}})
	}
	for i := range settlements {
		settlements[i].candidate.Generation = int64(i + 1)
		settlements[i].candidate.Snapshot = fmt.Sprintf("g%d", i+1)
	}
	_, changes := settleAll(freshHead("head"), settlements)
	if want := []verdictChange{{"title fails", "red"}, {"title passes", "green"}}; !slices.Equal(changes, want) {
		t.Fatalf("verdicts changed %v, want %v", changes, want)
	}
}

// The daemon's fence is the per-name maximum over every settlement it accepted (packages/envoy
// AGENTS.md, "Topic shapes"): a settlement that leaves a check out does not drop the check's run
// from the fence, so a late delivery of that check's older, failing view is still older. A fence
// replaced by each settlement forgets the passing run, takes the old failure as a new name, and
// turns a green head red until the check runs again.
func TestASettlementThatOmitsACheckKeepsItsRunInTheFence(t *testing.T) {
	firstView := SettlementCandidate{CheckRuns: []record.AttemptRun{{Name: "title", ID: 10}}, Generation: 1, Snapshot: "g1", Verdict: "red", Failing: []string{"title"}}
	settlements := []timedSettlement{
		{"title fails", firstView},
		{"title passes", SettlementCandidate{CheckRuns: []record.AttemptRun{{Name: "ci", ID: 20}, {Name: "title", ID: 11}}, Generation: 2, Snapshot: "g2", Verdict: "green", Failing: []string{}}},
		{"recreated record", SettlementCandidate{CheckRuns: []record.AttemptRun{{Name: "ci", ID: 21}}, Generation: 0, Snapshot: "r0", Verdict: "green", Failing: []string{}}},
		{"first view redelivered", firstView},
	}
	pr, changes := settleAll(freshHead("head"), settlements)
	if want := []verdictChange{{"title fails", "red"}, {"title passes", "green"}}; !slices.Equal(changes, want) {
		t.Fatalf("verdicts changed %v, want %v", changes, want)
	}
	if want := []record.AttemptRun{{Name: "ci", ID: 21}, {Name: "title", ID: 11}}; !slices.Equal(pr.CheckRuns, want) {
		t.Fatalf("fence = %v, want %v", pr.CheckRuns, want)
	}
}
