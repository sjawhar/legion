//go:build (!linux && !darwin) || (!amd64 && !arm64)

package main

import "errors"

func readHiddenAtTerminal(_ int, _, _ func()) ([]byte, error) {
	return nil, errors.New("no value prompt is available on this platform; pipe the value on stdin")
}
