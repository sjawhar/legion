package classify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

type fixture struct {
	Function string          `json:"fn"`
	Input    json.RawMessage `json:"input"`
	Output   json.RawMessage `json:"output"`
}

func TestFixturesReplayByteExactly(t *testing.T) {
	files := fixtureFiles(t)
	if len(files) == 0 {
		t.Fatal("classification fixture directory is empty")
	}

	for _, path := range files {
		path := path
		t.Run(strings.TrimSuffix(filepath.Base(path), ".json"), func(t *testing.T) {
			encoded, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			var fixture fixture
			if err := json.Unmarshal(encoded, &fixture); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}

			got, err := replayFixture(fixture.Function, fixture.Input)
			if err != nil {
				t.Fatalf("replay %s: %v", fixture.Function, err)
			}
			if !bytes.Equal(got, fixture.Output) {
				t.Fatalf("%s output differs\n got: %s\nwant: %s", fixture.Function, got, fixture.Output)
			}
		})
	}
}

func TestFixtureCorpusRejectsPlausibleClassifierMutations(t *testing.T) {
	mutations := []struct {
		name     string
		function string
	}{
		{"inverted check-run id comparison", "compareAttemptSets"},
		{"dropped duplicate-settlement branch", "classifySettlement"},
		{"dropped stored CI failures", "effectiveOutcome"},
		{"unfenced empty GitHub rollup", "acceptGitHubFence"},
		{"inverted stale-head comparison", "supersededBy"},
		{"dropped handoff-only push branch", "classifyPush"},
	}

	for _, mutation := range mutations {
		t.Run(mutation.function+"/"+mutation.name, func(t *testing.T) {
			for _, path := range fixtureFiles(t) {
				fixture := readFixture(t, path)
				if fixture.Function != mutation.function {
					continue
				}
				got, err := replayMutant(mutation.function, fixture.Input)
				if err != nil {
					t.Fatalf("mutate %s: %v", path, err)
				}
				if !bytes.Equal(got, fixture.Output) {
					t.Logf("%s rejects the mutation", filepath.Base(path))
					return
				}
			}
			t.Fatalf("no %s fixture rejects %s", mutation.function, mutation.name)
		})
	}
}

func TestProductionImportsArePure(t *testing.T) {
	packageDirectory := sourceDirectory(t)
	files, err := filepath.Glob(filepath.Join(packageDirectory, "*.go"))
	if err != nil {
		t.Fatalf("list package files: %v", err)
	}

	imports := map[string]bool{}
	fileSet := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse imports in %s: %v", path, err)
		}
		for _, importSpec := range file.Imports {
			imports[strings.Trim(importSpec.Path.Value, `\"`)] = true
		}
	}

	allowed := map[string]bool{
		"time": true,
		"github.com/sjawhar/legion/daemon/internal/record": true,
	}
	for imported := range imports {
		if !allowed[imported] {
			t.Errorf("production package imports %q; classifiers must be pure", imported)
		}
	}
	if !imports["time"] {
		t.Error("production package must use the contract's time.Time HeadClock")
	}
	if !imports["github.com/sjawhar/legion/daemon/internal/record"] {
		t.Error("production package must classify record values")
	}
}

func fixtureFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(fixtureDirectory(t))
	if err != nil {
		t.Fatalf("read classification fixture directory: %v", err)
	}

	var files []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		matches, err := filepath.Glob(filepath.Join(fixtureDirectory(t), entry.Name(), "*.json"))
		if err != nil {
			t.Fatalf("list %s fixtures: %v", entry.Name(), err)
		}
		if len(matches) == 0 {
			t.Fatalf("fixture class %s has no records", entry.Name())
		}
		files = append(files, matches...)
	}
	sort.Strings(files)
	return files
}

func readFixture(t *testing.T, path string) fixture {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture fixture
	if err := json.Unmarshal(encoded, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return fixture
}

func fixtureDirectory(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(sourceDirectory(t)))), "contracts", "fixtures", "classification")
}

