//go:build (!linux && !darwin) || (!amd64 && !arm64)

package main

func readHiddenAtTerminal(_ int, _, _ func()) ([]byte, error) {
	return nil, errNoValuePrompt
}
