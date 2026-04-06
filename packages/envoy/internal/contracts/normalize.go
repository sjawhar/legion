package contracts

import (
	"encoding/json"
	"fmt"
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
	owner := nestedString(input.Body, "repository", "owner", "login")
	if owner == "" {
		owner = "unknown"
	}
	repo := nestedString(input.Body, "repository", "name")
	if repo == "" {
		repo = "unknown"
	}
	topic := githubTopic(owner, repo, input.Event, input.Body)
	return Envelope{
		EventID:        input.EventID,
		Source:         "github",
		SourceEventID:  input.Delivery,
		Topic:          topic,
		DedupeKey:      "github." + input.Delivery,
		IssuedAt:       NowMillis(),
		PayloadSummary: githubSummary(input.Event, input.Body),
		TraceID:        input.TraceID,
	}
}

func GithubEnvelopes(input GithubEnvelopeInput, trigger string) []Envelope {
	item := GithubEnvelope(input)
	out := []Envelope{item}
	if githubCIEvent(input.Event) {
		prs := githubCIPullRequests(input.Event, input.Body)
		if len(prs) > 0 {
			owner := nestedString(input.Body, "repository", "owner", "login")
			if owner == "" {
				owner = "unknown"
			}
			repo := nestedString(input.Body, "repository", "name")
			if repo == "" {
				repo = "unknown"
			}
			out = make([]Envelope, 0, len(prs))
			for _, pr := range prs {
				ci := item
				ci.Topic = GithubSubject(owner, repo, "pr."+pr+".ci")
				ci.DedupeKey = "github." + input.Delivery + ".pr." + pr
				out = append(out, ci)
			}
		}
		return out
	}
	if !githubCommentEvent(input.Event) {
		return out
	}
	body := githubCommentBody(input.Event, input.Body)
	if !ContainsMention(body, trigger) {
		return out
	}
	// Publish mention topic with same structure: type.number.mention
	owner := nestedString(input.Body, "repository", "owner", "login")
	if owner == "" {
		owner = "unknown"
	}
	repo := nestedString(input.Body, "repository", "name")
	if repo == "" {
		repo = "unknown"
	}
	num := githubNumber(input.Event, input.Body)
	base := githubParentKind(input.Event, input.Body)
	if num != "" {
		// notifications.github.owner.repo.pr.7706.mention
		mention := item
		mention.Topic = GithubSubject(owner, repo, base+"."+num+".mention")
		out = append(out, mention)
	}
	// Also publish repo-wide mention: notifications.github.owner.repo.mention
	mention := item
	mention.Topic = GithubSubject(owner, repo, "mention")
	out = append(out, mention)
	return out
}

func GithubIsBotSender(body map[string]any) bool {
	return strings.EqualFold(nestedString(body, "sender", "type"), "Bot")
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
	event, _ := input.Body["event"].(map[string]any)
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
	event, _ := input.Body["event"].(map[string]any)
	thread := stringValue(event["thread_ts"])
	if thread == "" {
		thread = stringValue(event["ts"])
	}
	if thread != "" {
		team := stringValue(input.Body["team_id"])
		channel := stringValue(event["channel"])
		if team != "" && channel != "" {
			threaded := item
			threaded.Topic = SlackSubject(team, channel, "thread."+thread)
			out = append(out, threaded)
		}
	}
	return out
}

// githubTopic builds the full topic path with number hierarchy.
// PR #7706 opened:       pr.7706
// Comment on PR #7706:    pr.7706.comment
// Review on PR #7706:     pr.7706.review
// Issue #42 opened:       issue.42
// Comment on issue #42:   issue.42.comment
// Push event:             push
func githubTopic(owner string, repo string, event string, body map[string]any) string {
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
	// e.g., push, ci
	return GithubSubject(owner, repo, kind)
}

// githubParentKind returns the entity type that owns the number.
// For issue_comment, checks body["issue"]["pull_request"] to distinguish PR vs issue.
func githubParentKind(event string, body map[string]any) string {
	switch event {
	case "pull_request", "pull_request_review", "pull_request_review_comment":
		return "pr"
	case "issues":
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
	case "push":
		return "push"
	case "check_run", "check_suite":
		return "ci"
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
	obj, _ := body[key].(map[string]any)
	if obj == nil {
		return nil
	}
	prs, _ := obj["pull_requests"].([]any)
	var nums []string
	for _, pr := range prs {
		prMap, _ := pr.(map[string]any)
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
	default:
		data = map[string]string{
			"kind":   "unknown",
			"action": action,
			"repo":   repo,
		}
	}

	out, _ := json.Marshal(data)
	return string(out)
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
	event, _ := body["event"].(map[string]any)
	if stringValue(event["type"]) == "app_mention" {
		return "mention"
	}
	return "message"
}

func slackSummary(body map[string]any) string {
	event, _ := body["event"].(map[string]any)
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
	out, _ := json.Marshal(data)
	return string(out)
}

func nested(body map[string]any, keys ...string) any {
	var cur any = body
	for _, key := range keys {
		item, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = item[key]
	}
	return cur
}

func nestedString(body map[string]any, keys ...string) string {
	return stringValue(nested(body, keys...))
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func truncateBody(s string, maxChars int) string {
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	return string(runes[:maxChars])
}
