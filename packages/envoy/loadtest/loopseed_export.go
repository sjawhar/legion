package main

// loadgen loop-seed-export: the bridge from the Verified Linux spec loop's written seed
// (loopseed_write.go) to the run that drives it (run.go -loop-seed).
//
// A step cannot read the seed's 334,536 thread ids out of Postgres as it starts, and seed.json
// describes the other seed, so this subcommand reads loadtest.loop_ids once, joins it to the plan
// those ids were written from, and writes loopseed.seed.json: for every proposal its issue key,
// the artifact ids of its part documents with the last marked, the spare quote rows an anchored
// write can take, its review pages and review index, a capped sample of its open and of its
// resolved thread ids, and the requirement-id count a read or a write is drawn by. The file also
// carries the project key, the plan identity and the two search-term lists a requirement query
// reads by: every proposal name, which is a query's target, and the six status words the planner
// gives a thread (loopStatusNames).
//
// A proposal the id map does not hold - a write the deployment refused - is recorded with what it
// has, and the file's missing counts say how many of each kind the export could not fill, so a run
// never reads nothing in silence.
//
// The file is also this package's reader of itself: readLoopSeed, the weighted draw a step lands a
// read or a write with (pick), and what the seed cannot serve (refuseKind, for checkSeedMix).

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// loopSeedFileName is what loop-seed-export writes by default and run's -loop-seed reads.
const loopSeedFileName = "loopseed.seed.json"

// loopThreadSample is how many open and how many resolved thread ids a proposal is recorded with
// by default: enough that a step's status changes spread over a proposal's threads rather than
// hammering one, and little enough that the file of 891 proposals stays a few megabytes.
const loopThreadSample = 128

// loopSeed is loopseed.seed.json: what a run needs to address the records of a written loop seed.
type loopSeed struct {
	Plan         string `json:"plan"`
	PlanID       string `json:"plan_id"`
	Source       string `json:"source"`
	SourceSHA256 string `json:"source_sha256"`
	Seed         uint64 `json:"seed"`
	// Project is the project key every proposal's issue was created in, from the id map's own
	// project row.
	Project            string `json:"project"`
	ThreadsPerProposal int    `json:"threads_per_proposal"`
	// SearchTerms is every proposal name - what a requirement query by target searches for - and
	// StatusTerms the six status words a thread ends under.
	SearchTerms []string           `json:"search_terms"`
	StatusTerms []string           `json:"status_terms"`
	Proposals   []loopSeedProposal `json:"proposals"`
	Missing     loopSeedMissing    `json:"missing"`

	// picks is the weighted draw of each need, and searchEsc and statusEsc the two term lists
	// escaped for the query they land in; all three are built by prepare and are no part of the
	// file.
	picks                [loopNeedCount]loopPicks
	searchEsc, statusEsc []string
}

// loopSeedProposal is one proposal of the loop as a run addresses it: the records the id map holds
// for it, and the requirement-id count its share of the load is drawn by.
type loopSeedProposal struct {
	Index        int    `json:"index"`
	Name         string `json:"name"`
	Key          string `json:"key"`
	LiveInDriver bool   `json:"live_in_driver"`
	// IDs is the proposal's requirement ids: the weight a read or a write lands on it by.
	IDs   int            `json:"ids"`
	Parts []loopSeedPart `json:"parts"`
	// LastPart is where the plan's last part document stands in Parts, -1 when the id map does not
	// hold it. A requirement thread anchors on a spare quote row of that document alone.
	LastPart    int      `json:"last_part"`
	SpareQuotes []string `json:"spare_quotes"`
	// ReviewPages and ReviewIndex are the live proposal's review records; empty for a proposal the
	// driver is not live on, which has none.
	ReviewPages []loopSeedPage `json:"review_pages"`
	ReviewIndex string         `json:"review_index_artifact_id"`
	// OpenThreads are thread roots Dispatch holds open and ResolvedThreads roots it holds
	// resolved, at most ThreadsPerProposal of each (loopSample).
	OpenThreads     []string `json:"open_thread_ids"`
	ResolvedThreads []string `json:"resolved_thread_ids"`
}

