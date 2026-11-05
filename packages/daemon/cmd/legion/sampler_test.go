package main

import (
	"bytes"
	"context"
	"net"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

// holdSamplerMemoryLimit puts the Go heap's limit back after a test that ran the sampler: it
// sets the limit for the process it is the whole of, and this test binary is not that process.
func holdSamplerMemoryLimit(t *testing.T) {
	t.Helper()
	before := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(before) })
}

// `legion sampler --help` is its usage and exit 0, which main_test.go's
// TestEveryCommandAnswersHelp requires of every command and the docs site's CLI generator reads
// each command's flags from.
func TestTheSamplerCommandAnswersHelpWithItsUsage(t *testing.T) {
	for _, help := range []string{"-h", "-help", "--help"} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"legion", "sampler", help}, &stdout, &stderr)
		if code != 0 || !usageOf(stderr.String(), "sampler") {
			t.Fatalf("legion sampler %s = %d, stderr %q; want exit 0 and `usage: legion sampler …`", help, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "-listen") || !strings.Contains(stderr.String(), "-node-name") {
			t.Errorf("legion sampler %s does not print its flags: %q", help, stderr.String())
		}
	}
}

// The two modes do not mix: an issue pod's sampler reads one pod's roles and a node's reads
// every pod of its node, and no flag of one is read by the other. Each is a usage error naming
// the flag, as `legion launcher` refuses --test-postgres without --roles-dir, and nothing is
// bound or read before it.
func TestTheSamplerCommandRefusesAFlagOfTheOtherMode(t *testing.T) {
	unsetenv(t, "NODE_NAME")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"a project in node mode", []string{"--node", "--node-name", "grade-1", "--project", "INFRA"},
			"legion sampler: --project is an issue pod's: it is refused with --node"},
		{"an issue in node mode", []string{"--node", "--node-name", "grade-1", "--issue", "INFRA-1208"},
			"legion sampler: --issue is an issue pod's: it is refused with --node"},
		{"a volumes file in node mode", []string{"--node", "--node-name", "grade-1", "--volumes-file", "/v.json"},
			"legion sampler: --volumes-file is an issue pod's: it is refused with --node"},
		{"a roles file in node mode", []string{"--node", "--node-name", "grade-1", "--roles-file", "/r.json"},
			"legion sampler: --roles-file is an issue pod's: it is refused with --node"},
		{"a cgroup root in node mode", []string{"--node", "--node-name", "grade-1", "--cgroup-root", "/sys/fs/cgroup/legion.slice"},
			"legion sampler: --cgroup-root is an issue pod's: it is refused with --node"},
		{"a node name in an issue pod", []string{"--project", "INFRA", "--issue", "INFRA-1208", "--node-name", "grade-1"},
			"legion sampler: --node-name is a node's: it is refused without --node"},
		{"namespaces in an issue pod", []string{"--project", "INFRA", "--issue", "INFRA-1208", "--namespaces", "grade"},
			"legion sampler: --namespaces is a node's: it is refused without --node"},
		{"excluded namespaces in an issue pod", []string{"--project", "INFRA", "--issue", "INFRA-1208", "--exclude-namespaces", "kube-system"},
			"legion sampler: --exclude-namespaces is a node's: it is refused without --node"},
		{"excluded pods in an issue pod", []string{"--project", "INFRA", "--issue", "INFRA-1208", "--exclude-pods", "grade-run-7"},
			"legion sampler: --exclude-pods is a node's: it is refused without --node"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := append([]string{"legion", "sampler"}, tc.args...)
			code := run(context.Background(), args, &stdout, &stderr)
			if code != 2 || strings.TrimSpace(stderr.String()) != tc.want {
				t.Fatalf("exit %d, stderr %q; want 2 and %q", code, stderr.String(), tc.want)
			}
		})
	}
}

// Each mode has its own required flags: an issue pod's series all carry the project and the
// issue, and a node's sampler is told which node it runs on — the name a DaemonSet projects as
// NODE_NAME, which this test takes out of the environment so the flag answers for itself.
func TestTheSamplerCommandRefusesWithoutWhatItsModeNeeds(t *testing.T) {
	unsetenv(t, "NODE_NAME")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no project or issue", []string{}, "--project and --issue are required"},
		{"no issue", []string{"--project", "INFRA"}, "--project and --issue are required"},
		{"no project", []string{"--issue", "INFRA-1208"}, "--project and --issue are required"},
		{"no node name", []string{"--node"}, "--node-name is required with --node"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := append([]string{"legion", "sampler"}, tc.args...)
			code := run(context.Background(), args, &stdout, &stderr)
			if code != 2 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("exit %d, stderr %q; want 2 naming %q", code, stderr.String(), tc.want)
			}
		})
	}
}

// --rules is the deployment's own kind rules, and a sampler started with rules it cannot read
// refuses rather than reporting every process of the pod under the built-in list alone: the
// operator who wrote them would never learn they were dropped.
func TestTheSamplerCommandRefusesRulesItCannotRead(t *testing.T) {
	unsetenv(t, "NODE_NAME")
	for _, tc := range []struct {
		name, rules, want string
	}{
		{"no JSON at all", "[{", "--rules is a JSON list"},
		{"a list of something else", `["go test"]`, "--rules is a JSON list"},
		{"a rule the engine refuses", `[{"program":"go*","kind":"build"}]`, "go*"},
		{"a kind no rule may answer", `[{"program":"gopls","kind":"agent"}]`, "agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{
				"legion", "sampler", "--project", "INFRA", "--issue", "INFRA-1208", "--rules", tc.rules,
			}, &stdout, &stderr)
			if code != 2 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("exit %d, stderr %q; want 2 naming %q", code, stderr.String(), tc.want)
			}
		})
	}
}

// A port another process holds is one line on stderr and a non-zero exit, never a panic: the
// launcher starts the sampler again five seconds later, and every role of the pod goes on
// running meanwhile (dispatch://INFRA-1208/spec#b-a-safe).
func TestTheSamplerCommandExitsOnAPortItCannotBind(t *testing.T) {
	holdSamplerMemoryLimit(t)
	unsetenv(t, "NODE_NAME")
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a loopback port: %v", err)
	}
	defer held.Close()

	// A sampler that took the port would serve until its context ended; the bound makes that
	// a failure of this test and not a hung run.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{
		"legion", "sampler", "--listen", held.Addr().String(),
		"--project", "INFRA", "--issue", "INFRA-1208",
		"--roles-file", "/var/run/legion/roles/sampler/roles.json",
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("the sampler exited 0 on a port it cannot bind; stderr %q", stderr.String())
	}
	line := strings.TrimSpace(stderr.String())
	if strings.Contains(line, "\n") {
		t.Errorf("stderr is %q, want one line", stderr.String())
	}
	if !strings.Contains(line, held.Addr().String()) {
		t.Errorf("stderr is %q, want the address %s named", line, held.Addr())
	}
}
