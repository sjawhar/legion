//go:build !linux

package main

import (
	"fmt"
	"os"
	"runtime"
)

// main refuses: the abstract unix socket the test serves exists only in a Linux network namespace,
// and the held-port image is a Linux image.
func main() {
	fmt.Fprintf(os.Stderr, "envoy-listener-heldport: runs on linux only, not %s\n", runtime.GOOS)
	os.Exit(1)
}