// loopSeedPart is one part document of a proposal: the id a read addresses it by and the file name
// the issue document route resolves it by (GET /api/v1/issues/{key}/artifacts/{slug}/text takes a
// slug or an unambiguous filename, internal/dispatch/api/routes_table.go:335).
type loopSeedPart struct {
	Index      int    `json:"index"`
	ArtifactID string `json:"artifact_id"`
	Slug       string `json:"slug"`
}

// loopSeedPage is one review page of a proposal: the id a verdict writes a new version of, where
// the page stands in the proposal's pages, and the byte size the plan drew for it. A verdict
// draws the page's own text from the last two (loopPageText), so it replaces the page's paragraph
// with text of the page's size without reading the page first.
type loopSeedPage struct {
	Index      int    `json:"index"`
	ArtifactID string `json:"artifact_id"`
	Bytes      int64  `json:"bytes"`
}

// loopSeedMissing counts what the export could not fill: the records the plan holds that the id
// map does not, and the proposals that hold none of a kind the plan gives them. A run that reads
// this file sees what it cannot reach rather than reading nothing.
type loopSeedMissing struct {
	Issues        int `json:"issues"`
	Parts         int `json:"parts"`
	LastParts     int `json:"last_parts"`
	Threads       int `json:"threads"`
	Resolves      int `json:"resolves"`
	ReviewPages   int `json:"review_pages"`
	ReviewIndexes int `json:"review_indexes"`
	// The proposals the plan gives a record of a kind that hold none of it at all.
	ProposalsWithoutPart           int `json:"proposals_without_part"`
	ProposalsWithoutReviewPage     int `json:"proposals_without_review_page"`
	ProposalsWithoutReviewIndex    int `json:"proposals_without_review_index"`
	ProposalsWithoutOpenThread     int `json:"proposals_without_open_thread"`
	ProposalsWithoutResolvedThread int `json:"proposals_without_resolved_thread"`
}

// loopNeed is what a read or a write needs of the proposal it lands on. Each need is drawn over
// the proposals that hold it, so a verdict never lands on a proposal with no review page and a
// review-page read never renders an empty artifact id.
type loopNeed int

const (
	loopNeedAny loopNeed = iota
	loopNeedPart
	loopNeedLastPart
	loopNeedReviewPage
	loopNeedReviewIndex
	loopNeedOpenThread
	loopNeedResolvedThread
	loopNeedCount
)

// loopNeedNames is how a refusal names what no proposal holds.
var loopNeedNames = [loopNeedCount]string{
	"an issue key",
	"a part document",
	"a spare quote row on its last part document",
	"a review page",
	"a review index",
	"an open thread",
	"a resolved thread",
}

// serves reports whether the proposal holds what the need asks of it.
func (pr *loopSeedProposal) serves(need loopNeed) bool {
	switch need {
	case loopNeedPart:
		return len(pr.Parts) > 0
	case loopNeedLastPart:
		return pr.LastPart >= 0 && len(pr.SpareQuotes) > 0
	case loopNeedReviewPage:
		return len(pr.ReviewPages) > 0
	case loopNeedReviewIndex:
		return pr.ReviewIndex != ""
	case loopNeedOpenThread:
		return len(pr.OpenThreads) > 0
	case loopNeedResolvedThread:
		return len(pr.ResolvedThreads) > 0
	}
	return true
}

// loopPicks is the weighted draw of one need: the proposals that serve it, in the seed's order,
// and the running sum of their requirement-id counts.
type loopPicks struct {
	at    []int
	cum   []float64
	total float64
}

// --- the export ------------------------------------------------------------------------------

// loopIDRows is what readLoopIDs reads over: a pool or a connection, as loopQuerier is what the
// check reads one row over (loopseed_check.go).
type loopIDRows interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// readLoopIDs reads one plan's id map, its replies apart: a run addresses issues, part documents,
// thread roots, resolves, review pages and review indexes, and the loop's 130,066 reply rows are
// a quarter of the table that nothing here reads.
func readLoopIDs(ctx context.Context, q loopIDRows, planID string) (map[string]string, error) {
	rows, err := q.Query(ctx, `select path, local_id from loadtest.loop_ids where plan = $1 and kind <> $2`,
		planID, loopKindReply)
	if err != nil {
		return nil, fmt.Errorf("read loadtest.loop_ids: %w", err)
	}
	defer rows.Close()
	ids := map[string]string{}
	var path, local string
	for rows.Next() {
		if err := rows.Scan(&path, &local); err != nil {
			return nil, fmt.Errorf("read loadtest.loop_ids: %w", err)
		}
		ids[path] = local
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read loadtest.loop_ids: %w", err)
	}
	return ids, nil
}

