// Package pulumitest is the live tests' harness: it builds the envoy-infra plugin from this module
// and runs the pulumi CLI on Pulumi YAML programs that use it, each stack on a file backend of its
// own in a temporary directory.
package pulumitest

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// SmokeImage returns ENVOY_SMOKE_IMAGE, the already-built listener image a live test runs. Unset,
// it fails the test at once and prints the build command: a build inside go test spends the test's
// deadline (LEGION-361), so no live test ever builds the image itself.
func SmokeImage(t testing.TB) string {
	t.Helper()
	image := os.Getenv("ENVOY_SMOKE_IMAGE")
	if image == "" {
		t.Fatal("ENVOY_SMOKE_IMAGE is unset, and this test never builds the image itself.\n" +
			"From the repository root:\n" +
			"  docker buildx build --load -t legion-envoy-smoke:local " +
			"-f packages/envoy/docker/Dockerfile .\n" +
			"then, in packages/envoy/infra, with the pulumi CLI on PATH:\n" +
			"  ENVOY_SMOKE_IMAGE=legion-envoy-smoke:local go test -count=1 -tags pulumilive -v " +
			"-timeout 15m ./host/\n" +
			"-count=1 because go's test cache cannot see the image behind a fixed tag, and would " +
			"replay a cached PASS against an image that had since changed. In CI the build is a " +
			"step of its own; see .github/workflows/envoy-and-contracts.yaml.")
	}
	return image
}

// PluginName is the plugin binary's file name, which Pulumi looks for in a plugin directory.
const PluginName = "pulumi-resource-envoy-infra"

// Plugin builds the plugin from this module into a directory of its own and returns the binary's
// path.
func Plugin(t testing.TB) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), PluginName)
	build := exec.Command("go", "build", "-ldflags", "-X main.version=0.0.1", "-o", bin,
		"github.com/sjawhar/envoy/infra/cmd/pulumi-resource-envoy-infra")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the plugin: %v\n%s", err, out)
	}
	return bin
}

// CLI runs pulumi in one directory, on a file backend of its own.
type CLI struct {
	t   testing.TB
	dir string
	env []string
}

// NewCLI returns a CLI that runs pulumi in dir with the caller's environment plus a file backend
// in a temporary directory, the test passphrase, and no update check.
func NewCLI(t testing.TB, dir string) *CLI {
	t.Helper()
	return &CLI{t: t, dir: dir, env: append(os.Environ(),
		"PULUMI_BACKEND_URL=file://"+t.TempDir(),
		"PULUMI_CONFIG_PASSPHRASE=test",
		"PULUMI_SKIP_UPDATE_CHECK=true",
	)}
}

// Run runs pulumi with args and returns its stdout, failing the test with its stderr and stdout
// when it exits non-zero.
func (c *CLI) Run(args ...string) string {
	c.t.Helper()
	stdout, stderr, err := c.exec(args...)
	if err != nil {
		c.t.Fatalf("pulumi %s: %v\nstderr:\n%s\nstdout:\n%s", strings.Join(args, " "), err, stderr, stdout)
	}
	return stdout
}

func (c *CLI) exec(args ...string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("pulumi", args...)
	cmd.Dir = c.dir
	cmd.Env = c.env
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// Stack is the stack "test" of a Pulumi YAML project that loads the plugin from a local path.
type Stack struct {
	*CLI
	project string
	plugin  string
}

// stackName is every Stack's stack.
const stackName = "test"

// NewStack creates the stack of a project named project, whose programs load the plugin binary
// plugin, and destroys whatever the stack holds when the test ends.
func NewStack(t testing.TB, plugin, project string) *Stack {
	t.Helper()
	s := &Stack{CLI: NewCLI(t, t.TempDir()), project: project, plugin: plugin}
	s.SetProgram(map[string]any{})
	s.Run("stack", "init", stackName, "--non-interactive")
	t.Cleanup(func() {
		if stdout, stderr, err := s.exec(s.args("destroy", "--yes", "--skip-preview")...); err != nil {
			t.Errorf("destroy the stack at the end of the test: %v\nstderr:\n%s\nstdout:\n%s", err, stderr, stdout)
		}
	})
	return s
}

// SetProgram writes the project's Pulumi.yaml with the program's top-level sections (resources,
// outputs) after the project's name, runtime and plugin path. It is written as JSON, which YAML
// reads, so no value needs quoting by hand.
func (s *Stack) SetProgram(program map[string]any) {
	s.t.Helper()
	project := map[string]any{
		"name":    s.project,
		"runtime": "yaml",
		"plugins": map[string]any{"providers": []any{
			map[string]any{"name": "envoy-infra", "path": filepath.Dir(s.plugin)},
		}},
	}
	for k, v := range program {
		project[k] = v
	}
	b, err := json.MarshalIndent(project, "", "  ")
	if err != nil {
		s.t.Fatalf("render Pulumi.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "Pulumi.yaml"), b, 0o600); err != nil {
		s.t.Fatalf("write Pulumi.yaml: %v", err)
	}
}

// URN returns the URN of the root-level resource name of type typ in this stack.
func (s *Stack) URN(typ, name string) string {
	return "urn:pulumi:" + stackName + "::" + s.project + "::" + typ + "::" + name
}

func (s *Stack) args(args ...string) []string {
	return append(args, "--stack", stackName, "--non-interactive")
}

// Up applies the program.
func (s *Stack) Up() {
	s.t.Helper()
	s.Run(s.args("up", "--yes", "--skip-preview")...)
}

// Destroy destroys every resource of the stack.
func (s *Stack) Destroy() {
	s.t.Helper()
	s.Run(s.args("destroy", "--yes", "--skip-preview")...)
}

// Step is one step of a preview: the operation and the resource it applies to.
type Step struct {
	Op  string `json:"op"`
	URN string `json:"urn"`
}

// Preview previews the program, unchanged resources included, and returns its steps.
func (s *Stack) Preview() []Step {
	s.t.Helper()
	out := s.Run(s.args("preview", "--json", "--show-sames")...)
	var preview struct {
		Steps []Step `json:"steps"`
	}
	if err := json.Unmarshal([]byte(out), &preview); err != nil {
		s.t.Fatalf("parse pulumi preview --json: %v\n%s", err, out)
	}
	return preview.Steps
}

// Resource is one resource of an exported deployment.
type Resource struct {
	URN      string `json:"urn"`
	Type     string `json:"type"`
	Parent   string `json:"parent"`
	Provider string `json:"provider"` // <provider URN>::<provider ID>
}

// Export returns the resources of the stack's deployment.
func (s *Stack) Export() []Resource {
	s.t.Helper()
	out := s.Run("stack", "export", "--stack", stackName)
	var export struct {
		Deployment struct {
			Resources []Resource `json:"resources"`
		} `json:"deployment"`
	}
	if err := json.Unmarshal([]byte(out), &export); err != nil {
		s.t.Fatalf("parse pulumi stack export: %v\n%s", err, out)
	}
	return export.Deployment.Resources
}
