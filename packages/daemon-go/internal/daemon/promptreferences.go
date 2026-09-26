package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/promptrefs"
)

// promptKind is a refusal's words for a name, of one promptrefs.Kind, that Oh My Pi cannot find.
type promptKind struct {
	noun, namedBy, consequence, remedy, discoveryName string
}

// promptKinds are the words for each promptrefs.Kind. A worker whose call or read names one Oh My
// Pi cannot find gets an error listing what it has, and carries on without it, so the load probe
// resolves every name.
var promptKinds = [len(promptrefs.Kinds)]promptKind{
	promptrefs.TaskAgents: {
		noun:          "task agent",
		namedBy:       "dispatched by",
		consequence:   "a worker that calls one gets a tool result listing the agents it has, and carries on without it",
		remedy:        "pi-legion-envoy ships every agent its prompts dispatch; install the release built from this daemon's commit",
		discoveryName: "agent discovery",
	},
	promptrefs.Skills: {
		noun:        "skill",
		namedBy:     "loaded by",
		consequence: "a worker told to load one reads `Unknown skill` and carries on without it",
		remedy: "pi-legion-envoy ships every skill its prompts load; install the release built from this daemon's commit, " +
			"and check that the settings this Oh My Pi reads (`disabledExtensions`, `skills`) neither disable nor filter it",
		discoveryName: "skill discovery",
	},
}

// promptReferences are the task agents and skills the plugin's own files name: every reference in
// a Markdown file under the plugin's skills directories (the manifest's `omp.skills`, read with its
// contract, readPluginManifest) and its `agents/` directory, the agent definitions Oh My Pi
// discovers there, each named relative to the plugin. The role prompts' references are added to
// them (promptrefs.Roles, Merge).
func promptReferences(manifest string, skills []string) (promptrefs.Names, error) {
	names := promptrefs.New()
	root := filepath.Dir(manifest)
	for _, dir := range skills {
		if err := names.Collect(root, filepath.Join(root, dir), ""); err != nil {
			return names, fmt.Errorf("pi-legion-envoy at %s ships skills in %s, which the gate cannot read: %w", manifest, dir, err)
		}
	}
	// A plugin without agents/ ships no agent definition, so none names anything.
	if err := names.Collect(root, filepath.Join(root, "agents"), ""); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return names, fmt.Errorf("pi-legion-envoy at %s ships agents in agents/, which the gate cannot read: %w", manifest, err)
	}
	return names, nil
}

