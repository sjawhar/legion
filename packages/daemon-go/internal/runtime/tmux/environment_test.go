package tmux

import (
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
		// The operator's session bus, which the keyring behind their hawk login answers on: a
		// tmux stage proof hands it to its model key command alone (scripts/e2e/lib/
		// install-model-gateway.sh), never to a pane.
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus",
		"MALFORMED",
	}
	got := PaneEnvironment(environ, "/var/lib/legion")
	want := map[string]string{
		"HOME":            "/home/legion",
		"PATH":            "/var/lib/legion/worker-bin:/var/lib/legion/bin:/usr/local/bin:/usr/bin",
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

func TestPaneEnvironmentPutsWorkerBinAndTheLauncherFirstExactlyOnce(t *testing.T) {
	stateDir := "/var/lib/legion"
	workerBin := filepath.Join(stateDir, "worker-bin")
	env := PaneEnvironment([]string{
		"PATH=" + workerBin + ":/usr/local/bin:" + filepath.Join(stateDir, "bin") + ":" + workerBin + ":/usr/bin",
	}, stateDir)
	if got, want := env["PATH"], workerBin+":"+filepath.Join(stateDir, "bin")+":/usr/local/bin:/usr/bin"; got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
	if strings.Count(env["PATH"], workerBin) != 1 {
		t.Fatalf("PATH = %q carries worker-bin more than once", env["PATH"])
	}
}
