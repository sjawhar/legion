package intake

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

var issueKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[0-9]+$`)

// decodedMessage is the checked envelope identity and its workflow fact. A nil Fact is a valid
// additive or non-workflow event that the shipped reducer acknowledges without a state change.
type decodedMessage struct {
	Source  string
	EventID string
	Fact    Fact
}

type envoyEnvelope struct {
	EventID        string `json:"event_id"`
	Source         string `json:"source"`
	SourceEventID  string `json:"source_event_id"`
	Topic          string `json:"topic"`
	DedupeKey      string `json:"dedupe_key"`
	IssuedAt       int64  `json:"issued_at"`
	PayloadSummary string `json:"payload_summary"`
	Payload        string `json:"payload"`
	TraceID        string `json:"trace_id"`
}

// decodeMessage decodes the Envoy envelope first, then the subject's source-specific payload.
// project is the daemon's Dispatch project: another project's issue event is a nil Fact.
func decodeMessage(subject, project string, data []byte) (decodedMessage, error) {
	var envelope envoyEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return decodedMessage{}, fmt.Errorf("decode Envoy envelope: %w", err)
	}
	if err := envelope.valid(); err != nil {
		return decodedMessage{}, err
	}

	var fact Fact
	var err error
	switch {
	case strings.HasPrefix(subject, "notifications.dispatch.issue."):
		if envelope.Source != "dispatch" {
			return decodedMessage{}, fmt.Errorf("Dispatch subject has envelope source %q", envelope.Source)
		}
		fact, err = decodeDispatchFact(subject, project, envelope.Payload)
	case strings.HasPrefix(subject, "notifications.github."):
		if envelope.Source != "github" {
			return decodedMessage{}, fmt.Errorf("GitHub subject has envelope source %q", envelope.Source)
		}
		fact, err = decodeGitHubFact(subject, envelope.Payload, envelope.IssuedAt)
	default:
		return decodedMessage{}, fmt.Errorf("unsupported durable subject %q", subject)
	}
	if err != nil {
		return decodedMessage{}, err
	}
	return decodedMessage{Source: envelope.Source, EventID: envelope.EventID, Fact: fact}, nil
}

func (e envoyEnvelope) valid() error {
	switch {
	case strings.TrimSpace(e.EventID) == "":
		return fmt.Errorf("Envoy envelope has no event_id")
	case strings.TrimSpace(e.Source) == "":
		return fmt.Errorf("Envoy envelope has no source")
	case strings.TrimSpace(e.SourceEventID) == "":
		return fmt.Errorf("Envoy envelope has no source_event_id")
	case strings.TrimSpace(e.Topic) == "":
		return fmt.Errorf("Envoy envelope has no topic")
	case strings.TrimSpace(e.DedupeKey) == "":
		return fmt.Errorf("Envoy envelope has no dedupe_key")
	case e.IssuedAt == 0:
		return fmt.Errorf("Envoy envelope has no issued_at")
	case strings.TrimSpace(e.PayloadSummary) == "":
		return fmt.Errorf("Envoy envelope has no payload_summary")
	case strings.TrimSpace(e.TraceID) == "":
		return fmt.Errorf("Envoy envelope has no trace_id")
	case !json.Valid([]byte(e.Payload)):
		return fmt.Errorf("Envoy envelope payload is not JSON")
	}
	return nil
}

type dispatchEvent struct {
	ID       int64           `json:"id"`
	IssueKey string          `json:"issue_key"`
	Seq      int64           `json:"seq"`
	Notify   *bool           `json:"notify"`
	Type     string          `json:"type"`
	Payload  json.RawMessage `json:"payload"`
}

// decodeDispatchFact decodes one Dispatch issue event of project. The stream carries every
// project's events; one whose subject key is not a project issue key is another daemon's, and is
// skipped before its payload is read, so it is never poison here.
func decodeDispatchFact(subject, project, payload string) (Fact, error) {
	subjectKey, ok := dispatchSubjectKey(subject)
	if !ok {
		return nil, fmt.Errorf("Dispatch durable subject has no issue key: %s", subject)
	}
	if keyProject, _, _ := strings.Cut(subjectKey, "-"); keyProject != project || !issueKeyPattern.MatchString(subjectKey) {
		return nil, nil
	}
	var event dispatchEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil, fmt.Errorf("decode Dispatch event: %w", err)
	}
	if event.ID <= 0 || event.Seq <= 0 || event.Notify == nil || !issueKeyPattern.MatchString(event.IssueKey) || event.Type == "" || !isJSONObject(event.Payload) {
		return nil, fmt.Errorf("Dispatch durable message payload is not a valid Dispatch event")
	}
	if subjectKey != event.IssueKey {
		return nil, fmt.Errorf("Dispatch durable subject key %s disagrees with event issue_key %s", subjectKey, event.IssueKey)
	}

	switch event.Type {
	case "issue.created", "issue.updated", "issue.closed":
		var issue struct {
			Key    string  `json:"key"`
			Status string  `json:"status"`
			Title  string  `json:"title"`
			Parent *string `json:"parent"`
			Rank   string  `json:"rank"`
		}
		if err := json.Unmarshal(event.Payload, &issue); err != nil {
			return nil, fmt.Errorf("decode Dispatch issue payload: %w", err)
		}
		if issue.Key != event.IssueKey {
			return nil, fmt.Errorf("Dispatch issue event payload key disagrees with event issue_key")
		}
		parent := ""
		if issue.Parent != nil {
			if !issueKeyPattern.MatchString(*issue.Parent) {
				return nil, fmt.Errorf("Dispatch issue event payload has an invalid parent key")
			}
			parent = *issue.Parent
		}
		return DispatchIssue{Key: event.IssueKey, Seq: event.Seq, Type: event.Type, Status: issue.Status, Title: issue.Title, Parent: parent, Rank: issue.Rank}, nil
	case "artifact.approved", "artifact.changes_requested":
		var artifact struct {
			ArtifactID string `json:"artifact_id"`
			Version    int    `json:"version"`
			Reason     string `json:"reason"`
		}
		if err := json.Unmarshal(event.Payload, &artifact); err != nil {
			return nil, fmt.Errorf("decode Dispatch artifact payload: %w", err)
		}
		if artifact.ArtifactID == "" {
			return nil, fmt.Errorf("Dispatch %s payload has no artifact_id", event.Type)
		}
		if artifact.Version <= 0 {
			return nil, fmt.Errorf("Dispatch %s payload has no positive integer version", event.Type)
		}
		kind := DispatchArtifactApproved
		if event.Type == "artifact.changes_requested" {
			if artifact.Reason == "" {
				return nil, fmt.Errorf("Dispatch artifact.changes_requested payload has no reason")
			}
			kind = DispatchArtifactChangesRequested
		}
		return DispatchArtifact{Key: event.IssueKey, ArtifactID: artifact.ArtifactID, Kind: kind, Version: artifact.Version, Reason: artifact.Reason}, nil
	case "artifact.version":
		var artifact struct {
			ArtifactID string `json:"artifact_id"`
			Version    struct {
				Number int `json:"number"`
			} `json:"version"`
		}
		if err := json.Unmarshal(event.Payload, &artifact); err != nil {
			return nil, fmt.Errorf("decode Dispatch artifact version payload: %w", err)
		}
		if artifact.ArtifactID == "" {
			return nil, fmt.Errorf("Dispatch artifact.version payload has no artifact_id")
		}
		if artifact.Version.Number <= 0 {
			return nil, fmt.Errorf("Dispatch artifact.version payload has no positive integer version.number")
		}
		return DispatchArtifact{Key: event.IssueKey, ArtifactID: artifact.ArtifactID, Kind: DispatchArtifactVersion, Version: artifact.Version.Number}, nil
	default:
		return nil, nil
	}
}

func dispatchSubjectKey(subject string) (string, bool) {
	const prefix = "notifications.dispatch.issue."
	remaining := strings.TrimPrefix(subject, prefix)
	key, _, _ := strings.Cut(remaining, ".")
	return key, key != ""
}

func isJSONObject(raw json.RawMessage) bool {
	var item map[string]json.RawMessage
	return json.Unmarshal(raw, &item) == nil && item != nil
}

func decodeGitHubFact(subject, payload string, issuedAt int64) (Fact, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &raw); err != nil || raw == nil {
		return nil, nil
	}
	kind, ok := rawString(raw, "kind")
	if !ok {
		return nil, nil
	}
	switch kind {
	case "pr":
		return decodePullRequest(raw)
	case "review":
		return decodeReview(raw)
	case "push":
		return decodePush(raw)
	case "checks":
		return decodeChecks(subject, raw, issuedAt)
	case "comment":
		// Comments route to the current role but do not change the durable workflow record.
		return nil, nil
	default:
		return nil, nil
	}
}

func decodePullRequest(raw map[string]json.RawMessage) (Fact, error) {
	repo, number, action, ok := githubIdentity(raw)
	if !ok {
		return nil, nil
	}
	branch, _ := rawString(raw, "head_ref")
	sha, _ := rawString(raw, "head_sha")
	body, _ := rawString(raw, "body")
	updatedAt := rawTimestamp(raw, "updated_at")
	switch action {
	case "opened":
		if sha == "" {
			return nil, nil
		}
		url, _ := rawString(raw, "url")
		return PullRequestOpened{Repo: repo, Number: number, Branch: branch, HeadSHA: sha, Body: body, URL: url, UpdatedAt: updatedAt}, nil
	case "synchronize":
		if sha == "" {
			return nil, nil
		}
		return PullRequestSynchronized{Repo: repo, Number: number, Branch: branch, HeadSHA: sha, Body: body, UpdatedAt: updatedAt}, nil
	case "closed":
		merged, _ := rawString(raw, "merged")
		if merged == "true" {
			mergeSHA, _ := rawString(raw, "merge_commit_sha")
			return PullRequestMerged{Repo: repo, Number: number, MergeSHA: mergeSHA}, nil
		}
		return PullRequestClosed{Repo: repo, Number: number}, nil
	default:
		return nil, nil
	}
}

func decodeReview(raw map[string]json.RawMessage) (Fact, error) {
	repo, number, action, ok := githubIdentity(raw)
	if !ok || action != "submitted" {
		return nil, nil
	}
	state, _ := rawString(raw, "state")
	commitID, _ := rawString(raw, "commit_id")
	headSHA, _ := rawString(raw, "head_sha")
	author, _ := rawString(raw, "author")
	body, _ := rawString(raw, "body")
	return PullRequestReview{Repo: repo, Number: number, State: strings.ToLower(state), CommitID: commitID, HeadSHA: headSHA, Author: author, Body: body}, nil
}

func decodePush(raw map[string]json.RawMessage) (Fact, error) {
	repo, ok := rawString(raw, "repo")
	if !ok {
		return nil, nil
	}
	ref, ok := rawString(raw, "ref")
	if !ok || !strings.HasPrefix(ref, "refs/heads/") {
		return nil, nil
	}
	after, ok := rawString(raw, "after")
	if !ok || after == "" {
		return nil, nil
	}
	changedPaths := rawStringPointer(raw, "changed_paths")
	truncated := rawStringPointer(raw, "changed_paths_truncated")
	return Push{Repo: repo, Branch: strings.TrimPrefix(ref, "refs/heads/"), After: after, ChangedPaths: changedPaths, Truncated: truncated}, nil
}

func decodeChecks(subject string, raw map[string]json.RawMessage, issuedAt int64) (Fact, error) {
	repo, repoOK := rawString(raw, "repo")
	number, numberOK := rawNumber(raw, "number")
	if !repoOK || !numberOK {
		return nil, nil
	}
	if !checksSubjectMatches(subject, repo, number) {
		return nil, nil
	}
	headSHA, ok := rawString(raw, "sha")
	if !ok || headSHA == "" {
		return nil, nil
	}
	runs, ok := rawCheckRuns(raw["check_runs"])
	if !ok {
		return nil, nil
	}
	generation, ok := rawInt64(raw, "generation")
	if !ok || generation < 0 {
		return nil, nil
	}
	snapshot, ok := rawString(raw, "snapshot")
	if !ok || snapshot == "" {
		return nil, nil
	}
	failed, ok := rawStatusGroup(raw, "failed")
	if !ok {
		return nil, nil
	}
	cancelled, ok := rawStatusGroup(raw, "cancelled")
	if !ok {
		return nil, nil
	}
	settledAt := time.UnixMilli(issuedAt).UTC()
	if value, present := raw["settled_at"]; present {
		milliseconds, ok := rawInt64Value(value)
		if !ok || milliseconds < 0 {
			return nil, nil
		}
		settledAt = time.UnixMilli(milliseconds).UTC()
	}
	verdict := ""
	if len(failed) > 0 {
		verdict = "red"
	} else if len(cancelled) == 0 {
		verdict = "green"
	}
	return PullRequestChecks{Repo: repo, Number: number, HeadSHA: headSHA, CheckRuns: runs, Generation: generation, Snapshot: snapshot, Verdict: verdict, Failing: failed, SettledAt: settledAt}, nil
}

func githubIdentity(raw map[string]json.RawMessage) (string, int, string, bool) {
	repo, repoOK := rawString(raw, "repo")
	number, numberOK := rawNumber(raw, "number")
	action, actionOK := rawString(raw, "action")
	return repo, number, action, repoOK && numberOK && actionOK
}

func checksSubjectMatches(subject, repo string, number int) bool {
	segments := strings.Split(subject, ".")
	if len(segments) != 7 || segments[0] != "notifications" || segments[1] != "github" || segments[4] != "pr" || segments[6] != "checks" {
		return false
	}
	return segments[2]+"/"+segments[3] == repo && segments[5] == strconv.Itoa(number)
}

func rawCheckRuns(value json.RawMessage) ([]record.AttemptRun, bool) {
	var incoming []struct {
		Name string `json:"name"`
		ID   int64  `json:"id"`
	}
	if err := json.Unmarshal(value, &incoming); err != nil || len(incoming) == 0 {
		return nil, false
	}
	seen := make(map[string]struct{}, len(incoming))
	runs := make([]record.AttemptRun, 0, len(incoming))
	for _, item := range incoming {
		if item.Name == "" || item.ID <= 0 {
			return nil, false
		}
		if _, duplicate := seen[item.Name]; duplicate {
			return nil, false
		}
		seen[item.Name] = struct{}{}
		runs = append(runs, record.AttemptRun{Name: item.Name, ID: item.ID})
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Name < runs[j].Name })
	return runs, true
}

func rawStatusGroup(raw map[string]json.RawMessage, key string) ([]string, bool) {
	var group struct {
		Count  int      `json:"count"`
		Checks []string `json:"checks"`
	}
	if err := json.Unmarshal(raw[key], &group); err != nil || group.Count < 0 || len(group.Checks) != group.Count {
		return nil, false
	}
	for _, check := range group.Checks {
		if check == "" {
			return nil, false
		}
	}
	return group.Checks, true
}

func rawString(raw map[string]json.RawMessage, key string) (string, bool) {
	value, present := raw[key]
	if !present {
		return "", false
	}
	var decoded string
	if err := json.Unmarshal(value, &decoded); err != nil {
		return "", false
	}
	return decoded, true
}

func rawStringPointer(raw map[string]json.RawMessage, key string) *string {
	value, ok := rawString(raw, key)
	if !ok {
		return nil
	}
	return new(value)
}

func rawNumber(raw map[string]json.RawMessage, key string) (int, bool) {
	value, present := raw[key]
	if !present {
		return 0, false
	}
	var integer int64
	if rawInt, ok := rawInt64Value(value); ok {
		integer = rawInt
	} else {
		var text string
		if err := json.Unmarshal(value, &text); err != nil || !regexp.MustCompile(`^\d+$`).MatchString(text) {
			return 0, false
		}
		parsed, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return 0, false
		}
		integer = parsed
	}
	if integer < 0 || integer > int64(math.MaxInt) {
		return 0, false
	}
	return int(integer), true
}

func rawInt64(raw map[string]json.RawMessage, key string) (int64, bool) {
	value, present := raw[key]
	if !present {
		return 0, false
	}
	return rawInt64Value(value)
}

func rawInt64Value(value json.RawMessage) (int64, bool) {
	var integer int64
	if err := json.Unmarshal(value, &integer); err != nil {
		return 0, false
	}
	return integer, true
}

func rawTimestamp(raw map[string]json.RawMessage, key string) time.Time {
	value, ok := rawString(raw, key)
	if !ok || value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}
