package contracts

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/core"
)

type GithubEnvelopeInput struct {
	Event    string
	Delivery string
	Body     map[string]any
	EventID  string
	TraceID  string
}

func GithubEnvelope(input GithubEnvelopeInput) Envelope {
	owner, repo := githubRepo(input.Body)
	topic := githubTopic(owner, repo, input.Event, input.Body)
	return Envelope{
		EventID:        input.EventID,
		Source:         "github",
		SourceEventID:  input.Delivery,
		Topic:          topic,
		DedupeKey:      "github." + input.Delivery,
		IssuedAt:       NowMillis(),
		PayloadSummary: githubSummary(input.Event, input.Body),
		Payload:        githubPayload(input.Event, input.Body),
		TraceID:        input.TraceID,
	}
}

func GithubEnvelopes(input GithubEnvelopeInput, trigger string) []Envelope {
	if input.Event == "workflow_run" && len(sliceValue(nested(input.Body, "workflow_run", "pull_requests"))) > 0 {
		return nil
	}
	if githubCIEvent(input.Event) {
		// CI events fold into envoy_ci_state through the webhook handler's
		// CIRecorder. The listener emits one settled pr.<n>.checks envelope.
		return nil
	}
	item := GithubEnvelope(input)
	if item.Topic == "" {
		// Event should not be routed (e.g. push to non-heads/tags ref).
		return nil
	}
	if !githubCommentEvent(input.Event) {
		return []Envelope{item}
	}
	body := githubCommentBody(input.Event, input.Body)
	if !ContainsMention(body, trigger) {
		return []Envelope{item}
	}
	owner, repo := githubRepo(input.Body)
	// Publish mention topic with same structure: type.number.mention
	num := githubNumber(input.Event, input.Body)
	base := githubParentKind(input.Event, input.Body)
	// Fan-out copies share the base dedupe key, and the MOST SPECIFIC copy is
	// published FIRST. The listener dedupes per (dedupe_key, session) and the
	// per-machine listener consumes in stream (= publish) order, so a session
	// whose subscriptions match several copies receives exactly one push,
	// labeled with the most specific matching topic. Publishing the base copy
	// first instead would silently starve the more specific subscriptions.
	mentions := []Envelope{}
	if num != "" {
		// notifications.github.owner.repo.pr.7706.mention
		mention := item
		mention.Topic = GithubSubject(owner, repo, base+"."+num+".mention")
		mentions = append(mentions, mention)
	}
	// Also publish repo-wide mention: notifications.github.owner.repo.mention
	mention := item
	mention.Topic = GithubSubject(owner, repo, "mention")
	mentions = append(mentions, mention)
	return append(mentions, item)
}

// CIObservation is a per-PR check_run or check_suite fact for the CI state
// aggregator.
type CIObservation struct {
	Owner      string
	Repo       string
	Number     string
	SHA        string
	AppID      string
	CheckName  string
	CheckRunID uint64
	SuiteID    string
	URL        string
	Status     string
	Conclusion string
	ObservedAt string
}

// GithubCIObservations extracts one CI observation per associated PR. Both
// check runs and suites are folded into durable per-head state; neither emits a
// raw webhook envelope.
func GithubCIObservations(event string, body map[string]any) []CIObservation {
	var key string
	switch event {
	case "check_run":
		key = "check_run"
	case "check_suite":
		key = "check_suite"
	default:
		return nil
	}
	prs := githubCIPullRequests(event, body)
	if len(prs) == 0 {
		return nil
	}
	sha := nestedString(body, key, "head_sha")
	if sha == "" {
		return nil
	}
	owner, repo := githubRepo(body)
	obs := CIObservation{
		Owner:      owner,
		Repo:       repo,
		SHA:        sha,
		AppID:      nestedNumberString(body, key, "app", "id"),
		Status:     nestedString(body, key, "status"),
		Conclusion: nestedString(body, key, "conclusion"),
	}
	if event == "check_run" {
		checkRunID := githubPositiveUint64(nested(body, key, "id"))
		if checkRunID == 0 {
			log.Printf("github ci check run skipped: invalid id=%v", nested(body, key, "id"))
			return nil
		}
		obs.CheckName = nestedString(body, key, "name")
		obs.CheckRunID = checkRunID
		obs.URL = nestedString(body, key, "html_url")
		obs.ObservedAt = nestedString(body, key, "completed_at")
		if obs.ObservedAt == "" {
			obs.ObservedAt = nestedString(body, key, "started_at")
		}
		if obs.CheckName == "" {
			return nil
		}
	} else {
		obs.SuiteID = nestedNumberString(body, key, "id")
		obs.ObservedAt = nestedString(body, key, "updated_at")
		if obs.SuiteID == "" {
			return nil
		}
	}
	out := make([]CIObservation, 0, len(prs))
	for _, pr := range prs {
		obs.Number = pr
		out = append(out, obs)
	}
	return out
}

