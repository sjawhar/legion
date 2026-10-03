package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// handoffProof is one production-like proof, every field of it non-blank.
const handoffProof = `{"criterion":"a handoff without proof is refused","surface":"the branch CLI in a scratch workspace",` +
	`"command":"legion handoff write --phase implement","observed":"exit 1 naming proof",` +
	`"headSha":"0123456789abcdef0123456789abcdef01234567","negativeControl":"the same payload with its proof: exit 0"}`

// verifiedProof is a tester's verdict on the implementer's proof that holds.
const verifiedProof = `{"verdict":"verified","how":"re-ran its command at its head"}`

// writableHandoffs are a handoff of each phase its rules accept, as its role prompt has the worker
// write it.
var writableHandoffs = map[string]string{
	"architect": `{"scope":"small","subIssues":[]}`,
	"plan": `{"requiredSkills":{"implement":["legion-worker"],"test":["legion-worker"],"review":["none: no skill covers a one-line change"]},` +
		`"gapAnalysis":{"findings":[]},"planReview":{"verdict":"approved","rounds":1},"specDepartures":[]}`,
	"implement": `{"filesChanged":["x.go"],"proof":[` + handoffProof + `]}`,
	"test":      `{"passed":3,"failed":0,"implementerProof":` + verifiedProof + `,"proof":[` + handoffProof + `]}`,
	"review":    `{"verdict":"approved","critical":0}`,
}

func TestHandoffWriteAndReadPersistInWorkspace(t *testing.T) {
	workspace := t.TempDir()
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "handoff", "write", "--workspace", workspace, "--phase", "implement", "--data", writableHandoffs["implement"]}, &out, &errb); code != 0 {
		t.Fatalf("handoff write = %d: %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(workspace, ".legion", "implement.json")); err != nil {
		t.Fatalf("handoff file: %v", err)
	}
	out.Reset()
	if code := run(context.Background(), []string{"legion", "handoff", "read", "--workspace", workspace, "--phase", "implement"}, &out, &errb); code != 0 || !strings.Contains(out.String(), `"phase": "implement"`) {
		t.Fatalf("handoff read = %d: stdout %s stderr %s", code, out.String(), errb.String())
	}
}

// A handoff past one argv string's 128 KiB cap (MAX_ARG_STRLEN) can only arrive on stdin: the
// legion tool sends every handoff_write payload that way.
func TestHandoffWriteReadsAPayloadOverTheArgvCapFromStdin(t *testing.T) {
	workspace := t.TempDir()
	records := make([]string, 0, 2000)
	for i := range 2000 {
		records = append(records, fmt.Sprintf(`{"round":%d,"note":"%s"}`, i, strings.Repeat("r", 80)))
	}
	payload := `{"implementerProof":` + verifiedProof + `,"proof":[` + handoffProof + `],"rounds":[` + strings.Join(records, ",") + `]}`
	if len(payload) <= 128*1024 {
		t.Fatalf("payload is %d bytes, not over the 128 KiB argv cap", len(payload))
	}
	input, err := os.CreateTemp(t.TempDir(), "handoff-*.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := input.WriteString(payload); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	previous := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = previous; _ = input.Close() })
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "handoff", "write", "--workspace", workspace, "--phase", "test"}, &out, &errb); code != 0 {
		t.Fatalf("handoff write from stdin = %d: %s", code, errb.String())
	}
	written, err := os.ReadFile(filepath.Join(workspace, ".legion", "test.json"))
	if err != nil {
		t.Fatalf("handoff file: %v", err)
	}
	var handoff struct {
		Phase  string            `json:"phase"`
		Rounds []json.RawMessage `json:"rounds"`
	}
	if err := json.Unmarshal(written, &handoff); err != nil || handoff.Phase != "test" || len(handoff.Rounds) != 2000 {
		t.Fatalf("handoff file = phase %q, %d rounds, err %v; want test and 2000", handoff.Phase, len(handoff.Rounds), err)
	}
}

// The command writes a handoff's schemaVersion, phase and completed itself. A payload carrying them
// — an agent copying handoff_read's output into data — is refused naming every one it carries and
// that the command writes them, so the next call succeeds; nothing is written.
func TestHandoffWriteRefusesTheFieldsItWritesNamingEach(t *testing.T) {
	workspace := t.TempDir()
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "handoff", "write", "--workspace", workspace, "--phase", "implement",
		"--data", `{"schemaVersion":1,"completed":"2026-09-25T00:00:00Z","proof":["ran it"]}`}, &out, &errb)
	// The refusal's fixed parenthetical names every reserved field, so the carried list is asserted
	// where the refusal lists what the data carries.
	refusal := errb.String()
	if code != 1 || !strings.Contains(refusal, "data carries schemaVersion, completed, which this command writes itself") {
		t.Fatalf("handoff write with schemaVersion and completed in data = %d, stderr %q; want one refusal naming both and that the command writes them", code, refusal)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".legion")); !os.IsNotExist(err) {
		t.Fatalf(".legion after the refused write: %v, want none", err)
	}
}

