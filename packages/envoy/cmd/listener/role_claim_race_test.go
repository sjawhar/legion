package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
)

// claimRace has every id in ids claim role through handler at the same moment, and returns each
// claimant's status and body in the order of ids.
func claimRace(t *testing.T, handler http.HandlerFunc, ids []string, role string, soft bool) (codes []int, bodies []string) {
	t.Helper()
	requests := make([]string, len(ids))
	for i, id := range ids {
		requests[i] = fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":%t}`, id, role, soft)
	}
	return claimRaceBodies(t, handler, requests)
}

// claimRaceBodies posts every request in requests to handler at the same moment, and returns each
// answer's status and body in the order of requests. It is claimRace for a race whose claimants
// differ in more than their session id - the processes of one session id, each stating a client
// instance of its own.
func claimRaceBodies(t *testing.T, handler http.HandlerFunc, requests []string) (codes []int, bodies []string) {
	t.Helper()
	codes = make([]int, len(requests))
	bodies = make([]string, len(requests))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, request := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			<-start
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set", strings.NewReader(request)))
			codes[i], bodies[i] = recorder.Code, strings.TrimSpace(recorder.Body.String())
		}()
	}
	close(start)
	wg.Wait()
	return codes, bodies
}

// registerClaimants registers count live sessions with the ids this test's claims are made under.
func registerClaimants(t *testing.T, sessions *session.SessionRegistry, count int) []string {
	t.Helper()
	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("ses_claimant_%02d", i)
		if err := sessions.Put(ids[i], session.SessionEntry{MachineID: "test-machine"}); err != nil {
			t.Fatalf("register %s: %v", ids[i], err)
		}
	}
	return ids
}

// Sessions claiming one role at the same moment make as many honest claims, and POST /v1/roles/set
// documents exactly two answers for them: the 200 of a claim that landed, and, for a soft claim a
// live holder holds, the 409 naming that holder. A 500 is not one of them. While the claim's write
// was conditional on the revision it had just read, most of a burst lost that compare-and-swap and
// came back JetStream's wrong last sequence (error 10071, which nats.go answers as an
// *nats.APIError matching nats.ErrKeyExists); storeAnswered counts that as the store's own answer,
// so writeStoreError wrote it as this route's 500 -- a hard claim refused although last-claim-wins
// refuses nobody.
//
// The hard burst therefore asks for 200 from every claimant, and the soft burst for 200 or a 409
// naming one of the claimants. Both then ask that the role ended held by a claimant whose interest
// row names the role topic, since a claim that landed must not have its topic rolled back under it.
func TestRoleSetConcurrentClaimsOfOneRoleAnswerOnlyWhatTheRouteDocuments(t *testing.T) {
	const claimants = 24

	for _, tc := range []struct {
		name string
		role string
		soft bool
		want string
	}{
		{name: "hard claims all land", role: "legion-controller", want: "the 200 of a claim that landed"},
		{name: "soft claims land or are refused with their holder", role: "legion-tester", soft: true,
			want: "the 200 of a claim that landed or the 409 of a live holder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry, sessions := setupSessionsTest(t, nil, nil)
			d := withRoleClaimDrain(t, &listenerDeps{
				client:   setupPublishTestClient(t),
				registry: registry,
				sessions: sessions,
			})
			ids := registerClaimants(t, sessions, claimants)

			codes, bodies := claimRace(t, roleSetHandler(d, "test-machine", logging.New("test")), ids, tc.role, tc.soft)

			for i, code := range codes {
				switch {
				case code == http.StatusOK:
				case code == http.StatusConflict && tc.soft:
					var answer struct {
						Holder string `json:"holder"`
						Role   string `json:"role"`
					}
					if err := json.Unmarshal([]byte(bodies[i]), &answer); err != nil {
						t.Errorf("%s: 409 body %s: %v", ids[i], bodies[i], err)
						continue
					}
					if answer.Role != tc.role || !slices.Contains(ids, answer.Holder) {
						t.Errorf("%s: refused with role %q held by %q, want %q held by one of the claimants",
							ids[i], answer.Role, answer.Holder, tc.role)
					}
				default:
					t.Errorf("%s: claim of %s answered %d %s, want %s", ids[i], tc.role, code, bodies[i], tc.want)
				}
			}
			if t.Failed() {
				t.FailNow()
			}

			holder, err := registry.RoleHolder(tc.role)
			if err != nil {
				t.Fatalf("read the holder of %s after %d concurrent claims: %v", tc.role, claimants, err)
			}
			if !slices.Contains(ids, holder) {
				t.Fatalf("after %d concurrent claims %s is held by %q, want one of the claimants", claimants, tc.role, holder)
			}
			item, err := registry.Get(t.Context(), holder)
			if err != nil {
				t.Fatalf("read the interest row of %s, the holder of %s: %v", holder, tc.role, err)
			}
			if topic := contracts.RoleTopicPrefix + tc.role; !slices.Contains(item.Topics, topic) {
				t.Fatalf("the holder %s of %s has topics %v, want %s among them", holder, tc.role, item.Topics, topic)
			}
		})
	}
}

// roleClaimant is one process making one claim in a race: the session id it claims under and the
// client instance it states. For the processes of one session id, the instance is all that tells
// one claimant from another.
type roleClaimant struct {
	sessionID string
	instance  string
}

