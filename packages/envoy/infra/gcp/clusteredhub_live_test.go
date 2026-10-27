//go:build natslive

package gcp_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/nats"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The tests in this file run what gcp:ClusteredNatsHub deploys on this machine, so they carry the
// natslive tag. They need no container runtime: each installs the tools it runs with go install,
// at the versions the hub runs, into its own temporary directory, and a tool that does not install
// fails the test. The chart is the vendored archive, so no test reaches the chart repository.
//
// The tools are the helm pulumi-kubernetes v4.34.2's provider renders charts with (its
// provider/go.mod), and the nats-server, config reloader and exporter chart 2.15.1 runs by default
// (values.yaml: container.image.tag 2.15.1-alpine, reloader.image.tag 0.24.0,
// promExporter.image.tag 0.20.2), the versions the hub's pinned images hold.
const (
	helmTool       = "helm.sh/helm/v3/cmd/helm@v3.20.2"
	natsServerTool = "github.com/nats-io/nats-server/v2@v2.15.1"
	reloaderTool   = "github.com/nats-io/nack/cmd/nats-server-config-reloader@v0.24.0"
	exporterTool   = "github.com/nats-io/prometheus-nats-exporter@v0.20.2"

	// clusteredHubChartSHA256 is the SHA-256 the chart index
	// (https://nats-io.github.io/k8s/helm/charts/index.yaml) publishes for nats 2.15.1, whose
	// archive is vendored in testdata.
	clusteredHubChartSHA256 = "b209c62784b1b24c714d5ff6503b4c7dd948ec187d4222beee16b579f1258cc1"
)

// installTool installs module, a main package at a pinned version, with go install into dir, and
// returns the path of its binary, named binary, or why it did not install.
func installTool(dir, module, binary string) (string, error) {
	cmd := exec.Command("go", "install", module)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOBIN="+dir, "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go install %s: %v\n%s", module, err, out)
	}
	path := filepath.Join(dir, binary)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("go install %s wrote no %s: %w", module, binary, err)
	}
	return path, nil
}

// liveTool is a main package at a pinned version and the name of the binary it installs.
type liveTool struct{ module, binary string }

// installTools installs each of tools with installTool into dir, all at once, and returns their
// binaries' paths in tools' order, failing the test with each tool that did not install.
func installTools(t *testing.T, dir string, tools ...liveTool) []string {
	t.Helper()
	paths := make([]string, len(tools))
	errs := make([]error, len(tools))
	var wg sync.WaitGroup
	for i, tool := range tools {
		wg.Go(func() { paths[i], errs[i] = installTool(dir, tool.module, tool.binary) })
	}
	wg.Wait()
	require.NoError(t, errors.Join(errs...))
	return paths
}

// liveHubKeys are the keys of the users the live hub serves: the operator's and the host's seeds,
// which the test's clients connect with, and the session user's public key, whose seed nobody
// keeps.
type liveHubKeys struct {
	operatorSeed, operatorKey string
	hostSeed, hostKey         string
	sessionKey                string
}

// liveHubArgs are clusteredArgs with keys' users: an operator with explicit permissions, a host
// holding core.HostPermissions and the session user, a storage class, a priority class, sizes
// other than the defaults, and a client Service named apart from the release, so each reaches the
// rendering by its own value.
func liveHubArgs(keys liveHubKeys) *gcp.ClusteredNatsHubArgs {
	args := clusteredArgs()
	args.ServiceName = "envoy-nats-client"
	args.StorageClassName = pulumi.StringRef("premium-rwo")
	args.DiskSizeGb = pulumi.IntRef(12)
	args.MaxFileStoreGb = pulumi.IntRef(8)
	args.PriorityClassName = pulumi.StringRef("envoy-critical")
	args.Users = []gcp.NATSUserArgs{
		{Name: "live-operator", NKey: keys.operatorKey, Permissions: &gcp.PermissionsArgs{
			Publish:   nats.RulesArgs{Allow: []string{">"}},
			Subscribe: nats.RulesArgs{Allow: []string{">"}, Deny: []string{"$SYS.>"}},
		}},
		{Name: "live-host", NKey: keys.hostKey, MachineID: pulumi.StringRef("live-host")},
	}
	args.SessionUser = &gcp.SessionUserArgs{Name: "live-sessions", NKey: keys.sessionKey}
	return args
}

// liveAuthConf is the auth.conf the hub renders for args, the text its chart's ConfigMap holds.
func liveAuthConf(t *testing.T, args *gcp.ClusteredNatsHubArgs) string {
	t.Helper()
	s, err := gcp.ResolveClusteredNatsHub(args)
	require.NoError(t, err)
	return s.AuthConf
}

// clusteredHubRender is what helm renders from the values the hub registers its chart with: the
// manifests, and the ConfigMap's nats.conf and auth.conf, for the users of keys.
type clusteredHubRender struct {
	keys               liveHubKeys
	settings           gcp.ClusteredNatsHubSettings
	archive            string
	release, namespace string
	manifests          []map[string]any
	natsConf, authConf string
}

var (
	renderMu sync.Mutex
	rendered *clusteredHubRender
)