func GithubIsBotSender(body map[string]any) bool {
	return strings.EqualFold(nestedString(body, "sender", "type"), "Bot")
}

func githubRepo(body map[string]any) (string, string) {
	owner := nestedString(body, "repository", "owner", "login")
	repo := nestedString(body, "repository", "name")
	if owner == "" || repo == "" {
		owner2, name2, found := strings.Cut(nestedString(body, "repository", "full_name"), "/")
		if found && owner == "" {
			owner = owner2
		}
		if found && repo == "" {
			repo = name2
		}
	}
	if owner == "" {
		owner = "unknown"
	}
	if repo == "" {
		repo = "unknown"
	}
	return owner, repo
}

func ContainsMention(body string, trigger string) bool {
	body = strings.ToLower(body)
	trigger = strings.ToLower(strings.TrimSpace(trigger))
	if body == "" || trigger == "" {
		return false
	}
	idx := strings.Index(body, trigger)
	for idx >= 0 {
		if mentionEdge(body, idx, idx+len(trigger)) {
			return true
		}
		next := idx + len(trigger)
		if next >= len(body) {
			return false
		}
		more := strings.Index(body[next:], trigger)
		if more < 0 {
			return false
		}
		idx = next + more
	}
	return false
}

type SlackEnvelopeInput struct {
	Body    map[string]any
	EventID string
	TraceID string
}

func SlackEnvelope(input SlackEnvelopeInput) Envelope {
	team := stringValue(input.Body["team_id"])
	if team == "" {
		team = "unknown"
	}
	event := mapValue(input.Body["event"])
	channel := stringValue(event["channel"])
	if channel == "" {
		channel = "unknown"
	}
	return Envelope{
		EventID:        input.EventID,
		Source:         "slack",
		SourceEventID:  stringValue(input.Body["event_id"]),
		Topic:          SlackSubject(team, channel, slackKind(input.Body)),
		DedupeKey:      "slack." + stringValue(input.Body["event_id"]),
		IssuedAt:       NowMillis(),
		PayloadSummary: slackSummary(input.Body),
		Payload:        slackPayload(input.Body),
		TraceID:        input.TraceID,
	}
}

func SlackEnvelopes(input SlackEnvelopeInput) []Envelope {
	item := SlackEnvelope(input)
	out := []Envelope{item}
	event := mapValue(input.Body["event"])
	thread := stringValue(event["thread_ts"])
	if thread != "" {
		team := stringValue(input.Body["team_id"])
		channel := stringValue(event["channel"])
		if team != "" && channel != "" {
			threaded := item
			threaded.Topic = SlackThreadSubject(team, channel, thread, slackKind(input.Body))
			// Thread copy FIRST, sharing the base dedupe key: per-(key, session)
			// dedupe + publish-order delivery means a dual-subscribed session gets
			// one push labeled with the most specific topic, while thread-only and
			// channel-only subscribers each get their matching copy.
			out = []Envelope{threaded, item}
		}
	}
	return out
}

// githubTopic builds the full topic path with number hierarchy.
// PR #7706 opened:        pr.7706
// Comment on PR #7706:    pr.7706.comment
// Review on PR #7706:     pr.7706.review
// Issue #42 opened:       issue.42
// Comment on issue #42:   issue.42.comment
// Push to branch main:    push.branch.main
// Push to tag v1.0.0:     push.tag.v1_0_0 (dots sanitized to underscores)
// workflow_run:            workflow.<filename>.<action>
// Returns empty string for events that should not be routed (e.g. push to refs that aren't heads or tags).
func githubTopic(owner string, repo string, event string, body map[string]any) string {
	if event == "push" {
		refType, refName, ok := githubPushRefSegments(body)
		if !ok {
			return ""
		}
		return GithubPushSubject(owner, repo, refType, refName)
	}
	if event == "workflow_run" {
		filename := githubWorkflowFilename(body)
		action := stringValue(body["action"])
		if filename == "" || action == "" {
			return ""
		}
		return GithubWorkflowSubject(owner, repo, filename, action)
	}
	num := githubNumber(event, body)
	parent := githubParentKind(event, body)
	kind := githubKind(event)
	if num != "" && kind != parent {
		// e.g., pr.7706.comment, pr.7706.review, issue.42.comment
		return GithubSubject(owner, repo, parent+"."+num+"."+kind)
	}
	if num != "" {
		// e.g., pr.7706, issue.42
		return GithubSubject(owner, repo, kind+"."+num)
	}
	// e.g., ci (un-PR'd; filtered out by GithubEnvelopes)
	return GithubSubject(owner, repo, kind)
}

