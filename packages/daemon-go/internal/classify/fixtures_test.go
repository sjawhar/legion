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
	"strconv"
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
			want, err := keptFixtureOutput(fixture.Function, fixture.Output)
			if err != nil {
				t.Fatalf("project expected output: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s output differs\n got: %s\nwant: %s", fixture.Function, got, want)
			}
		})
	}
}

// TestProductionImportsArePure protects the pure decision package from gaining transport, clock,
// database, or workflow dependencies.
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
	if !imports["time"] || !imports["github.com/sjawhar/legion/daemon/internal/record"] {
		t.Error("production package must retain its record and time contracts")
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
		return canonicalOutcome(EffectiveOutcome(decoded.PR.record(), decoded.Incoming))
	case "acceptGitHubFence":
		var decoded struct {
			PR        fixturePullRequest  `json:"pr"`
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
		return replayDispatchDecision(input)
	case "reduceGithubEvent":
		return replayGitHubDecision(input)
	default:
		return nil, fmt.Errorf("unknown fixture function %q", function)
	}
}

func replayDispatchDecision(input json.RawMessage) ([]byte, error) {
	var decoded fixtureDispatchInput
	if err := json.Unmarshal(input, &decoded); err != nil {
		return nil, err
	}
	before, found := decoded.State.Gates[decoded.Event.Key]
	if !found || decoded.State.Issues[decoded.Event.Key] == nil || before.ArtifactID != stringValue(decoded.Event.Payload, "artifact_id") {
		return canonicalJSON(map[string]any{})
	}
	kind := DesignGateEventKind(strings.TrimPrefix(decoded.Event.Type, "artifact."))
	version := fixtureVersion(decoded.Event.Payload)
	after := ApplyDesignGateEvent(before.record(decoded.Event.Key), kind, version)
	if sameGate(before.record(decoded.Event.Key), after) {
		return canonicalJSON(map[string]any{})
	}
	return canonicalJSON(map[string]any{"gate": fixtureGateValue(after)})
}

func replayGitHubDecision(input json.RawMessage) ([]byte, error) {
	var decoded fixtureGithubInput
	if err := json.Unmarshal(input, &decoded); err != nil {
		return nil, err
	}
	payload, err := decodePayload(decoded.Payload)
	if err != nil {
		return nil, err
	}
	pr, found := decoded.State.pullRequest(payload)
	if !found {
		if decoded.canRegisterPullRequest(payload) {
			return canonicalJSON(map[string]any{"fixAttempts": 0})
		}
		return canonicalJSON(map[string]any{})
	}
	before := pr
	deleted := false
	switch stringValue(payload, "kind") {
	case "pr":
		switch stringValue(payload, "action") {
		case "synchronize":
			if stringValue(payload, "head_sha") != "" && stringValue(payload, "head_sha") != pr.HeadSHA {
				pr = AdvancePullRequestHead(pr, stringValue(payload, "head_sha"))
			}
		case "closed":
			deleted = true
		}
	case "push":
		pr = ApplyPush(pr, stringValue(payload, "after"), ClassifyPush(PushPayload{
			ChangedPaths: optionalString(payload, "changed_paths"), ChangedPathsTruncated: optionalString(payload, "changed_paths_truncated"),
		}))
	case "review":
		pr = ApplyReview(pr, lowerASCII(stringValue(payload, "state")), stringValue(payload, "commit_id"))
	case "checks":
		candidate := SettlementCandidate{CheckRuns: attemptRuns(payload), Generation: int64(numberValue(payload, "generation")), Snapshot: stringValue(payload, "snapshot"), Verdict: stringValue(payload, "verdict"), Failing: stringSlice(payload, "failing")}
		pr, _ = ApplySettlement(pr, candidate)
		if pr.Verdict == "red" {
			pr, _ = BlockFixAttempt(pr, 3)
		}
	}
	result := map[string]any{}
	if deleted {
		result["fixAttempts"] = nil
		if before.BlockedAttempts != 0 {
			result["blockedAttempts"] = nil
		}
		return canonicalJSON(result)
	}
	if pr.FixAttempts != before.FixAttempts {
		result["fixAttempts"] = pr.FixAttempts
	}
	if pr.BlockedAttempts != before.BlockedAttempts {
		if pr.BlockedAttempts == 0 {
			result["blockedAttempts"] = nil
		} else {
			result["blockedAttempts"] = pr.BlockedAttempts
		}
	}
	return canonicalJSON(result)
}

