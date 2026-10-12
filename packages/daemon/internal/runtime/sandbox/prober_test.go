package sandbox

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// proberOptions are testOptions without anything only supervision reads: no provisioning token
// source, no GitHub credential function, no store, no connection directory and no worker stream.
func proberOptions() Options {
	opts := testOptions()
	opts.Tokens, opts.GitHubCredential, opts.Store, opts.Conns, opts.StreamURL = nil, nil, nil, nil, ""
	return opts
}

// asProber builds the rig's runtime as NewProber builds it: the probe's configuration alone and the
// informers, with none of supervision's options.
func asProber() rigOption {
	return func(g *rig, o *Options) {
		g.prober = true
		o.Tokens, o.GitHubCredential, o.Store, o.Conns, o.StreamURL = nil, nil, nil, nil, ""
	}
}

// The prober takes Options that hold nothing only supervision reads, which the full runtime refuses.
func TestAProberNeedsNoSupervision(t *testing.T) {
	if _, err := configure(proberOptions()); err == nil {
		t.Fatal("configure accepted options with no token source, store or connections; the control must refuse")
	}
	if _, err := configureProber(proberOptions()); err != nil {
		t.Fatalf("configureProber = %v, want the probe's options accepted", err)
	}
}

// Every refusal of what the probe itself reads stands for the prober: the namespace, the
// digest-pinned image, the pool the runtime places every pod on, and every role's reservation.
func TestAProberRefusesWhatTheFullRuntimeRefusesOfTheProbe(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Options)
		want string
	}{
		{"no namespace", func(o *Options) { o.Namespace = "" }, "no namespace"},
		{"an image by tag", func(o *Options) { o.Image = "ghcr.io/example/legion-worker:latest" }, "is not pinned by digest"},
		{"the pool selector", func(o *Options) { o.Scheduling.NodeSelector = map[string]string{poolKey: "other"} }, "is the runtime's"},
		{"a missing reservation", func(o *Options) { delete(o.Resources, claim.RoleImplementer) }, "no reservation for role implementer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := proberOptions()
			tc.edit(&opts)
			if _, err := configureProber(opts); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("configureProber = %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

// For the same configuration the prober writes exactly the probe Sandbox the daemon's own runtime
// writes, so a probe it passes is the one a booting daemon would pass.
func TestAProberBuildsTheProbeSandboxTheDaemonBuilds(t *testing.T) {
	full, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	opts := goldenOptions()
	opts.Tokens, opts.GitHubCredential, opts.Store, opts.Conns, opts.StreamURL = nil, nil, nil, nil, ""
	prober, err := configureProber(opts)
	if err != nil {
		t.Fatal(err)
	}
	shutdown := time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)
	want, err := encodeProbe(full.probeManifest(probeSandboxName, probeOptions(t), shutdown))
	if err != nil {
		t.Fatal(err)
	}
	got, err := encodeProbe(prober.probeManifest(probeSandboxName, probeOptions(t), shutdown))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Object, want.Object) {
		t.Fatalf("the prober's probe Sandbox differs from the daemon's:\n got %v\nwant %v", got.Object, want.Object)
	}
}

// A prober with no token source, store or connections runs the probe end to end: it creates the
// probe Sandbox, reads the OK line from its pod, passes, and deletes the Sandbox.
func TestAProberProbesTheImage(t *testing.T) {
	g := newProbeRig(t, nil, asProber()).succeeds(okLine(3))
	if err := g.probe(probeOptions(t)); err != nil {
		t.Fatalf("ProbeImage = %v, want a pass", err)
	}
	if creates := g.creates.Load(); creates != 1 {
		t.Errorf("the prober created %d probe Sandboxes, want 1", creates)
	}
	g.eventually("the probe Sandbox to be deleted", func() bool { return g.sandbox(probeSandboxName) == nil })
}

// The prober refuses what the daemon's probe refuses: a Failed probe pod fails it, quoting its log.
func TestAProberRefusesAnImageTheDaemonsProbeRefuses(t *testing.T) {
	g := newProbeRig(t, nil, asProber()).fails("legion probe-image: pi-legion speaks daemon API contract 2; this daemon requires 3")
	err := g.probe(probeOptions(t))
	wantContains(t, err, "pod "+probeSandboxName+" Failed", "speaks daemon API contract 2")
}
