package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
)

// gitCredentialBundle writes a two-owner gh bundle, acme's write token and sjawhar's read token,
// and returns its root and acme's directory, which GH_CONFIG_DIR names.
func gitCredentialBundle(t *testing.T) (root, defaultDir string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "gh")
	expires := time.Now().Add(time.Hour)
	if _, err := ghconfig.WriteBundle(root, ghconfig.Bundle{Default: "acme", Owners: map[string]ghconfig.Rendered{
		"acme":    ghconfig.Render("ghs_acme_write", "implement", expires),
		"sjawhar": ghconfig.Render("ghs_sjawhar_read", "implement", expires),
	}}); err != nil {
		t.Fatal(err)
	}
	return root, ghconfig.OwnerDir(root, "acme")
}

func runGitCredentialHere(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	previous := gitCredentialStdin
	gitCredentialStdin = strings.NewReader(stdin)
	t.Cleanup(func() { gitCredentialStdin = previous })
	var out, errb bytes.Buffer
	code = run(context.Background(), append([]string{"legion", "git-credential"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

// `legion git-credential get` answers git's credential protocol from the hosts.yml of the owner the
// path git asks about names (credential.useHttpPath), lowercased, in that owner's directory of
// LEGION_GH_OWNERS_DIR, and from nothing else: it answers nothing, exit 1, naming why, for any host
// but github.com over https, a request with no path, a path whose owner is no GitHub login or one
// the bundle holds no directory for, an unset LEGION_GH_OWNERS_DIR, and a hosts.yml with no token.
// GH_CONFIG_DIR, the issue owner's write token, is never an answer for another owner's repository.
// store and erase write nothing and exit 0.
func TestGitCredentialAnswersFromTheOwnersDirectory(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty", ghconfig.HostsFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	both := map[string]string{"GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root}
	const answer = "username=x-access-token\npassword=%s\n"
	for _, tc := range []struct {
		name     string
		env      map[string]string
		args     []string
		stdin    string
		code     int
		stdout   string
		stderrIn string
	}{
		{
			name:   "another owner's directory",
			env:    both,
			args:   []string{"get"},
			stdin:  "protocol=https\nhost=github.com\npath=sjawhar/legion-smoke.git\n\n",
			stdout: strings.Replace(answer, "%s", "ghs_sjawhar_read", 1),
		},
		{
			name:   "an owner named in another case",
			env:    both,
			args:   []string{"get"},
			stdin:  "protocol=https\nhost=github.com\npath=SJawhar/legion-smoke\n",
			stdout: strings.Replace(answer, "%s", "ghs_sjawhar_read", 1),
		},
		{
			name:   "the issue owner's directory",
			env:    map[string]string{"LEGION_GH_OWNERS_DIR": root},
			args:   []string{"get"},
			stdin:  "protocol=https\nhost=github.com\npath=/acme/widgets.git\n\n",
			stdout: strings.Replace(answer, "%s", "ghs_acme_write", 1),
		},
		{
			name:     "an owner the bundle does not hold",
			env:      both,
			args:     []string{"get"},
			stdin:    "protocol=https\nhost=github.com\npath=Other/Thing\n\n",
			code:     1,
			stderrIn: `"other" is not an owner of this project`,
		},
		{
			name:     "a path that is no owner",
			env:      both,
			args:     []string{"get"},
			stdin:    "protocol=https\nhost=github.com\npath=../sjawhar/x\n\n",
			code:     1,
			stderrIn: `"..", which is no GitHub login`,
		},
		{
			name:     "no path (useHttpPath off)",
			env:      both,
			args:     []string{"get"},
			stdin:    "protocol=https\nhost=github.com\n\n",
			code:     1,
			stderrIn: "git sent no path",
		},
		{
			name:     "no owners directory",
			env:      map[string]string{"GH_CONFIG_DIR": defaultDir},
			args:     []string{"get"},
			stdin:    "protocol=https\nhost=github.com\npath=acme/widgets.git\n\n",
			code:     1,
			stderrIn: "LEGION_GH_OWNERS_DIR is unset",
		},
		{
			name:     "another host",
			env:      both,
			args:     []string{"get"},
			stdin:    "protocol=https\nhost=gitlab.com\npath=acme/widgets.git\n\n",
			code:     1,
			stderrIn: `host "gitlab.com"`,
		},
		{
			name:     "plain http",
			env:      both,
			args:     []string{"get"},
			stdin:    "protocol=http\nhost=github.com\npath=acme/widgets.git\n\n",
			code:     1,
			stderrIn: `protocol "http"`,
		},
		{
			name:     "a hosts.yml with no token",
			env:      both,
			args:     []string{"get"},
			stdin:    "protocol=https\nhost=github.com\npath=empty/thing.git\n\n",
			code:     1,
			stderrIn: "names no github.com oauth_token",
		},
		{
			name:  "store",
			env:   map[string]string{"GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root},
			args:  []string{"store"},
			stdin: "protocol=https\nhost=github.com\nusername=x-access-token\npassword=ghs_x\n\n",
		},
		{
			name:  "erase",
			env:   map[string]string{"GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root},
			args:  []string{"erase"},
			stdin: "protocol=https\nhost=github.com\n\n",
		},
		{
			name:     "an unknown operation",
			env:      map[string]string{"GH_CONFIG_DIR": defaultDir},
			args:     []string{"approve"},
			code:     2,
			stderrIn: "usage: legion git-credential get|store|erase",
		},
		{
			name:     "no operation",
			env:      map[string]string{"GH_CONFIG_DIR": defaultDir},
			code:     2,
			stderrIn: "usage: legion git-credential get|store|erase",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range []string{"GH_CONFIG_DIR", "LEGION_GH_OWNERS_DIR"} {
				if value, ok := tc.env[name]; ok {
					t.Setenv(name, value)
				} else {
					unsetenv(t, name)
				}
			}
			code, stdout, stderr := runGitCredentialHere(t, tc.stdin, tc.args...)
			if code != tc.code || stdout != tc.stdout || !strings.Contains(stderr, tc.stderrIn) {
				t.Fatalf("legion git-credential %v = exit %d, stdout %q, stderr %q; want exit %d, stdout %q, stderr containing %q",
					tc.args, code, stdout, stderr, tc.code, tc.stdout, tc.stderrIn)
			}
		})
	}
}

// Each operation the usage line names answers --help, exit 0, with its own usage, as the docs
// site's CLI generator reads every subcommand a usage line names (docs/site/generators/legion-cli.sh),
// and reads no request.
func TestGitCredentialOperationsAnswerHelp(t *testing.T) {
	for _, op := range []string{"get", "store", "erase"} {
		code, stdout, stderr := runGitCredentialHere(t, "protocol=https\nhost=github.com\n\n", op, "--help")
		if code != 0 || stdout != "" || !usageOf(stderr, "git-credential", op) {
			t.Errorf("legion git-credential %s --help = exit %d, stdout %q, stderr %q; want exit 0 and its usage", op, code, stdout, stderr)
		}
	}
}
