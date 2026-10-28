package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestVersionSizes(t *testing.T) {
	versions := []listedVersion{{Number: 1, Size: new(10)}, {Number: 2}, {Number: 4, Size: new(40)}}
	if got := versionSizes(versions, 3); !slices.Equal(got, []int{10, -1, -1}) {
		t.Fatalf("sizes %v", got)
	}
	if n := versionSizesMissing([]*replayArtifact{{VersionSizes: []int{10, -1, -1}}, {VersionSizes: []int{5}}}); n != 2 {
		t.Fatalf("missing %d", n)
	}
}

func TestSetAnswerVersions(t *testing.T) {
	at := tAt
	st := replayState{Issues: []replayIssue{{Key: "PRJ-1", Exists: true,
		Artifacts: []replayArtifact{{ID: "d1", Kind: "doc", Version: 4}},
		Asks: []replayAsk{
			{ID: "a1", State: "answered", BlockID: new("b1"), BlockArtifactID: new("d1"), Answer: &replayAskAnswer{At: at}},
			{ID: "a2", State: "answered", BlockID: new("b2"), BlockArtifactID: new("d1"), Answer: &replayAskAnswer{At: at.Add(time.Hour)}},
			{ID: "a3", State: "open", BlockID: new("b3"), BlockArtifactID: new("d1")},
		}}}}
	byID := map[string]listedArtifact{"d1": {ID: "d1", Versions: []listedVersion{
		{Number: 2, CreatedAt: at.Add(-time.Minute)}, {Number: 3, CreatedAt: at.Add(time.Second)},
		{Number: 4, CreatedAt: at.Add(time.Minute)}, {Number: 5, CreatedAt: at.Add(2 * time.Hour)},
	}}}
	setAnswerVersions(&st, byID)
	asks := st.Issues[0].Asks
	if asks[0].AnswerVersion != 3 || asks[1].AnswerVersion != 0 || asks[2].AnswerVersion != 0 {
		t.Fatalf("answer versions %d %d %d: the first version at or after the answer, none past Version", asks[0].AnswerVersion, asks[1].AnswerVersion, asks[2].AnswerVersion)
	}
}

func TestBuildBlockMap(t *testing.T) {
	md := "é intro paragraph\n\n| a | b |\n|---|---|\n| x | y |\n"
	tableFrom := strings.Index(md, "| a")
	served := []servedBlock{
		{ID: "p", Type: "paragraph", From: 0, To: len("é intro paragraph")},
		{ID: "t", Type: "table", From: tableFrom, To: len(md) - 1},
		{ID: "bad", Type: "paragraph", From: 5, To: len(md) + 10},
	}
	served[1].Table = &struct {
		Rows []struct {
			ID    string `json:"id"`
			Cells []struct {
				ID   string `json:"id"`
				Text string `json:"text"`
			} `json:"cells"`
		} `json:"rows"`
	}{}
	if err := json.Unmarshal([]byte(`{"rows":[{"id":"r1","cells":[{"id":"c1","text":"x"},{"id":"c2","text":"y"}]}]}`), served[1].Table); err != nil {
		t.Fatal(err)
	}
	bm, bad := buildBlockMap(7, md, served)
	if bad != 1 || bm.Version != 7 || len(bm.Blocks) != 2 {
		t.Fatalf("map %+v, bad %d", bm, bad)
	}
	if bm.Blocks[0].Text != "é intro paragraph" || !strings.HasPrefix(bm.Blocks[1].Text, "| a | b |") {
		t.Fatal("block texts must be the byte ranges of the markdown")
	}
	if i, text, ok := bm.find("c2"); !ok || i != 1 || text != "y" {
		t.Fatalf("cell lookup %d %q %v", i, text, ok)
	}
	if i, text, ok := bm.find("r1"); !ok || i != 1 || text != bm.Blocks[1].Text {
		t.Fatalf("row lookup %d %v", i, ok)
	}
}