func sourceDirectory(t *testing.T) string {
	t.Helper()
	_, path, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate classifier test source")
	}
	return filepath.Dir(path)
}

func replayFixture(function string, input json.RawMessage) ([]byte, error) {
	switch function {
	case "compareAttemptSets":
		var decoded struct {
			Stored   []record.AttemptRun `json:"stored"`
			Incoming []record.AttemptRun `json:"incoming"`
		}
		if err := decodeFixture(input, &decoded); err != nil {
			return nil, err
		}
		return canonicalJSON(CompareAttemptSets(decoded.Stored, decoded.Incoming))
	case "classifySettlement":
		var decoded struct {
			PR       fixturePullRequest  `json:"pr"`
			Incoming SettlementCandidate `json:"incoming"`
		}
		if err := decodeFixture(input, &decoded); err != nil {
			return nil, err
		}
		return canonicalJSON(ClassifySettlement(decoded.PR.record(), decoded.Incoming))
	case "effectiveOutcome":
		var decoded struct {
			PR       fixturePullRequest  `json:"pr"`
			Incoming SettlementCandidate `json:"incoming"`
		}
		if err := decodeFixture(input, &decoded); err != nil {
			return nil, err
		}
		outcome := EffectiveOutcome(decoded.PR.record(), decoded.Incoming)
		return canonicalOutcome(outcome)
	case "acceptGitHubFence":
		var decoded struct {
			PR        fixturePullRequest `json:"pr"`
			CheckRuns []record.AttemptRun `json:"checkRuns"`
		}
		if err := decodeFixture(input, &decoded); err != nil {
			return nil, err
		}
		return canonicalJSON(AcceptGitHubFence(decoded.PR.record(), decoded.CheckRuns))
	case "supersededBy":
		var decoded struct {
			Incoming fixtureHeadClock `json:"incoming"`
			Applied  fixtureHeadClock `json:"applied"`
		}
		if err := decodeFixture(input, &decoded); err != nil {
			return nil, err
		}
		return canonicalJSON(SupersededBy(decoded.Incoming.clock(), decoded.Applied.clock()))
	case "classifyPush":
		var decoded PushPayload
		if err := decodeFixture(input, &decoded); err != nil {
			return nil, err
		}
		return canonicalJSON(ClassifyPush(decoded))
	case "reduceDispatchEvent":
		var decoded fixtureDispatchInput
		if err := decodeFixture(input, &decoded); err != nil {
			return nil, err
		}
		return canonicalDecision(classifyDispatchDecision(decoded.State.state(nil), decoded.event()))
	case "reduceGithubEvent":
		var decoded fixtureGithubInput
		if err := decodeFixture(input, &decoded); err != nil {
			return nil, err
		}
		payload, err := decodePayload(decoded.Payload)
		if err != nil {
			return nil, err
		}
		projects := map[string]string{}
		for key, project := range decoded.Config.Projects {
			projects[key] = project.Repo
		}
		return canonicalDecision(classifyGithubDecision(
			decoded.State.state(projects),
			githubDecisionEvent{
				Topic:     decoded.Topic,
				Payload:   payload,
				UpdatedAt: timestamp(payload["updated_at"]),
			},
		))
	default:
		return nil, fmt.Errorf("unknown fixture function %q", function)
	}
}

func decodeFixture(input json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func canonicalJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		return nil, err
	}
	return json.Marshal(generic)
}

func canonicalDecision(decision reducerDecision) ([]byte, error) {
	output := map[string]any{"effectKinds": decision.EffectKinds}
	if decision.Status != nil {
		output["status"] = *decision.Status
	}
	if decision.FixAttemptsSet {
		if decision.FixAttempts == nil {
			output["fixAttempts"] = nil
		} else {
			output["fixAttempts"] = *decision.FixAttempts
		}
	}
	if decision.BlockedAttemptsSet {
		if decision.BlockedAttempts == nil {
			output["blockedAttempts"] = nil
		} else {
			output["blockedAttempts"] = *decision.BlockedAttempts
		}
	}
	if decision.Gate != nil {
		gate := map[string]any{
			"artifactId":    decision.Gate.ArtifactID,
			"latestVersion": decision.Gate.LatestVersion,
		}
		if decision.Gate.ApprovedVersion != nil {
			gate["approvedVersion"] = *decision.Gate.ApprovedVersion
		}
		output["gate"] = gate
	}
	return canonicalJSON(output)
}