// A role is held by one session, through one claim record, so however many soft claims of an
// unheld role are made at the same moment, exactly one of them may be granted. The route decides
// a claim on one read of that record and store.Registry.SetRoleWithPrevious writes against a
// second read of its own, and a soft claim with no precondition left the two unconnected: a burst
// of claims under one session id was granted to several of them at once, because each one's lost
// compare-and-swap was read as the concurrent success of its own session (the heldByClaimant arm
// of SetRoleWithPrevious) while the claim that stands names another process entirely. Two
// processes then believe they hold one role, and both answer for it.
//
// Both bursts here race over the real route and the real buckets, with no stub between them. Each
// asks for one 200 and no more, for the rest to be the refusal this route words - the 409 naming
// the session the standing claim gives the role to - and for the bucket to name the claim that
// was granted, the process it was granted to among it.
func TestConcurrentSoftClaimsOfOneUnheldRoleAreGrantedToExactlyOneProcess(t *testing.T) {
	const racers = 8

	for _, tc := range []struct {
		name    string
		role    string
		racers  func(t *testing.T, sessions *session.SessionRegistry) []roleClaimant
		refused string
	}{
		{
			name:    "claimants of their own session ids",
			role:    "legion-race-sessions",
			refused: "the holder the standing claim gives the role to",
			racers: func(t *testing.T, sessions *session.SessionRegistry) []roleClaimant {
				t.Helper()
				claimants := make([]roleClaimant, racers)
				for i, id := range registerClaimants(t, sessions, racers) {
					claimants[i] = roleClaimant{sessionID: id, instance: fmt.Sprintf("ci_race_%02d", i)}
				}
				return claimants
			},
		},
		{
			name:    "processes of one session id",
			role:    "legion-race-processes",
			refused: "the 409 that holds a second process off",
			racers: func(t *testing.T, sessions *session.SessionRegistry) []roleClaimant {
				t.Helper()
				const sessionID = "ses_race_one_session"
				if err := sessions.Put(sessionID, session.SessionEntry{MachineID: "test-machine"}); err != nil {
					t.Fatalf("register %s: %v", sessionID, err)
				}
				claimants := make([]roleClaimant, racers)
				for i := range claimants {
					claimants[i] = roleClaimant{sessionID: sessionID, instance: fmt.Sprintf("ci_race_process_%02d", i)}
				}
				return claimants
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry, sessions := setupSessionsTest(t, nil, nil)
			d := withRoleClaimDrain(t, &listenerDeps{
				client:   setupPublishTestClient(t),
				registry: registry,
				sessions: sessions,
			})
			claimants := tc.racers(t, sessions)
			requests := make([]string, len(claimants))
			for i, claimant := range claimants {
				requests[i] = fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true,"client_instance_id":%q}`,
					claimant.sessionID, tc.role, claimant.instance)
			}

			codes, bodies := claimRaceBodies(t, roleSetHandler(d, "test-machine", logging.New("test")), requests)

			var granted []roleClaimant
			for i, code := range codes {
				if code == http.StatusOK {
					granted = append(granted, claimants[i])
				}
			}
			if len(granted) != 1 {
				t.Fatalf("%d of %d concurrent soft claims of the unheld role %s were granted, want exactly 1: granted to %v, with statuses %v",
					len(granted), len(claimants), tc.role, granted, codes)
			}
			winner := granted[0]
			for i, code := range codes {
				if code == http.StatusOK {
					continue
				}
				if code != http.StatusConflict {
					t.Errorf("%s as %s: claim of %s answered %d %s, want %s",
						claimants[i].sessionID, claimants[i].instance, tc.role, code, bodies[i], tc.refused)
					continue
				}
				var answer struct {
					Role   string `json:"role"`
					Holder string `json:"holder"`
				}
				if err := json.Unmarshal([]byte(bodies[i]), &answer); err != nil {
					t.Errorf("%s as %s: 409 body %s: %v", claimants[i].sessionID, claimants[i].instance, bodies[i], err)
					continue
				}
				if answer.Role != tc.role || answer.Holder != winner.sessionID {
					t.Errorf("%s as %s: refused with role %q held by %q, want %q held by %q",
						claimants[i].sessionID, claimants[i].instance, answer.Role, answer.Holder, tc.role, winner.sessionID)
				}
			}
			if t.Failed() {
				t.FailNow()
			}

			claim, err := registry.RoleClaim(t.Context(), tc.role)
			if err != nil {
				t.Fatalf("read %s's claim after %d concurrent claims: %v", tc.role, len(claimants), err)
			}
			if claim.HolderSessionID != winner.sessionID || claim.ClientInstanceID != winner.instance {
				t.Fatalf("%s's claim names %q held by the process %q; want the one claimant that was granted it, %q as %q",
					tc.role, claim.HolderSessionID, claim.ClientInstanceID, winner.sessionID, winner.instance)
			}
			item, err := registry.Get(t.Context(), winner.sessionID)
			if err != nil {
				t.Fatalf("read the interest row of %s, the holder of %s: %v", winner.sessionID, tc.role, err)
			}
			if topic := contracts.RoleTopicPrefix + tc.role; !slices.Contains(item.Topics, topic) {
				t.Fatalf("the holder %s of %s has topics %v, want %s among them", winner.sessionID, tc.role, item.Topics, topic)
			}
		})
	}
}
