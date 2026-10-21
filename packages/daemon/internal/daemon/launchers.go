package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/omplaunch"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
)

// launchReads is what boot reads to launch panes on one harness: the pane's Launcher, the log line
// that names what boot resolved, and, for Oh My Pi, the invocation its boot gate runs too.
type launchReads struct {
	launcher   tmux.Launcher
	log        func(*slog.Logger)
	invocation string // Oh My Pi's resolved invocation; "" for every other harness
}

// launchResolver resolves one harness's launch from the configuration and the daemon's environment
// (lookup, and getenv for the OMP invocation), writing nothing.
type launchResolver func(cfg config.Config, lookup func(string) (string, bool), getenv func(string) string) (launchReads, error)

// launchResolvers is each harness's boot resolution, by the backend it launches (runtime.Backends).
var launchResolvers = map[runtime.Backend]launchResolver{
	runtime.BackendOmp:    resolveOmp,
	runtime.BackendClaude: resolveClaude,
}

// resolveLaunchers is every launch the configuration's backends need (config.Backends), resolved
// in that order; Oh My Pi's only when boot runs the host's Oh My Pi (hostOMP).
func resolveLaunchers(cfg config.Config, lookup func(string) (string, bool), getenv func(string) string, hostOMP bool) (map[runtime.Backend]launchReads, error) {
	reads := map[runtime.Backend]launchReads{}
	for _, backend := range cfg.Backends() {
		if backend == runtime.BackendOmp && !hostOMP {
			continue
		}
		resolve, ok := launchResolvers[backend]
		if !ok {
			return nil, fmt.Errorf("the daemon has no launch for backend %q under runtime: tmux", backend)
		}
		read, err := resolve(cfg, lookup, getenv)
		if err != nil {
			return nil, err
		}
		reads[backend] = read
	}
	return reads, nil
}

// resolveOmp is the OMP invocation every pane and the boot gate run, whose one command is `mise
// where <tool>` when the invocation names a mise tool (omplaunch.ResolveInvocation).
func resolveOmp(cfg config.Config, _ func(string) (string, bool), getenv func(string) string) (launchReads, error) {
	invocation, err := omplaunch.ResolveInvocation(cfg.OmpInvocation, getenv)
	if err != nil {
		return launchReads{}, err
	}
	return launchReads{
		launcher:   tmux.NewOmpLauncher(cfg.OmpLaunchPrefix, invocation),
		invocation: invocation,
		log: func(log *slog.Logger) {
			log.Info("legion daemon resolved OMP invocation for boot probes and panes", "invocation", invocation)
		},
	}, nil
}

// claudePluginDirEnv names the claude-envoy plugin's package root a Claude Code pane loads.
const claudePluginDirEnv = "LEGION_CLAUDE_PLUGIN_DIR"

// resolveClaude is how a pane runs Claude Code, for a configuration whose worker_backends puts a role
// on claude: `claude` and the `bun` its MCP server runs on, each by its LEGION_<TOOL>_PATH override
// or on PATH, and the claude-envoy plugin at LEGION_CLAUDE_PLUGIN_DIR, which must hold its built
// channel server and the generated Legion plugin (`bun run build` in packages/claude-envoy). Every
// gap is named in one refusal.
func resolveClaude(_ config.Config, lookupEnv func(string) (string, bool), _ func(string) string) (launchReads, error) {
	var launch tmux.ClaudeLaunch
	var missing []string
	for tool, into := range map[string]*string{"claude": &launch.Binary, "bun": &launch.Bun} {
		found, err := resolveTool(lookupEnv, tool)
		if err != nil {
			return launchReads{}, err
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
		return launchReads{}, fmt.Errorf("%s is not an absolute path: %q", claudePluginDirEnv, dir)
	default:
		for _, file := range []string{filepath.Join("dist", "envoy-channel.js"), filepath.Join("legion", ".claude-plugin", "plugin.json")} {
			if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
				return launchReads{}, fmt.Errorf("%s %s holds no %s: build the plugin (bun run build in packages/claude-envoy)", claudePluginDirEnv, dir, file)
			}
		}
		launch.PluginDir = dir
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return launchReads{}, fmt.Errorf("worker_backends runs a role on claude, and boot is missing %s", strings.Join(missing, ", "))
	}
	return launchReads{
		launcher: tmux.NewClaudeLauncher(launch),
		log: func(log *slog.Logger) {
			log.Info("legion daemon resolved Claude Code for the roles worker_backends puts on claude",
				"claude", launch.Binary, "plugin", launch.PluginDir, "bun", launch.Bun)
		},
	}, nil
}
