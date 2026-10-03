package main

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// planReviewMaxRounds is the most rounds of plan review a planner runs before it proceeds with the
// plan: a review still rejecting before the last is revised and reviewed again, never recorded.
const planReviewMaxRounds = 3

// requiredSkillsProblem is every refusal of a plan's skill lists, wherever in them it falls.
const requiredSkillsProblem = "missing or empty — name the skills this role must load, or state `none: <what you looked through and why nothing fits>`"

// handoffWriteProblems is every reason `legion handoff write` refuses fields as phase's handoff,
// each `<field path>: <reason>` so the worker's next write can fix it, and none for a handoff it
// writes. Each field the phase declares is held to its shape (handoffPhases), and every other field
// passes untouched, since the next worker may need it. A plan whose shape holds is then held to
// three rules of its own: every downstream role's skill list is present and non-empty (a project
// with no skill that fits says so as the single entry `none: <what was looked through and why
// nothing fits>`), and the gap analysis before the draft and the plan review after it are both
// recorded, a failed call as its error. That record is the planner's own report: the write checks
// its shape, not that the checks ran.
func handoffWriteProblems(phase string, fields map[string]any) []string {
	c := &checker{}
	handoffPhases[phase](c, "", fields, true)
	if phase == "plan" && len(c.problems) == 0 {
		requiredSkillsWritten(c, fields["requiredSkills"])
		analysis, present := fields["gapAnalysis"]
		gapAnalysisWritten(c, "gapAnalysis", analysis, present)
		review, present := fields["planReview"]
		planReviewWritten(c, "planReview", review, present)
	}
	return c.problems
}

// checker collects problems. aborts counts the ones no rule across fields may run past: a value of
// the wrong type, or outside its options, which such a rule would read as if it were there.
type checker struct {
	problems []string
	aborts   int
}

func (c *checker) add(path, reason string) { c.problems = append(c.problems, path+": "+reason) }

func (c *checker) abort(path, reason string) {
	c.aborts++
	c.add(path, reason)
}

func (c *checker) wrongType(path, expected string, value any, present bool) {
	c.abort(path, "Invalid input: expected "+expected+", received "+received(value, present))
}

// received names what a value is; present is false for a key its object leaves out.
func received(value any, present bool) string {
	switch value.(type) {
	case nil:
		if present {
			return "null"
		}
		return "undefined"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	default:
		return "object"
	}
}

// rule checks the value at path, a key its object holds (present) or leaves out.
type rule func(c *checker, path string, value any, present bool)

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func optional(r rule) rule {
	return func(c *checker, path string, value any, present bool) {
		if present {
			r(c, path, value, present)
		}
	}
}

func text(c *checker, path string, value any, present bool) {
	if _, ok := value.(string); !ok {
		c.wrongType(path, "string", value, present)
	}
}

// nonEmpty is a string with something besides whitespace: a blank field is not a record.
func nonEmpty(c *checker, path string, value any, present bool) {
	s, ok := value.(string)
	if !ok {
		c.wrongType(path, "string", value, present)
		return
	}
	if strings.TrimSpace(s) == "" {
		c.add(path, "Too small: expected string to have >=1 characters")
	}
}

func number(c *checker, path string, value any, present bool) {
	if _, ok := value.(float64); !ok {
		c.wrongType(path, "number", value, present)
	}
}

func boolean(c *checker, path string, value any, present bool) {
	if _, ok := value.(bool); !ok {
		c.wrongType(path, "boolean", value, present)
	}
}

func oneOf(options ...string) rule {
	quoted := make([]string, len(options))
	for i, option := range options {
		quoted[i] = strconv.Quote(option)
	}
	reason := "Invalid option: expected one of " + strings.Join(quoted, "|")
	return func(c *checker, path string, value any, _ bool) {
		if s, ok := value.(string); !ok || !slices.Contains(options, s) {
			c.abort(path, reason)
		}
	}
}

// array is a list of at least minimum elements, each held to element.
func array(element rule, minimum int) rule {
	return func(c *checker, path string, value any, present bool) {
		list, ok := value.([]any)
		if !ok {
			c.wrongType(path, "array", value, present)
			return
		}
		for i, item := range list {
			element(c, join(path, strconv.Itoa(i)), item, true)
		}
		if len(list) < minimum {
			c.add(path, fmt.Sprintf("Too small: expected array to have >=%d items", minimum))
		}
	}
}

type field struct {
	key  string
	rule rule
}