// loopPlanRow is the source row one proposal was planned from, which loopStatuses draws that
// proposal's thread statuses again from: the plan carries the counts and never the statuses, and
// partThreads rebuilds the row the same way.
func loopPlanRow(pr *loopProposal) loopRow {
	return loopRow{IDs: pr.IDs, LiveIDs: pr.LiveIDs, Met: pr.Met, Rejected: pr.Rejected,
		Parked: pr.Parked, Withdrawn: pr.Withdrawn, Other: pr.Other,
		ThreadBytes: pr.ThreadBytes, ReplyLines: pr.ReplyLines}
}

// loopSample is at most n of ids, spread evenly over them: the i-th of n is ids[i*len(ids)/n], so
// a capped sample spans a proposal's threads rather than stopping at its first n. It draws no
// random number, so one id map gives one sample.
func loopSample(ids []string, n int) []string {
	if len(ids) <= n {
		return ids
	}
	out := make([]string, n)
	for i := range out {
		out[i] = ids[i*len(ids)/n]
	}
	return out
}

// buildLoopSeed joins the plan with the id map of the seed written from it. It refuses a map with
// no project row, since without the project key no read of the loop can name one.
func buildLoopSeed(plan string, p *loopPlan, ids map[string]string, threadsPer int) (*loopSeed, error) {
	planID := loopPlanID(p)
	project, ok := ids[loopKindProject]
	if !ok {
		return nil, fmt.Errorf("the id map holds no project of plan %s: write the seed first (loadgen loop-seed-write)", planID)
	}
	ls := &loopSeed{
		Plan: plan, PlanID: planID, Source: p.Source, SourceSHA256: p.SourceSHA256, Seed: p.Seed,
		Project: project, ThreadsPerProposal: threadsPer,
		SearchTerms: make([]string, 0, len(p.Proposals)),
		StatusTerms: loopStatusNames[:],
		Proposals:   make([]loopSeedProposal, 0, len(p.Proposals)),
	}
	pages := map[int][]loopReviewPage{}
	for _, page := range p.ReviewPages {
		pages[page.Proposal] = append(pages[page.Proposal], page)
	}
	indexes := map[int]bool{}
	for _, ix := range p.ReviewIndexes {
		indexes[ix.Proposal] = true
	}
	for i := range p.Proposals {
		pr := &p.Proposals[i]
		ls.SearchTerms = append(ls.SearchTerms, pr.Name)
		sp := loopSeedProposal{
			Index: i, Name: pr.Name, Key: ids[loopIssuePath(i)], LiveInDriver: pr.LiveInDriver,
			IDs: pr.IDs, LastPart: -1,
		}
		if sp.Key == "" {
			ls.Missing.Issues++
		}
		ls.exportParts(pr, &sp, ids)
		ls.exportThreads(p.Seed, pr, &sp, ids)
		ls.exportReview(pr, &sp, pages[i], indexes[i], ids)
		ls.Proposals = append(ls.Proposals, sp)
	}
	return ls, nil
}

// exportParts records the proposal's part documents in the plan's order, marks the last and takes
// the spare quote rows the planner reserved on it (loopSpareRow), which are the lines a
// requirement thread written during a run anchors on.
func (ls *loopSeed) exportParts(pr *loopProposal, sp *loopSeedProposal, ids map[string]string) {
	for part := range pr.Parts {
		id, ok := ids[loopPartPath(sp.Index, part)]
		if !ok {
			ls.Missing.Parts++
			continue
		}
		if part == len(pr.Parts)-1 {
			sp.LastPart = len(sp.Parts)
		}
		sp.Parts = append(sp.Parts, loopSeedPart{Index: part, ArtifactID: id, Slug: loopPartName(part)})
	}
	if len(pr.Parts) > 0 && len(sp.Parts) == 0 {
		ls.Missing.ProposalsWithoutPart++
	}
	if sp.LastPart < 0 {
		ls.Missing.LastParts++
		return
	}
	for s := range pr.Parts[len(pr.Parts)-1].SpareQuotes {
		sp.SpareQuotes = append(sp.SpareQuotes, loopSpareRow(sp.Index, s))
	}
}

