package daemon

import (
	"bytes"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/config"
)

// reserved is a pod's CPU and memory reservation, both set, each request equal to its limit.
var reserved = config.Reservation{CPU: "500m", CPURequest: "500m", Memory: "1Gi", MemoryRequest: "1Gi"}

// bursting requests less memory than its limit, and 1000m of cpu under a limit of 1: a quantity
// compare, so only the memory makes it Burstable.
var bursting = config.Reservation{CPU: "1", CPURequest: "1000m", Memory: "32Gi", MemoryRequest: "8Gi"}

// spelledApart requests what it is limited to, each spelled two ways (1000m against 1, 1024Mi
// against 1Gi): a string compare would read it Burstable, a quantity compare reads it Guaranteed.
var spelledApart = config.Reservation{CPU: "1", CPURequest: "1000m", Memory: "1Gi", MemoryRequest: "1024Mi"}

// Deployment reads the configuration alone: the decisions, the runtime, whether a broker is
// configured, the pods that reserve no CPU and memory — a pod lacking either quantity counts, the
// issue pod first, and the controller's pod only under controller: daemon (a Kubernetes block the
// loader settled holds both, so only a value built by hand names one) — and the pods that request
// less than their limit, the controller's pod among them only under controller: daemon. A tmux
// configuration names no pod: the tmux runtime sets no limits at all, which the report says in its
// own words.
func TestDeploymentReadsTheConfigurationAlone(t *testing.T) {
	decided := map[capabilities.Name]string{capabilities.Secrets: "dispatch://LEGION-205 enrolls pods later"}
	kubernetes := func(resources config.PodResources, agentSecrets *config.AgentSecretsConfig, launch config.ControllerLaunch) config.Config {
		return config.Config{
			Runtime:          config.Runtime{Name: "kubernetes", Kubernetes: &config.Kubernetes{Resources: resources, AgentSecrets: agentSecrets}},
			ControllerLaunch: launch,
			Capabilities:     config.Capabilities{Decided: decided},
		}
	}
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want capabilities.Deployment
	}{
		{"tmux", config.Config{Runtime: config.Runtime{Name: "tmux"}, Capabilities: config.Capabilities{Decided: decided}},
			capabilities.Deployment{Runtime: "tmux", Decided: decided}},
		{"nothing reserved, no broker", kubernetes(config.PodResources{}, nil, config.ControllerLaunchOperator),
			capabilities.Deployment{Runtime: "kubernetes", Decided: decided, UnreservedPods: []capabilities.Pod{capabilities.PodIssue}}},
		{"the issue pod reserved, a broker configured", kubernetes(config.PodResources{Issue: reserved},
			&config.AgentSecretsConfig{URL: "https://secrets.internal.example"}, config.ControllerLaunchOperator),
			capabilities.Deployment{Runtime: "kubernetes", Decided: decided, AgentSecrets: true}},
		{"the issue pod missing one quantity, and the controller under controller: daemon", kubernetes(config.PodResources{Issue: config.Reservation{CPU: "2"}}, nil, config.ControllerLaunchDaemon),
			capabilities.Deployment{Runtime: "kubernetes", Decided: decided, UnreservedPods: []capabilities.Pod{capabilities.PodIssue, capabilities.PodController}}},
		{"both pods requesting less than their limits", kubernetes(config.PodResources{Issue: bursting, Controller: bursting}, nil, config.ControllerLaunchDaemon),
			capabilities.Deployment{Runtime: "kubernetes", Decided: decided, BurstablePods: []capabilities.Pod{capabilities.PodIssue, capabilities.PodController}}},
		{"a Burstable controller the daemon does not launch", kubernetes(config.PodResources{Issue: reserved, Controller: bursting}, nil, config.ControllerLaunchOperator),
			capabilities.Deployment{Runtime: "kubernetes", Decided: decided}},
		{"a request spelled apart from its equal limit", kubernetes(config.PodResources{Issue: spelledApart, Controller: spelledApart}, nil, config.ControllerLaunchDaemon),
			capabilities.Deployment{Runtime: "kubernetes", Decided: decided}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Deployment(tc.cfg); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Deployment(cfg) = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// reportCapabilities logs the open rows once at boot and again only when the set changes, and
// answers the names each time: the tick carries them while the log stays quiet. Under tmux every
// deployment row starts open; the model-fallback read closing one changes the set.
func TestReportCapabilitiesLogsAtBootAndOnChangeAlone(t *testing.T) {
	var logged bytes.Buffer
	s := &supervision{
		cfg: config.Config{Runtime: config.Runtime{Name: "tmux"}},
		log: slog.New(slog.NewTextHandler(&logged, nil)),
	}
	warnings := func() int { return strings.Count(logged.String(), "level=WARN") }

	if got, want := s.reportCapabilities(), []string{"secrets", "model-fallback", "resource-limits"}; !slices.Equal(got, want) {
		t.Fatalf("reportCapabilities at boot = %v, want %v", got, want)
	}
	if warnings() != 3 {
		t.Fatalf("boot logged %d warnings, want one per open row (3):\n%s", warnings(), logged.String())
	}
	for _, want := range []string{
		`msg="capability secrets is open: the tmux runtime enrolls no process with the secrets broker; to record a decision, add to legion.yaml: capabilities.decided.secrets: \"<reason>\""`,
		`configLine="capabilities.decided.model-fallback: \"<reason>\""`,
	} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("the boot report lacks %s:\n%s", want, logged.String())
		}
	}

	s.reportCapabilities()
	if warnings() != 3 {
		t.Fatalf("an unchanged set logged again (%d warnings):\n%s", warnings(), logged.String())
	}

	s.imageReport.ModelFallback = "on"
	if got, want := s.reportCapabilities(), []string{"secrets", "resource-limits"}; !slices.Equal(got, want) {
		t.Fatalf("reportCapabilities after the read = %v, want %v", got, want)
	}
	if warnings() != 5 {
		t.Fatalf("the changed set logged %d warnings in all, want 3 then 2 (5):\n%s", warnings(), logged.String())
	}
	if strings.Count(logged.String(), "capability model-fallback is open") != 1 {
		t.Fatalf("model-fallback was logged after it closed:\n%s", logged.String())
	}
}