func canonicalOutcome(outcome CiOutcome) ([]byte, error) {
	verdict := any(outcome.Verdict)
	if outcome.Verdict == "" {
		verdict = nil
	}
	return canonicalJSON(map[string]any{
		"verdict":         verdict,
		"failing":         outcome.Failing,
		"failingStatuses": outcome.FailingStatuses,
	})
}

func replayMutant(function string, input json.RawMessage) ([]byte, error) {
	switch function {
	case "compareAttemptSets":
		var decoded struct {
			Stored   []record.AttemptRun `json:"stored"`
			Incoming []record.AttemptRun `json:"incoming"`
		}
		if err := decodeFixture(input, &decoded); err != nil {
			return nil, err
		}
		return canonicalJSON(mutantCompareAttemptSets(decoded.Stored, decoded.Incoming))
	case "classifySettlement":
		var decoded struct {
			PR       fixturePullRequest  `json:"pr"`
			Incoming SettlementCandidate `json:"incoming"`
		}
		if err := decodeFixture(input, &decoded); err != nil {
			return nil, err
		}
		classification := ClassifySettlement(decoded.PR.record(), decoded.Incoming)
		if classification == SettlementDuplicate {
			classification = SettlementNewer
		}
		return canonicalJSON(classification)
	case "effectiveOutcome":
		var decoded struct {
			PR       fixturePullRequest  `json:"pr"`
			Incoming SettlementCandidate `json:"incoming"`
		}
		if err := decodeFixture(input, &decoded); err != nil {
			return nil, err
		}
		pr := decoded.PR.record()
		failing := append([]string{}, decoded.Incoming.Failing...)
		statuses := make([]string, len(pr.FailingStatuses))
		copy(statuses, pr.FailingStatuses)
		verdict := decoded.Incoming.Verdict
		if len(failing) != 0 || len(statuses) != 0 {
			verdict = "red"
		}
		return canonicalOutcome(CiOutcome{Verdict: verdict, Failing: failing, FailingStatuses: statuses})
	case "acceptGitHubFence":
		var decoded struct {
			PR        fixturePullRequest `json:"pr"`
			CheckRuns []record.AttemptRun `json:"checkRuns"`
		}
		if err := decodeFixture(input, &decoded); err != nil {
			return nil, err
		}
		classification := AcceptGitHubFence(decoded.PR.record(), decoded.CheckRuns)
		if len(decoded.CheckRuns) == 0 {
			classification = GitHubFenceUnfenced
		}
		return canonicalJSON(classification)
	case "supersededBy":
		var decoded struct {
			Incoming fixtureHeadClock `json:"incoming"`
			Applied  fixtureHeadClock `json:"applied"`
		}
		if err := decodeFixture(input, &decoded); err != nil {
			return nil, err
		}
		incoming, applied := decoded.Incoming.clock(), decoded.Applied.clock()
		mutant := SupersededBy(incoming, applied)
		if !incoming.UpdatedAt.IsZero() && !applied.UpdatedAt.IsZero() && incoming.UpdatedAt.Before(applied.UpdatedAt) {
			mutant = false
		}
		return canonicalJSON(mutant)
	case "classifyPush":
		var decoded PushPayload
		if err := decodeFixture(input, &decoded); err != nil {
			return nil, err
		}
		classification := ClassifyPush(decoded)
		if classification.HandoffOnly {
			classification = PushClassification{}
		}
		return canonicalJSON(classification)
	default:
		return nil, fmt.Errorf("unknown classifier mutation %q", function)
	}
}

