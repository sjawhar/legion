package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const skillsCheckUsage = "legion skills-check --dir <skills volume> --omp <omp path>"

// ompPluginList is what `omp plugin list --json` answers, of which this reads the marketplace
// plugins alone: each entry's id is `<plugin>@<marketplace>`, and each of its install entries names
// the directory that scope's install put the plugin's files in. The npm plugins the image carries
// are no marketplace's and are not read. Unknown members are ignored on purpose: this is Oh My Pi's
// own output, not a contract Legion owns, and a field added to it must not fail a role's launch.
type ompPluginList struct {
	Marketplace []struct {
		ID      string `json:"id"`
		Entries []struct {
			Scope       string `json:"scope"`
			InstallPath string `json:"installPath"`
		} `json:"entries"`
	} `json:"marketplace"`
}

// installedSkills asks Oh My Pi where it installed each plugin of marketplace and answers the
// directory by plugin name. The path comes from Oh My Pi rather than from a layout Legion assumes,
// so a release that moves its plugin cache moves this with it. An entry with no path, and a plugin
// installed in more than one scope with differing paths, are each refused: the check has to know
// which bytes an agent would load, and two answers are not one.
func installedSkills(ctx context.Context, ompPath, marketplace string) (map[string]string, error) {
	command := exec.CommandContext(ctx, ompPath, "plugin", "list", "--json")
	out, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("%s plugin list --json exited %d: %s", ompPath, exit.ExitCode(), strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, fmt.Errorf("run %s plugin list --json: %w", ompPath, err)
	}
	var listed ompPluginList
	if err := json.Unmarshal(out, &listed); err != nil {
		return nil, fmt.Errorf("parse %s plugin list --json: %w", ompPath, err)
	}
	installed := map[string]string{}
	for _, plugin := range listed.Marketplace {
		name, named, found := strings.Cut(plugin.ID, "@")
		if !found || named != marketplace {
			continue
		}
		for _, entry := range plugin.Entries {
			if entry.InstallPath == "" {
				return nil, fmt.Errorf("%s reports plugin %q of marketplace %q installed in scope %q with no directory", ompPath, name, marketplace, entry.Scope)
			}
			if held, ok := installed[name]; ok && held != entry.InstallPath {
				return nil, fmt.Errorf("%s reports plugin %q of marketplace %q installed at both %s and %s: the check cannot say which bytes an agent would load",
					ompPath, name, marketplace, held, entry.InstallPath)
			}
			installed[name] = entry.InstallPath
		}
	}
	return installed, nil
}

// skillsCheck is what a role's start wrapper runs after it has installed the pod's skills: it
// compares the files Oh My Pi installed with the digests the pod's skills init container recorded
// from the clone at the commit it recorded (`workspace.CheckSkillsInstalled`, dispatch://INFRA-1300
// R4). A difference, a plugin the install left nowhere, and a plugin installed that the records do
// not name are each a refusal, with one line on stderr naming the plugin and what differed, and the
// role's launch then fails before Oh My Pi starts rather than an agent running with skills nobody
// can account for.
//
// It reads the records and runs Oh My Pi's own `plugin list --json`; it reaches no network, takes
// no lock and writes nothing. It is deliberately not part of `workspace-init`: those subcommands
// are a pod's init containers, and this one runs in the role's launch, where it may do nothing an
// init container's work would.
func skillsCheck(ctx context.Context, dir, ompPath string, stdout io.Writer) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("--dir must be an absolute path (got %q)", dir)
	}
	if !filepath.IsAbs(ompPath) {
		return fmt.Errorf("--omp must be an absolute path (got %q)", ompPath)
	}
	marketplace, err := skillsRecordLine(workspace.SkillsMarketplaceFile(dir))
	if err != nil {
		return err
	}
	commit, err := skillsRecordLine(workspace.SkillsCommitFile(dir))
	if err != nil {
		return err
	}
	installed, err := installedSkills(ctx, ompPath, marketplace)
	if err != nil {
		return err
	}
	if err := workspace.CheckSkillsInstalled(dir, installed); err != nil {
		return err
	}
	_, order, err := workspace.ReadSkillsTrees(dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "skills-check: %s of marketplace %s installed from %s\n", strings.Join(order, ", "), marketplace, commit)
	return nil
}

// skillsRecordLine is a one-line record of the skills volume, trimmed; an empty one is refused by
// name, since a role that installed from an empty marketplace name installed from nothing.
func skillsRecordLine(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the skills record %s: %w", path, err)
	}
	line := strings.TrimSpace(string(content))
	if line == "" {
		return "", fmt.Errorf("the skills record %s is empty", path)
	}
	return line, nil
}

func runSkillsCheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("skills-check", "usage: "+skillsCheckUsage, stderr)
	dir := flags.String("dir", "", "the pod's skills volume root, which its init container filled (required)")
	ompPath := flags.String("omp", "", "the Oh My Pi the pod runs, by its image path (required)")
	if code, ok := parseWorkspaceInitFlags(flags, args, stderr); !ok {
		return code
	}
	if err := skillsCheck(ctx, *dir, *ompPath, stdout); err != nil {
		fmt.Fprintf(stderr, "legion skills-check: %v\n", err)
		return 1
	}
	return 0
}
