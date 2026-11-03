package main

import (
	"errors"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
)

func TestRouteMixRendersEachPlaceholder(t *testing.T) {
	mix, err := parseRouteMix("test", strings.Join([]string{
		"# comment",
		"",
		"5 browser person GET /api/v1/issues/{key}",
		"1 browser person GET /api/v1/issues?project={project}",
		"2 pod agent GET /api/v1/artifacts/{artifact_id}/text",
		"3 cli agent GET /api/v1/references?to={key_ref}",
	}, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	is := seedIssue{Key: "LGA-7", ArtifactID: "0b6f1c3e-1111-2222-3333-444455556666"}
	want := map[string]struct {
		path   string
		person bool
	}{
		"browser GET /api/v1/issues/{key}":             {"/api/v1/issues/LGA-7", true},
		"browser GET /api/v1/issues?project={project}": {"/api/v1/issues?project=LGB", true},
		"pod GET /api/v1/artifacts/{artifact_id}/text": {"/api/v1/artifacts/0b6f1c3e-1111-2222-3333-444455556666/text", false},
		"cli GET /api/v1/references?to={key_ref}":      {"/api/v1/references?to=dispatch%3A%2F%2FLGA-7", false},
	}
	if len(mix.Routes) != len(want) {
		t.Fatalf("parsed %d routes, want %d", len(mix.Routes), len(want))
	}
	for _, r := range mix.Routes {
		w, ok := want[r.Name]
		if !ok {
			t.Fatalf("unexpected route name %q", r.Name)
		}
		if got := r.render(is, "LGB"); got != w.path {
			t.Errorf("%s renders %q, want %q", r.Name, got, w.path)
		}
		if r.Person != w.person {
			t.Errorf("%s person = %t, want %t", r.Name, r.Person, w.person)
		}
	}
}

func TestRouteMixRefusalsNameTheirLine(t *testing.T) {
	cases := map[string]string{
		"method other than GET": "1 cli agent POST /api/v1/issues/{key}/comments",
		"unknown placeholder":   "1 cli agent GET /api/v1/issues/{issue}",
		"missing field":         "1 cli agent GET",
		"zero weight":           "0 cli agent GET /api/v1/projects",
		"negative weight":       "-2 cli agent GET /api/v1/projects",
		"unknown identity":      "1 cli robot GET /api/v1/projects",
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			text := "# header\n1 other agent GET /api/v1/projects\n" + line + "\n2 cli agent GET /api/v1/agents\n"
			_, err := parseRouteMix("test", text)
			var lineErr *routeMixError
			if !errors.As(err, &lineErr) {
				t.Fatalf("parse accepted %q or failed without a line: %v", line, err)
			}
			if lineErr.Line != 3 {
				t.Fatalf("refusal names line %d, want 3", lineErr.Line)
			}
		})
	}
}

func TestCommittedDefaultRouteMixParses(t *testing.T) {
	mix, err := loadRouteMix("")
	if err != nil {
		t.Fatal(err)
	}
	if len(mix.Routes) != 18 {
		t.Fatalf("default route mix has %d routes, want the archive's 18", len(mix.Routes))
	}
	groups := map[string]float64{}
	for _, r := range mix.Routes {
		groups[r.Caller] += r.Weight
		if path := r.render(seedIssue{Key: "LGA-1", ArtifactID: "id"}, "LGA"); strings.ContainsAny(path, "{}") {
			t.Errorf("%s leaves a placeholder in %q", r.Name, path)
		}
	}
	want := map[string]float64{"browser": 39, "cli": 34, "pod": 17, "other": 10}
	for caller, w := range want {
		if groups[caller] != w {
			t.Errorf("caller group %s weighs %g, want %g", caller, groups[caller], w)
		}
	}
}

// specLoopRouteMixName is the committed route mix of the Verified Linux spec loop, which a step
// runs with -route-mix; the step gives it by path, as run-step.sh gives any route mix of its own.
const specLoopRouteMixName = "routes-spec-loop-2026-11-03.txt"

