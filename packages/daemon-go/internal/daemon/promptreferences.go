package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// promptKind is one form in which a Legion prompt names something Oh My Pi resolves only when a
// worker uses it: reference, the form a prompt writes; variable, the load probe's input and the
// prefix of its answers (probe.mjs); and a refusal's words for a name Oh My Pi cannot find.
type promptKind struct {
	reference                                         *regexp.Regexp
	variable                                          string
	noun, namedBy, consequence, remedy, discoveryName string
}

// promptKinds are the two forms: a task agent dispatched as `task(agent="<name>")` and a skill
// loaded as `skill://<name>`, whose name ends on a letter or digit, so a sentence's closing period
// is not read as part of it. A worker whose call or read names one Oh My Pi cannot find gets an
// error listing what it has, and carries on without it, so the load probe resolves every name.
var promptKinds = [...]promptKind{
	{
		reference:     regexp.MustCompile(`agent="([a-z0-9][a-z0-9._-]*)"`),
		variable:      "LEGION_PROMPT_AGENTS",
		noun:          "task agent",
		namedBy:       "dispatched by",
		consequence:   "a worker that calls one gets a tool result listing the agents it has, and carries on without it",
		remedy:        "pi-legion-envoy ships every agent its prompts dispatch; install the release built from this daemon's commit",
		discoveryName: "agent discovery",
	},
	{
		reference:   regexp.MustCompile(`skill://([a-z0-9](?:[a-z0-9._-]*[a-z0-9])?)`),
		variable:    "LEGION_PROMPT_SKILLS",
		noun:        "skill",
		namedBy:     "loaded by",
		consequence: "a worker told to load one reads `Unknown skill` and carries on without it",
		remedy: "pi-legion-envoy ships every skill its prompts load; install the release built from this daemon's commit, " +
			"and check that the settings this Oh My Pi reads (`disabledExtensions`, `skills`) neither disable nor filter it",
		discoveryName: "skill discovery",
	},
}

// promptNames holds, for each of promptKinds, every name Legion's prompts write in that form, each
// with the prompt files that write it.
type promptNames [len(promptKinds)]map[string][]string

// promptReferences are the task agents and skills Legion's prompts name: every reference in a
// Markdown file under the plugin's skills directories (the manifest's `omp.skills`, read with its
// contract, readPluginManifest) and its `agents/` directory, the agent definitions Oh My Pi
// discovers there (both named relative to the plugin), and under rolesDir, when set (named
// `roles/<file>`).
func promptReferences(manifest string, skills []string, rolesDir string) (promptNames, error) {
	var names promptNames
	for i := range names {
		names[i] = map[string][]string{}
	}
	root := filepath.Dir(manifest)
	for _, dir := range skills {
		if err := names.collect(root, filepath.Join(root, dir), ""); err != nil {
			return names, fmt.Errorf("pi-legion-envoy at %s ships skills in %s, which the gate cannot read: %w", manifest, dir, err)
		}
	}
	// A plugin without agents/ ships no agent definition, so none names anything.
	if err := names.collect(root, filepath.Join(root, "agents"), ""); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return names, fmt.Errorf("pi-legion-envoy at %s ships agents in agents/, which the gate cannot read: %w", manifest, err)
	}
	if rolesDir != "" {
		if err := names.collect(rolesDir, rolesDir, "roles"); err != nil {
			return names, fmt.Errorf("the role prompts directory %s cannot be read: %w", rolesDir, err)
		}
	}
	return names, nil
}

// RolePromptReferences are the task agents and skills the role prompts under rolesDir name, each
// with the `roles/<file>` that names it, encoded for `legion probe-image --role-references`: the
// daemon inlines its own role prompts into every Sandbox pod, so the image's probe must resolve
// what those name, not the image's copy.
func RolePromptReferences(rolesDir string) (string, error) {
	var names promptNames
	for i := range names {
		names[i] = map[string][]string{}
	}
	if err := names.collect(rolesDir, rolesDir, "roles"); err != nil {
		return "", fmt.Errorf("the role prompts directory %s cannot be read: %w", rolesDir, err)
	}
	encoded := map[string]map[string][]string{}
	for i, kind := range promptKinds {
		encoded[kind.variable] = names[i]
	}
	raw, err := json.Marshal(encoded)
	return string(raw), err
}

// addEncoded adds references RolePromptReferences encoded, refusing an encoding it cannot read.
func (names promptNames) addEncoded(raw string) error {
	var encoded map[string]map[string][]string
	if err := json.Unmarshal([]byte(raw), &encoded); err != nil {
		return fmt.Errorf("the role prompt references %q are not RolePromptReferences' encoding: %w", raw, err)
	}
	for i, kind := range promptKinds {
		for name, files := range encoded[kind.variable] {
			for _, file := range files {
				if !slices.Contains(names[i][name], file) {
					names[i][name] = append(names[i][name], file)
				}
			}
		}
	}
	return nil
}

// collect adds every reference in a Markdown file under dir, each file named by its path relative
// to base under prefix. dir may be a link to a directory (LEGION_ROLE_PROMPTS_DIR can name one),
// which the walk follows; filepath.WalkDir alone would report the link and read nothing under it.
func (names promptNames) collect(base, dir, prefix string) error {
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
		for i, kind := range promptKinds {
			for _, match := range kind.reference.FindAllSubmatch(body, -1) {
				if name := string(match[1]); !slices.Contains(names[i][name], file) {
					names[i][name] = append(names[i][name], file)
				}
			}
		}
		return nil
	})
}

