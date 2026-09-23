package classify

import (
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

type decisionIssue struct {
	Status     string
	Parent     string
	Children   []string
	LastSeq    int64
	HasLastSeq bool
}

type decisionGate struct {
	ArtifactID      string
	LatestVersion   int
	ApprovedVersion *int
}

type decisionTree struct {
	Root   string
	Status string
}

type decisionPR struct {
	record.PullRequest
	HasBlockedAttempts bool
	HeadCounted        bool
}

type decisionState struct {
	Issues        map[string]*decisionIssue
	Gates         map[string]*decisionGate
	Trees         map[string]decisionTree
	Queue         map[string]bool
	PRs           map[string]*decisionPR
	PRByBranch    map[string]string
	PRTombstones  map[string]time.Time
	ProjectRepos  map[string]string
}

type dispatchDecisionEvent struct {
	Key     string
	Type    string
	Seq     int64
	Payload map[string]any
}

type githubDecisionEvent struct {
	Topic     string
	Payload   map[string]any
	UpdatedAt time.Time
}

type reducerDecision struct {
	EffectKinds         []string
	Status              *string
	FixAttemptsSet      bool
	FixAttempts         *int
	BlockedAttemptsSet  bool
	BlockedAttempts     *int
	Gate                *decisionGate
}

func classifyDispatchDecision(state decisionState, event dispatchDecisionEvent) reducerDecision {
	beforeStatus := issueStatus(state.Issues[event.Key])
	beforeGate := cloneGate(state.Gates[event.Key])

	effects := []string{}
	issue := state.Issues[event.Key]
	recognized := issue == nil || !issue.HasLastSeq || event.Seq > issue.LastSeq
	if recognized {
		switch event.Type {
		case "issue.created":
			effects = reduceIssueCreated(&state, event)
		case "issue.updated":
			effects = reduceIssueUpdated(&state, event)
		case "issue.closed":
			effects = reduceIssueClosed(&state, event)
		case "child.status":
			effects = reduceChildStatus(&state, event)
		case "artifact.approved":
			effects = reduceArtifactApproved(&state, event)
		case "artifact.changes_requested":
			effects = reduceArtifactChangesRequested(&state, event)
		case "artifact.version":
			effects = reduceArtifactVersion(&state, event)
		default:
			recognized = false
		}
	}
	if recognized {
		if issue := state.Issues[event.Key]; issue != nil {
			issue.LastSeq = event.Seq
			issue.HasLastSeq = true
		}
	}

	decision := reducerDecision{EffectKinds: effects}
	if afterStatus := issueStatus(state.Issues[event.Key]); afterStatus != beforeStatus {
		decision.Status = new(afterStatus)
	}
	if afterGate := state.Gates[event.Key]; !sameGate(beforeGate, afterGate) {
		decision.Gate = cloneGate(afterGate)
	}
	return decision
}

func classifyGithubDecision(state decisionState, event githubDecisionEvent) reducerDecision {
	payload := event.Payload
	if payload == nil || checksTopic(event.Topic) {
		return reducerDecision{EffectKinds: []string{}}
	}

	prKey := trackedPRKey(state, payload)
	before := cloneDecisionPR(state.PRs[prKey])
	effects := []string{}
	kind := stringField(payload, "kind")
	switch kind {
	case "comment":
		effects = reducePRComment(&state, payload)
	case "review":
		effects = reduceReview(&state, payload)
	case "pr":
		effects = reducePullRequest(&state, event, payload)
	case "push":
		effects = reducePush(&state, payload)
	}

	if prKey == "" {
		prKey = trackedPRKey(state, payload)
	}
	after := state.PRs[prKey]
	decision := reducerDecision{EffectKinds: effects}
	if fixAttemptsDiffer(before, after) {
		decision.FixAttemptsSet = true
		if after != nil {
			decision.FixAttempts = new(after.FixAttempts)
		}
	}
	if blockedAttemptsDiffer(before, after) {
		decision.BlockedAttemptsSet = true
		if after != nil && after.HasBlockedAttempts {
			decision.BlockedAttempts = new(after.BlockedAttempts)
		}
	}
	return decision
}

func reduceIssueCreated(state *decisionState, event dispatchDecisionEvent) []string {
	if state.Issues[event.Key] != nil {
		return []string{}
	}
	status := stringField(event.Payload, "status")
	if status == "" {
		return []string{}
	}
	parent := stringField(event.Payload, "parent")
	issue := &decisionIssue{Status: status, Parent: parent, Children: []string{}}
	state.Issues[event.Key] = issue
	if parent == "" {
		return []string{"controller"}
	}
	parentIssue := state.Issues[parent]
	if parentIssue == nil {
		return []string{}
	}
	if !containsString(parentIssue.Children, event.Key) {
		parentIssue.Children = append(parentIssue.Children, event.Key)
	}
	return state.routeArchitect(parent)
}

func reduceIssueUpdated(state *decisionState, event dispatchDecisionEvent) []string {
	issue := state.Issues[event.Key]
	status := stringField(event.Payload, "status")
	if issue == nil || status == "" {
		return []string{}
	}
	changed := issue.Status != status
	issue.Status = status
	if !changed {
		return []string{}
	}
	if status == "todo" {
		if issue.Parent == "" {
			return []string{"admit"}
		}
		if state.liveAncestorTree(event.Key) {
			return []string{}
		}
		effects := []string{"log", "admit"}
		return effects
	}
	if (status == "backlog" || status == "icebox") && state.Trees[event.Key].Status == "active" {
		return []string{"linger"}
	}
	if staleQueuedStatus(status) && (state.Queue[event.Key] || state.Trees[event.Key].Status == "queued") {
		return []string{"dequeue"}
	}
	return []string{}
}

func reduceIssueClosed(state *decisionState, event dispatchDecisionEvent) []string {
	issue := state.Issues[event.Key]
	status := stringField(event.Payload, "status")
	if issue == nil || status == "" {
		return []string{}
	}
	wasOpen := issue.Status != "done"
	issue.Status = status
	effects := []string{}
	if state.Trees[event.Key].Status == "active" {
		effects = append(effects, "linger")
	} else if staleQueuedStatus(status) && (state.Queue[event.Key] || state.Trees[event.Key].Status == "queued") {
		effects = append(effects, "dequeue")
	}
	if issue.Parent == "" || !wasOpen || state.Issues[issue.Parent] == nil {
		return effects
	}
	effects = append(effects, state.routeArchitect(issue.Parent)...)
	if state.openChildren(issue.Parent) == 0 {
		effects = append(effects, state.routeArchitect(issue.Parent)...)
	}
	return effects
}

func reduceChildStatus(state *decisionState, event dispatchDecisionEvent) []string {
	if state.Issues[event.Key] == nil || stringField(event.Payload, "child_key") == "" ||
		stringField(event.Payload, "from") == "" || stringField(event.Payload, "to") == "" {
		return []string{}
	}
	return state.routeArchitect(event.Key)
}

func reduceArtifactApproved(state *decisionState, event dispatchDecisionEvent) []string {
	gate := state.gateFor(event.Key, stringField(event.Payload, "artifact_id"))
	version, ok := intField(event.Payload, "version")
	if gate == nil || !ok || (gate.ApprovedVersion != nil && *gate.ApprovedVersion == version) {
		return []string{}
	}
	gate.ApprovedVersion = new(version)
	if version > gate.LatestVersion {
		gate.LatestVersion = version
	}
	if gate.ApprovedVersion == nil || *gate.ApprovedVersion != gate.LatestVersion {
		return []string{}
	}
	return state.routeArchitect(event.Key)
}

func reduceArtifactChangesRequested(state *decisionState, event dispatchDecisionEvent) []string {
	gate := state.gateFor(event.Key, stringField(event.Payload, "artifact_id"))
	version, ok := intField(event.Payload, "version")
	if gate == nil || !ok || !hasString(event.Payload, "reason") {
		return []string{}
	}
	gate.ApprovedVersion = nil
	if version > gate.LatestVersion {
		gate.LatestVersion = version
	}
	return state.routeArchitect(event.Key)
}

func reduceArtifactVersion(state *decisionState, event dispatchDecisionEvent) []string {
	gate := state.gateFor(event.Key, stringField(event.Payload, "artifact_id"))
	versionObject, ok := event.Payload["version"].(map[string]any)
	if gate == nil || !ok {
		return []string{}
	}
	version, ok := intField(versionObject, "number")
	if !ok {
		return []string{}
	}
	if version > gate.LatestVersion {
		gate.LatestVersion = version
	}
	return []string{}
}

func reducePRComment(state *decisionState, payload map[string]any) []string {
	repo, number, ok := prIdentity(payload)
	if !ok || stringField(payload, "action") != "created" || stringField(payload, "parent_kind") != "pr" || filtered(payload) {
		return []string{}
	}
	pr := state.PRs[prKey(repo, number)]
	if pr == nil {
		return []string{}
	}
	return state.routeActive(pr.Issue)
}

func reduceReview(state *decisionState, payload map[string]any) []string {
	repo, number, ok := prIdentity(payload)
	if !ok || stringField(payload, "action") != "submitted" {
		return []string{}
	}
	pr := state.PRs[prKey(repo, number)]
	if pr == nil {
		return []string{}
	}
	decision := lowerASCII(stringField(payload, "state"))
	currentHead := hasString(payload, "commit_id") && stringField(payload, "commit_id") == pr.HeadSHA
	prior := pr.ReviewDecision
	if decision == "changes_requested" || (currentHead && decision == "approved") {
		pr.ReviewDecision = decision
	}
	effects := state.routeActive(pr.Issue)
	if currentHead && decision == "approved" && prior != "approved" && pr.Verdict == "green" {
		effects = append(effects, state.routeActive(pr.Issue)...)
	}
	return effects
}

func reducePullRequest(state *decisionState, event githubDecisionEvent, payload map[string]any) []string {
	repo, number, ok := prIdentity(payload)
	if !ok {
		return []string{}
	}
	key := prKey(repo, number)
	branch := stringField(payload, "head_ref")
	sha := stringField(payload, "head_sha")
	action := stringField(payload, "action")
	pr := state.PRs[key]
	if action == "opened" {
		if pr = state.registerPR(repo, number, branch, stringField(payload, "body"), sha, event.UpdatedAt); pr == nil {
			return []string{}
		}
		return state.routeActive(pr.Issue)
	}
	if pr == nil && action == "synchronize" {
		pr = state.registerPR(repo, number, branch, stringField(payload, "body"), sha, event.UpdatedAt)
	}
	if pr == nil {
		return []string{}
	}
	if action == "synchronize" {
		source := "webhook"
		if event.Topic == "resync" {
			source = "resync"
		}
		if SupersededBy(HeadClock{UpdatedAt: event.UpdatedAt, Source: source}, HeadClock{UpdatedAt: pr.HeadUpdatedAt, Source: pr.HeadUpdatedAtSource}) || sha == "" {
			return []string{}
		}
		if pr.HeadSHA == sha {
			if !event.UpdatedAt.IsZero() && (pr.HeadUpdatedAt.IsZero() || event.UpdatedAt.After(pr.HeadUpdatedAt)) {
				pr.HeadUpdatedAt = event.UpdatedAt
				pr.HeadUpdatedAtSource = source
			}
			return []string{}
		}
		resetPRHead(pr, sha)
		pr.HeadUpdatedAt = event.UpdatedAt
		pr.HeadUpdatedAtSource = source
		return []string{}
	}
	if action != "closed" {
		return []string{}
	}
	issue := pr.Issue
	delete(state.PRs, key)
	for branchKey, mapped := range state.PRByBranch {
		if mapped == key {
			delete(state.PRByBranch, branchKey)
		}
	}
	return state.routeActive(issue)
}

func reducePush(state *decisionState, payload map[string]any) []string {
	repo := stringField(payload, "repo")
	ref := stringField(payload, "ref")
	after := stringField(payload, "after")
	if repo == "" || !hasPrefix(ref, "refs/heads/") || after == "" {
		return []string{}
	}
	key := state.PRByBranch[repo+"@"+ref[len("refs/heads/"):]]
	pr := state.PRs[key]
	if pr == nil {
		return []string{}
	}
	classification := ClassifyPush(PushPayload{ChangedPaths: optionalString(payload, "changed_paths"), ChangedPathsTruncated: optionalString(payload, "changed_paths_truncated")})
	counted := false
	if pr.HeadSHA == after {
		counted = pr.HeadCounted
	} else {
		counted = pr.Verdict == "red"
	}
	if pr.HeadSHA == after {
		if classification.HandoffOnly && pr.HeadCounted {
			if pr.HasBlockedAttempts && pr.BlockedAttempts == pr.FixAttempts {
				pr.HasBlockedAttempts = false
			}
			pr.FixAttempts--
			pr.HeadCounted = false
		}
	} else {
		pr.PendingPush = &record.PendingPush{SHA: after, HandoffOnly: classification.HandoffOnly, Unknown: classification.Unknown}
	}
	if classification.HandoffOnly || classification.Unknown == "" || !counted {
		return []string{}
	}
	return []string{"log"}
}

func resetPRHead(pr *decisionPR, headSHA string) {
	handoffOnly := pr.PendingPush != nil && pr.PendingPush.SHA == headSHA && pr.PendingPush.HandoffOnly
	if pr.PendingPush != nil && pr.PendingPush.SHA == headSHA {
		pr.PendingPush = nil
	}
	if pr.Verdict == "red" && !handoffOnly {
		pr.FixAttempts++
		pr.HeadCounted = true
	} else {
		pr.HeadCounted = false
	}
	pr.HeadSHA = headSHA
	pr.Verdict = ""
	pr.Failing = []string{}
	pr.FailingStatuses = []string{}
	pr.CheckRuns = nil
	pr.Generation = 0
	pr.Snapshot = ""
	pr.Reconciled = false
	pr.ReviewDecision = ""
}

func (state *decisionState) registerPR(repo string, number int, branch, body, sha string, updatedAt time.Time) *decisionPR {
	key := prKey(repo, number)
	if existing := state.PRs[key]; existing != nil && !updatedAt.IsZero() && !existing.HeadUpdatedAt.IsZero() && !updatedAt.After(existing.HeadUpdatedAt) {
		return nil
	}
	if tombstone, found := state.PRTombstones[key]; found && !updatedAt.IsZero() && !tombstone.IsZero() && !updatedAt.After(tombstone) {
		return nil
	}
	issue := issueForBranch(branch)
	if issue == "" {
		issue = issueForPRBody(body)
	}
	if issue == "" || state.Issues[issue] == nil || state.ProjectRepos[projectForIssue(issue)] != repo || sha == "" {
		return nil
	}
	pr := &decisionPR{PullRequest: record.PullRequest{
		Issue: issue, Repo: repo, Number: number, Branch: branch, HeadSHA: sha, HeadUpdatedAt: updatedAt,
		HeadUpdatedAtSource: "webhook", Failing: []string{}, FailingStatuses: []string{}, CheckRuns: nil,
	}}
	state.PRs[key] = pr
	if branch != "" {
		state.PRByBranch[repo+"@"+branch] = key
	}
	delete(state.PRTombstones, key)
	return pr
}

func (state *decisionState) gateFor(issue, artifactID string) *decisionGate {
	if state.Issues[issue] == nil || artifactID == "" {
		return nil
	}
	gate := state.Gates[issue]
	if gate == nil || gate.ArtifactID != artifactID {
		return nil
	}
	return gate
}

func (state decisionState) routeActive(issue string) []string {
	tree := state.treeFor(issue)
	if tree == nil {
		return []string{}
	}
	if tree.Status == "closed" {
		return []string{"controller"}
	}
	return []string{"publish"}
}

func (state decisionState) routeArchitect(issue string) []string {
	return state.routeActive(issue)
}

func (state decisionState) treeFor(issue string) *decisionTree {
	seen := map[string]bool{}
	current := issue
	for current != "" && !seen[current] {
		seen[current] = true
		if tree, found := state.Trees[current]; found {
			return &tree
		}
		node := state.Issues[current]
		if node == nil {
			return nil
		}
		current = node.Parent
	}
	return nil
}

func (state decisionState) liveAncestorTree(issue string) bool {
	seen := map[string]bool{issue: true}
	current := ""
	if node := state.Issues[issue]; node != nil {
		current = node.Parent
	}
	for current != "" && !seen[current] {
		seen[current] = true
		if tree, found := state.Trees[current]; found {
			return tree.Status != "lingering" && tree.Status != "closed"
		}
		node := state.Issues[current]
		if node == nil {
			return false
		}
		current = node.Parent
	}
	return false
}

func (state decisionState) openChildren(parent string) int {
	node := state.Issues[parent]
	if node == nil {
		return 0
	}
	open := 0
	for _, child := range node.Children {
		if state.Issues[child] == nil || state.Issues[child].Status != "done" {
			open++
		}
	}
	return open
}

func trackedPRKey(state decisionState, payload map[string]any) string {
	repo, number, ok := prIdentity(payload)
	if ok {
		return prKey(repo, number)
	}
	ref := stringField(payload, "ref")
	if repo == "" || !hasPrefix(ref, "refs/heads/") {
		return ""
	}
	return state.PRByBranch[repo+"@"+ref[len("refs/heads/"):]]
}

func prIdentity(payload map[string]any) (string, int, bool) {
	repo := stringField(payload, "repo")
	number, ok := intField(payload, "number")
	return repo, number, repo != "" && ok
}

func prKey(repo string, number int) string {
	return repo + "#" + decimal(number)
}

func projectForIssue(issue string) string {
	for index := range issue {
		if issue[index] == '-' {
			return issue[:index]
		}
	}
	return ""
}

func issueForBranch(branch string) string {
	if !hasPrefix(branch, "legion/") {
		return ""
	}
	return validIssueKey(branch[len("legion/"):])
}

func issueForPRBody(body string) string {
	const prefix = "Dispatch: "
	for offset := range len(body) - len(prefix) + 1 {
		if body[offset:offset+len(prefix)] != prefix {
			continue
		}
		end := offset + len(prefix)
		for end < len(body) && body[end] != '\n' && body[end] != '\r' {
			end++
		}
		if key := validIssueKey(body[offset+len(prefix) : end]); key != "" {
			return key
		}
	}
	return ""
}

func validIssueKey(value string) string {
	dash := -1
	for index := range value {
		character := value[index]
		if character == '-' {
			if dash != -1 || index == 0 || index == len(value)-1 {
				return ""
			}
			dash = index
			continue
		}
		if dash == -1 {
			if character < 'A' || character > 'Z' && (character < '0' || character > '9') {
				return ""
			}
		} else if character < '0' || character > '9' {
			return ""
		}
	}
	if dash == -1 {
		return ""
	}
	return value
}

func checksTopic(topic string) bool {
	const suffix = ".checks"
	return len(topic) >= len(suffix) && topic[len(topic)-len(suffix):] == suffix
}

func filtered(payload map[string]any) bool {
	return stringField(payload, "legion_footer") == "true" || containsSubstring(stringField(payload, "body"), "<!-- legion:")
}

func staleQueuedStatus(status string) bool {
	return status == "triage" || status == "icebox" || status == "backlog" || status == "done"
}

func stringField(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}

func hasString(values map[string]any, key string) bool {
	_, found := values[key].(string)
	return found
}

func optionalString(values map[string]any, key string) *string {
	value, found := values[key].(string)
	if !found {
		return nil
	}
	return new(value)
}

func intField(values map[string]any, key string) (int, bool) {
	value, found := values[key]
	if !found {
		return 0, false
	}
	switch number := value.(type) {
	case float64:
		if number != float64(int(number)) {
			return 0, false
		}
		return int(number), true
	case string:
		return decimalValue(number)
	default:
		return 0, false
	}
}

func decimalValue(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	result := 0
	for index := range value {
		character := value[index]
		if character < '0' || character > '9' {
			return 0, false
		}
		result = result*10 + int(character-'0')
	}
	return result, true
}

func decimal(value int) string {
	if value == 0 {
		return "0"
	}
	encoded := [20]byte{}
	index := len(encoded)
	for value > 0 {
		index--
		encoded[index] = byte('0' + value%10)
		value /= 10
	}
	return string(encoded[index:])
}

func lowerASCII(value string) string {
	encoded := []byte(value)
	for index := range encoded {
		if encoded[index] >= 'A' && encoded[index] <= 'Z' {
			encoded[index] += 'a' - 'A'
		}
	}
	return string(encoded)
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsSubstring(value, wanted string) bool {
	if wanted == "" {
		return true
	}
	for index := range len(value) - len(wanted) + 1 {
		if value[index:index+len(wanted)] == wanted {
			return true
		}
	}
	return false
}

func hasPrefix(value, prefix string) bool {
	return len(value) >= len(prefix) && value[:len(prefix)] == prefix
}

func issueStatus(issue *decisionIssue) string {
	if issue == nil {
		return ""
	}
	return issue.Status
}

func cloneGate(gate *decisionGate) *decisionGate {
	if gate == nil {
		return nil
	}
	copy := *gate
	if gate.ApprovedVersion != nil {
		copy.ApprovedVersion = new(*gate.ApprovedVersion)
	}
	return new(copy)
}

func sameGate(left, right *decisionGate) bool {
	if left == nil || right == nil {
		return left == right
	}
	if left.ArtifactID != right.ArtifactID || left.LatestVersion != right.LatestVersion {
		return false
	}
	if left.ApprovedVersion == nil || right.ApprovedVersion == nil {
		return left.ApprovedVersion == nil && right.ApprovedVersion == nil
	}
	return *left.ApprovedVersion == *right.ApprovedVersion
}

func cloneDecisionPR(pr *decisionPR) *decisionPR {
	if pr == nil {
		return nil
	}
	copy := *pr
	return new(copy)
}

func fixAttemptsDiffer(before, after *decisionPR) bool {
	if before == nil || after == nil {
		return before != after
	}
	return before.FixAttempts != after.FixAttempts
}

func blockedAttemptsDiffer(before, after *decisionPR) bool {
	beforeSet := before != nil && before.HasBlockedAttempts
	afterSet := after != nil && after.HasBlockedAttempts
	if beforeSet != afterSet {
		return true
	}
	return beforeSet && before.BlockedAttempts != after.BlockedAttempts
}
