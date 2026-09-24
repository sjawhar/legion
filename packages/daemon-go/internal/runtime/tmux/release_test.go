package tmux

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// Release takes the claim once. A claim paired with another claim's locator is refused before tmux
// is asked anything: releasing one claim never stops a pane recorded for another.
func TestReleaseRefusesALocatorOfAnotherClaim(t *testing.T) {
	requireTmux(t)
	rt, err := New(Options{
		Project: "omp", StateDir: t.TempDir(), StreamAddress: "unix:///s", DaemonURL: "http://127.0.0.1:1",
		EnvoyURL: "http://127.0.0.1:2", OmpInvocation: "omp", StopGrace: time.Second, ProbeInterval: time.Second,
		AdoptTimeout: time.Second, Conns: fake.NewConns(), Environ: []string{"PATH=" + os.Getenv("PATH")},
		Executable: func() (string, error) { return "/opt/legion", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	var ran [][]string
	rt.run = func(_ context.Context, argv []string) (result, error) {
		ran = append(ran, argv)
		return result{exitCode: 1, stderr: "no server running on /tmp/tmux-1000/legion-omp"}, nil
	}
	other := runtime.Locator{
		Runtime: runtime.RuntimeTmux, Claim: "legion-omp-LEGION-43-reviewer", Incarnation: "4242:77",
		Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"},
	}
	err = rt.Release(context.Background(), runtime.Known{Claim: "legion-omp-LEGION-43-tester", Locator: &other})
	if err == nil || !strings.Contains(err.Error(), "legion-omp-LEGION-43-reviewer") {
		t.Fatalf("Release = %v, want a refusal naming the locator's claim", err)
	}
	if len(ran) != 0 {
		t.Fatalf("the refused release ran tmux: %q", ran)
	}
}
