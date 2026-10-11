package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
)

// `legion status <issue> <status>` is the operator's: without --operator-token-file it is a usage
// error naming the flag, and the daemon is never reached — no grant is read from the environment.
func TestStatusIssueRequiresTheOperatorTokenFile(t *testing.T) {
	d := newControllerDaemon(t)
	t.Setenv("LEGION_DAEMON_URL", d.url)
	code, out, errb := issueStatus(t, "LEGION-208", "todo")
	if want := "legion status: --operator-token-file is required"; code != 2 || out != "" || !strings.Contains(errb, want) {
		t.Fatalf("legion status = %d, stdout %q, stderr %q; want a usage error saying %q", code, out, errb, want)
	}
	if n := len(d.requests()); n != 0 {
		t.Fatalf("%d requests reached the daemon, want none", n)
	}
}

// issueStatus runs `legion status <args>` and holds it to the rule every run keeps: the operator
// token is in neither stream.
func issueStatus(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), append([]string{"legion", "status"}, args...), &out, &errb)
	for name, stream := range map[string]string{"stdout": out.String(), "stderr": errb.String()} {
		if strings.Contains(stream, controllerOperatorToken) {
			t.Fatalf("legion status %v printed the operator token on %s: %q", args, name, stream)
		}
	}
	return code, out.String(), errb.String()
}

// From an operator shell, the operator's bearer buys a controller grant through the grants
// route's operator form, and the grant sets the status: two requests, the bearer on the first
// only.
func TestStatusWithTheOperatorTokenFileMintsAGrantAndSetsTheStatus(t *testing.T) {
	d := newControllerDaemon(t)
	tokenFile := writeFile(t, "operator-token", controllerOperatorToken+"\n")
	t.Setenv("LEGION_DAEMON_URL", "http://127.0.0.1:1")

	code, out, errb := issueStatus(t, "LEGSMOKE-3", "backlog", "--operator-token-file", tokenFile, "--port", strconv.Itoa(d.port))
	if code != 0 || errb != "" {
		t.Fatalf("legion status = %d, stderr %q; want 0", code, errb)
	}
	if out != "{}\n" {
		t.Errorf("stdout = %q, want the daemon's answer", out)
	}
	requests := d.requests()
	if len(requests) != 2 {
		t.Fatalf("%d requests reached the daemon, want the grant and the status", len(requests))
	}
	grant, status := requests[0], requests[1]
	if grant.method != http.MethodPost || grant.path != "/legion/v1/grants" || grant.authorization != "Bearer "+controllerOperatorToken ||
		string(grant.body) != "{}" || grant.status != http.StatusOK {
		t.Fatalf("grant request = %s %s auth %q body %q → %d", grant.method, grant.path, grant.authorization, grant.body, grant.status)
	}
	var minted api.GrantResponse
	if err := json.Unmarshal(grant.answer, &minted); err != nil || minted.GrantID == "" {
		t.Fatalf("grant answer %s: %v", grant.answer, err)
	}
	var sent api.IssueStatusRequest
	decodeStrict(t, status.body, &sent)
	if status.path != "/legion/v1/issues/status" || status.authorization != "" ||
		sent != (api.IssueStatusRequest{GrantID: minted.GrantID, Issue: "LEGSMOKE-3", Status: "backlog"}) {
		t.Fatalf("status request = %s auth %q body %+v; want the minted grant, no bearer", status.path, status.authorization, sent)
	}
	if got := d.dispatch.writes; !slices.Equal(got, []string{"LEGSMOKE-3=backlog"}) {
		t.Fatalf("Dispatch was asked for %v, want LEGSMOKE-3=backlog", got)
	}
}

