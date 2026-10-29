package gcp

import (
	"math/big"
	"strings"

	"github.com/sjawhar/envoy/infra/core"
)

// autopilotPod is a pod Autopilot sizes: its main container, whose request and limit the inputs
// cpuInput and memoryInput set (cpu and memory, nil when unset for defaultCPU and defaultMemory),
// and the sidecars beside it at fixed sizes. name is the pod as a refusal names it, such as "the
// hub pod", and memoryExamples the quantities a refusal of memoryInput suggests.
type autopilotPod struct {
	name                      string
	cpuInput, memoryInput     string
	cpu, memory               *string
	defaultCPU, defaultMemory string
	memoryExamples            string
	sidecars                  []autopilotSidecar
}

// autopilotSidecar is a sidecar's fixed size, request and limit, and its name as a refusal writes
// it before 's, such as "the reloader".
type autopilotSidecar struct{ name, cpu, memory string }

// clusteredHubPodMinCPU is the least CPU Autopilot runs one of the clustered hub's pods at without
// raising its request: 250m, the general-purpose compute class's minimum for a pod with neither pod
// anti-affinity nor the safe-to-evict annotation, without bursting
// (https://cloud.google.com/kubernetes-engine/docs/concepts/autopilot-resource-requests,
// "Minimums and maximums for compute classes"). The hub's pods and its tailnet hosts carry
// neither: they spread by topology spread constraints alone. gcp:Dispatch's pods carry pod
// anti-affinity, so its floor is autopilotPodMinCPU, 500m. The memory floor, 512Mi, is the same for
// both shapes (autopilotPodMinMemory). A cluster with bursting runs pods as small as 50m and 52Mi,
// so these floors hold on a cluster with or without it.
var clusteredHubPodMinCPU = big.NewRat(1, 4)

// autopilotPodSize resolves pod's main container's CPU and memory (appQuantity's rules) and
// refuses a size at which Autopilot would not run the pod as rendered: the pod's total, with its
// sidecars', below clusteredHubPodMinCPU or autopilotPodMinMemory or outside the memory-per-CPU
// ratio (Autopilot raises the smaller of the two, so the refusal names that one's input), or above
// autopilotPodMaxCPU or autopilotPodMaxMemory (Autopilot rejects the pod). Each refusal is a
// core.InputError naming cpuInput or memoryInput, and quotes the sidecars' sizes it counted.
func autopilotPodSize(pod autopilotPod) (cpu, memory string, err error) {
	cpu, podCPU, err := appQuantity(pod.cpuInput, pod.cpu, pod.defaultCPU, "1, 1500m or 2")
	if err != nil {
		return "", "", &core.InputError{Input: pod.cpuInput, Err: err}
	}
	memory, podMemory, err := appQuantity(pod.memoryInput, pod.memory, pod.defaultMemory, pod.memoryExamples)
	if err != nil {
		return "", "", &core.InputError{Input: pod.memoryInput, Err: err}
	}
	sidecarCPU, sidecarMemory := new(big.Rat), new(big.Rat)
	for _, sidecar := range pod.sidecars {
		value, _ := quantityValue(sidecar.cpu)
		sidecarCPU.Add(sidecarCPU, value)
		value, _ = quantityValue(sidecar.memory)
		sidecarMemory.Add(sidecarMemory, value)
	}
	podCPU = new(big.Rat).Add(podCPU, sidecarCPU)
	podMemory = new(big.Rat).Add(podMemory, sidecarMemory)
	// with names each sidecar's size, as size writes it, for a refusal.
	with := func(size func(autopilotSidecar) string) string {
		sizes := make([]string, len(pod.sidecars))
		for i, sidecar := range pod.sidecars {
			sizes[i] = sidecar.name + "'s " + size(sidecar)
		}
		return "with " + strings.Join(sizes, " and ")
	}
	cpuOf := func(s autopilotSidecar) string { return s.cpu }
	memoryOf := func(s autopilotSidecar) string { return s.memory }
	switch {
	case podCPU.Cmp(clusteredHubPodMinCPU) < 0:
		return "", "", core.InputErrorf(pod.cpuInput, "%s %q must be at least %s: %s %s would request less than %s, below which Autopilot raises a pod with neither pod anti-affinity nor the safe-to-evict annotation",
			pod.cpuInput, cpu, milliCPU(new(big.Rat).Sub(clusteredHubPodMinCPU, sidecarCPU)), with(cpuOf), pod.name, milliCPU(clusteredHubPodMinCPU))
	case podCPU.Cmp(autopilotPodMaxCPU) > 0:
		return "", "", core.InputErrorf(pod.cpuInput, "%s %q must be at most %s: %s %s would request above %s CPU, and Autopilot rejects such a pod",
			pod.cpuInput, cpu, milliCPU(new(big.Rat).Sub(autopilotPodMaxCPU, sidecarCPU)), with(cpuOf), pod.name, autopilotPodMaxCPU.RatString())
	case podMemory.Cmp(autopilotPodMinMemory) < 0:
		return "", "", core.InputErrorf(pod.memoryInput, "%s %q must be at least %s: %s %s would request less than %s, below which Autopilot raises it",
			pod.memoryInput, memory, mebibytes(new(big.Rat).Sub(autopilotPodMinMemory, sidecarMemory)), with(memoryOf), pod.name, mebibytes(autopilotPodMinMemory))
	case podMemory.Cmp(autopilotPodMaxMemory) > 0:
		return "", "", core.InputErrorf(pod.memoryInput, "%s %q must be at most %s: %s %s would request above 110Gi, and Autopilot rejects such a pod",
			pod.memoryInput, memory, mebibytes(new(big.Rat).Sub(autopilotPodMaxMemory, sidecarMemory)), with(memoryOf), pod.name)
	}
	gibPerCPU := new(big.Rat).Quo(podMemory, podCPU)
	gibPerCPU.Quo(gibPerCPU, gibibyte)
	if gibPerCPU.Cmp(autopilotMinGiBPerCPU) >= 0 && gibPerCPU.Cmp(autopilotMaxGiBPerCPU) <= 0 {
		return cpu, memory, nil
	}
	smaller := pod.cpuInput // too much memory per CPU: Autopilot raises the CPU
	if gibPerCPU.Cmp(autopilotMinGiBPerCPU) < 0 {
		smaller = pod.memoryInput
	}
	return "", "", core.InputErrorf(smaller, "%s %q and %s %q: %s %s must hold between 1 and 6.5 GiB of memory per CPU, or Autopilot raises the smaller of the two",
		pod.cpuInput, cpu, pod.memoryInput, memory,
		with(func(s autopilotSidecar) string { return s.cpu + " and " + s.memory }), pod.name)
}
