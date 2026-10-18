package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
)

// daemonTools are the binaries Legion itself runs on the host: the git and jj of workspace
// provisioning and removal (internal/workspace). Boot resolves each once, so no Legion-owned child
// is a PATH lookup that whatever wrapper heads the operator's PATH could answer. No gh is among
// them, and no pane is told any of them: a pane's gh, git and jj are whatever its PATH, the
// daemon's, gives, with its gh reading the role's App token from its GH_CONFIG_DIR.
var daemonTools = []string{"git", "jj"}

// toolEnv is the daemon's own override for a tool at boot, LEGION_<TOOL>_PATH; it is never a
// pane's variable.
func toolEnv(tool string) string { return "LEGION_" + strings.ToUpper(tool) + "_PATH" }

// resolveTools resolves each daemon tool by its LEGION_<TOOL>_PATH override, which must be an
// absolute executable, or on PATH. It names every missing tool with its override in one error, and
// refuses a jj older than the oldest the daemon runs (checkJJVersion).
func resolveTools(lookupEnv func(string) (string, bool)) (map[string]string, error) {
	tools := map[string]string{}
	var missing []string
	for _, tool := range daemonTools {
		found, err := findTool(lookupEnv, tool)
		if err != nil {
			return nil, err
		}
		if found == "" {
			missing = append(missing, fmt.Sprintf("%s (set %s to an absolute executable path)", tool, toolEnv(tool)))
			continue
		}
		tools[tool] = found
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required daemon tools: %s", strings.Join(missing, ", "))
	}
	if err := checkJJVersion(tools["jj"]); err != nil {
		return nil, err
	}
	return tools, nil
}

// findTool is tool's LEGION_<TOOL>_PATH override, which must be an absolute executable, else the
// first executable tool on PATH, else "".
func findTool(lookupEnv func(string) (string, bool), tool string) (string, error) {
	if configured, ok := lookupEnv(toolEnv(tool)); ok && configured != "" {
		if !filepath.IsAbs(configured) || !isExecutable(configured) {
			return "", fmt.Errorf("%s is not an absolute executable path: %q", toolEnv(tool), configured)
		}
		return configured, nil
	}
	path, _ := lookupEnv("PATH")
	for _, dir := range filepath.SplitList(path) {
		candidate := filepath.Join(dir, tool)
		if dir != "" && filepath.IsAbs(candidate) && isExecutable(candidate) {
			return candidate, nil
		}
	}
	return "", nil
}

// claudePluginDirEnv names the claude-envoy plugin's package root a Claude Code pane loads.
const claudePluginDirEnv = "LEGION_CLAUDE_PLUGIN_DIR"

// resolveClaude is how a pane runs Claude Code, for a configuration whose worker_backends puts a role
// on claude: `claude` and the `bun` its MCP server runs on, each by its LEGION_<TOOL>_PATH override
// or on PATH, and the claude-envoy plugin at LEGION_CLAUDE_PLUGIN_DIR, which must hold its built
// channel server and the generated Legion plugin (`bun run build` in packages/claude-envoy). Every
// gap is named in one refusal.
func resolveClaude(lookupEnv func(string) (string, bool)) (*tmux.ClaudeLaunch, error) {
	var launch tmux.ClaudeLaunch
	var missing []string
	for tool, into := range map[string]*string{"claude": &launch.Binary, "bun": &launch.Bun} {
		found, err := findTool(lookupEnv, tool)
		if err != nil {
			return nil, err
		}
		if found == "" {
			missing = append(missing, fmt.Sprintf("%s (set %s to an absolute executable path)", tool, toolEnv(tool)))
		}
		*into = found
	}
	dir, _ := lookupEnv(claudePluginDirEnv)
	switch {
	case dir == "":
		missing = append(missing, fmt.Sprintf("the claude-envoy plugin (set %s to its package directory)", claudePluginDirEnv))
	case !filepath.IsAbs(dir):
		return nil, fmt.Errorf("%s is not an absolute path: %q", claudePluginDirEnv, dir)
	default:
		for _, file := range []string{filepath.Join("dist", "envoy-channel.js"), filepath.Join("legion", ".claude-plugin", "plugin.json")} {
			if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
				return nil, fmt.Errorf("%s %s holds no %s: build the plugin (bun run build in packages/claude-envoy)", claudePluginDirEnv, dir, file)
			}
		}
		launch.PluginDir = dir
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, fmt.Errorf("worker_backends runs a role on claude, and boot is missing %s", strings.Join(missing, ", "))
	}
	return &launch, nil
}

// minimumJJ is the oldest jj the daemon runs, as major and minor: 0.38, the release that moved a
// repository's and a workspace's own configuration out of the repository and into the config
// home, under the id `.jj/repo/config-id` or `.jj/workspace-config-id` names (jj's CHANGELOG,
// 0.38.0). Provisioning and removal assume that layout: the runner removes a `.jj/repo/config.toml`
// with no config-id beside it as a legacy file jj would migrate into the configuration it reads
// (internal/workspace, disarmLegacyConfig), and on a jj before 0.38 that file is the repository's
// live configuration, the one `jj config set --repo` writes Legion's own
// git.abandon-unreachable-commits into. The worker image's jj is 0.45; this holds the host's jj,
// which the tmux runtime runs, to the same layout.
var minimumJJ = [2]int{0, 38}

// jjVersionTimeout bounds `jj --version`, which starts no other process.
const jjVersionTimeout = 30 * time.Second

// jjVersion reads the release from `jj --version`'s first line, `jj <major>.<minor>.<patch>`, a
// build's suffix after it (`-<hash>`, a fork's `-sami.<date>-<hash>`) ignored.
var jjVersion = regexp.MustCompile(`^jj (\d+)\.(\d+)\.\d+`)

// checkJJVersion runs jj --version and refuses a jj older than minimumJJ, or one whose version it
// cannot read, naming the path, what it found and LEGION_JJ_PATH.
func checkJJVersion(jj string) error {
	override := toolEnv("jj")
	floor := fmt.Sprintf("%d.%d", minimumJJ[0], minimumJJ[1])
	ctx, cancel := context.WithTimeout(context.Background(), jjVersionTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, jj, "--version")
	command.Env = runtime.WithoutNATSSeeds(os.Environ())
	command.WaitDelay = time.Second
	output, err := command.Output()
	if err != nil {
		var exited *exec.ExitError
		if errors.As(err, &exited) {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exited.Stderr)))
		}
		return fmt.Errorf("could not read the version of the jj at %s (`jj --version`: %v): set %s to an absolute path of jj %s or later", jj, err, override, floor)
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(output)), "\n")
	match := jjVersion.FindStringSubmatch(first)
	if match == nil {
		return fmt.Errorf("could not read the version of the jj at %s (`jj --version` printed %q): set %s to an absolute path of jj %s or later", jj, first, override, floor)
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	if major < minimumJJ[0] || (major == minimumJJ[0] && minor < minimumJJ[1]) {
		return fmt.Errorf("the jj at %s is %s, older than %s, the oldest the daemon runs: set %s to an absolute path of jj %s or later", jj, strings.TrimPrefix(first, "jj "), floor, override, floor)
	}
	return nil
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func envValue(environ []string, name string) (string, bool) {
	for i := len(environ) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(environ[i], name+"="); ok {
			return value, true
		}
	}
	return "", false
}
