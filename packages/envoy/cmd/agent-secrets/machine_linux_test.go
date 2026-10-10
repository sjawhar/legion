// packages/envoy/cmd/agent-secrets/machine_linux_test.go
//go:build linux

// The machine and grant commands act under this machine's login: the CLI asks the helper for a
// launcher proof (sign-launcher) and calls the broker's operator routes with it. These tests drive
// the real CLI against a real helper and a real broker (brokertest.NewRig), as
// relogin_linux_test.go does. Linux-only for the real helper's pidfd pinning.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

// tableRows splits a table the CLI printed into its header's columns and each row's fields.
func tableRows(t *testing.T, stdout string) (header []string, rows [][]string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	header = strings.Fields(lines[0])
	for _, line := range lines[1:] {
		rows = append(rows, strings.Fields(line))
	}
	return header, rows
}

// loggedInElsewhere logs another machine of the rig's operator in, with a key of its own, and
// returns its helper.Broker and the launcher credential its approval minted.
func loggedInElsewhere(t *testing.T, rig *brokertest.Rig, host string) (*helper.Broker, string) {
	t.Helper()
	b := &helper.Broker{URL: rig.URL, OperatorFile: rig.OperatorFile, HTTP: http.DefaultClient}
	code, err := b.Login(context.Background(), host)
	if err != nil {
		t.Fatalf("log %s in: %v", host, err)
	}
	id := rig.DecideMachineLogin(t, code, true)
	deadline := time.Now().Add(20 * time.Second)
	for !b.HasCredential() {
		if time.Now().After(deadline) {
			t.Fatalf("%s's login never issued: %+v", host, b.LoginStatus())
		}
		time.Sleep(50 * time.Millisecond)
	}
	return b, id
}

// TestMachineListAndRevokeUnderTheMachinesLogin: once this machine is logged in, `machine list`
// shows its login and another of the operator's machines, and `--json` prints the broker's body
// verbatim, which with no service login on the rig is exactly what Dispatch's machine-login page
// reads for the operator. `machine revoke` ends the other machine's login, which the broker then
// refuses, and prints `revoked <id>`. Revoking this machine's own login, its id typed in upper
// case (the broker reads any case), warns that it ends this machine's access, and the broker
// refuses the next command, which says to log the machine in again.
func TestMachineListAndRevokeUnderTheMachinesLogin(t *testing.T) {
	rig := brokertest.NewRig(t)
	binary := buildAgentSecrets(t)
	_, sock := serveRealHelper(t, rig.URL, rig.Operator)
	if code, exit, stderr := machineLoginThen(t, rig, binary, sock, "approve", nil); exit != 0 {
		t.Fatalf("machine login (code %s): exit %d, stderr %q", code, exit, stderr)
	}
	machine := func(args ...string) (string, string, int) {
		t.Helper()
		return runAgentSecrets(t, binary, rig.URL, t.TempDir(), []string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, append([]string{"machine"}, args...)...)
	}
	laptop, laptopID := loggedInElsewhere(t, rig, "example-host-laptop")

	stdout, stderr, exit := machine("list")
	if exit != 0 || !strings.Contains(stdout, "testhost") { // serveRealHelper's Hostname is the HOST column
		t.Fatalf("machine list: %d %q %q", exit, stdout, stderr)
	}
	header, rows := tableRows(t, stdout)
	if want := []string{"CREDENTIAL_ID", "HOST", "APPROVED_BY", "ISSUED", "EXPIRES", "STATE"}; !slices.Equal(header, want) || len(rows) != 2 {
		t.Fatalf("machine list = %q, want the header %v and two rows", stdout, want)
	}
	if laptopRow := rows[0]; laptopRow[0] != laptopID || laptopRow[1] != "example-host-laptop" || laptopRow[2] != rig.Operator || laptopRow[5] != "ok" {
		t.Fatalf("machine list's first row = %v, want the laptop's login %s, newest first", laptopRow, laptopID)
	}
	own := rows[1][0]
	if rows[1][1] != "testhost" || rows[1][5] != "ok" {
		t.Fatalf("machine list's second row = %v, want this machine's login", rows[1])
	}

	stdout, stderr, exit = machine("list", "--json")
	status, page := rig.UI(t, http.MethodGet, "/v1/launcher-credentials?approver="+url.QueryEscape(rig.Operator), nil)
	if exit != 0 || status != http.StatusOK || stdout != string(page) {
		t.Fatalf("machine list --json: exit %d, stdout %q, stderr %q; want Dispatch's page's body %q", exit, stdout, stderr, page)
	}

	if stdout, stderr, exit := machine("revoke", laptopID); exit != 0 || stdout != "revoked "+laptopID+"\n" || stderr != "" {
		t.Fatalf("machine revoke the laptop: exit %d, stdout %q, stderr %q", exit, stdout, stderr)
	}
	boxKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&boxKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := laptop.EnrollBox(context.Background(), "laptop-box", thumbprint, nil); err == nil || laptop.HasCredential() {
		t.Fatalf("the laptop enrolling after its login was revoked: %v, credential held %v; want the broker's refusal", err, laptop.HasCredential())
	}
	if stdout, _, _ := machine("list"); strings.Contains(stdout, laptopID) || !strings.Contains(stdout, own) {
		t.Fatalf("machine list after revoking the laptop = %q, want this machine's login alone", stdout)
	}

	const ownWarning = "agent-secrets machine revoke: revoking this machine's own login ends every session it enrolled, this one's broker access included\n"
	if stdout, stderr, exit := machine("revoke", strings.ToUpper(own)); exit != 0 || stdout != "revoked "+strings.ToUpper(own)+"\n" || stderr != ownWarning {
		t.Fatalf("machine revoke this machine's own login in upper case: exit %d, stdout %q, stderr %q; want %q", exit, stdout, stderr, ownWarning)
	}
	const refused = "agent-secrets machine list: the launcher credential is not valid (LAUNCHER_INVALID); this machine's login is expired or revoked (run: agent-secrets machine login)\n"
	if stdout, stderr, exit := machine("list"); exit != 1 || stdout != "" || stderr != refused {
		t.Fatalf("machine list after revoking this machine's own login: exit %d, stdout %q, stderr %q; want 1, %q", exit, stdout, stderr, refused)
	}
}