// object holds each declared field to its rule, in order, and passes every other field.
func object(fields ...field) rule {
	return func(c *checker, path string, value any, present bool) {
		fieldsOf, ok := value.(map[string]any)
		if !ok {
			c.wrongType(path, "object", value, present)
			return
		}
		for _, f := range fields {
			v, held := fieldsOf[f.key]
			f.rule(c, join(path, f.key), v, held)
		}
	}
}

// refined is r, then across, a rule across the object's fields, once r found every field there.
func refined(r rule, across func(c *checker, path string, fields map[string]any)) rule {
	return func(c *checker, path string, value any, present bool) {
		before := c.aborts
		r(c, path, value, present)
		if fields, ok := value.(map[string]any); ok && c.aborts == before {
			across(c, path, fields)
		}
	}
}

// recorded is a write-time record the plan must carry: its absence says what to record.
func recorded(what string, r rule) rule {
	return func(c *checker, path string, value any, present bool) {
		if !present {
			c.abort(path, "missing — record "+what)
			return
		}
		r(c, path, value, present)
	}
}

var (
	words     = optional(array(text, 0))
	sizes     = []string{"trivial", "small", "medium", "large"}
	baseShape = []field{{"learningsInjected", words}, {"learningsHelpful", words}}
	routing   = optional(object(
		field{"skipArchitect", optional(boolean)},
		field{"complexity", optional(oneOf(sizes...))},
		field{"estimatedImplementers", optional(number)},
	))
	// proof is one production-like proof: the changed behaviour exercised on the surface a user
	// reaches it through, never a unit suite.
	proof = object(
		field{"criterion", nonEmpty}, field{"surface", nonEmpty}, field{"command", nonEmpty},
		field{"observed", nonEmpty}, field{"headSha", nonEmpty}, field{"negativeControl", nonEmpty},
	)
)

func phaseShape(fields ...field) rule { return object(append(slices.Clone(baseShape), fields...)...) }

// handoffPhases are the phase words a handoff is written under, .legion/<phase>.json, each with its
// handoff's shape: the words the role prompts pass to the legion tool's handoff_write, whose enum is
// packages/contracts/src/handoff-schema.ts HANDOFF_PHASES. A pane's role is its claim role,
// LEGION_ROLE, never one of these.
var handoffPhases = map[string]rule{
	"architect": phaseShape(
		field{"scope", optional(oneOf(sizes...))},
		field{"components", words},
		field{"subIssues", words},
		field{"routingHints", routing},
		field{"concerns", words},
	),
	"plan": phaseShape(
		field{"taskCount", optional(number)},
		field{"independentTasks", optional(number)},
		field{"routingHints", routing},
		field{"concerns", words},
		field{"workflowRecommendation", optional(text)},
		field{"requiredSkills", optional(object(
			field{"implement", words}, field{"test", words}, field{"review", words},
		))},
		field{"gapAnalysis", optional(object(
			field{"findings", optional(array(object(field{"finding", text}, field{"answer", text}), 0))},
			field{"error", optional(text)},
		))},
		field{"planReview", optional(object(
			field{"verdict", oneOf("approved", "rejected", "failed")},
			field{"rounds", number},
			field{"remainingIssues", optional(array(object(field{"issue", text}, field{"evidence", text}), 0))},
			field{"error", optional(text)},
		))},
	),
	"implement": phaseShape(
		field{"filesChanged", words},
		field{"proof", array(proof, 1)},
		field{"trickyParts", words},
		field{"deviations", words},
		field{"openQuestions", words},
		field{"subPlanningNeeded", optional(boolean)},
		field{"discoveredComplexity", words},
		field{"suggestedSubWorkers", optional(number)},
	),
	"test": refined(phaseShape(
		field{"passed", optional(number)},
		field{"failed", optional(number)},
		field{"failures", optional(array(object(field{"criterion", text}, field{"evidence", text}), 0))},
		field{"implementerProof", object(field{"verdict", oneOf("verified", "rejected")}, field{"how", nonEmpty})},
		field{"proof", optional(array(proof, 1))},
		field{"documentationFeedback", optional(text)},
		field{"observations", words},
	), testRecords),
	"review": phaseShape(
		field{"critical", optional(number)},
		field{"important", optional(number)},
		field{"minor", optional(number)},
		field{"verdict", optional(oneOf("approved", "changes_requested"))},
		field{"keyFindings", optional(array(object(field{"severity", text}, field{"file", text}, field{"description", text}), 0))},
	),
}