// TestSpecLoopRouteMixParses: the committed loop mix is the seven reads of the issue's plan, at
// its weights, all of them the loop driver's.
func TestSpecLoopRouteMixParses(t *testing.T) {
	mix, err := loadRouteMix(specLoopRouteMixName)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"loop GET /api/v1/issues/{key}/comments":                             13.7,
		"loop GET /api/v1/search?q={search_term}&project={project}":          13.7,
		"loop GET /api/v1/search?q={status_term}&project={project}":          13.6,
		"loop GET /api/v1/artifacts/{review_page_artifact_id}/text":          24,
		"loop GET /api/v1/issues/{key}/comments?artifact={part_artifact_id}": 21,
		"loop GET /api/v1/artifacts/{review_index_artifact_id}/text":         10,
		"loop GET /api/v1/issues/{key}/artifacts/{part_slug}/text":           3,
	}
	if len(mix.Routes) != len(want) {
		t.Fatalf("the loop route mix has %d routes, want the plan's %d", len(mix.Routes), len(want))
	}
	total := 0.0
	for _, r := range mix.Routes {
		w, ok := want[r.Name]
		if !ok {
			t.Fatalf("unexpected route %q", r.Name)
		}
		if r.Weight != w {
			t.Errorf("%s weighs %g, want %g", r.Name, r.Weight, w)
		}
		if r.Caller != "loop" || r.Person {
			t.Errorf("%s is caller %q person %t, want the loop driver's agent", r.Name, r.Caller, r.Person)
		}
		total += r.Weight
	}
	if total != 99 {
		t.Errorf("the loop route mix weighs %g, want the plan's 99", total)
	}
	if b, err := os.ReadFile(specLoopRouteMixName); err != nil {
		t.Fatal(err)
	} else if head := string(b); !strings.Contains(head, "INFRA-988") || !strings.Contains(head, "share of the loop's reads") {
		t.Error("the committed file's header names neither the issue nor what its weights are")
	}
}

// TestLoopRouteMixRendersEachPlaceholder: every placeholder of the loop's mix resolves against the
// proposal the request landed on, and none is left in the path.
func TestLoopRouteMixRendersEachPlaceholder(t *testing.T) {
	ls := loopTestSeed(t)
	mix, err := loadRouteMix(specLoopRouteMixName)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(3, 2))
	pr := &ls.Proposals[0]
	seen := map[string]bool{}
	for _, rt := range mix.Routes {
		for range 50 {
			path := rt.renderLoop(ls, 0, rng)
			if strings.ContainsAny(path, "{}") {
				t.Fatalf("%s leaves a placeholder in %q", rt.Name, path)
			}
			if !strings.Contains(path, pr.Key) && !strings.Contains(path, "/artifacts/") && !strings.Contains(path, "/search?") {
				t.Fatalf("%s rendered %q, which names neither the proposal nor one of its records", rt.Name, path)
			}
			seen[path] = true
		}
	}
	// Each record of the proposal the mix reads is reached, and nothing else is.
	want := map[string]bool{
		"/api/v1/issues/LOOP-1/comments":                           true,
		"/api/v1/artifacts/page-a0/text":                           true,
		"/api/v1/artifacts/page-a1/text":                           true,
		"/api/v1/artifacts/index-a/text":                           true,
		"/api/v1/issues/LOOP-1/comments?artifact=part-a0":          true,
		"/api/v1/issues/LOOP-1/comments?artifact=part-a1":          true,
		"/api/v1/issues/LOOP-1/artifacts/part-001.md/text":         true,
		"/api/v1/issues/LOOP-1/artifacts/part-002.md/text":         true,
		"/api/v1/search?q=alpha&project=LOOP":                      true,
		"/api/v1/search?q=beta&project=LOOP":                       true,
		"/api/v1/search?q=gamma&project=LOOP":                      true,
		"/api/v1/search?q=" + loopStatusNames[0] + "&project=LOOP": true,
	}
	for path := range want {
		if !seen[path] {
			t.Errorf("no draw reached %q", path)
		}
	}
	for path := range seen {
		if !want[path] && !strings.HasPrefix(path, "/api/v1/search?q=") {
			t.Errorf("a draw reached %q, which is no record of proposal alpha", path)
		}
	}
}