// promptCheck is what the load probe must also find: names, the task agents and skills Legion's
// prompts name; and, unless skipAgentModels, that each of those agents runs on its own model (the
// probe's agentModels). The zero value asks for nothing.
type promptCheck struct {
	names           promptrefs.Names
	skipAgentModels bool
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
	for _, kind := range promptrefs.Kinds {
		if len(c.names[kind]) > 0 {
			assignments = append(assignments, kind.Variable()+"='"+strings.Join(slices.Sorted(maps.Keys(c.names[kind])), ",")+"'")
		}
	}
	if len(assignments) == 0 {
		return ""
	}
	if len(c.names[promptrefs.TaskAgents]) > 0 && c.skipAgentModels {
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
	for _, kind := range promptrefs.Kinds {
		refusals = append(refusals, promptKinds[kind].refusal(kind, output, c.names[kind], lane))
	}
	if !c.skipAgentModels {
		refusals = append(refusals, agentModelRefusal(output, c.names[promptrefs.TaskAgents], lane))
	}
	return errors.Join(refusals...)
}

// agentModelRefusal judges the load probe's answer on the models of agents, the task agents
// Legion's prompts name: nil when there are none or each runs on its own model, else the refusal
// naming each that does not, the files that dispatch it, its model, and why. An agent the probe did
// not find is refused by name already (the task agents' kind), and has no model to judge; and a
// probe that could not discover the task agents at all is refused by that kind too, and never asked
// about their models, so it gives no answer here to judge.
func agentModelRefusal(output string, agents map[string][]string, lane string) error {
	if len(agents) == 0 {
		return nil
	}
	discoveryFailed := unresolvable(promptrefs.TaskAgents)
	var unresolved []string
	for line := range strings.Lines(output) {
		line = strings.TrimSpace(line)
		switch {
		case line == agentModelsResolved, strings.HasPrefix(line, discoveryFailed):
			return nil
		case strings.HasPrefix(line, agentModelsUnresolvable):
			return fmt.Errorf("Oh My Pi, %s, could not resolve task agents' models for the load probe (%s): pin a fork release whose model resolver the probe can import",
				lane, strings.TrimPrefix(line, agentModelsUnresolvable))
		case strings.HasPrefix(line, agentModelUnresolved):
			var answer struct{ Agent, Model, Why string }
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, agentModelUnresolved)), &answer); err != nil || answer.Agent == "" {
				return fmt.Errorf("the load probe answered %q on the task agents' models, which the gate cannot read", line)
			}
			unresolved = append(unresolved, fmt.Sprintf("task agent %s (dispatched by %s) on its model %s: %s",
				answer.Agent, strings.Join(agents[answer.Agent], ", "), answer.Model, answer.Why))
		}
	}
	if len(unresolved) == 0 {
		// The probe answers from Oh My Pi's session_shutdown handler, which Oh My Pi gives 2 s at
		// the pinned release (SESSION_SHUTDOWN_HANDLER_TIMEOUT_MS, extensibility/extensions/runner.ts); at
		// that pin the process outlives the handler, so the answer still arrives.
		return fmt.Errorf("the load probe gave no answer on the models of the task agents Legion's prompts name (%s): "+
			"it answers from Oh My Pi's session_shutdown handler, which Oh My Pi abandons after 2 s, so a key command or token refresh slower than that, "+
			"under an Oh My Pi that ends when it abandons the handler, is the likely cause",
			strings.Join(slices.Sorted(maps.Keys(agents)), ", "))
	}
	return fmt.Errorf("Oh My Pi, %s, cannot run %s. A worker that dispatches one runs it on another model, or not at all: configure each role the agents name in the settings this Oh My Pi reads (modelRoles, or task.agentModelOverrides), on a model whose key works",
		lane, strings.Join(unresolved, "; "))
}

// unresolvable begins the load probe's answer that it could not discover a kind's names at all.
func unresolvable(kind promptrefs.Kind) string { return kind.Variable() + "_UNRESOLVABLE=" }

// refusal judges the load probe's answer on named, kind's names, in k's words: nil when there are
// none or Oh My Pi found every one.
func (k promptKind) refusal(kind promptrefs.Kind, output string, named map[string][]string, lane string) error {
	if len(named) == 0 {
		return nil
	}
	variable := kind.Variable()
	for line := range strings.Lines(output) {
		line = strings.TrimSpace(line)
		if line == variable+"=resolved" {
			return nil
		}
		if rest, ok := strings.CutPrefix(line, variable+"_MISSING="); ok {
			var missing []string
			for _, name := range strings.Split(rest, ",") {
				missing = append(missing, name+" ("+k.namedBy+" "+strings.Join(named[name], ", ")+")")
			}
			return fmt.Errorf("Oh My Pi, %s, finds no %s %s: %s. %s", lane, k.noun, strings.Join(missing, "; "), k.consequence, k.remedy)
		}
		if rest, ok := strings.CutPrefix(line, unresolvable(kind)); ok {
			return fmt.Errorf("Oh My Pi, %s, could not resolve %ss for the load probe (%s): pin a fork release whose %s the probe can import", lane, k.noun, rest, k.discoveryName)
		}
	}
	return fmt.Errorf("the load probe gave no answer on the %ss Legion's prompts name (%s)", k.noun, strings.Join(slices.Sorted(maps.Keys(named)), ", "))
}
