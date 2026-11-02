package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// fixedState is one projected state, as the daemon's own source would answer it.
type fixedState struct{ state State }

func (f fixedState) State(context.Context, pgx.Tx) (State, error) { return f.state, nil }

// suspendedByTag is the one member name no response a session reads may carry.
const suspendedByTag = "suspendedBy"

// jsonMembers is every JSON member name the type tree rooted at t can write, by its path from the
// root, following structs, pointers, slices and maps and stopping at a type it has already walked.
func jsonMembers(t reflect.Type, path string, seen map[reflect.Type]bool, into map[string]string) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return
	}
	seen[t] = true
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		member := path + "." + name
		into[member] = field.Type.String()
		jsonMembers(field.Type, member, seen, into)
	}
}

// membersOf is jsonMembers over one root type.
func membersOf(t reflect.Type) map[string]string {
	members := map[string]string{}
	jsonMembers(t, t.Name(), map[reflect.Type]bool{}, members)
	return members
}

// INFRA-945 R6: `released` is a new value of a claim's suspension source, and no response a
// session reads carries that source at all — not the unauthenticated state, not the claim
// routes' own answers. A plugin built against an older contract therefore reads no value it does
// not know, which is why the daemon serves one shape rather than two. The value lives on the
// operator routes alone, which carry an operator bearer and which no plugin calls.
func TestNoSessionFacingResponseCarriesTheSuspensionSource(t *testing.T) {
	for _, response := range []any{
		State{}, ReleaseRecordsResponse{}, ControllerRegisterResponse{}, ControllerSecretResponse{},
		HandoffCompleteResponse{}, WaveReleaseResponse{},
	} {
		root := reflect.TypeOf(response)
		for member := range membersOf(root) {
			if strings.HasSuffix(member, "."+suspendedByTag) {
				t.Errorf("%s carries %s; a session reads this response, and no session reads the suspension source", root.Name(), member)
			}
		}
	}

	// The one surface that does carry it, so this test says where the value went rather than only
	// where it did not.
	operator := membersOf(reflect.TypeOf(OperatorClaim{}))
	if _, carried := operator["OperatorClaim."+suspendedByTag]; !carried {
		t.Error("the operator claim view no longer carries suspendedBy; it is the one response that serves it")
	}
}

// A released claim is suspended, and that is the state every session-facing reader sees: the
// state route's claim view carries `state` alone, and the source is told only to the operator.
func TestReleasedClaimReadsAsSuspendedOnTheStateRoute(t *testing.T) {
	released := supervise.Claim{
		Token: "legion-legion-legion-208-implementer", Project: testProject, Tree: "LEGION-208",
		Issue: "LEGION-208", Role: claim.RoleImplementer, Generation: 2, Session: "ses_implementer",
		State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased,
	}
	// The operator's view of that same claim: the state is the suspension, the source says why.
	view := operatorView(released)
	if view.State != string(supervise.StateSuspended) || view.SuspendedBy != string(supervise.SuspendedByReleased) {
		t.Fatalf("the operator claim view = %s/%s, want suspended/released", view.State, view.SuspendedBy)
	}

	st := testStore(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	// The claim as the state projection writes it (internal/projection): the claim's own state,
	// and no member for its source.
	state := State{Issues: map[string]Issue{
		"LEGION-208": {
			Key: "LEGION-208", Generation: 2, Phase: phase.Implementing, Status: "in_progress",
			Workers: map[claim.Role]PhaseView{
				claim.RoleImplementer: {Claim: ClaimView{Session: released.Session, State: string(released.State)}},
			},
		},
	}}
	server := NewServer("127.0.0.1", 8439, Options{
		State: fixedState{state: state}, StateTransactions: st, Project: testProject, Log: quiet,
	})
	recorder := httptest.NewRecorder()
	server.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/legion/v1/state", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("state = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	body := recorder.Body.String()
	if strings.Contains(body, suspendedByTag) {
		t.Errorf("the unauthenticated state carries %s: %s", suspendedByTag, body)
	}
	var served struct {
		Issues map[string]struct {
			Workers map[string]struct {
				Claim struct {
					State string `json:"state"`
				} `json:"claim"`
			} `json:"workers"`
		} `json:"issues"`
	}
	if err := json.Unmarshal([]byte(body), &served); err != nil {
		t.Fatalf("read the served state: %v", err)
	}
	if got := served.Issues["LEGION-208"].Workers["implementer"].Claim.State; got != string(supervise.StateSuspended) {
		t.Errorf("the released claim's served state = %q, want %q", got, supervise.StateSuspended)
	}
}