// TestGrantListAndRevokeUnderTheMachinesLogin: a box this machine enrolled gets an agent-tier
// secret automatically. `grant list` shows that grant, and `--json` prints exactly what Dispatch's
// Live grants page reads for the operator. `grant revoke` ends it, as the operator's revoke on that
// page does: the box can no longer read the value, and its next request for the secret waits for
// approval.
func TestGrantListAndRevokeUnderTheMachinesLogin(t *testing.T) {
	rig := brokertest.NewRig(t)
	binary := buildAgentSecrets(t)
	_, sock := serveRealHelper(t, rig.URL, rig.Operator)
	if code, exit, stderr := machineLoginThen(t, rig, binary, sock, "approve", nil); exit != 0 {
		t.Fatalf("machine login (code %s): exit %d, stderr %q", code, exit, stderr)
	}
	grant := func(args ...string) (string, string, int) {
		t.Helper()
		return runAgentSecrets(t, binary, rig.URL, t.TempDir(), []string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, append([]string{"grant"}, args...)...)
	}
	boxKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&boxKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	runtimeID := "grant-box-" + t.Name()
	enrolled, err := helper.Call(sock, helper.Request{Op: "enroll-box", RuntimeID: runtimeID, Thumbprint: thumbprint}, 10*time.Second)
	if err != nil || !enrolled.OK {
		t.Fatalf("enroll-box: %+v %v", enrolled, err)
	}
	keyDir := writeKeyDir(t, boxKey, enrolled.EnrollmentID)
	stdout, stderr, exit := runAgentSecrets(t, binary, rig.URL, keyDir, nil, "request", "TEST_AUTO_SECRET", "--json")
	var granted RequestResult
	if err := json.Unmarshal([]byte(stdout), &granted); exit != 0 || err != nil || granted.State != "granted" || granted.GrantID == nil {
		t.Fatalf("request TEST_AUTO_SECRET from the box: exit %d, stdout %q, stderr %q; want it granted at once", exit, stdout, stderr)
	}
	grantID := *granted.GrantID

	stdout, stderr, exit = grant("list")
	header, rows := tableRows(t, stdout)
	if want := []string{"GRANT_ID", "SECRETS", "GRANTED", "APPROVER", "SESSION", "OPERATOR", "EXPIRES"}; exit != 0 || !slices.Equal(header, want) || len(rows) != 1 {
		t.Fatalf("grant list: exit %d, stdout %q, stderr %q; want the header %v and one row", exit, stdout, stderr, want)
	}
	if row := rows[0]; !slices.Equal(row[:6], []string{grantID, "TEST_AUTO_SECRET", "automatic", "-", "box/" + runtimeID, rig.Operator}) {
		t.Fatalf("grant list's row = %v, want the box's automatic grant %s", row, grantID)
	}

	stdout, stderr, exit = grant("list", "--json")
	status, page := rig.UI(t, http.MethodGet, "/v1/grants?approver="+url.QueryEscape(rig.Operator), nil)
	if exit != 0 || status != http.StatusOK || stdout != string(page) {
		t.Fatalf("grant list --json: exit %d, stdout %q, stderr %q; want the Live grants page's body %q", exit, stdout, stderr, page)
	}

	if stdout, stderr, exit := grant("revoke", grantID); exit != 0 || stdout != "revoked "+grantID+"\n" || stderr != "" {
		t.Fatalf("grant revoke: exit %d, stdout %q, stderr %q", exit, stdout, stderr)
	}
	if _, err := newClient(rig.URL).GrantValues(context.Background(), &fileSigner{key: boxKey, enrollmentID: enrolled.EnrollmentID}, grantID); err == nil {
		t.Fatal("the box read the revoked grant's values")
	}
	if stdout, _, _ := grant("list"); strings.Contains(stdout, grantID) {
		t.Fatalf("grant list after the revoke = %q, want the grant gone", stdout)
	}
	if stdout, stderr, exit := runAgentSecrets(t, binary, rig.URL, keyDir, nil, "request", "TEST_AUTO_SECRET", "--json"); exit != exitPending {
		t.Fatalf("the box asking again after the operator's revoke: exit %d, stdout %q, stderr %q; want it waiting for approval (%d)", exit, stdout, stderr, exitPending)
	}
	if stdout, stderr, exit := grant("revoke", uuid.NewString()); exit != 1 || stdout != "" || !strings.Contains(stderr, "(NOT_FOUND)") {
		t.Fatalf("grant revoke of an unknown grant: exit %d, stdout %q, stderr %q; want 1 naming NOT_FOUND", exit, stdout, stderr)
	}
}