func TestCountAnchorRules(t *testing.T) {
	text := "First unique paragraph sentence here.\n\nSecond paragraph with other words.\n"
	st := replayState{
		Issues: []replayIssue{{Key: "PRJ-1", Exists: true,
			Comments: []replayComment{
				{ID: "c1", Anchor: &replayAnchor{ArtifactID: "d1", Quote: "unique paragraph", BlockID: new("p1")}},
				{ID: "c2", Anchor: &replayAnchor{ArtifactID: "d1", Quote: "gone", BlockID: new("p2"), Orphaned: true}},
				{ID: "c3", Anchor: &replayAnchor{ArtifactID: "nodoc", Quote: "x"}},
				{ID: "c4"},
			}}},
		BlockMaps: map[string]*replayBlockMap{"d1": {Version: 2, Blocks: []replayBlock{
			{ID: "p1", Text: "First unique paragraph sentence here."},
			{ID: "p2", Text: "Second paragraph with other words."},
		}}},
	}
	window := []replayEvent{
		{Type: "comment.created", Payload: json.RawMessage(`{"id":"w1","anchor":{"artifact_id":"d1","quote":"other words","block_id":"p2"}}`)},
		{Type: "comment.created", Payload: json.RawMessage(`{"id":"w2","body":"plain"}`)},
	}
	got := countAnchorRules(st, window, map[string]string{"d1": text})
	want := map[string]int{ruleLoggedQuote: 2, ruleInBlock: 1, "no_text": 1}
	if len(got) != len(want) || got[ruleLoggedQuote] != 2 || got[ruleInBlock] != 1 || got["no_text"] != 1 {
		t.Fatalf("rules %v, want %v", got, want)
	}
	if ids := anchoredDocs(&st, window); !slices.Equal(ids, []string{"d1", "nodoc"}) {
		t.Fatalf("anchored docs %v", ids)
	}
}

func TestPlanHistoryTypedAskDocument(t *testing.T) {
	ask := ":::ask{#b1 urgency=\"high\" multiple=\"false\" state=\"answered\" answered_by=\"u\" answered_at=\"2026-10-26T22:00:00Z\" selected=\"[]\" answer=\"x\"}\nQuestion text\n:::\n"
	text := "Intro.\n\n" + ask + "\n## Later\n\n" + strings.Repeat("later section words. ", 30) + "\n"
	// Version 3 carries the answer (production's change there is the answer's), the rest pads.
	real := []int{400, 420, 520, 300, len(text)}
	p, err := planHistory(text, real, 5, []int{0}, []int{3})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.AnswerAt[3], []int{0}) || p.Restore != 5 || p.Flipped != 0 || p.Unreachable != 0 {
		t.Fatalf("plan %+v", p)
	}
	if strings.Contains(text[p.RunStart:p.RunEnd], ":::ask") {
		t.Fatal("a run never holds a typed ask block")
	}
	for n := 2; n <= 5; n++ {
		if p.Sizes[n-1]-p.Sizes[n-2] != real[n-1]-real[n-2] {
			t.Fatalf("version %d changes %d, production %d", n, p.Sizes[n-1]-p.Sizes[n-2], real[n-1]-real[n-2])
		}
	}
}

func TestPlanHistorySignFlip(t *testing.T) {
	// A typed ask block no run may remove keeps the base large; production's first version was
	// smaller than that, so the walk back flips signs and keeps every |change|.
	ask := ":::ask{#b1 state=\"open\"}\n" + strings.Repeat("Question words go on. ", 20) + "\n:::\n"
	text := ask + "\nTail paragraph.\n"
	real := []int{100, 200, len(text) + 100, len(text)}
	p, err := planHistory(text, real, 4, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Flipped == 0 || p.Unreachable != 0 {
		t.Fatalf("flipped %d unreachable %d", p.Flipped, p.Unreachable)
	}
	floor := len(baseText(text, p.RunStart, p.RunEnd)) + padOverhead(1) + padMin
	for n := 2; n <= 4; n++ {
		got, want := p.Sizes[n-1]-p.Sizes[n-2], real[n-1]-real[n-2]
		if got != want && got != -want {
			t.Fatalf("version %d changes %d, |production| %d", n, got, want)
		}
	}
	for n := 1; n < p.Restore; n++ {
		if p.Sizes[n-1] < floor {
			t.Fatalf("version %d planned %d under the floor %d", n, p.Sizes[n-1], floor)
		}
	}
}