// renderClusteredHub renders the hub's chart once per test binary, so the cluster test runs the
// configuration the chart test checked: it runs the hub under the mocks for liveHubArgs with fresh
// keys, writes the values its chart registration holds to a file, and runs helm template on the
// vendored archive of the chart and version the registration names, under its release name and
// namespace, after checking the archive's SHA-256.
func renderClusteredHub(t *testing.T) *clusteredHubRender {
	t.Helper()
	renderMu.Lock()
	defer renderMu.Unlock()
	if rendered != nil {
		return rendered
	}
	var keys liveHubKeys
	keys.operatorSeed, keys.operatorKey = testnats.User(t)
	keys.hostSeed, keys.hostKey = testnats.User(t)
	_, keys.sessionKey = testnats.User(t)
	settings, err := gcp.ResolveClusteredNatsHub(liveHubArgs(keys))
	require.NoError(t, err)

	m := runClusteredHub(t, liveHubArgs(keys))
	chart := m.One(t, chartType)
	release, namespace := chart.Inputs["name"].StringValue(), chart.Inputs["namespace"].StringValue()
	archive := filepath.Join("testdata", chart.Inputs["chart"].StringValue()+"-"+chart.Inputs["version"].StringValue()+".tgz")
	require.Equal(t, clusteredHubChartSHA256, fileSHA256(t, archive), "%s is the archive the chart index publishes", archive)

	dir := t.TempDir()
	values, err := json.Marshal(chartValues(t, m))
	require.NoError(t, err)
	valuesFile := filepath.Join(dir, "values.json")
	require.NoError(t, os.WriteFile(valuesFile, values, 0o600))
	helm, err := installTool(dir, helmTool, "helm")
	require.NoError(t, err)
	home := filepath.Join(dir, "helm-home")
	cmd := exec.Command(helm, "template", release, archive, "-n", namespace, "-f", valuesFile)
	cmd.Env = append(os.Environ(), "HELM_CACHE_HOME="+home, "HELM_CONFIG_HOME="+home, "HELM_DATA_HOME="+home)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, "helm template: %s", stderr.String())

	r := &clusteredHubRender{
		keys: keys, settings: settings, archive: archive, release: release, namespace: namespace,
		manifests: decodeManifests(t, out),
	}
	data := r.one(t, "ConfigMap", release+"-config")["data"].(map[string]any)
	r.natsConf, r.authConf = data["nats.conf"].(string), data[clusteredHubAuthConfKey].(string)
	t.Logf("%s rendered release %s in namespace %s from %s (sha256 %s) with the values of the hub's chart registration: %d manifests",
		helmTool, release, namespace, archive, clusteredHubChartSHA256, len(r.manifests))
	rendered = r
	return r
}

// clusteredHubAuthConfKey is the ConfigMap key holding the users' authorization, which nats.conf
// includes.
const clusteredHubAuthConfKey = "auth.conf"

// fileSHA256 is the hex SHA-256 of the file at path.
func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// decodeManifests decodes helm template's output, a YAML stream, into its non-empty documents.
func decodeManifests(t *testing.T, out []byte) []map[string]any {
	t.Helper()
	var manifests []map[string]any
	decoder := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return manifests
		}
		require.NoError(t, err)
		if doc != nil {
			manifests = append(manifests, doc)
		}
	}
}

// one is the rendered manifest of kind named name, failing the test unless there is exactly one.
func (r *clusteredHubRender) one(t *testing.T, kind, name string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, m := range r.manifests {
		if m["kind"] == kind && dig(t, m, "metadata", "name") == name {
			found = append(found, m)
		}
	}
	require.Len(t, found, 1, "the rendering holds one %s %s", kind, name)
	return found[0]
}

// container is the hub pods' container named name, as the rendered StatefulSet holds it.
func (r *clusteredHubRender) container(t *testing.T, name string) map[string]any {
	t.Helper()
	return podContainer(t, dig(t, r.one(t, "StatefulSet", r.release), "spec", "template").(map[string]any), name)
}

// dig is the value at path in v, each step a key of an object or an index of an array, failing
// the test when a step is missing.
func dig(t *testing.T, v any, path ...any) any {
	t.Helper()
	for i, step := range path {
		switch key := step.(type) {
		case string:
			object, ok := v.(map[string]any)
			require.True(t, ok, "%v is an object", path[:i])
			v, ok = object[key]
			require.True(t, ok, "%v is set", path[:i+1])
		case int:
			array, ok := v.([]any)
			require.True(t, ok && key < len(array), "%v has an item %d", path[:i], key)
			v = array[key]
		}
	}
	return v
}

// stringMap is v, a YAML object of strings, as a map of strings.
func stringMap(t *testing.T, v any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for key, value := range v.(map[string]any) {
		s, ok := value.(string)
		require.True(t, ok, "%s is a string", key)
		out[key] = s
	}
	return out
}

// stringList is v, a YAML array of strings, as a slice.
func stringList(t *testing.T, v any) []string {
	t.Helper()
	var out []string
	for _, item := range v.([]any) {
		s, ok := item.(string)
		require.True(t, ok, "%v is a string", item)
		out = append(out, s)
	}
	return out
}

