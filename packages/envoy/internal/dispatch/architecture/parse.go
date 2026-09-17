// Package architecture parses, validates, and projects a project's
// architecture model: every *.md file in .dispatch/architecture/ at one commit
// of the configured source repository. Parse is pure — no database, no
// network; the importer owns fetching and projection.
package architecture

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Component is one architecture component: a single markdown file whose name
// is the component id, whose front matter carries the structured fields, and
// whose body is the component's prose (stored verbatim).
type Component struct {
	ID        string
	Title     string
	Prose     string
	Parent    string
	External  bool
	DependsOn []string
	Paths     []string
}

// Model is a validated component set. Components are sorted by id so
// projection order is deterministic.
type Model struct {
	Components []Component
}

// frontMatter is the YAML block a component file may open with.
type frontMatter struct {
	Title     string   `yaml:"title"`
	Parent    string   `yaml:"parent"`
	DependsOn []string `yaml:"depends_on"`
	Paths     []string `yaml:"paths"`
	External  bool     `yaml:"external"`
}

// Parse validates the file set whole and returns the model, or ONE error
// naming every problem (errors.Join). files is keyed by file name (the
// basename inside .dispatch/architecture/). An empty set is a valid, empty
// model.
func Parse(files map[string][]byte) (Model, error) {
	var problems []error
	components := map[string]Component{}
	firstFile := map[string]string{}

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		id := strings.ToLower(strings.TrimSuffix(name, ".md"))
		if !strings.HasSuffix(name, ".md") || !text.IsComponentID(id) {
			problems = append(problems, fmt.Errorf("%s: file name must be <slug>.md (slug: %s)", name, text.ComponentIDPattern))
			continue
		}
		content := files[name]
		// Text only: the prose lands in a text column and the snapshot in
		// jsonb, neither of which takes invalid UTF-8 or NUL — reject here so
		// the failure is a named model problem, not a projection error.
		if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
			problems = append(problems, fmt.Errorf("%s: file must be valid UTF-8 text without NUL bytes", name))
			continue
		}
		matter, prose, err := splitFrontMatter(content)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if existing, dup := firstFile[id]; dup {
			problems = append(problems, fmt.Errorf("%s: duplicate component id %q (already defined by %s)", name, id, existing))
			continue
		}
		firstFile[id] = name
		component := Component{
			ID:        id,
			Title:     matter.Title,
			Prose:     prose,
			Parent:    matter.Parent,
			External:  matter.External,
			DependsOn: matter.DependsOn,
			Paths:     matter.Paths,
		}
		if component.Title == "" {
			component.Title = id
		}
		for _, path := range component.Paths {
			if !validRepoPath(path) {
				problems = append(problems, fmt.Errorf("%s: path %q must be repo-relative (no leading /, no .. segment)", name, path))
			}
		}
		components[id] = component
	}

	// Set validation: references and containment run over the whole set, so a
	// file rejected above simply cannot be referenced.
	ids := make([]string, 0, len(components))
	for id := range components {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		component := components[id]
		name := firstFile[id]
		if component.Parent != "" {
			if _, ok := components[component.Parent]; !ok {
				problems = append(problems, fmt.Errorf("%s: unknown parent %q", name, component.Parent))
			}
		}
		for _, dependency := range component.DependsOn {
			if dependency == id {
				problems = append(problems, fmt.Errorf("%s: component depends on itself", name))
				continue
			}
			if _, ok := components[dependency]; !ok {
				problems = append(problems, fmt.Errorf("%s: unknown depends_on %q", name, dependency))
			}
		}
	}
	problems = append(problems, containmentCycles(ids, components, firstFile)...)

	if len(problems) > 0 {
		return Model{}, errors.Join(problems...)
	}
	model := Model{Components: make([]Component, 0, len(ids))}
	for _, id := range ids {
		model.Components = append(model.Components, components[id])
	}
	return model, nil
}

// containmentCycles walks each component's parent chain in ids order and
// reports every cycle once, by its lexically smallest member.
func containmentCycles(ids []string, components map[string]Component, firstFile map[string]string) []error {
	reported := map[string]bool{}
	var problems []error
	for _, start := range ids {
		seen := map[string]int{}
		var chain []string
		current := start
		for current != "" && !reported[current] {
			if at, ok := seen[current]; ok {
				cycle := chain[at:]
				for _, member := range cycle {
					reported[member] = true
				}
				problems = append(problems, fmt.Errorf("%s: containment cycle %s", firstFile[cycle[0]], strings.Join(append(cycle, cycle[0]), " -> ")))
				break
			}
			seen[current] = len(chain)
			chain = append(chain, current)
			next, ok := components[current]
			if !ok {
				break
			}
			current = next.Parent
		}
	}
	return problems
}

// splitFrontMatter separates the optional leading `---` YAML block from the
// markdown body. A file without front matter is all body with defaults; an
// empty block (`---` immediately closed by `---`) is the same. The closer is
// the first line after the opener that is exactly `---` (CR-trimmed) — a
// `---foo` line or a horizontal rule inside the body is not one. Front-matter
// keys are checked against the known set, so a typo (`depends-on`,
// `parents`) is a named problem rather than a silently dropped edge.
func splitFrontMatter(content []byte) (frontMatter, string, error) {
	text := string(content)
	var matter frontMatter
	opener, rest, found := strings.Cut(text, "\n")
	if strings.TrimSuffix(opener, "\r") != "---" {
		return matter, text, nil
	}
	if !found {
		// A file that is exactly "---": an opener with no closer.
		return matter, "", errors.New("unclosed front matter (missing closing ---)")
	}
	lines := strings.Split(rest, "\n")
	closer := -1
	for n, line := range lines {
		if strings.TrimSuffix(line, "\r") == "---" {
			closer = n
			break
		}
	}
	if closer < 0 {
		return matter, "", errors.New("unclosed front matter (missing closing ---)")
	}
	block := strings.Join(lines[:closer], "\n")
	body := strings.Join(lines[closer+1:], "\n")
	decoder := yaml.NewDecoder(strings.NewReader(block))
	decoder.KnownFields(true)
	if err := decoder.Decode(&matter); err != nil && !errors.Is(err, io.EOF) {
		return frontMatter{}, "", fmt.Errorf("front matter: %w", err)
	}
	return matter, body, nil
}

// validRepoPath accepts a repo-relative path: non-empty, no leading slash, and
// no "." or ".." segment.
func validRepoPath(path string) bool {
	if path == "" || strings.HasPrefix(path, "/") {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}
