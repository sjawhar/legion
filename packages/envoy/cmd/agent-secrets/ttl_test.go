package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/record"
)

// ttlBroker is a fake broker for --ttl: POST /v1/requests verifies the signed request object
// against its own URL, records the lifetime it signed, and answers by the first secret's name —
// TTL_GRANTED and TTL_PENDING echo the lifetime, while OLD_GRANTED, OLD_PENDING, OLD_COALESCED,
// OLD_DENIED and OLD_APPROVED_GRANTED answer as a broker from before lifetime_seconds (no such
// field). OLD_APPROVED_GRANTED is an existing human-approved grant reused by that broker, and
// OLD_COALESCED a request an earlier call left pending, which that broker answers `coalesced`
// whatever --ttl asked; the client must leave both alone when it refuses the mismatched lifetime.
// OTHER_GRANTED names a lifetime of its own. It serves values for every grant, and counts each
// grant revoke and request cancel.
type ttlBroker struct {
	srv *httptest.Server

	mu        sync.Mutex
	posts     int
	lifetimes []int // each POST's signed lifetime, 0 for none
	verifyErr error
	revoked   []string
	cancelled []string
}

func newTTLBroker(t *testing.T) *ttlBroker {
	t.Helper()
	b := &ttlBroker{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Request string `json:"request"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		obj, err := record.VerifyRequestObject(body.Request, b.srv.URL, time.Minute, time.Now())
		lifetime := 0
		if obj.LifetimeSeconds != nil {
			lifetime = *obj.LifetimeSeconds
		}
		b.mu.Lock()
		b.posts++
		b.lifetimes = append(b.lifetimes, lifetime)
		if err != nil {
			b.verifyErr = err
		}
		b.mu.Unlock()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		name := obj.Details[0].Identifier
		answer := map[string]any{
			"request_id": "req-" + strings.ToLower(name), "state": "granted",
			"secrets":  []map[string]string{{"name": name, "decision": "automatic"}},
			"grant_id": "grant-" + strings.ToLower(name), "record_id": nil, "approver": nil,
		}
		var echoed any
		if obj.LifetimeSeconds != nil {
			echoed = *obj.LifetimeSeconds
		}
		switch name {
		case "TTL_GRANTED":
			answer["lifetime_seconds"] = echoed
		case "TTL_PENDING", "OLD_PENDING", "OLD_COALESCED":
			answer["state"], answer["grant_id"], answer["record_id"], answer["approver"] = "pending", nil, "rec-1", "ada@example.com"
			answer["secrets"] = []map[string]string{{"name": name, "decision": "approval"}}
			if name == "TTL_PENDING" {
				answer["lifetime_seconds"] = echoed
			}
			if name == "OLD_COALESCED" {
				answer["coalesced"] = true
			}
		case "OLD_GRANTED":
		case "OLD_APPROVED_GRANTED":
			answer["record_id"] = "rec-old-approved"
		case "OLD_DENIED":
			answer["state"], answer["grant_id"] = "denied", nil
			answer["secrets"] = []map[string]string{{"name": name, "decision": "deny"}}
		case "OTHER_GRANTED":
			answer["lifetime_seconds"] = 43200
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		writeJSON(w, answer)
	})
	mux.HandleFunc("POST /v1/grants/{id}/values", func(w http.ResponseWriter, r *http.Request) {
		name := strings.ToUpper(strings.TrimPrefix(r.PathValue("id"), "grant-"))
		writeJSON(w, map[string]any{"values": map[string]string{name: "value-of-" + name}, "expires_at": nil})
	})
	mux.HandleFunc("POST /v1/grants/{id}/revoke", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.revoked = append(b.revoked, r.PathValue("id"))
		b.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/requests/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.cancelled = append(b.cancelled, r.PathValue("id"))
		b.mu.Unlock()
		writeJSON(w, map[string]any{"state": "cancelled"})
	})
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

// take returns what the broker recorded since the last take, and forgets it.
func (b *ttlBroker) take() (posts int, lifetimes []int, verifyErr error, revoked, cancelled []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	posts, lifetimes, verifyErr, revoked, cancelled = b.posts, b.lifetimes, b.verifyErr, b.revoked, b.cancelled
	b.posts, b.lifetimes, b.verifyErr, b.revoked, b.cancelled = 0, nil, nil, nil, nil
	return
}

// TestTTLReachesTheSignedRequestObject: `--ttl 2m` on `request` and on the exec form signs
// lifetime_seconds 120 into the request object the broker verifies, and a broker that gives the
// grant that lifetime is answered as before: request prints its id and state, and the exec form
// runs the command with the secret. With no --ttl the object carries no lifetime claim at all
// (a claim of 0 would not verify), the request for a grant that lasts until the session ends.
func TestTTLReachesTheSignedRequestObject(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newTTLBroker(t)
	keyDir := newKeyDir(t)
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"request --ttl 2m", []string{"request", "TTL_GRANTED", "--ttl", "2m"}, 120},
		{"request --ttl=90s before the name", []string{"request", "--ttl=90s", "TTL_GRANTED"}, 90},
		{"exec form --ttl 2m", []string{"TTL_GRANTED", "--ttl", "2m", "--", "sh", "-c", `echo "ran $TTL_GRANTED"`}, 120},
		{"request without --ttl", []string{"request", "TTL_GRANTED"}, 0},
		{"exec form without --ttl", []string{"TTL_GRANTED", "--", "sh", "-c", `echo "ran $TTL_GRANTED"`}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil, tc.args...)
			posts, lifetimes, verifyErr, revoked, cancelled := b.take()
			if verifyErr != nil {
				t.Fatalf("the broker could not verify the request object: %v", verifyErr)
			}
			if posts != 1 || lifetimes[0] != tc.want {
				t.Fatalf("signed lifetimes = %v over %d requests, want one request signing %d", lifetimes, posts, tc.want)
			}
			if exit != 0 || len(revoked) != 0 || len(cancelled) != 0 {
				t.Fatalf("exit %d, revoked %v, cancelled %v (stdout %q, stderr %q); want exit 0 ending nothing", exit, revoked, cancelled, stdout, stderr)
			}
			want := "request_id: req-ttl_granted\nstate: granted\n"
			if tc.args[0] != "request" {
				want = "ran value-of-TTL_GRANTED\n"
			}
			if stdout != want {
				t.Fatalf("stdout = %q, want %q", stdout, want)
			}
		})
	}
}

// TestABadTTLExitsTwo: a --ttl that is not a positive whole number of seconds, or is past the most
// the broker records, is a usage error (exit 2) on `request` and on the exec form, and nothing
// reaches the broker or runs.
func TestABadTTLExitsTwo(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newTTLBroker(t)
	keyDir := newKeyDir(t)
	for _, ttl := range []string{"0", "0s", "-2m", "1500ms", "2", "two minutes", "600000h"} {
		for _, args := range [][]string{
			{"request", "TTL_GRANTED", "--ttl", ttl},
			{"TTL_GRANTED", "--ttl", ttl, "--", "sh", "-c", "echo ran-the-child"},
		} {
			t.Run(strings.Join(args[:3], " ")+" "+ttl, func(t *testing.T) {
				stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil, args...)
				if posts, _, _, _, _ := b.take(); exit != exitUsageError || posts != 0 || strings.Contains(stdout, "ran-the-child") {
					t.Fatalf("exit %d after %d broker requests (stdout %q, stderr %q); want exit %d with nothing asked or run", exit, posts, stdout, stderr, exitUsageError)
				}
				if !strings.Contains(stderr, "ttl") {
					t.Fatalf("stderr = %q, want it to name --ttl", stderr)
				}
			})
		}
	}
}

// TestATTLTheBrokerDidNotGiveEndsWhatTheRequestMade: a broker or helper from before --ttl signs or
// grants without the lifetime and does not refuse the request, so the grant would outlive what
// --ttl asked for. When the answer names no lifetime, or another one, the CLI revokes the grant it
// was given (or cancels the request still waiting for approval), exits 1 saying why, and prints
// no answer and runs nothing. What an earlier call made is never ended: an existing approved grant
// and a pending request the answer says this call joined (`coalesced`) are left alone, and the
// exit names them. A denied request made nothing to end, and still exits 77.
func TestATTLTheBrokerDidNotGiveEndsWhatTheRequestMade(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newTTLBroker(t)
	keyDir := newKeyDir(t)
	for _, tc := range []struct {
		name                string
		args                []string
		exit                int
		revoked, cancelled  string
		stderrNames, absent string
	}{
		{"request granted with no lifetime", []string{"request", "OLD_GRANTED", "--ttl", "2m"}, 1, "grant-old_granted", "", "no lifetime", ""},
		{"request --json granted with no lifetime", []string{"request", "OLD_GRANTED", "--ttl", "2m", "--json"}, 1, "grant-old_granted", "", "no lifetime", ""},
		{"exec form granted with no lifetime", []string{"OLD_GRANTED", "--ttl", "2m", "--", "sh", "-c", "echo ran-the-child"}, 1, "grant-old_granted", "", "nothing was run", ""},
		{"existing approved grant with no lifetime", []string{"request", "OLD_APPROVED_GRANTED", "--ttl", "2m"}, 1, "", "", "existing grant", ""},
		{"exec form granted another lifetime", []string{"OTHER_GRANTED", "--ttl", "2m", "--", "sh", "-c", "echo ran-the-child"}, 1, "grant-other_granted", "", "a lifetime of 43200s", ""},
		{"request pending with no lifetime", []string{"request", "OLD_PENDING", "--ttl", "2m"}, 1, "", "req-old_pending", "the request was ended", ""},
		{"exec form pending with no lifetime", []string{"OLD_PENDING", "--ttl", "2m", "--wait", "1s", "--", "sh", "-c", "echo ran-the-child"}, 1, "", "req-old_pending", "the request was ended", ""},
		{"request joined a pending request with no lifetime", []string{"request", "OLD_COALESCED", "--ttl", "2m"}, 1, "", "", "the existing pending request req-old_coalesced", ""},
		{"exec form joined a pending request with no lifetime", []string{"OLD_COALESCED", "--ttl", "2m", "--wait", "1s", "--", "sh", "-c", "echo ran-the-child"}, 1, "", "", "the existing pending request req-old_coalesced", ""},
		{"request denied", []string{"request", "OLD_DENIED", "--ttl", "2m"}, exitDenied, "", "", "", "lifetime"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil, tc.args...)
			_, lifetimes, _, revoked, cancelled := b.take()
			if len(lifetimes) != 1 || lifetimes[0] != 120 {
				t.Fatalf("signed lifetimes = %v, want one request signing 120", lifetimes)
			}
			if exit != tc.exit {
				t.Fatalf("exit = %d, want %d (stdout %q, stderr %q)", exit, tc.exit, stdout, stderr)
			}
			if got := strings.Join(revoked, ","); got != tc.revoked {
				t.Fatalf("revoked grants = %q, want %q", got, tc.revoked)
			}
			if got := strings.Join(cancelled, ","); got != tc.cancelled {
				t.Fatalf("cancelled requests = %q, want %q", got, tc.cancelled)
			}
			if tc.exit == 1 && stdout != "" {
				t.Fatalf("stdout = %q, want nothing printed or run for a grant that was ended", stdout)
			}
			if tc.stderrNames != "" && (!strings.Contains(stderr, tc.stderrNames) || !strings.Contains(stderr, "--ttl 120s")) {
				t.Fatalf("stderr = %q, want it to name %q and the --ttl asked for", stderr, tc.stderrNames)
			}
			if tc.absent != "" && strings.Contains(stderr, tc.absent) {
				t.Fatalf("stderr = %q, want no word of %q", stderr, tc.absent)
			}
		})
	}
}
