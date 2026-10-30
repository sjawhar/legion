package main

// The replay seed's decisions of what to send, apart from sending it (replay_seed.go): which issues
// are seeded and which are placeholders, each document's text at each version of its history, which
// comments are recreated and in what order, where an anchor lands when its logged quote cannot hold
// it, and which issue fields the closing PATCH sends. Everything here is a function of the bundle,
// the keys file and the rng seed, so a resumed seed decides exactly what the first run decided.

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// replaySeedActorID is the session the seed's own synthetic writes act as: the project, the
// placeholders, documents, files and history edits, whose authors the bundle does not carry.
const replaySeedActorID = "ses-replay-seed"

// placeholderTitle is a placeholder issue's title: an issue that keeps the numbering and nothing else.
const placeholderTitle = "placeholder"

// padToken opens every padding paragraph the seed's history writes (padText), and no document it
// seeds holds it.
const padToken = "zqpad"

// seedIssuePlan is one issue number of the project: seeded from the bundle, or a placeholder.
type seedIssuePlan struct {
	Key    string
	Number int
	// Issue is the bundle's issue; nil for a placeholder.
	Issue *replayIssue
}

func (p seedIssuePlan) placeholder() bool { return p.Issue == nil }

// readKeysFile reads a keys file: issue keys separated by newlines, commas or blanks; # starts a
// comment that runs to the line's end.
func readKeysFile(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	keys := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "#")
		for _, k := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
			keys[k] = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("keys file %s names no issue", path)
	}
	return keys, nil
}

