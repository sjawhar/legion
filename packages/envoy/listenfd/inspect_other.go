//go:build !linux && !darwin

package listenfd

import (
	"fmt"
	"net"
	"runtime"
)

// inspect refuses every descriptor: an inherited socket is read on linux and darwin only.
func inspect(int) (*net.TCPAddr, bool, error) {
	return nil, false, fmt.Errorf("cannot be inherited on %s: an inherited listening socket is supported on linux and darwin only", runtime.GOOS)
}
