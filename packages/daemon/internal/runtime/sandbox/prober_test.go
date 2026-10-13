package sandbox

import (
	"context"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// proberOptions are testOptions without anything only supervision reads: no provisioning token
// source, no gh-files function for the roles, no store, no connection directory and no worker
// stream.
func proberOptions() Options {
	opts := testOptions()
	opts.Tokens, opts.GitHubCredential, opts.Store, opts.Conns, opts.StreamURL = nil, nil, nil, nil, ""
	return opts
}

// asProber builds the rig's runtime with newProber, from options holding none of supervision's.
func asProber() rigOption {
	return func(g *rig, o *Options) {
		g.prober = true
		o.Tokens, o.GitHubCredential, o.Store, o.Conns, o.StreamURL = nil, nil, nil, nil, ""
	}
}

// probeRunPattern is a Prober's probe run: eight hex characters drawn for each Prober.
var probeRunPattern = regexp.MustCompile(`^[0-9a-f]{8}$`)

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

// For the same configuration a Prober writes the probe Sandbox the daemon's own runtime writes, to
// the last field, under a name of its own: the daemon's, then the Prober's run. So a probe it passes
// is the one a booting daemon would pass, and the two never share one Sandbox. Each Prober draws a
// run of its own.
func TestAProberBuildsTheProbeSandboxTheDaemonBuilds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	full, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	opts := goldenOptions()
	opts.Tokens, opts.GitHubCredential, opts.Store, opts.Conns, opts.StreamURL = nil, nil, nil, nil, ""
	prober, err := newProber(ctx, newDynamic(t), kubefake.NewClientset(), opts)
	if err != nil {
		t.Fatal(err)
	}
	another, err := newProber(ctx, newDynamic(t), kubefake.NewClientset(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if full.probeRun != "" || !probeRunPattern.MatchString(prober.r.probeRun) || another.r.probeRun == prober.r.probeRun {
		t.Fatalf("probe runs: the daemon's %q, two Probers' %q and %q; want none for the daemon and eight hex of its own for each Prober",
			full.probeRun, prober.r.probeRun, another.r.probeRun)
	}
	daemonName := probeName(full.project, testDigestHex, full.probeRun)
	proberName := probeName(prober.r.project, testDigestHex, prober.r.probeRun)
	if proberName != daemonName+"-"+prober.r.probeRun || len(proberName) > maxNameLength {
		t.Fatalf("the Prober's probe Sandbox is %q, want the daemon's %q and its run, within %d characters", proberName, daemonName, maxNameLength)
	}
	shutdown := time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)
	want, err := encodeProbe(full.probeManifest(daemonName, probeOptions(t), shutdown))
	if err != nil {
		t.Fatal(err)
	}
	got, err := encodeProbe(prober.r.probeManifest(proberName, probeOptions(t), shutdown))
	if err != nil {
		t.Fatal(err)
	}
	if got.GetName() != proberName || got.GetLabels()[labelProbe] == "" {
		t.Fatalf("the Prober's manifest is named %q with labels %v, want %q carrying %s", got.GetName(), got.GetLabels(), proberName, labelProbe)
	}
	unstructured.RemoveNestedField(want.Object, "metadata", "name")
	unstructured.RemoveNestedField(got.Object, "metadata", "name")
	if !reflect.DeepEqual(got.Object, want.Object) {
		t.Fatalf("the prober's probe Sandbox differs from the daemon's beyond its name:\n got %v\nwant %v", got.Object, want.Object)
	}
}

// A prober with no token source, store or connections runs the probe end to end: it creates the
// probe Sandbox under its own name, reads the OK line from its pod, passes, and deletes the Sandbox.
func TestAProberProbesTheImage(t *testing.T) {
	g := newProbeRig(t, nil, asProber()).succeeds(okLine(3))
	if err := g.probe(probeOptions(t)); err != nil {
		t.Fatalf("ProbeImage = %v, want a pass", err)
	}
	if creates := g.creates.Load(); creates != 1 {
		t.Errorf("the prober created %d probe Sandboxes, want 1", creates)
	}
	name := probeName(testProject, testDigestHex, g.p.r.probeRun)
	if verbs := sandboxWrites(g.rig, name); !slices.Equal(verbs, []string{"create", "delete"}) {
		t.Errorf("the prober's writes to its Sandbox %s = %v, want its create and its delete", name, verbs)
	}
	g.eventually("the probe Sandbox to be deleted", func() bool { return g.sandbox(name) == nil })
}

// The prober refuses what the daemon's probe refuses: a Failed probe pod fails it, quoting its log.
func TestAProberRefusesAnImageTheDaemonsProbeRefuses(t *testing.T) {
	g := newProbeRig(t, nil, asProber()).fails("legion probe-image: pi-legion speaks daemon API contract 2; this daemon requires 3")
	err := g.probe(probeOptions(t))
	wantContains(t, err, "pod "+probeName(testProject, testDigestHex, g.p.r.probeRun)+" Failed", "speaks daemon API contract 2")
}

// A probe-boot may run while the project's daemon boots: on one cluster, with either probe in
// flight, the other runs to its pass beside it. Each creates and deletes its own Sandbox and touches
// nothing of the other's — no create meets the other's name, no delete takes it — and the probe
// held in flight then passes on its first attempt, its Sandbox the one it created.
func TestAProberAndABootingDaemonEachKeepTheirOwnProbeSandbox(t *testing.T) {
	for _, daemonFirst := range []bool{true, false} {
		name := map[bool]string{true: "the daemon's probe in flight", false: "the prober's probe in flight"}[daemonFirst]
		t.Run(name, func(t *testing.T) {
			g := newProbeRig(t, nil).succeeds(okLine(3))
			prober, err := newProber(g.ctx, g.dyn, g.kube, g.opts)
			if err != nil {
				t.Fatal(err)
			}
			proberName := probeName(testProject, testDigestHex, prober.r.probeRun)
			daemon := func() error { _, err := g.r.ProbeImage(g.ctx, probeOptions(t)); return err }
			probe := func() error { _, err := prober.ProbeImage(g.ctx, probeOptions(t)); return err }
			inFlight, inFlightName, beside := daemon, probeSandboxName, probe
			if !daemonFirst {
				inFlight, inFlightName, beside = probe, proberName, daemon
			}
			g.with(func() { g.held = func(name string) bool { return name == inFlightName } })
			done := make(chan error, 1)
			go func() { done <- inFlight() }()
			g.eventually("the probe held in flight to have its pod", func() bool { return g.pod(inFlightName) != nil })
			held := g.sandbox(inFlightName)

			if err := beside(); err != nil {
				t.Fatalf("the probe beside the one in flight = %v, want a pass", err)
			}
			if s := g.sandbox(inFlightName); s == nil || s.UID != held.UID {
				t.Fatalf("the Sandbox %s in flight is gone or replaced after the other probe ran", inFlightName)
			}
			g.with(func() { g.held = nil })
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the probe held in flight = %v, want a pass", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the probe held in flight never finished")
			}

			for _, sandbox := range []string{probeSandboxName, proberName} {
				if verbs := sandboxWrites(g.rig, sandbox); !slices.Equal(verbs, []string{"create", "delete"}) {
					t.Errorf("the writes to the Sandbox %s = %v, want its own probe's create and delete alone", sandbox, verbs)
				}
			}
			for _, w := range g.writes() {
				if w.verb == "create" && w.name == proberName {
					if labels := w.object.(*unstructured.Unstructured).GetLabels(); labels[labelProbe] == "" || labels[labelProject] != testProject {
						t.Errorf("the prober's Sandbox was created with labels %v, want the probe label and project %s", labels, testProject)
					}
				}
			}
		})
	}
}

