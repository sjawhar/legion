// Package prompts composes the role instructions the daemon gives an OMP pane, from the role
// prompts it embeds.
package prompts

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
)

// roleParts are the shared role prompts: each role's own part, the phase workers' cores and the
// mechanics fragments (Compose), and the controller's prompt (ControllerPromptPath).
//
//go:embed roles
var roleParts embed.FS

//go:embed go/*.md
var goParts embed.FS

// Parts is the ordered role-prompt files a runtime concatenates before its addressing and
// deployment-instruction fragments. A runtime must keep this list in one --append-system-prompt
// argument because OMP applies only the final occurrence of that flag.
type Parts struct {
	RolePromptPaths []string
}

// Composer keeps state-local copies of the shared role prompts and the daemon's additions.
type Composer struct {
	sharedDir string
	goDir     string
}

// eachPart calls visit with every file of an embedded prompt directory, by its path below that
// directory, in lexical order.
func eachPart(parts embed.FS, dir string, visit func(name string, body []byte) error) error {
	return fs.WalkDir(parts, dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := parts.ReadFile(path)
		if err != nil {
			return err
		}
		return visit(strings.TrimPrefix(path, dir+"/"), body)
	})
}

// RoleReferences are the task agents and skills the shared role prompts name, each file named
// `roles/<file>`: the daemon hands these prompts to every worker, so a gate or an image probe
// resolves what they name beside the plugin's own.
func RoleReferences() promptrefs.Names {
	names := promptrefs.New()
	if err := eachPart(roleParts, "roles", func(name string, body []byte) error {
		names.Text("roles/"+name, body)
		return nil
	}); err != nil {
		panic(fmt.Sprintf("prompts: read the embedded role prompts: %v", err))
	}
	return names
}

// New snapshots the embedded role prompts and the daemon's own parts below stateDir wherever the
// file there does not already hold them. The caller constructs it during daemon boot, and `legion
// controller start` for the controller's prompt. The files stay across restarts, since a resumed
// pane reads its prompt from the snapshot by path. The daemon owns these files; an operator's own
// text is the deployment `instructions`. A file already holding the current content is left as it
// is, so an ordinary restart changes nothing, and one an older daemon wrote is rewritten.
func New(stateDir string) (*Composer, error) {
	sharedDir := filepath.Join(stateDir, "prompts", "shared")
	if err := eachPart(roleParts, "roles", func(name string, body []byte) error {
		return syncPrompt(filepath.Join(sharedDir, name), body, "shared role prompt")
	}); err != nil {
		return nil, err
	}
	goDir := filepath.Join(stateDir, "prompts", "go")
	if err := eachPart(goParts, "go", func(name string, body []byte) error {
		return syncPrompt(filepath.Join(goDir, name), body, "Go daemon prompt")
	}); err != nil {
		return nil, err
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

// ControllerPromptPath is the state-local controller prompt the launcher can pass to Oh My Pi.
func (c *Composer) ControllerPromptPath() (string, error) {
	if c == nil {
		return "", fmt.Errorf("controller role prompt: nil composer")
	}
	return filepath.Join(c.sharedDir, "controller-root.md"), nil
}

// Compose returns the shared role parts followed by this daemon's parts: the role's own, then the
// text every architect (architect-common.md) or every phase worker (worker-common.md) shares. The
// runtime appends addressing and deployment instructions after these paths.
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