// TestMachineAndGrantCommandsRefusedInsideASession: a registered session acts on itself alone, so
// the helper refuses it a launcher proof, and every machine and grant command run from inside one
// says so and exits 1.
func TestMachineAndGrantCommandsRefusedInsideASession(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock := realHelper(t)
	for _, args := range [][]string{{"machine", "list"}, {"machine", "revoke", uuid.NewString()}, {"grant", "list"}, {"grant", "revoke", uuid.NewString()}} {
		code, stdout, stderr := runInSession(t, binary, sock, args...)
		prefix := "agent-secrets " + args[0] + " " + args[1] + ": IN_SESSION: pid "
		if code != 1 || stdout != "" || !strings.HasPrefix(stderr, prefix) || !strings.HasSuffix(stderr, "is inside a registered host session, which acts on itself alone; run machine and grant commands from your own shell\n") {
			t.Fatalf("%v inside a session: exit %d, stdout %q, stderr %q; want 1 and IN_SESSION", args, code, stdout, stderr)
		}
	}
}

// TestMachineAndGrantCommandsOnAMachineNotLoggedIn: from the operator's own shell on a machine
// whose helper holds no machine login, every machine and grant command says to log it in.
func TestMachineAndGrantCommandsOnAMachineNotLoggedIn(t *testing.T) {
	binary := buildAgentSecrets(t)
	_, sock := serveRealHelper(t, "http://127.0.0.1:1", "ada@example.com")
	for _, args := range [][]string{{"machine", "list"}, {"machine", "revoke", uuid.NewString()}, {"grant", "list"}, {"grant", "revoke", uuid.NewString()}} {
		stdout, stderr, exit := runAgentSecrets(t, binary, "http://127.0.0.1:1", t.TempDir(), []string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, args...)
		want := "agent-secrets " + args[0] + " " + args[1] + ": this machine is not logged in to the secrets broker; run: agent-secrets machine login\n"
		if exit != 1 || stdout != "" || stderr != want {
			t.Fatalf("%v with no machine login: exit %d, stdout %q, stderr %q; want 1, %q", args, exit, stdout, stderr, want)
		}
	}
}
