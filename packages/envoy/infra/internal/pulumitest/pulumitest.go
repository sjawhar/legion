// Package pulumitest is the live tests' harness: it builds the envoy-infra plugin from this module
// and runs the pulumi CLI on Pulumi YAML programs that use it, each stack on a file backend of its
// own in a temporary directory, under a PULUMI_HOME the test binary makes for itself.
package pulumitest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// localRun is how to run the live tests by hand, from the repository root.
const localRun = "From the repository root, build the listener image and fetch the docker provider " +
	"plugin, checking its digest:\n" +
	"  docker buildx build --load -t legion-envoy-smoke:local -f packages/envoy/docker/Dockerfile .\n" +
	"  curl -fsSL -o /tmp/pulumi-resource-docker-v4.11.0-linux-amd64.tar.gz " +
	"https://github.com/pulumi/pulumi-docker/releases/download/v4.11.0/pulumi-resource-docker-v4.11.0-linux-amd64.tar.gz\n" +
	"  echo '4dbcd9defa0fb03999cdfeab50bd461451b7c65e8566927994f63ee956055ffe  " +
	"/tmp/pulumi-resource-docker-v4.11.0-linux-amd64.tar.gz' | sha256sum -c -\n" +
	"then, in packages/envoy/infra, with the pulumi CLI on PATH:\n" +
	"  ENVOY_SMOKE_IMAGE=legion-envoy-smoke:local " +
	"ENVOY_INFRA_DOCKER_PLUGIN=/tmp/pulumi-resource-docker-v4.11.0-linux-amd64.tar.gz " +
	"go test -count=1 -tags pulumilive -v -timeout 15m ./host/\n" +
	"-count=1 because go's test cache cannot see the image behind a fixed tag, and would " +
	"replay a cached PASS against an image that had since changed. In CI each is a step of its " +
	"own; see .github/workflows/envoy-and-contracts.yaml."

// SmokeImage returns ENVOY_SMOKE_IMAGE, the already-built listener image a live test runs. Unset,
// it fails the test at once and prints the build command: a build inside go test spends the test's
// deadline (LEGION-361), so no live test ever builds the image itself.
func SmokeImage(t testing.TB) string {
	t.Helper()
	image := os.Getenv("ENVOY_SMOKE_IMAGE")
	if image == "" {
		t.Fatal("ENVOY_SMOKE_IMAGE is unset, and this test never builds the image itself.\n" + localRun)
	}
	return image
}

// DockerPluginEnv names the docker provider plugin tarball, v4.11.0 for this machine's platform,
// that the test binary installs into its own PULUMI_HOME. The live programs pin that version, so
// the CLI loads it from there and never downloads a plugin itself.
const DockerPluginEnv = "ENVOY_INFRA_DOCKER_PLUGIN"

var (
	mainRuns bool
	homeOnce sync.Once
	home     string
	homeErr  error
)

// Main runs the package's tests, then removes the PULUMI_HOME they shared, and returns the exit
// code. A package whose tests use NewCLI calls it from TestMain: os.Exit(pulumitest.Main(m)).
func Main(m *testing.M) int {
	mainRuns = true
	code := m.Run()
	if home != "" {
		if err := os.RemoveAll(home); err != nil {
			fmt.Fprintf(os.Stderr, "remove the tests' PULUMI_HOME %s: %v\n", home, err)
			if code == 0 {
				code = 1
			}
		}
	}
	return code
}

// pulumiHome returns the test binary's PULUMI_HOME, made on first use with the docker plugin
// DockerPluginEnv names installed. It fails the test when that input is unset or the install
// fails, and when no TestMain calls Main, which removes the home.
func pulumiHome(t testing.TB) string {
	t.Helper()
	if !mainRuns {
		t.Fatal("pulumitest: call os.Exit(pulumitest.Main(m)) from the package's TestMain, " +
			"which removes the PULUMI_HOME the tests share")
	}
	homeOnce.Do(func() { home, homeErr = newHome() })
	if homeErr != nil {
		t.Fatal(homeErr)
	}
	return home
}

// newHome makes a temporary PULUMI_HOME and installs the docker plugin into it. It returns the
// directory even when the install fails, so Main removes it.
func newHome() (string, error) {
	tarball := os.Getenv(DockerPluginEnv)
	if tarball == "" {
		return "", errors.New(DockerPluginEnv + " is unset: it names the docker provider plugin " +
			"tarball the tests install, so the pulumi CLI never downloads one unchecked.\n" + localRun)
	}
	dir, err := os.MkdirTemp("", "envoy-infra-pulumi-home-")
	if err != nil {
		return "", fmt.Errorf("make the tests' PULUMI_HOME: %w", err)
	}
	install := exec.Command("pulumi", "plugin", "install", "resource", "docker", "v4.11.0", "--file", tarball)
	install.Env = append(os.Environ(), "PULUMI_HOME="+dir, "PULUMI_SKIP_UPDATE_CHECK=true")
	if out, err := install.CombinedOutput(); err != nil {
		return dir, fmt.Errorf("install the docker plugin from %s=%s: %v\n%s", DockerPluginEnv, tarball, err, out)
	}
	return dir, nil
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

// NewCLI returns a CLI that runs pulumi in dir with the caller's environment plus the test
// binary's PULUMI_HOME, a file backend in a temporary directory, the test passphrase, and no
// update check.
func NewCLI(t testing.TB, dir string) *CLI {
	t.Helper()
	return &CLI{t: t, dir: dir, env: append(os.Environ(),
		"PULUMI_HOME="+pulumiHome(t),
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
