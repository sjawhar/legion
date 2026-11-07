package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The record a forced create leaves and the nudge an unforced one gets.
//
// A score is the share of the new text's picked terms a candidate holds, and pickRelatedTerms
// keeps only a term some other item of the project holds. So a fixture controls the score by
// splitting a vocabulary between two open issues: the new text holds every word of both, the
// first issue holds `shared` of them and the second the rest, every word has document frequency
// 1 and so the same idf, and the first issue's score is `shared` over the whole. Each case below
// derives its split from the two bars, so no case is pinned to today's numbers.

// duplicateWords is a vocabulary of distinct nonsense words, one lexeme each: no digit (the term
// picker drops a number as noise), no English stem shared with another of them, and nothing a
// stop-word list holds.
var duplicateWords = []string{
	"zymurgy", "astrolabe", "gnomon", "weighbridge", "quorvex", "tanbrim", "velduct", "harnolo",
	"pelmaris", "corvanth", "dulrique", "mirabant", "sellowy", "framtoss", "glovence", "pardwell",
	"thrinbeck", "ulvarone", "wexlimb", "yarrowstan", "zepharine", "brantwick", "clovermast",
	"driftkell",
}

// duplicateVocabularyTest is one fixture: a project with two open issues holding disjoint halves
// of a vocabulary, and the text of a create that holds all of it.
type duplicateVocabularyTest struct {
	project string
	// candidate is the issue the new text scores against.
	candidate model.Issue
	// title and spec are the new create's text.
	title string
	spec  string
	// score is the share of the vocabulary the candidate holds.
	score float64
}

// newDuplicateVocabulary builds that fixture for a target score: the candidate holds the nearest
// whole share of the vocabulary, and the rest is spread over as many open issues as it takes for
// each of them to hold less than the candidate does. Every word then has a document frequency of
// 1, so the candidate's score is its share and it is the closest candidate.
func newDuplicateVocabulary(t *testing.T, handler http.Handler, project string, target float64) duplicateVocabularyTest {
	t.Helper()
	createDuplicateTestProject(t, handler, project)
	total := len(duplicateWords)
	shared := int(math.Round(target * float64(total)))
	if shared < 1 || shared > total {
		t.Fatalf("target share %v needs %d of %d words", target, shared, total)
	}
	candidate := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": project, "title": "Candidate " + duplicateWords[0],
		"spec": strings.Join(duplicateWords[:shared], " ") + ".", "force": true,
	}))
	chunk := max(1, shared-1)
	for start := shared; start < total; start += chunk {
		rest := duplicateWords[start:min(start+chunk, total)]
		requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
			"project": project, "title": "Other " + rest[0],
			"spec": strings.Join(rest, " ") + ".", "force": true,
		}))
	}
	return duplicateVocabularyTest{
		project:   project,
		candidate: candidate,
		title:     "New " + duplicateWords[0],
		spec:      strings.Join(duplicateWords, " ") + ".",
		score:     float64(shared) / float64(total),
	}
}

// createdEventPayload is the issue.created event of key, as a reader of the event log sees it.
func createdEventPayload(t *testing.T, handler http.Handler, key string) model.IssueEventPayload {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/events?after=0&limit=50", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read %s's events: status=%d body=%s", key, response.Code, response.Body.String())
	}
	var events []struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &events); err != nil {
		t.Fatalf("decode %s's events: %v", key, err)
	}
	for _, event := range events {
		if event.Type != "issue.created" {
			continue
		}
		var payload model.IssueEventPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode %s's issue.created payload: %v", key, err)
		}
		return payload
	}
	t.Fatalf("%s has no issue.created event", key)
	return model.IssueEventPayload{}
}

// rawCreatedPayload is the same event's payload as JSON, for a case that must tell an absent
// field from an empty one.
func rawCreatedPayload(t *testing.T, handler http.Handler, key string) map[string]any {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/events?after=0&limit=50", nil, "alice")
	var events []struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &events); err != nil {
		t.Fatalf("decode %s's events: %v", key, err)
	}
	for _, event := range events {
		if event.Type == "issue.created" {
			return event.Payload
		}
	}
	t.Fatalf("%s has no issue.created event", key)
	return nil
}

func TestCreateIssueForcedPastARefusalRecordsWhatItNamed(t *testing.T) {
	handler := newTestHandler(t)
	fixture := newDuplicateVocabulary(t, handler, "FORCED", 1)

	forced := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": fixture.project, "title": fixture.title, "spec": fixture.spec, "force": true,
	}))
	payload := createdEventPayload(t, handler, forced.Key)
	if payload.Forced == nil {
		t.Fatalf("forced create %s carries no forced record", forced.Key)
	}
	if len(payload.Forced.Past) == 0 || payload.Forced.Past[0] != fixture.candidate.Key {
		t.Fatalf("forced.past = %v, want %s first", payload.Forced.Past, fixture.candidate.Key)
	}
}

