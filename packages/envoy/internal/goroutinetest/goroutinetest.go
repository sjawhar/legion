// Package goroutinetest reads the goroutine dump, for a test that waits on what another goroutine
// is doing at a point the code under test neither logs nor signals.
package goroutinetest

import (
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// Traces returns the trace of every goroutine in the process, read from one dump of them all. Each
// trace opens with its header line, "goroutine <id> [<state>]:".
func Traces() []string {
	dump := make([]byte, 1<<20)
	for {
		n := runtime.Stack(dump, true)
		if n < len(dump) {
			return strings.Split(string(dump[:n]), "\n\n")
		}
		dump = make([]byte, 2*len(dump))
	}
}

// ID returns the calling goroutine's id, which the runtime gives no other goroutine.
func ID() uint64 {
	header := make([]byte, 64)
	header = header[:runtime.Stack(header, false)]
	// The trace opens with "goroutine <id> ".
	id, err := strconv.ParseUint(strings.Fields(string(header))[1], 10, 64)
	if err != nil {
		panic(fmt.Sprintf("read the goroutine id from %q: %v", header, err))
	}
	return id
}

// Live reports whether the goroutine with the id ID returned has yet to return.
func Live(id uint64) bool {
	header := fmt.Sprintf("goroutine %d ", id)
	return slices.ContainsFunc(Traces(), func(trace string) bool { return strings.HasPrefix(trace, header) })
}
