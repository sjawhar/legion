package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// daemonTools are the binaries Legion itself runs: `legion gh`'s gh, and the git and jj of
// workspace provisioning. Boot resolves each once, so no Legion-owned child is a PATH lookup that
// whatever wrapper heads the operator's PATH could answer.
var daemonTools = []string{"gh", "git", "jj"}

// toolEnv is the pane variable that names a resolved tool, and its override at boot.
func toolEnv(tool string) string { return "LEGION_" + strings.ToUpper(tool) + "_PATH" }

// resolveTools resolves each daemon tool by its LEGION_<TOOL>_PATH override, which must be an
// absolute executable, or on PATH, and names every missing tool with its override in one error
// (packages/daemon/src/daemon/environment.ts:384-409).
func resolveTools(lookupEnv func(string) (string, bool)) (map[string]string, error) {
	path, _ := lookupEnv("PATH")
	tools := map[string]string{}
	var missing []string
	for _, tool := range daemonTools {
		if configured, ok := lookupEnv(toolEnv(tool)); ok && configured != "" {
			if !filepath.IsAbs(configured) || !isExecutable(configured) {
				return nil, fmt.Errorf("%s is not an absolute executable path: %q", toolEnv(tool), configured)
			}
			tools[tool] = configured
			continue
		}
		found := ""
		for _, dir := range filepath.SplitList(path) {
			candidate := filepath.Join(dir, tool)
			if dir != "" && filepath.IsAbs(candidate) && isExecutable(candidate) {
				found = candidate
				break
			}
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
	return tools, nil
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

// paneTools names each resolved tool by the variable a pane reads it from (LEGION_GH_PATH, …).
func paneTools(tools map[string]string) map[string]string {
	named := map[string]string{}
	for tool, path := range tools {
		named[toolEnv(tool)] = path
	}
	return named
}

func envValue(environ []string, name string) (string, bool) {
	for i := len(environ) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(environ[i], name+"="); ok {
			return value, true
		}
	}
	return "", false
}