// exportThreads records a sample of the proposal's thread roots, open and resolved apart. A thread
// is open when the planner left its id live, and resolved only when the id map holds the resolve
// the seed sent as well as the root: a root whose resolve was refused is open in Dispatch whatever
// its planned status, and a status change that reopened it would reopen nothing.
func (ls *loopSeed) exportThreads(seed uint64, pr *loopProposal, sp *loopSeedProposal, ids map[string]string) {
	if pr.IDs == 0 {
		return
	}
	statuses := loopStatuses(seed, sp.Index, loopPlanRow(pr))
	var open, resolved []string
	for part := range pr.Parts {
		pt := &pr.Parts[part]
		for j := pt.FirstThread; j < pt.FirstThread+pt.Threads; j++ {
			id, ok := ids[loopThreadPath(sp.Index, part, j)]
			if !ok {
				ls.Missing.Threads++
				continue
			}
			if statuses[j] == loopStatusLive {
				open = append(open, id)
				continue
			}
			if _, ok := ids[loopResolvePath(sp.Index, part, j)]; !ok {
				ls.Missing.Resolves++
				continue
			}
			resolved = append(resolved, id)
		}
	}
	sp.OpenThreads = loopSample(open, ls.ThreadsPerProposal)
	sp.ResolvedThreads = loopSample(resolved, ls.ThreadsPerProposal)
	if pr.LiveIDs > 0 && len(open) == 0 {
		ls.Missing.ProposalsWithoutOpenThread++
	}
	if pr.IDs > pr.LiveIDs && len(resolved) == 0 {
		ls.Missing.ProposalsWithoutResolvedThread++
	}
}

// exportReview records the live proposal's review pages, in the plan's order, each with the index
// and the byte size its text is drawn from, and its review index.
func (ls *loopSeed) exportReview(pr *loopProposal, sp *loopSeedProposal, pages []loopReviewPage, hasIndex bool, ids map[string]string) {
	for _, page := range pages {
		id, ok := ids[loopPagePath(sp.Index, page.Index)]
		if !ok {
			ls.Missing.ReviewPages++
			continue
		}
		sp.ReviewPages = append(sp.ReviewPages,
			loopSeedPage{Index: page.Index, ArtifactID: id, Bytes: page.Bytes})
	}
	if len(pages) > 0 && len(sp.ReviewPages) == 0 {
		ls.Missing.ProposalsWithoutReviewPage++
	}
	if !hasIndex {
		return
	}
	if id, ok := ids[loopIndexPath(sp.Index)]; ok {
		sp.ReviewIndex = id
		return
	}
	ls.Missing.ReviewIndexes++
	ls.Missing.ProposalsWithoutReviewIndex++
}

// --- what a run reads ------------------------------------------------------------------------

// readLoopSeed reads a loop seed loop-seed-export wrote. It refuses one that names no project or
// holds no proposal, since a step whose reads can name neither a project nor an issue has nothing
// to send.
func readLoopSeed(path string) (*loopSeed, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read loop seed: %w", err)
	}
	var ls loopSeed
	if err := json.Unmarshal(b, &ls); err != nil {
		return nil, fmt.Errorf("loop seed %s: %w", path, err)
	}
	if ls.Project == "" {
		return nil, fmt.Errorf("loop seed %s names no project", path)
	}
	if len(ls.Proposals) == 0 {
		return nil, fmt.Errorf("loop seed %s holds no proposal", path)
	}
	if len(ls.SearchTerms) == 0 || len(ls.StatusTerms) == 0 {
		return nil, fmt.Errorf("loop seed %s holds no search term or no status term", path)
	}
	return &ls, nil
}

