package main

import (
	_ "embed"
	"fmt"
	"math/rand/v2"
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
	// holes are the template's placeholders, in the order it uses them, and need what the route
	// needs of the proposal it lands on under a loop seed (loopPlaceholders). A route that uses
	// no loop placeholder needs loopNeedAny, which every proposal serves.
	holes []string
	// lits are the template's literal pieces around those holes: lits[i] precedes holes[i] and
	// the last piece follows the last hole, so a render is one pass with no replacer to build.
	lits []string
	need loopNeed
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
var routePlaceholders = []string{"key", "project", "artifact_id", "key_ref",
	"part_artifact_id", "part_slug", "review_page_artifact_id", "review_index_artifact_id",
	"search_term", "status_term"}

// loopPlaceholders are the placeholders the loop's own seed fills from the proposal a request
// lands on, each with what it needs of that proposal: a route that uses one is drawn onto a
// proposal that holds the record (checkRouteMixSeed, loopSeed.pick). A plain seed.json run has no
// proposal to resolve them against and is refused rather than sending a URL with an empty id in
// it, unless render fills the placeholder from the issue itself (seedPlaceholders).
var loopPlaceholders = map[string]loopNeed{
	"artifact_id":              loopNeedPart,
	"part_artifact_id":         loopNeedPart,
	"part_slug":                loopNeedPart,
	"review_page_artifact_id":  loopNeedReviewPage,
	"review_index_artifact_id": loopNeedReviewIndex,
	"search_term":              loopNeedAny,
	"status_term":              loopNeedAny,
}

// seedPlaceholders are the placeholders render fills from one issue of a plain seed.json, so a
// route that uses these alone needs no loop seed.
var seedPlaceholders = []string{"key", "project", "artifact_id", "key_ref"}

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
		lits, holes, err := checkTemplate(f[4])
		if err != nil {
			return fail("%v", err)
		}
		need, err := templateNeed(holes)
		if err != nil {
			return fail("%v", err)
		}
		r := route{Name: f[1] + " GET " + f[4], Weight: w, Caller: f[1], Person: person,
			Template: f[4], holes: holes, lits: lits, need: need}
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

// checkTemplate answers the template's literal pieces and its placeholders, in the order it uses
// them, and refuses a template that is not a path or names a placeholder this package does not
// fill. lits[i] is the text before holes[i] and the last piece is the text after the last hole,
// so the two together spell the template.
func checkTemplate(t string) (lits, holes []string, err error) {
	if !strings.HasPrefix(t, "/") {
		return nil, nil, fmt.Errorf("path template %q does not start with /", t)
	}
	rest := t
	for {
		open := strings.IndexAny(rest, "{}")
		if open < 0 {
			return append(lits, rest), holes, nil
		}
		if rest[open] == '}' {
			return nil, nil, fmt.Errorf("path template %q has a } with no {", t)
		}
		end := strings.IndexByte(rest[open:], '}')
		if end < 0 {
			return nil, nil, fmt.Errorf("path template %q has an unclosed {", t)
		}
		name := rest[open+1 : open+end]
		if !slices.Contains(routePlaceholders, name) {
			return nil, nil, fmt.Errorf("unknown placeholder {%s}; placeholders are {%s}", name, strings.Join(routePlaceholders, "}, {"))
		}
		lits = append(lits, rest[:open])
		holes = append(holes, name)
		rest = rest[open+end+1:]
	}
}

// templateNeed is what a route needs of the proposal it lands on: the one record its loop
// placeholders read. A route that would need two - a review page and a review index, say - is
// refused, since one request reads one record and the proposal it lands on is drawn for that
// record alone.
func templateNeed(holes []string) (loopNeed, error) {
	need, from := loopNeedAny, ""
	for _, name := range holes {
		h, ok := loopPlaceholders[name]
		if !ok || h == loopNeedAny {
			continue
		}
		if need != loopNeedAny && h != need {
			return loopNeedAny, fmt.Errorf("uses {%s} and {%s}, which need different records of one proposal; a route reads one", from, name)
		}
		need, from = h, name
	}
	return need, nil
}

// checkRouteMixSeed refuses a route mix the step's seed cannot fill. Without a loop seed the
// loop's placeholders have no proposal to resolve against - bar the ones render fills from an
// issue of seed.json - and with one a placeholder no proposal holds the record of would send the
// read nowhere: each refusal names the placeholder and its route, rather than leaving a step to
// read an empty id.
func checkRouteMixSeed(m routeMix, ls *loopSeed) error {
	for _, rt := range m.Routes {
		for _, name := range rt.holes {
			need, drawn := loopPlaceholders[name]
			if !drawn {
				continue
			}
			if ls == nil {
				if slices.Contains(seedPlaceholders, name) {
					continue
				}
				return fmt.Errorf("route mix %s: %s uses {%s}, which only the loop's own seed fills: give -loop-seed", m.Source, rt.Name, name)
			}
			if ls.servingProposals(need) == 0 {
				return fmt.Errorf("route mix %s: %s uses {%s}, and no proposal of the loop seed holds %s", m.Source, rt.Name, name, loopNeedNames[need])
			}
		}
	}
	return nil
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

// renderLoop fills the template on proposal at of a loop seed: its issue key, the seed's project,
// and the part document, review page, review index or search term the request reads, each drawn
// from that proposal's own records with rng. The proposal was drawn for this route's need, so
// every placeholder the loop fills resolves: {artifact_id} names a part of the proposal exactly
// as {part_artifact_id} does, and a record the proposal holds none of belongs to no placeholder
// of this template. The four draws are made in this order whether or not the template uses them,
// so one -rng-seed gives one schedule.
func (r route) renderLoop(ls *loopSeed, at int, rng *rand.Rand) string {
	pr := &ls.Proposals[at]
	var part loopSeedPart
	if n := len(pr.Parts); n > 0 {
		part = pr.Parts[rng.IntN(n)]
	}
	page := ""
	if n := len(pr.ReviewPages); n > 0 {
		page = pr.ReviewPages[rng.IntN(n)].ArtifactID
	}
	search, status := ls.searchTerm(rng), ls.statusTerm(rng)
	var b strings.Builder
	b.Grow(len(r.Template) + 64)
	for i, lit := range r.lits {
		b.WriteString(lit)
		if i >= len(r.holes) {
			break
		}
		switch r.holes[i] {
		case "key":
			b.WriteString(pr.Key)
		case "project":
			b.WriteString(ls.Project)
		case "artifact_id", "part_artifact_id":
			b.WriteString(part.ArtifactID)
		case "key_ref":
			b.WriteString(url.QueryEscape("dispatch://" + pr.Key))
		case "part_slug":
			b.WriteString(part.Slug)
		case "review_page_artifact_id":
			b.WriteString(page)
		case "review_index_artifact_id":
			b.WriteString(pr.ReviewIndex)
		case "search_term":
			b.WriteString(search)
		case "status_term":
			b.WriteString(status)
		}
	}
	return b.String()
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
