package prompts

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The controller's prompt is its shared role part, which holds wherever it runs, then the daemon's
// part for how this controller was launched: in the operator's terminal by `legion controller
// start`, or headless in a pod the daemon launched (`controller: daemon`). The shared part never
// says which, so neither launch is told it runs the other way.
func TestTheControllersPromptIsItsRolePartThenItsLaunchsPart(t *testing.T) {
	stateDir := t.TempDir()
	composer, err := New(stateDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	shared := filepath.Join(stateDir, "prompts", "shared", "controller-root.md")
	for _, tc := range []struct {
		name     string
		headless bool
		part     string
		says     []string
		never    []string
	}{
		{"started by the operator", false, "controller-interactive.md",
			[]string{"`legion controller start`", "operator's terminal", "typed directly into this session"},
			[]string{"headless"}},
		{"launched by the daemon", true, "controller-headless.md",
			[]string{"headless", "`controller: daemon`", "Dispatch", "Envoy", "relaunches"},
			[]string{"legion controller start", "typed directly"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths, err := composer.ControllerPromptPaths(tc.headless)
			if err != nil {
				t.Fatalf("ControllerPromptPaths: %v", err)
			}
			if want := []string{shared, filepath.Join(stateDir, "prompts", "go", tc.part)}; !slices.Equal(paths, want) {
				t.Fatalf("paths = %q, want %q", paths, want)
			}
			text := ""
			for _, path := range paths {
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read %s: %v", path, err)
				}
				text += string(body)
			}
			for _, want := range tc.says {
				if !strings.Contains(text, want) {
					t.Errorf("the prompt does not say %q", want)
				}
			}
			for _, never := range tc.never {
				if strings.Contains(text, never) {
					t.Errorf("the prompt says %q, which is the other launch's", never)
				}
			}
		})
	}
}
