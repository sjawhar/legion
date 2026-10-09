package handoff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// handoffProof is one production-like proof, every field of it non-blank.
const handoffProof = `{"criterion":"a handoff without proof is refused","surface":"the daemon's completion route",` +
	`"command":"handoff_complete after pushing implement.json without proof","observed":"422 HANDOFF_INVALID naming proof",` +
	`"headSha":"0123456789abcdef0123456789abcdef01234567","negativeControl":"the same file with its proof: 200"}`

// verifiedProof is a tester's verdict on the implementer's proof that holds.
const verifiedProof = `{"verdict":"verified","how":"re-ran its command at its head"}`

// validHandoffs are a handoff of each phase its rules accept, as its role prompt has the worker
// write it, before the stamps.
var validHandoffs = map[string]string{
	"architect": `{"scope":"small","subIssues":[]}`,
	"plan": `{"requiredSkills":{"implement":["legion-worker"],"test":["legion-worker"],"review":["none: no skill covers a one-line change"]},` +
		`"gapAnalysis":{"findings":[]},"planReview":{"verdict":"approved","rounds":1},"specDepartures":[]}`,
	"implement": `{"filesChanged":["x.go"],"proof":[` + handoffProof + `]}`,
	"test":      `{"passed":3,"failed":0,"implementerProof":` + verifiedProof + `,"proof":[` + handoffProof + `]}`,
	"review":    `{"verdict":"approved","critical":0}`,
}

// decode is data as the JSON object a handoff file holds.
func decode(t *testing.T, data string) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(data), &object); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return object
}

// stamped is data with the four stamps a worker writes on a handoff of phase for THIS-1.
func stamped(t *testing.T, phase, data string) map[string]any {
	t.Helper()
	object := decode(t, data)
	object["schemaVersion"], object["phase"], object["issue"], object["completed"] = float64(1), phase, "THIS-1", "2026-10-09T08:00:00Z"
	return object
}

// fields is the field each problem names, in order.
func fields(problems []string) []string {
	var named []string
	for _, problem := range problems {
		field, _, _ := strings.Cut(problem, ": ")
		named = append(named, field)
	}
	return named
}

// A handoff is refused when its phase's rules refuse it, naming every field at fault so the next
// write can fix it. The implementer's proof, the tester's verdict on it and its own proof, and the
// plan's skills, two checks and departures are the records the next role reads; a declared field of
// the wrong type would have that role read a value that is not there.
func TestProblemsNamesEachFieldItsPhasesRulesRefuse(t *testing.T) {
	blankProof := strings.Replace(strings.Replace(handoffProof, `"422 HANDOFF_INVALID naming proof"`, `"  "`, 1), `,"negativeControl":"the same file with its proof: 200"`, "", 1)
	plan := func(field, value string) string {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(validHandoffs["plan"]), &fields); err != nil {
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
		if err := json.Unmarshal([]byte(validHandoffs["plan"]), &fields); err != nil {
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
			problems := Problems(tc.phase, "THIS-1", stamped(t, tc.phase, tc.data))
			if named := fields(problems); !slices.Equal(named, tc.fields) {
				t.Fatalf("problems %q name %v, want %v", problems, named, tc.fields)
			}
			for _, problem := range problems {
				if _, reason, found := strings.Cut(problem, ": "); !found || strings.TrimSpace(reason) == "" {
					t.Fatalf("problem %q is not `<field>: <reason>`", problem)
				}
			}
		})
	}
}

// What the rules allow holds, every field the phase does not declare included: a test handoff that
// records its failure needs no proof of its own, a plan may say no skill fits and record a failed
// check as its error, and a rejection after the last round is recorded with its issues.
func TestProblemsIsEmptyForWhatItsPhasesRulesAllow(t *testing.T) {
	cases := map[string][2]string{}
	for phase, data := range validHandoffs {
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
			if problems := Problems(tc[0], "THIS-1", stamped(t, tc[0], tc[1])); len(problems) != 0 {
				t.Fatalf("%s handoff %s: problems %q, want none", tc[0], tc[1], problems)
			}
		})
	}
}

