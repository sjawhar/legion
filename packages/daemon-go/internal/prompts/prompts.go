// Package prompts composes the role instructions the Go daemon gives an OMP pane.
package prompts

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// sharedPromptFiles is the complete role-prompt directory the shipped daemon validates at boot
// (packages/daemon/src/daemon/environment.ts:45-54). The non-spawned files stay in this list so a
// broken role bundle fails at boot rather than later when another daemon path needs it.
var sharedPromptFiles = []string{
	"architect-root.md",
	"controller-root.md",
	"architect.md",
	"planner.md",
	"implementer.md",
	"tester.md",
	"reviewer.md",
	"merger.md",
	"core/common.md",
	"core/planner.md",
	"core/implementer.md",
	"core/tester.md",
	"core/reviewer.md",
	"core/oracle.md",
	"mechanics/headless.md",
	"mechanics/interactive.md",
}

//go:embed go/*.md
var goParts embed.FS

// Parts is the ordered role-prompt files a runtime concatenates before its addressing and
// deployment-instruction fragments. A runtime must keep this list in one --append-system-prompt
// argument because OMP applies only the final occurrence of that flag.
type Parts struct {
	RolePromptPaths []string
}

// Composer keeps the source role-prompt directory and the state-local Go daemon additions.
type Composer struct {
	rolesDir string
	goDir    string
}

// SourceRolePromptsDir is the checkout's packages/pi-envoy/roles directory. Like the shipped
// source daemon's SOURCE_ROLE_PROMPTS_DIR, this intentionally follows the compiled source tree;
// deployments that package prompts elsewhere set LEGION_ROLE_PROMPTS_DIR.
func SourceRolePromptsDir() string {
	_, source, _, ok := goruntime.Caller(0)
	if !ok {
		return filepath.Join("packages", "pi-envoy", "roles")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "../../../..", "packages", "pi-envoy", "roles"))
}

// ResolveRolePromptsDir chooses the explicit absolute override when present, otherwise the
// checkout source directory. It mirrors the shipped daemon's LEGION_ROLE_PROMPTS_DIR contract.
func ResolveRolePromptsDir(lookupEnv func(string) (string, bool)) (string, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	if configured, set := lookupEnv("LEGION_ROLE_PROMPTS_DIR"); set {
		if !filepath.IsAbs(configured) {
			return "", fmt.Errorf("LEGION_ROLE_PROMPTS_DIR must be an absolute path (got %s)", configured)
		}
		return configured, nil
	}
	return SourceRolePromptsDir(), nil
}

// New validates the complete shared role bundle, then writes each embedded Go-specific prompt
// once below stateDir. The caller constructs it during daemon boot; retaining the generated files
// gives a resumed pane the same prompt even after a restart.
func New(rolesDir, stateDir string) (*Composer, error) {
	missing := make([]string, 0)
	for _, name := range sharedPromptFiles {
		info, err := os.Stat(filepath.Join(rolesDir, name))
		if err != nil || !info.Mode().IsRegular() {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("Role prompts directory %s is missing %s (set LEGION_ROLE_PROMPTS_DIR to the directory holding pi-envoy's roles/*.md)", rolesDir, strings.Join(missing, ", "))
	}

	goDir := filepath.Join(stateDir, "prompts", "go")
	if err := os.MkdirAll(goDir, 0o700); err != nil {
		return nil, fmt.Errorf("create Go daemon prompt directory %s: %w", goDir, err)
	}
	for _, name := range []string{"architect-root.md", "architect.md", "planner.md", "implementer.md", "tester.md", "reviewer.md", "merger.md"} {
		path := filepath.Join(goDir, name)
		if _, err := os.Lstat(path); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("inspect Go daemon prompt %s: %w", path, err)
		}
		body, err := goParts.ReadFile(filepath.Join("go", name))
		if err != nil {
			return nil, fmt.Errorf("read embedded Go daemon prompt %s: %w", name, err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return nil, fmt.Errorf("write Go daemon prompt %s: %w", path, err)
		}
	}
	return &Composer{rolesDir: rolesDir, goDir: goDir}, nil
}

// Compose returns the shipped daemon's shared role parts followed by this daemon's role-specific
// override. The runtime appends addressing and deployment instructions after these paths.
func (c *Composer) Compose(role claim.Role, isRoot bool) (Parts, error) {
	if c == nil {
		return Parts{}, fmt.Errorf("compose role prompt: nil composer")
	}
	var shared []string
	var goPart string
	switch role {
	case claim.RoleArchitect:
		if isRoot {
			shared, goPart = []string{"architect-root.md"}, "architect-root.md"
		} else {
			shared, goPart = []string{"architect.md"}, "architect.md"
		}
	case claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer:
		name := string(role)
		shared = []string{"core/common.md", filepath.Join("core", name+".md"), "mechanics/headless.md", name + ".md"}
		goPart = name + ".md"
	case claim.RoleMerger:
		shared, goPart = []string{"mechanics/headless.md", "merger.md"}, "merger.md"
	default:
		return Parts{}, fmt.Errorf("compose role prompt: unsupported role %q", role)
	}

	paths := make([]string, 0, len(shared)+1)
	for _, part := range shared {
		paths = append(paths, filepath.Join(c.rolesDir, part))
	}
	paths = append(paths, filepath.Join(c.goDir, goPart))
	return Parts{RolePromptPaths: slices.Clone(paths)}, nil
}
