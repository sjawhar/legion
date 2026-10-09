//go:build (!linux && !darwin) || (!amd64 && !arm64)

package main

import (
	"errors"
	"testing"
)

func TestUnsupportedValuePromptRefusesWithoutReading(t *testing.T) {
	line, err := readHiddenAtTerminal(-1, func() { t.Fatal("unsupported prompt showed its label") }, func() { t.Fatal("unsupported prompt called onStop") })
	if line != nil || !errors.Is(err, errNoValuePrompt) {
		t.Fatalf("unsupported prompt returned %q, %v; want %v", line, err, errNoValuePrompt)
	}
}