func mutantCompareAttemptSets(stored, incoming []record.AttemptRun) AttemptSetOrder {
	known := make(map[string]int64, len(stored))
	for _, run := range stored {
		known[run.Name] = run.ID
	}
	var higher, lower bool
	for _, run := range incoming {
		storedID, found := known[run.Name]
		if !found || run.ID < storedID {
			higher = true
		} else if run.ID > storedID {
			lower = true
		}
	}
	switch {
	case higher && lower:
		return AttemptSetMixed
	case higher:
		return AttemptSetNewer
	case lower:
		return AttemptSetOlder
	default:
		return AttemptSetEqual
	}
}

type fixturePullRequest struct {
	Key                    string                `json:"key"`
	Repo                   string                `json:"repo"`
	Number                 int                   `json:"number"`
	Branch                 string                `json:"branch"`
	HeadSHA                string                `json:"headSha"`
	HeadUpdatedAt          json.RawMessage       `json:"headUpdatedAt"`
	HeadUpdatedAtSource    string                `json:"headUpdatedAtSource"`
	Verdict                string                `json:"verdict"`
	Failing                []string              `json:"failing"`
	FailingStatuses        []string              `json:"failingStatuses"`
	ReviewDecision         string                `json:"reviewDecision"`
	FixAttempts            int                   `json:"fixAttempts"`
	BlockedAttempts        *int                  `json:"blockedAttempts"`
	CheckRuns              *[]record.AttemptRun  `json:"ciCheckRuns"`
	Generation             *int64                `json:"ciSettlementGeneration"`
	Snapshot               *string               `json:"ciSnapshot"`
	Reconciled             bool                  `json:"ciReconciled"`
	PendingPush            *record.PendingPush   `json:"pendingPush"`
	HeadCounted            *bool                 `json:"headCounted"`
	CISettledAt            json.RawMessage       `json:"ciSettledAt"`
}

func (fixture fixturePullRequest) record() record.PullRequest {
	checkRuns := []record.AttemptRun(nil)
	if fixture.CheckRuns != nil {
		checkRuns = append([]record.AttemptRun{}, (*fixture.CheckRuns)...)
	}
	generation := int64(0)
	if fixture.Generation != nil {
		generation = *fixture.Generation
	}
	snapshot := ""
	if fixture.Snapshot != nil {
		snapshot = *fixture.Snapshot
	}
	return record.PullRequest{
		Issue:               fixture.Key,
		Repo:                fixture.Repo,
		Number:              fixture.Number,
		Branch:              fixture.Branch,
		HeadSHA:             fixture.HeadSHA,
		HeadUpdatedAt:       timestampJSON(fixture.HeadUpdatedAt),
		HeadUpdatedAtSource: fixture.HeadUpdatedAtSource,
		Verdict:             fixture.Verdict,
		Failing:             append([]string{}, fixture.Failing...),
		FailingStatuses:     append([]string{}, fixture.FailingStatuses...),
		ReviewDecision:      fixture.ReviewDecision,
		FixAttempts:         fixture.FixAttempts,
		CheckRuns:           checkRuns,
		Generation:          generation,
		Snapshot:            snapshot,
		Reconciled:          fixture.Reconciled,
		PendingPush:         fixture.PendingPush,
	}
}

type fixtureHeadClock struct {
	UpdatedAt json.RawMessage `json:"updatedAt"`
	Source    string          `json:"source"`
}

func (fixture fixtureHeadClock) clock() HeadClock {
	return HeadClock{UpdatedAt: timestampJSON(fixture.UpdatedAt), Source: fixture.Source}
}

type fixtureIssue struct {
	Status         string   `json:"status"`
	Parent         string   `json:"parent"`
	Children       []string `json:"children"`
	LastAppliedSeq *int64   `json:"lastAppliedSeq"`
}

type fixtureGate struct {
	ArtifactID      string `json:"artifactId"`
	LatestVersion   int    `json:"latestVersion"`
	ApprovedVersion *int   `json:"approvedVersion"`
}

type fixtureTree struct {
	Root   string `json:"root"`
	Status string `json:"status"`
}

