// Package prompts composes the role instructions the Go daemon gives an OMP pane.
package prompts

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
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

// Composer keeps state-local copies of the shared role prompts and the Go daemon additions.
type Composer struct {
	sharedDir string
	goDir     string
}

// ResolveRolePromptsDir chooses the explicit absolute override when present, otherwise the
// role-prompts directory beside the running legion executable. It refuses one missing any file of
// the shared bundle (CheckRolePrompts), naming LEGION_ROLE_PROMPTS_DIR, before any caller reads
// it. A deployed binary never reads its build checkout.
func ResolveRolePromptsDir(lookupEnv func(string) (string, bool)) (string, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find the legion executable for role prompts: %w", err)
	}
	dir := filepath.Join(filepath.Dir(executable), "role-prompts")
	if configured, set := lookupEnv("LEGION_ROLE_PROMPTS_DIR"); set {
		if !filepath.IsAbs(configured) {
			return "", fmt.Errorf("LEGION_ROLE_PROMPTS_DIR must be an absolute path (got %s)", configured)
		}
		dir = configured
	}
	if err := CheckRolePrompts(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// CheckRolePrompts refuses a role-prompt directory missing any file of the shared bundle, naming
// the directory and every missing file. It reads and writes nothing else, so `legion controller
// start`, whose prompt is `controller-root.md`, checks the same bundle a daemon boot does before
// its one daemon call.
func CheckRolePrompts(rolesDir string) error {
	missing := make([]string, 0)
	for _, name := range sharedPromptFiles {
		info, err := os.Stat(filepath.Join(rolesDir, name))
		if err != nil || !info.Mode().IsRegular() {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("Role prompts directory %s is missing %s (set LEGION_ROLE_PROMPTS_DIR to the directory holding pi-envoy's roles/*.md)", rolesDir, strings.Join(missing, ", "))
	}
	return nil
}

// RoleReferences reads only the prompt files boot snapshots and panes consume. CheckStart uses
// it against the resolved deployment bundle without writing; normal boot uses it against the
// snapshot.
func RoleReferences(rolesDir string) (promptrefs.Names, error) {
	names := promptrefs.New()
	for _, name := range sharedPromptFiles {
		path := filepath.Join(rolesDir, name)
		if err := names.File(rolesDir, path, "roles"); err != nil {
			return promptrefs.Names{}, fmt.Errorf("read shared role prompt %s: %w", path, err)
		}
	}
	return names, nil
}

// New validates the complete shared role bundle (CheckRolePrompts), then snapshots it and each
// embedded Go-specific prompt below stateDir wherever the file there does not already hold it. The
// caller constructs it during daemon boot. The files stay across restarts, since a resumed pane
// reads the running daemon's prompt snapshot rather than a deployment directory that can change
// after boot. The daemon owns these files; an operator's own text is the deployment
// `instructions`. A file already holding the current content is left as it is, so an ordinary
// restart changes nothing.
func New(rolesDir, stateDir string) (*Composer, error) {
	if err := CheckRolePrompts(rolesDir); err != nil {
		return nil, err
	}

	sharedDir := filepath.Join(stateDir, "prompts", "shared")
	for _, name := range sharedPromptFiles {
		source := filepath.Join(rolesDir, name)
		body, err := os.ReadFile(source)
		if err != nil {
			return nil, fmt.Errorf("read shared role prompt %s: %w", source, err)
		}
		if err := syncPrompt(filepath.Join(sharedDir, name), body, "shared role prompt"); err != nil {
			return nil, err
		}
	}

	goDir := filepath.Join(stateDir, "prompts", "go")
	entries, err := goParts.ReadDir("go")
	if err != nil {
		return nil, fmt.Errorf("list embedded Go daemon prompts: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		body, err := goParts.ReadFile(filepath.Join("go", name))
		if err != nil {
			return nil, fmt.Errorf("read embedded Go daemon prompt %s: %w", name, err)
		}
		if err := syncPrompt(filepath.Join(goDir, name), body, "Go daemon prompt"); err != nil {
			return nil, err
		}
	}
	return &Composer{sharedDir: sharedDir, goDir: goDir}, nil
}

func syncPrompt(path string, body []byte, kind string) error {
	written, err := os.ReadFile(path)
	if err == nil && bytes.Equal(written, body) {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect %s %s: %w", kind, path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s directory %s: %w", kind, filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("write %s %s: %w", kind, path, err)
	}
	return nil
}

// SharedRolePromptsDir is the state-local shared bundle that panes and boot probes both consume.
func (c *Composer) SharedRolePromptsDir() (string, error) {
	if c == nil {
		return "", fmt.Errorf("shared role prompts: nil composer")
	}
	return c.sharedDir, nil
}

// ControllerPromptPath is the state-local controller prompt the launcher can pass to Oh My Pi.
func (c *Composer) ControllerPromptPath() (string, error) {
	if c == nil {
		return "", fmt.Errorf("controller role prompt: nil composer")
	}
	return filepath.Join(c.sharedDir, "controller-root.md"), nil
}

// Compose returns the shipped daemon's shared role parts followed by this daemon's parts: the
// role's own, then the text every architect (architect-common.md) or every phase worker
// (worker-common.md) shares. The runtime appends addressing and deployment instructions after
// these paths.
func (c *Composer) Compose(role claim.Role, isRoot bool) (Parts, error) {
	if c == nil {
		return Parts{}, fmt.Errorf("compose role prompt: nil composer")
	}
	var shared, daemonParts []string
	switch role {
	case claim.RoleArchitect:
		name := "architect.md"
		if isRoot {
			name = "architect-root.md"
		}
		shared, daemonParts = []string{name}, []string{name, "architect-common.md"}
	case claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer:
		name := string(role)
		shared = []string{"core/common.md", filepath.Join("core", name+".md"), "mechanics/headless.md", name + ".md"}
		daemonParts = []string{name + ".md", "worker-common.md"}
	case claim.RoleMerger:
		shared, daemonParts = []string{"mechanics/headless.md", "merger.md"}, []string{"merger.md", "worker-common.md"}
	default:
		return Parts{}, fmt.Errorf("compose role prompt: unsupported role %q", role)
	}

	paths := make([]string, 0, len(shared)+len(daemonParts))
	for _, part := range shared {
		paths = append(paths, filepath.Join(c.sharedDir, part))
	}
	for _, part := range daemonParts {
		paths = append(paths, filepath.Join(c.goDir, part))
	}
	return Parts{RolePromptPaths: slices.Clone(paths)}, nil
}