// The chart the hub installs renders what the hub's values say. helm, at the version
// pulumi-kubernetes's provider renders with, renders the vendored chart from the values the hub
// registers its chart with: one StatefulSet of three servers spread one per zone, with their
// claims, images, priority class and session-user annotation and no config checksum; the reloader
// watching auth.conf beside nats.conf; the ConfigMap holding the hub's auth.conf and a nats.conf
// that includes it, clusters and runs JetStream within the store size; the budget; and the client
// Service, on the nats port alone. No nats-box Deployment and no test Pod.
func TestClusteredNatsHubChart(t *testing.T) {
	r := renderClusteredHub(t)
	s := r.settings
	t.Logf("chart archive %s sha256 %s", r.archive, clusteredHubChartSHA256)

	var kinds []string
	for _, m := range r.manifests {
		kinds = append(kinds, fmt.Sprintf("%s %s", m["kind"], dig(t, m, "metadata", "name")))
	}
	t.Logf("rendered: %s", strings.Join(kinds, ", "))
	require.ElementsMatch(t, []string{
		"StatefulSet " + s.Name, "Service " + s.ServiceName, "Service " + s.Name + "-headless",
		"ConfigMap " + s.Name + "-config", "PodDisruptionBudget " + s.Name,
	}, kinds, "no nats-box Deployment, no test Pod, nothing else")

	statefulSet := r.one(t, "StatefulSet", s.Name)
	require.EqualValues(t, 3, dig(t, statefulSet, "spec", "replicas"))
	require.Equal(t, s.PodSelector, stringMap(t, dig(t, statefulSet, "spec", "selector", "matchLabels")),
		"the hub's selector is the StatefulSet's")
	pod := dig(t, statefulSet, "spec", "template")
	spread := dig(t, pod, "spec", "topologySpreadConstraints").([]any)
	require.Len(t, spread, 1)
	require.Equal(t, "topology.kubernetes.io/zone", dig(t, spread[0], "topologyKey"))
	require.EqualValues(t, 1, dig(t, spread[0], "maxSkew"))
	require.EqualValues(t, 3, dig(t, spread[0], "minDomains"))
	require.Equal(t, "DoNotSchedule", dig(t, spread[0], "whenUnsatisfiable"))
	require.Equal(t, s.PodSelector, stringMap(t, dig(t, spread[0], "labelSelector", "matchLabels")))

	claims := dig(t, statefulSet, "spec", "volumeClaimTemplates").([]any)
	require.Len(t, claims, 1)
	require.Equal(t, fmt.Sprintf("%dGi", s.DiskSizeGb), dig(t, claims[0], "spec", "resources", "requests", "storage"))
	require.Equal(t, s.StorageClassName, dig(t, claims[0], "spec", "storageClassName"))

	var names []string
	for _, c := range dig(t, pod, "spec", "containers").([]any) {
		names = append(names, c.(map[string]any)["name"].(string))
	}
	require.Equal(t, []string{"nats", "reloader", "prom-exporter"}, names)
	require.Equal(t, s.NatsImage, r.container(t, "nats")["image"])
	require.Equal(t, s.ReloaderImage, r.container(t, "reloader")["image"])
	require.Equal(t, s.ExporterImage, r.container(t, "prom-exporter")["image"])
	// nats-server runs under GOMEMLIMIT at 80% of its memory limit, the chart's container.env
	// rendered as a plain env var: liveHubArgs leave natsMemory at 2Gi, so ⌊0.8 × 2048⌋ MiB.
	require.Equal(t, "2Gi", s.NatsMemory)
	require.Equal(t, "2Gi", dig(t, r.container(t, "nats"), "resources", "limits", "memory"))
	require.Contains(t, r.container(t, "nats")["env"], map[string]any{"name": "GOMEMLIMIT", "value": "1638MiB"})
	require.Equal(t, []string{
		"-pid", "/var/run/nats/nats.pid", "-config", "/etc/nats-config/nats.conf", "-config", "/etc/nats-config/auth.conf",
	}, stringList(t, r.container(t, "reloader")["args"]), "the reloader watches auth.conf beside nats.conf")
	require.Equal(t, []any{map[string]any{"name": "prom-metrics", "containerPort": 7777}},
		r.container(t, "prom-exporter")["ports"], "the exporter's port is the one the PodMonitoring scrapes")

	annotations := dig(t, pod, "metadata", "annotations").(map[string]any)
	require.NotContains(t, annotations, "checksum/config")
	require.Equal(t, map[string]any{"envoy-infra.sjawhar.dev/no-auth-user": r.keys.sessionKey}, annotations)
	require.Equal(t, s.PriorityClassName, dig(t, pod, "spec", "priorityClassName"))

	require.Equal(t, s.AuthConf, r.authConf, "the ConfigMap's auth.conf is the hub's rendering")
	require.Regexp(t, `(?m)^\s*include auth\.conf;$`, r.natsConf)
	compact := strings.Join(strings.Fields(r.natsConf), "")
	var routes []string
	for i := range 3 {
		routes = append(routes, fmt.Sprintf(`"nats://%s-%d.%s-headless:6222"`, s.Name, i, s.Name))
	}
	require.Contains(t, compact, fmt.Sprintf(`"cluster":{"name":%q,"no_advertise":true,"port":6222,"routes":[%s]}`, s.Name, strings.Join(routes, ",")))
	require.Contains(t, compact, fmt.Sprintf(`"jetstream":{"max_file_store":%dGi,"max_memory_store":0,"store_dir":"/data"}`, s.MaxFileStoreGb))
	t.Logf("nats.conf:\n%s", r.natsConf)

	budget := r.one(t, "PodDisruptionBudget", s.Name)
	require.EqualValues(t, 1, dig(t, budget, "spec", "maxUnavailable"))
	require.Equal(t, s.PodSelector, stringMap(t, dig(t, budget, "spec", "selector", "matchLabels")), "the hub's selector is the budget's")

	service := r.one(t, "Service", s.ServiceName)
	require.Equal(t, s.PodSelector, stringMap(t, dig(t, service, "spec", "selector")), "the hub's selector is the client Service's")
	ports := dig(t, service, "spec", "ports").([]any)
	require.Len(t, ports, 1, "the client Service exposes one port")
	require.Equal(t, "nats", dig(t, ports[0], "name"))
	require.EqualValues(t, 4222, dig(t, ports[0], "port"))
}

// hubServer is one nats-server of the local cluster, on the rendered nats.conf and auth.conf in a
// directory of its own.
type hubServer struct {
	name                   string
	dir                    string
	client, monitor, route int
	binary                 string
	args, env              []string
	proc                   *cmdtest.Process
}

// start starts the server and waits until it listens on all three of its ports (its last bind is
// the route port's); the test kills it by its PID when it ends if it is still running.
func (s *hubServer) start(t *testing.T) {
	t.Helper()
	listening := fmt.Sprintf("Listening for route connections on 127.0.0.1:%d", s.route)
	s.proc = startBound(t, s.name, func() *exec.Cmd {
		cmd := exec.Command(s.binary, s.args...)
		cmd.Env = append(os.Environ(), s.env...)
		return cmd
	}, func(p *cmdtest.Process) error {
		if !strings.Contains(p.Output.String(), listening) {
			return errors.New("not listening yet")
		}
		return nil
	})
}

// startLogged starts cmd as cmdtest.Start does, and logs its output when the test fails.
func startLogged(t *testing.T, name string, cmd *exec.Cmd) *cmdtest.Process {
	t.Helper()
	p := cmdtest.Start(t, name, cmd)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s output:\n%s", name, p.Output.String())
		}
	})
	return p
}