// planIssues lays out every issue number in order. An issue is seeded when it existed at the
// bundle's instant and, with a keys set, is listed; every other number is a placeholder, so
// recreating them in order reproduces each key. A keys set naming a key the bundle lacks is refused.
func planIssues(st replayState, keys map[string]bool) ([]seedIssuePlan, error) {
	plans := make([]seedIssuePlan, 0, len(st.Issues))
	seen := map[string]bool{}
	for i := range st.Issues {
		is := &st.Issues[i]
		if is.Number != i+1 || is.Key != fmt.Sprintf("%s-%d", st.Project, is.Number) {
			return nil, fmt.Errorf("state.json issue %d is %s (number %d): the issues must be every number from 1 in order", i+1, is.Key, is.Number)
		}
		p := seedIssuePlan{Key: is.Key, Number: is.Number}
		if is.Exists && (keys == nil || keys[is.Key]) {
			p.Issue = is
			seen[is.Key] = true
		}
		plans = append(plans, p)
	}
	var missing []string
	for k := range keys {
		if !seen[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("keys file names issues the bundle does not hold at its instant: %s", strings.Join(missing, ", "))
	}
	return plans, nil
}

// --- comments ------------------------------------------------------------------------------------

// commentStepKind is how a comment write is counted in the ops log.
type commentStepKind string

const (
	commentAnchored commentStepKind = "comment-anchored"
	commentReply    commentStepKind = "comment-reply"
	commentPlain    commentStepKind = "comment-plain"
)

// commentStep is one comment to recreate. RootDoc is the logged document its thread's root is
// anchored on, empty when the root is not anchored.
type commentStep struct {
	C       replayComment
	Kind    commentStepKind
	RootDoc string
}

// commentPlan is an issue's comments as the seed recreates them.
type commentPlan struct {
	// Main are the comments step 3 posts, in creation order.
	Main []commentStep
	// ByAsk are the threads on asks, posted once their ask exists (step 4), in creation order.
	ByAsk map[string][]commentStep
	// Resolve are the roots resolved at the bundle's instant that a window event refers to: they
	// are created with their replies, then resolved, in creation order.
	Resolve []replayComment
	// SkippedResolved counts the comments of resolved threads no window event refers to;
	// SkippedAsk the comments on asks not open at the instant; SkippedOrphan replies whose root
	// the issue does not hold.
	SkippedResolved, SkippedAsk, SkippedOrphan int
}

// planComments selects and orders an issue's comments: every root open at the instant with its
// replies, and every resolved root a window event refers to (created, then resolved) with its
// replies; the replies of other resolved roots are skipped. A thread on an ask is posted with the
// ask when the ask was open at the instant (it is seeded), and skipped otherwise.
func planComments(is *replayIssue) commentPlan {
	plan := commentPlan{ByAsk: map[string][]commentStep{}}
	openAsk := map[string]bool{}
	for _, a := range is.Asks {
		openAsk[a.ID] = a.State != "resolved"
	}
	roots := map[string]*replayComment{}
	for i := range is.Comments {
		if c := &is.Comments[i]; c.ReplyTo == nil {
			roots[c.ID] = c
		}
	}
	for _, c := range is.Comments {
		root := &c
		if c.ReplyTo != nil {
			id := *c.ReplyTo
			if c.ThreadRootID != nil {
				id = *c.ThreadRootID
			}
			if root = roots[id]; root == nil {
				plan.SkippedOrphan++
				continue
			}
		}
		if !root.Open && !root.WindowRefs {
			plan.SkippedResolved++
			continue
		}
		if root.AskID != nil && !openAsk[*root.AskID] {
			plan.SkippedAsk++
			continue
		}
		step := commentStep{C: c, Kind: commentPlain}
		if root.Anchor != nil {
			step.RootDoc = root.Anchor.ArtifactID
		}
		switch {
		case c.ReplyTo != nil:
			step.Kind = commentReply
		case c.Anchor != nil:
			step.Kind = commentAnchored
		}
		if root.AskID != nil {
			plan.ByAsk[*root.AskID] = append(plan.ByAsk[*root.AskID], step)
		} else {
			plan.Main = append(plan.Main, step)
		}
		if c.ReplyTo == nil && !c.Open {
			plan.Resolve = append(plan.Resolve, c)
		}
	}
	return plan
}

// splitAtAnswer splits an ask's thread at its answer: the comments made before the answer, then
// the rest. Without an answer every comment comes before it.
func splitAtAnswer(thread []commentStep, answer *replayAskAnswer) (before, after []commentStep) {
	if answer == nil {
		return thread, nil
	}
	for i, s := range thread {
		if !s.C.CreatedAt.Before(answer.At) {
			return thread[:i], thread[i:]
		}
	}
	return thread, nil
}

// --- anchors -------------------------------------------------------------------------------------

// loggedQuote is the logged quote an anchor is recreated on, when it can hold it: it is not
// orphaned and occurs exactly once in the document's text.
func loggedQuote(text string, a *replayAnchor) (string, bool) {
	if a == nil || a.Orphaned || a.Quote == "" || strings.Count(text, a.Quote) != 1 {
		return "", false
	}
	return a.Quote, true
}

// fallbackSyntax are the characters a fallback sentence never holds: markdown syntax, HTML and
// escapes, so the sentence reads the same in the markdown and in the document's text.
const fallbackSyntax = "`*_~[](){}<>#|\\!:=&\"$^@"

// blockMarker is a list item's or a block quote's marker at a line's start.
var blockMarker = regexp.MustCompile(`^(?:[-*+]|\d+[.)]|>)\s+`)

// fallbackCandidates are the sentences of text an anchor can fall back to, in text order: plain
// text of 20 to 200 bytes outside code fences, front matter and directive blocks, starting with a
// letter, holding no markdown syntax character, tab or double blank, and occurring exactly once in
// the text.
func fallbackCandidates(text string) []string {
	var out []string
	seen := map[string]bool{}
	inFence, inFront := false, false
	directive := 0
	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if i == 0 && trimmed == "---" {
			inFront = true
			continue
		}
		if inFront {
			if trimmed == "---" {
				inFront = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(trimmed, ":::") {
			if trimmed == ":::" {
				directive = max(0, directive-1)
			} else {
				directive++
			}
			continue
		}
		if directive > 0 || strings.HasPrefix(line, "    ") {
			continue
		}
		for loc := blockMarker.FindStringIndex(trimmed); loc != nil; loc = blockMarker.FindStringIndex(trimmed) {
			trimmed = trimmed[loc[1]:]
		}
		// A table row's cells are each a segment of their own.
		segments := []string{trimmed}
		if strings.HasPrefix(trimmed, "|") {
			segments = strings.Split(trimmed, "|")
		}
		var lineSentences []string
		for _, seg := range segments {
			lineSentences = append(lineSentences, sentences(strings.TrimSpace(seg))...)
		}
		for _, s := range lineSentences {
			if len(s) < 20 || len(s) > 200 || seen[s] {
				continue
			}
			seen[s] = true
			r := s[0]
			if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') || strings.ContainsAny(s, fallbackSyntax+"\t") ||
				strings.Contains(s, "  ") || strings.Contains(s, padToken) || strings.Count(text, s) != 1 {
				continue
			}
			out = append(out, s)
		}
	}
	return out
}

// sentences cuts a line after each ". ", "? " and "; ", keeping the punctuation.
func sentences(line string) []string {
	var out []string
	for line != "" {
		cut := -1
		for i := 0; i+1 < len(line); i++ {
			if (line[i] == '.' || line[i] == '?' || line[i] == ';') && line[i+1] == ' ' {
				cut = i + 1
				break
			}
		}
		if cut < 0 {
			out = append(out, strings.TrimSpace(line))
			break
		}
		out = append(out, strings.TrimSpace(line[:cut]))
		line = line[cut:]
	}
	return out
}

// fallbackAttempts bounds how many fallback sentences one anchor tries before the seed stops.
const fallbackAttempts = 8

// fallbackOrder is the order in which an anchored item tries the candidates, at most
// fallbackAttempts of them: a permutation drawn by a PCG keyed by the rng seed and the document id
// (the stream) and the item's logged id, so the choice never depends on which items a resumed seed
// already wrote.
func fallbackOrder(rngSeed uint64, docID, itemID string, candidates []string) []string {
	if len(candidates) == 0 {
		return nil
	}
	h := fnv.New64a()
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], rngSeed)
	h.Write(b[:])
	h.Write([]byte(docID))
	stream := h.Sum64()
	h.Reset()
	h.Write([]byte(itemID))
	r := rand.New(rand.NewPCG(stream, h.Sum64()))
	perm := r.Perm(len(candidates))
	out := make([]string, 0, min(len(perm), fallbackAttempts))
	for _, i := range perm[:min(len(perm), fallbackAttempts)] {
		out = append(out, candidates[i])
	}
	return out
}

