package tmux

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// parseShellEnvironment reads `show-environment -s` output as a whole, never line by line. Every
// fixture below is one of the shipped parser's (packages/daemon/src/daemon/__tests__/
// tmux.test.ts:128-253), including the reviewer's multi-line shapes: a PEM whose `=`-padded last
// line looks like `NAME=`, continuation lines reading `HOME;=x` and `key = value`, and a value
// that contains the escaped text `"; export HOME;`.
func TestParseShellEnvironmentNamesEveryEntry(t *testing.T) {
	for _, tc := range []struct {
		name string
		dump string
		want []string
	}{
		{
			name: "entries and markers, as tmux 3.7c prints them (tmux.test.ts:129-158)",
			dump: strings.Join([]string{
				`//registry.example/:_authToken="npm_abc"; export //registry.example/:_authToken;`,
				`A B="spaced"; export A B;`,
				`BASH_FUNC_git-fixup%%="() {  git commit --fixup=HEAD`,
				`}"; export BASH_FUNC_git-fixup%%;`,
				`EMPTY=""; export EMPTY;`,
				`HOME="/home/legion"; export HOME;`,
				`unset MARKED;`,
				`PATH="/full/bin:/usr/bin"; export PATH;`,
				`Q"N="q"; export Q"N;`,
				`TRAILBS="ends\\"; export TRAILBS;`,
				`X;="semi"; export X;;`,
				``,
			}, "\n"),
			want: []string{
				"//registry.example/:_authToken", "A B", "BASH_FUNC_git-fixup%%", "EMPTY", "HOME",
				"PATH", `Q"N`, "TRAILBS", "X;",
			},
		},
		{
			name: "multi-line values, a PEM among them (tmux.test.ts:160-189)",
			dump: strings.Join([]string{
				`BASH_FUNC_x%%="() {  echo \"hi \$1\";`,
				` local a=1`,
				`}"; export BASH_FUNC_x%%;`,
				`EVIL="a\"; export HOME;`,
				`b"; export EVIL;`,
				`QUOTEY="say \"hi\" \\ back \` + "`" + `tick\` + "`" + ` \$dollar"; export QUOTEY;`,
				`RAW_FAKE_PEM="-----BEGIN FAKE KEY-----`,
				`QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo=`,
				`ZmFrZS1rZXktYnl0ZXMtbm90LXJlYWw=`,
				`SE9NRTs9eA==`,
				`-----END FAKE KEY-----"; export RAW_FAKE_PEM;`,
				`TRICKY="l1`,
				`HOME;=x`,
				`key = value"; export TRICKY;`,
				``,
			}, "\n"),
			want: []string{"BASH_FUNC_x%%", "EVIL", "QUOTEY", "RAW_FAKE_PEM", "TRICKY"},
		},
		{
			name: "the C-locale rendering, newlines vis-encoded as _ (tmux.test.ts:191-195)",
			dump: `RAW_FAKE_PEM="-----BEGIN FAKE KEY-----_ZmFrZS1rZXktYnl0ZXMtbm90LXJlYWw=_-----END FAKE KEY-----"; export RAW_FAKE_PEM;` +
				"\n" + `TRICKY="l1_HOME;=x_key = value"; export TRICKY;` + "\n",
			want: []string{"RAW_FAKE_PEM", "TRICKY"},
		},
		{
			name: "a fresh session table: markers only",
			dump: "unset DISPLAY;\nunset SSH_AUTH_SOCK;\n",
			want: []string{},
		},
		{
			name: "no output",
			dump: "",
			want: []string{},
		},
		{
			name: "an entry named `unset X` is an entry (tmux.test.ts:204-213)",
			dump: "unset GONE;\nunset X=\"a;\nb\"; export unset X;\nA=\"1\"; export A;\n",
			want: []string{"unset X", "A"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseShellEnvironment(tc.dump, globalTable)
			if err != nil {
				t.Fatalf("parseShellEnvironment: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("names = %q, want %q", got, tc.want)
			}
		})
	}
}

// Anything else is refused naming the table and the byte offset, never the text there — which is
// a value (tmux.test.ts:214-252).
func TestParseShellEnvironmentRefusesOtherShapesWithoutQuotingThem(t *testing.T) {
	for _, tc := range []struct {
		dump  string
		table envTable
		want  string
	}{
		{"unset GONE\nA=\"leaked-value\"; export A;\n", globalTable, "tmux show-environment -s (global): malformed unset marker at byte 0, cannot read the table"},
		{"unset X;", sessionTable("legion-omp"), "tmux show-environment -s (session legion-omp): malformed unset marker at byte 0, cannot read the table"},
		{"unset ;\n", sessionTable("legion-omp"), "tmux show-environment -s (session legion-omp): malformed unset marker at byte 0, cannot read the table"},
		{"unset X\n", sessionTable("legion-omp"), "tmux show-environment -s (session legion-omp): malformed unset marker at byte 0, cannot read the table"},
		{`A="leaked-value`, globalTable, "tmux show-environment -s (global): unterminated value at byte 15, cannot read the table"},
		{"A=\"x\\n\"; export A;\n", globalTable, "tmux show-environment -s (global): unknown escape in value at byte 4, cannot read the table"},
		{"A=\"leaked\"; export B;\n", globalTable, "tmux show-environment -s (global): expected `\"; export NAME;` closing the entry at byte 9, cannot read the table"},
		{"A=leaked-value; export A;\n", globalTable, "tmux show-environment -s (global): expected `\"` after NAME= at byte 2, cannot read the table"},
		{"leaked-value\n", globalTable, "tmux show-environment -s (global): expected NAME= at byte 0, cannot read the table"},
	} {
		_, err := parseShellEnvironment(tc.dump, tc.table)
		if err == nil {
			t.Errorf("%q: parsed, want %q", tc.dump, tc.want)
			continue
		}
		if err.Error() != tc.want {
			t.Errorf("%q: error = %q, want %q", tc.dump, err, tc.want)
		}
		if strings.Contains(err.Error(), "leaked") {
			t.Errorf("%q: the refusal quotes the value: %q", tc.dump, err)
		}
	}
}

// The pane environment is the allow-list read verbatim from the daemon's own environment, less any
// credential-shaped name, with the four XDG base directories moved under `<state_dir>/home`
// (environment.ts:191-294; LEGION-206 P1).
func TestPaneEnvironment(t *testing.T) {
	environ := []string{
		"HOME=/home/legion",
		"PATH=/usr/local/bin:/usr/bin",
		"LANG=en_US.UTF-8",
		"LC_ALL=C.UTF-8",
		"TMUX_TMPDIR=/run/tmux",
		"OMP_PROFILE=legion",
		"MISE_DATA_DIR=/opt/mise",
		"XDG_CONFIG_HOME=/home/legion/.config",
		"XDG_RUNTIME_DIR=/run/user/1000",
		"https_proxy=http://proxy:3128",
		// Not on the list: a pane never sees them.
		"DISPATCH_TOKEN=leaked",
		"LEGION_DAEMON_URL=http://127.0.0.1:1",
		"SSH_AUTH_SOCK=/tmp/agent",
		"TMUX=/tmp/tmux-1000/default,1,0",
		"OMP_SESSION_ID=01a0",
		"ANTHROPIC_API_KEY=leaked",
		"MALFORMED",
	}
	got := PaneEnvironment(environ, "/var/lib/legion")
	want := map[string]string{
		"HOME":            "/home/legion",
		"PATH":            "/var/lib/legion/worker-bin:/usr/local/bin:/usr/bin",
		"LANG":            "en_US.UTF-8",
		"LC_ALL":          "C.UTF-8",
		"TMUX_TMPDIR":     "/run/tmux",
		"OMP_PROFILE":     "legion",
		"MISE_DATA_DIR":   "/opt/mise",
		"XDG_RUNTIME_DIR": "/run/user/1000",
		"https_proxy":     "http://proxy:3128",
		"XDG_CONFIG_HOME": "/var/lib/legion/home/.config",
		"XDG_CACHE_HOME":  "/var/lib/legion/home/.cache",
		"XDG_DATA_HOME":   "/var/lib/legion/home/.local/share",
		"XDG_STATE_HOME":  "/var/lib/legion/home/.local/state",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PaneEnvironment =\n%v\nwant\n%v", got, want)
	}
}

// The second line of defence behind the allow-list (environment.ts:247-260): a trailing
// credential segment, optionally followed by _FILE, or PRIVATE_KEY anywhere, in any case — and
// nothing that merely contains one of those words.
func TestIsSecretLikeName(t *testing.T) {
	for name, want := range map[string]bool{
		"ANTHROPIC_API_KEY":      true,
		"GH_TOKEN":               true,
		"LEGION_GRANT":           true,
		"db_password":            true,
		"CLIENT_SECRET_FILE":     true,
		"GITHUB_PAT":             true,
		"AWS_CREDENTIALS":        true,
		"MY_PRIVATE_KEY_PATH":    true,
		"X_PASSWD":               true,
		"TOKENIZER":              false,
		"X_PATH":                 false,
		"X_KEYBOARD":             false,
		"PATH":                   false,
		"HOME":                   false,
		"XDG_CONFIG_HOME":        false,
		"LEGION_BOOT_TOKEN_FILE": true,
	} {
		if got := isSecretLikeName(name); got != want {
			t.Errorf("isSecretLikeName(%q) = %v, want %v", name, got, want)
		}
	}
}

// writeExecutable makes an executable file for the resolution tests.
func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeMise serves the configured tool's install root. Its test keeps an executable named omp
// earlier on PATH, so ResolveOmpInvocation must not resolve through ordinary command lookup.
func writeMise(t *testing.T, path, tool, install string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = where ] && [ \"$2\" = \"" + tool + "\" ]; then\n" +
		"  printf '%s\\n' \"" + install + "\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// ResolveOmpInvocation is the shipped resolveOmpInvocation (environment.ts:312-341): an absolute
// LEGION_OMP_PATH is resolved and used directly; otherwise the configured invocation must be
// `mise x <tool> -- omp`, kept verbatim with mise pinned to its resolved absolute path, so mise
// still activates the tool inside the pane.
func TestResolveOmpInvocation(t *testing.T) {
	dir := t.TempDir()
	realOmp := filepath.Join(dir, "installs", "omp-18", "bin", "omp")
	writeExecutable(t, realOmp)
	linkedOmp := filepath.Join(dir, "bin", "omp")
	if err := os.MkdirAll(filepath.Dir(linkedOmp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realOmp, linkedOmp); err != nil {
		t.Fatal(err)
	}
	mise := filepath.Join(dir, "mise dir", "mise")
	writeMise(t, mise, "github:sjawhar/oh-my-pi@18.1.21", filepath.Dir(filepath.Dir(realOmp)))
	notExecutable := filepath.Join(dir, "plain")
	if err := os.WriteFile(notExecutable, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	const pinned = "mise x github:sjawhar/oh-my-pi@18.1.21 -- omp"

	for _, tc := range []struct {
		name       string
		invocation string
		env        map[string]string
		want       string
		refusal    string
	}{
		{
			name:       "LEGION_OMP_PATH names an executable: its real path, used directly",
			invocation: pinned,
			env:        map[string]string{"LEGION_OMP_PATH": linkedOmp},
			want:       realOmp,
		},
		{
			name:       "LEGION_OMP_PATH wins over any invocation shape",
			invocation: "omp",
			env:        map[string]string{"LEGION_OMP_PATH": linkedOmp},
			want:       realOmp,
		},
		{
			name:       "LEGION_OMP_PATH with no invocation configured",
			invocation: "",
			env:        map[string]string{"LEGION_OMP_PATH": linkedOmp},
			want:       realOmp,
		},
		{
			// The shipped daemon falls back to its pinned default; the Go daemon has no copy of
			// the pin, so it names the key instead.
			name:       "no invocation and no LEGION_OMP_PATH",
			invocation: "",
			env:        map[string]string{"PATH": filepath.Dir(mise)},
			refusal:    "omp_invocation is not set: set it to 'mise x <tool> -- omp', or set LEGION_OMP_PATH to an absolute executable path",
		},
		{
			name:       "LEGION_OMP_PATH relative",
			invocation: pinned,
			env:        map[string]string{"LEGION_OMP_PATH": "bin/omp"},
			refusal:    "LEGION_OMP_PATH must be an absolute executable path",
		},
		{
			name:       "LEGION_OMP_PATH not executable",
			invocation: pinned,
			env:        map[string]string{"LEGION_OMP_PATH": notExecutable},
			refusal:    "LEGION_OMP_PATH is not an executable: " + notExecutable,
		},
		{
			name:       "mise resolves the configured tool's binary, not an omp earlier on PATH",
			invocation: pinned,
			env:        map[string]string{"LEGION_MISE_PATH": mise, "PATH": filepath.Dir(linkedOmp)},
			want:       "'" + mise + "' x github:sjawhar/oh-my-pi@18.1.21 -- " + realOmp,
		},
		{
			name:       "mise resolves the configured tool's binary through PATH",
			invocation: pinned,
			env:        map[string]string{"PATH": filepath.Dir(linkedOmp) + ":" + filepath.Dir(mise)},
			want:       "'" + mise + "' x github:sjawhar/oh-my-pi@18.1.21 -- " + realOmp,
		},
		{
			name:       "any other invocation",
			invocation: "omp --mode rpc",
			env:        map[string]string{"PATH": filepath.Dir(mise)},
			refusal:    "OMP invocation must be 'mise x <tool> -- omp'. Set LEGION_OMP_PATH to an absolute executable path.",
		},
		{
			name:       "no mise anywhere",
			invocation: pinned,
			env:        map[string]string{"PATH": "/nonexistent"},
			refusal:    "Missing required daemon tool: mise (set LEGION_MISE_PATH to an absolute executable path)",
		},
		{
			name:       "LEGION_MISE_PATH relative",
			invocation: pinned,
			env:        map[string]string{"LEGION_MISE_PATH": "mise"},
			refusal:    "LEGION_MISE_PATH must be an absolute executable path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveOmpInvocation(tc.invocation, func(name string) string { return tc.env[name] })
			if tc.refusal != "" {
				if err == nil || err.Error() != tc.refusal {
					t.Fatalf("got (%q, %v), want refusal %q", got, err, tc.refusal)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveOmpInvocation: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