// The write refuses a handoff its phase's rules refuse, naming every field at fault so the next
// write can fix it, and writes nothing. The implementer's proof, the tester's verdict on it and its
// own proof, and the plan's skills, two checks and departures are the records the next role reads;
// a declared field of the wrong type would have that role read a value that is not there.
func TestHandoffWriteRefusesWhatItsPhasesRulesRefuseNamingEachField(t *testing.T) {
	blankProof := strings.Replace(strings.Replace(handoffProof, `"exit 1 naming proof"`, `"  "`, 1), `,"negativeControl":"the same payload with its proof: exit 0"`, "", 1)
	plan := func(field, value string) string {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(writableHandoffs["plan"]), &fields); err != nil {
			t.Fatal(err)
		}
		fields[field] = json.RawMessage(value)
		encoded, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	planWithout := func(field string) string {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(writableHandoffs["plan"]), &fields); err != nil {
			t.Fatal(err)
		}
		delete(fields, field)
		encoded, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	departure := `{"spec":"the plan polls the uploader","plan":"the plan reads its event channel","evidence":"the measurement covers every required event","outcome":{"kind":"unchanged"}}`
	tooManyDepartures := `[` + strings.TrimSuffix(strings.Repeat(departure+",", maxSpecDepartures+1), ",") + `]`
	tooLongDeparture := `{"spec":"` + strings.Repeat("x", maxSpecDepartureTextBytes+1) + `","plan":"the plan reads its event channel","evidence":"the measurement covers every required event","outcome":{"kind":"unchanged"}}`
	tooLongUnknown := strings.Repeat("x", 5000)
	for _, tc := range []struct {
		name, phase, data string
		// fields are the fields the refusal names, in its order.
		fields []string
	}{
		{"an implement handoff without proof", "implement", `{"filesChanged":["x.go"]}`, []string{"proof"}},
		{"an implement handoff whose proof is empty", "implement", `{"proof":[]}`, []string{"proof"}},
		{"a proof that is a sentence", "implement", `{"proof":["ran it"]}`, []string{"proof.0"}},
		{"a proof with a blank field and a missing one", "implement", `{"proof":[` + blankProof + `]}`, []string{"proof.0.observed", "proof.0.negativeControl"}},
		{"a test handoff without a verdict on the implementer's proof", "test", `{"passed":3,"proof":[` + handoffProof + `]}`, []string{"implementerProof"}},
		{"a verdict outside its options, and a blank how", "test", `{"implementerProof":{"verdict":"maybe","how":" "},"proof":[` + handoffProof + `]}`, []string{"implementerProof.verdict", "implementerProof.how"}},
		{"a passing test handoff without a proof of the tester's own", "test", `{"passed":3,"implementerProof":` + verifiedProof + `}`, []string{"proof"}},
		{"failed > 0 without a recorded failure", "test", `{"failed":1,"implementerProof":` + verifiedProof + `,"proof":[` + handoffProof + `]}`, []string{"failures"}},
		{"a rejected implementer proof without a recorded failure", "test", `{"implementerProof":{"verdict":"rejected","how":"its command exits 2"},"proof":[` + handoffProof + `]}`, []string{"failures"}},
		{"a plan with none of its four records", "plan", `{"taskCount":3}`, []string{"requiredSkills", "gapAnalysis", "planReview", "specDepartures"}},
		{"a role's empty skill list and another's blank entry", "plan", plan("requiredSkills", `{"implement":[],"test":[" "],"review":["legion-worker"]}`), []string{"requiredSkills.implement", "requiredSkills.test.0"}},
		{"a skill list left out", "plan", plan("requiredSkills", `{"implement":["legion-worker"]}`), []string{"requiredSkills.test", "requiredSkills.review"}},
		{"a gap analysis with both findings and an error", "plan", plan("gapAnalysis", `{"findings":[],"error":"timed out"}`), []string{"gapAnalysis"}},
		{"a gap analysis with neither", "plan", plan("gapAnalysis", `{}`), []string{"gapAnalysis"}},
		{"a finding with a blank answer", "plan", plan("gapAnalysis", `{"findings":[{"finding":"no acceptance line for the error path","answer":""}]}`), []string{"gapAnalysis.findings.0.answer"}},
		{"a rejection recorded before the last round, without its issues", "plan", plan("planReview", `{"verdict":"rejected","rounds":1}`), []string{"planReview.remainingIssues", "planReview.rounds"}},
		{"an approval with an issue standing", "plan", plan("planReview", `{"verdict":"approved","rounds":2,"remainingIssues":[{"issue":"i","evidence":"e"}]}`), []string{"planReview.remainingIssues"}},
		{"a failed review without its error", "plan", plan("planReview", `{"verdict":"failed","rounds":1}`), []string{"planReview.error"}},
		{"an approval carrying an error", "plan", plan("planReview", `{"verdict":"approved","rounds":1,"error":"timed out"}`), []string{"planReview.error"}},
		{"more rounds than a planner runs", "plan", plan("planReview", `{"verdict":"approved","rounds":4}`), []string{"planReview.rounds"}},
		{"a fractional round count", "plan", plan("planReview", `{"verdict":"rejected","rounds":2.5,"remainingIssues":[{"issue":"i","evidence":"e"}]}`), []string{"planReview.rounds"}},
		{"a plan without its departures from the spec", "plan", planWithout("specDepartures"), []string{"specDepartures"}},
		{"a plan whose departures are a scalar", "plan", plan("specDepartures", `"none"`), []string{"specDepartures"}},
		{"a plan whose departures are an object", "plan", plan("specDepartures", `{"spec":"the plan polls"}`), []string{"specDepartures"}},
		{"a plan with a malformed departure", "plan", plan("specDepartures", `[{"spec":"","plan":"the plan reads its event channel","evidence":"the measurement covers every required event","outcome":{"kind":"changed"}}]`), []string{"specDepartures.0.spec", "specDepartures.0.outcome"}},
		{"a plan that marks a changed scope unchanged", "plan", plan("specDepartures", `[{"spec":"the plan changes a child boundary","plan":"the plan moves work to a new child","evidence":"the measured boundary excludes the work","outcome":{"kind":"unchanged","scope":"the child now owns the work"}}]`), []string{"specDepartures.0.outcome.scope"}},
		{"a plan with an unknown oversized departure field", "plan", plan("specDepartures", `[{"spec":"the plan polls the uploader","plan":"the plan reads its event channel","evidence":"the measurement covers every required event","outcome":{"kind":"unchanged"},"extra":"`+tooLongUnknown+`"}]`), []string{"specDepartures.0.extra"}},
		{"a plan with an unknown oversized outcome field", "plan", plan("specDepartures", `[{"spec":"the plan polls the uploader","plan":"the plan reads its event channel","evidence":"the measurement covers every required event","outcome":{"kind":"unchanged","extra":"`+tooLongUnknown+`"}}]`), []string{"specDepartures.0.outcome.extra"}},
		{"a plan with too many departures", "plan", plan("specDepartures", tooManyDepartures), []string{"specDepartures"}},
		{"a plan with a departure that is too long", "plan", plan("specDepartures", `[`+tooLongDeparture+`]`), []string{"specDepartures.0.spec"}},
		{"a plan's declared field of the wrong type, before its three rules", "plan", `{"taskCount":"3"}`, []string{"taskCount"}},
		{"a review's declared fields of the wrong type", "review", `{"critical":"1","verdict":"lgtm","keyFindings":[{"severity":"minor"}]}`, []string{"critical", "verdict", "keyFindings.0.file", "keyFindings.0.description"}},
		{"an architect's scope and routing hints outside their options", "architect", `{"scope":"huge","routingHints":{"skipArchitect":"no"}}`, []string{"scope", "routingHints.skipArchitect"}},
		{"learnings that are not a list of paths", "review", `{"learningsInjected":"docs/solutions/a.md","learningsHelpful":[1]}`, []string{"learningsInjected", "learningsHelpful.0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "handoff", "write", "--workspace", workspace, "--phase", tc.phase, "--data", tc.data}, &out, &errb)
			refusal, found := strings.CutPrefix(strings.TrimSuffix(errb.String(), "\n"), "legion handoff write: Invalid "+tc.phase+" handoff: ")
			if code != 1 || !found {
				t.Fatalf("handoff write --phase %s %s = %d, stderr %q; want an invalid %s handoff refused", tc.phase, tc.data, code, errb.String(), tc.phase)
			}
			var named []string
			for _, problem := range strings.Split(refusal, "; ") {
				field, _, _ := strings.Cut(problem, ": ")
				named = append(named, field)
			}
			if !slices.Equal(named, tc.fields) {
				t.Fatalf("refusal %q names %v, want %v", refusal, named, tc.fields)
			}
			if _, err := os.Stat(filepath.Join(workspace, ".legion")); !os.IsNotExist(err) {
				t.Fatalf(".legion after the refused write: %v, want none", err)
			}
		})
	}
}