// prepare drops the proposals the export could not name an issue key for - a write the deployment
// refused leaves no issue to read or to write - and builds the weighted draw of every need over
// the rest. It answers how many it dropped, which the step logs. Every later index into Proposals,
// the step's seedFile among them, is an index of what prepare left.
func (ls *loopSeed) prepare() int {
	before := len(ls.Proposals)
	ls.Proposals = slices.DeleteFunc(ls.Proposals, func(pr loopSeedProposal) bool { return pr.Key == "" })
	// Built from nothing every time, so a seed prepared again is prepared, not prepared twice.
	ls.picks = [loopNeedCount]loopPicks{}
	for need := loopNeedAny; need < loopNeedCount; need++ {
		p := &ls.picks[need]
		for i := range ls.Proposals {
			if !ls.Proposals[i].serves(need) {
				continue
			}
			p.at = append(p.at, i)
			p.total += float64(ls.Proposals[i].IDs)
			p.cum = append(p.cum, p.total)
		}
	}
	// The two term lists are fixed, so each term is escaped once here rather than on every draw.
	ls.searchEsc = make([]string, len(ls.SearchTerms))
	for i, term := range ls.SearchTerms {
		ls.searchEsc[i] = url.QueryEscape(term)
	}
	ls.statusEsc = make([]string, len(ls.StatusTerms))
	for i, term := range ls.StatusTerms {
		ls.statusEsc[i] = url.QueryEscape(term)
	}
	return before - len(ls.Proposals)
}

// servingProposals is how many proposals hold what one need asks, which is 0 for a need the seed
// cannot serve at all.
func (ls *loopSeed) servingProposals(need loopNeed) int { return len(ls.picks[need].at) }

// pick maps x in [0, 1) to one of the proposals that serve the need, in proportion to its
// requirement ids: the loop's load is its requirements, so a proposal of 4,000 ids takes four
// hundred times the reads and writes of one of 10. A seed whose proposals hold no requirement id
// at all - only a test builds one - draws over them evenly, so a step still sends something.
// prepare must have run, and the need must be one some proposal serves (checkSeedMix and
// checkRouteMixSeed refuse a step whose mix asks for a need no proposal holds).
func (ls *loopSeed) pick(need loopNeed, x float64) int {
	p := &ls.picks[need]
	if p.total <= 0 {
		return p.at[min(int(x*float64(len(p.at))), len(p.at)-1)]
	}
	target := x * p.total
	i := sort.Search(len(p.cum), func(i int) bool { return target < p.cum[i] })
	return p.at[min(i, len(p.at)-1)]
}

// refuseKind says why this loop seed cannot serve one write kind, "" when it can (mixSeed). A loop
// seed serves the loop's own three kinds and a plain comment; the other four read records of
// seed.json's shape - a spec quote, a seeded root comment, a document with a revision token in it -
// which no proposal of the loop holds.
func (ls *loopSeed) refuseKind(kind string) string {
	switch kind {
	case kindComment:
		return ""
	case kindVerdict:
		return ls.refuseNeed(loopNeedReviewPage, "a verdict writes a new version of a review page")
	case kindRequirement:
		return ls.refuseNeed(loopNeedLastPart, "a requirement thread anchors on a spare quote row of a last part document")
	case kindStatus:
		if why := ls.refuseNeed(loopNeedOpenThread, "a status change replies in an open thread and resolves it"); why != "" {
			return why
		}
		return ls.refuseNeed(loopNeedResolvedThread, "a status change of 1.2% replies alone in a resolved thread, which reopens it")
	}
	return fmt.Sprintf("a loop seed holds nothing a %s write reads: its kinds are %s, %s, %s and %s",
		kind, kindComment, kindVerdict, kindRequirement, kindStatus)
}

func (ls *loopSeed) refuseNeed(need loopNeed, why string) string {
	if ls.servingProposals(need) > 0 {
		return ""
	}
	return fmt.Sprintf("no proposal holds %s: %s", loopNeedNames[need], why)
}

// seedFile is the step's issue list: one issue per proposal, in the seed's own order, so that
// index i of the step's issues is proposal i of the loop seed. The issues are not sorted by key as
// readSeed sorts seed.json's, since that order is the plan's and the weighted draw addresses it.
// An issue's document is the proposal's first part document. Under a loop seed a read of
// {artifact_id} is drawn for a proposal that holds a part and renders that drawn part's id
// (renderLoop), so this one is what a reader of the step's issue list sees, not what the read
// lands on.
func (ls *loopSeed) seedFile() seedFile {
	sf := seedFile{Projects: []string{ls.Project}, Issues: make([]seedIssue, len(ls.Proposals))}
	for i := range ls.Proposals {
		pr := &ls.Proposals[i]
		is := seedIssue{Key: pr.Key, Project: ls.Project, SpecName: specName}
		if len(pr.Parts) > 0 {
			is.ArtifactID = pr.Parts[0].ArtifactID
		}
		sf.Issues[i] = is
	}
	return sf
}