// A Prober starts no refresher of the roles' gh files (newProber). With a live issue pod of the
// project in the namespace, whose role Secrets a refresher reads at every tick, a Prober holds its
// probe past many ticks and reads no Secret; the control, a runtime that does start one (start),
// reads them within the same wait.
func TestAProberRunsNoRefresherPastARefreshTick(t *testing.T) {
	g := newProbeRig(t, nil).succeeds(okLine(3))
	g.autoStart.Store(true)
	g.spawn(rootSpec(t))
	g.autoStart.Store(false)
	tick := 5 * time.Millisecond
	defer func(was time.Duration) { gitHubRefreshInterval = was }(gitHubRefreshInterval)
	gitHubRefreshInterval = tick
	prober, err := newProber(g.ctx, g.dyn, g.kube, g.opts)
	if err != nil {
		t.Fatal(err)
	}
	proberName := probeName(testProject, testDigestHex, prober.r.probeRun)
	g.clearActions()
	g.with(func() { g.held = func(name string) bool { return name == proberName } })
	done := make(chan error, 1)
	go func() { _, err := prober.ProbeImage(g.ctx, probeOptions(t)); done <- err }()
	g.eventually("the prober's probe to have its pod", func() bool { return g.pod(proberName) != nil })
	time.Sleep(40 * tick)

	if reads := secretReads(g.rig); reads != 0 {
		t.Fatalf("the Prober read %d Secrets while its probe was held past 40 refresh ticks; it starts no refresher", reads)
	}
	g.with(func() { g.held = nil })
	if err := <-done; err != nil {
		t.Fatalf("ProbeImage = %v, want a pass", err)
	}
	control, err := configure(g.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := control.start(g.ctx, g.dyn, g.kube); err != nil {
		t.Fatal(err)
	}
	g.eventually("the control's refresher to read the issue pod's role Secrets", func() bool { return secretReads(g.rig) > 0 })
}

// sandboxWrites are the verbs of every write to the Sandbox named name, in order.
func sandboxWrites(g *rig, name string) []string {
	var verbs []string
	for _, w := range g.writes() {
		if w.resource == "sandboxes" && w.name == name {
			verbs = append(verbs, w.verb)
		}
	}
	return verbs
}

// secretReads counts the Secret gets the rig has recorded.
func secretReads(g *rig) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	reads := 0
	for _, a := range g.actions {
		if a.verb == "get" && a.resource == "secrets" {
			reads++
		}
	}
	return reads
}