// githubPushRefSegments splits a push event's ref field into (refType, refName).
// Returns false for refs other than refs/heads/... or refs/tags/...
func githubPushRefSegments(body map[string]any) (refType, refName string, ok bool) {
	ref := stringValue(body["ref"])
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		return "branch", strings.TrimPrefix(ref, "refs/heads/"), true
	case strings.HasPrefix(ref, "refs/tags/"):
		return "tag", strings.TrimPrefix(ref, "refs/tags/"), true
	}
	return "", "", false
}

// githubWorkflowFilename returns the basename of the workflow_run.path field,
// e.g. ".github/workflows/ci.yml" -> "ci.yml". Returns "" if the path is missing.
func githubWorkflowFilename(body map[string]any) string {
	path := nestedString(body, "workflow_run", "path")
	if path == "" {
		return ""
	}
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

// nestedNumberString formats a nested numeric (or any) value as a string,
// returning "" if missing. JSON numbers always unmarshal to float64; whole
// values are formatted as integers so large IDs (e.g. GitHub run_id) do not
// render in scientific notation.
func nestedNumberString(body map[string]any, keys ...string) string {
	v := nested(body, keys...)
	if v == nil {
		return ""
	}
	if f, ok := v.(float64); ok && !math.IsNaN(f) && !math.IsInf(f, 0) && f == math.Trunc(f) {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprintf("%v", v)
}

// githubParentKind returns the entity type that owns the number.
// For issue_comment, checks body["issue"]["pull_request"] to distinguish PR vs issue.
func githubParentKind(event string, body map[string]any) string {
	switch event {
	case "pull_request", "pull_request_review", "pull_request_review_comment":
		return "pr"
	case "issues":
		return "issue"
	case "sub_issues":
		return "issue"
	case "issue_comment":
		if nested(body, "issue", "pull_request") != nil {
			return "pr"
		}
		return "issue"
	}
	return ""
}

func githubKind(event string) string {
	switch event {
	case "pull_request":
		return "pr"
	case "issues":
		return "issue"
	case "sub_issues":
		return "sub_issue"
	case "push":
		return "push"
	case "check_run", "check_suite":
		return "ci"
	case "workflow_run":
		return "workflow"
	case "issue_comment":
		return "comment"
	case "pull_request_review":
		return "review"
	case "pull_request_review_comment":
		return "comment"
	default:
		return "comment"
	}
}

// GithubPRNumber returns a non-negative integer encoded as a JSON number or decimal-digit string.
func GithubPRNumber(value any) string {
	switch number := value.(type) {
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number != math.Trunc(number) {
			return ""
		}
		return strconv.FormatFloat(number, 'f', -1, 64)
	case int:
		if number < 0 {
			return ""
		}
		return strconv.Itoa(number)
	case int64:
		if number < 0 {
			return ""
		}
		return strconv.FormatInt(number, 10)
	case uint64:
		return strconv.FormatUint(number, 10)
	case string:
		if number == "" {
			return ""
		}
		for _, character := range number {
			if character < '0' || character > '9' {
				return ""
			}
		}
		return number
	default:
		return ""
	}
}

func githubPositiveUint64(value any) uint64 {
	number := GithubPRNumber(value)
	if number == "" {
		return 0
	}
	id, err := strconv.ParseUint(number, 10, 64)
	if err != nil || id == 0 {
		return 0
	}
	return id
}

func githubNumber(event string, body map[string]any) string {
	switch event {
	case "pull_request", "pull_request_review", "pull_request_review_comment":
		return GithubPRNumber(nested(body, "pull_request", "number"))
	case "issues":
		return GithubPRNumber(nested(body, "issue", "number"))
	case "sub_issues":
		return GithubPRNumber(nested(body, "parent_issue", "number"))
	case "issue_comment":
		return GithubPRNumber(nested(body, "issue", "number"))
	case "check_run", "check_suite":
		if nums := githubCIPullRequests(event, body); len(nums) > 0 {
			return nums[0]
		}
	}
	return ""
}

func githubCommentEvent(event string) bool {
	switch event {
	case "issue_comment", "pull_request_review_comment", "pull_request_review":
		return true
	}
	return false
}

func githubCIEvent(event string) bool {
	switch event {
	case "check_run", "check_suite":
		return true
	}
	return false
}

func githubCIPullRequests(event string, body map[string]any) []string {
	var key string
	switch event {
	case "check_run":
		key = "check_run"
	case "check_suite":
		key = "check_suite"
	default:
		return nil
	}
	obj := mapValue(body[key])
	if obj == nil {
		return nil
	}
	prs := sliceValue(obj["pull_requests"])
	var nums []string
	for _, pr := range prs {
		prMap := mapValue(pr)
		if prMap == nil {
			continue
		}
		if number := GithubPRNumber(prMap["number"]); number != "" {
			nums = append(nums, number)
		}
	}
	return nums
}

func githubCommentBody(event string, body map[string]any) string {
	action := stringValue(body["action"])
	if event == "pull_request_review" {
		if action != "submitted" {
			return ""
		}
		return nestedString(body, "review", "body")
	}
	if action != "created" {
		return ""
	}
	return nestedString(body, "comment", "body")
}

func githubSummary(event string, body map[string]any) string {
	owner, repo := githubRepo(body)
	repository := owner + "/" + repo
	action := stringValue(body["action"])
	number := githubNumber(event, body)

	var summary string
	switch event {
	case "issue_comment":
		author := nestedString(body, "comment", "user", "login")
		if action == "created" {
			summary = fmt.Sprintf(
				"comment on %s#%s by %s: %s",
				repository,
				number,
				author,
				first(nestedString(body, "comment", "body"), 100),
			)
		} else {
			summary = fmt.Sprintf("comment %s on %s#%s by %s", action, repository, number, author)
		}
	case "pull_request_review_comment":
		line := nestedNumberString(body, "comment", "line")
		if line == "" {
			line = nestedNumberString(body, "comment", "original_line")
		}
		author := nestedString(body, "comment", "user", "login")
		path := nestedString(body, "comment", "path")
		if action == "created" {
			summary = fmt.Sprintf(
				"review comment on %s#%s by %s (%s:%s): %s",
				repository,
				number,
				author,
				path,
				line,
				first(nestedString(body, "comment", "body"), 80),
			)
		} else {
			summary = fmt.Sprintf(
				"review comment %s on %s#%s by %s (%s:%s)",
				action,
				repository,
				number,
				author,
				path,
				line,
			)
		}
	case "pull_request_review":
		summary = fmt.Sprintf(
			"review %s on %s#%s by %s",
			nestedString(body, "review", "state"),
			repository,
			number,
			nestedString(body, "review", "user", "login"),
		)
		if reviewBody := nestedString(body, "review", "body"); reviewBody != "" {
			summary += ": " + first(reviewBody, 80)
		}
	case "pull_request":
		if action == "closed" && boolValue(nested(body, "pull_request", "merged")) {
			summary = fmt.Sprintf(
				"pr merged: %s#%s by %s → %s",
				repository,
				number,
				nestedString(body, "pull_request", "merged_by", "login"),
				shortSHA(nestedString(body, "pull_request", "merge_commit_sha")),
			)
		} else {
			summary = fmt.Sprintf(
				"pr %s: %s#%s %s",
				action,
				repository,
				number,
				first(nestedString(body, "pull_request", "title"), 90),
			)
		}
	case "sub_issues":
		summary = fmt.Sprintf("sub issue %s: %s#%s", action, repository, number)
	case "issues":
		summary = fmt.Sprintf(
			"issue %s: %s#%s %s",
			action,
			repository,
			number,
			first(nestedString(body, "issue", "title"), 90),
		)
	case "push":
		_, refName, _ := githubPushRefSegments(body)
		summary = fmt.Sprintf(
			"push to %s: %s (%s) by %s",
			refName,
			first(nestedString(body, "head_commit", "message"), 70),
			shortSHA(stringValue(body["after"])),
			nestedString(body, "pusher", "name"),
		)
	case "workflow_run":
		status := nestedString(body, "workflow_run", "status")
		if conclusion := nestedString(body, "workflow_run", "conclusion"); conclusion != "" {
			status += "/" + conclusion
		}
		summary = fmt.Sprintf(
			"workflow %s %s run %s %s",
			nestedString(body, "workflow_run", "name"),
			nestedString(body, "workflow_run", "head_branch"),
			nestedNumberString(body, "workflow_run", "id"),
			status,
		)
	default:
		summary = fmt.Sprintf("%s %s", event, action)
	}
	return OneLineSummary(summary)
}

func githubPayload(event string, body map[string]any) string {
	repo := nestedString(body, "repository", "full_name")
	if repo == "" {
		owner, name := githubRepo(body)
		if owner != "unknown" && name != "unknown" {
			repo = owner + "/" + name
		}
	}
	action := stringValue(body["action"])
	number := githubNumber(event, body)

	var data map[string]string
	switch event {
	case "issue_comment":
		commentBody := nestedString(body, "comment", "body")
		data = map[string]string{
			"kind":        "comment",
			"action":      action,
			"repo":        repo,
			"number":      number,
			"title":       nestedString(body, "issue", "title"),
			"parent_kind": githubParentKind(event, body),
			"author":      nestedString(body, "comment", "user", "login"),
			"url":         nestedString(body, "comment", "html_url"),
		}
		if action == "edited" {
			data["body_changed"] = "true"
		} else {
			addCappedBody(data, commentBody)
		}
		if marker := core.ParseAskMarker(commentBody); marker != nil && marker.Origin != nil && marker.Origin.SessionID != "" {
			data["dispatch_session"] = marker.Origin.SessionID
		}
	case "pull_request_review_comment":
		data = map[string]string{
			"kind":        "comment",
			"action":      action,
			"repo":        repo,
			"number":      number,
			"title":       nestedString(body, "pull_request", "title"),
			"parent_kind": githubParentKind(event, body),
			"author":      nestedString(body, "comment", "user", "login"),
			"url":         nestedString(body, "comment", "html_url"),
			"path":        nestedString(body, "comment", "path"),
		}
		line := nestedNumberString(body, "comment", "line")
		if line == "" {
			line = nestedNumberString(body, "comment", "original_line")
		}
		data["line"] = line
		if action == "edited" {
			data["body_changed"] = "true"
		} else {
			addCappedBody(data, nestedString(body, "comment", "body"))
		}
	case "pull_request_review":
		data = map[string]string{
			"kind":        "review",
			"action":      action,
			"repo":        repo,
			"number":      number,
			"title":       nestedString(body, "pull_request", "title"),
			"parent_kind": "pr",
			"author":      nestedString(body, "review", "user", "login"),
			"url":         nestedString(body, "review", "html_url"),
			"state":       nestedString(body, "review", "state"),
		}
		addCappedBody(data, nestedString(body, "review", "body"))
	case "pull_request":
		data = map[string]string{
			"kind":             "pr",
			"action":           action,
			"repo":             repo,
			"number":           number,
			"title":            nestedString(body, "pull_request", "title"),
			"author":           nestedString(body, "pull_request", "user", "login"),
			"url":              nestedString(body, "pull_request", "html_url"),
			"head_sha":         nestedString(body, "pull_request", "head", "sha"),
			"head_ref":         nestedString(body, "pull_request", "head", "ref"),
			"base_ref":         nestedString(body, "pull_request", "base", "ref"),
			"merged":           strconv.FormatBool(boolValue(nested(body, "pull_request", "merged"))),
			"merge_commit_sha": nestedString(body, "pull_request", "merge_commit_sha"),
			"merged_by":        nestedString(body, "pull_request", "merged_by", "login"),
			"updated_at":       nestedString(body, "pull_request", "updated_at"),
		}
		addCappedBody(data, nestedString(body, "pull_request", "body"))
	case "issues":
		issueBody := nestedString(body, "issue", "body")
		data = map[string]string{
			"kind":   "issue",
			"action": action,
			"repo":   repo,
			"number": number,
			"title":  nestedString(body, "issue", "title"),
			"author": nestedString(body, "issue", "user", "login"),
			"url":    nestedString(body, "issue", "html_url"),
		}
		addCappedBody(data, issueBody)
		if marker := core.ParseMetaMarker(issueBody); marker != nil && marker.Origin != nil && marker.Origin.SessionID != "" {
			data["dispatch_session"] = marker.Origin.SessionID
		}
	case "push":
		data = map[string]string{
			"kind":         "push",
			"repo":         repo,
			"ref":          stringValue(body["ref"]),
			"after":        stringValue(body["after"]),
			"before":       stringValue(body["before"]),
			"pusher":       nestedString(body, "pusher", "name"),
			"head_subject": firstNonEmptyLine(nestedString(body, "head_commit", "message")),
			"commit_count": strconv.Itoa(len(sliceValue(body["commits"]))),
			"compare_url":  stringValue(body["compare"]),
		}
	case "workflow_run":
		headBranch := nestedString(body, "workflow_run", "head_branch")
		data = map[string]string{
			"kind":           "workflow",
			"action":         action,
			"repo":           repo,
			"workflow":       nestedString(body, "workflow_run", "name"),
			"path":           nestedString(body, "workflow_run", "path"),
			"branch":         headBranch,
			"head_branch":    headBranch,
			"status":         nestedString(body, "workflow_run", "status"),
			"conclusion":     nestedString(body, "workflow_run", "conclusion"),
			"run_id":         nestedNumberString(body, "workflow_run", "id"),
			"run_attempt":    nestedNumberString(body, "workflow_run", "run_attempt"),
			"head_sha":       nestedString(body, "workflow_run", "head_sha"),
			"pr_numbers":     githubWorkflowPullRequestNumbers(body),
			"run_started_at": nestedString(body, "workflow_run", "run_started_at"),
			"updated_at":     nestedString(body, "workflow_run", "updated_at"),
			"url":            nestedString(body, "workflow_run", "html_url"),
		}
	default:
		return ""
	}
	return payloadJSON(data)
}

func githubWorkflowPullRequestNumbers(body map[string]any) string {
	var numbers []string
	for _, pullRequest := range sliceValue(nested(body, "workflow_run", "pull_requests")) {
		number := nestedNumberString(mapValue(pullRequest), "number")
		if number != "" {
			numbers = append(numbers, number)
		}
	}
	return strings.Join(numbers, ",")
}

func addCappedBody(data map[string]string, body string) {
	capped, truncated := capBody(body)
	data["body"] = capped
	if truncated {
		data["body_truncated"] = "true"
	}
}

func mentionEdge(body string, start int, end int) bool {
	if !mentionLeft(body, start) {
		return false
	}
	return mentionRight(body, end)
}

func mentionLeft(body string, idx int) bool {
	if idx == 0 {
		return true
	}
	return !mentionWord(rune(body[idx-1]))
}

func mentionRight(body string, idx int) bool {
	if idx >= len(body) {
		return true
	}
	return !mentionWord(rune(body[idx]))
}

func mentionWord(ch rune) bool {
	if ch >= 'a' && ch <= 'z' {
		return true
	}
	if ch >= '0' && ch <= '9' {
		return true
	}
	if ch == '_' {
		return true
	}
	if ch == '@' {
		return true
	}
	if ch == '.' {
		return true
	}
	return false
}

func slackKind(body map[string]any) string {
	event := mapValue(body["event"])
	if stringValue(event["type"]) == "app_mention" {
		return "mention"
	}
	return "message"
}

func slackSummary(body map[string]any) string {
	event := mapValue(body["event"])
	message := slackMessage(event)
	subtype := stringValue(event["subtype"])
	channel := slackEventString(event, message, "channel")
	if channel == "" {
		channel = "unknown"
	}
	text := ""
	if subtype != "message_deleted" {
		text = slackEventString(event, message, "text")
	}

	var summary string
	switch {
	case stringValue(event["type"]) == "app_mention":
		summary = fmt.Sprintf(
			"slack mention in %s from %s: %s",
			channel,
			slackEventString(event, message, "user"),
			first(text, 100),
		)
	case subtype == "":
		if thread := slackEventString(event, message, "thread_ts"); thread != "" {
			summary = fmt.Sprintf(
				"slack thread reply in %s from %s (thread %s): %s",
				channel,
				slackEventString(event, message, "user"),
				thread,
				first(text, 100),
			)
		} else {
			summary = fmt.Sprintf(
				"slack message in %s from %s: %s",
				channel,
				slackEventString(event, message, "user"),
				first(text, 100),
			)
		}
	case subtype == "bot_message":
		summary = fmt.Sprintf("slack bot message in %s from %s: %s", channel, slackBotName(event), first(text, 100))
	case subtype == "message_changed":
		summary = fmt.Sprintf(
			"slack message edited in %s by %s at %s: %s",
			channel,
			slackEventString(event, message, "user"),
			slackEventString(event, message, "ts"),
			first(text, 100),
		)
	case subtype == "message_deleted":
		summary = fmt.Sprintf("slack message deleted in %s: %s", channel, stringValue(event["deleted_ts"]))
	default:
		summary = fmt.Sprintf("slack %s message in %s: %s", subtype, channel, first(text, 100))
	}
	if suffix := slackFilesSuffix(slackFiles(event, message)); suffix != "" {
		summary += suffix
	}
	return OneLineSummary(summary)
}

func slackPayload(body map[string]any) string {
	event := mapValue(body["event"])
	message := slackMessage(event)
	subtype := stringValue(event["subtype"])
	data := map[string]string{"kind": slackKind(body)}
	data["event_type"] = stringValue(event["type"])
	data["subtype"] = subtype
	data["team_id"] = stringValue(body["team_id"])
	data["channel_id"] = slackEventString(event, message, "channel")
	data["channel_type"] = slackEventString(event, message, "channel_type")
	data["user_id"] = slackEventString(event, message, "user")
	data["bot_id"] = slackEventString(event, message, "bot_id")
	data["bot_name"] = slackBotName(event)
	data["ts"] = slackEventString(event, message, "ts")
	data["event_ts"] = stringValue(event["event_ts"])
	data["thread_ts"] = slackEventString(event, message, "thread_ts")
	if subtype == "thread_broadcast" {
		data["root_ts"] = nestedString(event, "root", "ts")
	}
	if subtype != "message_deleted" {
		if text := slackEventString(event, message, "text"); text != "" {
			capped, truncated := capBody(text)
			data["text"] = capped
			if truncated {
				data["body_truncated"] = "true"
			}
		}
	}
	if subtype == "message_changed" {
		data["edited_by"] = nestedString(message, "edited", "user")
	}
	if subtype == "message_deleted" {
		data["deleted_ts"] = stringValue(event["deleted_ts"])
	}
	files := slackFiles(event, message)
	if len(files) > 0 {
		data["file_count"] = strconv.Itoa(len(files))
		var filePairs []string
		for _, file := range files {
			if len(filePairs) == 10 {
				break
			}
			fileData := mapValue(file)
			if fileData == nil {
				continue
			}
			filePairs = append(filePairs, stringValue(fileData["name"])+"|"+stringValue(fileData["filetype"]))
		}
		data["files"] = strings.Join(filePairs, ",")
	}
	if attachments := sliceValue(message["attachments"]); len(attachments) > 0 {
		data["attachment_count"] = strconv.Itoa(len(attachments))
	} else if subtype == "message_changed" {
		if attachments := sliceValue(event["attachments"]); len(attachments) > 0 {
			data["attachment_count"] = strconv.Itoa(len(attachments))
		}
	}
	return payloadJSON(data)
}

func slackMessage(event map[string]any) map[string]any {
	if stringValue(event["subtype"]) == "message_changed" {
		if message := mapValue(event["message"]); message != nil {
			return message
		}
	}
	return event
}

func slackEventString(event, message map[string]any, key string) string {
	if value := stringValue(message[key]); value != "" {
		return value
	}
	return stringValue(event[key])
}

func slackBotName(event map[string]any) string {
	if username := stringValue(event["username"]); username != "" {
		return username
	}
	if name := nestedString(event, "bot_profile", "name"); name != "" {
		return name
	}
	return stringValue(event["bot_id"])
}

func slackFiles(event, message map[string]any) []any {
	if files := sliceValue(message["files"]); len(files) > 0 {
		return files
	}
	if stringValue(event["subtype"]) == "message_changed" {
		return sliceValue(event["files"])
	}
	return nil
}

func slackFilesSuffix(files []any) string {
	if len(files) == 0 {
		return ""
	}
	file := mapValue(files[0])
	label := nestedString(file, "title")
	if label == "" {
		label = stringValue(file["filetype"])
	}
	return fmt.Sprintf(" (%d file(s): %s)", len(files), first(label, 80))
}

func payloadJSON(data map[string]string) string {
	for key, value := range data {
		if value == "" {
			delete(data, key)
		}
	}
	out, _ := json.Marshal(data)
	return string(out)
}

func nested(body map[string]any, keys ...string) any {
	var cur any = body
	for _, key := range keys {
		item := mapValue(cur)
		if item == nil {
			return nil
		}
		cur = item[key]
	}
	return cur
}

func nestedString(body map[string]any, keys ...string) string {
	return stringValue(nested(body, keys...))
}

func mapValue(value any) map[string]any {
	item, _ := value.(map[string]any)
	return item
}

func sliceValue(value any) []any {
	items, _ := value.([]any)
	return items
}

func stringValue(value any) string {
	switch text := value.(type) {
	case string:
		return text
	default:
		return ""
	}
}

func first(s string, maxRunes int) string {
	return truncateWithEllipsis(firstNonEmptyLine(s), maxRunes)
}

func firstNonEmptyLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimLeft(line, " \t\r")
		if strings.TrimSpace(line) != "" {
			return line
		}
	}
	return ""
}

