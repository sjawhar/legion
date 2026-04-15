//go:build smoke

// Package smoke contains container-level smoke tests for the Envoy listener.
// Builds the Docker image from the local Dockerfile and verifies critical
// endpoints work end-to-end with a real NATS server via testcontainers.
//
// Run: go test -tags smoke -v -timeout 5m ./internal/smoke/
package smoke

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestSmoke(t *testing.T) {
	ctx := context.Background()

	// Shared Docker network for container-to-container communication.
	net, err := network.New(ctx)
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	t.Cleanup(func() { net.Remove(ctx) })

	// Start NATS using the same module the rest of the project uses.
	natsC, err := tcnats.Run(ctx, "nats:2.10",
		network.WithNetwork([]string{"nats"}, net),
	)
	testcontainers.CleanupContainer(t, natsC)
	if err != nil {
		t.Fatalf("start NATS: %v", err)
	}

	listenerC, err := testcontainers.Run(ctx, "",
		testcontainers.WithDockerfile(testcontainers.FromDockerfile{
			Context:       "../../",
			Dockerfile:    "docker/Dockerfile",
			PrintBuildLog: true,
			KeepImage:     true,
		}),
		network.WithNetwork([]string{"listener"}, net),
		testcontainers.WithEnv(map[string]string{
			"NATS_URLS":        "nats://nats:4222",
			"ENVOY_MACHINE_ID": "smoke-test",
			"PORT":             "9020",
		}),
		testcontainers.WithExposedPorts("9020/tcp"),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/healthz").
				WithPort("9020/tcp").
				WithStatusCodeMatcher(func(status int) bool {
					return status == http.StatusOK
				}).
				WithStartupTimeout(60*time.Second),
		),
	)
	testcontainers.CleanupContainer(t, listenerC)
	if err != nil {
		t.Fatalf("start listener: %v", err)
	}

	host, err := listenerC.Host(ctx)
	if err != nil {
		t.Fatalf("get host: %v", err)
	}
	port, err := listenerC.MappedPort(ctx, "9020")
	if err != nil {
		t.Fatalf("get mapped port: %v", err)
	}
	baseURL := fmt.Sprintf("http://%s:%s", host, port.Port())

	t.Run("healthz", func(t *testing.T) {
		resp, err := http.Get(baseURL + "/healthz")
		if err != nil {
			t.Fatalf("GET /healthz: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /healthz: got %d, want %d", resp.StatusCode, http.StatusOK)
		}

		var body map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode /healthz: %v", err)
		}

		status, ok := body["status"].(string)
		if !ok {
			t.Fatal("/healthz missing 'status' field")
		}
		if status != "healthy" && status != "starting" {
			t.Errorf("/healthz status=%q, want 'healthy' or 'starting'", status)
		}
	})

	t.Run("metrics", func(t *testing.T) {
		resp, err := http.Get(baseURL + "/metrics")
		if err != nil {
			t.Fatalf("GET /metrics: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /metrics: got %d, want %d", resp.StatusCode, http.StatusOK)
		}

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read /metrics: %v", err)
		}
		body := string(bodyBytes)

		for _, metric := range []string{
			"envoy_messages_received_total",
			"envoy_active_sessions",
			"envoy_active_interests",
		} {
			if !strings.Contains(body, metric) {
				t.Errorf("/metrics missing %q", metric)
			}
		}
	})

	t.Run("sessions", func(t *testing.T) {
		resp, err := http.Get(baseURL + "/v1/sessions")
		if err != nil {
			t.Fatalf("GET /v1/sessions: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /v1/sessions: got %d, want %d", resp.StatusCode, http.StatusOK)
		}
	})

	t.Run("subscribe_and_verify", func(t *testing.T) {
		subBody := `{"session_id":"ses_smoke_test","topics":["notifications.test.smoke"],"port":8080}`
		resp, err := http.Post(
			baseURL+"/v1/interests/subscribe",
			"application/json",
			strings.NewReader(subBody),
		)
		if err != nil {
			t.Fatalf("POST /v1/interests/subscribe: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("POST /v1/interests/subscribe: got %d, want %d", resp.StatusCode, http.StatusOK)
		}

		sessResp, err := http.Get(baseURL + "/v1/sessions")
		if err != nil {
			t.Fatalf("GET /v1/sessions: %v", err)
		}
		defer sessResp.Body.Close()

		bodyBytes, err := io.ReadAll(sessResp.Body)
		if err != nil {
			t.Fatalf("read /v1/sessions: %v", err)
		}

		if !strings.Contains(string(bodyBytes), "ses_smoke_test") {
			t.Error("registered session not visible in /v1/sessions")
		}
	})
}