// What the probe's run saw of the pool reaches the report: a kubernetes daemon whose probe pod
// waited on capacity before it scheduled reports pool-capacity open, with the count and the
// scheduler's reason, until the file decides it; one whose pod scheduled at once reports it present.
func TestReportCapabilitiesCarriesTheProbesCapacityWaits(t *testing.T) {
	reason := "probe pod legion-probe-legion-1d10089a0000 is Unschedulable: 0/83 nodes are available: 25 Insufficient cpu"
	decided := map[capabilities.Name]string{capabilities.Secrets: "dispatch://LEGION-205 enrolls pods later"}
	cfg := config.Config{
		Runtime:      config.Runtime{Name: "kubernetes", Kubernetes: &config.Kubernetes{Resources: config.DefaultResources()}},
		Capabilities: config.Capabilities{Decided: decided},
	}
	var logged bytes.Buffer
	s := &supervision{cfg: cfg, log: slog.New(slog.NewTextHandler(&logged, nil)), probed: true,
		imageReport: bootprobe.ImageReport{ModelFallback: "on", CapacityWaits: 2, CapacityReason: reason}}

	if got, want := s.reportCapabilities(), []string{"pool-capacity"}; !slices.Equal(got, want) {
		t.Fatalf("reportCapabilities = %v, want %v", got, want)
	}
	if want := "the image probe pod was Unschedulable 2 time(s) at boot before it scheduled: " + reason; !strings.Contains(logged.String(), want) {
		t.Fatalf("the open row was logged without the count and the reason:\n%s", logged.String())
	}

	decided[capabilities.PoolCapacity] = "the pool is sized for the trees; the probe waits"
	if got := s.reportCapabilities(); len(got) != 0 {
		t.Fatalf("reportCapabilities with the row decided = %v, want none open", got)
	}

	s.imageReport.CapacityWaits, s.imageReport.CapacityReason = 0, ""
	if got := states(s.deployment().Report())[capabilities.PoolCapacity]; got.Status != capabilities.StatusPresent || got.Detail != "the image probe pod scheduled at its first attempt" {
		t.Fatalf("pool-capacity with no wait = %s (%s), want present", got.Status, got.Detail)
	}
}

// states is a report by capability.
func states(report []capabilities.State) map[capabilities.Name]capabilities.State {
	m := map[capabilities.Name]capabilities.State{}
	for _, state := range report {
		m[state.Name] = state
	}
	return m
}
