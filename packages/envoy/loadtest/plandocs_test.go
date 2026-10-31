package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// planFixtureSeed is a small seed: n issues in key order, each with the quotes and the root
// comment ids every event kind needs, and its own spec document.
func planFixtureSeed(n int) seedFile {
	sf := seedFile{Projects: []string{"LGA"}}
	for i := range n {
		sf.Issues = append(sf.Issues, seedIssue{
			Key:                fmt.Sprintf("LGA-%03d", i+1),
			Project:            "LGA",
			ArtifactID:         fmt.Sprintf("20000000-0000-4000-8000-%012d", i+1),
			SpecName:           specName,
			SpecBytes:          4000,
			Quotes:             []string{fmt.Sprintf("Requirement LGA-%03d-R1 holds", i+1)},
			AnchoredCommentIDs: []string{fmt.Sprintf("30000000-0000-4000-8000-%012d", i+1)},
			PlainCommentIDs:    []string{fmt.Sprintf("40000000-0000-4000-8000-%012d", i+1)},
			Route:              "session:" + sessionID(i),
		})
	}
	return sf
}

func planFixtureConfig() runConfig {
	mix, err := parseEventMix(defaultEventMix)
	if err != nil {
		panic(err)
	}
	return runConfig{sessions: 12, events: 40, warmup: 2 * time.Second, duration: 8 * time.Second,
		human: 0.1, mix: mix, rngSeed: 1}
}

// runPlannerCounts is what the run's own planner yields: openLoop, which runEvents drives, over a
// window in the past, so it never sleeps; each in-window edit and upload counted on the document
// of the issue its plan names.
func runPlannerCounts(cfg runConfig, sf seedFile) map[string]docVersions {
	st := newStep(cfg, sf, log.New(io.Discard, "", 0), 0)
	st.openWindow(time.Now().Add(-cfg.warmup - cfg.duration - time.Second))
	counts := map[string]docVersions{}
	var marker int64
	openLoop(context.Background(), cfg.rngSeed, 1, cfg.events, st.start, st.winStart, st.winEnd,
		func(r *rand.Rand, in bool) {
			marker++
			p := planEvent(r, cfg, st.sessions, sf, marker)
			if !in || (p.kind != kindEdit && p.kind != kindUpload) {
				return
			}
			id := sf.Issues[p.issue].ArtifactID
			d := counts[id]
			d.Artifact = id
			if p.kind == kindEdit {
				d.Edits++
			} else {
				d.Uploads++
			}
			d.Versions++
			counts[id] = d
		})
	return counts
}

// TestPlanDocsGivesTheRunsOwnCounts: plan-docs' edit and upload counts are the ones the run's
// event generator plans for the same inputs, and it prints them most versions first, ties by
// artifact id.
func TestPlanDocsGivesTheRunsOwnCounts(t *testing.T) {
	sf := planFixtureSeed(20)
	cfg := planFixtureConfig()
	want := runPlannerCounts(cfg, sf)
	if len(want) < 2 {
		t.Fatalf("the fixture step plans versions for %d documents; it must write more than one", len(want))
	}
	got := planVersionDocs(cfg, sf)
	if len(got) != len(want) {
		t.Fatalf("plan-docs names %d documents, the run plans versions for %d", len(got), len(want))
	}
	for _, d := range got {
		if w, ok := want[d.Artifact]; !ok || d != w {
			t.Errorf("plan-docs gives %s %+v, the run's planner %+v (found %t)", d.Artifact, d, w, ok)
		}
		if d.Versions != d.Edits+d.Uploads || d.Versions == 0 {
			t.Errorf("%s: versions %d is not its %d edits and %d uploads", d.Artifact, d.Versions, d.Edits, d.Uploads)
		}
	}
	if !slices.IsSortedFunc(got, func(a, b docVersions) int {
		if a.Versions != b.Versions {
			return b.Versions - a.Versions
		}
		return strings.Compare(a.Artifact, b.Artifact)
	}) {
		t.Errorf("plan-docs' documents are not most versions first, ties by id: %+v", got)
	}

	// The plan is a pure function of the inputs: the same flags give the same plan, another seed
	// another one, and a step with no events plans nothing.
	if again := planVersionDocs(cfg, sf); !slices.Equal(again, got) {
		t.Error("the same flags gave two plans")
	}
	other := cfg
	other.rngSeed = 2
	if slices.Equal(planVersionDocs(other, sf), got) {
		t.Error("another -rng-seed gave the same plan")
	}
	noEvents := cfg
	noEvents.events = 0
	if docs := planVersionDocs(noEvents, sf); len(docs) != 0 {
		t.Errorf("a step at 0 events/s plans %d documents", len(docs))
	}
}

