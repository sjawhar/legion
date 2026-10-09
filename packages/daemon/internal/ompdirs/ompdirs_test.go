package ompdirs

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
}

// The profile root is where Oh My Pi, started under the environment, keeps the profile's state:
// the profile OMP_PROFILE (else PI_PROFILE) names under HOME's config root, unless an XDG data
// root already holds that profile's state, and an honoured PI_CODING_AGENT_DIR elsewhere turns
// the XDG data root off. The gate's manifest tests (internal/daemon) hold the full resolution to
// Oh My Pi's; these hold the root itself.
func TestProfileRootIsWhereOhMyPiKeepsTheProfilesState(t *testing.T) {
	passwdHome := ""
	if u, err := user.Current(); err == nil {
		passwdHome = u.HomeDir
	}
	for _, testCase := range []struct {
		name    string
		env     func(home, xdg string) map[string]string
		layout  func(t *testing.T, home, xdg string)
		want    func(home, xdg string) string
		profile string
	}{
		{
			name: "the default profile under HOME",
			env:  func(home, xdg string) map[string]string { return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg} },
			want: func(home, _ string) string { return filepath.Join(home, ".omp") },
		},
		{
			name:   "the default profile under an XDG data root that exists",
			env:    func(home, xdg string) map[string]string { return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg} },
			layout: func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:   func(_, xdg string) string { return filepath.Join(xdg, "omp") },
		},
		{
			name: "a named profile, trimmed, whose XDG directory does not exist",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "OMP_PROFILE": " work "}
			},
			layout:  func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:    func(home, _ string) string { return filepath.Join(home, ".omp", "profiles", "work") },
			profile: "work",
		},
		{
			name: "a named profile whose XDG directory exists",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "OMP_PROFILE": "work"}
			},
			layout:  func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp", "profiles", "work")) },
			want:    func(_, xdg string) string { return filepath.Join(xdg, "omp", "profiles", "work") },
			profile: "work",
		},
		{
			name:    "PI_PROFILE when OMP_PROFILE is unset",
			env:     func(home, _ string) map[string]string { return map[string]string{"HOME": home, "PI_PROFILE": "legacy"} },
			want:    func(home, _ string) string { return filepath.Join(home, ".omp", "profiles", "legacy") },
			profile: "legacy",
		},
		{
			name: "an empty OMP_PROFILE, or default, is the default profile",
			env: func(home, _ string) map[string]string {
				return map[string]string{"HOME": home, "OMP_PROFILE": "", "PI_PROFILE": "legacy"}
			},
			want: func(home, _ string) string { return filepath.Join(home, ".omp") },
		},
		{
			name: "no HOME is the account's home directory",
			env:  func(string, string) map[string]string { return map[string]string{} },
			want: func(string, string) string { return filepath.Join(passwdHome, ".omp") },
		},
		{
			name: "PI_CONFIG_DIR names the config root under HOME",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "PI_CONFIG_DIR": ".omp-alt"}
			},
			want: func(home, _ string) string { return filepath.Join(home, ".omp-alt") },
		},
		{
			name: "an honoured PI_CODING_AGENT_DIR turns the XDG data root off",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "PI_CODING_AGENT_DIR": filepath.Join(home, "elsewhere")}
			},
			layout: func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:   func(home, _ string) string { return filepath.Join(home, ".omp") },
		},
		{
			name: "a relative PI_CODING_AGENT_DIR naming the config root's own agent directory keeps the XDG data root",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "PI_CODING_AGENT_DIR": "../.omp/agent"}
			},
			layout: func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:   func(_, xdg string) string { return filepath.Join(xdg, "omp") },
		},
		{
			name: "the agent directory PI_PROFILE hands down keeps the XDG data root",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "OMP_PROFILE": "", "PI_PROFILE": "work",
					"PI_CODING_AGENT_DIR": filepath.Join(home, ".omp", "profiles", "work", "agent")}
			},
			layout: func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:   func(_, xdg string) string { return filepath.Join(xdg, "omp") },
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			home, xdg := t.TempDir(), t.TempDir()
			if testCase.layout != nil {
				testCase.layout(t, home, xdg)
			}
			root, profile, err := ProfileRoot(testCase.env(home, xdg), filepath.Join(home, "work"))
			if err != nil {
				t.Fatalf("ProfileRoot: %v", err)
			}
			if want := testCase.want(home, xdg); root != want {
				t.Errorf("root = %s, want %s", root, want)
			}
			if profile != testCase.profile {
				t.Errorf("profile = %q, want %q", profile, testCase.profile)
			}
		})
	}
}

// A profile name Oh My Pi cannot use is refused in its words, naming the name as given.
func TestProfileRootRefusesAProfileOhMyPiRefuses(t *testing.T) {
	for _, name := range []string{"Work", "..", "work.", "-work", "con", "nul.txt"} {
		_, _, err := ProfileRoot(map[string]string{"HOME": t.TempDir(), "OMP_PROFILE": name}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), `Invalid OMP profile "`+name+`"`) {
			t.Errorf("OMP_PROFILE=%q: err = %v, want the invalid-profile refusal naming it", name, err)
		}
		if _, valid := NormalizeProfile(name); valid {
			t.Errorf("NormalizeProfile(%q) accepts a name Oh My Pi refuses", name)
		}
	}
	for requested, want := range map[string]string{"": "", "default": "", " work ": "work", "a.b-c_d": "a.b-c_d"} {
		if got, valid := NormalizeProfile(requested); !valid || got != want {
			t.Errorf("NormalizeProfile(%q) = %q, %t, want %q, true", requested, got, valid, want)
		}
	}
}
