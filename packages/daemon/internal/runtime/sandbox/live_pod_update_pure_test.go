// The Stage 4a update-round check's pure logic. Neither function touches a cluster, so, unlike
// live_pod_update_test.go's own `//go:build e2e`, this file carries no build tag: ordinary
// `go test ./...` compiles and exercises it directly (live_pod_update_unit_test.go).
package sandbox

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
)

// podUpdateRunIssue is pod-update's issue key in the run whose project is project, seeded by that
// project and this check's own name (runIssue), so it is neither cache-b's key
// (cacheRunIssue) nor another run's. Both workers of the issue push its branch, legion/<issue>,
// onto the run's repository, which every Stage 4a run shares, so each run only ever creates and
// deletes its own; the key keeps cache-b's `legion/S4A-<digits>` shape, so a branch a killed run
// left is swept by cache-lost-volume's own sweep (staleRunBranches).
func podUpdateRunIssue(project string) string { return runIssue(project + " pod-update") }

// podUpdateFleetRunIssue is pod-update-fleet's second issue key in the run whose project is
// project, seeded as podUpdateRunIssue's is but by this check's own name, so the two issues the
// one template change drifts at once are two issues, each pushing a branch of its own onto the
// repository every Stage 4a run shares. It keeps the same `legion/S4A-<digits>` shape, so a
// branch a killed run left is swept by cache-lost-volume's own sweep (staleRunBranches).
func podUpdateFleetRunIssue(project string) string { return runIssue(project + " pod-update-fleet") }

// movedPodShape is the recorded shape with its disk moved one gibibyte up, and the one reason the
// runtime then names for the pod (podShapeDrift). The disk is what the harness moves because it is
// the operator's own value and nothing of the pod itself: the Sandbox keeps the pod it has, every
// process in it keeps running, and the only thing that has moved is the configuration the pod was
// built from — which is what a round is opened on.
func movedPodShape(held string) (shape, reason string, err error) {
	var record podShapeRecord
	if err := json.Unmarshal([]byte(held), &record); err != nil {
		return "", "", fmt.Errorf("the recorded shape %q cannot be read: %w", held, err)
	}
	disk, err := resource.ParseQuantity(record.Disk)
	if err != nil {
		return "", "", fmt.Errorf("the recorded disk %q: %w", record.Disk, err)
	}
	was := record.Disk
	disk.Add(resource.MustParse("1Gi"))
	record.Disk = disk.String()
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", "", err
	}
	return string(encoded), "disk " + record.Disk + ", now " + was, nil
}
