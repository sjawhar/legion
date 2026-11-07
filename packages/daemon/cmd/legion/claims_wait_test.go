package main

// Every `legion claims` command answers within its --wait bound: dispatch://INFRA-714 R10. A
// suspend and a retry of a starting worker did not answer in 120 s and 300 s, and an operator with
// no answer at all cannot tell a daemon that is working from one that is wedged.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// slowDaemon never answers the claims routes: it holds every request until the test ends, as a
// daemon whose decision is behind a launch that takes minutes does.
func slowDaemon(t *testing.T) string {
	t.Helper()
	held := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-held:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(held); server.Close() })
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Port()
}

// Each subcommand takes a --wait that bounds the whole call, and a call that reaches it is
// refused with a reason naming the bound rather than hanging.
func TestEveryClaimsSubcommandAnswersWithinItsWait(t *testing.T) {
	port := slowDaemon(t)
	for name, args := range map[string][]string{
		"suspend": {"suspend", "--claim", string(architectClaim)},
		"resume":  {"resume", "--claim", string(architectClaim)},
		"stop":    {"stop", "--claim", string(architectClaim)},
		"close":   {"close", "--claim", string(architectClaim)},
		"retry":   {"retry", "--claim", string(architectClaim)},
		"deliver": {"deliver", "--claim", string(architectClaim), "--task", "carry on"},
		"list":    {"list"},
		"spawn":   {"spawn", "--tree", "LEGION-208", "--issue", "LEGION-208", "--role", "architect"},
	} {
		t.Run(name, func(t *testing.T) {
			var out, errb bytes.Buffer
			full := append([]string{"legion", "claims"}, args...)
			full = append(full, "--port", port, "--operator-token-file", operatorTokenFile(t), "--wait", "200ms")
			began := time.Now()

			code := run(context.Background(), full, &out, &errb)

			if took := time.Since(began); took > 10*time.Second {
				t.Fatalf("the command took %s, want it bounded by its --wait", took)
			}
			if code == 0 {
				t.Fatalf("the command exited 0 against a daemon that never answered; stdout %q", out.String())
			}
			if !strings.Contains(errb.String(), "--wait bound of 200ms") {
				t.Fatalf("stderr is %q, want a refusal naming the --wait bound", errb.String())
			}
		})
	}
}

// --wait defaults to one minute, which is the bound a command with no flag of its own runs under.
func TestClaimsWaitDefaultsToOneMinute(t *testing.T) {
	c := newClaimsCall("suspend", &bytes.Buffer{}, &bytes.Buffer{})
	if code, ok := c.parse([]string{"--operator-token-file", operatorTokenFile(t)}); !ok {
		t.Fatalf("parse exited %d, want it to accept a command with no --wait", code)
	}
	if *c.wait != time.Minute {
		t.Fatalf("--wait defaults to %s, want one minute", *c.wait)
	}
	if until := time.Until(c.until); until <= 0 || until > time.Minute {
		t.Fatalf("the call's bound is %s away, want it within the minute it was given", until)
	}
}

// The bound covers the whole call, not each request of it: a suspend the daemon answers 202 and
// then holds is refused once the command's own minute-or-less is gone, counted from its first
// request rather than restarted by each list it makes.
func TestTheWaitBoundsTheWholeSuspendCall(t *testing.T) {
	port, lists := heldSuspendDaemon(t, heldWorking)

	began := time.Now()
	code, out, errb := suspendAgainst(t, port, "400ms")

	if code == 0 {
		t.Fatalf("suspend exited 0 while its suspension stayed held; stdout %q", out)
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Fatalf("the command took %s, want it bounded by its --wait", took)
	}
	if !strings.Contains(errb, "--wait bound of 400ms") {
		t.Fatalf("stderr is %q, want a refusal naming the --wait bound", errb)
	}
	if got := lists(); got < 1 {
		t.Fatalf("the command listed the claims %d times, want it to have waited at all", got)
	}
}
