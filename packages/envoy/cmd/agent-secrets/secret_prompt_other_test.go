//go:build (!linux && !darwin) || (!amd64 && !arm64)

package main

import (
	"strings"
	"testing"
)

func TestUnsupportedValuePromptRefusesWithoutReading(t *testing.T) {
	line, err := readHiddenAtTerminal(-1, func() { t.Fatal("unsupported prompt showed its label") }, func() { t.Fatal("unsupported prompt called onStop") })
	if line != nil || err == nil || !strings.Contains(err.Error(), "no value prompt is available on this platform; pipe the value on stdin") {
		t.Fatalf("unsupported prompt returned %q, %v", line, err)
	}
}
