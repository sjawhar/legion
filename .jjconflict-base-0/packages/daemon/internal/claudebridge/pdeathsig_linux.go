package claudebridge

import (
	"os/exec"
	"syscall"
)

// setParentDeathSignal has the kernel SIGTERM Claude Code when the bridge dies, so a bridge the shim
// had to kill never leaves a Claude Code running with no one reading it.
func setParentDeathSignal(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
