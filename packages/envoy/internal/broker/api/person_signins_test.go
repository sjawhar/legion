// person_signins_test.go drives GET /v1/person-signins and POST
// /v1/person-signins/{id}/revoke, the sign-in routes Dispatch's page relays to, on the same real
// broker api_test.go mounts.
package api_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/store"
)

// The people, box keys and machine these tests sign in with. Each thumbprint is the 43 base64url
// characters an RFC 7638 one is, which the table itself holds them to.
const (
	signInAda        = "ada@example.com"
	signInGrace      = "grace@example.com"
	adaBoxOne        = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	adaBoxTwo        = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	graceBox         = "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
	signInFleetOne   = "example-fleet@example-project.iam.gserviceaccount.com"
	signInFleetTwo   = "example-pool@example-project.iam.gserviceaccount.com"
	unknownSignInID  = "00000000-0000-4000-8000-000000000000"
	notAUUIDSignInID = "not-a-uuid"
)

// wirePersonSignIn is one sign-in in GET /v1/person-signins's answer.
type wirePersonSignIn struct {
	SignInID   string    `json:"signin_id"`
	Machine    string    `json:"machine"`
	Thumbprint string    `json:"thumbprint"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

// signIn writes one sign-in straight into the store, as the token exchange does in its own
// transaction: these routes read and revoke, and nothing they serve creates one.
func (ts *testServer) signIn(t *testing.T, person, thumbprint, machine string) {
	t.Helper()
	ctx := context.Background()
	tx, err := ts.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := store.RecordPersonSignIn(ctx, tx, person, thumbprint, machine); err != nil {
		t.Fatalf("record the sign-in of %s on %s: %v", person, machine, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// personSignIns reads GET /v1/person-signins?person=<person>, as Dispatch's server does for its
// signed-in person.
func (ts *testServer) personSignIns(t *testing.T, person string) []wirePersonSignIn {
	t.Helper()
	status, body := ts.ui(t, http.MethodGet, "/v1/person-signins?person="+url.QueryEscape(person), nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/person-signins?person=%s = %d: %s", person, status, body)
	}
	return decode[struct {
		SignIns []wirePersonSignIn `json:"signins"`
	}](t, body).SignIns
}

// TestThePersonSignInsListIsThatPersonsLiveSignIns: the list answers one row per box key that
// person signed in with and has not revoked, each naming the machine and the box key, and
// nobody else's.
func TestThePersonSignInsListIsThatPersonsLiveSignIns(t *testing.T) {
	ts := newTestServer(t)
	ts.signIn(t, signInAda, adaBoxOne, signInFleetOne)
	ts.signIn(t, signInAda, adaBoxTwo, signInFleetTwo)
	ts.signIn(t, signInGrace, graceBox, signInFleetOne)

	rows := ts.personSignIns(t, signInAda)
	if len(rows) != 2 {
		t.Fatalf("GET /v1/person-signins = %+v, want the two boxes %s signed in on", rows, signInAda)
	}
	byKey := map[string]wirePersonSignIn{}
	for _, row := range rows {
		byKey[row.Thumbprint] = row
	}
	for thumbprint, machine := range map[string]string{adaBoxOne: signInFleetOne, adaBoxTwo: signInFleetTwo} {
		row, listed := byKey[thumbprint]
		if !listed {
			t.Fatalf("the list %+v carries no sign-in on %s", rows, thumbprint)
		}
		if row.Machine != machine {
			t.Errorf("the sign-in on %s names the machine %s, want %s", thumbprint, row.Machine, machine)
		}
		if row.SignInID == "" || row.CreatedAt.IsZero() || row.LastUsedAt.IsZero() {
			t.Errorf("the sign-in on %s = %+v, want an id and both times", thumbprint, row)
		}
	}
	if grace := ts.personSignIns(t, signInGrace); len(grace) != 1 || grace[0].Thumbprint != graceBox {
		t.Fatalf("GET /v1/person-signins for %s = %+v, want their one sign-in and nobody else's", signInGrace, grace)
	}
	// An address the caller spells in capitals is the same person: every login the broker
	// compares is lowercased first.
	if upper := ts.personSignIns(t, "Ada@Example.com"); len(upper) != 2 {
		t.Fatalf("GET /v1/person-signins for the address in capitals = %+v, want the same two sign-ins", upper)
	}
}

// TestRevokingASignInEndsItAndLeavesThePersonsOthers: the revoke answers "revoked" with how many
// access tokens it ended, takes the sign-in off the person's list, and leaves their other boxes
// and everybody else's alone. Revoking it again answers the same and ends nothing.
func TestRevokingASignInEndsItAndLeavesThePersonsOthers(t *testing.T) {
	ts := newTestServer(t)
	ts.signIn(t, signInAda, adaBoxOne, signInFleetOne)
	ts.signIn(t, signInAda, adaBoxTwo, signInFleetTwo)
	ts.signIn(t, signInGrace, graceBox, signInFleetOne)
	var target string
	for _, row := range ts.personSignIns(t, signInAda) {
		if row.Thumbprint == adaBoxOne {
			target = row.SignInID
		}
	}
	if target == "" {
		t.Fatal("the list carries no sign-in to revoke")
	}

	for i := range 2 {
		status, body := ts.ui(t, http.MethodPost, "/v1/person-signins/"+target+"/revoke", map[string]any{"person": signInAda})
		if status != http.StatusOK {
			t.Fatalf("revoke %d = %d: %s", i, status, body)
		}
		answer := decode[struct {
			State       string `json:"state"`
			TokensEnded int    `json:"tokens_ended"`
		}](t, body)
		if answer.State != "revoked" || answer.TokensEnded != 0 {
			t.Fatalf("revoke %d answered %+v, want revoked with no token ended: nothing was minted under it", i, answer)
		}
	}
	left := ts.personSignIns(t, signInAda)
	if len(left) != 1 || left[0].Thumbprint != adaBoxTwo {
		t.Fatalf("after the revoke %s holds %+v, want their other box alone", signInAda, left)
	}
	if grace := ts.personSignIns(t, signInGrace); len(grace) != 1 {
		t.Fatalf("after the revoke %s holds %+v, want their sign-in untouched", signInGrace, grace)
	}
}

// TestThePersonSignInRoutesRefuseWhatTheyCannot: no person named, a malformed id, an id nothing
// has, and another person's sign-in are each refused with the code that says which, and the
// last two alike, so nobody learns another person's sign-ins by trying ids.
func TestThePersonSignInRoutesRefuseWhatTheyCannot(t *testing.T) {
	ts := newTestServer(t)
	ts.signIn(t, signInGrace, graceBox, signInFleetOne)
	theirs := ts.personSignIns(t, signInGrace)[0].SignInID

	for _, c := range []struct {
		name   string
		method string
		path   string
		body   any
		status int
		code   string
	}{
		{name: "a list naming no person", method: http.MethodGet, path: "/v1/person-signins",
			status: http.StatusBadRequest, code: "PERSON_REQUIRED"},
		{name: "a list naming a blank person", method: http.MethodGet, path: "/v1/person-signins?person=%20%20",
			status: http.StatusBadRequest, code: "PERSON_REQUIRED"},
		{name: "a revoke naming no person", method: http.MethodPost, path: "/v1/person-signins/" + unknownSignInID + "/revoke",
			body: map[string]any{}, status: http.StatusBadRequest, code: "PERSON_REQUIRED"},
		{name: "a revoke of an id that is no UUID", method: http.MethodPost, path: "/v1/person-signins/" + notAUUIDSignInID + "/revoke",
			body: map[string]any{"person": signInAda}, status: http.StatusBadRequest, code: "SIGNIN_ID_INPUT"},
		{name: "a revoke of an id nothing has", method: http.MethodPost, path: "/v1/person-signins/" + unknownSignInID + "/revoke",
			body: map[string]any{"person": signInAda}, status: http.StatusNotFound, code: "NOT_FOUND"},
		{name: "a revoke of another person's sign-in", method: http.MethodPost, path: "/v1/person-signins/" + theirs + "/revoke",
			body: map[string]any{"person": signInAda}, status: http.StatusNotFound, code: "NOT_FOUND"},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, body := ts.ui(t, c.method, c.path, c.body)
			if status != c.status {
				t.Fatalf("%s %s = %d, want %d: %s", c.method, c.path, status, c.status, body)
			}
			if got := decode[struct {
				Code string `json:"code"`
			}](t, body).Code; got != c.code {
				t.Fatalf("%s %s answered the code %q, want %q", c.method, c.path, got, c.code)
			}
		})
	}
	if grace := ts.personSignIns(t, signInGrace); len(grace) != 1 {
		t.Fatalf("%s holds %+v after those refusals, want their sign-in untouched", signInGrace, grace)
	}
}
