package config

import (
	"strings"
	"testing"
)

// settingsRouteBlock is `settings_route` with the issuer the test names, and the audience and
// ServiceAccount a deployment writes beside it.
func settingsRouteBlock(issuer string) string {
	return "settings_route:\n  oidc_issuer: " + issuer + "\n  audience: legion-daemon\n" +
		"  service_accounts: [system:serviceaccount:dispatch:dispatch]\n"
}

// The issuer the daemon checks every admitted token's signature against is read over a transport
// nobody on the path can rewrite: a plaintext issuer out on the network is refused, naming the
// key, because whoever held that path could serve a key set of their own and mint a token for
// any ServiceAccount the deployment names.
func TestAPlaintextOIDCIssuerIsRefused(t *testing.T) {
	path := writeConfigFile(t, minimalFile+settingsRouteBlock("http://kubernetes.default.svc"))

	_, err := Load(path, noEnv)

	if err == nil {
		t.Fatal("the loader took a plaintext oidc_issuer, want it refused")
	}
	if !strings.Contains(err.Error(), "settings_route.oidc_issuer must be an https URL") {
		t.Fatalf("the refusal is %q, want it to name the key and https", err)
	}
}

// An https issuer is taken, and so is a loopback one over plain http: nothing is on that path,
// and it is how a test runs an issuer of its own.
func TestAnHTTPSIssuerAndALoopbackIssuerAreTaken(t *testing.T) {
	for name, issuer := range map[string]string{
		"an https issuer":    "https://container.googleapis.com/v1/projects/p/locations/l/clusters/c",
		"a loopback issuer":  "http://127.0.0.1:8443",
		"a localhost issuer": "http://localhost:8443",
	} {
		t.Run(name, func(t *testing.T) {
			path := writeConfigFile(t, minimalFile+settingsRouteBlock(issuer))

			cfg, err := Load(path, noEnv)

			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.SettingsRoute.OIDCIssuer != issuer {
				t.Fatalf("oidc_issuer = %q, want %q", cfg.SettingsRoute.OIDCIssuer, issuer)
			}
		})
	}
}

// Which paths each caller of the settings route may set is a property of the class table's rows
// and no second list (Row.Plain): the keys the daemon reads at each decision, and nothing that
// names a command it runs, a credential, the file holding one, an address it connects to or
// listens on, or the rule of who may call the route (INFRA-1077's spec, "Which settings a
// projected token may change").
func TestAProjectedCallerSetsThePlainDataRowsAndNothingElse(t *testing.T) {
	for _, path := range []string{
		"admission_cap", "review_round_cap", "max_fix_attempts", "linger_hours",
		"launch_failure_limit", "prompt_failure_limit", "prompt_retire_limit",
		"worker_boot_timeout_seconds", "tree_stop_timeout_seconds",
	} {
		if admitted, why := CallerProjected.Admits(path); !admitted {
			t.Errorf("a projected token may not set %s (%s), and it is plain data", path, why)
		}
	}
	for _, path := range []string{
		"github_apps.implement.private_key_command", "github_apps.implement.private_key",
		"github_apps.implement.token_command", "operator_token_file", "dispatch_url",
		"nats_urls", "envoy_url", "daemon_url", "advertise_host",
		"settings_route.oidc_issuer", "settings_route.audience", "settings_route.service_accounts",
		"instructions", "omp_invocation", "omp_launch_prefix", "runtime.kubernetes.image",
		"runtime.kubernetes.pod.env", "controller", "repository_renames",
		// The design gate is plain data by its row and is refused all the same (below).
		"gates", "gates.design",
	} {
		admitted, why := CallerProjected.Admits(path)
		if admitted {
			t.Errorf("a projected token may set %s, which is no plain data", path)
		}
		if why == "" {
			t.Errorf("the refusal of %s says nothing", path)
		}
	}
	// The operator bearer keeps the whole table, the fixed paths included: those the apply
	// refuses by name with their own reason.
	for _, path := range []string{"github_apps.implement.private_key_command", "admission_cap", "project"} {
		if admitted, why := CallerOperator.Admits(path); !admitted {
			t.Errorf("the operator bearer may not set %s (%s)", path, why)
		}
	}
	// A path the table does not hold is admitted here and refused by the apply as an unknown
	// setting, so no caller learns which paths exist from this answer alone.
	if admitted, _ := CallerProjected.Admits("no_such_setting"); !admitted {
		t.Error("a path the class table does not hold is refused by the caller rule, want it left to the apply")
	}
}

// The design gate is the one plain-data key a projected ServiceAccount token may not set, and it
// is refused by name: a gate is the rule that says a person decides a tree's specification, and
// a caller holding a token the whole service shares is not the party that may switch that rule
// off. Its row is a decision row still — the operator bearer and the daemon's own file change it
// as they change any other key — so the rule lives in the caller and not in the table.
func TestAProjectedCallerIsRefusedTheGatesBlockByName(t *testing.T) {
	if admitted, why := CallerProjected.Admits("admission_cap"); !admitted {
		t.Fatalf("a projected token may not set admission_cap (%s), and it is plain data", why)
	}
	for _, path := range []string{"gates", "gates.design"} {
		admitted, why := CallerProjected.Admits(path)
		if admitted {
			t.Errorf("a projected token may set %s, want it refused by name", path)
		}
		if why == "" {
			t.Errorf("the refusal of %s says nothing", path)
		}
	}
	// The row keeps its class, its holder and its answer to every other reader: only the
	// projected caller is refused it.
	row, known := ClassOf("gates.design")
	if !known || row.Class != ClassDecision || !row.Plain() {
		t.Fatalf("gates.design's row = %+v, want the decision row it was", row)
	}
	if admitted, why := CallerOperator.Admits("gates.design"); !admitted {
		t.Fatalf("the operator bearer may not set gates.design (%s)", why)
	}
}