// searchTerm and statusTerm are the terms a requirement query reads by, drawn from the seed's two
// lists. A proposal name carries spaces and punctuation, so prepare escaped every term for the
// query it lands in.
func (ls *loopSeed) searchTerm(r *rand.Rand) string {
	return ls.searchEsc[r.IntN(len(ls.searchEsc))]
}

func (ls *loopSeed) statusTerm(r *rand.Rand) string {
	return ls.statusEsc[r.IntN(len(ls.statusEsc))]
}

// --- the subcommand --------------------------------------------------------------------------

// loopSeedExportFlags are loop-seed-export's own flags. The Postgres URL carries the database's
// password, so it has the file form every credential of this package has (readCredentials):
// vm/vm.sh runs this phase on the pod that drives the deployment, where a value would be an
// argument anything that can list processes reads.
type loopSeedExportFlags struct {
	plan, pg, pgFile, out string
	threads               int
}

func (f *loopSeedExportFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.plan, "plan", "", "the plan the seed was written from (loopseed.plan.json)")
	fs.StringVar(&f.pg, "pg", "", "direct Postgres URL of the database loadtest.loop_ids was written to")
	fs.StringVar(&f.pgFile, "pg-file", "", "file holding the -pg value, so no password reaches the command line")
	fs.StringVar(&f.out, "out", "", "the loop seed's path (default $LOADTEST_ROOT/"+loopSeedFileName+")")
	fs.IntVar(&f.threads, "threads-per-proposal", loopThreadSample, "open and resolved thread ids recorded per proposal")
}

// resolve reads the credential file, judges every value and fills -out's default, before the
// first connection.
func (f *loopSeedExportFlags) resolve() error {
	if err := readCredentials(credentialFile{"-pg", "-pg-file", f.pgFile, &f.pg}); err != nil {
		return err
	}
	if f.plan == "" || f.pg == "" {
		return fmt.Errorf("-plan and -pg (or -pg-file) are required")
	}
	if _, ok := parsePG(f.pg); !ok {
		return fmt.Errorf("-pg %s is not a Postgres URL", redactPG(f.pg))
	}
	if f.threads < 1 {
		return fmt.Errorf("-threads-per-proposal must be at least 1")
	}
	if f.out == "" {
		f.out = filepath.Join(loadtestRoot(), loopSeedFileName)
	}
	return nil
}

// runLoopSeedExport writes what a run needs to drive a written loop seed: the id map joined to the
// plan it was written from.
func runLoopSeedExport(args []string) error {
	fs := flag.NewFlagSet("loop-seed-export", flag.ExitOnError)
	var f loopSeedExportFlags
	f.register(fs)
	_ = fs.Parse(args)

	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if err := f.resolve(); err != nil {
		return err
	}
	p, err := readLoopPlan(f.plan)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.out), 0o755); err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, f.pg)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	ids, err := readLoopIDs(ctx, conn, loopPlanID(p))
	if err != nil {
		return err
	}
	ls, err := buildLoopSeed(f.plan, p, ids, f.threads)
	if err != nil {
		return err
	}
	lg := log.New(os.Stderr, "loadgen loop-seed-export: ", 0)
	writeJSONFile(f.out, ls, 0o644, lg)
	lg.Printf("%d proposals of project %s, %d id-map rows, missing %s: %s",
		len(ls.Proposals), ls.Project, len(ids), ls.Missing.summary(), f.out)
	return nil
}

// summary is the missing counts in one line, "none" when the export filled everything.
func (m loopSeedMissing) summary() string {
	var parts []string
	for _, c := range []struct {
		name string
		n    int
	}{
		{"issues", m.Issues}, {"parts", m.Parts}, {"last parts", m.LastParts},
		{"threads", m.Threads}, {"resolves", m.Resolves},
		{"review pages", m.ReviewPages}, {"review indexes", m.ReviewIndexes},
	} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.name))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}