// What the rules allow is written as sent, every field the phase does not declare included: a
// test handoff that records its failure needs no proof of its own, a plan may say no skill fits
// and record a failed check as its error, and a rejection after the last round is recorded with
// its issues.
func TestHandoffWriteWritesWhatItsPhasesRulesAllow(t *testing.T) {
	cases := map[string][2]string{}
	for phase, data := range writableHandoffs {
		cases[phase] = [2]string{phase, data}
	}
	failure := `"failures":[{"criterion":"greet trims the name","evidence":"bun greet.ts ' Ada ' prints Hello,  Ada !"}]`
	cases["a test reporting a failure"] = [2]string{"test", `{"failed":1,` + failure + `,"implementerProof":` + verifiedProof + `}`}
	cases["a test rejecting the implementer's proof"] = [2]string{"test", `{` + failure + `,"implementerProof":{"verdict":"rejected","how":"its command exits 2"}}`}
	cases["a plan whose checks failed"] = [2]string{"plan", `{"requiredSkills":{"implement":["none: x"],"test":["none: x"],"review":["none: x"]},` +
		`"gapAnalysis":{"error":"the analyst timed out"},"planReview":{"verdict":"failed","rounds":2,"error":"the review timed out"},"specDepartures":[]}`}
	cases["a plan rejected after the last round"] = [2]string{"plan", `{"requiredSkills":{"implement":["a"],"test":["b"],"review":["c"]},` +
		`"gapAnalysis":{"findings":[{"finding":"no error path","answer":"task 3"}]},"planReview":{"verdict":"rejected","rounds":3,"remainingIssues":[{"issue":"i","evidence":"e"}]},"specDepartures":[]}`}
	cases["a plan with a changed scope"] = [2]string{"plan", `{"requiredSkills":{"implement":["a"],"test":["b"],"review":["c"]},` +
		`"gapAnalysis":{"findings":[]},"planReview":{"verdict":"approved","rounds":1},"specDepartures":[{"spec":"the plan keeps one child","plan":"the plan adds a child","evidence":"the measured boundary needs a separate surface","outcome":{"kind":"changed","scope":"the new child owns the separate surface"}}]}`}
	cases["an implement handoff with fields it does not declare"] = [2]string{"implement", `{"proof":[` + handoffProof + `],"rebase2":{"onto":"main"},"summary":"done"}`}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			var out, errb bytes.Buffer
			if code := run(context.Background(), []string{"legion", "handoff", "write", "--workspace", workspace, "--phase", tc[0], "--data", tc[1]}, &out, &errb); code != 0 {
				t.Fatalf("handoff write --phase %s %s = %d: %s", tc[0], tc[1], code, errb.String())
			}
			written, err := os.ReadFile(filepath.Join(workspace, ".legion", tc[0]+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var got, sent map[string]any
			if err := json.Unmarshal(written, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc[1]), &sent); err != nil {
				t.Fatal(err)
			}
			for key, value := range sent {
				if !reflect.DeepEqual(got[key], value) {
					t.Fatalf("%s.json %s = %v, want %v as sent", tc[0], key, got[key], value)
				}
			}
			if got["phase"] != tc[0] || got["schemaVersion"] != float64(1) || got["completed"] == nil {
				t.Fatalf("%s.json = %v, want the phase, schemaVersion 1 and completed the command writes", tc[0], got)
			}
		})
	}
}

// The planner's role prompt shows each shape of its two plan checks and of its departures from the
// spec as JSON (internal/prompts/roles/planner.md, "Plan handoff"); a planner that records them as
// shown is not refused.
func TestHandoffWriteAcceptsEveryPlanHandoffShapeThePlannerPromptShows(t *testing.T) {
	prompt, err := os.ReadFile(filepath.Join("..", "..", "internal", "prompts", "roles", "planner.md"))
	if err != nil {
		t.Fatal(err)
	}
	shapes := func(field string) []string {
		for _, line := range strings.Split(string(prompt), "\n") {
			if strings.HasPrefix(line, "- `"+field+"`:") {
				var found []string
				for _, match := range regexp.MustCompile("`(\\{[^`]*\\})`").FindAllStringSubmatch(line, -1) {
					found = append(found, strings.NewReplacer("…", "x", ": N", ": 1").Replace(match[1]))
				}
				return found
			}
		}
		t.Fatalf("planner.md shows no %s", field)
		return nil
	}
	gapAnalyses, planReviews := shapes("gapAnalysis"), shapes("planReview")
	var verdicts []string
	for _, review := range planReviews {
		var shown struct {
			Verdict string  `json:"verdict"`
			Rounds  float64 `json:"rounds"`
		}
		if err := json.Unmarshal([]byte(review), &shown); err != nil {
			t.Fatalf("planner.md's planReview %s: %v", review, err)
		}
		verdicts = append(verdicts, shown.Verdict)
		if shown.Verdict == "rejected" && shown.Rounds != planReviewMaxRounds {
			t.Fatalf("planner.md records a rejection at round %v, want the last, %d", shown.Rounds, planReviewMaxRounds)
		}
	}
	rawSpecDepartures := shapes("specDepartures")
	var specDepartures []string
	for _, departure := range rawSpecDepartures {
		var shown map[string]any
		if err := json.Unmarshal([]byte(departure), &shown); err != nil {
			t.Fatalf("planner.md's specDepartures %s: %v", departure, err)
		}
		if _, isDeparture := shown["spec"]; isDeparture {
			specDepartures = append(specDepartures, departure)
		}
	}
	if len(gapAnalyses) != 2 || !slices.Equal(verdicts, []string{"approved", "rejected", "failed"}) || len(specDepartures) != 1 {
		t.Fatalf("planner.md shows gapAnalysis %v, planReview verdicts %v and specDepartures %v, want two shapes, approved, rejected, failed, and one departure", gapAnalyses, verdicts, specDepartures)
	}
	skills := `{"implement":["none: x"],"test":["none: x"],"review":["none: x"]}`
	departures := `[` + strings.Join(specDepartures, ",") + `]`
	for _, gapAnalysis := range gapAnalyses {
		for _, planReview := range planReviews {
			data := `{"requiredSkills":` + skills + `,"gapAnalysis":` + gapAnalysis + `,"planReview":` + planReview + `,"specDepartures":` + departures + `}`
			var out, errb bytes.Buffer
			if code := run(context.Background(), []string{"legion", "handoff", "write", "--workspace", t.TempDir(), "--phase", "plan", "--data", data}, &out, &errb); code != 0 {
				t.Fatalf("handoff write --phase plan %s = %d: %s", data, code, errb.String())
			}
		}
	}
}

// A tester whose handoff is missing is refused before any request, and the refusal names the file
// its phase ends with, .legion/test.json.
func TestHandoffCompleteRefusesAMissingPhaseFileBeforeTheRequest(t *testing.T) {
	workspace, jj := handoffRepo(t)
	t.Setenv("LEGION_ROLE", "tester")
	t.Setenv("LEGION_JJ_PATH", jj)
	bodies := handoffDaemon(t, phase.Testing)
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "tests passed", "--verdict", "pass"}, &out, &errb)
	if code != 1 || len(*bodies) != 0 || !strings.Contains(errb.String(), filepath.Join(".legion", "test.json")) {
		t.Fatalf("handoff complete = %d, daemon read %v, stderr %q; want a refusal naming the tester's handoff file, .legion/test.json, before any request", code, *bodies, errb.String())
	}
}