// OneLineSummary returns the first non-empty message line, capped at 160 runes
// including its ellipsis. It is the common human-facing summary contract for
// normalized webhooks, listener API messages, and settled CI notifications.
func OneLineSummary(s string) string {
	line := firstNonEmptyLine(s)
	runes := []rune(line)
	if len(runes) <= 160 {
		return line
	}
	return string(runes[:159]) + "…"
}

func truncateWithEllipsis(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	if maxRunes <= 0 {
		return "…"
	}
	return string(runes[:maxRunes]) + "…"
}

func capBody(s string) (string, bool) {
	runes := []rune(s)
	if len(runes) <= 2048 {
		return s, false
	}
	return string(runes[:2048]), true
}

func shortSHA(sha string) string {
	runes := []rune(sha)
	if len(runes) <= 7 {
		return sha
	}
	return string(runes[:7])
}

func boolValue(value any) bool {
	flag, _ := value.(bool)
	return flag
}

type GhostWisprEnvelopeInput struct {
	EventType string
	Delivery  string
	Body      map[string]any
	EventID   string
	TraceID   string
}

var ghostWisprTopicSegmentSanitizer = strings.NewReplacer(
	".", "_",
	" ", "_",
	"\n", "_",
	"\r", "_",
	"\t", "_",
	">", "_",
	"*", "_",
	"/", "_",
)