func TestCreateIssueForcedPastNothingRecordsNothing(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "FORCED")
	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "FORCED", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))

	// A forced create the refusal would not have stopped, and an unforced create, both carry no
	// forced key at all, which is how a reader tells them from a create whose check did not
	// answer.
	forced := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "FORCED", "title": "Orchard pickers cannot log a crate",
		"spec": duplicateSpecHarvest, "force": true,
	}))
	plain := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "FORCED", "title": "Weighbridge power returns",
		"spec": strings.Join(duplicateWords[:6], " ") + ".",
	}))
	for _, key := range []string{forced.Key, plain.Key} {
		if payload, ok := rawCreatedPayload(t, handler, key)["forced"]; ok {
			t.Fatalf("%s carries forced = %#v, want no forced key", key, payload)
		}
	}
}

func TestCreateIssueForcedWithNoAnswerRecordsAnEmptyList(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	createDuplicateTestProject(t, handler, "FORCED")
	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "FORCED", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))

	// The gate reads the project's term statistics first, so a lock on that table holds its read
	// until the bound ends; the create goes in all the same, recording that the check did not
	// answer.
	served.duplicateBound = 200 * time.Millisecond
	lockTx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the lock transaction: %v", err)
	}
	t.Cleanup(func() { _ = lockTx.Rollback(context.Background()) })
	if _, err := lockTx.Exec(context.Background(), "lock table search_term_stats in access exclusive mode"); err != nil {
		t.Fatalf("lock search_term_stats: %v", err)
	}

	forced := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "FORCED", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe, "force": true,
	}))
	payload := createdEventPayload(t, handler, forced.Key)
	if payload.Forced == nil || len(payload.Forced.Past) != 0 {
		t.Fatalf("forced record of a create whose check did not answer = %#v, want an empty past", payload.Forced)
	}
	raw := rawCreatedPayload(t, handler, forced.Key)
	forcedRaw, ok := raw["forced"].(map[string]any)
	if !ok {
		t.Fatalf("forced on the wire = %#v", raw["forced"])
	}
	if past, ok := forcedRaw["past"].([]any); !ok || len(past) != 0 {
		t.Fatalf("forced.past on the wire = %#v, want []", forcedRaw["past"])
	}
}

// A create naming a GitHub issue runs no near-copy gate, so a forced one records nothing. A
// `forced: {past: []}` here would say a check did not answer that was never made, and the weekly
// count would report it as an override; `near_copy_check` has always been absent on this path,
// and the two records must agree.
func TestCreateIssueForcedExternalRecordsNoCheckThatNeverRan(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")
	mapping := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": "TEST",
	}, "alice")
	if mapping.Code != http.StatusOK {
		t.Fatalf("map external repository: status=%d body=%s", mapping.Code, mapping.Body.String())
	}
	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))

	// The same text as that open issue, which an unforced create of the project is refused for:
	// the external create goes in unchecked, and `force` changes nothing it records.
	response := createIssueRequest(t, handler, map[string]any{
		"external": "owner/repo#41", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe, "force": true,
	})
	created := requireCreatedIssue(t, response)
	if forced, ok := rawCreatedPayload(t, handler, created.Key)["forced"]; ok {
		t.Fatalf("forced external create %s carries forced = %#v, want no forced key: no gate ran",
			created.Key, forced)
	}
	suggestions := decodeBody[struct {
		Advice struct {
			Suggestions model.Suggestions `json:"suggestions"`
		} `json:"advice"`
	}](t, response).Advice.Suggestions
	if suggestions.NearCopyCheck != "" || len(suggestions.TitleMatches) != 0 || suggestions.LikelyDuplicate != nil {
		t.Fatalf("a create that ran no gate told its filer about one: %q %#v %#v",
			suggestions.NearCopyCheck, suggestions.TitleMatches, suggestions.LikelyDuplicate)
	}
}

// An unforced create the gate could not check is written, not refused, and the filer is told
// that no near-copy check was made. Refusing it would turn one slow read into a project nobody
// can write to.
func TestCreateIssueNudgeIsAbsentWhenTheGateDoesNotAnswer(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	createDuplicateTestProject(t, handler, "NUDGE")
	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "NUDGE", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))

	served.duplicateBound = 200 * time.Millisecond
	lockTx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the lock transaction: %v", err)
	}
	t.Cleanup(func() { _ = lockTx.Rollback(context.Background()) })
	if _, err := lockTx.Exec(context.Background(), "lock table search_term_stats in access exclusive mode"); err != nil {
		t.Fatalf("lock search_term_stats: %v", err)
	}

	response := createIssueRequest(t, handler, map[string]any{
		"project": "NUDGE", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	})
	created := requireCreatedIssue(t, response)
	suggestions := decodeBody[struct {
		Advice struct {
			Suggestions model.Suggestions `json:"suggestions"`
		} `json:"advice"`
	}](t, response).Advice.Suggestions
	if suggestions.NearCopyCheck == "" {
		t.Fatalf("create %s says nothing about the check that did not answer: %s", created.Key, response.Body.String())
	}
	if suggestions.LikelyDuplicate != nil || len(suggestions.TitleMatches) != 0 {
		t.Fatalf("a create the gate could not check named issues: %#v %#v",
			suggestions.LikelyDuplicate, suggestions.TitleMatches)
	}
	if _, ok := rawCreatedPayload(t, handler, created.Key)["forced"]; ok {
		t.Fatalf("an unforced create carries a forced record")
	}
}

