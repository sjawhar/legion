//go:build pulumilive

package host_test

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/internal/pulumitest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The live tests apply Pulumi YAML programs with the pulumi CLI against this machine's Docker. Each
// listener runs on the host's network on a free port, with a container name of its own, and
// reaches only a loopback NATS from testnats.

// TestMain removes the PULUMI_HOME the live tests share once they have run.
func TestMain(m *testing.M) { os.Exit(pulumitest.Main(m)) }

// healthWindow is how long a listener has to answer "healthy", and how long a negative control
// watches one that must not.
const healthWindow = 60 * time.Second

// dockerProvider is the explicit docker provider each program passes its listener, at the version
// the first consumer pins, the one the test binary installs (pulumitest.DockerPluginVersion).
var dockerProvider = map[string]any{
	"type":    "pulumi:providers:docker",
	"options": map[string]any{"version": pulumitest.DockerPluginVersion},
}

// freePorts returns n distinct loopback ports nothing listens on, holding each until all are
// picked.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	var ports []int
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer l.Close()
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	return ports
}

// containerName returns a container name no other run uses.
func containerName(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return "envoy-listener-live-" + hex.EncodeToString(b)
}

// health returns the status /healthz answers on port, or the error that kept it from answering.
func health(port int) (string, error) {
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	return body.Status, nil
}