// fakeHandoffJJ writes the jj a pane is told as LEGION_JJ_PATH, which reports the handoff committed
// (no working-copy change) and names commit as the commit carrying it; a decoy jj first on PATH
// fails naming itself.
func fakeHandoffJJ(t *testing.T, commit string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jj")
	script := `#!/bin/sh
case " $* " in
*" diff "*) ;;
*" log "*) printf '%s' "` + commit + `" ;;
*" remote list "*) echo "origin https://github.com/acme/widgets.git" ;;
*) echo "unexpected jj $*" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake jj: %v", err)
	}
	decoys := t.TempDir()
	if err := os.WriteFile(filepath.Join(decoys, "jj"), []byte("#!/bin/sh\necho 'the jj on PATH ran' >&2\nexit 97\n"), 0o700); err != nil {
		t.Fatalf("write the decoy jj: %v", err)
	}
	t.Setenv("PATH", decoys+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

// handoffDaemon is the daemon a pane completes its phase with: it serves the daemon's state
// document (GET /legion/v1/state, internal/api/state.go) with the pane's issue, THIS-1, in phase p,
// answers /legion/v1/handoff/complete, and hands back the completion bodies it read.
func handoffDaemon(t *testing.T, p phase.Phase) *[]map[string]any {
	t.Helper()
	bodies := &[]map[string]any{}
	state := api.State{Issues: map[string]api.Issue{
		"THIS-1":  {Key: "THIS-1", Generation: 1, Phase: p, PullRequest: &api.PullRequestView{Number: 42, Head: "c0de"}},
		"OTHER-2": {Key: "OTHER-2", Generation: 1, Phase: phase.Merging},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/legion/v1/state":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(state)
		case r.Method == http.MethodPost && r.URL.Path == "/legion/v1/gh-token":
			_, _ = w.Write([]byte(`{"token":"installation-token","appLogin":"legion-implementer[bot]"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/legion/v1/handoff/complete":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			*bodies = append(*bodies, body)
			_, _ = w.Write([]byte("{}"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("LEGION_DAEMON_URL", server.URL)
	t.Setenv("LEGION_ISSUE", "THIS-1")
	t.Setenv("LEGION_GRANT_FILE", "")
	if err := os.Unsetenv("LEGION_GRANT_FILE"); err != nil {
		t.Fatalf("unset LEGION_GRANT_FILE: %v", err)
	}
	t.Setenv("LEGION_GRANT", "grant-1")
	// The pane's identity is the one the daemon puts on it; a caller's exported JJ_USER/JJ_EMAIL
	// would otherwise be what `ownHandoff` compares the commit's author against, so whether these
	// tests pass would depend on the shell that ran them.
	t.Setenv("JJ_USER", "")
	t.Setenv("JJ_EMAIL", "")
	readyGitHub(t, `{"id":1,"name":"ci","status":"completed","conclusion":"success"}`, `{"context":"legacy","state":"success"}`, "clean")
	return bodies
}

// readyGitHub serves acme/widgets#42 to a merger's READY check: head c0de on main, whose ruleset
// requires the check "ci" and whose branch protection requires the status "legacy", reporting the
// given check run and commit status on the head (either may be empty) and the pull request's
// mergeable_state, plus the token route of the daemon the check redeems its grant at.
func readyGitHub(t *testing.T, checkRun, status, mergeableState string) {
	t.Helper()
	repo := "/repos/acme/widgets"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case repo + "/pulls/42":
			_, _ = w.Write([]byte(`{"head":{"sha":"c0de0000000000000000000000000000000000ff"},"base":{"ref":"main"},"mergeable_state":"` + mergeableState + `"}`))
		case repo + "/rules/branches/main":
			_, _ = w.Write([]byte(`[{"type":"pull_request"},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"ci"}]}}]`))
		case repo + "/branches/main":
			_, _ = w.Write([]byte(`{"name":"main","protection":{"required_status_checks":{"contexts":["legacy"],"checks":[{"context":"legacy"}]}}}`))
		case repo + "/commits/c0de0000000000000000000000000000000000ff/check-runs":
			runs := "[]"
			if checkRun != "" {
				runs = "[" + checkRun + "]"
			}
			_, _ = w.Write([]byte(`{"total_count":` + strconv.Itoa(strings.Count(runs, `"name"`)) + `,"check_runs":` + runs + `}`))
		case repo + "/commits/c0de0000000000000000000000000000000000ff/status":
			statuses := "[]"
			if status != "" {
				statuses = "[" + status + "]"
			}
			_, _ = w.Write([]byte(`{"statuses":` + statuses + `}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("LEGION_GITHUB_API_URL", server.URL)
}

// `legion handoff complete` resolves the committed handoff with the jj the daemon resolved at boot
// (LEGION_JJ_PATH, set on every pane) and refuses without it: never a PATH lookup.
func TestHandoffCompleteResolvesTheCommitWithTheJJBootResolved(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, ".legion"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".legion", "test.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGION_ROLE", "tester")
	jj := fakeHandoffJJ(t, "c0ffee")
	bodies := handoffDaemon(t, phase.Testing)
	args := []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "tests passed", "--verdict", "pass"}

	t.Setenv("LEGION_JJ_PATH", "")
	var out, errb bytes.Buffer
	if code := run(context.Background(), args, &out, &errb); code != 1 || !strings.Contains(errb.String(), "LEGION_JJ_PATH") {
		t.Fatalf("handoff complete without LEGION_JJ_PATH = %d, stderr %q; want a refusal naming it", code, errb.String())
	}

	t.Setenv("LEGION_JJ_PATH", jj)
	errb.Reset()
	if code := run(context.Background(), args, &out, &errb); code != 0 {
		t.Fatalf("handoff complete = %d, stderr %q", code, errb.String())
	}
	if len(*bodies) != 1 || (*bodies)[0]["commit"] != "c0ffee" {
		t.Fatalf("daemon read %v, want one completion naming commit c0ffee", *bodies)
	}
}

// The merger verifies and publishes READY and writes no handoff (internal/prompts/roles/merger.md:
// "merger is not a file-backed phase"), so its completion needs no .legion file and reports the
// commit its workspace sits on.
func TestHandoffCompleteReadyForTheMergerNeedsNoHandoffFile(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("LEGION_ROLE", "merger")
	t.Setenv("LEGION_JJ_PATH", fakeHandoffJJ(t, "beef"))
	bodies := handoffDaemon(t, phase.Merging)
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "gate facts hold", "--ready"}, &out, &errb); code != 0 {
		t.Fatalf("merger handoff complete --ready = %d, stderr %q", code, errb.String())
	}
	if len(*bodies) != 1 || (*bodies)[0]["ready"] != true || (*bodies)[0]["commit"] != "beef" {
		t.Fatalf("daemon read %v, want one READY naming commit beef", *bodies)
	}
}

// The completion names no run: the pane's own LEGION_GENERATION is the claim's launch counter,
// which moves on every relaunch within one run, and a live worker is handed the next run's task
// without being relaunched at all. The daemon attributes the completion to the delivery whose
// turn is running (internal/api's TestHandoffCompleteCarriesTheRunOfTheTaskBeingWorked), so the
// CLI sends exactly the fields the grant does not already carry.
func TestHandoffCompleteSendsNoGeneration(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("LEGION_ROLE", "merger")
	t.Setenv("LEGION_GENERATION", "7")
	t.Setenv("LEGION_JJ_PATH", fakeHandoffJJ(t, "beef"))
	bodies := handoffDaemon(t, phase.Merging)
	var out, errb bytes.Buffer

	if code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "gate facts hold", "--ready"}, &out, &errb); code != 0 {
		t.Fatalf("handoff complete = %d, stderr %q", code, errb.String())
	}

	if len(*bodies) != 1 {
		t.Fatalf("daemon read %v, want one completion", *bodies)
	}
	if _, sent := (*bodies)[0]["generation"]; sent {
		t.Fatalf("the completion carries %v, want no generation: the daemon reads the run from the delivery", (*bodies)[0]["generation"])
	}
}

// handoffRepo is a real colocated jj repository standing in for a pane workspace. It returns the
// workspace and the absolute jj a pane is told as LEGION_JJ_PATH.
func handoffRepo(t *testing.T) (string, string) {
	t.Helper()
	jj, err := exec.LookPath("jj")
	if err != nil {
		t.Fatalf("jj is required: %v", err)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	handoffJJ(t, jj, filepath.Dir(workspace), "git", "init", "--colocate", workspace)
	return workspace, jj
}

func handoffJJ(t *testing.T, jj, dir string, args ...string) string {
	t.Helper()
	command := exec.Command(jj, args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "JJ_USER=Legion test", "JJ_EMAIL=legion-test@example.invalid")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("jj %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func writeHandoffFile(t *testing.T, workspace, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(workspace, ".legion"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".legion", name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Every file-backed role prompt tells its pane to write its handoff under the phase word
// (internal/prompts/roles/*.md: the legion tool's handoff_write with phase plan|implement|test|review), while
// the pane's LEGION_ROLE is the claim word (planner, implementer, tester, reviewer). A pane that
// follows its prompt from its workspace and commits the handoff must be able to complete its
// phase, reporting the commit that carries that handoff.
func TestHandoffCompleteAcceptsTheHandoffItsRolePromptWrites(t *testing.T) {
	for _, tc := range []struct {
		role, phase, verdict string
		current              phase.Phase
	}{
		{"planner", "plan", "", phase.Planning},
		{"implementer", "implement", "", phase.Implementing},
		{"tester", "test", "pass", phase.Testing},
		{"reviewer", "review", "", phase.Reviewing},
	} {
		t.Run(tc.role, func(t *testing.T) {
			workspace, jj := handoffRepo(t)
			t.Chdir(workspace)
			var out, errb bytes.Buffer
			if code := run(context.Background(), []string{"legion", "handoff", "write", "--phase", tc.phase, "--data", writableHandoffs[tc.phase]}, &out, &errb); code != 0 {
				t.Fatalf("handoff write --phase %s = %d: %s", tc.phase, code, errb.String())
			}
			handoffJJ(t, jj, workspace, "commit", "-m", tc.phase+": record handoff")
			carrying := handoffJJ(t, jj, workspace, "log", "-r", "@-", "--no-graph", "-T", "commit_id")
			t.Setenv("LEGION_ROLE", tc.role)
			t.Setenv("LEGION_JJ_PATH", jj)
			bodies := handoffDaemon(t, tc.current)
			args := []string{"legion", "handoff", "complete", "--summary", "phase done"}
			if tc.verdict != "" {
				args = append(args, "--verdict", tc.verdict)
			}
			errb.Reset()
			if code := run(context.Background(), args, &out, &errb); code != 0 {
				t.Fatalf("%s handoff complete after committing .legion/%s.json = %d, stderr %q", tc.role, tc.phase, code, errb.String())
			}
			if len(*bodies) != 1 || (*bodies)[0]["commit"] != carrying {
				t.Fatalf("daemon read %v, want one completion naming %s, the commit carrying .legion/%s.json", *bodies, carrying, tc.phase)
			}
		})
	}
}

// The Stage 3 proof's primary issue, reduced: the base branch already carries .legion handoffs from
// an earlier merged pull request (sjawhar/legion-smoke main holds .legion/implementer.json from #89
// and .legion/implement.json from a later merge), the implementer commits its product change, and
// its fresh handoff is still uncommitted in @. Run from the workspace, as a pane runs it, the
// completion must refuse: the commit it would report carries another issue's handoff, not this
// phase's.
func TestHandoffCompleteRefusesWhenOnlyAStaleBaseHandoffIsCommitted(t *testing.T) {
	workspace, jj := handoffRepo(t)
	t.Chdir(workspace)
	writeHandoffFile(t, workspace, "implementer.json", `{"issue":"EARLIER-1"}`+"\n")
	writeHandoffFile(t, workspace, "implement.json", `{"issue":"EARLIER-1"}`+"\n")
	handoffJJ(t, jj, workspace, "commit", "-m", "an earlier merged pull request")
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("smoke\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	handoffJJ(t, jj, workspace, "commit", "-m", "feat: the product change")
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "handoff", "write", "--phase", "implement", "--data", writableHandoffs["implement"]}, &out, &errb); code != 0 {
		t.Fatalf("handoff write = %d: %s", code, errb.String())
	}
	t.Setenv("LEGION_ROLE", "implementer")
	t.Setenv("LEGION_JJ_PATH", jj)
	bodies := handoffDaemon(t, phase.Implementing)
	errb.Reset()
	code := run(context.Background(), []string{"legion", "handoff", "complete", "--summary", "implemented"}, &out, &errb)
	if code != 1 || len(*bodies) != 0 {
		t.Fatalf("handoff complete with this phase's handoff uncommitted = %d, daemon read %v, stderr %q; want a refusal before any request", code, *bodies, errb.String())
	}
}

// --workspace names the pane workspace from any directory. A handoff committed there completes the
// phase whatever the caller's working directory: the committed-handoff check must not resolve the
// handoff path against the caller's directory.
func TestHandoffCompleteWithWorkspaceFlagIgnoresTheCallersDirectory(t *testing.T) {
	workspace, jj := handoffRepo(t)
	writeHandoffFile(t, workspace, "implement.json", `{"issue":"THIS-1"}`+"\n")
	handoffJJ(t, jj, workspace, "commit", "-m", "implement: record handoff")
	carrying := handoffJJ(t, jj, workspace, "log", "-r", "@-", "--no-graph", "-T", "commit_id")
	t.Chdir(t.TempDir())
	t.Setenv("LEGION_ROLE", "implementer")
	t.Setenv("LEGION_JJ_PATH", jj)
	bodies := handoffDaemon(t, phase.Implementing)
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "implemented"}, &out, &errb); code != 0 {
		t.Fatalf("handoff complete --workspace %s from another directory = %d, stderr %q", workspace, code, errb.String())
	}
	if len(*bodies) != 1 || (*bodies)[0]["commit"] != carrying {
		t.Fatalf("daemon read %v, want one completion naming %s", *bodies, carrying)
	}
}

// A pane's workspace is cloned from the repository, so the base branch is its origin's main
// (trunk()). A handoff that only the base carries — main already holds .legion/implement.json from
// an earlier merged pull request, and this phase wrote none — is inherited, never this phase's: the
// completion refuses before any request, even with nothing uncommitted in the workspace.
func TestHandoffCompleteRefusesAHandoffOnlyTheOriginsMainCarries(t *testing.T) {
	seed, jj := handoffRepo(t)
	writeHandoffFile(t, seed, "implement.json", `{"issue":"EARLIER-1"}`+"\n")
	handoffJJ(t, jj, seed, "commit", "-m", "an earlier merged pull request")
	handoffJJ(t, jj, seed, "bookmark", "set", "main", "-r", "@-")
	workspace := filepath.Join(t.TempDir(), "workspace")
	handoffJJ(t, jj, filepath.Dir(workspace), "git", "clone", seed, workspace)
	trunk := handoffJJ(t, jj, workspace, "log", "-r", "trunk()", "--no-graph", "-T", "commit_id")
	if origin := handoffJJ(t, jj, workspace, "log", "-r", "main@origin", "--no-graph", "-T", "commit_id"); trunk != origin {
		t.Fatalf("trunk() in the clone is %q, want origin's main %q", trunk, origin)
	}
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("smoke\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	handoffJJ(t, jj, workspace, "commit", "-m", "feat: the product change")
	t.Chdir(workspace)
	t.Setenv("LEGION_ROLE", "implementer")
	t.Setenv("LEGION_JJ_PATH", jj)
	bodies := handoffDaemon(t, phase.Implementing)
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "handoff", "complete", "--summary", "implemented"}, &out, &errb)
	if code != 1 || len(*bodies) != 0 || !strings.Contains(errb.String(), "only the base branch carries it") {
		t.Fatalf("handoff complete with only origin's main carrying .legion/implement.json = %d, daemon read %v, stderr %q; want the inherited-handoff refusal before any request", code, *bodies, errb.String())
	}
}

// The implementer's production check writes no handoff: skills/legion-worker/SKILL.md's completion
// gate says the post-merge production check "writes no .legion/<phase>.json, commits no handoff,
// and reports with `handoff_complete` alone", and the daemon's workflow does not treat
// production_check as file-backed (internal/workflow/effects.go fileBacked). The phase starts only
// after the ordinary human squash merge deleted the issue branch, so the implementer's
// `jj git fetch` abandons the branch and leaves `@` on main, which now carries the merged
// .legion/implement.json. The daemon's state names the issue's phase; the completion must reach
// the daemon. In the Stage 3 acceptance run at 40a40069 the implementer was refused here twice
// ("only the base branch carries it") and completed only after resurrecting the deleted branch
// with `jj new <its old head>`.
func TestHandoffCompleteReportsTheProductionCheckOnTheMergedMain(t *testing.T) {
	seed, jj := handoffRepo(t)
	writeHandoffFile(t, seed, "implement.json", `{"issue":"THIS-1"}`+"\n")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("smoke\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	handoffJJ(t, jj, seed, "commit", "-m", "feat: the smoke change (#1)")
	handoffJJ(t, jj, seed, "bookmark", "set", "main", "-r", "@-")
	workspace := filepath.Join(t.TempDir(), "workspace")
	handoffJJ(t, jj, filepath.Dir(workspace), "git", "clone", seed, workspace)
	merged := handoffJJ(t, jj, workspace, "log", "-r", "trunk()", "--no-graph", "-T", "commit_id")
	if parent := handoffJJ(t, jj, workspace, "log", "-r", "@-", "--no-graph", "-T", "commit_id"); parent != merged {
		t.Fatalf("the workspace's @- is %q, want the merged main %q", parent, merged)
	}
	t.Chdir(workspace)
	t.Setenv("LEGION_ROLE", "implementer")
	t.Setenv("LEGION_JJ_PATH", jj)
	bodies := handoffDaemon(t, phase.ProductionCheck)
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "handoff", "complete", "--summary", "production check verified"}, &out, &errb); code != 0 {
		t.Fatalf("implementer handoff complete in production_check on the merged main = %d, stderr %q", code, errb.String())
	}
	if len(*bodies) != 1 {
		t.Fatalf("daemon read %v, want one production-check completion", *bodies)
	}
}

// The phase decides what a completion reports, and it is read before anything else. Retro writes
// no handoff, so the implementer's retro reports the commit its workspace stands on, never the
// commit that carried its last implementing handoff: that one was already reported, and the same
// commit in a file-backed phase would be refused as not new. A role reporting a phase it does not
// run — a tester whose issue moved on to reviewing — is not told to write another role's handoff:
// its completion reaches the daemon, which answers whose phase it is.
func TestHandoffCompleteReadsThePhaseBeforeTheHandoff(t *testing.T) {
	for _, tc := range []struct {
		name, role string
		current    phase.Phase
	}{
		{name: "the implementer's retro", role: "implementer", current: phase.Retro},
		{name: "a tester after its phase", role: "tester", current: phase.Reviewing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace, jj := handoffRepo(t)
			t.Chdir(workspace)
			writeHandoffFile(t, workspace, "implement.json", `{"issue":"THIS-1"}`+"\n")
			handoffJJ(t, jj, workspace, "commit", "-m", "implement: record handoff")
			if err := os.WriteFile(filepath.Join(workspace, "docs.md"), []byte("learning\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			handoffJJ(t, jj, workspace, "commit", "-m", "docs: the retro's learning")
			standing := handoffJJ(t, jj, workspace, "log", "-r", "@-", "--no-graph", "-T", "commit_id")
			t.Setenv("LEGION_ROLE", tc.role)
			t.Setenv("LEGION_JJ_PATH", jj)
			bodies := handoffDaemon(t, tc.current)
			args := []string{"legion", "handoff", "complete", "--summary", "done"}
			if tc.role == "tester" {
				args = append(args, "--verdict", "pass")
			}
			var out, errb bytes.Buffer
			if code := run(context.Background(), args, &out, &errb); code != 0 {
				t.Fatalf("%s handoff complete in %s = %d, stderr %q", tc.role, tc.current, code, errb.String())
			}
			if len(*bodies) != 1 || (*bodies)[0]["commit"] != standing {
				t.Fatalf("daemon read %v, want one completion naming %s, the commit the workspace stands on", *bodies, standing)
			}
		})
	}
}

// A handoff is written under its phase word, the one every role prompt passes to the legion tool's
// handoff_write (plan, implement, test, review, and the sub-architect's architect), and a pane's
// LEGION_ROLE is its claim role. A role word as a phase would write a file nothing reads, and a
// phase word as the role names no claim, so each is refused.
func TestHandoffTakesPhaseWordsForPhasesAndRolesForRoles(t *testing.T) {
	workspace := t.TempDir()
	var out, errb bytes.Buffer
	for _, args := range [][]string{
		{"write", "--phase", "planner", "--data", `{"summary":"done"}`},
		{"write", "--phase", "merge", "--data", `{"summary":"done"}`},
		{"read", "--phase", "implementer"},
	} {
		errb.Reset()
		if code := run(context.Background(), append([]string{"legion", "handoff", args[0], "--workspace", workspace}, args[1:]...), &out, &errb); code != 2 {
			t.Fatalf("legion handoff %v = %d, stderr %q; want the usage refusal", args, code, errb.String())
		}
	}
	if entries, err := os.ReadDir(filepath.Join(workspace, ".legion")); !os.IsNotExist(err) {
		t.Fatalf(".legion after the refused writes: %v %v, want none", entries, err)
	}
	t.Setenv("LEGION_ROLE", "test")
	bodies := handoffDaemon(t, phase.Testing)
	errb.Reset()
	if code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "done", "--verdict", "pass"}, &out, &errb); code != 1 || len(*bodies) != 0 || !strings.Contains(errb.String(), "LEGION_ROLE") {
		t.Fatalf("handoff complete with LEGION_ROLE=test = %d, daemon read %v, stderr %q; want a refusal naming LEGION_ROLE", code, *bodies, errb.String())
	}
}

// The end game every clean review round ends in (skills/legion-worker/references/merge-gate.md: the reviewer
// approves only a head that carries no .legion/, and the implementer pushes the .legion/
// deletion): once the branch head has deleted .legion/, the implementer and the tester report
// completion without recreating it (skills/legion-worker/SKILL.md's completion gate: once .legion/ is gone, "a
// later rebase, bare-gate re-check, confirmation, retro, or the post-merge production check writes
// no .legion/<phase>.json, commits no handoff, and reports with `handoff_complete` alone";
// internal/prompts/roles/implementer.md and tester.md say the same). The commit that deleted the
// handoff is the last commit on the branch that changed it, and the completion reports it.
func TestHandoffCompleteAfterTheLegionDeletionRecreatesNothing(t *testing.T) {
	for _, tc := range []struct {
		role, verdict string
		current       phase.Phase
	}{
		{"implementer", "", phase.Implementing},
		{"tester", "pass", phase.Testing},
	} {
		t.Run(tc.role, func(t *testing.T) {
			workspace, jj := handoffRepo(t)
			t.Chdir(workspace)
			if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("smoke\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			handoffJJ(t, jj, workspace, "commit", "-m", "feat: the product change")
			writeHandoffFile(t, workspace, "implement.json", `{"issue":"THIS-1"}`+"\n")
			handoffJJ(t, jj, workspace, "commit", "-m", "implement: record handoff")
			writeHandoffFile(t, workspace, "test.json", `{"issue":"THIS-1"}`+"\n")
			handoffJJ(t, jj, workspace, "commit", "-m", "test: record handoff")
			if err := os.RemoveAll(filepath.Join(workspace, ".legion")); err != nil {
				t.Fatal(err)
			}
			handoffJJ(t, jj, workspace, "commit", "-m", "chore: remove .legion/ before approval")
			deletion := handoffJJ(t, jj, workspace, "log", "-r", "@-", "--no-graph", "-T", "commit_id")
			t.Setenv("LEGION_ROLE", tc.role)
			t.Setenv("LEGION_JJ_PATH", jj)
			bodies := handoffDaemon(t, tc.current)
			args := []string{"legion", "handoff", "complete", "--summary", "the .legion/ deletion is pushed"}
			if tc.verdict != "" {
				args = append(args, "--verdict", tc.verdict)
			}
			var out, errb bytes.Buffer
			if code := run(context.Background(), args, &out, &errb); code != 0 {
				t.Fatalf("%s handoff complete after the .legion/ deletion = %d, stderr %q", tc.role, code, errb.String())
			}
			if len(*bodies) != 1 || (*bodies)[0]["commit"] != deletion {
				t.Fatalf("daemon read %v, want one completion naming %s, the commit that deleted .legion/", *bodies, deletion)
			}
			if _, err := os.Stat(filepath.Join(workspace, ".legion")); !os.IsNotExist(err) {
				t.Fatalf(".legion/ after the completion: %v, want it still absent", err)
			}
		})
	}
}

// A handoff commit is its own role's: the pane's App identity (JJ_USER/JJ_EMAIL, which the daemon
// sets on every pane) authors it. A tester whose handoff landed in the implementer's commit is
// refused before any request, naming the author and the fix; the same handoff committed by the
// tester itself completes.
func TestHandoffCompleteRefusesAHandoffCommitAnotherAppAuthored(t *testing.T) {
	for _, tc := range []struct {
		name   string
		author string
		ok     bool
	}{
		{name: "authored by the implementer", author: "legion-implementer[bot]"},
		{name: "authored by the tester", author: "legion-reviewer[bot]", ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace, jj := handoffRepo(t)
			t.Chdir(workspace)
			as := func(args ...string) {
				t.Helper()
				command := exec.Command(jj, args...)
				command.Dir = workspace
				command.Env = append(os.Environ(), "JJ_USER="+tc.author, "JJ_EMAIL=bot@example.invalid")
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("jj %v: %v\n%s", args, err, output)
				}
			}
			// The commit the handoff lands in is authored by tc.author, as a role's own fresh
			// working copy is.
			as("new")
			writeHandoffFile(t, workspace, "test.json", `{"issue":"THIS-1"}`+"\n")
			as("commit", "-m", "test: record handoff")
			t.Setenv("LEGION_ROLE", "tester")
			t.Setenv("LEGION_JJ_PATH", jj)
			bodies := handoffDaemon(t, phase.Testing)
			// After the harness, which clears whatever identity the calling shell exported.
			t.Setenv("JJ_USER", "legion-reviewer[bot]")
			t.Setenv("JJ_EMAIL", "bot@example.invalid")
			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "handoff", "complete", "--summary", "tests pass", "--verdict", "pass"}, &out, &errb)
			if tc.ok {
				if code != 0 || len(*bodies) != 1 {
					t.Fatalf("handoff complete on the tester's own commit = %d, daemon read %v, stderr %q", code, *bodies, errb.String())
				}
				return
			}
			if code != 1 || len(*bodies) != 0 || !strings.Contains(errb.String(), "legion-implementer[bot]") || !strings.Contains(errb.String(), "jj new") {
				t.Fatalf("handoff complete on the implementer's commit = %d, daemon read %v, stderr %q; want a refusal naming the author and jj new, before any request", code, *bodies, errb.String())
			}
		})
	}
}

// A merger's READY names a head a human merges, which GitHub merges only once every check the base
// branch requires has succeeded there. A head whose push skipped CI when it should not have reports
// none of them, so the completion refuses READY naming the head and the check, and nothing reaches
// the daemon; a required check still running, or one that failed, is refused the same way.
//
// A pull request that conflicts with its base (mergeable_state "dirty") gets no pull_request run,
// so a required check with no result on its head is refused naming the conflict rather than a
// skipped push. The conflict changes only that text, never which heads are refused: every row is
// posted or refused by its checks alone, whatever its mergeable_state, and a conflicting head
// whose required checks all succeeded is posted.
func TestHandoffCompleteReadyRefusesAHeadWithoutItsRequiredChecksGreen(t *testing.T) {
	const (
		ciGreen      = `{"id":1,"name":"ci","status":"completed","conclusion":"success"}`
		ciSkipped    = `{"id":1,"name":"ci","status":"completed","conclusion":"skipped"}`
		ciRunning    = `{"id":1,"name":"ci","status":"in_progress","conclusion":null}`
		legacyGreen  = `{"context":"legacy","state":"success"}`
		legacyFailed = `{"context":"legacy","state":"failure"}`
		skippedPush  = `: its push may have skipped CI`
		conflict     = `: the pull request conflicts with main, and GitHub starts no pull_request CI`
	)
	for _, tc := range []struct {
		name, checkRun, status, mergeableState, refusal string
	}{
		{"every required check green", ciGreen, legacyGreen, "clean", ""},
		{"a required check that ended skipped counts", ciSkipped, legacyGreen, "clean", ""},
		{"every required check green on a conflicting pull request", ciGreen, legacyGreen, "dirty", ""},
		{"a head whose push skipped CI", "", "", "blocked", `head c0de00000000 of pull request #42 has no result for the required check "ci"` + skippedPush},
		{"a head whose mergeability GitHub has not computed", "", "", "unknown", `head c0de00000000 of pull request #42 has no result for the required check "ci"` + skippedPush},
		{"a head of a conflicting pull request", "", "", "dirty", `head c0de00000000 of pull request #42 has no result for the required check "ci"` + conflict},
		{"a conflicting pull request missing one required check", ciGreen, "", "dirty", `head c0de00000000 of pull request #42 has no result for the required check "legacy"` + conflict},
		{"a required check still running", ciRunning, legacyGreen, "blocked", `the required check "ci" is still running on head c0de00000000`},
		{"a required check still running on a conflicting pull request", ciRunning, legacyGreen, "dirty", `the required check "ci" is still running on head c0de00000000`},
		{"a required status that failed", ciGreen, legacyFailed, "blocked", `the required check "legacy" ended failure on head c0de00000000`},
		{"a required status that failed on a conflicting pull request", ciGreen, legacyFailed, "dirty", `the required check "legacy" ended failure on head c0de00000000`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			t.Setenv("LEGION_ROLE", "merger")
			t.Setenv("LEGION_JJ_PATH", fakeHandoffJJ(t, "beef"))
			bodies := handoffDaemon(t, phase.Merging)
			readyGitHub(t, tc.checkRun, tc.status, tc.mergeableState)
			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "gate facts hold", "--ready"}, &out, &errb)
			if tc.refusal == "" {
				if code != 0 || len(*bodies) != 1 {
					t.Fatalf("READY = %d, daemon read %v, stderr %q; want it posted", code, *bodies, errb.String())
				}
				return
			}
			if code != 1 || len(*bodies) != 0 {
				t.Fatalf("READY = %d, daemon read %v, stderr %q; want it refused and nothing posted", code, *bodies, errb.String())
			}
			if !strings.Contains(errb.String(), "READY refused: "+tc.refusal) {
				t.Fatalf("READY refused with stderr %q; want the refusal to name %q", errb.String(), tc.refusal)
			}
		})
	}
}

// A private repository whose plan has no rulesets answers the rulesets read 403, "make this
// repository public to enable this feature" (docs/solutions/legion/controller-gate-2-required-checks-live-reads.md).
// It can define no ruleset, so none requires a check there, and READY rests on the branch's
// protection alone: posted when that requires nothing, refused when it requires a check the head
// lacks. Only that answer means no rulesets: a rulesets read that fails otherwise (another 403,
// such as a token that lost access, or a server error) leaves the required checks unknown, and
// READY is refused naming the read.
func TestHandoffCompleteReadyOnARepositoryWhosePlanHasNoRulesets(t *testing.T) {
	const planAnswer = `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature.","status":"403"}`
	unprotected := `{"name":"main","protected":false}`
	for _, tc := range []struct {
		name, branch string
		rulesStatus  int
		rulesBody    string
		posted       bool
		refusal      string
	}{
		{"and no branch protection", unprotected, http.StatusForbidden, planAnswer, true, ""},
		{"and branch protection requiring a check the head lacks", `{"name":"main","protected":true,"protection":{"required_status_checks":{"contexts":["legacy"]}}}`, http.StatusForbidden, planAnswer, false, `has no result for the required check "legacy"`},
		{"but the read is refused for another reason", unprotected, http.StatusForbidden, `{"message":"Resource not accessible by integration","status":"403"}`, false, "GET /rules/branches/main with 403"},
		{"but the read fails", unprotected, http.StatusInternalServerError, `{"message":"Server Error"}`, false, "GET /rules/branches/main with 500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := "/repos/acme/widgets"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case repo + "/pulls/42":
					_, _ = w.Write([]byte(`{"head":{"sha":"c0de0000000000000000000000000000000000ff"},"base":{"ref":"main"}}`))
				case repo + "/rules/branches/main":
					w.WriteHeader(tc.rulesStatus)
					_, _ = w.Write([]byte(tc.rulesBody))
				case repo + "/branches/main":
					_, _ = w.Write([]byte(tc.branch))
				case repo + "/commits/c0de0000000000000000000000000000000000ff/check-runs":
					_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
				case repo + "/commits/c0de0000000000000000000000000000000000ff/status":
					_, _ = w.Write([]byte(`{"statuses":[]}`))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			workspace := t.TempDir()
			t.Setenv("LEGION_ROLE", "merger")
			t.Setenv("LEGION_JJ_PATH", fakeHandoffJJ(t, "beef"))
			bodies := handoffDaemon(t, phase.Merging)
			t.Setenv("LEGION_GITHUB_API_URL", server.URL)
			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "gate facts hold", "--ready"}, &out, &errb)
			if tc.posted {
				// A base branch requiring no check has nothing to refuse, and READY says so rather
				// than reading like a head whose every required check was read and passed.
				if code != 0 || len(*bodies) != 1 || !strings.Contains(out.String(), `no check is required on "main" of acme/widgets`) {
					t.Fatalf("READY = %d, daemon read %v, stdout %q, stderr %q; want it posted, saying it read no checks", code, *bodies, out.String(), errb.String())
				}
				return
			}
			if code != 1 || len(*bodies) != 0 || !strings.Contains(errb.String(), tc.refusal) {
				t.Fatalf("READY = %d, daemon read %v, stderr %q; want a refusal naming %q", code, *bodies, errb.String(), tc.refusal)
			}
		})
	}
}