// promptCheck is what the load probe must also find: names, the task agents and skills Legion's
// prompts name; with agentModels, that each of those agents runs on its own model (the probe's
// agentModels); and profile, the words for the OMP profile a pane loads the plugin from, which a
// refusal on the pane lane names (probeLoad decides the lane). The zero value asks for nothing.
type promptCheck struct {
	names       promptNames
	agentModels bool
	profile     string
}

// The load probe's answers on the task agents' models (probe.mjs agentModels).
const (
	agentModelsResolved     = "LEGION_AGENT_MODELS=resolved"
	agentModelUnresolved    = "LEGION_AGENT_MODEL_UNRESOLVED="
	agentModelsUnresolvable = "LEGION_AGENT_MODELS_UNRESOLVABLE="
	skipAgentModels         = "LEGION_SKIP_AGENT_MODELS"
)

// export is the shell command handing the load probe each kind's names and, for a pod, the plugin
// root ("$1") as the one extension root its discovery reads; empty when the check asks for
// nothing. Names are the references' alphabet, so single quotes hold them.
func (c promptCheck) export(pod bool) string {
	var assignments []string
	for i, kind := range promptKinds {
		if len(c.names[i]) > 0 {
			assignments = append(assignments, kind.variable+"='"+strings.Join(slices.Sorted(maps.Keys(c.names[i])), ",")+"'")
		}
	}
	if len(assignments) == 0 {
		return ""
	}
	if len(c.names[0]) > 0 && !c.agentModels {
		assignments = append(assignments, skipAgentModels+"=1")
	}
	if pod {
		assignments = append(assignments, `LEGION_PROMPT_ROOT="$1"`)
	}
	return "export " + strings.Join(assignments, " ")
}

// refusal judges the load probe's answers: nil when Oh My Pi found every name, else, for each kind
// with a name it could not find, the refusal naming each one, the prompt files that name it, and
// lane, how the probed Oh My Pi loaded the plugin.
func (c promptCheck) refusal(output, lane string) error {
	var refusals []error
	for i, kind := range promptKinds {
		refusals = append(refusals, kind.refusal(output, c.names[i], lane))
	}
	if c.agentModels {
		refusals = append(refusals, agentModelRefusal(output, c.names[0], lane))
	}
	return errors.Join(refusals...)
}

// agentModelRefusal judges the load probe's answer on the models of agents, the task agents
// Legion's prompts name: nil when there are none or each runs on its own model, else the refusal
// naming each that does not, the files that dispatch it, its model, and why. An agent the probe did
// not find is refused by name already (the task agents' kind), and has no model to judge.
func agentModelRefusal(output string, agents map[string][]string, lane string) error {
	if len(agents) == 0 {
		return nil
	}
	var unresolved []string
	for line := range strings.Lines(output) {
		line = strings.TrimSpace(line)
		switch {
		case line == agentModelsResolved:
			return nil
		case strings.HasPrefix(line, agentModelsUnresolvable):
			return fmt.Errorf("Oh My Pi, %s, could not resolve task agents' models for the load probe (%s): pin a fork release whose model resolver the probe can import",
				lane, strings.TrimPrefix(line, agentModelsUnresolvable))
		case strings.HasPrefix(line, agentModelUnresolved):
			// <agent> <model> <why>
			fields := strings.SplitN(strings.TrimPrefix(line, agentModelUnresolved), " ", 3)
			if len(fields) < 3 {
				return fmt.Errorf("the load probe answered %q on the task agents' models, which the gate cannot read", line)
			}
			unresolved = append(unresolved, fmt.Sprintf("task agent %s (dispatched by %s) on its model %s: %s",
				fields[0], strings.Join(agents[fields[0]], ", "), fields[1], fields[2]))
		}
	}
	if len(unresolved) == 0 {
		return fmt.Errorf("the load probe gave no answer on the models of the task agents Legion's prompts name (%s)",
			strings.Join(slices.Sorted(maps.Keys(agents)), ", "))
	}
	return fmt.Errorf("Oh My Pi, %s, cannot run %s. A worker that dispatches one runs it on another model, or not at all: configure each role the agents name in the settings this Oh My Pi reads (modelRoles, or task.agentModelOverrides), on a model whose key works",
		lane, strings.Join(unresolved, "; "))
}

// refusal judges the load probe's answer on named, this kind's names: nil when there are none or
// Oh My Pi found every one.
func (k promptKind) refusal(output string, named map[string][]string, lane string) error {
	if len(named) == 0 {
		return nil
	}
	for line := range strings.Lines(output) {
		line = strings.TrimSpace(line)
		if line == k.variable+"=resolved" {
			return nil
		}
		if rest, ok := strings.CutPrefix(line, k.variable+"_MISSING="); ok {
			var missing []string
			for _, name := range strings.Split(rest, ",") {
				missing = append(missing, name+" ("+k.namedBy+" "+strings.Join(named[name], ", ")+")")
			}
			return fmt.Errorf("Oh My Pi, %s, finds no %s %s: %s. %s", lane, k.noun, strings.Join(missing, "; "), k.consequence, k.remedy)
		}
		if rest, ok := strings.CutPrefix(line, k.variable+"_UNRESOLVABLE="); ok {
			return fmt.Errorf("Oh My Pi, %s, could not resolve %ss for the load probe (%s): pin a fork release whose %s the probe can import", lane, k.noun, rest, k.discoveryName)
		}
	}
	return fmt.Errorf("the load probe gave no answer on the %ss Legion's prompts name (%s)", k.noun, strings.Join(slices.Sorted(maps.Keys(named)), ", "))
}
