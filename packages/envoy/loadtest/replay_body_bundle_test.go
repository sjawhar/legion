package main

import (
	"encoding/json"
	"flag"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// localPlaceholder stands for every local id a body names (a Dispatch uuid's length).
const localPlaceholder = "00000000-0000-0000-0000-000000000000"

// bodySizes keeps the largest JSON body of each kind and the largest document, and the bodies
// and documents over maxJSONBody.
type bodySizes struct {
	t       *testing.T
	largest map[string]sizeAt
	doc     sizeAt
	over    int
}

type sizeAt struct {
	bytes int
	where string
}

// body measures v as the request carries it (json.Marshal, as jsonBody and api.call encode it),
// and the document a create or an upload carries in content or spec.
func (b *bodySizes) body(kind, where string, v map[string]any) {
	raw, err := json.Marshal(v)
	if err != nil {
		b.t.Fatalf("%s %s: %v", kind, where, err)
	}
	if n := len(raw); n > b.largest[kind].bytes {
		b.largest[kind] = sizeAt{n, where}
	}
	if len(raw) > maxJSONBody {
		b.over++
		b.t.Errorf("%s %s: a JSON body of %d bytes, over %d", kind, where, len(raw), maxJSONBody)
	}
	for _, field := range []string{"content", "spec"} {
		if text, ok := v[field].(string); ok {
			b.document(kind+" "+field, where, len(text))
		}
	}
}

func (b *bodySizes) document(what, where string, n int) {
	if n > b.doc.bytes {
		b.doc = sizeAt{n, where}
	}
	if n > maxJSONBody {
		b.over++
		b.t.Errorf("%s %s: a document of %d bytes, over %d", what, where, n, maxJSONBody)
	}
}

// TestRequestBodiesBundle plans, over the bundle LOADTEST_REPLAY_BUNDLE names (skipped without
// it), every JSON request body loadgen replay-seed sends with every issue seeded and every one the
// replay source sends at rate 1 (loadgen replay-plan -replay-rate 1, its other flags at their
// defaults), and fails on a body over maxJSONBody (Dispatch's 413 REQUEST_TOO_LARGE) or a document
// over it at any planned version (413 CAP_EXCEEDED). A local id is a uuid-long placeholder. It logs
// the largest body of each kind: kinds, keys, ids and sizes, never a text.
func TestRequestBodiesBundle(t *testing.T) {
	dir := os.Getenv("LOADTEST_REPLAY_BUNDLE")
	if dir == "" {
		t.Skip("LOADTEST_REPLAY_BUNDLE is not set")
	}
	b, err := readBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	sizes := &bodySizes{t: t, largest: map[string]sizeAt{}}
	plans, err := planIssues(b.State, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &replaySeeder{b: b, rngSeed: 1}
	for _, p := range plans {
		seedBodies(t, s, p, sizes)
	}

	fs := flag.NewFlagSet("replay-plan", flag.ContinueOnError)
	rf := registerReplayFlags(fs)
	if err := fs.Parse([]string{"-replay-inputs", dir, "-replay-rate", "1"}); err != nil {
		t.Fatal(err)
	}
	o, err := rf.options(1)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planReplay(b, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, act := range plan.Actions {
		local := map[replayRef]string{}
		for _, r := range act.refs() {
			local[r] = localPlaceholder
		}
		for _, stage := range act.Stages {
			for _, r := range stage {
				where := act.Issue + " event " + strconv.FormatInt(act.EventID, 10)
				if r.Body != nil {
					body, _ := renderBody(r.Body, local, &replayActionState{}).(map[string]any)
					sizes.body("replay "+act.Kind, where, body)
				}
				if r.File != nil && strings.HasPrefix(r.File.MIME, "text/markdown") {
					sizes.document("replay "+act.Kind+" file", where, r.File.Size)
				}
			}
		}
	}

	for _, kind := range slices.Sorted(maps.Keys(sizes.largest)) {
		l := sizes.largest[kind]
		t.Logf("largest %-32s %8d bytes (%s)", kind, l.bytes, l.where)
	}
	t.Logf("largest document %d bytes (%s); %d seed issues, %d replay actions; %d over %d", sizes.doc.bytes, sizes.doc.where, len(plans), len(plan.Actions), sizes.over, maxJSONBody)
}

// seedBodies measures every JSON body the seed sends for p, as replay_seed.go builds it: the
// issue's create, its documents' creates and histories (each padding edit over the padding the
// edit before it left, as Dispatch stores the words sent; the restore; the ask steps' answers and
// resolves), its comments (an anchored one with every quote anchored may try), its asks, their
// answers and resolves, and its fields and claim.
func seedBodies(t *testing.T, s *replaySeeder, p seedIssuePlan, sizes *bodySizes) {
	_, body, _, err := s.issueBody(p)
	if err != nil {
		t.Errorf("%s: %v", p.Key, err)
		return
	}
	sizes.body("seed issue-create", p.Key, body)
	if p.placeholder() {
		return
	}
	is := p.Issue
	docs := map[string]*issueDoc{}
	for i := range is.Artifacts {
		a := &is.Artifacts[i]
		where := p.Key + " " + a.ID[:8]
		if a.Kind != "doc" {
			if a.Size != nil && a.MIME != nil && strings.HasPrefix(*a.MIME, "text/markdown") {
				sizes.document("seed file-create", where, *a.Size)
			}
			continue
		}
		text, err := s.docText(a)
		if err != nil {
			t.Errorf("%s: %v", where, err)
			continue
		}
		plan, err := docPlan(is, a, text)
		if err != nil {
			t.Errorf("%s: %v", where, err)
			continue
		}
		docs[a.ID] = &issueDoc{a: a, text: text, plan: plan, is: is}
		first := firstVersionText(is, a, text, plan)
		if !a.Primary {
			sizes.body("seed doc-create", where, docCreateBody(a.Name, first))
		}
		for n, size := range plan.Sizes {
			sizes.document("seed planned version", where+" v"+strconv.Itoa(n+1), size)
		}
		pad := currentPad(first)
		for n := 2; n <= plan.Final; n++ {
			at := where + " v" + strconv.Itoa(n)
			switch {
			case len(plan.AskAt[n]) > 0:
				for _, st := range plan.AskAt[n] {
					askBodies(t, s, is.Asks[st.Ask], docs, sizes, st.Resolve)
				}
			case n == plan.Restore:
				padBlock, before := "", ""
				if pad != "" {
					padBlock = localPlaceholder
				}
				if plan.RunEnd < len(text) {
					before = localPlaceholder
				}
				sizes.body("seed history-edit restore", at, historyEditBody(restoreEditOps(padBlock, text[plan.RunStart:plan.RunEnd], before)))
				pad = ""
			default:
				next := padNext(pad, len(pad)+plan.Sizes[n-1]-plan.Sizes[n-2])
				sizes.body("seed history-edit padding", at, historyEditBody([]any{padEditOp(pad, next)}))
				pad = next
			}
		}
	}

	cp := planComments(is)
	var steps []commentStep
	steps = append(steps, cp.Main...)
	for _, k := range slices.Sorted(maps.Keys(cp.ByAsk)) {
		steps = append(steps, cp.ByAsk[k]...)
	}
	for _, st := range steps {
		c := &st.C
		parent, ask := "", ""
		if c.ReplyTo != nil {
			parent = localPlaceholder
		}
		if c.AskID != nil && c.ReplyTo == nil {
			ask = localPlaceholder
		}
		_, body, err := commentBody(c, parent, ask)
		if err != nil {
			t.Errorf("%s: %v", p.Key, err)
			continue
		}
		where := p.Key + " comment " + c.ID[:8]
		if st.Kind != commentAnchored {
			sizes.body("seed "+string(st.Kind), where, body)
			continue
		}
		anchoredBodies(s, docs[c.Anchor.ArtifactID], "seed comment-anchored", where, c.ID, c.Anchor, body, sizes)
	}
	for _, c := range cp.Resolve {
		_, actor, err := actorOf(c.ResolvedBy)
		if err != nil {
			t.Errorf("%s: %v", p.Key, err)
			continue
		}
		if actor != nil {
			sizes.body("seed resolve", p.Key+" comment "+c.ID[:8], map[string]any{"actor": actor})
		}
	}
	for i := range is.Asks {
		a := &is.Asks[i]
		where := p.Key + " ask " + a.ID[:8]
		switch {
		case a.BlockID != nil:
		case a.Approval != nil:
			if _, body, err := approvalBody(a); err != nil {
				t.Errorf("%s: %v", where, err)
			} else {
				sizes.body("seed approval-request", where, body)
			}
		default:
			_, body, err := questionBody(a)
			if err != nil {
				t.Errorf("%s: %v", where, err)
				continue
			}
			if a.Anchor == nil {
				sizes.body("seed ask", where, body)
			} else {
				anchoredBodies(s, docs[a.Anchor.ArtifactID], "seed ask-anchored", where, a.ID, a.Anchor, body, sizes)
			}
		}
		if a.State == "answered" {
			askBodies(t, s, *a, docs, sizes, false)
		}
	}
	if patch := issuePatch(liveIssue{}, is); patch != nil {
		patch["actor"] = sessionActor(replaySeedActorID)
		sizes.body("seed patch", p.Key, patch)
	}
	if is.Claim != nil {
		_, actor, err := actorOf(&is.Claim.Actor)
		if err != nil {
			t.Errorf("%s: %v", p.Key, err)
		} else {
			sizes.body("seed claim", p.Key, withActor(map[string]any{}, actor))
		}
	}
}

// askBodies measures the answer (a review for an approval) or, with resolve, the resolve of ask a.
func askBodies(t *testing.T, s *replaySeeder, a replayAsk, docs map[string]*issueDoc, sizes *bodySizes, resolve bool) {
	where := a.ID[:8]
	switch {
	case resolve && a.Resolution != nil:
		if _, body, err := askResolveBody(&a); err != nil {
			t.Errorf("ask %s: %v", where, err)
		} else {
			sizes.body("seed ask-resolve", where, body)
		}
	case resolve || a.Answer == nil:
	case a.Approval != nil:
		if body, err := reviewFor(a.Answer); err != nil {
			t.Errorf("ask %s: %v", where, err)
		} else {
			sizes.body("seed review", where, body)
		}
	default:
		sizes.body("seed answer", where, answerBody(&a, json.RawMessage(`"2026-10-26T22:55:00.000000Z"`)))
	}
}

// anchoredBodies measures body anchored at every quote anchored may try for the logged anchor:
// the choice's candidates, and the choice's without the logged quote.
func anchoredBodies(s *replaySeeder, d *issueDoc, kind, where, logged string, a *replayAnchor, body map[string]any, sizes *bodySizes) {
	if d == nil {
		sizes.body(kind, where, body)
		return
	}
	for _, skip := range []bool{false, true} {
		choice := s.choose(d, logged, a, skip)
		for _, quote := range choice.Candidates {
			body["anchor"] = anchorField(localPlaceholder, quote, choice)
			sizes.body(kind, where, body)
		}
	}
	delete(body, "anchor")
}