func TestCreateIssueNudgesTheClosestOpenIssueBelowTheRefusal(t *testing.T) {
	handler := newTestHandler(t)
	// Midway between the two bars, so the case holds for any pair with
	// 0 < duplicateNudgeScore < duplicateRefuseScore < 1.
	fixture := newDuplicateVocabulary(t, handler, "NUDGE", (duplicateNudgeScore+duplicateRefuseScore)/2)

	response := createIssueRequest(t, handler, map[string]any{
		"project": fixture.project, "title": fixture.title, "spec": fixture.spec,
	})
	created := requireCreatedIssue(t, response)
	body := decodeBody[struct {
		Advice struct {
			Suggestions model.Suggestions `json:"suggestions"`
		} `json:"advice"`
	}](t, response)
	likely := body.Advice.Suggestions.LikelyDuplicate
	if likely == nil {
		t.Fatalf("create %s at score %.2f named no likely duplicate: %s",
			created.Key, fixture.score, response.Body.String())
	}
	if likely.Key != fixture.candidate.Key {
		t.Fatalf("likely duplicate = %s, want %s", likely.Key, fixture.candidate.Key)
	}
	if len(body.Advice.Suggestions.TitleMatches) != 0 {
		t.Fatalf("an unforced create filled title_matches: %#v", body.Advice.Suggestions.TitleMatches)
	}
}

func TestCreateIssueNudgesNobodyBelowTheNudgeBar(t *testing.T) {
	handler := newTestHandler(t)
	// Half the nudge bar: the candidate holds too little of the new text to be named.
	fixture := newDuplicateVocabulary(t, handler, "NUDGE", duplicateNudgeScore/2)

	response := createIssueRequest(t, handler, map[string]any{
		"project": fixture.project, "title": fixture.title, "spec": fixture.spec,
	})
	requireCreatedIssue(t, response)
	body := decodeBody[struct {
		Advice struct {
			Suggestions model.Suggestions `json:"suggestions"`
		} `json:"advice"`
	}](t, response)
	if body.Advice.Suggestions.LikelyDuplicate != nil {
		t.Fatalf("a create at score %.2f named a likely duplicate: %#v",
			fixture.score, body.Advice.Suggestions.LikelyDuplicate)
	}
}

func TestCreateIssueNudgesNobodyOnACreateItRefuses(t *testing.T) {
	handler := newTestHandler(t)
	fixture := newDuplicateVocabulary(t, handler, "NUDGE", 1)

	refused := createIssueRequest(t, handler, map[string]any{
		"project": fixture.project, "title": fixture.title, "spec": fixture.spec,
	})
	body := requireDuplicateRefusal(t, refused)
	if len(body.Candidates) == 0 || body.Candidates[0].Key != fixture.candidate.Key {
		t.Fatalf("refusal candidates = %#v, want %s first", body.Candidates, fixture.candidate.Key)
	}
	// A forced create at the same text is named what it went past, never nudged about it.
	forced := createIssueRequest(t, handler, map[string]any{
		"project": fixture.project, "title": fixture.title, "spec": fixture.spec, "force": true,
	})
	requireCreatedIssue(t, forced)
	suggestions := decodeBody[struct {
		Advice struct {
			Suggestions model.Suggestions `json:"suggestions"`
		} `json:"advice"`
	}](t, forced).Advice.Suggestions
	if suggestions.LikelyDuplicate != nil {
		t.Fatalf("a forced create was nudged: %#v", suggestions.LikelyDuplicate)
	}
	if len(suggestions.TitleMatches) == 0 || suggestions.TitleMatches[0].Key != fixture.candidate.Key {
		t.Fatalf("title_matches = %#v, want %s first", suggestions.TitleMatches, fixture.candidate.Key)
	}
}

// The gate's bars are a fraction of one and ordered, and three terms is the least it judges on.
func TestDuplicateGateBarsAreOrderedFractions(t *testing.T) {
	if !(0 < duplicateNudgeScore && duplicateNudgeScore < duplicateRefuseScore && duplicateRefuseScore <= 1) {
		t.Fatalf("bars out of order: nudge %v, refuse %v", duplicateNudgeScore, duplicateRefuseScore)
	}
	if duplicateMinTerms < 1 {
		t.Fatalf("duplicateMinTerms = %d", duplicateMinTerms)
	}
	if len(duplicateWords) < duplicateMinTerms {
		t.Fatalf("the fixture vocabulary holds %d words, under duplicateMinTerms %d",
			len(duplicateWords), duplicateMinTerms)
	}
}
