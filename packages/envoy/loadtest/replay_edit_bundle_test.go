package main

import (
	"flag"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestReplayEditsBundle plans the bundle LOADTEST_REPLAY_BUNDLE names (skipped without it) at rate
// 1 over its whole window (loadgen replay-plan -replay-rate 1, its other flags at their defaults)
// and replays every planned edit, document by document in plan order, against applyAsDispatch from
// the text the client holds: the document's text at the bundle's instant (what the seed leaves
// Dispatch holding), a created document's or spec's text, an upload's content, or the text the
// edit before it left. It fails on any edit Dispatch's rules refuse, on any whose result is not the
// text the plan then holds, and on an anchored root's quote the rendered text holds more than once.
// It logs counts only, never a text.
func TestReplayEditsBundle(t *testing.T) {
	dir := os.Getenv("LOADTEST_REPLAY_BUNDLE")
	if dir == "" {
		t.Skip("LOADTEST_REPLAY_BUNDLE is not set")
	}
	b, err := readBundle(dir)
	if err != nil {
		t.Fatal(err)
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
	texts := map[string]string{}
	for i := range b.State.Issues {
		for _, a := range b.State.Issues[i].Artifacts {
			if a.Kind == "doc" && a.TextPath != "" {
				raw, err := b.readFile(a.TextPath)
				if err != nil {
					t.Fatal(err)
				}
				texts[a.ID] = string(raw)
			}
		}
	}
	outcomes, ops := map[string]int{}, map[string]int{}
	edits := 0
	for _, act := range plan.Actions {
		if !act.Write || act.Doc == "" {
			continue
		}
		var body map[string]any
		for _, stage := range act.Stages {
			for _, r := range stage {
				if r.Method != "GET" {
					body = r.Body
				}
			}
		}
		switch act.Kind {
		case rkIssueCreate:
			texts[act.Doc] = body["spec"].(string)
		case rkDocCreate, rkUpload:
			texts[act.Doc] = body["content"].(string)
		case rkEdit, rkEditUnnamed:
			edits++
			batch := body["ops"].([]replayEditOp)
			for _, op := range batch {
				ops[op.Op]++
			}
			text, known := texts[act.Doc]
			if !known {
				t.Errorf("%s event %d: an edit of document %s the client never held", act.Issue, act.EventID, act.Doc)
				continue
			}
			got, code := applyAsDispatch(text, batch)
			switch {
			case code != "":
				outcomes[code]++
				t.Errorf("%s event %d: %d ops on document %s: Dispatch would answer %s", act.Issue, act.EventID, len(batch), act.Doc, code)
			case got != act.text:
				outcomes["applied, text differs"]++
				t.Errorf("%s event %d: document %s holds %d bytes after the edit, the plan %d", act.Issue, act.EventID, act.Doc, len(got), len(act.text))
			default:
				outcomes["applied"]++
			}
			texts[act.Doc] = act.text
		case rkAnchored:
			// An anchored root's quote must match once in the rendered text it lands in: Dispatch
			// answers a quote it matches twice with 409 TARGET_AMBIGUOUS.
			anchor, ok := body["anchor"].(map[string]any)
			if !ok {
				continue
			}
			quote := anchor["quote"].(string)
			if n := strings.Count(scanMarkdown(texts[act.Doc]).rendered, renderedLine(quote)); n > 1 {
				outcomes["anchor 409 TARGET_AMBIGUOUS"]++
				t.Errorf("%s event %d: the anchor quote on document %s matches %d places", act.Issue, act.EventID, act.Doc, n)
			} else {
				outcomes["anchor matches once"]++
			}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(outcomes)) {
		t.Logf("%-24s %5d", k, outcomes[k])
	}
	t.Logf("%d edits (%d from the diff, %d substitutes); ops %v; %d anchored roots unanchorable", edits, plan.Counts.Edits.Diff, plan.Counts.Edits.Substitute, ops, plan.Counts.Unanchorable)
	if edits == 0 {
		t.Fatal("the plan holds no edit")
	}
}
