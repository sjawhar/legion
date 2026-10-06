package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// run shares its process-group handling with workspace.execRunner (both call procgroup.Configure
// and classify the result with procgroup.HeldOpen): `sh` itself exits (`exit 0`) within
// milliseconds here, well short of the gate's own generous 5 s timeout, but `sleep 8 &`
// backgrounds a grandchild that keeps sh's inherited stdout pipe open long after, so Wait does not
// return until procgroup.WaitDelay gives up draining it. run must report the real exit (0) while
// still setting heldOpen, rather than let an attempt that never actually answered look like an
// ordinary successful probe.
func TestPluginGateRunReportsErrWaitDelayAsItsRealExitZeroButHeldOpen(t *testing.T) {
	g := pluginGate{
		env:     map[string]string{"PATH": os.Getenv("PATH")},
		workDir: t.TempDir(),
		timeout: 5 * time.Second,
		retry:   bootprobe.Retry{Initial: 10 * time.Millisecond, Max: 40 * time.Millisecond},
	}

	started := time.Now()
	r, err := g.run(context.Background(), "echo done; sleep 8 & exit 0")
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("run = %v, want no error (heldOpen is reported through ran, not err)", err)
	}
	if elapsed < procgroup.WaitDelay || elapsed > procgroup.WaitDelay+2*time.Second {
		t.Fatalf("run took %s, want close to its %s WaitDelay (sh itself exits in milliseconds; its 5 s timeout never fires a kill)", elapsed, procgroup.WaitDelay)
	}
	if r.exit != 0 {
		t.Errorf("exit = %d, want 0: sh itself really did exit 0", r.exit)
	}
	if !r.heldOpen {
		t.Error("heldOpen = false, want true: the backgrounded sleep kept sh's stdout pipe open past WaitDelay")
	}
	if r.timedOut {
		t.Error("timedOut = true, want false: the gate's own 5 s timeout never had reason to fire")
	}
	if got := r.heldOpenDetail("the probe"); got == "" || !strings.Contains(got, "held its output open") {
		t.Errorf("heldOpenDetail = %q, want it to say a process held its output open", got)
	}
}
