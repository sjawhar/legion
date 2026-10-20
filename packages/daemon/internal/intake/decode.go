package intake

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// decodedMessage is the checked envelope identity and its workflow fact. A nil Fact is a valid
// additive or non-workflow event that the shipped reducer acknowledges without a state change.
type decodedMessage struct {
	Source  string
	EventID string
	Fact    Fact
	// Unread names each optional field the decoder could not read and took as absent. The consumer
	// logs it; the message is not poison.
	Unread []string
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
// repositories are its configured repositories: a GitHub event whose payload names another
// repository is a nil Fact, whatever subject carried it.
func decodeMessage(subject, project string, repositories []ghrepo.Repository, data []byte) (decodedMessage, error) {
	var envelope envoyEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return decodedMessage{}, fmt.Errorf("decode Envoy envelope: %w", err)
	}
	if err := envelope.valid(); err != nil {
		return decodedMessage{}, err
	}

	var fact Fact
	var unread []string
	var err error
	switch {
	case strings.HasPrefix(subject, "notifications.dispatch.issue."):
		if envelope.Source != "dispatch" {
			return decodedMessage{}, fmt.Errorf("Dispatch subject has envelope source %q", envelope.Source)
		}
		fact, err = decodeDispatchFact(subject, project, envelope.Payload)
	case strings.HasPrefix(subject, dispatchProjectPrefix):
		if envelope.Source != "dispatch" {
			return decodedMessage{}, fmt.Errorf("Dispatch subject has envelope source %q", envelope.Source)
		}
		fact, err = decodeDispatchProjectFact(subject, project, envelope.Payload)
	case strings.HasPrefix(subject, "notifications.github."):
		if envelope.Source != "github" {
			return decodedMessage{}, fmt.Errorf("GitHub subject has envelope source %q", envelope.Source)
		}
		fact, unread, err = decodeGitHubFact(subject, repositories, envelope.Payload)
	default:
		return decodedMessage{}, fmt.Errorf("unsupported durable subject %q", subject)
	}
	if err != nil {
		return decodedMessage{}, err
	}
	return decodedMessage{Source: envelope.Source, EventID: envelope.EventID, Fact: fact, Unread: unread}, nil
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
	ID       int64  `json:"id"`
	IssueKey string `json:"issue_key"`
	Seq      int64  `json:"seq"`
	Actor    struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"actor"`
	Notify  *bool           `json:"notify"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// decodeDispatchFact decodes one Dispatch issue event of project. The stream carries every
// project's events; one whose subject key is not a project issue key is another daemon's, and is
// skipped before its payload is read, so it is never poison here.
func decodeDispatchFact(subject, project, payload string) (Fact, error) {
	subjectKey, ok := dispatchSubjectKey(subject)
	if !ok {
		return nil, fmt.Errorf("Dispatch durable subject has no issue key: %s", subject)
	}
	if keyProject, _, _ := strings.Cut(subjectKey, "-"); keyProject != project || !claim.IsIssueKey(subjectKey) {
		return nil, nil
	}
	var event dispatchEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil, fmt.Errorf("decode Dispatch event: %w", err)
	}
	if event.ID <= 0 || event.Seq <= 0 || event.Notify == nil || !claim.IsIssueKey(event.IssueKey) || event.Type == "" || !isJSONObject(event.Payload) {
		return nil, fmt.Errorf("Dispatch durable message payload is not a valid Dispatch event")
	}
	if subjectKey != event.IssueKey {
		return nil, fmt.Errorf("Dispatch durable subject key %s disagrees with event issue_key %s", subjectKey, event.IssueKey)
	}

	switch event.Type {
	case "issue.created", "issue.updated", "issue.closed":
		var issue struct {
			Key          string          `json:"key"`
			Status       string          `json:"status"`
			Title        string          `json:"title"`
			Parent       *string         `json:"parent"`
			Rank         string          `json:"rank"`
			Labels       []string        `json:"labels"`
			ResolvedRepo json.RawMessage `json:"resolved_repo"`
		}
		if err := json.Unmarshal(event.Payload, &issue); err != nil {
			return nil, fmt.Errorf("decode Dispatch issue payload: %w", err)
		}
		if issue.Key != event.IssueKey {
			return nil, fmt.Errorf("Dispatch issue event payload key disagrees with event issue_key")
		}
		// A Dispatch that resolves an issue's repository sends resolved_repo on every issue event,
		// null for none. An event JetStream retained from an older Dispatch has no such field: it
		// still carries its actor and sequence, which the workflow's fences read, so it is decoded
		// with its repository missing, for the consumer's PrepareRepository to fill.
		missing := issue.ResolvedRepo == nil
		var resolved *string
		if !missing {
			if err := json.Unmarshal(issue.ResolvedRepo, &resolved); err != nil {
				return nil, fmt.Errorf("decode Dispatch issue payload resolved_repo: %w", err)
			}
		}
		parent := ""
		if issue.Parent != nil {
			if !claim.IsIssueKey(*issue.Parent) {
				return nil, fmt.Errorf("Dispatch issue event payload has an invalid parent key")
			}
			parent = *issue.Parent
		}
		actorSession := ""
		daemonActor := event.Actor.Kind == "session" && event.Actor.ID == dispatch.DaemonSession(event.IssueKey)
		if event.Actor.Kind == "session" && !daemonActor {
			actorSession = event.Actor.ID
		}
		return DispatchIssue{Key: event.IssueKey, Seq: event.Seq, Type: event.Type, Status: issue.Status, Title: issue.Title, Parent: parent, Rank: issue.Rank,
			HandedOver: dispatch.CarriesLegionLabel(issue.Labels), ActorSession: actorSession, DaemonActor: daemonActor, ResolvedRepo: deref(resolved),
			ResolvedRepoMissing: missing}, nil
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

// dispatchProjectPrefix begins every Dispatch event of a project rather than an issue
// (packages/envoy/internal/dispatch/outbox/publisher.go envelope): notifications.dispatch.project.
// <KEY>.<type>.
const dispatchProjectPrefix = "notifications.dispatch.project."

// repoMappingType is the project event Dispatch emits when a repository's mapping to a project
// changes, on the project it changed for (a move emits on the project left and the one joined).
const repoMappingType = "settings.repo_project.updated"

// mappingSubject is the subject Dispatch publishes project's mapping changes on, which the
// Dispatch consumer hears beside every issue event.
func mappingSubject(project string) string {
	return dispatchProjectPrefix + project + "." + repoMappingType
}

// decodeDispatchProjectFact decodes one Dispatch project event: the mapping change of project is a
// RepoMappingChanged, and any other project's event, or another type, is no fact.
func decodeDispatchProjectFact(subject, project, payload string) (Fact, error) {
	subjectProject, eventType, ok := strings.Cut(strings.TrimPrefix(subject, dispatchProjectPrefix), ".")
	if !ok || subjectProject == "" {
		return nil, fmt.Errorf("Dispatch project subject has no project key: %s", subject)
	}
	if subjectProject != project || eventType != repoMappingType {
		return nil, nil
	}
	var event struct {
		Project string `json:"project"`
		Type    string `json:"type"`
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil, fmt.Errorf("decode Dispatch project event: %w", err)
	}
	if event.Project != subjectProject || event.Type != eventType {
		return nil, fmt.Errorf("Dispatch project event %s of %s disagrees with its subject %s", event.Type, event.Project, subject)
	}
	return RepoMappingChanged{Project: project}, nil
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func isJSONObject(raw json.RawMessage) bool {
	var item map[string]json.RawMessage
	return json.Unmarshal(raw, &item) == nil && item != nil
}

func decodeGitHubFact(subject string, repositories []ghrepo.Repository, payload string) (Fact, []string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &raw); err != nil || raw == nil {
		return nil, nil, nil
	}
	// The payload names the event's repository; the subject's segments cannot, since a name may hold
	// a dot. Only the event of a repository GitHub intake hears (ConsumerSpec.Repositories, as
	// SetRepositories changes them) is this daemon's, matched case-insensitively as GitHub matches
	// owner and repository names, and its fact names the heard repository.
	repo, _ := rawString(raw, "repo")
	at := slices.IndexFunc(repositories, func(heard ghrepo.Repository) bool { return strings.EqualFold(heard.String(), repo) })
	if at < 0 {
		return nil, nil, nil
	}
	repository := repositories[at]
	kind, ok := rawString(raw, "kind")
	if !ok {
		return nil, nil, nil
	}
	var fact Fact
	var err error
	switch kind {
	case "pr":
		fact, err = decodePullRequest(repository, raw)
	case "review":
		return decodeReview(repository, raw)
	case "push":
		fact, err = decodePush(repository, raw)
	case "checks":
		fact, err = decodeChecks(subject, repository, raw)
	}
	// Comments, and every other kind, route to the current role but do not change the durable
	// workflow record.
	return fact, nil, err
}

func decodePullRequest(repository ghrepo.Repository, raw map[string]json.RawMessage) (Fact, error) {
	number, action, ok := githubIdentity(raw)
	if !ok {
		return nil, nil
	}
	repo := repository.String()
	branch, _ := rawString(raw, "head_ref")
	sha, _ := rawString(raw, "head_sha")
	body, _ := rawString(raw, "body")
	updatedAt, _ := rawTimestamp(raw, "updated_at")
	switch action {
	case "opened", "reopened":
		// A reopened pull request is open again, recorded as when it opened.
		if sha == "" {
			return nil, nil
		}
		url, _ := rawString(raw, "url")
		return PullRequestOpened{Repo: repo, Number: number, Branch: branch, HeadSHA: sha, Body: body, URL: url, UpdatedAt: updatedAt, Reopened: action == "reopened"}, nil
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
		return PullRequestClosed{Repo: repo, Number: number, HeadSHA: sha, UpdatedAt: updatedAt}, nil
	default:
		return nil, nil
	}
}

func decodeReview(repository ghrepo.Repository, raw map[string]json.RawMessage) (Fact, []string, error) {
	number, action, ok := githubIdentity(raw)
	if !ok || action != "submitted" {
		return nil, nil, nil
	}
	state, _ := rawString(raw, "state")
	commitID, _ := rawString(raw, "commit_id")
	headSHA, _ := rawString(raw, "head_sha")
	author, _ := rawString(raw, "author")
	body, _ := rawString(raw, "body")
	bodyTruncated, _ := rawString(raw, "body_truncated")
	// The listener carries GitHub's review id as a decimal string; one that predates it carries
	// none, and the review is then ordered by when it is processed.
	var id int64
	if text, ok := rawString(raw, "review_id"); ok && text != "" {
		parsed, err := strconv.ParseInt(text, 10, 64)
		if err != nil || parsed <= 0 {
			return nil, nil, fmt.Errorf("review_id %q is not a positive integer", text)
		}
		id = parsed
	}
	// submitted_at is GitHub's RFC 3339 time; a listener that predates it carries none. One that
	// cannot be read is taken as none, and reported: a review without a time is ordered by its id,
	// so it is recorded rather than lost as poison.
	submittedAt, ok := rawTimestamp(raw, "submitted_at")
	var unread []string
	if !ok {
		unread = append(unread, fmt.Sprintf("submitted_at %s is not an RFC 3339 time", raw["submitted_at"]))
	}
	return PullRequestReview{Repo: repository.String(), Number: number, ID: id, SubmittedAt: submittedAt, State: strings.ToLower(state),
		CommitID: commitID, HeadSHA: headSHA, Author: author, Body: body, BodyTruncated: bodyTruncated == "true"}, unread, nil
}

func decodePush(repository ghrepo.Repository, raw map[string]json.RawMessage) (Fact, error) {
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
	pusher, _ := rawString(raw, "pusher")
	before, _ := rawString(raw, "before")
	forced := rawStringPointer(raw, "forced")
	return Push{Repo: repository.String(), Branch: strings.TrimPrefix(ref, "refs/heads/"), Before: before, After: after, ChangedPaths: changedPaths,
		Truncated: truncated, Forced: forced, Pusher: pusher}, nil
}

func decodeChecks(subject string, repository ghrepo.Repository, raw map[string]json.RawMessage) (Fact, error) {
	number, ok := rawNumber(raw, "number")
	if !ok || subject != githubRepositoryPrefix(repository)+".pr."+strconv.Itoa(number)+".checks" {
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
	return PullRequestChecks{Repo: repository.String(), Number: number, HeadSHA: headSHA, CheckRuns: runs, Generation: generation, Snapshot: snapshot, Failing: failed, Cancelled: cancelled}, nil
}

func githubIdentity(raw map[string]json.RawMessage) (int, string, bool) {
	number, numberOK := rawNumber(raw, "number")
	action, actionOK := rawString(raw, "action")
	return number, action, numberOK && actionOK
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

// rawTimestamp reads an optional RFC 3339 time: an absent, null or empty field is the zero time,
// and ok is false only when the field holds anything else, which is taken as the zero time too.
func rawTimestamp(raw map[string]json.RawMessage, key string) (time.Time, bool) {
	value, present := raw[key]
	if !present || string(value) == "null" {
		return time.Time{}, true
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return time.Time{}, false
	}
	if text == "" {
		return time.Time{}, true
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}