// --config finds the daemon the way `legion state --config` does.
func TestStatusWithTheOperatorTokenFileFindsTheDaemonFromTheConfiguration(t *testing.T) {
	d := newControllerDaemon(t)
	tokenFile := writeFile(t, "operator-token", controllerOperatorToken+"\n")
	code, _, errb := issueStatus(t, "LEGSMOKE-3", "icebox", "--operator-token-file", tokenFile, "--config", legionConfig(t, "demo", d.port))
	if code != 0 {
		t.Fatalf("legion status = %d, stderr %q", code, errb)
	}
	if got := d.dispatch.writes; !slices.Equal(got, []string{"LEGSMOKE-3=icebox"}) {
		t.Fatalf("Dispatch was asked for %v, want LEGSMOKE-3=icebox", got)
	}
}

// The wrong bearer is the daemon's refusal, printed as every operator command prints one; no
// status request follows.
func TestStatusWithAWrongOperatorTokenIsRefusedAndSetsNothing(t *testing.T) {
	d := newControllerDaemon(t)
	tokenFile := writeFile(t, "operator-token", "not-the-operator-token\n")
	code, _, errb := issueStatus(t, "LEGSMOKE-3", "backlog", "--operator-token-file", tokenFile, "--port", strconv.Itoa(d.port))
	if code != 1 || errb != "legion status: the daemon answered 403 Forbidden: Invalid operator token\n" {
		t.Fatalf("legion status = %d, stderr %q; want the 403 refusal", code, errb)
	}
	if requests := d.requests(); len(requests) != 1 || requests[0].path != "/legion/v1/grants" {
		t.Fatalf("requests = %d, want only the refused grant request", len(requests))
	}
	if len(d.dispatch.writes) != 0 {
		t.Fatalf("Dispatch was asked for %v", d.dispatch.writes)
	}
}

// A token file it cannot read is refused naming the flag and the path, before the daemon is
// reached.
func TestStatusRefusesAnOperatorTokenFileItCannotRead(t *testing.T) {
	d := newControllerDaemon(t)
	absent := filepath.Join(t.TempDir(), "absent")
	code, _, errb := issueStatus(t, "LEGSMOKE-3", "backlog", "--operator-token-file", absent, "--port", strconv.Itoa(d.port))
	if code != 1 || !strings.Contains(errb, fmt.Sprintf("--operator-token-file names %s, which could not be read", absent)) {
		t.Fatalf("legion status = %d, stderr %q", code, errb)
	}
	if n := len(d.requests()); n != 0 {
		t.Fatalf("%d requests reached the daemon, want none", n)
	}
}

// The operator token file is held to `legion controller start`'s rule: one its group or others
// can read is refused naming the path and the mode, and the daemon is never reached.
func TestStatusRefusesAGroupReadableOperatorTokenFile(t *testing.T) {
	d := newControllerDaemon(t)
	tokenFile := writeFile(t, "operator-token", controllerOperatorToken+"\n")
	if err := os.Chmod(tokenFile, 0o640); err != nil {
		t.Fatal(err)
	}
	code, _, errb := issueStatus(t, "LEGSMOKE-3", "backlog", "--operator-token-file", tokenFile, "--port", strconv.Itoa(d.port))
	if want := "--operator-token-file " + tokenFile + " is readable by its group or others (mode 0640); chmod 0600 it"; code != 1 || !strings.Contains(errb, want) {
		t.Fatalf("legion status = %d, stderr %q; want 1 and %q", code, errb, want)
	}
	if n := len(d.requests()); n != 0 {
		t.Fatalf("%d requests reached the daemon, want none", n)
	}
}

// Each usage error is its own, not the missing flag's: a status outside the three, a stray
// argument, and a flag without its value are refused with exit 2 even with the token file given.
func TestStatusIssueUsage(t *testing.T) {
	tokenFile := writeFile(t, "operator-token", controllerOperatorToken+"\n")
	for _, args := range [][]string{
		{"LEGSMOKE-3", "done", "--operator-token-file", tokenFile},
		{"LEGSMOKE-3", "backlog", "extra", "--operator-token-file", tokenFile},
		{"LEGSMOKE-3", "backlog", "--operator-token-file"},
	} {
		code, _, errb := issueStatus(t, args...)
		if code != 2 {
			t.Errorf("legion status %v = %d, stderr %q; want a usage error", args, code, errb)
		}
	}
}
