// Package promptrefs reads what Legion's prompts name that Oh My Pi resolves only when a worker
// uses it: the task agents they dispatch and the skills they load, each with the prompt files that
// name it. The boot gate and the image probe hand these names to the load probe (probe.mjs), and the
// daemon encodes its own role prompts' references for a Sandbox pod's probe (Roles).
package promptrefs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
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

// namePattern is each kind's name as a prompt can write it.
var namePattern = [kinds]string{
	TaskAgents: `[a-z0-9][a-z0-9._-]*`,
	Skills:     `[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?`,
}

// reference finds each kind's names in a prompt, and whole is a kind's name alone.
var (
	reference = [kinds]*regexp.Regexp{
		TaskAgents: regexp.MustCompile(`agent="(` + namePattern[TaskAgents] + `)"`),
		Skills:     regexp.MustCompile(`skill://(` + namePattern[Skills] + `)`),
	}
	whole = [kinds]*regexp.Regexp{
		TaskAgents: regexp.MustCompile(`^` + namePattern[TaskAgents] + `$`),
		Skills:     regexp.MustCompile(`^` + namePattern[Skills] + `$`),
	}
)

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

// add records that file names name in the form kind, once.
func (names Names) add(kind Kind, name, file string) {
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
				names.add(kind, string(match[1]), file)
			}
		}
		return nil
	})
}

// Roles are the references of the role prompts under rolesDir, each named `roles/<file>`, encoded
// for `legion probe-image --role-references`: the daemon hands its own role prompts to every
// worker, so a probe resolves what those name, not the probed image's copy.
func Roles(rolesDir string) (string, error) {
	names := New()
	if err := names.Collect(rolesDir, rolesDir, "roles"); err != nil {
		return "", fmt.Errorf("the role prompts directory %s cannot be read: %w", rolesDir, err)
	}
	encoded := map[string]map[string][]string{}
	for _, kind := range Kinds {
		encoded[kind.Variable()] = names[kind]
	}
	raw, err := json.Marshal(encoded)
	return string(raw), err
}

// AddEncoded adds the references Roles encoded, refusing anything but that encoding: one JSON
// object holding every kind once and no other key, each kind an object of its names (possibly
// none), each name once, as a prompt can write it, with the files that name it. An encoding that
// read as fewer references than it holds, or as names the load probe's input cannot carry, would
// let the probe pass without resolving the role prompts, so a daemon and an image that disagree on
// it refuse instead.
func (names Names) AddEncoded(raw string) error {
	read := New()
	seen := map[Kind]bool{}
	dec := json.NewDecoder(strings.NewReader(raw))
	err := members(dec, "the encoding", func(key string) error {
		kind := Kind(slices.Index(variable[:], key))
		switch {
		case kind < 0:
			return errors.New("unknown kind " + key)
		case seen[kind]:
			return errors.New(key + " appears twice")
		}
		seen[kind] = true
		return members(dec, key, func(name string) error {
			switch _, twice := read[kind][name]; {
			case twice:
				return fmt.Errorf("%s name %s appears twice", key, name)
			case !whole[kind].MatchString(name):
				return fmt.Errorf("%s name %q is not one a prompt can write", key, name)
			}
			var files []string
			if err := dec.Decode(&files); err != nil {
				return fmt.Errorf("%s name %s: %w", key, name, err)
			}
			if len(files) == 0 {
				return fmt.Errorf("%s name %s is named by no file", key, name)
			}
			read[kind][name] = files
			return nil
		})
	})
	if err == nil {
		if _, next := dec.Token(); next != io.EOF {
			err = errors.New("more than one JSON value")
		}
	}
	for _, kind := range Kinds {
		if err == nil && !seen[kind] {
			err = errors.New("no " + kind.Variable())
		}
	}
	if err != nil {
		return fmt.Errorf("the role prompt references %q are not promptrefs.Roles' encoding: %w", raw, err)
	}
	for _, kind := range Kinds {
		for name, files := range read[kind] {
			for _, file := range files {
				names.add(kind, name, file)
			}
		}
	}
	return nil
}

// members reads one JSON object from dec, calling member with each of its keys while dec stands at
// that key's value, which member reads. what names the object in a refusal of anything else.
func members(dec *json.Decoder, what string, member func(key string) error) error {
	open, err := dec.Token()
	if err != nil {
		return err
	}
	switch open {
	case json.Delim('{'):
	case nil:
		return fmt.Errorf("%s is null, not an object", what)
	default:
		return fmt.Errorf("%s is %v, not an object", what, open)
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return err
		}
		if err := member(key.(string)); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return err
}