// TestPlanDocsCommandPrintsTheTopDocuments: the command reads the seed file, cuts the plan to
// -top, prints one artifact id per line and, with -json, each document's counts.
func TestPlanDocsCommandPrintsTheTopDocuments(t *testing.T) {
	sf := planFixtureSeed(20)
	cfg := planFixtureConfig()
	want := planVersionDocs(cfg, sf)
	if len(want) < 4 {
		t.Fatalf("the fixture step plans versions for %d documents; it must write more than three", len(want))
	}
	want = want[:3]

	dir := t.TempDir()
	seedPath := filepath.Join(dir, "seed.json")
	b, err := json.Marshal(sf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seedPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"-seed", seedPath, "-events", "40", "-sessions", "12", "-warmup", "2s",
		"-duration", "8s", "-rng-seed", "1", "-top", "3"}

	lines := strings.Split(strings.TrimSuffix(planDocsOutput(t, args), "\n"), "\n")
	ids := make([]string, 0, len(want))
	for _, d := range want {
		ids = append(ids, d.Artifact)
	}
	if !slices.Equal(lines, ids) {
		t.Errorf("plan-docs printed %v, want the top 3 %v", lines, ids)
	}

	var asJSON []docVersions
	if err := json.Unmarshal([]byte(planDocsOutput(t, append(args, "-json"))), &asJSON); err != nil {
		t.Fatalf("plan-docs -json: %v", err)
	}
	if !slices.Equal(asJSON, want) {
		t.Errorf("plan-docs -json printed %+v, want %+v", asJSON, want)
	}
}

// planDocsOutput is what runPlanDocs prints for args.
func planDocsOutput(t *testing.T, args []string) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	runErr := runPlanDocs(args)
	os.Stdout = stdout
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Fatalf("plan-docs %v: %v", args, runErr)
	}
	return string(out)
}

// TestPlanDocsRefusals: the flags a step's plan needs, each refusal naming its input.
func TestPlanDocsRefusals(t *testing.T) {
	dir := t.TempDir()
	seedPath := filepath.Join(dir, "seed.json")
	b, err := json.Marshal(planFixtureSeed(3))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seedPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	ok := []string{"-seed", seedPath, "-events", "4", "-sessions", "3"}
	cases := map[string][]string{
		"-events":             {"-seed", seedPath, "-sessions", "3"},
		"-sessions":           {"-seed", seedPath, "-events", "4"},
		"-duration":           append(slices.Clone(ok), "-duration", "0s"),
		"-top":                append(slices.Clone(ok), "-top", "0"),
		"event mix":           append(slices.Clone(ok), "-event-mix", "bogus=1"),
		"read seed":           {"-seed", filepath.Join(dir, "none.json"), "-events", "4", "-sessions", "3"},
		"unexpected argument": append(slices.Clone(ok), "extra"),
	}
	for want, args := range cases {
		err := runPlanDocs(args)
		if err == nil {
			t.Errorf("plan-docs %v was accepted", args)
		} else if !strings.Contains(err.Error(), want) {
			t.Errorf("plan-docs %v is refused with %q, which does not name %q", args, err, want)
		}
	}
}
