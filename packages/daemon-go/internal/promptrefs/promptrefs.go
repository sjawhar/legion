// Package promptrefs reads what Legion's prompts name that Oh My Pi resolves only when a worker
// uses it: the task agents they dispatch and the skills they load, each with the prompt files that
// name it. The boot gate and the image probe hand these names to the load probe (probe.mjs), and the
// daemon encodes its own role prompts' references for a Sandbox pod's probe (Roles).
package promptrefs

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
)

// Kind is one form in which a prompt names something.
type Kind int

const (
	// TaskAgents are the agents a prompt dispatches as `task(agent="<name>")`.
	TaskAgents Kind = iota
	// Skills are the skills a prompt loads as `skill://<name>`, whose name ends on a letter or
	// digit, so a sentence's closing period is not read as part of it.
	Skills
	kinds
)

// Kinds are every kind, in order.
var Kinds = [kinds]Kind{TaskAgents, Skills}

var reference = [kinds]*regexp.Regexp{
	TaskAgents: regexp.MustCompile(`agent="([a-z0-9][a-z0-9._-]*)"`),
	Skills:     regexp.MustCompile(`skill://([a-z0-9](?:[a-z0-9._-]*[a-z0-9])?)`),
}

var variable = [kinds]string{TaskAgents: "LEGION_PROMPT_AGENTS", Skills: "LEGION_PROMPT_SKILLS"}

// Variable is the load probe's input listing a kind's names, and the prefix of its answers on them
// (probe.mjs); it also keys the kind in Roles' encoding.
func (k Kind) Variable() string { return variable[k] }

// Names holds, for each kind, every name the prompts read so far write in that form, each with the
// prompt files that write it.
type Names [kinds]map[string][]string

// New holds no name of any kind.
func New() Names {
	var names Names
	for i := range names {
		names[i] = map[string][]string{}
	}
	return names
}

// Add records that file names name in the form kind, once.
func (names Names) Add(kind Kind, name, file string) {
	if !slices.Contains(names[kind][name], file) {
		names[kind][name] = append(names[kind][name], file)
	}
}

// Collect adds every reference in a Markdown file under dir, each file named by its path relative
// to base under prefix. dir may be a link to a directory (LEGION_ROLE_PROMPTS_DIR can name one),
// which the walk follows; filepath.WalkDir alone would report the link and read nothing under it.
func (names Names) Collect(base, dir, prefix string) error {
	under, err := filepath.Rel(base, dir)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	return filepath.WalkDir(resolved, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".md" {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(resolved, path)
		if err != nil {
			return err
		}
		file := filepath.Join(prefix, under, rel)
		for _, kind := range Kinds {
			for _, match := range reference[kind].FindAllSubmatch(body, -1) {
				names.Add(kind, string(match[1]), file)
			}
		}
		return nil
	})
}

// CollectRoles adds every reference in the role prompts under rolesDir, each named `roles/<file>`.
func (names Names) CollectRoles(rolesDir string) error {
	if err := names.Collect(rolesDir, rolesDir, "roles"); err != nil {
		return fmt.Errorf("the role prompts directory %s cannot be read: %w", rolesDir, err)
	}
	return nil
}

// Roles are the references of the role prompts under rolesDir, encoded for `legion probe-image
// --role-references`: the daemon hands its own role prompts to every worker, so a probe resolves
// what those name, not the probed image's copy.
func Roles(rolesDir string) (string, error) {
	names := New()
	if err := names.CollectRoles(rolesDir); err != nil {
		return "", err
	}
	encoded := map[string]map[string][]string{}
	for _, kind := range Kinds {
		encoded[kind.Variable()] = names[kind]
	}
	raw, err := json.Marshal(encoded)
	return string(raw), err
}

// AddEncoded adds the references Roles encoded, refusing an encoding it cannot read.
func (names Names) AddEncoded(raw string) error {
	var encoded map[string]map[string][]string
	if err := json.Unmarshal([]byte(raw), &encoded); err != nil {
		return fmt.Errorf("the role prompt references %q are not promptrefs.Roles' encoding: %w", raw, err)
	}
	for _, kind := range Kinds {
		for name, files := range encoded[kind.Variable()] {
			for _, file := range files {
				names.Add(kind, name, file)
			}
		}
	}
	return nil
}
