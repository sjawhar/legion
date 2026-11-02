package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func synthIssues(n int, exists func(int) bool) replayState {
	st := replayState{Project: "PRJ"}
	for i := 1; i <= n; i++ {
		is := replayIssue{Key: "PRJ-" + strconv.Itoa(i), Number: i, Exists: exists(i)}
		if is.Exists {
			is.Title = "synthetic " + strconv.Itoa(i)
		}
		st.Issues = append(st.Issues, is)
	}
	return st
}

func TestPlanIssuesPlaceholders(t *testing.T) {
	st := synthIssues(5, func(i int) bool { return i != 3 })
	plans, err := planIssues(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	var placeholders []string
	for _, p := range plans {
		if p.placeholder() {
			placeholders = append(placeholders, p.Key)
		}
	}
	if !slices.Equal(placeholders, []string{"PRJ-3"}) || len(plans) != 5 {
		t.Fatalf("without keys: %d plans, placeholders %v", len(plans), placeholders)
	}

	plans, err = planIssues(st, map[string]bool{"PRJ-2": true, "PRJ-5": true})
	if err != nil {
		t.Fatal(err)
	}
	var seeded []string
	for i, p := range plans {
		if p.Number != i+1 {
			t.Fatalf("plan %d has number %d: numbering must hold", i, p.Number)
		}
		if !p.placeholder() {
			seeded = append(seeded, p.Key)
		}
	}
	if !slices.Equal(seeded, []string{"PRJ-2", "PRJ-5"}) || len(plans) != 5 {
		t.Fatalf("with keys: %d plans, seeded %v", len(plans), seeded)
	}

	if _, err := planIssues(st, map[string]bool{"PRJ-3": true}); err == nil {
		t.Fatal("a key the bundle holds no issue for must be refused")
	}
	st.Issues[1].Number = 7
	if _, err := planIssues(st, nil); err == nil {
		t.Fatal("a gap in the numbering must be refused")
	}
}

func TestReadKeysFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys")
	if err := os.WriteFile(path, []byte("PRJ-1, PRJ-2\n# comment\nPRJ-3 # trailing\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keys, err := readKeysFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 3 || !keys["PRJ-1"] || !keys["PRJ-2"] || !keys["PRJ-3"] {
		t.Fatalf("keys %v", keys)
	}
}

func cmt(id string, at int, mods ...func(*replayComment)) replayComment {
	c := replayComment{ID: id, CreatedAt: tAt.Add(time.Duration(at) * time.Minute), Actor: replayActor{Kind: "session", ID: "s1"}, Body: "body " + id, Open: true}
	for _, m := range mods {
		m(&c)
	}
	return c
}

func replyTo(root string) func(*replayComment) {
	return func(c *replayComment) { c.ReplyTo = new(root); c.ThreadRootID = new(root) }
}
func anchoredOn(doc, quote string) func(*replayComment) {
	return func(c *replayComment) { c.Anchor = &replayAnchor{ArtifactID: doc, Quote: quote} }
}
func resolvedC(windowRefs bool) func(*replayComment) {
	return func(c *replayComment) {
		c.Open = false
		c.WindowRefs = windowRefs
		c.ResolvedBy = &replayActor{Kind: "session", ID: "s2"}
	}
}
func onAsk(ask string) func(*replayComment) { return func(c *replayComment) { c.AskID = new(ask) } }

func TestPlanCommentsSelectionAndOrder(t *testing.T) {
	// ask-gone is a resolved typed ask block the bundle keeps: its thread is skipped like a thread
	// on an ask the bundle does not hold.
	is := &replayIssue{Key: "PRJ-1", Asks: []replayAsk{{ID: "ask-open"}, {ID: "ask-gone", State: "resolved", BlockID: new("b1")}}, Comments: []replayComment{
		cmt("r1", 1, anchoredOn("d1", "quote one")),
		cmt("r2", 2, resolvedC(false)),
		cmt("r3", 3, resolvedC(true), anchoredOn("d1", "quote three")),
		cmt("x1", 4, replyTo("r1")),
		cmt("x2", 5, replyTo("r2")),
		cmt("x3", 6, replyTo("r3"), resolvedC(false)),
		cmt("r4", 7),
		cmt("a1", 8, onAsk("ask-open")),
		cmt("a2", 9, onAsk("ask-gone")),
		cmt("x4", 10, replyTo("a1")),
		cmt("x5", 11, replyTo("a2")),
		cmt("x6", 12, replyTo("missing")),
	}}
	plan := planComments(is)
	var main []string
	for _, s := range plan.Main {
		main = append(main, s.C.ID+":"+string(s.Kind)+":"+s.RootDoc)
	}
	want := []string{"r1:comment-anchored:d1", "r3:comment-anchored:d1", "x1:comment-reply:d1", "x3:comment-reply:d1", "r4:comment-plain:"}
	if !slices.Equal(main, want) {
		t.Fatalf("main\n got %v\nwant %v", main, want)
	}
	var onAskIDs []string
	for _, s := range plan.ByAsk["ask-open"] {
		onAskIDs = append(onAskIDs, s.C.ID)
	}
	if !slices.Equal(onAskIDs, []string{"a1", "x4"}) || len(plan.ByAsk) != 1 {
		t.Fatalf("by ask %v", plan.ByAsk)
	}
	if len(plan.Resolve) != 1 || plan.Resolve[0].ID != "r3" {
		t.Fatalf("resolve %v", plan.Resolve)
	}
	if plan.SkippedResolved != 2 || plan.SkippedAsk != 2 || plan.SkippedOrphan != 1 {
		t.Fatalf("skipped resolved %d ask %d orphan %d", plan.SkippedResolved, plan.SkippedAsk, plan.SkippedOrphan)
	}
	if got := expectedOpenAnchored(is); got["d1"] != 1 || len(got) != 1 {
		t.Fatalf("expected open anchored %v: r1 stays open, r3 is resolved", got)
	}
}

func TestSplitAtAnswer(t *testing.T) {
	thread := []commentStep{{C: cmt("a", 1)}, {C: cmt("b", 3)}, {C: cmt("c", 5)}}
	before, after := splitAtAnswer(thread, &replayAskAnswer{At: tAt.Add(3 * time.Minute)})
	if len(before) != 1 || len(after) != 2 {
		t.Fatalf("before %d after %d", len(before), len(after))
	}
	before, after = splitAtAnswer(thread, nil)
	if len(before) != 3 || after != nil {
		t.Fatal("without an answer the whole thread comes first")
	}
}

const fallbackDoc = `---
title: Front matter sentence that must never be used here
---
# Heading line with words

The first plain sentence is long enough here. Second sentence of the same line goes on.
This line has *emphasis* and must be skipped entirely.
- A list item sentence is long enough too.

` + "```" + `
Code fence sentence that must never be used here.
` + "```" + `

:::ask{#b1 state="open"}
A directive body sentence that must never be used.
:::

Repeated sentence for testing duplicates. Repeated sentence for testing duplicates.
Short one.
`

func TestFallbackCandidates(t *testing.T) {
	got := fallbackCandidates(fallbackDoc)
	want := []string{"The first plain sentence is long enough here.", "Second sentence of the same line goes on.", "A list item sentence is long enough too."}
	if !slices.Equal(got, want) {
		t.Fatalf("candidates\n got %q\nwant %q", got, want)
	}
	for _, c := range got {
		if strings.Count(fallbackDoc, c) != 1 || strings.ContainsAny(c, fallbackSyntax) || len(c) < 20 || len(c) > 200 {
			t.Fatalf("candidate %q breaks a rule", c)
		}
	}
}

func TestFallbackOrderDeterministic(t *testing.T) {
	cands := fallbackCandidates(fallbackDoc)
	a := fallbackOrder(1, "doc-1", "comment-1", cands)
	if !slices.Equal(a, fallbackOrder(1, "doc-1", "comment-1", cands)) {
		t.Fatal("the same seed, document and item must draw the same order")
	}
	if len(a) != len(cands) {
		t.Fatalf("order of %d candidates has %d", len(cands), len(a))
	}
	differ := false
	for seed := uint64(2); seed < 40 && !differ; seed++ {
		differ = !slices.Equal(a, fallbackOrder(seed, "doc-1", "comment-1", cands))
	}
	if !differ {
		t.Fatal("the rng seed does not change the order")
	}
	many := make([]string, 50)
	for i := range many {
		many[i] = "candidate " + strconv.Itoa(i)
	}
	if n := len(fallbackOrder(1, "d", "c", many)); n != fallbackAttempts {
		t.Fatalf("fallback attempts are bounded at %d, got %d", fallbackAttempts, n)
	}
	if fallbackOrder(1, "d", "c", nil) != nil {
		t.Fatal("no candidates, no order")
	}
}

func TestLoggedQuoteOrFallback(t *testing.T) {
	text := "Alpha beta gamma delta epsilon.\n\nRepeat me here. Repeat me here.\n"
	if q, ok := loggedQuote(text, &replayAnchor{Quote: "beta gamma"}); !ok || q != "beta gamma" {
		t.Fatal("a quote occurring once is used")
	}
	for name, a := range map[string]*replayAnchor{
		"absent":    {Quote: "not in the text"},
		"ambiguous": {Quote: "Repeat me here."},
		"orphaned":  {Quote: "beta gamma", Orphaned: true},
		"empty":     {Quote: ""},
	} {
		if _, ok := loggedQuote(text, a); ok {
			t.Errorf("%s quote must fall back", name)
		}
	}
}

func TestApprovalSummaryAndReview(t *testing.T) {
	if s := approvalSummary("Approve spec.md (version 12)? Synthetic summary."); s != "Synthetic summary." {
		t.Fatalf("summary %q", s)
	}
	if s := approvalSummary("Approve spec.md (version 3)?"); s != "" {
		t.Fatalf("prefix alone gives %q", s)
	}
	body, err := reviewFor(&replayAskAnswer{Selected: []string{"Approve"}})
	if err != nil || body["state"] != "approved" {
		t.Fatalf("approve: %v %v", body, err)
	}
	if _, err := reviewFor(&replayAskAnswer{Selected: []string{"Request changes"}}); err == nil {
		t.Fatal("changes without a reason must be refused")
	}
	body, err = reviewFor(&replayAskAnswer{Selected: []string{"Request changes"}, Text: new("synthetic reason")})
	if err != nil || body["state"] != "changes_requested" || body["reason"] != "synthetic reason" {
		t.Fatalf("changes: %v %v", body, err)
	}
}

func TestIssuePatchAndClaim(t *testing.T) {
	want := &replayIssue{Status: "in_progress", Priority: new(2), Labels: []string{"b", "a"}, Parent: new("PRJ-1"),
		BlockedBy: []string{"PRJ-2"}, Route: new("session:s1"), Claim: &replayClaim{Actor: replayActor{Kind: "session", ID: "s1"}}}
	cur := liveIssue{Status: "triage", Labels: []string{}}
	patch := issuePatch(cur, want)
	for _, k := range []string{"status", "priority", "labels", "parent", "blocked_by", "route"} {
		if _, ok := patch[k]; !ok {
			t.Errorf("patch lacks %s", k)
		}
	}
	same := liveIssue{Status: "in_progress", Priority: new(2), Labels: []string{"a", "b"}, Parent: new("PRJ-1"), BlockedBy: []string{"PRJ-2"}, Route: new("session:s1")}
	if p := issuePatch(same, want); p != nil {
		t.Fatalf("an issue that already matches needs no patch, got %v", p)
	}
	if p := issuePatch(liveIssue{Status: "triage"}, nil); p["status"] != "done" || len(p) != 1 {
		t.Fatalf("placeholder patch %v", p)
	}
	if issuePatch(liveIssue{Status: "done"}, nil) != nil {
		t.Fatal("a done placeholder needs no patch")
	}
	if !claimWanted(same, want) {
		t.Fatal("an unclaimed issue wants its logged claim")
	}
	same.Claim = &struct {
		Actor replayActor `json:"actor"`
		At    time.Time   `json:"at"`
	}{Actor: replayActor{Kind: "session", ID: "s1"}}
	if claimWanted(same, want) {
		t.Fatal("an issue claimed by the logged actor wants no claim")
	}
}

func TestMaskAnsweredAt(t *testing.T) {
	md := `:::ask{#b1 state="answered" answered_at="2026-10-26T22:00:00Z" selected="[]"}` + "\nQ\n:::\n"
	masked, changed := maskAnsweredAt(md)
	if !changed || strings.Contains(masked, "2026") {
		t.Fatalf("masked %q", masked)
	}
	if _, changed := maskAnsweredAt("plain"); changed {
		t.Fatal("plain text has nothing to mask")
	}
}

func TestDistribution(t *testing.T) {
	med, mean := distribution([]int{1, 3, 10, 20}, []int{1, 1, 1, 7})
	if med != 6.5 || mean != (1+3+10+140)/10.0 {
		t.Fatalf("median %v mean %v", med, mean)
	}
	if med, mean := distribution(nil, nil); med != 0 || mean != 0 {
		t.Fatal("empty distribution")
	}
}
