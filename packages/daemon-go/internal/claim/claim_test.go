package claim

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// The six words a role travels as: the pane's `LEGION_ROLE`, the register response's `role`, the
// `workers` key in the state the plugin reads. A renamed word is a cross-language break, so the
// list is pinned here rather than left to the constant declarations.
func TestRoleConstantsAreTheWireWords(t *testing.T) {
	roles := []Role{RoleArchitect, RolePlanner, RoleImplementer, RoleTester, RoleReviewer, RoleMerger}
	want := []string{"architect", "planner", "implementer", "tester", "reviewer", "merger"}
	if len(roles) != len(want) {
		t.Fatalf("roles: got %d constants, want %d", len(roles), len(want))
	}
	for i, role := range roles {
		if string(role) != want[i] {
			t.Errorf("role %d: got %q, want %q", i, role, want[i])
		}
	}
}

// The register/ready/exit wire is exchanged with the Oh My Pi plugin and mirrored by
// `packages/contracts/src/legion-go-api.ts`, so every member name here is a contract with another
// language. Marshalling is what pins them: a renamed Go field silently changes the wire unless a
// test reads the bytes.
func TestWireMemberNames(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		value any
		want  string
	}{
		{
			name: "register request",
			value: RegisterRequest{
				BootToken:      "boot-1",
				SessionID:      "ses_architect_208",
				OmpSessionFile: "/state/sessions/ses_architect_208.json",
				AgentID:        "agent-1",
				PluginContract: 1,
			},
			want: `{"bootToken":"boot-1","sessionId":"ses_architect_208",` +
				`"ompSessionFile":"/state/sessions/ses_architect_208.json","agentId":"agent-1",` +
				`"pluginContract":1}`,
		},
		{
			name: "register response",
			value: RegisterResponse{
				ClaimToken: "legion-omp-LEGION-208-architect",
				Tree:       "LEGION-208",
				Issue:      "LEGION-208",
				Role:       RoleArchitect,
				Generation: 3,
				Secret:     "sec-1",
			},
			want: `{"claimToken":"legion-omp-LEGION-208-architect","tree":"LEGION-208",` +
				`"issue":"LEGION-208","role":"architect","generation":3,"secret":"sec-1"}`,
		},
		{
			name: "ready request",
			value: ReadyRequest{
				ClaimToken: "legion-omp-LEGION-208-tester",
				SessionID:  "ses_tester_208",
				Secret:     "sec-2",
				Generation: 4,
			},
			want: `{"claimToken":"legion-omp-LEGION-208-tester","sessionId":"ses_tester_208",` +
				`"secret":"sec-2","generation":4}`,
		},
		{
			name: "exit request",
			value: ExitRequest{
				ClaimToken: "legion-omp-LEGION-208-tester",
				SessionID:  "ses_tester_208",
				Secret:     "sec-2",
				Generation: 4,
				Reason:     "phase complete",
			},
			want: `{"claimToken":"legion-omp-LEGION-208-tester","sessionId":"ses_tester_208",` +
				`"secret":"sec-2","generation":4,"reason":"phase complete"}`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			encoded, err := json.Marshal(testCase.value)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(encoded) != testCase.want {
				t.Fatalf("wire shape\n got: %s\nwant: %s", encoded, testCase.want)
			}
		})
	}
}

// A refusal read back off the wire is the same value the route answered: the plugin decides
// whether to exit from the status, and an operator reads the sentence.
func TestRefusalsCarryTheStatusAndSentenceTheRoutesAnswer(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		refusal     Refusal
		wantStatus  int
		wantMessage string
	}{
		{"invalid boot token", InvalidBootToken, http.StatusForbidden, "Invalid boot token"},
		{"stale generation", StaleGeneration, http.StatusConflict, "Stale generation"},
		{
			name:        "same agent refusal",
			refusal:     SameAgentRefusal,
			wantStatus:  http.StatusConflict,
			wantMessage: "Worker respawn must resume the same agent session",
		},
		{"invalid secret", InvalidSecret, http.StatusForbidden, "Invalid session secret"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.refusal.Status != testCase.wantStatus {
				t.Errorf("status: got %d, want %d", testCase.refusal.Status, testCase.wantStatus)
			}
			if testCase.refusal.Error() != testCase.wantMessage {
				t.Errorf("message: got %q, want %q", testCase.refusal.Error(), testCase.wantMessage)
			}
			encoded, err := json.Marshal(testCase.refusal)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			want := `{"error":"` + testCase.wantMessage + `"}`
			if string(encoded) != want {
				t.Fatalf("body\n got: %s\nwant: %s", encoded, want)
			}
		})
	}
}

// A handler wraps a refusal with the context of the request it refused; the caller still has to
// be able to ask which refusal it was, because the status it answers depends on it.
func TestAWrappedRefusalIsStillIdentifiable(t *testing.T) {
	wrapped := errors.Join(errors.New("register LEGION-208/tester"), SameAgentRefusal)
	if !errors.Is(wrapped, SameAgentRefusal) {
		t.Fatalf("errors.Is(wrapped, SameAgentRefusal): got false, want true")
	}
	if errors.Is(wrapped, StaleGeneration) {
		t.Fatalf("errors.Is(wrapped, StaleGeneration): got true, want false")
	}
}

// The project word every token and the private tmux server carry is the shipped daemon's
// (config.ts:1429-1433): the operator's `project` lowercased, with everything outside [a-z0-9]
// dropped, so `sjawhar/legion` and `LEGION` name the same server either daemon would.
func TestProjectTokenIsTheShippedSpelling(t *testing.T) {
	for project, want := range map[string]string{
		"legion":           "legion",
		"E2E1234567":       "e2e1234567",
		"sjawhar/legion":   "sjawharlegion",
		"  Acme Widgets-2": "acmewidgets2",
	} {
		got, err := ProjectToken(project)
		if err != nil || got != want {
			t.Errorf("ProjectToken(%q) = %q, %v; want %q", project, got, err, want)
		}
	}
	if _, err := ProjectToken("-/_ "); err == nil || !strings.Contains(err.Error(), "project") {
		t.Errorf("ProjectToken of a project with no alphanumeric character = %v, want a refusal naming project", err)
	}
}

// A claim token is the Envoy role the agent claims and the name every file of its pane is filed
// under, so it is the shipped roleToken byte for byte (legion-roles.ts:48-55) — a token Envoy
// would refuse, or one the TypeScript plugin would spell differently, is a role nobody reaches.
func TestNewTokenIsTheShippedRoleToken(t *testing.T) {
	got, err := NewToken("legion", "LEGION-208", RoleTester)
	if err != nil || got != "legion-legion-legion-208-tester" {
		t.Fatalf("NewToken = %q, %v; want legion-legion-legion-208-tester", got, err)
	}
	for _, testCase := range []struct {
		name, project, issue string
		role                 Role
		wantFragment         string
	}{
		{"a lowercase issue key", "legion", "legion-208", RoleTester, `"legion-208"`},
		{"an issue key with no number", "legion", "LEGION", RoleTester, `"LEGION"`},
		{"a role no agent holds", "legion", "LEGION-208", Role("operator"), `"operator"`},
		{"a project that is not a token", "Legion", "LEGION-208", RoleTester, `"Legion"`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			token, err := NewToken(testCase.project, testCase.issue, testCase.role)
			if err == nil {
				t.Fatalf("NewToken = %q, want a refusal", token)
			}
			if !strings.Contains(err.Error(), testCase.wantFragment) {
				t.Errorf("the refusal does not name %s: %v", testCase.wantFragment, err)
			}
		})
	}
}