func keptFixtureOutput(function string, raw json.RawMessage) ([]byte, error) {
	if function != "reduceDispatchEvent" && function != "reduceGithubEvent" {
		return canonicalJSONRaw(raw)
	}
	var output map[string]json.RawMessage
	if err := decodeFixture(raw, &output); err != nil {
		return nil, err
	}
	kept := map[string]json.RawMessage{}
	for _, key := range []string{"fixAttempts", "blockedAttempts", "gate"} {
		if value, found := output[key]; found {
			kept[key] = value
		}
	}
	return canonicalJSON(kept)
}

func canonicalJSONRaw(raw json.RawMessage) ([]byte, error) {
	var value any
	if err := decodeFixture(raw, &value); err != nil {
		return nil, err
	}
	return canonicalJSON(value)
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

func canonicalOutcome(outcome CiOutcome) ([]byte, error) {
	verdict := any(outcome.Verdict)
	if outcome.Verdict == "" {
		verdict = nil
	}
	return canonicalJSON(map[string]any{"verdict": verdict, "failing": outcome.Failing, "failingStatuses": outcome.FailingStatuses})
}

type fixturePullRequest struct {
	Key                 string               `json:"key"`
	Repo                string               `json:"repo"`
	Number              int                  `json:"number"`
	Branch              string               `json:"branch"`
	HeadSHA             string               `json:"headSha"`
	HeadUpdatedAt       json.RawMessage      `json:"headUpdatedAt"`
	HeadUpdatedAtSource string               `json:"headUpdatedAtSource"`
	Verdict             string               `json:"verdict"`
	Failing             []string             `json:"failing"`
	FailingStatuses     []string             `json:"failingStatuses"`
	ReviewDecision      string               `json:"reviewDecision"`
	FixAttempts         int                  `json:"fixAttempts"`
	BlockedAttempts     *int                 `json:"blockedAttempts"`
	CheckRuns           *[]record.AttemptRun `json:"ciCheckRuns"`
	Generation          *int64               `json:"ciSettlementGeneration"`
	Snapshot            *string              `json:"ciSnapshot"`
	Reconciled          bool                 `json:"ciReconciled"`
	PendingPush         *record.PendingPush  `json:"pendingPush"`
	HeadCounted         *bool                `json:"headCounted"`
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
	blocked := 0
	if fixture.BlockedAttempts != nil {
		blocked = *fixture.BlockedAttempts
	}
	headCounted := ""
	if fixture.HeadCounted != nil && *fixture.HeadCounted {
		headCounted = fixture.HeadSHA
	}
	return record.PullRequest{Issue: fixture.Key, Repo: fixture.Repo, Number: fixture.Number, Branch: fixture.Branch, HeadSHA: fixture.HeadSHA,
		HeadUpdatedAt: timestampJSON(fixture.HeadUpdatedAt), HeadUpdatedAtSource: fixture.HeadUpdatedAtSource, Verdict: fixture.Verdict,
		Failing: append([]string{}, fixture.Failing...), FailingStatuses: append([]string{}, fixture.FailingStatuses...), ReviewDecision: fixture.ReviewDecision,
		FixAttempts: fixture.FixAttempts, BlockedAttempts: blocked, CheckRuns: checkRuns, Generation: generation, Snapshot: snapshot,
		Reconciled: fixture.Reconciled, PendingPush: fixture.PendingPush, HeadCounted: headCounted}
}

type fixtureHeadClock struct {
	UpdatedAt json.RawMessage `json:"updatedAt"`
	Source    string          `json:"source"`
}

func (fixture fixtureHeadClock) clock() HeadClock {
	return HeadClock{UpdatedAt: timestampJSON(fixture.UpdatedAt), Source: fixture.Source}
}

type fixtureGate struct {
	ArtifactID      string `json:"artifactId"`
	LatestVersion   int    `json:"latestVersion"`
	ApprovedVersion *int   `json:"approvedVersion"`
}

func (fixture fixtureGate) record(issue string) record.DesignGate {
	return record.DesignGate{
		Issue: issue, ArtifactID: fixture.ArtifactID, LatestVersion: fixture.LatestVersion, ApprovedVersion: fixture.ApprovedVersion,
	}
}

type fixtureState struct {
	Issues       map[string]json.RawMessage    `json:"issues"`
	Gates        map[string]fixtureGate        `json:"gates"`
	PRs          map[string]fixturePullRequest `json:"prs"`
	PRByBranch   map[string]string             `json:"prByBranch"`
	PRTombstones map[string]json.RawMessage    `json:"prTombstones"`
}

func (state fixtureState) pullRequest(payload map[string]any) (record.PullRequest, bool) {
	repo := stringValue(payload, "repo")
	number := numberValue(payload, "number")
	if repo != "" && number != 0 {
		pr, ok := state.PRs[repo+"#"+strconv.Itoa(number)]
		return pr.record(), ok
	}
	branch := strings.TrimPrefix(stringValue(payload, "ref"), "refs/heads/")
	if key := state.PRByBranch[repo+"@"+branch]; key != "" {
		pr, ok := state.PRs[key]
		return pr.record(), ok
	}
	return record.PullRequest{}, false
}

type fixtureDispatchInput struct {
	State fixtureState `json:"state"`
	Event struct {
		Key     string         `json:"key"`
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	} `json:"event"`
}

type fixtureGithubInput struct {
	State   fixtureState    `json:"state"`
	Payload json.RawMessage `json:"payload"`
	Config  struct {
		Projects map[string]struct {
			Repo string `json:"repo"`
		} `json:"projects"`
	} `json:"config"`
}

func (input fixtureGithubInput) canRegisterPullRequest(payload map[string]any) bool {
	action := stringValue(payload, "action")
	if stringValue(payload, "kind") != "pr" || (action != "opened" && action != "synchronize") {
		return false
	}
	branch := strings.TrimPrefix(stringValue(payload, "head_ref"), "legion/")
	if branch == stringValue(payload, "head_ref") || input.State.Issues[branch] == nil || stringValue(payload, "head_sha") == "" {
		return false
	}
	project, _, found := strings.Cut(branch, "-")
	if !found || input.Config.Projects[project].Repo != stringValue(payload, "repo") {
		return false
	}
	if tombstone, found := input.State.PRTombstones[stringValue(payload, "repo")+"#"+strconv.Itoa(numberValue(payload, "number"))]; found {
		var millis float64
		if json.Unmarshal(tombstone, &millis) == nil && timestampJSONValue(stringValue(payload, "updated_at")).UnixMilli() <= int64(millis) {
			return false
		}
	}
	return true
}

func timestampJSONValue(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
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
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return time.Time{}
	}
	switch value := value.(type) {
	case float64:
		return time.UnixMilli(int64(value))
	case string:
		parsed, _ := time.Parse(time.RFC3339Nano, value)
		return parsed
	}
	return time.Time{}
}
func stringValue(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}
func optionalString(values map[string]any, key string) *string {
	value, ok := values[key].(string)
	if !ok {
		return nil
	}
	return &value
}
func numberValue(values map[string]any, key string) int {
	switch value := values[key].(type) {
	case float64:
		return int(value)
	case string:
		parsed, _ := strconv.Atoi(value)
		return parsed
	}
	return 0
}
func fixtureVersion(values map[string]any) int {
	if value := numberValue(values, "version"); value != 0 {
		return value
	}
	if version, ok := values["version"].(map[string]any); ok {
		return numberValue(version, "number")
	}
	return 0
}
func lowerASCII(value string) string                        { return strings.ToLower(value) }
func attemptRuns(values map[string]any) []record.AttemptRun { return nil }
func stringSlice(values map[string]any, key string) []string {
	raw, _ := values[key].([]any)
	result := make([]string, 0, len(raw))
	for _, value := range raw {
		if text, ok := value.(string); ok {
			result = append(result, text)
		}
	}
	return result
}
func fixtureGateValue(gate record.DesignGate) map[string]any {
	value := map[string]any{"artifactId": gate.ArtifactID, "latestVersion": gate.LatestVersion}
	if gate.ApprovedVersion != nil {
		value["approvedVersion"] = *gate.ApprovedVersion
	}
	return value
}
func sameGate(left, right record.DesignGate) bool {
	if left.ArtifactID != right.ArtifactID || left.LatestVersion != right.LatestVersion {
		return false
	}
	if left.ApprovedVersion == nil || right.ApprovedVersion == nil {
		return left.ApprovedVersion == nil && right.ApprovedVersion == nil
	}
	return *left.ApprovedVersion == *right.ApprovedVersion
}