// The worker stamps its handoff with the phase and issue it is of, and the daemon checks the file
// at .legion/<issue>/<phase>.json against both: a file copied from another phase, or another tree's,
// is refused by name, and a stamp of the wrong type is refused as the type. schemaVersion and
// completed are the worker's to write, held only to their types when present: a handoff without
// them is a handoff. Every stamp problem is reported beside the shape's, so one push fixes them all.
func TestProblemsHoldsTheFourStamps(t *testing.T) {
	implement := func(stamps string) map[string]any {
		return decode(t, `{`+stamps+`,"proof":[`+handoffProof+`]}`)
	}
	for _, tc := range []struct {
		name   string
		object map[string]any
		fields []string
		reason string
	}{
		{"every stamp as the worker writes it", stamped(t, "implement", validHandoffs["implement"]), nil, ""},
		{"neither schemaVersion nor completed", implement(`"phase":"implement","issue":"THIS-1"`), nil, ""},
		{"no phase", implement(`"issue":"THIS-1"`), []string{"phase"}, `phase: missing — write "implement"`},
		{"another phase's word", implement(`"phase":"test","issue":"THIS-1"`), []string{"phase"}, `phase: Invalid input: expected "implement", received "test"`},
		{"a phase that is not a string", implement(`"phase":1,"issue":"THIS-1"`), []string{"phase"}, `phase: Invalid input: expected string, received number`},
		{"no issue", implement(`"phase":"implement"`), []string{"issue"}, `issue: missing — write "THIS-1"`},
		{"another tree's issue", implement(`"phase":"implement","issue":"OTHER-2"`), []string{"issue"}, `issue: Invalid input: expected "THIS-1", received "OTHER-2"`},
		{"a schemaVersion that is a string", implement(`"schemaVersion":"1","phase":"implement","issue":"THIS-1"`), []string{"schemaVersion"}, `schemaVersion: Invalid input: expected number, received string`},
		{"a completed that is a number", implement(`"phase":"implement","issue":"THIS-1","completed":1760000000`), []string{"completed"}, `completed: Invalid input: expected string, received number`},
		{"every stamp wrong, beside the shape's problem", decode(t, `{"schemaVersion":"1","phase":"plan","issue":"OTHER-2","completed":false,"proof":[]}`),
			[]string{"schemaVersion", "phase", "issue", "completed", "proof"}, `phase: Invalid input: expected "implement", received "plan"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems := Problems("implement", "THIS-1", tc.object)
			if named := fields(problems); !slices.Equal(named, tc.fields) {
				t.Fatalf("problems %q name %v, want %v", problems, named, tc.fields)
			}
			if tc.reason != "" && !slices.Contains(problems, tc.reason) {
				t.Fatalf("problems %q, want one reading %q", problems, tc.reason)
			}
		})
	}
}

// A plan whose stamps are wrong is still held to its shape, and a plan whose shape holds is still
// held to its four records: a stamp problem never hides a record the next role needs.
func TestProblemsChecksAPlansRecordsWhateverItsStampsSay(t *testing.T) {
	object := decode(t, `{"phase":"implement","issue":"THIS-1","taskCount":3}`)
	problems := Problems("plan", "THIS-1", object)
	if named := fields(problems); !slices.Equal(named, []string{"phase", "requiredSkills", "gapAnalysis", "planReview", "specDepartures"}) {
		t.Fatalf("problems %q name %v, want the phase stamp and the plan's four records", problems, named)
	}
}

// The planner's role prompt shows each shape of its two plan checks and of its departures from the
// spec as JSON (internal/prompts/roles/planner.md, "Plan handoff"); a planner that records them as
// shown is not refused.
func TestProblemsAcceptsEveryPlanHandoffShapeThePlannerPromptShows(t *testing.T) {
	prompt, err := os.ReadFile(filepath.Join("..", "prompts", "roles", "planner.md"))
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
			if problems := Problems("plan", "THIS-1", stamped(t, "plan", data)); len(problems) != 0 {
				t.Fatalf("plan handoff %s: problems %q, want none", data, problems)
			}
		}
	}
}

// Dir and Path are the one spelling of where an issue's handoffs live, as the role prompts and the
// retro's removal name them.
func TestDirAndPathSpellTheHandoffLayout(t *testing.T) {
	if got := Dir("LEGION-631"); got != ".legion/LEGION-631" {
		t.Fatalf("Dir = %q", got)
	}
	if got := Path("LEGION-631", "implement"); got != ".legion/LEGION-631/implement.json" {
		t.Fatalf("Path = %q", got)
	}
}