// GhostWisprEnvelope normalizes a Ghost Wispr webhook event into an Envoy envelope.
// Returns a single envelope (no fan-out — Ghost Wispr events are 1:1).
func GhostWisprEnvelope(input GhostWisprEnvelopeInput) Envelope {
	eventType := normalizeGhostWisprEventType(input.EventType)
	sessionID := ghostWisprTopicSessionID(input.Body)
	kind := ghostWisprKind(eventType)
	topic := GhostWisprSubject(sessionID, kind)
	return Envelope{
		EventID:        input.EventID,
		Source:         "ghostwispr",
		SourceEventID:  input.Delivery,
		Topic:          topic,
		DedupeKey:      "ghostwispr." + input.Delivery,
		IssuedAt:       NowMillis(),
		PayloadSummary: ghostWisprSummary(eventType, input.Body),
		Payload:        ghostWisprPayload(eventType, input.Body),
		TraceID:        input.TraceID,
	}
}

// ghostWisprKind maps Ghost Wispr event types to NATS topic kinds.
func ghostWisprKind(eventType string) string {
	switch normalizeGhostWisprEventType(eventType) {
	case "session_started":
		return "session.started"
	case "session_ended":
		return "session.ended"
	case "summary_ready":
		return "summary.ready"
	default:
		return normalizeGhostWisprEventType(eventType)
	}
}

