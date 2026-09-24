package testnats

import (
	"crypto/rand"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// A start that fails after Docker created the container — here a readiness wait that can never
// succeed, as a start timing out under load does — leaves no container behind once the test ends:
// the container is the test's to remove from the moment it exists, whatever the start returned.
func TestAFailedStartLeavesNoContainer(t *testing.T) {
	label := "legion-testnats-" + rand.Text()
	t.Run("start", func(t *testing.T) {
		_, err := start(t,
			testcontainers.WithLabels(map[string]string{"legion.test": label}),
			testcontainers.WithWaitStrategy(wait.ForLog("a line NATS never prints").WithStartupTimeout(3*time.Second)),
		)
		if err == nil {
			t.Fatal("a start whose readiness wait cannot succeed returned no error")
		}
	})
	out, err := exec.Command("docker", "ps", "--all", "--quiet", "--filter", "label=legion.test="+label).CombinedOutput()
	if err != nil {
		t.Fatalf("docker ps: %v: %s", err, out)
	}
	if left := strings.Fields(string(out)); len(left) != 0 {
		t.Errorf("containers %v of the failed start are still there", left)
		_ = exec.Command("docker", append([]string{"rm", "--force"}, left...)...).Run()
	}
}
