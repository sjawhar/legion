package main

import (
	_ "embed"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
)

// defaultRouteMixName is the committed route mix a step uses when -route-mix is empty.
const defaultRouteMixName = "routes-2026-10-26-1719Z.txt"

//go:embed routes-2026-10-26-1719Z.txt
var defaultRouteMixText string

// route is one GET of a route-mix file:
//
//	<weight> <caller> <identity: person|agent> GET <path template>
//
// Its name in summaries and tables is "<caller> GET <template>".
type route struct {
	Name     string
	Weight   float64
	Caller   string
	Person   bool
	Template string
}

type routeMix struct {
	Source string
	Routes []route
	total  float64
}

// routeMixError refuses one line of a route-mix file.
type routeMixError struct {
	Source string
	Line   int
	Reason string
}

func (e *routeMixError) Error() string {
	return fmt.Sprintf("route mix %s line %d: %s", e.Source, e.Line, e.Reason)
}

// routePlaceholders are the template placeholders a route may use, in the order the refusal
// lists them.
var routePlaceholders = []string{"key", "project", "artifact_id", "key_ref"}

// loadRouteMix reads path, or the committed default when path is empty.
func loadRouteMix(path string) (routeMix, error) {
	if path == "" {
		return parseRouteMix(defaultRouteMixName+" (embedded)", defaultRouteMixText)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return routeMix{}, fmt.Errorf("read route mix: %w", err)
	}
	return parseRouteMix(path, string(b))
}

// parseRouteMix reads a route-mix file. Blank lines and lines starting with # are skipped. A
// method other than GET (writes are the event generator's), a missing or extra field, a weight
// that is not a positive number, an identity other than person or agent, a template that is not
// a path, an unknown placeholder and a repeated route are refused with their line number.
func parseRouteMix(source, text string) (routeMix, error) {
	m := routeMix{Source: source}
	seen := map[string]int{}
	for i, line := range strings.Split(text, "\n") {
		n := i + 1
		fail := func(format string, args ...any) (routeMix, error) {
			return routeMix{}, &routeMixError{Source: source, Line: n, Reason: fmt.Sprintf(format, args...)}
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 5 {
			return fail("has %d fields, want 5: <weight> <caller> <person|agent> GET <path template>", len(f))
		}
		w, err := strconv.ParseFloat(f[0], 64)
		if err != nil || !finite(w) || w <= 0 {
			return fail("weight %q is not a positive number", f[0])
		}
		var person bool
		switch f[2] {
		case "person":
			person = true
		case "agent":
		default:
			return fail("identity %q is neither person nor agent", f[2])
		}
		if f[3] != "GET" {
			return fail("method %q is refused: only GET routes are requests (writes are the event generator's)", f[3])
		}
		if err := checkTemplate(f[4]); err != nil {
			return fail("%v", err)
		}
		r := route{Name: f[1] + " GET " + f[4], Weight: w, Caller: f[1], Person: person, Template: f[4]}
		if first, dup := seen[r.Name]; dup {
			return fail("repeats route %q of line %d", r.Name, first)
		}
		seen[r.Name] = n
		m.Routes = append(m.Routes, r)
		m.total += w
	}
	if len(m.Routes) == 0 {
		return routeMix{}, fmt.Errorf("route mix %s has no routes", source)
	}
	return m, nil
}

func checkTemplate(t string) error {
	if !strings.HasPrefix(t, "/") {
		return fmt.Errorf("path template %q does not start with /", t)
	}
	rest := t
	for {
		open := strings.IndexAny(rest, "{}")
		if open < 0 {
			return nil
		}
		if rest[open] == '}' {
			return fmt.Errorf("path template %q has a } with no {", t)
		}
		end := strings.IndexByte(rest[open:], '}')
		if end < 0 {
			return fmt.Errorf("path template %q has an unclosed {", t)
		}
		name := rest[open+1 : open+end]
		if !slices.Contains(routePlaceholders, name) {
			return fmt.Errorf("unknown placeholder {%s}; placeholders are {%s}", name, strings.Join(routePlaceholders, "}, {"))
		}
		rest = rest[open+end+1:]
	}
}

// render fills the template for one issue and one project.
func (r route) render(is seedIssue, project string) string {
	return strings.NewReplacer(
		"{key}", is.Key,
		"{project}", project,
		"{artifact_id}", is.ArtifactID,
		"{key_ref}", url.QueryEscape("dispatch://"+is.Key),
	).Replace(r.Template)
}

// pick maps x in [0, 1) to a route in proportion to the weights.
func (m routeMix) pick(x float64) *route {
	x *= m.total
	for i := range m.Routes {
		if x < m.Routes[i].Weight {
			return &m.Routes[i]
		}
		x -= m.Routes[i].Weight
	}
	return &m.Routes[len(m.Routes)-1]
}
