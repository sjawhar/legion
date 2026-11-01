//go:build !linux

package claudebridge

import "os/exec"

// setParentDeathSignal is Linux's alone: elsewhere Claude Code ends when its input does, which a
// killed bridge closes.
func setParentDeathSignal(*exec.Cmd) {}