// startBound starts the process build makes and waits up to 30 s for ready to return nil. A
// process that exits because a port it binds is taken is started again, from a new build, up to
// 10 times: cmdtest.FreeTCPPort's ports lie inside the kernel's ephemeral range where that range
// starts below them (gVisor's is 16000-65535), so an outgoing connection can hold one when the
// process binds it.
func startBound(t *testing.T, name string, build func() *exec.Cmd, ready func(*cmdtest.Process) error) *cmdtest.Process {
	t.Helper()
	for attempt := 1; ; attempt++ {
		p := startLogged(t, name, build())
		err := awaitReady(p, ready)
		if err == nil {
			return p
		}
		if attempt == 10 || !strings.Contains(p.Output.String(), "address already in use") {
			t.Fatalf("%s did not start (attempt %d): %v\n%s", name, attempt, err, p.Output.String())
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// awaitReady waits up to 30 s for ready to return nil while p runs, and returns ready's last error
// otherwise.
func awaitReady(p *cmdtest.Process, ready func(*cmdtest.Process) error) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case <-p.Exited:
			return errors.New("the process exited")
		default:
		}
		err := ready(p)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// kill sends the server SIGKILL and waits for it to exit.
func (s *hubServer) kill(t *testing.T) {
	t.Helper()
	require.NoError(t, s.proc.Cmd.Process.Signal(syscall.SIGKILL))
	s.proc.WaitExit(t, "SIGKILL")
}

func (s *hubServer) url() string { return fmt.Sprintf("nats://127.0.0.1:%d", s.client) }

// monitorJSON decodes the answer of the server's monitoring endpoint path into into.
func (s *hubServer) monitorJSON(path string, into any) error {
	return getJSON(fmt.Sprintf("http://127.0.0.1:%d%s", s.monitor, path), into)
}

var monitorClient = &http.Client{Timeout: 2 * time.Second}

func getJSON(url string, into any) error {
	resp, err := monitorClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// streamLastSeq is the last sequence of the server's own copy of stream, from its /jsz.
func (s *hubServer) streamLastSeq(stream string) (uint64, error) {
	var jsz struct {
		Accounts []struct {
			Streams []struct {
				Name  string `json:"name"`
				State struct {
					LastSeq uint64 `json:"last_seq"`
				} `json:"state"`
			} `json:"stream_detail"`
		} `json:"account_details"`
	}
	if err := s.monitorJSON("/jsz?accounts=true&streams=true", &jsz); err != nil {
		return 0, err
	}
	for _, a := range jsz.Accounts {
		for _, st := range a.Streams {
			if st.Name == stream {
				return st.State.LastSeq, nil
			}
		}
	}
	return 0, fmt.Errorf("%s holds no copy of %s", s.name, stream)
}

// authorizedUser is the user the server authorized conn's connection as: an nkey user's public
// key, the session user's for a client presenting no credential.
func (s *hubServer) authorizedUser(t *testing.T, conn *natsgo.Conn) string {
	t.Helper()
	cid, err := conn.GetClientID()
	require.NoError(t, err)
	var connz struct {
		Connections []struct {
			AuthorizedUser string `json:"authorized_user"`
		} `json:"connections"`
	}
	require.NoError(t, s.monitorJSON(fmt.Sprintf("/connz?auth=true&cid=%d", cid), &connz))
	require.Len(t, connz.Connections, 1, "%s has the connection %d", s.name, cid)
	return connz.Connections[0].AuthorizedUser
}

// count is how often the server's output holds text.
func (s *hubServer) count(text string) int { return strings.Count(s.proc.Output.String(), text) }

// within calls check every 100 ms until it returns nil, and returns how long that took, failing
// the test with check's last error when timeout passes first.
func within(t *testing.T, timeout time.Duration, what string, check func() error) time.Duration {
	t.Helper()
	start := time.Now()
	for {
		err := check()
		if err == nil {
			return time.Since(start)
		}
		if time.Since(start) > timeout {
			t.Fatalf("%s: not within %s: %v", what, timeout, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// podArgs is a container's rendered arguments with each key of replace, a value fixed by the pod,
// replaced by its value on this machine, failing the test when one is in no argument, so a chart
// that moved it is caught here.
func podArgs(t *testing.T, rendered []string, replace map[string]string) []string {
	t.Helper()
	args := slices.Clone(rendered)
	for old, local := range replace {
		found := false
		for i := range args {
			if strings.Contains(args[i], old) {
				args[i], found = strings.ReplaceAll(args[i], old, local), true
			}
		}
		require.True(t, found, "%q is in the rendered arguments %q", old, rendered)
	}
	return args
}

// seedOption authenticates a connection as the nkey user whose seed is seed.
func seedOption(t *testing.T, seed string) natsgo.Option {
	t.Helper()
	user, err := nkeys.FromSeed([]byte(seed))
	require.NoError(t, err)
	public, err := user.PublicKey()
	require.NoError(t, err)
	return natsgo.Nkey(public, user.Sign)
}

// connectHub connects to urls as the user whose seed is seed, reconnecting to any of them without
// limit, closed when the test ends.
func connectHub(t *testing.T, urls []string, seed string, opts ...natsgo.Option) *natsgo.Conn {
	t.Helper()
	conn, err := natsgo.Connect(strings.Join(urls, ","), append([]natsgo.Option{
		seedOption(t, seed), natsgo.Timeout(time.Second), natsgo.MaxReconnects(-1), natsgo.ReconnectWait(100 * time.Millisecond),
	}, opts...)...)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	return conn
}

// awaitViolation requires the next error the server sent conn to be its permissions violation for
// want, within 5 s.
func awaitViolation(t *testing.T, conn *natsgo.Conn, errs <-chan error, want string) {
	t.Helper()
	require.NoError(t, conn.Flush())
	select {
	case err := <-errs:
		require.ErrorIs(t, err, natsgo.ErrPermissionViolation)
		require.EqualError(t, err, "nats: permissions violation: Permissions Violation for "+want)
	case <-time.After(5 * time.Second):
		t.Fatalf("the server reported no permissions violation within 5s, want one for %s", want)
	}
}

// writeAtomically replaces the file at path with text by a rename, as Kubernetes swaps a
// ConfigMap's files: a reader sees the old file or the new one, never part of either.
func writeAtomically(t *testing.T, path, text string) {
	t.Helper()
	tmp := path + ".new"
	require.NoError(t, os.WriteFile(tmp, []byte(text), 0o600))
	require.NoError(t, os.Rename(tmp, path))
}

// promSeries is one series of an exporter's scrape.
type promSeries struct {
	name   string
	labels map[string]string
	value  float64
}

var promLabel = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)

// scrape reads every series the exporter on port serves.
func scrape(port int) ([]promSeries, error) {
	resp, err := monitorClient.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var series []promSeries
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s := promSeries{labels: map[string]string{}}
		rest := line
		if open := strings.IndexByte(line, '{'); open >= 0 {
			end := strings.LastIndexByte(line, '}')
			s.name, rest = line[:open], line[end+1:]
			for _, m := range promLabel.FindAllStringSubmatch(line[open+1:end], -1) {
				s.labels[m[1]] = m[2]
			}
		} else {
			s.name, rest, _ = strings.Cut(line, " ")
		}
		if s.value, err = strconv.ParseFloat(strings.Fields(rest)[0], 64); err != nil {
			return nil, fmt.Errorf("%q: %w", line, err)
		}
		series = append(series, s)
	}
	return series, nil
}

// lastSeqMetric is the exporter's metric of a stream copy's last sequence, under the chart's
// -prefix=nats.
const lastSeqMetric = "nats_stream_last_seq"

// streamSeries is every series of stream the exporters on ports serve, each exporter scraped
// once; an exporter whose server is down serves none.
func streamSeries(ports []int, stream string) ([]promSeries, error) {
	var out []promSeries
	for _, port := range ports {
		series, err := scrape(port)
		if err != nil {
			return nil, err
		}
		for _, s := range series {
			if s.labels["stream_name"] == stream {
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// lastSeqs is the last_seq series among series, by the server_name label.
func lastSeqs(series []promSeries) map[string]float64 {
	out := map[string]float64{}
	for _, s := range series {
		if s.name == lastSeqMetric {
			out[s.labels["server_name"]] = s.value
		}
	}
	return out
}

// awaitLastSeqs waits up to 30 s for the exporters on ports to report want last_seq series of
// stream, each value, and returns them.
func awaitLastSeqs(t *testing.T, ports []int, stream string, want int, value float64) map[string]float64 {
	t.Helper()
	var got map[string]float64
	within(t, 30*time.Second, fmt.Sprintf("%d %s series of %s at %v", want, lastSeqMetric, stream, value), func() error {
		series, err := streamSeries(ports, stream)
		if err != nil {
			return err
		}
		got = lastSeqs(series)
		if len(got) != want {
			return fmt.Errorf("%d series: %v", len(got), got)
		}
		for server, v := range got {
			if v != value {
				return fmt.Errorf("%s reports %v", server, v)
			}
		}
		return nil
	})
	return got
}

// The configuration the chart renders for the hub runs as a cluster on three local nats-server
// v2.15.1 processes, each on the rendered nats.conf and auth.conf: they form one cluster with a
// JetStream meta leader and serve the hub's users; a stream at three copies keeps every
// acknowledged message exactly once across a SIGKILL of its leader and recovers the killed copy;
// the exporter, run as the chart runs it, reports a copy's last sequence per server; and the
// reloader, run as the chart runs it, applies a users change to every server with no restart and
// no reconnect, while a session-user change is refused by every server's reload and takes effect
// only on a restart, which is why the hub rolls its pods for one.
func TestClusteredNatsHubCluster(t *testing.T) {
	r := renderClusteredHub(t)
	bin := t.TempDir()
	tools := installTools(t, bin,
		liveTool{natsServerTool, "nats-server"},
		liveTool{reloaderTool, "nats-server-config-reloader"},
		liveTool{exporterTool, "prometheus-nats-exporter"})
	natsServer, reloader, exporter := tools[0], tools[1], tools[2]

	natsContainer := r.container(t, "nats")
	require.Contains(t, natsContainer["env"], map[string]any{"name": "SERVER_NAME", "value": "$(POD_NAME)"},
		"each server is named after its pod")
	servers := make([]*hubServer, 3)
	for i := range servers {
		servers[i] = &hubServer{
			name: fmt.Sprintf("%s-%d", r.release, i), dir: t.TempDir(), binary: natsServer,
			client: cmdtest.FreeTCPPort(t), monitor: cmdtest.FreeTCPPort(t), route: cmdtest.FreeTCPPort(t),
		}
	}
	for i, s := range servers {
		// jetstream.store_dir: /data, the pod's claim, is the one value replaced in the server's
		// copy of nats.conf: nats-server v2.15.1 reads the file onto the options its flags set and
		// refuses -sd beside a store_dir in the file ("Duplicate 'store_dir' configuration"), so
		// no command-line override exists for it.
		storeDir := `"store_dir": "/data"`
		require.Equal(t, 1, strings.Count(r.natsConf, storeDir), "the rendered nats.conf sets %s once", storeDir)
		natsConf := strings.Replace(r.natsConf, storeDir, fmt.Sprintf(`"store_dir": %q`, filepath.Join(s.dir, "data")), 1)
		require.NoError(t, os.WriteFile(filepath.Join(s.dir, "nats.conf"), []byte(natsConf), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(s.dir, clusteredHubAuthConfKey), []byte(r.authConf), 0o600))
		var routes []string
		for j, other := range servers {
			if j != i {
				routes = append(routes, fmt.Sprintf("nats://127.0.0.1:%d", other.route))
			}
		}
		// The container's own arguments (--config /etc/nats-config/nats.conf) with the ConfigMap's
		// mount replaced by the server's directory, then a command-line override for each other
		// value of nats.conf that a pod fixes, since three servers here share one host and
		// filesystem:
		s.args = append(podArgs(t, stringList(t, natsContainer["args"]), map[string]string{"/etc/nats-config/": s.dir + "/"}),
			// port: 4222, the client port each pod has to itself.
			"-p", strconv.Itoa(s.client),
			// http_port: 8222, the monitoring port.
			"-m", strconv.Itoa(s.monitor),
			// cluster.port: 6222, the route port.
			"--cluster", fmt.Sprintf("nats://127.0.0.1:%d", s.route),
			// cluster.routes, the three pods' DNS names in the headless Service: here the other two
			// servers' route ports.
			"--routes", strings.Join(routes, ","),
			// pid_file: /var/run/nats/nats.pid, the emptyDir the pod shares with the reloader.
			"-P", filepath.Join(s.dir, "nats.pid"),
		)
		// server_name: $SERVER_NAME, which the container sets to its pod's name.
		s.env = []string{"SERVER_NAME=" + s.name}
		s.start(t)
		t.Logf("%s: nats-server %s", s.name, strings.Join(s.args, " "))
	}
	urls := make([]string, len(servers))
	for i, s := range servers {
		urls[i] = s.url()
	}
	byName := map[string]*hubServer{}
	for _, s := range servers {
		byName[s.name] = s
	}

	clusterName := regexp.MustCompile(`"cluster":\{"name":"([^"]+)"`).FindStringSubmatch(strings.Join(strings.Fields(r.natsConf), ""))
	require.Len(t, clusterName, 2, "the rendered nats.conf names its cluster")

	// (a) One cluster with a JetStream meta leader, serving the hub's users.
	var metaLeader string
	formed := within(t, 60*time.Second, "the three servers form one cluster with a meta leader", func() error {
		leaders := map[string]bool{}
		for _, s := range servers {
			var varz struct {
				ServerName string `json:"server_name"`
				Cluster    struct {
					Name string `json:"name"`
				} `json:"cluster"`
			}
			if err := s.monitorJSON("/varz", &varz); err != nil {
				return err
			}
			if varz.ServerName != s.name || varz.Cluster.Name != clusterName[1] {
				return fmt.Errorf("%s's /varz names server %q in cluster %q", s.name, varz.ServerName, varz.Cluster.Name)
			}
			var routez struct {
				Routes []struct {
					RemoteName string `json:"remote_name"`
				} `json:"routes"`
			}
			if err := s.monitorJSON("/routez", &routez); err != nil {
				return err
			}
			remotes := map[string]bool{}
			for _, route := range routez.Routes {
				remotes[route.RemoteName] = true
			}
			if len(remotes) != 2 || remotes[s.name] {
				return fmt.Errorf("%s routes to %v", s.name, remotes)
			}
			var jsz struct {
				Meta struct {
					Leader string `json:"leader"`
					Size   int    `json:"cluster_size"`
				} `json:"meta_cluster"`
			}
			if err := s.monitorJSON("/jsz", &jsz); err != nil {
				return err
			}
			if jsz.Meta.Leader == "" || jsz.Meta.Size != 3 {
				return fmt.Errorf("%s's meta cluster: leader %q, size %d", s.name, jsz.Meta.Leader, jsz.Meta.Size)
			}
			leaders[jsz.Meta.Leader] = true
			metaLeader = jsz.Meta.Leader
		}
		if len(leaders) != 1 {
			return fmt.Errorf("the servers name meta leaders %v", leaders)
		}
		return nil
	})
	t.Logf("(a) cluster %q (from the rendered nats.conf, as each /varz reports it) formed in %s: each server routes to the other two; JetStream meta leader %s",
		clusterName[1], formed.Round(time.Millisecond), metaLeader)

	for _, s := range servers {
		conn, err := testnats.ConnectSeed(t, s.url(), r.keys.hostSeed)
		require.NoError(t, err)
		require.Equal(t, r.keys.hostKey, s.authorizedUser(t, conn))
		conn.Close()
		t.Logf("(a) the keyed user live-host (core.HostPermissions) connected to %s as %s", s.name, r.keys.hostKey)
	}
	session := core.SessionPermissions().Publish.Allow
	allowed := strings.NewReplacer("*", "live", ">", "live").Replace(session[0])
	require.True(t, bus.SubjectContains(session[0], allowed))
	refused := "notifications.live"
	for _, pattern := range session {
		require.False(t, bus.SubjectContains(pattern, refused), "%s is outside core.SessionPermissions' publish grant", refused)
	}
	for _, s := range servers {
		errs := make(chan error, 16)
		conn, err := testnats.ConnectSeed(t, s.url(), "", natsgo.ErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) { errs <- err }))
		require.NoError(t, err)
		require.Equal(t, r.keys.sessionKey, s.authorizedUser(t, conn), "a client with no credential is the session user")
		require.NoError(t, conn.Publish(allowed, nil))
		require.NoError(t, conn.Publish(refused, nil))
		awaitViolation(t, conn, errs, fmt.Sprintf("Publish to %q", refused))
		conn.Close()
		t.Logf("(a) a client with no credential on %s became the session user %s, published to %s and was refused a publish to %s",
			s.name, r.keys.sessionKey, allowed, refused)
	}

	// (b) A stream at three copies across a SIGKILL of its leader.
	const r3, messages = "LIVE_R3", 1000
	operator := connectHub(t, urls, r.keys.operatorSeed)
	js, err := operator.JetStream(natsgo.MaxWait(2 * time.Second))
	require.NoError(t, err)
	within(t, 30*time.Second, "the R3 stream is created", func() error {
		_, err := js.AddStream(&natsgo.StreamConfig{Name: r3, Subjects: []string{"live.r3"}, Replicas: 3, Storage: natsgo.FileStorage})
		return err
	})
	// The leader is polled with a short wait, so a request left unanswered while the stream has no
	// leader delays the election's measure by at most 500 ms.
	poll, err := operator.JetStream(natsgo.MaxWait(500 * time.Millisecond))
	require.NoError(t, err)
	streamLeader := func() (string, error) {
		info, err := poll.StreamInfo(r3)
		if err != nil {
			return "", err
		}
		if info.Cluster == nil || info.Cluster.Leader == "" {
			return "", errors.New("the stream has no leader")
		}
		return info.Cluster.Leader, nil
	}
	awaitCurrent := func(what string) time.Duration {
		return within(t, 30*time.Second, what, func() error {
			info, err := js.StreamInfo(r3)
			if err != nil {
				return err
			}
			if info.Cluster == nil || info.Cluster.Leader == "" || len(info.Cluster.Replicas) != 2 {
				return fmt.Errorf("cluster %+v", info.Cluster)
			}
			for _, p := range info.Cluster.Replicas {
				if !p.Current || p.Offline || p.Lag != 0 {
					return fmt.Errorf("the copy on %s: current %t, offline %t, lag %d", p.Name, p.Current, p.Offline, p.Lag)
				}
			}
			for _, s := range servers {
				last, err := s.streamLastSeq(r3)
				if err != nil {
					return err
				}
				if last != info.State.LastSeq {
					return fmt.Errorf("%s's copy ends at %d, the stream at %d", s.name, last, info.State.LastSeq)
				}
			}
			return nil
		})
	}

	publisher := connectHub(t, urls, r.keys.operatorSeed)
	pubJS, err := publisher.JetStream()
	require.NoError(t, err)
	var acked, retries atomic.Int64
	published := make(chan error, 1)
	go func() {
		for i := 1; i <= messages; i++ {
			id := strconv.Itoa(i)
			deadline := time.Now().Add(30 * time.Second)
			for {
				msg := natsgo.NewMsg("live.r3")
				msg.Data = []byte(id)
				_, err := pubJS.PublishMsg(msg, natsgo.MsgId(id), natsgo.AckWait(2*time.Second))
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					published <- fmt.Errorf("message %s unacknowledged for 30s: %w", id, err)
					return
				}
				retries.Add(1)
				time.Sleep(50 * time.Millisecond)
			}
			acked.Store(int64(i))
			time.Sleep(5 * time.Millisecond)
		}
		published <- nil
	}()
	within(t, 30*time.Second, "the publisher's first 200 acknowledgements", func() error {
		if acked.Load() < 200 {
			return fmt.Errorf("%d acknowledged", acked.Load())
		}
		return nil
	})
	leader, err := streamLeader()
	require.NoError(t, err)
	killed := byName[leader]
	require.NotNil(t, killed, "the stream leader %s is one of the servers", leader)
	killed.kill(t)
	atKill := acked.Load()
	require.Less(t, atKill, int64(messages), "the leader dies mid-run")
	var newLeader string
	// nats-server v2.15.1's Raft waits a random 4-9 s without the leader's heartbeat before a
	// follower campaigns (server/raft.go, minElectionTimeoutDefault and maxElectionTimeoutDefault),
	// so 15 s holds the slowest draw and its vote with room for a loaded runner.
	elected := within(t, 15*time.Second, "a new stream leader after the SIGKILL", func() error {
		l, err := streamLeader()
		if err != nil {
			return err
		}
		if l == leader {
			return fmt.Errorf("the leader is still %s", l)
		}
		newLeader = l
		return nil
	})
	t.Logf("(b) SIGKILL of the stream leader %s (pid %d) after %d of %d messages acknowledged; %s leads %s %s later",
		leader, killed.proc.Cmd.Process.Pid, atKill, messages, newLeader, r3, elected.Round(time.Millisecond))
	select {
	case err := <-published:
		require.NoError(t, err)
	case <-time.After(2 * time.Minute):
		t.Fatalf("the publisher did not finish within 2m: %d acknowledged", acked.Load())
	}
	info, err := js.StreamInfo(r3)
	require.NoError(t, err)
	require.EqualValues(t, messages, info.State.Msgs)
	ids := map[string]int{}
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq; seq++ {
		msg, err := js.GetMsg(r3, seq)
		require.NoError(t, err)
		ids[msg.Header.Get(natsgo.MsgIdHdr)]++
	}
	for i := 1; i <= messages; i++ {
		require.Equal(t, 1, ids[strconv.Itoa(i)], "message %d is in the stream exactly once", i)
	}
	require.Len(t, ids, messages, "the stream holds no message that was not published")
	t.Logf("(b) the stream holds each of the %d acknowledged messages exactly once (sequences %d-%d; %d publish attempts retried)",
		messages, info.State.FirstSeq, info.State.LastSeq, retries.Load())
	killed.start(t)
	caughtUp := awaitCurrent("all three copies current after the killed server restarts on its store")
	t.Logf("(b) %s restarted on its store (pid %d); all three copies current at sequence %d %s later",
		killed.name, killed.proc.Cmd.Process.Pid, info.State.LastSeq, caughtUp.Round(time.Millisecond))

	// (c) The exporter, with the chart's arguments, against each server.
	exporterArgs := stringList(t, r.container(t, "prom-exporter")["args"])
	exporterPorts := make([]int, len(servers))
	for i, s := range servers {
		var args []string
		startBound(t, s.name+" exporter", func() *exec.Cmd {
			exporterPorts[i] = cmdtest.FreeTCPPort(t)
			// The chart's arguments with the exporter's port, -port=7777, and the monitoring port
			// of http://localhost:8222/, both fixed by the pod, replaced by this server's.
			args = podArgs(t, exporterArgs, map[string]string{
				"-port=7777":     fmt.Sprintf("-port=%d", exporterPorts[i]),
				"localhost:8222": fmt.Sprintf("localhost:%d", s.monitor),
			})
			return exec.Command(exporter, args...)
		}, func(*cmdtest.Process) error {
			_, err := scrape(exporterPorts[i])
			return err
		})
		t.Logf("%s: prometheus-nats-exporter %s", s.name, strings.Join(args, " "))
	}
	atRest := awaitLastSeqs(t, exporterPorts, r3, 3, float64(messages))
	series, err := streamSeries(exporterPorts, r3)
	require.NoError(t, err)
	var metricNames []string
	for _, s := range series {
		if !slices.Contains(metricNames, s.name) {
			metricNames = append(metricNames, s.name)
		}
	}
	sort.Strings(metricNames)
	t.Logf("(c) the exporters' series of %s: %s", r3, strings.Join(metricNames, ", "))
	t.Logf("(c) %s at rest: %d %s series, equal: %v", r3, len(atRest), lastSeqMetric, atRest)

	leader, err = streamLeader()
	require.NoError(t, err)
	var down *hubServer
	for _, s := range servers {
		if s.name != leader {
			down = s
			break
		}
	}
	down.kill(t)
	whileDown := awaitLastSeqs(t, exporterPorts, r3, 2, float64(messages))
	require.NotContains(t, whileDown, down.name)
	t.Logf("(c) while %s is down: %d %s series of %s: %v", down.name, len(whileDown), lastSeqMetric, r3, whileDown)
	down.start(t)
	awaitCurrent("all three copies current after " + down.name + " restarts")
	awaitLastSeqs(t, exporterPorts, r3, 3, float64(messages))

	const r1 = "LIVE_R1"
	within(t, 30*time.Second, "the R1 stream is created", func() error {
		_, err := js.AddStream(&natsgo.StreamConfig{Name: r1, Subjects: []string{"live.r1"}, Replicas: 1, Storage: natsgo.FileStorage})
		return err
	})
	for i := range 5 {
		_, err := js.Publish("live.r1", []byte(strconv.Itoa(i)))
		require.NoError(t, err)
	}
	single := awaitLastSeqs(t, exporterPorts, r1, 1, 5)
	t.Logf("(c) the R1 stream %s: %d %s series: %v", r1, len(single), lastSeqMetric, single)

	// (d) The reloader, with the chart's arguments, beside each server: a users change applies
	// with no restart and no reconnect.
	reloaderArgs := stringList(t, r.container(t, "reloader")["args"])
	pids := make([]int, len(servers))
	for i, s := range servers {
		pids[i] = s.proc.Cmd.Process.Pid
		// The chart's arguments with the pid file's emptyDir, /var/run/nats/, and the
		// ConfigMap's mount, /etc/nats-config/, replaced by the server's directory.
		args := podArgs(t, reloaderArgs, map[string]string{"/var/run/nats/": s.dir + "/", "/etc/nats-config/": s.dir + "/"})
		p := startLogged(t, s.name+" reloader", exec.Command(reloader, args...))
		p.WaitForOutput(t, fmt.Sprintf("Live, ready to kick pid %d", pids[i]))
		t.Logf("%s: nats-server-config-reloader %s, watching pid %d", s.name, strings.Join(args, " "), pids[i])
	}
	var disconnects atomic.Int64
	throughout := connectHub(t, urls[:1], r.keys.operatorSeed,
		natsgo.DisconnectErrHandler(func(*natsgo.Conn, error) { disconnects.Add(1) }))
	serverID := throughout.ConnectedServerId()
	cid, err := throughout.GetClientID()
	require.NoError(t, err)
	t.Logf("(d) a client connected throughout: server_id %s, cid %d", serverID, cid)

	reloaded := "Reloaded server configuration"
	writeAll := func(authConf string) []int {
		before := make([]int, len(servers))
		for i, s := range servers {
			before[i] = s.count(reloaded)
			writeAtomically(t, filepath.Join(s.dir, clusteredHubAuthConfKey), authConf)
		}
		return before
	}
	addedSeed, addedKey := testnats.User(t)
	withAdded := liveHubArgs(r.keys)
	withAdded.Users = append(withAdded.Users, gcp.NATSUserArgs{Name: "live-added", NKey: addedKey, Permissions: &gcp.PermissionsArgs{
		Publish: nats.RulesArgs{Allow: []string{"live.added.>"}}, Subscribe: nats.RulesArgs{Allow: []string{"_INBOX.>"}},
	}})
	before := writeAll(liveAuthConf(t, withAdded))
	for i, s := range servers {
		took := within(t, 30*time.Second, "the added user connects to "+s.name, func() error {
			if s.count(reloaded) == before[i] {
				return errors.New("no reload yet")
			}
			conn, err := testnats.ConnectSeed(t, s.url(), addedSeed)
			if err != nil {
				return err
			}
			conn.Close()
			return nil
		})
		t.Logf("(d) %s reloaded the auth.conf adding live-added (%q in its log) and admits it, %s after the rename",
			s.name, reloaded, took.Round(time.Millisecond))
	}
	before = writeAll(r.authConf)
	for i, s := range servers {
		took := within(t, 30*time.Second, "the removed user is refused by "+s.name, func() error {
			if s.count(reloaded) == before[i] {
				return errors.New("no reload yet")
			}
			conn, err := testnats.ConnectSeed(t, s.url(), addedSeed)
			if err == nil {
				conn.Close()
				return errors.New("still admitted")
			}
			if !errors.Is(err, natsgo.ErrAuthorization) {
				return err
			}
			return nil
		})
		t.Logf("(d) %s reloaded the auth.conf without live-added and refuses it (authorization violation), %s after the rename",
			s.name, took.Round(time.Millisecond))
	}
	require.NoError(t, throughout.Flush())
	require.True(t, throughout.IsConnected())
	require.Equal(t, serverID, throughout.ConnectedServerId())
	stillCID, err := throughout.GetClientID()
	require.NoError(t, err)
	require.Equal(t, cid, stillCID)
	require.Zero(t, throughout.Stats().Reconnects)
	require.Zero(t, disconnects.Load())
	for i, s := range servers {
		require.Equal(t, pids[i], s.proc.Cmd.Process.Pid)
		select {
		case <-s.proc.Exited:
			t.Fatalf("%s exited", s.name)
		default:
		}
		pidFile, err := os.ReadFile(filepath.Join(s.dir, "nats.pid"))
		require.NoError(t, err)
		require.Equal(t, strconv.Itoa(pids[i]), string(pidFile))
	}
	t.Logf("(d) through both changes the client kept server_id %s and cid %d with 0 reconnects and 0 disconnects; server pids unchanged: %v",
		serverID, cid, pids)

	// (e) A session-user change: every server's reload refuses it.
	_, newSessionKey := testnats.User(t)
	withNewSession := liveHubArgs(r.keys)
	withNewSession.SessionUser.NKey = newSessionKey
	refusal := "config reload not supported for NoAuthUser"
	refusals := make([]int, len(servers))
	for i, s := range servers {
		refusals[i] = s.count(refusal)
	}
	before = writeAll(liveAuthConf(t, withNewSession))
	for i, s := range servers {
		within(t, 30*time.Second, s.name+" refuses the session-user change", func() error {
			if s.count(refusal) == refusals[i] {
				return errors.New("no refusal yet")
			}
			return nil
		})
		var line string
		for _, l := range strings.Split(s.proc.Output.String(), "\n") {
			if strings.Contains(l, refusal) {
				line = l
			}
		}
		require.Contains(t, line, fmt.Sprintf("old=%s, new=%s", r.keys.sessionKey, newSessionKey))
		require.Equal(t, before[i], s.count(reloaded), "%s applied nothing", s.name)
		t.Logf("(e) %s refused the reload; its log: %s", s.name, strings.TrimSpace(line))

		conn, err := testnats.ConnectSeed(t, s.url(), "")
		require.NoError(t, err)
		require.Equal(t, r.keys.sessionKey, s.authorizedUser(t, conn), "the previous session user stays in force")
		conn.Close()
		conn, err = testnats.ConnectSeed(t, s.url(), r.keys.hostSeed)
		require.NoError(t, err)
		conn.Close()
		t.Logf("(e) %s still makes a client with no credential the previous session user %s and admits live-host", s.name, r.keys.sessionKey)
	}
	restarted := servers[0]
	restarted.proc.Terminate(t)
	restarted.proc.WaitExit(t, "SIGTERM")
	restarted.start(t)
	var user string
	within(t, 30*time.Second, restarted.name+" admits a client with no credential after its restart", func() error {
		conn, err := testnats.ConnectSeed(t, restarted.url(), "")
		if err != nil {
			return err
		}
		defer conn.Close()
		user = restarted.authorizedUser(t, conn)
		return nil
	})
	require.Equal(t, newSessionKey, user)
	t.Logf("(e) %s restarted (pid %d) makes a client with no credential the new session user %s", restarted.name, restarted.proc.Cmd.Process.Pid, user)
}