type fixtureState struct {
	Project      string                        `json:"project"`
	Issues       map[string]fixtureIssue       `json:"issues"`
	Gates        map[string]fixtureGate        `json:"gates"`
	Trees        map[string]fixtureTree        `json:"trees"`
	Phases       map[string]any                `json:"phases"`
	Roles        map[string]bool               `json:"roles"`
	Admission    struct{ Queue []string `json:"queue"` } `json:"admission"`
	PRs          map[string]fixturePullRequest `json:"prs"`
	PRByBranch   map[string]string             `json:"prByBranch"`
	PRTombstones map[string]json.RawMessage    `json:"prTombstones"`
}

func (fixture fixtureState) state(projectRepos map[string]string) decisionState {
	state := decisionState{
		Issues:       map[string]*decisionIssue{},
		Gates:        map[string]*decisionGate{},
		Trees:        map[string]decisionTree{},
		Queue:        map[string]bool{},
		PRs:          map[string]*decisionPR{},
		PRByBranch:   map[string]string{},
		PRTombstones: map[string]time.Time{},
		ProjectRepos: projectRepos,
	}
	for key, issue := range fixture.Issues {
		state.Issues[key] = &decisionIssue{
			Status:     issue.Status,
			Parent:     issue.Parent,
			Children:   append([]string{}, issue.Children...),
			HasLastSeq: issue.LastAppliedSeq != nil,
		}
		if issue.LastAppliedSeq != nil {
			state.Issues[key].LastSeq = *issue.LastAppliedSeq
		}
	}
	for key, gate := range fixture.Gates {
		state.Gates[key] = &decisionGate{
			ArtifactID:      gate.ArtifactID,
			LatestVersion:   gate.LatestVersion,
			ApprovedVersion: gate.ApprovedVersion,
		}
	}
	for key, tree := range fixture.Trees {
		state.Trees[key] = decisionTree{Root: tree.Root, Status: tree.Status}
	}
	for _, issue := range fixture.Admission.Queue {
		state.Queue[issue] = true
	}
	for key, pullRequest := range fixture.PRs {
		state.PRs[key] = &decisionPR{
			PullRequest:         pullRequest.record(),
			HasBlockedAttempts: pullRequest.BlockedAttempts != nil,
			HeadCounted:        pullRequest.HeadCounted != nil && *pullRequest.HeadCounted,
		}
		if pullRequest.BlockedAttempts != nil {
			state.PRs[key].BlockedAttempts = *pullRequest.BlockedAttempts
		}
	}
	for key, mapped := range fixture.PRByBranch {
		state.PRByBranch[key] = mapped
	}
	for key, tombstone := range fixture.PRTombstones {
		state.PRTombstones[key] = timestampJSON(tombstone)
	}
	return state
}

type fixtureDispatchInput struct {
	State fixtureState `json:"state"`
	Event struct {
		Key     string         `json:"key"`
		Type    string         `json:"type"`
		Seq     int64          `json:"seq"`
		Payload map[string]any `json:"payload"`
	} `json:"event"`
}

func (fixture fixtureDispatchInput) event() dispatchDecisionEvent {
	return dispatchDecisionEvent{
		Key:     fixture.Event.Key,
		Type:    fixture.Event.Type,
		Seq:     fixture.Event.Seq,
		Payload: fixture.Event.Payload,
	}
}

type fixtureGithubInput struct {
	State   fixtureState `json:"state"`
	Topic   string       `json:"topic"`
	Payload json.RawMessage `json:"payload"`
	Config struct {
		Projects map[string]struct {
			Repo string `json:"repo"`
		} `json:"projects"`
	} `json:"config"`
}

func decodePayload(raw json.RawMessage) (map[string]any, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	if encoded, ok := value.(string); ok {
		if err := json.Unmarshal([]byte(encoded), &value); err != nil {
			return nil, err
		}
	}
	payload, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("payload is %T, want object", value)
	}
	return payload, nil
}

func timestampJSON(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return time.Time{}
	}
	return timestamp(value)
}

func timestamp(value any) time.Time {
	switch value := value.(type) {
	case float64:
		return time.UnixMilli(int64(value))
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err == nil {
			return parsed
		}
	}
	return time.Time{}
}
