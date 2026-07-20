package contracts

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
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
	item := GithubEnvelope(input)
	if item.Topic == "" {
		// Event should not be routed (e.g. push to non-heads/tags ref).
		return nil
	}
	out := []Envelope{item}
	owner, repo := githubRepo(input.Body)
	if githubCIEvent(input.Event) {
		// CI events (check_run/check_suite) are no longer published raw. They fold
		// into envoy_ci_state via the webhook handler's CIRecorder (see
		// GithubCIObservations) and are re-emitted as a debounced per-commit summary
		// on pr.<n>.ci by the listener's summary loop. See internal/cistore.
		return nil
	}
	if !githubCommentEvent(input.Event) {
		return out
	}
	body := githubCommentBody(input.Event, input.Body)
	if !ContainsMention(body, trigger) {
		return out
	}
	// Publish mention topic with same structure: type.number.mention
	num := githubNumber(input.Event, input.Body)
	base := githubParentKind(input.Event, input.Body)
	// Fanned-out copies carry distinct dedupe keys: the listener dedupes per
	// (dedupe_key, session), so sharing the base key would suppress these copies
	// for any session whose subscriptions also match the base topic.
	if num != "" {
		// notifications.github.owner.repo.pr.7706.mention
		mention := item
		mention.Topic = GithubSubject(owner, repo, base+"."+num+".mention")
		mention.DedupeKey = item.DedupeKey + ".mention"
		out = append(out, mention)
	}
	// Also publish repo-wide mention: notifications.github.owner.repo.mention
	mention := item
	mention.Topic = GithubSubject(owner, repo, "mention")
	mention.DedupeKey = item.DedupeKey + ".mention.repo"
	out = append(out, mention)
	return out
}

// CIObservation is the per-(PR, check) fact the CI summary aggregator needs,
// extracted from a check_run webhook.
type CIObservation struct {
	Owner      string
	Repo       string
	Number     string
	SHA        string
	AppID      string
	CheckName  string
	Status     string
	Conclusion string
}

// GithubCIObservations extracts one CIObservation per associated PR from a
// check_run webhook. It returns nil when the event is not a check_run, has no
// associated PR, or lacks the head SHA / check name needed to summarize.
//
// check_suite is intentionally ignored: it is a per-app rollup with no
// per-check name, so it would add a redundant row next to the check_runs it
// aggregates. The per-check view is built from check_run only. GitHub Actions
// emits per-job check_runs, so nothing is lost for the current CI.
func GithubCIObservations(event string, body map[string]any) []CIObservation {
	if event != "check_run" {
		return nil
	}
	prs := githubCIPullRequests(event, body)
	if len(prs) == 0 {
		return nil
	}
	sha := nestedString(body, "check_run", "head_sha")
	name := nestedString(body, "check_run", "name")
	if sha == "" || name == "" {
		return nil
	}
	owner, repo := githubRepo(body)
	appID := nestedNumberString(body, "check_run", "app", "id")
	status := nestedString(body, "check_run", "status")
	conclusion := nestedString(body, "check_run", "conclusion")
	out := make([]CIObservation, 0, len(prs))
	for _, pr := range prs {
		out = append(out, CIObservation{
			Owner:      owner,
			Repo:       repo,
			Number:     pr,
			SHA:        sha,
			AppID:      appID,
			CheckName:  name,
			Status:     status,
			Conclusion: conclusion,
		})
	}
	return out
}

func GithubIsBotSender(body map[string]any) bool {
	return strings.EqualFold(nestedString(body, "sender", "type"), "Bot")
}

