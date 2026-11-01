package config

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// An issue pod runs every workflow role in one container with one reservation, so a key naming a
// workflow role sizes nothing: each is refused, naming the one key that sizes the pod.
func TestAWorkflowRoleReservationKeyIsRefusedNamingTheIssuePodsKey(t *testing.T) {
	for _, role := range claim.Roles {
		t.Run(string(role), func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, kubernetesFile+"    resources: {"+string(role)+": {cpu: 1}}\n"), noEnv)
			want := "runtime.kubernetes.resources." + string(role) + " is gone: an issue pod has one reservation, runtime.kubernetes.resources.issue"
			if err == nil || err.Error() != want {
				t.Fatalf("LoadForValidation = %v, want %q", err, want)
			}
		})
	}
}

// A key that is neither the issue pod's nor the controller's is refused, naming the two.
func TestAnUnknownReservationKeyIsRefused(t *testing.T) {
	_, err := LoadForValidation(writeConfigFile(t, kubernetesFile+"    resources: {probe: {cpu: 1}}\n"), noEnv)
	if want := `runtime.kubernetes.resources key "probe" must be issue or controller`; err == nil || err.Error() != want {
		t.Fatalf("LoadForValidation = %v, want %q", err, want)
	}
}

// A file that sets no reservation keeps what an issue pod reserved before it ran in one container:
// the sum of the six role defaults, 3 CPU and 19Gi each requested in full, and an 80Gi disk limit
// over a 6Gi request. The controller's pod keeps its own default.
func TestTheIssuePodsDefaultReservationIsTheSumOfTheSixRolesItReplaced(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	want := PodResources{
		Issue:      Reservation{CPU: "3", CPURequest: "3", Memory: "19Gi", MemoryRequest: "19Gi", EphemeralStorage: "80Gi", EphemeralStorageRequest: "6Gi"},
		Controller: Reservation{CPU: "1", CPURequest: "1", Memory: "4Gi", MemoryRequest: "4Gi", EphemeralStorage: "10Gi", EphemeralStorageRequest: "1Gi"},
	}
	if got := cfg.Runtime.Kubernetes.Resources; got != want {
		t.Fatalf("resources = %+v, want %+v", got, want)
	}
	if got := DefaultResources(); got != want {
		t.Fatalf("DefaultResources() = %+v, want the settled block's %+v", got, want)
	}
}

// The issue pod's entry settles field by field over its default, as a role's did: a raised limit
// alone is requested in full, a lower request beside its limit is kept, and the disk bound's two
// sides settle apart. A request past its limit is refused naming both keys of the pod's entry.
func TestTheIssuePodsReservationSettlesFieldByField(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile+"    resources: {issue: {cpu: 4, memory: 32Gi, memory_request: 20Gi, ephemeral_storage: 10Gi, ephemeral_storage_request: 10Gi}}\n"), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	want := Reservation{CPU: "4", CPURequest: "4", Memory: "32Gi", MemoryRequest: "20Gi", EphemeralStorage: "10Gi", EphemeralStorageRequest: "10Gi"}
	if got := cfg.Runtime.Kubernetes.Resources.Issue; got != want {
		t.Errorf("resources.issue = %+v, want %+v", got, want)
	}
	if !cfg.Runtime.Kubernetes.Resources.Issue.Burstable() {
		t.Errorf("resources.issue %+v reads Guaranteed, want Burstable: its memory request is below its limit", want)
	}
	_, err = LoadForValidation(writeConfigFile(t, kubernetesFile+"    resources: {issue: {memory_request: 20Gi}}\n"), noEnv)
	if want := "runtime.kubernetes.resources.issue.memory_request 20Gi exceeds memory 19Gi"; err == nil || err.Error() != want {
		t.Fatalf("LoadForValidation = %v, want %q", err, want)
	}
	_, err = LoadForValidation(writeConfigFile(t, kubernetesFile+"    resources: {issue: {requests: {cpu: 1}}}\n"), noEnv)
	if err == nil || !strings.Contains(err.Error(), "runtime.kubernetes.resources.issue.requests is gone") {
		t.Fatalf("LoadForValidation = %v, want the earlier shape's key refused", err)
	}
}
