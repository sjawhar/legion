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
	codes = make([]int, len(ids))
	bodies = make([]string, len(ids))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":%t}`, id, role, soft)
			recorder := httptest.NewRecorder()
			<-start
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set", strings.NewReader(body)))
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