// requireHealthy waits up to healthWindow for /healthz on port to answer "healthy".
func requireHealthy(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(healthWindow)
	var status string
	var err error
	for time.Now().Before(deadline) {
		if status, err = health(port); err == nil && status == "healthy" {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("listener on port %d did not answer \"healthy\" within %s: last status %q, error %v",
		port, healthWindow, status, err)
}

// requireNeverHealthy watches /healthz on port for healthWindow and fails the test if it ever
// answers "healthy".
func requireNeverHealthy(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(healthWindow)
	for time.Now().Before(deadline) {
		if status, err := health(port); err == nil && status == "healthy" {
			t.Fatalf("listener on port %d answered \"healthy\"", port)
		}
		time.Sleep(time.Second)
	}
}

// dockerCLI runs the docker CLI and returns its combined output and error.
func dockerCLI(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

// listener is an envoy-infra:host:Listener resource of a program, with the explicit docker
// provider.
func listener(properties map[string]any) map[string]any {
	return map[string]any{
		"type":       "envoy-infra:host:Listener",
		"properties": properties,
		"options":    map[string]any{"providers": []string{"${docker}"}},
	}
}

func TestLiveListenerRunsAHealthyListenerWithTheExplicitProvider(t *testing.T) {
	image := pulumitest.SmokeImage(t)
	_, natsURL := testnats.Start(t)
	port := freePorts(t, 1)[0]
	name := containerName(t)

	s := pulumitest.NewStack(t, pulumitest.Plugin(t), "listener-live")
	s.SetProgram(map[string]any{"resources": map[string]any{
		"docker": dockerProvider,
		"live": listener(map[string]any{
			"machineId":     "live-test",
			"natsUrls":      []string{natsURL},
			"image":         image,
			"port":          port,
			"containerName": name,
		}),
	}})
	s.Up()
	requireHealthy(t, port)

	var providerURN, containerProvider string
	for _, r := range s.Export() {
		switch r.Type {
		case "pulumi:providers:docker":
			providerURN = r.URN
		case "docker:index/container:Container":
			containerProvider = r.Provider
		}
	}
	require.Equal(t, s.URN("pulumi:providers:docker", "docker"), providerURN)
	require.True(t, strings.HasPrefix(containerProvider, providerURN+"::"),
		"the container's provider is %q, not the explicit docker provider %q", containerProvider, providerURN)

	s.Destroy()
	out, err := dockerCLI("container", "inspect", name)
	require.Error(t, err, "container %s is still there after destroy:\n%s", name, out)
	require.Contains(t, out, "No such container")
}

// TestLiveAdoptionPreviewsClean applies the Docker-host program's two root-level resources, then
// previews the same values as a listener that adopts them.
func TestLiveAdoptionPreviewsClean(t *testing.T) {
	image := pulumitest.SmokeImage(t)
	_, natsURL := testnats.Start(t)
	port := freePorts(t, 1)[0]
	name := containerName(t)
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)

	s := pulumitest.NewStack(t, pulumitest.Plugin(t), "adopt-live")
	// Program A: the table's inputs, written out as the Docker-host program writes them.
	s.SetProgram(map[string]any{"resources": map[string]any{
		"docker": dockerProvider,
		"envoy-image-live": map[string]any{
			"type":       "docker:index/remoteImage:RemoteImage",
			"properties": map[string]any{"name": image, "keepLocally": true},
			"options":    map[string]any{"provider": "${docker}"},
		},
		"listener-live": map[string]any{
			"type": "docker:index/container:Container",
			"properties": map[string]any{
				"name":                name,
				"image":               "${envoy-image-live.imageId}",
				"command":             []string{"/usr/local/bin/envoy-listener"},
				"restart":             "unless-stopped",
				"stopTimeout":         30,
				"destroyGraceSeconds": 30,
				"networkMode":         "host",
				"logOpts":             map[string]string{"max-file": "5", "max-size": "20m"},
				"envs": []string{
					fmt.Sprintf("PORT=%d", port),
					"ENVOY_MACHINE_ID=live-test",
					"NATS_URLS=" + natsURL,
					"ENVOY_HOST_BRIDGE=127.0.0.1",
				},
				"healthcheck": map[string]any{
					"tests":       []string{"CMD", "curl", "-f", healthURL},
					"interval":    "10s",
					"timeout":     "3s",
					"retries":     3,
					"startPeriod": "30s",
				},
				"wait":        true,
				"waitTimeout": 90,
			},
			"options": map[string]any{"provider": "${docker}", "deleteBeforeReplace": true},
		},
	}})
	s.Up()

	// Program B: the same provider and values, as a listener that adopts both resources.
	s.SetProgram(map[string]any{"resources": map[string]any{
		"docker": dockerProvider,
		"live": listener(map[string]any{
			"machineId":     "live-test",
			"natsUrls":      []string{natsURL},
			"image":         image,
			"port":          port,
			"containerName": name,
			"hostBridge":    "127.0.0.1",
			"adopt":         map[string]any{"container": "listener-live", "image": "envoy-image-live"},
		}),
	}})
	steps := s.Preview()

	component := s.URN("envoy-infra:host:Listener", "live")
	childURN := func(typ, name string) string { return s.URN("envoy-infra:host:Listener$"+typ, name) }
	ops := map[string]string{}
	for _, step := range steps {
		t.Logf("preview step: %-6s %s", step.Op, step.URN)
		ops[step.URN] = step.Op
	}
	require.Equal(t, "same", ops[childURN("docker:index/container:Container", "live-container")])
	require.Equal(t, "same", ops[childURN("docker:index/remoteImage:RemoteImage", "live-image")])
	require.Equal(t, "same", ops[s.URN("pulumi:providers:docker", "docker")])
	require.Equal(t, "create", ops[component])
	for urn, op := range ops {
		if op == "same" || urn == component {
			continue
		}
		// The plugin's own default provider exists only in state, so its create is allowed.
		require.True(t, op == "create" && strings.HasPrefix(urn, s.URN("pulumi:providers:envoy-infra", "default")),
			"step %s %s: the only steps besides same are the component's create and the plugin's default provider's", op, urn)
	}
	for _, old := range []string{
		s.URN("docker:index/container:Container", "listener-live"),
		s.URN("docker:index/remoteImage:RemoteImage", "envoy-image-live"),
	} {
		require.NotContains(t, ops, old, "the adopted root-level resource %s has a step of its own", old)
	}
}

func TestLiveSeedReachesNATS(t *testing.T) {
	image := pulumitest.SmokeImage(t)
	seed, public := testnats.User(t)
	natsURL := testnats.StartNkeyAuthorized(t, public)
	ports := freePorts(t, 2)
	seeded, unseeded := containerName(t), containerName(t)

	s := pulumitest.NewStack(t, pulumitest.Plugin(t), "seed-live")
	s.SetProgram(map[string]any{"resources": map[string]any{
		"docker": dockerProvider,
		"seeded": listener(map[string]any{
			"machineId":     "live-seeded",
			"natsUrls":      []string{natsURL},
			"image":         image,
			"port":          ports[0],
			"containerName": seeded,
			"seed":          map[string]any{"fn::secret": seed},
		}),
		// The negative control: Docker restarts it while NATS refuses it, so nothing waits on its
		// health.
		"unseeded": listener(map[string]any{
			"machineId":      "live-unseeded",
			"natsUrls":       []string{natsURL},
			"image":          image,
			"port":           ports[1],
			"containerName":  unseeded,
			"waitForHealthy": false,
		}),
	}})
	s.Up()

	requireHealthy(t, ports[0])
	requireNeverHealthy(t, ports[1])
	logs, err := dockerCLI("logs", unseeded)
	require.NoError(t, err, "docker logs %s:\n%s", unseeded, logs)
	require.Contains(t, strings.ToLower(logs), "authorization violation",
		"the unseeded listener's log shows NATS refusing it:\n%s", logs)
}

// TestLiveGoConsumersCanUseTheSDK generates the Go SDK from the plugin, as a consumer's
// `pulumi package add` does, and builds a program that constructs a listener through it.
func TestLiveGoConsumersCanUseTheSDK(t *testing.T) {
	plugin := pulumitest.Plugin(t)
	dir := t.TempDir()
	for file, content := range map[string]string{
		"Pulumi.yaml": "name: sdk-consumer\nruntime: go\n",
		"go.mod":      "module example.com/sdkconsumer\n\ngo 1.26.8\n",
		"main.go": `package main

import (
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/sdk/go/envoyinfra/host"
)

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		_, err := host.NewListener(ctx, "example-host-a", &host.ListenerArgs{
			MachineId: "example-host-a",
			NatsUrls:  pulumi.StringArray{pulumi.String("nats://127.0.0.1:4222")},
			Image:     "ghcr.io/sjawhar/legion/envoy:example",
			Seed:      pulumi.String("SUEXAMPLESEED"),
			Adopt:     host.AdoptArgs{Container: pulumi.String("listener-example-host-a")},
		})
		return err
	})
}
`,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600))
	}

	// package add writes the SDK under sdks/, points go.mod at it and resolves the program's
	// imports. The program is a module of its own, outside this workspace.
	t.Setenv("GOWORK", "off")
	pulumitest.NewCLI(t, dir).Run("package", "add", plugin)
	build := exec.Command("go", "build", "./...")
	build.Dir = dir
	out, err := build.CombinedOutput()
	require.NoError(t, err, "go build ./... of the SDK consumer:\n%s", out)
}
