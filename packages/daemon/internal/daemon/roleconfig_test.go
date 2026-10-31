package daemon

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
)

// role_config_files reaches the runtime that launches each role, under either runtime, as the file
// settled it: the tmux runtime's Options and the Sandbox runtime's, which tell each role its own
// overlays and hand the image probe every role's.
func TestRoleConfigFilesReachEitherRuntime(t *testing.T) {
	files := map[claim.Role][]string{
		claim.RoleImplementer: {"/etc/legion-operator/fallback.yml"},
		claim.RoleController:  {"/etc/legion-operator/fallback.yml", "/etc/legion-operator/controller.yml"},
	}
	cfg := config.Config{Runtime: config.Runtime{Name: "tmux"}, RoleConfigFiles: files}
	if got := tmuxOptions(cfg, "test", nil, "", "", quietLogger()).RoleConfigFiles; !reflect.DeepEqual(got, files) {
		t.Errorf("tmux RoleConfigFiles = %v, want %v", got, files)
	}
	k := kubernetesConfig(t, "https://127.0.0.1:1")
	k.RoleConfigFiles = files
	opts, err := sandboxOptions(k, *k.Runtime.Kubernetes, "test", "", lookup(nil), quietLogger())
	if err != nil {
		t.Fatalf("sandboxOptions: %v", err)
	}
	if !reflect.DeepEqual(opts.RoleConfigFiles, files) {
		t.Errorf("sandbox RoleConfigFiles = %v, want %v", opts.RoleConfigFiles, files)
	}
}

// The tmux boot gate reads each named role's PI_CONFIG_FILES as its pane will start Oh My Pi on it
// (runtime.tmux.pi_config_files, then the role's own): a role file Oh My Pi cannot parse refuses
// the boot, naming the role, the file and Oh My Pi's error line, rather than failing each launch of
// that role; a role file Oh My Pi starts on, or no key at all, passes.
func TestTheTmuxBootGateRefusesARoleOverlayOhMyPiCannotParse(t *testing.T) {
	dir := t.TempDir()
	omp := filepath.Join(dir, "omp")
	script := "#!/bin/sh\nIFS=:\nfor f in $PI_CONFIG_FILES; do case \"$f\" in *malformed*)\n" +
		"  echo \"error: Failed to parse config overlay $f: SyntaxError\" >&2; exit 1;; esac; done\n" +
		"echo '{\"value\": false}'\n"
	if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	operator, good, malformed := filepath.Join(dir, "operator.yml"), filepath.Join(dir, "fallback.yml"), filepath.Join(dir, "malformed.yml")
	for _, file := range []string{operator, good, malformed} {
		if err := os.WriteFile(file, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	image := capabilities.Image{Launch: omp, Env: []string{"PATH=" + os.Getenv("PATH")}, WorkDir: dir}
	cfg := config.Config{Runtime: config.Runtime{Name: "tmux", Tmux: &config.Tmux{PiConfigFiles: []string{operator}}}}
	for _, tc := range []struct {
		name  string
		files map[claim.Role][]string
		want  string
	}{
		{"no key", nil, ""},
		{"a file Oh My Pi starts on", map[claim.Role][]string{claim.RoleImplementer: {good}}, ""},
		{"a file Oh My Pi cannot parse", map[claim.Role][]string{claim.RoleImplementer: {good}, claim.RoleTester: {good, malformed}},
			"boot gate: role_config_files.tester: the tester's Oh My Pi would not start: Oh My Pi refuses " + malformed +
				": error: Failed to parse config overlay " + malformed + ": SyntaxError"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg.RoleConfigFiles = tc.files
			err := checkPaneRoleOverlays(context.Background(), cfg, image, quietLogger())
			if (tc.want == "" && err != nil) || (tc.want != "" && (err == nil || err.Error() != tc.want)) {
				t.Fatalf("checkPaneRoleOverlays = %v, want %q", err, tc.want)
			}
		})
	}
}

// The gate boot builds for a tmux daemon (prepare) refuses a role file Oh My Pi cannot parse,
// before the plugin gate's load probes run, naming the role and the file.
func TestTheTmuxBootRefusesARoleOverlayOhMyPiCannotParse(t *testing.T) {
	t.Setenv("LEGION_TEST_PG_DSN", "postgres://legion:legion@127.0.0.1:1/legion?sslmode=disable&connect_timeout=1")
	dir := t.TempDir()
	omp := filepath.Join(dir, "omp")
	script := "#!/bin/sh\nIFS=:\nfor f in $PI_CONFIG_FILES; do case \"$f\" in *malformed*)\n" +
		"  echo \"error: Failed to parse config overlay $f: SyntaxError\" >&2; exit 1;; esac; done\n" +
		"echo '{\"value\": false}'\n"
	if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	malformed := filepath.Join(dir, "malformed.yml")
	if err := os.WriteFile(malformed, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.RoleConfigFiles = map[claim.Role][]string{claim.RoleMerger: {malformed}}
	p, err := prepare(cfg, quietLogger(), overrides{environ: []string{}, getenv: func(name string) string {
		if name == "LEGION_OMP_PATH" {
			return omp
		}
		return os.Getenv(name)
	}})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	want := "boot gate: role_config_files.merger: the merger's Oh My Pi would not start: Oh My Pi refuses " + malformed +
		": error: Failed to parse config overlay " + malformed + ": SyntaxError"
	if p.gate == nil {
		t.Fatal("prepare built no boot gate")
	}
	if err := p.gate(context.Background()); err == nil || err.Error() != want {
		t.Fatalf("the boot gate = %v, want %q", err, want)
	}
}