// testRecords: a test handoff that reports no failure carries the tester's own proof, and a
// reported failure — failed > 0, or the implementer's proof rejected — is a recorded one.
func testRecords(c *checker, path string, fields map[string]any) {
	failures, _ := fields["failures"].([]any)
	proofs, _ := fields["proof"].([]any)
	failed, _ := fields["failed"].(float64)
	if len(failures) == 0 && failed <= 0 && len(proofs) == 0 {
		c.add(join(path, "proof"), "a passing test handoff needs the tester's own production-like proof")
	}
	if failed != 0 && len(failures) == 0 {
		c.add(join(path, "failures"), "a test handoff that reports failed > 0 records at least one failure")
	}
	implementerProof, _ := fields["implementerProof"].(map[string]any)
	if implementerProof["verdict"] == "rejected" && len(failures) == 0 {
		c.add(join(path, "failures"), "a rejected implementer proof is a recorded failure")
	}
}

// requiredSkillsWritten holds a plan to a non-empty skill list for each downstream role.
func requiredSkillsWritten(c *checker, value any) {
	skills, ok := value.(map[string]any)
	if !ok {
		c.add("requiredSkills", requiredSkillsProblem)
		return
	}
	for _, role := range []string{"implement", "test", "review"} {
		path := "requiredSkills." + role
		list, _ := skills[role].([]any)
		if len(list) == 0 {
			c.add(path, requiredSkillsProblem)
			continue
		}
		for i, entry := range list {
			if name, _ := entry.(string); strings.TrimSpace(name) == "" {
				c.add(join(path, strconv.Itoa(i)), requiredSkillsProblem)
			}
		}
	}
}

// gapAnalysisWritten: every finding of the gap analysis before the plan was drafted, with the
// plan's answer to it, or the failed call's error, never both.
var gapAnalysisWritten = recorded(
	"the gap analyst's `findings`, each with how the plan answers it (`[]` when it found none), or its failed call's `error`",
	refined(object(
		field{"findings", optional(array(object(field{"finding", nonEmpty}, field{"answer", nonEmpty}), 0))},
		field{"error", optional(nonEmpty)},
	), func(c *checker, path string, fields map[string]any) {
		_, findings := fields["findings"]
		_, failed := fields["error"]
		if findings == failed {
			c.add(path, "record exactly one of `findings` or the failed call's `error`")
		}
	}),
)

// planReviewWritten: the plan review after the draft. A rejection is recorded only after the last
// round the planner runs, with the issues that round named; an approval leaves none standing; a
// failure names its call's error, and nothing else does.
var planReviewWritten = recorded(
	"the plan review's `verdict` and `rounds`, with `remainingIssues` when it was rejected or `error` when a review's call failed",
	refined(object(
		field{"verdict", oneOf("approved", "rejected", "failed")},
		field{"rounds", reviewRounds},
		field{"remainingIssues", optional(array(object(field{"issue", nonEmpty}, field{"evidence", nonEmpty}), 0))},
		field{"error", optional(nonEmpty)},
	), func(c *checker, path string, fields map[string]any) {
		verdict, _ := fields["verdict"].(string)
		rounds, _ := fields["rounds"].(float64)
		remaining, _ := fields["remainingIssues"].([]any)
		if verdict == "rejected" && len(remaining) == 0 {
			c.add(join(path, "remainingIssues"), "a rejected review records the blocking issues its last round named")
		}
		if verdict == "rejected" && rounds < planReviewMaxRounds {
			c.add(join(path, "rounds"), fmt.Sprintf("a review still rejecting after %s of %d rounds is revised and reviewed again, not recorded",
				strconv.FormatFloat(rounds, 'f', -1, 64), planReviewMaxRounds))
		}
		if verdict == "approved" && len(remaining) > 0 {
			c.add(join(path, "remainingIssues"), "an approved review leaves no blocking issue standing")
		}
		if _, failedCall := fields["error"]; (verdict == "failed") != failedCall {
			c.add(join(path, "error"), "a failed review records its call's error, and only a failed review does")
		}
	}),
)

// reviewRounds is the count of reviews run, a failed one included: a whole number from one to the
// most a planner runs.
func reviewRounds(c *checker, path string, value any, present bool) {
	rounds, ok := value.(float64)
	if !ok {
		c.wrongType(path, "number", value, present)
		return
	}
	if rounds != math.Trunc(rounds) {
		c.abort(path, "Invalid input: expected int, received number")
		return
	}
	if rounds < 1 {
		c.add(path, "Too small: expected number to be >=1")
	}
	if rounds > planReviewMaxRounds {
		c.add(path, fmt.Sprintf("Too big: expected number to be <=%d", planReviewMaxRounds))
	}
}
