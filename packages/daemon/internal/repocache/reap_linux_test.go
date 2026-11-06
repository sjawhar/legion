//go:build linux

package repocache

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// reapExited reaps the orphans a process is handed as their subreaper. It runs in a child of the
// test made a subreaper (PR_SET_CHILD_SUBREAPER), so an orphaned grandchild comes to it as one
// comes to a container's PID 1, and no other test's processes are in reach.
func TestReapExitedReapsOrphans(t *testing.T) {
	if os.Getenv("REPOCACHE_REAP_HELPER") == "1" {
		const prSetChildSubreaper = 36
		if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
			fmt.Println("prctl:", errno)
			os.Exit(2)
		}
		if err := exec.Command("/bin/sh", "-c", "/bin/sh -c 'sleep 0.2' & exit 0").Run(); err != nil {
			fmt.Println("spawn:", err)
			os.Exit(2)
		}
		time.Sleep(time.Second)
		reaped, err := reapExited()
		fmt.Printf("reaped=%d err=%v\n", reaped, err)
		os.Exit(0)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestReapExitedReapsOrphans$")
	command.Env = append(os.Environ(), "REPOCACHE_REAP_HELPER=1")
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("the helper failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "reaped=1 err=<nil>") {
		t.Fatalf("the helper said %q, want one orphan reaped", out)
	}
}
