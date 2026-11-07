// request_detail_test.go drives the broker's own sentence about a refusal — the secret that
// refused the request and the rule that did, which the broker answers as detail on POST
// /v1/requests and GET /v1/requests/{id} — from the wire to every line of this CLI that says a
// request was refused: the request form's block, status's human output, and the exec form's
// denied line on stderr, which is the one an agent reads. A broker that recorded no detail leaves
// each of those lines exactly as it was.
package main

import (
	"context"
	"strings"
	"testing"
)

// TestTheClientDecodesARefusalsDetail: both request routes carry detail, so both of this client's
// methods decode it, and an answer carrying none (DENY_ME, as a broker from before the field
// answers every denial) decodes as nil rather than as an empty sentence no line could tell from a
// real one.
func TestTheClientDecodesARefusalsDetail(t *testing.T) {
	broker, _ := fakeBroker(t)
	defer broker.Close()
	c, signer, _, _ := renewalSession(t, broker.URL)
	ctx := context.Background()

	refused, _, err := c.CreateRequest(ctx, signer, []string{"DETAIL_DENIED"}, "need it", nil, "")
	if err != nil || refused.State != "denied" || refused.Detail == nil || *refused.Detail != refusedDetail {
		t.Fatalf("CreateRequest(DETAIL_DENIED) = %+v (detail %v), %v; want it denied with the detail %q",
			refused, refused.Detail, err, refusedDetail)
	}
	read, _, err := c.GetRequest(ctx, signer, refused.RequestID)
	if err != nil || read.State != "denied" || read.Detail == nil || *read.Detail != refusedDetail {
		t.Fatalf("GetRequest(%s) = %+v (detail %v), %v; want it denied with the detail %q",
			refused.RequestID, read, read.Detail, err, refusedDetail)
	}
	unexplained, _, err := c.CreateRequest(ctx, signer, []string{"DENY_ME"}, "need it", nil, "")
	if err != nil || unexplained.State != "denied" || unexplained.Detail != nil {
		t.Fatalf("CreateRequest(DENY_ME) = %+v (detail %v), %v; want it denied with no detail at all",
			unexplained, unexplained.Detail, err)
	}
}

// TestEveryLineAboutARefusalNamesTheSecretAndTheRule pins the three places this CLI prints a
// request's outcome, each with the broker's detail added to what it printed before: the request
// form's block gains a detail line, status prints one after what it already says, and the exec
// form's denied line on stderr ends with the reason, so an agent reading that one line learns
// which secret refused it and that asking again will not help. status --json stays the broker's
// own response body, byte for byte, which carries the field already. The denial the broker did
// not explain (DENY_ME, as every denial of a broker from before the field reads) closes it: each
// of those lines is then exactly the line it was, with no empty detail line and no dangling
// colon. Every case drives the one built binary, since a build costs more than all of them.
func TestEveryLineAboutARefusalNamesTheSecretAndTheRule(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)
	source := fakeSourceText(broker)

	t.Run("the request form", func(t *testing.T) {
		stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, "request", "DETAIL_DENIED")
		if want := "request_id: req-denied-listed\nstate: denied\nsource: " + source + "\ndetail: " + refusedDetail + "\n"; exit != exitDenied || stdout != want {
			t.Errorf("request DETAIL_DENIED = exit %d stdout %q (stderr %q); want exit %d stdout %q", exit, stdout, stderr, exitDenied, want)
		}
	})

	t.Run("status", func(t *testing.T) {
		stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, "status", "req-denied-listed")
		if want := "state: denied\nsource: " + source + "\ndetail: " + refusedDetail + "\n"; exit != 0 || stdout != want {
			t.Errorf("status req-denied-listed = exit %d stdout %q (stderr %q); want exit 0 stdout %q", exit, stdout, stderr, want)
		}
		stdout, stderr, exit = runAgentSecrets(t, binary, broker.URL, keyDir, nil, "status", "req-denied-listed", "--json")
		if exit != 0 || stdout != deniedDetailStatusJSON {
			t.Errorf("status req-denied-listed --json = exit %d stdout %q (stderr %q); want exit 0 and the broker's answer byte for byte %q",
				exit, stdout, stderr, deniedDetailStatusJSON)
		}
	})

	t.Run("the exec form", func(t *testing.T) {
		stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil,
			"DETAIL_DENIED", "--", "sh", "-c", "echo ran-the-child")
		if want := "agent-secrets: request req-denied-listed to " + source + " was denied: " + refusedDetail + "\n"; exit != exitDenied || stderr != want {
			t.Errorf("exec form of DETAIL_DENIED = exit %d stderr %q; want exit %d stderr %q", exit, stderr, exitDenied, want)
		}
		if strings.Contains(stdout, "ran-the-child") {
			t.Errorf("child ran for a refused request: stdout=%q", stdout)
		}
	})

	t.Run("a denial the broker did not explain", func(t *testing.T) {
		stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, "request", "DENY_ME")
		if want := "request_id: req-denied\nstate: denied\nsource: " + source + "\n"; exit != exitDenied || stdout != want {
			t.Errorf("request DENY_ME = exit %d stdout %q (stderr %q); want exit %d stdout %q", exit, stdout, stderr, exitDenied, want)
		}
		stdout, stderr, exit = runAgentSecrets(t, binary, broker.URL, keyDir, nil,
			"DENY_ME", "--", "sh", "-c", "echo ran-the-child")
		if want := "agent-secrets: request req-denied to " + source + " was denied\n"; exit != exitDenied || stderr != want {
			t.Errorf("exec form of DENY_ME = exit %d stderr %q; want exit %d stderr %q", exit, stderr, exitDenied, want)
		}
		if strings.Contains(stdout, "ran-the-child") {
			t.Errorf("child ran for a denied request: stdout=%q", stdout)
		}
	})
}
