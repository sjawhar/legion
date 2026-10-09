package daemon

import (
	"bytes"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
)

// reserved is a role's CPU and memory reservation, both set: each is the container's request and
// its limit.
var reserved = config.RoleResources{CPU: "500m", Memory: "1Gi"}

// Deployment reads the configuration alone: the decisions, the runtime, whether a broker is
// configured, and the roles whose pods reserve no CPU and memory — a role lacking either quantity
// counts, in claim.Roles' order, and the controller only under controller: daemon (a Kubernetes
// block the loader settled holds every role, so only a value built by hand names one). A tmux
// configuration names no role: the tmux runtime sets no limits at all, which the report says in
// its own words.
func TestDeploymentReadsTheConfigurationAlone(t *testing.T) {
	decided := map[capabilities.Name]string{capabilities.Secrets: "dispatch://LEGION-205 enrolls pods later"}
	kubernetes := func(resources map[claim.Role]config.RoleResources, agentSecrets *config.AgentSecretsConfig, launch config.ControllerLaunch) config.Config {
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
		{"nothing reserved, no broker", kubernetes(nil, nil, config.ControllerLaunchOperator),
			capabilities.Deployment{Runtime: "kubernetes", Decided: decided, RolesWithoutResources: claim.Roles}},
		{"every workflow role reserved, a broker configured", kubernetes(map[claim.Role]config.RoleResources{
			claim.RoleArchitect: reserved, claim.RolePlanner: reserved, claim.RoleImplementer: reserved,
			claim.RoleTester: reserved, claim.RoleReviewer: reserved, claim.RoleMerger: reserved,
		}, &config.AgentSecretsConfig{URL: "https://secrets.internal.example"}, config.ControllerLaunchOperator),
			capabilities.Deployment{Runtime: "kubernetes", Decided: decided, AgentSecrets: true}},
		{"a role missing one quantity, and the controller under controller: daemon", kubernetes(map[claim.Role]config.RoleResources{
			claim.RoleArchitect: reserved, claim.RolePlanner: reserved, claim.RoleImplementer: reserved,
			claim.RoleTester:   {CPU: "2"},
			claim.RoleReviewer: reserved, claim.RoleMerger: reserved,
		}, nil, config.ControllerLaunchDaemon),
			capabilities.Deployment{Runtime: "kubernetes", Decided: decided, RolesWithoutResources: []claim.Role{claim.RoleTester, claim.RoleController}}},
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