func githubRepo(body map[string]any) (string, string) {
	owner := nestedString(body, "repository", "owner", "login")
	if owner == "" {
		owner = "unknown"
	}
	repo := nestedString(body, "repository", "name")
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
			// Distinct key so the thread copy survives per-(key, session) dedupe
			// when the session is also subscribed to the channel topic.
			threaded.DedupeKey = item.DedupeKey + ".thread"
			out = append(out, threaded)
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

func githubNumber(event string, body map[string]any) string {
	switch event {
	case "pull_request", "pull_request_review", "pull_request_review_comment":
		n := nested(body, "pull_request", "number")
		if n != nil {
			return fmt.Sprintf("%v", n)
		}
	case "issues":
		n := nested(body, "issue", "number")
		if n != nil {
			return fmt.Sprintf("%v", n)
		}
	case "sub_issues":
		n := nested(body, "parent_issue", "number")
		if n != nil {
			return fmt.Sprintf("%v", n)
		}
	case "issue_comment":
		n := nested(body, "issue", "number")
		if n != nil {
			return fmt.Sprintf("%v", n)
		}
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
		n := prMap["number"]
		if n != nil {
			nums = append(nums, fmt.Sprintf("%v", n))
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
	repo := nestedString(body, "repository", "full_name")
	action := stringValue(body["action"])
	num := githubNumber(event, body)

	var data map[string]string

	switch event {
	case "issue_comment":
		data = map[string]string{
			"kind":        "comment",
			"action":      action,
			"repo":        repo,
			"number":      num,
			"title":       nestedString(body, "issue", "title"),
			"parent_kind": githubParentKind(event, body),
			"author":      nestedString(body, "comment", "user", "login"),
			"body":        truncateBody(nestedString(body, "comment", "body"), 500),
			"url":         nestedString(body, "comment", "html_url"),
		}
	case "pull_request_review_comment":
		data = map[string]string{
			"kind":        "comment",
			"action":      action,
			"repo":        repo,
			"number":      num,
			"title":       nestedString(body, "pull_request", "title"),
			"parent_kind": githubParentKind(event, body),
			"author":      nestedString(body, "comment", "user", "login"),
			"body":        truncateBody(nestedString(body, "comment", "body"), 500),
			"url":         nestedString(body, "comment", "html_url"),
		}
	case "pull_request_review":
		data = map[string]string{
			"kind":        "review",
			"action":      action,
			"repo":        repo,
			"number":      num,
			"title":       nestedString(body, "pull_request", "title"),
			"parent_kind": "pr",
			"author":      nestedString(body, "review", "user", "login"),
			"body":        truncateBody(nestedString(body, "review", "body"), 500),
			"url":         nestedString(body, "review", "html_url"),
			"state":       nestedString(body, "review", "state"),
		}
	case "pull_request":
		data = map[string]string{
			"kind":   "pr",
			"action": action,
			"repo":   repo,
			"number": num,
			"title":  nestedString(body, "pull_request", "title"),
			"author": nestedString(body, "pull_request", "user", "login"),
			"body":   truncateBody(nestedString(body, "pull_request", "body"), 500),
			"url":    nestedString(body, "pull_request", "html_url"),
		}
	case "issues":
		data = map[string]string{
			"kind":   "issue",
			"action": action,
			"repo":   repo,
			"number": num,
			"title":  nestedString(body, "issue", "title"),
			"author": nestedString(body, "issue", "user", "login"),
			"body":   truncateBody(nestedString(body, "issue", "body"), 500),
			"url":    nestedString(body, "issue", "html_url"),
		}
	case "push":
		data = map[string]string{
			"kind": "push",
			"repo": repo,
			"ref":  stringValue(body["ref"]),
		}
	case "check_run":
		data = map[string]string{
			"kind":       "ci",
			"action":     action,
			"repo":       repo,
			"number":     num,
			"name":       nestedString(body, "check_run", "name"),
			"status":     nestedString(body, "check_run", "status"),
			"conclusion": nestedString(body, "check_run", "conclusion"),
		}
	case "check_suite":
		data = map[string]string{
			"kind":       "ci",
			"action":     action,
			"repo":       repo,
			"number":     num,
			"status":     nestedString(body, "check_suite", "status"),
			"conclusion": nestedString(body, "check_suite", "conclusion"),
		}
	case "workflow_run":
		data = map[string]string{
			"kind":       "workflow",
			"action":     action,
			"repo":       repo,
			"workflow":   nestedString(body, "workflow_run", "name"),
			"path":       nestedString(body, "workflow_run", "path"),
			"branch":     nestedString(body, "workflow_run", "head_branch"),
			"status":     nestedString(body, "workflow_run", "status"),
			"conclusion": nestedString(body, "workflow_run", "conclusion"),
		}
	default:
		data = map[string]string{
			"kind":   "unknown",
			"action": action,
			"repo":   repo,
		}
	}

	return summaryJSON(data)
}

func githubPayload(event string, body map[string]any) string {
	repo := nestedString(body, "repository", "full_name")
	action := stringValue(body["action"])
	num := githubNumber(event, body)

	var data map[string]string

	switch event {
	case "issue_comment":
		data = map[string]string{
			"kind":        "comment",
			"action":      action,
			"repo":        repo,
			"number":      num,
			"title":       nestedString(body, "issue", "title"),
			"parent_kind": githubParentKind(event, body),
			"author":      nestedString(body, "comment", "user", "login"),
			"body":        nestedString(body, "comment", "body"),
			"url":         nestedString(body, "comment", "html_url"),
		}
	case "pull_request_review_comment":
		data = map[string]string{
			"kind":        "comment",
			"action":      action,
			"repo":        repo,
			"number":      num,
			"title":       nestedString(body, "pull_request", "title"),
			"parent_kind": githubParentKind(event, body),
			"author":      nestedString(body, "comment", "user", "login"),
			"body":        nestedString(body, "comment", "body"),
			"url":         nestedString(body, "comment", "html_url"),
		}
	case "pull_request_review":
		data = map[string]string{
			"kind":        "review",
			"action":      action,
			"repo":        repo,
			"number":      num,
			"title":       nestedString(body, "pull_request", "title"),
			"parent_kind": "pr",
			"author":      nestedString(body, "review", "user", "login"),
			"body":        nestedString(body, "review", "body"),
			"url":         nestedString(body, "review", "html_url"),
			"state":       nestedString(body, "review", "state"),
		}
	case "pull_request":
		data = map[string]string{
			"kind":   "pr",
			"action": action,
			"repo":   repo,
			"number": num,
			"title":  nestedString(body, "pull_request", "title"),
			"author": nestedString(body, "pull_request", "user", "login"),
			"body":   nestedString(body, "pull_request", "body"),
			"url":    nestedString(body, "pull_request", "html_url"),
		}
	case "issues":
		data = map[string]string{
			"kind":   "issue",
			"action": action,
			"repo":   repo,
			"number": num,
			"title":  nestedString(body, "issue", "title"),
			"author": nestedString(body, "issue", "user", "login"),
			"body":   nestedString(body, "issue", "body"),
			"url":    nestedString(body, "issue", "html_url"),
		}
	case "workflow_run":
		data = map[string]string{
			"kind":       "workflow",
			"action":     action,
			"repo":       repo,
			"workflow":   nestedString(body, "workflow_run", "name"),
			"path":       nestedString(body, "workflow_run", "path"),
			"branch":     nestedString(body, "workflow_run", "head_branch"),
			"status":     nestedString(body, "workflow_run", "status"),
			"conclusion": nestedString(body, "workflow_run", "conclusion"),
			"run_id":     nestedNumberString(body, "workflow_run", "id"),
			"url":        nestedString(body, "workflow_run", "html_url"),
		}
	default:
		return ""
	}

	return summaryJSON(data)
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
	data := map[string]string{
		"kind":    slackKind(body),
		"user":    stringValue(event["user"]),
		"channel": stringValue(event["channel"]),
		"text":    stringValue(event["text"]),
	}
	if ts := stringValue(event["ts"]); ts != "" {
		data["ts"] = ts
	}
	if thread := stringValue(event["thread_ts"]); thread != "" {
		data["thread_ts"] = thread
	}
	return summaryJSON(data)
}

func summaryJSON(data map[string]string) string {
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

func truncateBody(s string, maxChars int) string {
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	return string(runes[:maxChars]) + "... [truncated]"
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

// ghostWisprSummary builds a JSON summary of the Ghost Wispr event.
func ghostWisprSummary(eventType string, body map[string]any) string {
	data := map[string]string{
		"event_type": normalizeGhostWisprEventType(eventType),
		"session_id": ghostWisprSummarySessionID(body),
	}
	if title := truncateBody(strings.TrimSpace(nestedString(body, "payload", "title")), 500); title != "" {
		data["title"] = title
	}
	if duration := nested(body, "payload", "duration"); duration != nil {
		data["duration"] = fmt.Sprintf("%v", duration)
	}
	return summaryJSON(data)
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
