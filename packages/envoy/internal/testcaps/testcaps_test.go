package testcaps

import (
	"testing"

	"github.com/docker/docker/api/types/container"
)

// Cap sets the CPU cap in Docker's nano-CPUs, the memory cap in bytes, and swap equal to memory,
// and leaves the rest of the host configuration as it was.
func TestCapSetsCPUMemoryAndSwap(t *testing.T) {
	hostConfig := container.HostConfig{NetworkMode: "host"}
	Cap(1.5, 512<<20)(&hostConfig)
	if hostConfig.NanoCPUs != 1_500_000_000 || hostConfig.Memory != 512<<20 || hostConfig.MemorySwap != 512<<20 {
		t.Errorf("caps: NanoCPUs %d, Memory %d, MemorySwap %d; want 1500000000, %d, %d",
			hostConfig.NanoCPUs, hostConfig.Memory, hostConfig.MemorySwap, 512<<20, 512<<20)
	}
	if hostConfig.NetworkMode != "host" {
		t.Errorf("NetworkMode %q, want the caller's %q kept", hostConfig.NetworkMode, "host")
	}
}