// --- asks ----------------------------------------------------------------------------------------

var approvalPrefix = regexp.MustCompile(`^Approve .* \(version \d+\)\?( |$)`)

// approvalSummary is the summary an approval request re-sends: the logged question after the
// server-owned "Approve <name> (version N)?" prefix; empty when the question is the prefix alone.
func approvalSummary(question string) string {
	loc := approvalPrefix.FindStringIndex(question)
	if loc == nil {
		return strings.TrimSpace(question)
	}
	return strings.TrimSpace(question[loc[1]:])
}

// reviewFor maps an approval ask's answer onto POST /api/v1/artifacts/{id}/reviews.
func reviewFor(ans *replayAskAnswer) (map[string]any, error) {
	if ans == nil || len(ans.Selected) != 1 {
		return nil, fmt.Errorf("an approval answer selects exactly one option")
	}
	switch ans.Selected[0] {
	case "Approve":
		body := map[string]any{"state": "approved"}
		if ans.Text != nil && strings.TrimSpace(*ans.Text) != "" {
			body["reason"] = *ans.Text
		}
		return body, nil
	case "Request changes":
		if ans.Text == nil || strings.TrimSpace(*ans.Text) == "" {
			return nil, fmt.Errorf("a request for changes needs the answer's text as its reason")
		}
		return map[string]any{"state": "changes_requested", "reason": *ans.Text}, nil
	}
	return nil, fmt.Errorf("approval option %d characters long is neither Approve nor Request changes", len(ans.Selected[0]))
}

// --- issue fields --------------------------------------------------------------------------------

// liveIssue is the part of GET /api/v1/issues/{key} the closing PATCH compares.
type liveIssue struct {
	Status    string   `json:"status"`
	Priority  *int     `json:"priority"`
	Labels    []string `json:"labels"`
	Parent    *string  `json:"parent"`
	BlockedBy []string `json:"blocked_by"`
	Route     *string  `json:"route"`
	Claim     *struct {
		Actor replayActor `json:"actor"`
		At    time.Time   `json:"at"`
	} `json:"claim"`
}

// issuePatch is the closing PATCH body: each field of want that differs from the issue as it
// stands, nil when none does. A placeholder wants only status done.
func issuePatch(cur liveIssue, want *replayIssue) map[string]any {
	body := map[string]any{}
	if want == nil {
		if cur.Status != "done" {
			body["status"] = "done"
		}
		return nilIfEmpty(body)
	}
	if want.Status != "" && want.Status != cur.Status {
		body["status"] = want.Status
	}
	if !equalIntPtr(want.Priority, cur.Priority) {
		body["priority"] = want.Priority
	}
	if !sameSet(want.Labels, cur.Labels) {
		body["labels"] = nonNil(want.Labels)
	}
	if !equalStrPtr(want.Parent, cur.Parent) {
		body["parent"] = want.Parent
	}
	if !sameSet(want.BlockedBy, cur.BlockedBy) {
		body["blocked_by"] = nonNil(want.BlockedBy)
	}
	if want.Route != nil && !equalStrPtr(want.Route, cur.Route) {
		body["route"] = *want.Route
	}
	return nilIfEmpty(body)
}

