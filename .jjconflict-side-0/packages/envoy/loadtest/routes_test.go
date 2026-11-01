package main

import (
	"errors"
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
