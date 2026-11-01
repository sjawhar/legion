package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// probe-boot names its configuration only by --config: an argument is refused as start refuses it.
func TestProbeBootRefusesAnArgument(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "probe-boot", "legion.yaml"}, &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), `unexpected argument "legion.yaml"`) {
		t.Fatalf("exit code = %d, stderr %q; want 2 naming the argument", code, errb.String())
	}
}

// probe-boot over a cluster without Agent Sandbox refuses as boot does, naming the CRD, after
// reading the configuration without running either App's private_key_command; it only reads the
// cluster, and prints no OK line.
func TestProbeBootRefusesAClusterWithoutAgentSandboxAndRunsNoKeyCommand(t *testing.T) {
	legionState(t)
	var mu sync.Mutex
	var requested []string
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requested = append(requested, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "NotFound", "code": 404})
	}))
	defer apiServer.Close()
	config := kubernetesCheckConfig(t, "")
	dir := filepath.Dir(config)
	marker := filepath.Join(dir, "private-key-command-ran")
	body, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	body = bytes.ReplaceAll(body, []byte(`private_key_command: "exit 1"`), []byte(`private_key_command: "touch `+marker+`; exit 1"`))
	if err := os.WriteFile(config, body, 0o600); err != nil {
		t.Fatal(err)
	}
	kubeconfig := "apiVersion: v1\nkind: Config\n" +
		"clusters: [{name: example, cluster: {server: \"" + apiServer.URL + "\"}}]\n" +
		"users: [{name: legion-daemon, user: {token: placeholder}}]\n" +
		"contexts: [{name: legion-daemon, context: {cluster: example, user: legion-daemon, namespace: legion}}]\n" +
		"current-context: legion-daemon\n"
	if err := os.WriteFile(filepath.Join(dir, "kubeconfig"), []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "probe-boot", "--config", config}, &out, &errb)

	if code != 1 || out.Len() != 0 || !strings.HasPrefix(errb.String(), "legion probe-boot: ") || !strings.Contains(errb.String(), "sandboxes.agents.x-k8s.io") {
		t.Fatalf("exit code = %d, stdout %q, stderr %q; want 1, no OK line, and a refusal naming the Sandbox CRD", code, out.String(), errb.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("a private_key_command ran (marker stat: %v)", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requested) == 0 {
		t.Fatal("probe-boot asked the cluster nothing")
	}
	for _, request := range requested {
		if !strings.HasPrefix(request, "GET ") {
			t.Errorf("probe-boot sent %s; its cluster check may only read", request)
		}
	}
}
