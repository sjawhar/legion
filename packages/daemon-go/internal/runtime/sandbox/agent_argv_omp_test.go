package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// realOmp is the pinned Oh My Pi binary LEGION_TEST_OMP names, as for internal/modelroute's route
// test; GitHub Actions installs it (.github/actions/install-omp), so a run there never skips.
func realOmp(t *testing.T) string {
	t.Helper()
	omp := os.Getenv("LEGION_TEST_OMP")
	switch {
	case omp != "":
		return omp
	case os.Getenv("GITHUB_ACTIONS") == "true":
		t.Fatal("LEGION_TEST_OMP is unset on GitHub Actions: name the pinned Oh My Pi binary (the daemon-go job installs it)")
	}
	t.Skip("LEGION_TEST_OMP names no Oh My Pi binary")
	return ""
}

// importMarker is a module whose top-level code writes name into the test's marker directory, so
// a marker is present exactly when Oh My Pi imported the module.
func importMarker(name string) string {
	return "import { writeFileSync } from \"node:fs\";\n" +
		"writeFileSync(process.env.LEGION_TEST_MARKS + \"/" + name + "\", \"imported\\n\");\n" +
		"export default function () {}\n"
}

// A pod's agent runs agentArgv in the repository's working copy. On the pinned binary that argv
// imports none of the repository's extensions, hooks or TypeScript custom commands, each of which
// runs its module code at import, while the Legion plugin still loads as the one explicit
// extension. The control drops --no-extensions and keeps the plugin root, and imports all four:
// without it, a missing marker could mean a fixture Oh My Pi never looks at.
func TestTheAgentArgvImportsNoRepositoryExtensionHookOrCommand(t *testing.T) {
	omp := realOmp(t)
	dir := t.TempDir()
	plugin, repo, home := filepath.Join(dir, "plugin"), filepath.Join(dir, "repo"), filepath.Join(dir, "home")
	for path, content := range map[string]string{
		filepath.Join(plugin, "package.json"):                             `{"name":"legion-test-plugin","version":"0.0.1","omp":{"extensions":["extension.js"]}}`,
		filepath.Join(plugin, "extension.js"):                             importMarker("plugin"),
		filepath.Join(repo, ".omp", "extensions", "repository.ts"):        importMarker("extension"),
		filepath.Join(repo, ".omp", "hooks", "pre", "repository.ts"):      importMarker("hook"),
		filepath.Join(repo, ".omp", "commands", "repository", "index.ts"): importMarker("command"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "--quiet", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	pod := launch{prompt: "The test's system prompt."}.agentArgv([]string{omp})
	root := slices.Index(pod, legionPlugin)
	if root < 0 {
		t.Fatalf("agentArgv %q names no %s", pod, legionPlugin)
	}
	pod[root] = plugin
	control := slices.DeleteFunc(slices.Clone(pod), func(arg string) bool { return arg == "--no-extensions" })

	for _, testCase := range []struct {
		name string
		argv []string
		want []string
	}{
		{"the pod's argv", pod, []string{"plugin"}},
		{"the discovery-on control", control, []string{"command", "extension", "hook", "plugin"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			marks := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, testCase.argv[0], testCase.argv[1:]...)
			cmd.Dir = repo
			cmd.Env = []string{"HOME=" + home, "PATH=/usr/local/bin:/usr/bin:/bin", "LEGION_TEST_MARKS=" + marks}
			// One RPC request, then end of input: Oh My Pi answers it after its session has loaded
			// every extension, hook and command, and exits when stdin closes.
			cmd.Stdin = strings.NewReader(`{"type":"get_state","id":"1"}` + "\n")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("omp %s: %v\n%s", strings.Join(testCase.argv[1:], " "), err, out)
			}
			entries, err := os.ReadDir(marks)
			if err != nil {
				t.Fatal(err)
			}
			var imported []string
			for _, entry := range entries {
				imported = append(imported, entry.Name())
			}
			slices.Sort(imported)
			if !slices.Equal(imported, testCase.want) {
				t.Errorf("omp %s imported %q, want %q", strings.Join(testCase.argv[1:], " "), imported, testCase.want)
			}
		})
	}
}