// TestLoopRouteMixIsRefusedWithoutALoopSeed: a loop placeholder has nothing to resolve against in
// a seed.json run, and a record no proposal holds has nothing either; each refusal names the
// placeholder, so neither is rendered as an empty string.
func TestLoopRouteMixIsRefusedWithoutALoopSeed(t *testing.T) {
	mix, err := loadRouteMix(specLoopRouteMixName)
	if err != nil {
		t.Fatal(err)
	}
	err = checkRouteMixSeed(mix, nil)
	if err == nil || !strings.Contains(err.Error(), "-loop-seed") {
		t.Fatalf("the loop route mix without a loop seed is refused with %v, want a refusal naming the flag", err)
	}
	if !strings.Contains(err.Error(), "{search_term}") && !strings.Contains(err.Error(), "{review_page_artifact_id}") &&
		!strings.Contains(err.Error(), "{part_artifact_id}") && !strings.Contains(err.Error(), "{part_slug}") &&
		!strings.Contains(err.Error(), "{review_index_artifact_id}") && !strings.Contains(err.Error(), "{status_term}") {
		t.Fatalf("the refusal %q names no placeholder", err)
	}
	for name, line := range map[string]string{
		"{part_artifact_id}":         "1 loop agent GET /api/v1/issues/{key}/comments?artifact={part_artifact_id}",
		"{part_slug}":                "1 loop agent GET /api/v1/issues/{key}/artifacts/{part_slug}/text",
		"{review_page_artifact_id}":  "1 loop agent GET /api/v1/artifacts/{review_page_artifact_id}/text",
		"{review_index_artifact_id}": "1 loop agent GET /api/v1/artifacts/{review_index_artifact_id}/text",
		"{search_term}":              "1 loop agent GET /api/v1/search?q={search_term}",
		"{status_term}":              "1 loop agent GET /api/v1/search?q={status_term}",
	} {
		one, err := parseRouteMix("test", line)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		err = checkRouteMixSeed(one, nil)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("a mix of %s alone is refused with %v, want a refusal naming it", name, err)
		}
		if err := checkRouteMixSeed(one, loopTestSeed(t)); err != nil {
			t.Errorf("%s against the loop seed: %v", name, err)
		}
	}
	// A loop seed whose proposals hold no review page cannot serve the review reads either.
	ls := &loopSeed{Project: "LOOP", SearchTerms: []string{"alpha"}, StatusTerms: loopStatusNames[:],
		Proposals: []loopSeedProposal{{Index: 0, Name: "alpha", Key: "LOOP-1", IDs: 3, LastPart: -1}}}
	ls.prepare()
	err = checkRouteMixSeed(mix, ls)
	if err == nil || !strings.Contains(err.Error(), "{review_page_artifact_id}") || !strings.Contains(err.Error(), "a review page") {
		t.Fatalf("a loop seed with no review page is refused with %v, want a refusal naming the placeholder and the record", err)
	}
	// The seed.json mix needs no loop seed and is accepted without one, as it always has been.
	plain, err := loadRouteMix("")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkRouteMixSeed(plain, nil); err != nil {
		t.Fatalf("the committed default route mix is refused without a loop seed: %v", err)
	}
}

// TestRouteNeedsOneRecordOfItsProposal: a route that would read two records of one proposal is
// refused at its line, since the proposal it lands on is drawn for one record.
func TestRouteNeedsOneRecordOfItsProposal(t *testing.T) {
	_, err := parseRouteMix("test", "1 loop agent GET /api/v1/artifacts/{review_page_artifact_id}/text?part={part_artifact_id}")
	var lineErr *routeMixError
	if !errors.As(err, &lineErr) || !strings.Contains(lineErr.Reason, "different records") {
		t.Fatalf("a route of two records is accepted or refused otherwise: %v", err)
	}
}