func nilIfEmpty(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	return m
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func equalIntPtr(a, b *int) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func equalStrPtr(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

// claimWanted says whether the issue still needs its logged claim: it had one at the instant and
// the claim in force is not that actor's.
func claimWanted(cur liveIssue, want *replayIssue) bool {
	if want == nil || want.Claim == nil {
		return false
	}
	return cur.Claim == nil || cur.Claim.Actor.Kind != want.Claim.Actor.Kind || cur.Claim.Actor.ID != want.Claim.Actor.ID
}

// --- checks --------------------------------------------------------------------------------------

var (
	answeredAtAttr = regexp.MustCompile(`answered_at="[^"]*"`)
	// resolvedBlock matches a typed ask block directive whose state is resolved.
	resolvedBlock = regexp.MustCompile(`(?m)^:::ask\{#[^}]* state="resolved"`)
)

// maskAnsweredAt blanks every typed ask block's answered_at attribute, which the seed cannot set
// through the API (the answer's time is the seed's own).
func maskAnsweredAt(markdown string) (string, bool) {
	masked := answeredAtAttr.ReplaceAllString(markdown, `answered_at=""`)
	return masked, masked != markdown
}

// expectedOpenAnchored is, per logged document id, the open anchored items steps 3 and 4 leave on
// it: the anchored roots recreated and not resolved, and the anchored asks recreated through
// POST /asks (typed ask blocks and approval asks carry no anchor).
func expectedOpenAnchored(is *replayIssue) map[string]int {
	out := map[string]int{}
	plan := planComments(is)
	resolved := map[string]bool{}
	for _, c := range plan.Resolve {
		resolved[c.ID] = true
	}
	count := func(steps []commentStep) {
		for _, s := range steps {
			if s.Kind == commentAnchored && !resolved[s.C.ID] {
				out[s.C.Anchor.ArtifactID]++
			}
		}
	}
	count(plan.Main)
	for _, thread := range plan.ByAsk {
		count(thread)
	}
	for _, a := range is.Asks {
		if a.BlockID == nil && a.Approval == nil && a.Anchor != nil {
			out[a.Anchor.ArtifactID]++
		}
	}
	return out
}

// windowWrites are the window writes production counts toward a document's load: each
// artifact.version, or a comment.created with an anchor or a reply_to.
type windowWrites struct {
	// Writes counts those events per issue key; AnchoredRoots counts the anchored roots
	// (comment.created with an anchor and no reply_to) per logged document id.
	Writes        map[string]int
	AnchoredRoots map[string]int
}

func countWindowWrites(window []replayEvent) windowWrites {
	ww := windowWrites{Writes: map[string]int{}, AnchoredRoots: map[string]int{}}
	for _, ev := range window {
		if ev.IssueKey == nil {
			continue
		}
		switch ev.Type {
		case evArtifactVersion:
			ww.Writes[*ev.IssueKey]++
		case evCommentCreated:
			var p struct {
				Anchor  *replayAnchor `json:"anchor"`
				ReplyTo *string       `json:"reply_to"`
			}
			if json.Unmarshal(ev.Payload, &p) != nil || (p.Anchor == nil && p.ReplyTo == nil) {
				continue
			}
			ww.Writes[*ev.IssueKey]++
			if p.Anchor != nil && p.ReplyTo == nil {
				ww.AnchoredRoots[p.Anchor.ArtifactID]++
			}
		}
	}
	return ww
}

// distribution is the median and the weighted mean of values.
func distribution(values []int, weights []int) (median, weightedMean float64) {
	if len(values) == 0 {
		return 0, 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	mid := len(sorted) / 2
	median = float64(sorted[mid])
	if len(sorted)%2 == 0 {
		median = float64(sorted[mid-1]+sorted[mid]) / 2
	}
	var sum, wsum float64
	for i, v := range values {
		sum += float64(v) * float64(weights[i])
		wsum += float64(weights[i])
	}
	if wsum > 0 {
		weightedMean = sum / wsum
	}
	return median, weightedMean
}