// ghostWisprSummary builds a concise prose summary of the Ghost Wispr event.
func ghostWisprSummary(eventType string, body map[string]any) string {
	normalizedEventType := normalizeGhostWisprEventType(eventType)
	summary := fmt.Sprintf("ghostwispr %s for session %s", normalizedEventType, ghostWisprSummarySessionID(body))
	if title := nestedString(body, "payload", "title"); title != "" {
		summary += ": " + first(title, 80)
	}
	return OneLineSummary(summary)
}

func ghostWisprPayload(eventType string, body map[string]any) string {
	normalizedEventType := normalizeGhostWisprEventType(eventType)
	data := map[string]string{"event_type": normalizedEventType}
	data["session_id"] = ghostWisprSummarySessionID(body)
	data["title"] = nestedString(body, "payload", "title")
	data["duration"] = nestedNumberString(body, "payload", "duration")
	data["created_at"] = stringValue(body["created_at"])
	if normalizedEventType == "summary_ready" {
		data["status"] = nestedString(body, "payload", "status")
		if summary := nestedString(body, "payload", "summary"); summary != "" {
			capped, truncated := capBody(summary)
			data["summary"] = capped
			if truncated {
				data["body_truncated"] = "true"
			}
		}
		data["summary_preset"] = nestedString(body, "payload", "summary_preset")
		data["timestamp"] = nestedString(body, "payload", "timestamp")
		data["version"] = nestedNumberString(body, "payload", "version")
		data["payload_type"] = nestedString(body, "payload", "type")
	}
	return payloadJSON(data)
}

func ghostWisprSummarySessionID(body map[string]any) string {
	return strings.TrimSpace(nestedString(body, "payload", "session_id"))
}

func ghostWisprTopicSessionID(body map[string]any) string {
	sessionID := ghostWisprSummarySessionID(body)
	if sessionID == "" {
		return "unknown"
	}
	sessionID = ghostWisprTopicSegmentSanitizer.Replace(sessionID)
	sessionID = strings.Trim(sessionID, "_")
	if sessionID == "" {
		return "unknown"
	}
	return sessionID
}

func normalizeGhostWisprEventType(eventType string) string {
	eventType = strings.TrimSpace(strings.ToLower(eventType))
	return strings.ReplaceAll(eventType, ".", "_")
}
