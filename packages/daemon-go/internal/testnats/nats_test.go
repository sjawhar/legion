package testnats

import (
	"crypto/rand"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// A start that fails after Docker created the container — here a readiness wait that can never
// succeed, as a start timing out under load does — leaves no container behind once the test ends:
// the container is the test's to remove from the moment it exists, whatever the start returned.
// The container is asked for by its label on the same Docker daemon testcontainers created it on.
func TestAFailedStartLeavesNoContainer(t *testing.T) {
	label := "legion-testnats-" + rand.Text()
	t.Run("start", func(t *testing.T) {
		created, err := start(t,
			testcontainers.WithLabels(map[string]string{"legion.test": label}),
			testcontainers.WithWaitStrategy(wait.ForLog("a line NATS never prints").WithStartupTimeout(3*time.Second)),
		)
		if err == nil {
			t.Fatal("a start whose readiness wait cannot succeed returned no error")
		}
		if created == nil {
			t.Fatalf("the failed start created no container (%v), so there was nothing to leak", err)
		}
	})
	docker, err := testcontainers.NewDockerClientWithOpts(t.Context())
	if err != nil {
		t.Fatalf("the Docker client testcontainers uses: %v", err)
	}
	defer docker.Close()
	left, err := docker.ContainerList(t.Context(), container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", "legion.test="+label))})
	if err != nil {
		t.Fatalf("list containers labelled %s: %v", label, err)
	}
	for _, c := range left {
		t.Errorf("container %s of the failed start is still there (%s)", c.ID, c.State)
		_ = docker.ContainerRemove(t.Context(), c.ID, container.RemoveOptions{Force: true})
	}
}
