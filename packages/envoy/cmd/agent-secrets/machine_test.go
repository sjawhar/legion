// packages/envoy/cmd/agent-secrets/machine_test.go

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/helper"
)

// TestMachineAndGrantUsage: each command names its verbs when given none or an unknown one, and a
// revoke takes exactly one id; none of them reaches a helper.
func TestMachineAndGrantUsage(t *testing.T) {
	binary := buildAgentSecrets(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"machine"}, "agent-secrets machine: a subcommand is required: login, login-status, list, revoke\n"},
		{[]string{"machine", "logout"}, "agent-secrets machine: unknown subcommand \"logout\"; the subcommands are login, login-status, list, revoke\n"},
		{[]string{"grant"}, "agent-secrets grant: a subcommand is required: list, revoke\n"},
		{[]string{"grant", "show"}, "agent-secrets grant: unknown subcommand \"show\"; the subcommands are list, revoke\n"},
		{[]string{"machine", "revoke"}, "agent-secrets machine revoke: exactly one CREDENTIAL_ID is required\n"},
		{[]string{"machine", "revoke", "a", "b"}, "agent-secrets machine revoke: exactly one CREDENTIAL_ID is required\n"},
		{[]string{"grant", "revoke"}, "agent-secrets grant revoke: exactly one GRANT_ID is required\n"},
		{[]string{"machine", "list", "extra"}, "agent-secrets machine list: unexpected argument \"extra\"\n"},
		{[]string{"grant", "list", "extra"}, "agent-secrets grant list: unexpected argument \"extra\"\n"},
	} {
		stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", t.TempDir(), []string{"AGENT_SECRETS_HELPER_SOCK=" + filepath.Join(t.TempDir(), "none.sock")}, tc.args...)
		if exit != exitUsageError || stdout != "" || stderr != tc.want {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q; want %d, %q", tc.args, exit, stdout, stderr, exitUsageError, tc.want)
		}
	}
}

// TestLauncherIsNoLongerACommand: the machine login commands moved to `machine`, with no alias, so
// `launcher login` is read as the form that runs a command and refused for want of one. The socket
// and runtime dir point nowhere, so a binary that still knew `launcher login` could reach no helper.
func TestLauncherIsNoLongerACommand(t *testing.T) {
	binary := buildAgentSecrets(t)
	_, stderr, exit := runAgentSecrets(t, binary, "http://unused", t.TempDir(), []string{"AGENT_SECRETS_HELPER_SOCK=" + filepath.Join(t.TempDir(), "none.sock"), "XDG_RUNTIME_DIR=" + t.TempDir()}, "launcher", "login")
	if exit != exitUsageError || strings.Contains(stderr, "launcher login") || !strings.Contains(stderr, "agent-secrets machine login") {
		t.Fatalf("launcher login: exit %d, stderr %q; want the usage, which names machine login and no launcher form", exit, stderr)
	}
}

// TestMachineAndGrantCommandsNeedTheHelper: a machine with no helper has no machine login to act
// as, so the commands refuse before calling anything, naming the socket they looked for; and they
// need the broker's address.
func TestMachineAndGrantCommandsNeedTheHelper(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock := filepath.Join(t.TempDir(), "none.sock")
	for _, args := range [][]string{{"machine", "list"}, {"machine", "revoke", "id"}, {"grant", "list"}, {"grant", "revoke", "id"}} {
		prefix := "agent-secrets " + args[0] + " " + args[1] + ": "
		_, stderr, exit := runAgentSecrets(t, binary, "http://unused", t.TempDir(), []string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, args...)
		want := prefix + "no agent-secrets-helper at " + sock + " (AGENT_SECRETS_HELPER_SOCK): machine and grant commands act under this machine's login, which its helper holds\n"
		if exit != exitUsageError || stderr != want {
			t.Fatalf("%v with no helper: exit %d, stderr %q; want %d, %q", args, exit, stderr, exitUsageError, want)
		}
		_, stderr, exit = runAgentSecrets(t, binary, "", t.TempDir(), []string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, args...)
		if want := prefix + "AGENT_SECRETS_URL is required\n"; exit != exitUsageError || stderr != want {
			t.Fatalf("%v with no AGENT_SECRETS_URL: exit %d, stderr %q; want %d, %q", args, exit, stderr, exitUsageError, want)
		}
	}
}

// TestMachineAndGrantCommandsOnAnOlderHelper: a helper from before sign-launcher answers it as an
// unknown op, and the command says the helper must be restarted on this release.
func TestMachineAndGrantCommandsOnAnOlderHelper(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock, reqs := fakeHelper(t, helper.Response{Code: helper.CodeBadRequest, Error: "unknown op sign-launcher"})
	stdout, stderr, exit := runAgentSecrets(t, binary, "https://broker.internal.example", t.TempDir(), []string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, "grant", "list")
	const want = "agent-secrets grant list: this machine's agent-secrets-helper is older than this client and cannot sign for machine and grant commands; restart it on this release\n"
	if exit != 1 || stdout != "" || stderr != want {
		t.Fatalf("grant list on an older helper: exit %d, stdout %q, stderr %q; want 1, %q", exit, stdout, stderr, want)
	}
	if req := <-reqs; req.Op != "sign-launcher" || req.Method != "GET" || req.URL != "https://broker.internal.example/v1/operator/grants" {
		t.Fatalf("the helper was asked %+v, want sign-launcher for GET /v1/operator/grants", req)
	}
}

// TestLauncherSignerRequestsNoSecrets: a machine login asks the broker for no secret, so the
// launcher signer builds no request object.
func TestLauncherSignerRequestsNoSecrets(t *testing.T) {
	if _, err := (&launcherSigner{sock: "unused"}).SignRequestObject("https://broker.internal.example", []string{"X"}, "why"); err == nil {
		t.Fatal("launcherSigner signed a request object")
	}
}
