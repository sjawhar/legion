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

// serverStartedSubject is the one subject Dispatch publishes its own start on (not an issue's):
// the daemon's third durable filters on it (OpenConsumers), and decodeMessage matches it whole.
const serverStartedSubject = "notifications.dispatch.server.started"

// decodeMessage decodes the Envoy envelope first, then the subject's source-specific payload.
// project is the daemon's Dispatch project: another project's issue event is a nil Fact.
// repositories are its configured repositories: a GitHub event whose payload names another
// repository is a nil Fact, whatever subject carried it. Dispatch's own start is matched before
// the issue subjects, which it is not one of.
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
	case subject == serverStartedSubject:
		if envelope.Source != "dispatch" {
			return decodedMessage{}, fmt.Errorf("Dispatch subject has envelope source %q", envelope.Source)
		}
		fact, err = decodeServerStarted(envelope.Payload)
	case strings.HasPrefix(subject, "notifications.dispatch.issue."):
		if envelope.Source != "dispatch" {
			return decodedMessage{}, fmt.Errorf("Dispatch subject has envelope source %q", envelope.Source)
		}
		fact, unread, err = decodeDispatchFact(subject, project, envelope.Payload)
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

// decodeServerStarted decodes Dispatch's server.started: the build commit Dispatch started on
// (ServerStarted), which the workflow compares against the open production checks. A start naming
// no commit — a Dispatch built without one stamped — is no fact, not poison: there is nothing to
// compare, and the daemon's own boot covers its own build.
func decodeServerStarted(payload string) (Fact, error) {
	var started struct {
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal([]byte(payload), &started); err != nil {
		return nil, fmt.Errorf("decode Dispatch server.started payload: %w", err)
	}
	if started.Commit == "" {
		return nil, nil
	}
	return ServerStarted{Commit: started.Commit}, nil
}

type dispatchEvent struct {
	ID       int64  `json:"id"`
	IssueKey string `json:"issue_key"`
	Seq      int64  `json:"seq"`
	Actor    struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"actor"`
	Notify *bool  `json:"notify"`
	Type   string `json:"type"`
	// CreatedAt is Dispatch's time of the event, zero when the event carries none. A review's
	// event shares it with the review's own row, which Dispatch writes in the same transaction.
	CreatedAt time.Time       `json:"created_at"`
	Payload   json.RawMessage `json:"payload"`
}

// mark is the event's place in its issue's event log.
func (e dispatchEvent) mark() record.EventMark {
	return record.EventMark{Seq: e.Seq, At: e.CreatedAt.UTC()}
}

// decodeDispatchFact decodes one Dispatch issue event of project. The stream carries every
// project's events; one whose subject key is not a project issue key is another daemon's, and is
// skipped before its payload is read, so it is never poison here. A field the mirror only records
// (an issue's claim, an ask's kind) that cannot be read is taken as unknown and named in unread,
// rather than making the event poison: an event the workflow acts on is never lost to a field it
// does not act on.
func decodeDispatchFact(subject, project, payload string) (Fact, []string, error) {
	subjectKey, ok := dispatchSubjectKey(subject)
	if !ok {
		return nil, nil, fmt.Errorf("Dispatch durable subject has no issue key: %s", subject)
	}
	if keyProject, _, _ := strings.Cut(subjectKey, "-"); keyProject != project || !claim.IsIssueKey(subjectKey) {
		return nil, nil, nil
	}
	var event dispatchEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil, nil, fmt.Errorf("decode Dispatch event: %w", err)
	}
	if event.ID <= 0 || event.Seq <= 0 || event.Notify == nil || !claim.IsIssueKey(event.IssueKey) || event.Type == "" || !isJSONObject(event.Payload) {
		return nil, nil, fmt.Errorf("Dispatch durable message payload is not a valid Dispatch event")
	}
	if subjectKey != event.IssueKey {
		return nil, nil, fmt.Errorf("Dispatch durable subject key %s disagrees with event issue_key %s", subjectKey, event.IssueKey)
	}
	var unread []string
	fact, err := decodeDispatchEvent(event, &unread)
	if err != nil {
		return nil, nil, err
	}
	return fact, unread, nil
}

// decodeDispatchEvent decodes a checked Dispatch event of the daemon's project by its type, adding
// to unread each field it took as unknown.
func decodeDispatchEvent(event dispatchEvent, unread *[]string) (Fact, error) {
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
			// An event retained from a Dispatch that did not yet send blocked_by decodes to no
			// blockers, as an issue waiting on nothing does: unlike a missing repository, which the
			// consumer fills, nothing reads an absence here.
			BlockedBy []string `json:"blocked_by"`
			// Claim is present on every issue event of a Dispatch that has claims (null for none),
			// and absent from an event retained from before it did, which says nothing of the claim.
			Claim json.RawMessage `json:"claim"`
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
		claimedBy, claimKnown := issueEventClaim(issue.Claim, unread)
		return DispatchIssue{Key: event.IssueKey, Seq: event.Seq, Type: event.Type, Status: issue.Status, Title: issue.Title, Parent: parent, Rank: issue.Rank,
			HandedOver: dispatch.CarriesLegionLabel(issue.Labels), ActorSession: actorSession, DaemonActor: daemonActor, ResolvedRepo: deref(resolved),
			ResolvedRepoMissing: missing, Blockers: issue.BlockedBy, ClaimedBy: claimedBy, ClaimKnown: claimKnown}, nil
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
		return DispatchArtifact{Key: event.IssueKey, ArtifactID: artifact.ArtifactID, Kind: kind, Version: artifact.Version, Reason: artifact.Reason, Mark: event.mark()}, nil
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
		return DispatchArtifact{Key: event.IssueKey, ArtifactID: artifact.ArtifactID, Kind: DispatchArtifactVersion, Version: artifact.Version.Number, Mark: event.mark()}, nil
	case "issue.deployed":
		var deployed struct {
			Repo        string `json:"repo"`
			Number      int    `json:"number"`
			RunID       int64  `json:"run_id"`
			CompletedAt string `json:"completed_at"`
		}
		if err := json.Unmarshal(event.Payload, &deployed); err != nil {
			return nil, fmt.Errorf("decode Dispatch issue.deployed payload: %w", err)
		}
		if deployed.Repo == "" {
			return nil, fmt.Errorf("Dispatch issue.deployed payload has no repo")
		}
		if deployed.Number <= 0 {
			return nil, fmt.Errorf("Dispatch issue.deployed payload has no positive integer number")
		}
		completedAt, err := time.Parse(time.RFC3339, deployed.CompletedAt)
		if err != nil {
			return nil, fmt.Errorf("Dispatch issue.deployed payload has no RFC 3339 completed_at: %w", err)
		}
		return IssueDeployed{Issue: event.IssueKey, Repo: deployed.Repo, Number: deployed.Number, RunID: deployed.RunID, CompletedAt: completedAt.UTC()}, nil
	case "ask.opened":
		var ask struct {
			ID     string `json:"id"`
			Kind   string `json:"kind"`
			Author struct {
				Kind string `json:"kind"`
				ID   string `json:"id"`
			} `json:"author"`
			Restored bool `json:"restored"`
		}
		if err := json.Unmarshal(event.Payload, &ask); err != nil {
			return nil, fmt.Errorf("decode Dispatch ask.opened payload: %w", err)
		}
		if ask.ID == "" {
			return nil, fmt.Errorf("Dispatch ask.opened payload has no id")
		}
		kind := askKind(ask.Kind, unread)
		// Settlement restoring a decision block it retracted appends ask.opened with the block's
		// original author: nobody asked anything, so it is the ask's state alone.
		if ask.Restored {
			return DispatchAskRestored{Key: event.IssueKey, Ask: ask.ID, Kind: kind, Open: true, Mark: event.mark()}, nil
		}
		session := ""
		if ask.Author.Kind == "session" {
			if ask.Author.ID == "" {
				return nil, fmt.Errorf("Dispatch ask.opened payload has a session author with no id")
			}
			session = ask.Author.ID
		}
		return DispatchAskOpened{Key: event.IssueKey, Ask: ask.ID, Kind: kind, AuthorSession: session, Mark: event.mark()}, nil
	case "ask.answered", "ask.resolved":
		var ask struct {
			ID       string `json:"id"`
			Kind     string `json:"kind"`
			Restored bool   `json:"restored"`
		}
		if err := json.Unmarshal(event.Payload, &ask); err != nil {
			return nil, fmt.Errorf("decode Dispatch %s payload: %w", event.Type, err)
		}
		if ask.ID == "" {
			return nil, fmt.Errorf("Dispatch %s payload has no id", event.Type)
		}
		kind := askKind(ask.Kind, unread)
		// Settlement restoring a decision block it retracted appends ask.answered with the answer
		// given before the retraction, credited to the editor who restored the block: nobody
		// answered anything, so it is the ask's state alone.
		if ask.Restored {
			return DispatchAskRestored{Key: event.IssueKey, Ask: ask.ID, Kind: kind, Mark: event.mark()}, nil
		}
		return DispatchAskClosed{Key: event.IssueKey, Ask: ask.ID, Kind: kind, State: strings.TrimPrefix(event.Type, "ask."),
			Actor: dispatch.Actor(event.Actor), Mark: event.mark()}, nil
	case "ask.handed_back":
		// An approval request its asker returned to the person (Dispatch's handBackApprovalAsk):
		// whatever the thread held, it now waits on the human, as a session's human-turn reply does.
		var ask struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(event.Payload, &ask); err != nil {
			return nil, fmt.Errorf("decode Dispatch ask.handed_back payload: %w", err)
		}
		if ask.ID == "" {
			return nil, fmt.Errorf("Dispatch ask.handed_back payload has no id")
		}
		return DispatchAskReply{Key: event.IssueKey, Ask: ask.ID, Kind: askKind(ask.Kind, unread), WaitingOn: "human",
			Actor: dispatch.Actor(event.Actor), Mark: event.mark()}, nil
	case "comment.created", "comment.answered":
		// comment.answered is a reply a session sends through a delivery's callback (Dispatch's
		// comment_delivery), which joins its thread as comment.created does and carries the same
		// ask_id and ask_waiting_on. A comment on no ask is no fact.
		var comment struct {
			Ask       string `json:"ask_id"`
			WaitingOn string `json:"ask_waiting_on"`
		}
		if err := json.Unmarshal(event.Payload, &comment); err != nil {
			return nil, fmt.Errorf("decode Dispatch %s payload: %w", event.Type, err)
		}
		if comment.Ask == "" {
			return nil, nil
		}
		// A turn the daemon cannot read could be either, so the reply moves nothing.
		switch comment.WaitingOn {
		case "", "human", "agent":
		default:
			*unread = append(*unread, fmt.Sprintf("ask_waiting_on %q is no turn", comment.WaitingOn))
			return nil, nil
		}
		return DispatchAskReply{Key: event.IssueKey, Ask: comment.Ask, WaitingOn: comment.WaitingOn, Actor: dispatch.Actor(event.Actor), Mark: event.mark()}, nil
	case "issue.claimed", "issue.released":
		var payload struct {
			Claim json.RawMessage `json:"claim"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return nil, fmt.Errorf("decode Dispatch %s payload: %w", event.Type, err)
		}
		// A claim event whose claim cannot be named records nothing: the next issue event, the
		// periodic re-read or the next boot's listing carries the claim Dispatch shows.
		holder, known := issueEventClaim(payload.Claim, unread)
		if known && event.Type == "issue.claimed" && holder == "" {
			*unread = append(*unread, "claim is null on issue.claimed")
			known = false
		}
		if !known {
			return nil, nil
		}
		return DispatchClaim{Key: event.IssueKey, Seq: event.Seq, ClaimedBy: holder}, nil
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

// issueEventClaim reads an issue event's claim, `<kind>:<id>`, "" for null, and whether the event
// showed it at all: an absent field, or a claim naming no actor, is unknown, the latter named in
// unread.
func issueEventClaim(raw json.RawMessage, unread *[]string) (string, bool) {
	if raw == nil {
		return "", false
	}
	var claimed *struct {
		Actor struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"actor"`
	}
	if err := json.Unmarshal(raw, &claimed); err != nil {
		*unread = append(*unread, fmt.Sprintf("claim %s is not a claim: %v", raw, err))
		return "", false
	}
	if claimed == nil {
		return "", true
	}
	if claimed.Actor.Kind == "" || claimed.Actor.ID == "" {
		*unread = append(*unread, fmt.Sprintf("claim %s names no actor", raw))
		return "", false
	}
	return claimed.Actor.Kind + ":" + claimed.Actor.ID, true
}

// askKind is an ask event's kind as the mirror keeps it: record.AskQuestion or record.AskApproval,
// and "" for an absent kind or one this daemon does not know (an event retained from before
// Dispatch had the two, whose kind is `action`), the latter named in unread.
func askKind(kind string, unread *[]string) string {
	switch kind {
	case record.AskQuestion, record.AskApproval, "":
		return kind
	}
	*unread = append(*unread, fmt.Sprintf("ask kind %q is not one the daemon knows", kind))
	return ""
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
	case "workflow":
		fact = decodeWorkflowRun(repository, raw)
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

// decodePush decodes a push to an issue's branch (claim.BranchIssue). A push to any other branch is
// no fact, so it is acknowledged without a transaction: the workflow applies a push only to the pull
// request it records on the push's branch, and it records one only on an issue's branch. A mapped
// repository that other automation pushes to many times a minute would otherwise cost the daemon one
// transaction under the fact lock (ApplyFact) for every push, all of them changing nothing.
func decodePush(repository ghrepo.Repository, raw map[string]json.RawMessage) (Fact, error) {
	ref, _ := rawString(raw, "ref")
	branch, isHead := strings.CutPrefix(ref, "refs/heads/")
	if _, isIssueBranch := claim.BranchIssue(branch); !isHead || !isIssueBranch {
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
	return Push{Repo: repository.String(), Branch: branch, Before: before, After: after, ChangedPaths: changedPaths,
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

// decodeWorkflowRun reads a workflow run's envelope (Envoy's `workflow_run` payload): its
// completion is a WorkflowRunCompleted, and every other action, or a completion that names no
// positive run id and attempt (decimal strings, as Envoy writes GitHub's numbers), no workflow
// path or no branch, is no fact. A malformed run is acknowledged rather than poison: it can wake
// nothing, and the next run's envelope is read on its own.
func decodeWorkflowRun(repository ghrepo.Repository, raw map[string]json.RawMessage) Fact {
	if action, _ := rawString(raw, "action"); action != "completed" {
		return nil
	}
	runID, ok := rawPositiveDecimal(raw, "run_id", 64)
	if !ok {
		return nil
	}
	attempt, ok := rawPositiveDecimal(raw, "run_attempt", 32)
	if !ok {
		return nil
	}
	path, _ := rawString(raw, "path")
	branch, _ := rawString(raw, "head_branch")
	if path == "" || branch == "" {
		return nil
	}
	return WorkflowRunCompleted{Repo: repository.String(), Path: path, RunID: runID, Attempt: int(attempt), HeadBranch: branch}
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
		if err := json.Unmarshal(value, &text); err != nil || !decimalDigits.MatchString(text) {
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

// decimalDigits is a number as Envoy writes GitHub's numeric fields into a payload: decimal digits
// alone, with none of the sign or base prefix strconv would otherwise accept. rawNumber holds a
// number written as a string to it, and rawPositiveDecimal every number it reads.
var decimalDigits = regexp.MustCompile(`^[0-9]+$`)

// rawPositiveDecimal reads a positive integer of at most bits bits written as a decimal string.
func rawPositiveDecimal(raw map[string]json.RawMessage, key string, bits int) (int64, bool) {
	text, ok := rawString(raw, key)
	if !ok || !decimalDigits.MatchString(text) {
		return 0, false
	}
	value, err := strconv.ParseInt(text, 10, bits)
	return value, err == nil && value > 0
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
