package pmdoc

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestZZPerf(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&b, "Unmatched [^QQ%d].\n\n", i)
	}
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&b, "[^d%d]: Definition.\n\n", i)
	}
	start := time.Now()
	_, err := Parse(b.String())
	t.Logf("zz bytes=%d parse=%v err=%v", b.Len(), time.Since(start), err)
}
