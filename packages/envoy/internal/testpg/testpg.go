// Package testpg starts a throwaway Postgres container for a test that brings its own servers, as a
// natslive test does with testnats, and needs the URL to hand a binary. A test of Dispatch's store
// against a server CI provides uses internal/dispatch/store/storetest instead.
package testpg

import (
	"context"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image is the pgvector server image CI uses for Dispatch's Postgres service. The envoy-infra-go
// job pulls it before the natslive tests run, so a test does not fetch it in the middle of a lane.
const Image = "mirror.gcr.io/pgvector/pgvector@sha256:7b822b0aac60967beb1ea5e576b8602c94c300a157d187f385ae3e0da199b90a"

// URL starts Image, waits for Postgres to finish its first initialization and be ready again, and
// returns its postgres database's loopback connection URL. The test container ends with t.
func URL(t testing.TB) string {
	t.Helper()
	ctx := context.Background()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        Image,
			ExposedPorts: []string{"5432/tcp"},
			Env:          map[string]string{"POSTGRES_PASSWORD": "test"},
			WaitingFor:   wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, container)
	if err != nil {
		t.Fatalf("start Postgres: %v", err)
	}
	endpoint, err := container.PortEndpoint(ctx, "5432/tcp", "")
	if err != nil {
		t.Fatalf("Postgres endpoint: %v", err)
	}
	return "postgres://postgres:test@" + endpoint + "/postgres?sslmode=disable"
}
