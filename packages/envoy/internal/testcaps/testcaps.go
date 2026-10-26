// Package testcaps caps the CPU and memory of a container a test starts. Without a cap a test's
// server can take every core and all the memory of its host, which a devbox shares with every
// other session's work. Every testcontainers start in this module passes one: Cap's result as the
// request's HostConfigModifier, or through testcontainers.WithHostConfigModifier.
//
// The package imports no container runtime, only Docker's host configuration type, so it is not
// one of the container rigs internal/testnats's import guard allows.
package testcaps

import "github.com/docker/docker/api/types/container"

// Cap returns a host configuration modifier that holds a container to cpus cores and memory
// bytes. Swap is held to the memory cap, so the container cannot grow past it into swap.
func Cap(cpus float64, memory int64) func(*container.HostConfig) {
	return func(hostConfig *container.HostConfig) {
		hostConfig.NanoCPUs = int64(cpus * 1e9)
		hostConfig.Memory = memory
		hostConfig.MemorySwap = memory
	}
}
