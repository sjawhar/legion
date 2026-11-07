package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/lookpath"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// DesignGate is the word `gates.design` took before it took a list of steps, and which it still
// accepts in place of one: `root-issues` for the single step spec, `off` for no step at all.
type DesignGate string

const (
	DesignGateRootIssues DesignGate = "root-issues"
	DesignGateOff        DesignGate = "off"
)

// Valid says whether gate is one of the two words. readDesignSteps is its only reader: the words
// are an input spelling of a step list, and nothing downstream asks the daemon for a word —
// `legion controller start` reads the deployment's steps (designGateSteps).
func (gate DesignGate) Valid() bool {
	return gate == DesignGateRootIssues || gate == DesignGateOff
}

// Steps is the design-gate steps the word resolves to: no step under `off`, and the single step
// spec under `root-issues` and under an unset word, which is what the gate held before a
// deployment could name a list.
func (gate DesignGate) Steps() []record.DesignStep {
	if gate == DesignGateOff {
		return []record.DesignStep{}
	}
	return []record.DesignStep{record.DesignStepSpec}
}

// Gates contains the workflow gates the daemon enforces.
type Gates struct {
	// Design is the design-gate steps the deployment gates, in the order `gates.design` names
	// them, which is the order a root's architect takes them in. A nil list is the unset field
	// and resolves to the single step spec, as the gate held before a deployment could name a
	// list; a non-nil empty list is `off` and gates no step.
	Design []record.DesignStep
}

// Steps is the design-gate steps the deployment gates, in the order it named them. The daemon
// keeps one gate record for each step, and every reader of the gate asks this list
// (classify.RequiredSteps). A nil list is the unset field, which gates the single step spec as
// the gate did before a deployment could name a list, so a Config built without its Gates field
// gates rather than waves through; a non-nil empty list is `off` and gates no step.
func (g Gates) Steps() []record.DesignStep {
	if g.Design == nil {
		return DesignGateRootIssues.Steps()
	}
	return g.Design
}

// Project is a Dispatch project prefix's configuration: its optional merge-queue role, merge rules,
// the Slack channels whose @-mentions reach its controller, and whether a merge that closes one of
// its issues opens a production check at all. Its repositories are Dispatch's mapping (Settings →
// Repositories → Projects), which the daemon reads at boot and when it changes, never this file.
type Project struct {
	MergeQueueRole string
	Merge          MergeRules
	// SlackChannels are `slack_channels`, the Slack channel ids whose app mentions the project's
	// controller subscribes to, in the file's order; nil when the file names none.
	SlackChannels []string
	// CommitIssueTrailerRepos are `commit_issue_trailer_repos`, the repositories whose commits a
	// pane stamps with the issue key, in the file's order; nil when the file names none. The
	// project's repositories are Dispatch's mapping (Settings → Repositories → Projects), and this
	// key only selects among them, so a name the project does not serve stamps nothing.
	CommitIssueTrailerRepos []ghrepo.Repository
	// Memory is `memory`, the project's per-tree memory setting; its zero value is off, which a
	// file that names no block leaves.
	Memory Memory
	// ProductionCheck is `production_check`, the role that works the production check a merge
	// closing one of the project's issues opens: the tester, which a project that names no key
	// gets, or the empty role, which is the file's `off` and opens no check at all. The key
	// decides the checks opened after a load, never one already open: a check running under the
	// role the file named when it opened keeps that role, and moving the key to off ends no
	// check that is already open.
	ProductionCheck claim.Role
}

// ProductionCheckOff is the one word of `production_check` that takes the check away, which the
// loader reads as the empty role (readProject).
const ProductionCheckOff = "off"

// ProductionCheckWord is the setting as a record or a reader of it sees it: the role that works
// the project's production checks, and ProductionCheckOff for the project that opens none, never
// the empty string the role takes for off, as Memory.Word gives back a word for the absent mode.
func (p Project) ProductionCheckWord() string {
	if p.ProductionCheck == "" {
		return ProductionCheckOff
	}
	return string(p.ProductionCheck)
}

// MergeRules is the project's `merge` policy. A path matching ByHand is never auto-armed.
type MergeRules struct {
	ByHand []PathPattern
}

// PathPattern is one validated regular expression from a project's merge policy.
type PathPattern struct {
	Expression string
	Regexp     *regexp.Regexp
}

// MatchByHand reports whether any changed path matches a by-hand rule.
func (rules MergeRules) MatchByHand(paths []string) bool {
	return matchesPathPattern(rules.ByHand, paths)
}

func matchesPathPattern(patterns []PathPattern, paths []string) bool {
	for _, pattern := range patterns {
		for _, path := range paths {
			if pattern.Regexp.MatchString(path) {
				return true
			}
		}
	}
	return false
}

// GitHubApp is one App's configuration. Load resolves a configured private-key source into
// PrivateKey; LoadForValidation preserves command and secret sources without executing them.
// TokenCommand is the fourth source, for a deployment whose App keys only a secrets broker holds:
// the daemon holds no key and runs the command for every lease it needs (appauth's token mode), so
// neither loader runs it.
type GitHubApp struct {
	AppID             string
	PrivateKey        string
	PrivateKeyCommand string
	PrivateKeySecret  string
	TokenCommand      string
	Installations     map[string]string
}

// GitHubApps holds the two App identities Legion needs: the implement App acts for the implementer
// and the merger, the review App for every other role, so a review never comes from the account
// that wrote the change.
type GitHubApps struct {
	Implement GitHubApp
	Review    GitHubApp
}

// missingDispatchTokenFile is the one refusal of a Dispatch URL without its bearer, in the daemon's
// loader and the controller's.
const missingDispatchTokenFile = "dispatch_token_file is required when dispatch_url is configured"

var (
	projectKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]*$`)
	roleNamePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// slackChannelPattern is a Slack channel id: C for a public channel, G for a private one, then
	// capital letters and digits. It is one NATS subject token of the mention topic the controller
	// subscribes to (notifications.slack.<team>.<channel>.mention), so nothing else is taken.
	slackChannelPattern = regexp.MustCompile(`^[CG][A-Z0-9]+$`)
)

// readProjects is `projects`, and the warnings its projects raise (Config.Warnings), in file
// order: a key a project still sets that the daemon no longer reads is warned about, never
// refused.
func readProjects(value *yaml.Node, key string) (map[string]Project, []string, error) {
	if value.Tag == "!!null" || value.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("%s must be a mapping", key)
	}
	projects := make(map[string]Project, len(value.Content)/2)
	var warnings []string
	// channelOwner is the project whose slack_channels named a channel first, in file order.
	channelOwner := map[string]string{}
	for index := 0; index+1 < len(value.Content); index += 2 {
		keyNode, entryNode := value.Content[index], value.Content[index+1]
		name := keyNode.Value
		if keyNode.Kind != yaml.ScalarNode || !projectKeyPattern.MatchString(name) {
			return nil, nil, fmt.Errorf(`%s key %q must match ^[A-Z][A-Z0-9]*$`, key, name)
		}
		if _, exists := projects[name]; exists {
			return nil, nil, fmt.Errorf("%s names %s twice", key, name)
		}
		project, raised, err := readProject(entryNode, key+"."+name)
		if err != nil {
			return nil, nil, err
		}
		warnings = append(warnings, raised...)
		for _, channel := range project.SlackChannels {
			if owner, taken := channelOwner[channel]; taken {
				return nil, nil, fmt.Errorf("%s.%s.slack_channels names %q, which %s.%s.slack_channels already names: a channel's mentions reach one project's controller",
					key, name, channel, key, owner)
			}
			channelOwner[channel] = name
		}
		projects[name] = project
	}
	if len(projects) == 0 {
		return nil, nil, errors.New("projects must declare at least one project")
	}
	return projects, warnings, nil
}

// readProject is one `projects.<KEY>` entry and the warnings it raises. `deploys` is among its
// known keys and nothing is read from it: INFRA's own deployment file still sets the key, and a
// reader that refused it would make this release that deployment's outage, on a schedule this
// repository does not control
// (docs/solutions/envoy/a-setting-another-repositorys-deployment-sets-is-removed-in-two-releases-pr2674.md).
// A project that names it is warned about instead.
func readProject(value *yaml.Node, key string) (Project, []string, error) {
	if value.Kind != yaml.MappingNode {
		return Project{}, nil, fmt.Errorf("%s must be a mapping", key)
	}
	fields, err := members(value, key, "commit_issue_trailer_repos", "deploys", "memory", "merge", "merge_queue_role", "mergeQueueRole", "production_check", "slack_channels")
	var unknown unknownKeyError
	if errors.As(err, &unknown) {
		if unknown.name == "repo" {
			return Project{}, nil, fmt.Errorf(`Unknown key %q: the project's repositories are Dispatch's mapping (Settings → Repositories → Projects)`, key+".repo")
		}
		// The shipped loader's own words (validateProjectEntry, config.ts).
		return Project{}, nil, fmt.Errorf(`Unknown key %q`, key+"."+unknown.name)
	}
	if err != nil {
		return Project{}, nil, err
	}
	// The mapping itself is scanned for `deploys` rather than fields, which drops a null member:
	// `deploys:` with nothing under it is a key the deployment still writes, so it warns too.
	// members has already refused the key named twice, so this is at most one line.
	var warnings []string
	for index := 0; index+1 < len(value.Content); index += 2 {
		if value.Content[index].Value == "deploys" {
			warnings = []string{fmt.Sprintf("%s.deploys is set and ignored: the daemon no longer wakes a production check from a deploy run, so the workflows this key names configure nothing; remove it from the deployment's legion.yaml", key)}
			break
		}
	}
	var project Project
	role := fields["merge_queue_role"]
	if role == nil {
		role = fields["mergeQueueRole"]
	}
	if project.MergeQueueRole, err = stringOf(role, key+".merge_queue_role"); err != nil {
		return Project{}, nil, err
	}
	if project.MergeQueueRole != "" && !roleNamePattern.MatchString(project.MergeQueueRole) {
		return Project{}, nil, fmt.Errorf("%s.merge_queue_role is a bare role name (no notifications.role. prefix)", key)
	}
	if merge := fields["merge"]; merge != nil {
		if project.Merge, err = readMergeRules(merge, key+".merge"); err != nil {
			return Project{}, nil, err
		}
	}
	if channels := fields["slack_channels"]; channels != nil {
		if project.SlackChannels, err = readSlackChannels(channels, key+".slack_channels"); err != nil {
			return Project{}, nil, err
		}
	}
	if repos := fields["commit_issue_trailer_repos"]; repos != nil {
		if project.CommitIssueTrailerRepos, err = readCommitIssueTrailerRepos(repos, key+".commit_issue_trailer_repos"); err != nil {
			return Project{}, nil, err
		}
	}
	if memory := fields["memory"]; memory != nil {
		if project.Memory, err = readMemory(memory, key+".memory"); err != nil {
			return Project{}, nil, err
		}
	}
	// production_check is read by the key's presence rather than by its value: a project that
	// names no key gets the tester, and the one word that takes the check away, `off`, is the
	// empty role. A bare `off` reaches the reader as the string, as `memory.mode`'s does.
	project.ProductionCheck = claim.RoleTester
	if check := fields["production_check"]; check != nil {
		word, err := stringOf(check, key+".production_check")
		if err != nil {
			return Project{}, nil, err
		}
		switch word {
		case ProductionCheckOff:
			project.ProductionCheck = ""
		case string(claim.RoleTester):
			project.ProductionCheck = claim.RoleTester
		default:
			return Project{}, nil, fmt.Errorf("%s.production_check must be %s or %s (got %q)", key, ProductionCheckOff, claim.RoleTester, word)
		}
	}
	return project, warnings, nil
}

// readSlackChannels is a project's `slack_channels`: Slack channel ids, each named once. An empty
// list is no channel, as an absent key is.
func readSlackChannels(value *yaml.Node, key string) ([]string, error) {
	channels, err := readStrings(value, key)
	if err != nil || len(channels) == 0 {
		return nil, err
	}
	for index, channel := range channels {
		if !slackChannelPattern.MatchString(channel) {
			return nil, fmt.Errorf("%s entry %q is not a Slack channel id (C or G, then capital letters and digits, such as C0123ABCD)", key, channel)
		}
		if slices.Contains(channels[:index], channel) {
			return nil, fmt.Errorf("%s names %q twice", key, channel)
		}
	}
	return channels, nil
}

// readCommitIssueTrailerRepos is a project's `commit_issue_trailer_repos`: a list of strings, each
// exactly a repository Parse takes, whose commits a pane stamps with the issue key. One repository
// is named once, compared as GitHub compares names, without case: a second entry stamps the same
// commits the first does, so it is the file saying one thing twice rather than a second rule. An
// empty list is no repository, as an absent key is.
func readCommitIssueTrailerRepos(value *yaml.Node, key string) ([]ghrepo.Repository, error) {
	names, err := readStrings(value, key)
	if err != nil || len(names) == 0 {
		return nil, err
	}
	repos := make([]ghrepo.Repository, 0, len(names))
	for index, name := range names {
		repo, err := ghrepo.Parse(fmt.Sprintf("%s[%d]", key, index), name)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(repos, func(named ghrepo.Repository) bool {
			return strings.EqualFold(named.String(), repo.String())
		}) {
			return nil, fmt.Errorf("%s names %s twice", key, repo)
		}
		repos = append(repos, repo)
	}
	return repos, nil
}

func readMergeRules(value *yaml.Node, key string) (MergeRules, error) {
	if value.Kind != yaml.MappingNode {
		return MergeRules{}, fmt.Errorf("%s must be a mapping", key)
	}
	fields, err := members(value, key, "by_hand")
	if err != nil {
		return MergeRules{}, err
	}
	var rules MergeRules
	if byHand := fields["by_hand"]; byHand != nil {
		if rules.ByHand, err = readPathPatterns(byHand, key+".by_hand"); err != nil {
			return MergeRules{}, err
		}
	}
	return rules, nil
}

func readPathPatterns(value *yaml.Node, key string) ([]PathPattern, error) {
	expressions, err := readStrings(value, key)
	if err != nil || len(expressions) == 0 {
		return nil, err
	}
	patterns := make([]PathPattern, 0, len(expressions))
	for _, expression := range expressions {
		if slices.Contains(expressions[:len(patterns)], expression) {
			return nil, fmt.Errorf("%s names %q twice", key, expression)
		}
		compiled, err := regexp.Compile(expression)
		if err != nil {
			return nil, fmt.Errorf("%s entry %q is not a valid regular expression: %w", key, expression, err)
		}
		patterns = append(patterns, PathPattern{Expression: expression, Regexp: compiled})
	}
	return patterns, nil
}

func readGates(value *yaml.Node, key string) (*Gates, error) {
	if value.Tag == "!!null" || value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping", key)
	}
	for index := 0; index+1 < len(value.Content); index += 2 {
		// A present merge is refused whatever its value, null included, as the shipped loader
		// refuses it (parseGates, config.ts).
		if value.Content[index].Value == "merge" {
			return nil, errors.New(gatesMergeMessage)
		}
	}
	fields, err := members(value, key, "design")
	if err != nil {
		return nil, err
	}
	gates := Gates{Design: DesignGateRootIssues.Steps()}
	if design := fields["design"]; design != nil {
		steps, err := readDesignSteps(design, key+".design")
		if err != nil {
			return nil, err
		}
		gates.Design = steps
	}
	return &gates, nil
}

// readDesignSteps is `gates.design`: the steps the deployment gates, in the order it names them.
// A list of step names is that list, and the empty list gates no step. The two words the key
// took before it took a list are read as the lists they mean: `root-issues` is the single step
// spec, `off` no step. An absent key is the single step spec, which readGates applies, and so
// are the two spellings of a key that names nothing, `design:` with nothing after it and
// `design: ""`, which the loader read as the unset key before the key took a list.
//
// A step named twice is refused rather than collapsed, as a repeated merge rule is
// (readPathPatterns): one gate record is kept for each step, so the second name is either a
// mistake or an order the daemon cannot keep.
func readDesignSteps(value *yaml.Node, key string) ([]record.DesignStep, error) {
	names := record.StepWords()
	refusal := fmt.Errorf("%s must be a list of %s, or 'root-issues' for the single step 'spec', or 'off' for no step", key, names)
	switch {
	case value.Tag == "!!null", value.Kind == yaml.ScalarNode && value.Tag == "!!str" && value.Value == "":
		return DesignGateRootIssues.Steps(), nil
	case value.Kind == yaml.ScalarNode && value.Tag == "!!str":
		word := DesignGate(value.Value)
		if !word.Valid() {
			return nil, refusal
		}
		return word.Steps(), nil
	case value.Kind != yaml.SequenceNode:
		return nil, refusal
	}
	steps := make([]record.DesignStep, 0, len(value.Content))
	for index, entry := range value.Content {
		if entry.Kind != yaml.ScalarNode || entry.Tag != "!!str" {
			return nil, fmt.Errorf("%s[%d] must be %s", key, index, names)
		}
		step := record.DesignStep(entry.Value)
		if !step.Valid() {
			return nil, fmt.Errorf("%s[%d] must be %s", key, index, names)
		}
		if slices.Contains(steps, step) {
			return nil, fmt.Errorf("%s names '%s' twice", key, step)
		}
		steps = append(steps, step)
	}
	return steps, nil
}

func readGitHubApps(value *yaml.Node, key string) (*GitHubApps, error) {
	if value.Tag == "!!null" || value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping", key)
	}
	roles, err := members(value, key, "implement", "review")
	if err != nil {
		return nil, err
	}
	for _, role := range []string{"implement", "review"} {
		if roles[role] == nil {
			return nil, fmt.Errorf("%s.%s is required", key, role)
		}
	}
	implement, err := readGitHubApp(roles["implement"], key+".implement")
	if err != nil {
		return nil, err
	}
	review, err := readGitHubApp(roles["review"], key+".review")
	if err != nil {
		return nil, err
	}
	return &GitHubApps{Implement: implement, Review: review}, nil
}

func readGitHubApp(value *yaml.Node, key string) (GitHubApp, error) {
	if value.Kind != yaml.MappingNode {
		return GitHubApp{}, fmt.Errorf("%s must be a mapping", key)
	}
	fields, err := members(value, key, "app_id", "private_key", "private_key_command", "private_key_secret", "token_command", "installations")
	if err != nil {
		return GitHubApp{}, err
	}
	var app GitHubApp
	for _, field := range []struct {
		name   string
		target *string
	}{
		{"app_id", &app.AppID}, {"private_key", &app.PrivateKey},
		{"private_key_command", &app.PrivateKeyCommand}, {"private_key_secret", &app.PrivateKeySecret},
		{"token_command", &app.TokenCommand},
	} {
		if *field.target, err = stringOf(fields[field.name], key+"."+field.name); err != nil {
			return GitHubApp{}, err
		}
	}
	if strings.ContainsAny(app.PrivateKeySecret, " \t\n\r") {
		return GitHubApp{}, fmt.Errorf("%s.private_key_secret must be a single secretsd key name (no whitespace)", key)
	}
	if fields["installations"] != nil {
		if app.Installations, err = readInstallations(fields["installations"], key+".installations"); err != nil {
			return GitHubApp{}, err
		}
	}
	if strings.TrimSpace(app.AppID) == "" {
		return GitHubApp{}, fmt.Errorf("%s is missing required fields: app_id", key)
	}
	sources := 0
	for _, source := range []string{app.PrivateKey, app.PrivateKeyCommand, app.PrivateKeySecret, app.TokenCommand} {
		if source != "" {
			sources++
		}
	}
	if sources != 1 {
		return GitHubApp{}, fmt.Errorf("%s requires exactly one of %s", key, appSources)
	}
	if app.TokenCommand != "" && len(app.Installations) > 0 {
		return GitHubApp{}, fmt.Errorf("%s.installations cannot be set with token_command: the command's release names each installation", key)
	}
	if app.Installations == nil {
		app.Installations = map[string]string{}
	}
	return app, nil
}

// appSources names the four sources of an App's credential, of which an App configures exactly one.
const appSources = "private_key, private_key_command, private_key_secret, or token_command"

func readInstallations(value *yaml.Node, key string) (map[string]string, error) {
	if value.Tag == "!!null" || value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping of strings", key)
	}
	installations := make(map[string]string, len(value.Content)/2)
	for index := 0; index+1 < len(value.Content); index += 2 {
		owner, value := value.Content[index].Value, value.Content[index+1]
		read, err := readString(value, key+"."+owner)
		if err != nil {
			return nil, fmt.Errorf("%s must be a mapping of strings", key)
		}
		if read == nil {
			return nil, fmt.Errorf("%s must be a mapping of strings", key)
		}
		installations[owner] = *read
	}
	return installations, nil
}

func readPositiveInteger(value *yaml.Node, key string, max int) (*int, error) {
	read, err := readInt(value, key)
	if err != nil || read == nil {
		return read, err
	}
	if *read <= 0 {
		return nil, fmt.Errorf("%s must be a positive integer", key)
	}
	if max > 0 && *read > max {
		return nil, fmt.Errorf("%s must be at most %d", key, max)
	}
	return read, nil
}

// ResolveGitHubApps obtains App private keys only after Config has accepted every local setting.
// private_key_command runs with the daemon environment; private_key_secret deliberately drops only
// SECRETSD_SESSION_TOKEN_FILE so a key grant remains attached to the daemon's launcher terminal.
// An App on token_command has no key to resolve: the token manager runs its command per lease.
func ResolveGitHubApps(apps GitHubApps) (GitHubApps, error) {
	implement, err := resolveGitHubApp(apps.Implement, "github_apps.implement")
	if err != nil {
		return GitHubApps{}, err
	}
	review, err := resolveGitHubApp(apps.Review, "github_apps.review")
	if err != nil {
		return GitHubApps{}, err
	}
	return GitHubApps{Implement: implement, Review: review}, nil
}

func resolveGitHubApp(app GitHubApp, field string) (GitHubApp, error) {
	switch {
	case app.PrivateKey != "", app.TokenCommand != "":
		return app, nil
	case app.PrivateKeyCommand != "":
		key, err := executePrivateKeyCommand(app.PrivateKeyCommand, field+".private_key_command")
		if err != nil {
			return GitHubApp{}, err
		}
		app.PrivateKey = key
		return app, nil
	case app.PrivateKeySecret != "":
		key, err := resolvePrivateKeySecret(app.PrivateKeySecret, field+".private_key_secret")
		if err != nil {
			return GitHubApp{}, err
		}
		app.PrivateKey = key
		return app, nil
	default:
		return GitHubApp{}, fmt.Errorf("%s requires exactly one of %s", field, appSources)
	}
}

// executePrivateKeyCommand runs command and returns its trimmed stdout. Its stderr goes to the
// daemon's own stderr as it is written, so an operator sees what `agent-secrets` prints while it
// waits for a person's grant (the request's id), and is kept for the failure's message.
func executePrivateKeyCommand(command, field string) (string, error) {
	keyCommand := exec.Command("sh", "-c", command)
	keyCommand.Env = runtime.WithoutNATSSeeds(os.Environ())
	var stderr strings.Builder
	keyCommand.Stderr = io.MultiWriter(os.Stderr, &stderr)
	result, err := keyCommand.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			exit.Stderr = []byte(stderr.String())
		}
		return "", commandError(field, err)
	}
	key := strings.TrimSpace(string(result))
	if key == "" {
		return "", fmt.Errorf("%s produced empty output", field)
	}
	return key, nil
}

func resolvePrivateKeySecret(name, field string) (string, error) {
	statusText, err := runAppSecretsGet(name, "--no-request", field)
	if err != nil {
		return "", err
	}
	var status struct {
		Key  string `json:"key"`
		Tier string `json:"tier"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(statusText)), &status); err != nil || status.Key != name || status.Tier == "" {
		return "", fmt.Errorf("%s: secrets get %s --no-request printed an unparsable status (expected {\"key\",\"tier\"})", field, name)
	}
	if status.Tier != "human" {
		return "", fmt.Errorf("App private key %s is readable by agent-tier callers; move it to a daemon-only store", name)
	}
	encoded, err := runAppSecretsGet(name, "--value", field)
	if err != nil {
		return "", err
	}
	key, err := DecodePrivateKey(encoded)
	if err != nil {
		return "", fmt.Errorf("%s: %s did not decode to a PEM private key (expected base64 of a -----BEGIN block)", field, name)
	}
	return key, nil
}

// DecodePrivateKey is a stored App key's PEM text: the base64 of it decoded as Node's Buffer.from
// decodes it (decodeBase64LikeNode), refused unless it is a PEM block, and trimmed.
func DecodePrivateKey(encoded string) (string, error) {
	decoded, err := decodeBase64LikeNode(encoded)
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(decoded))
	if !strings.HasPrefix(key, "-----BEGIN") {
		return "", errors.New("does not decode to a PEM block (-----BEGIN …)")
	}
	return key, nil
}

// decodeBase64LikeNode mirrors Buffer.from(value, "base64"): whitespace is insignificant,
// padding is optional, URL-safe characters are accepted, and a final dangling sextet is ignored.
func decodeBase64LikeNode(encoded string) ([]byte, error) {
	var raw strings.Builder
	for _, character := range encoded {
		switch {
		case character >= 'A' && character <= 'Z', character >= 'a' && character <= 'z', character >= '0' && character <= '9', character == '+', character == '/':
			raw.WriteRune(character)
		case character == '-':
			raw.WriteByte('+')
		case character == '_':
			raw.WriteByte('/')
		case character == '=', character == ' ', character == '\t', character == '\n', character == '\r':
		default:
			return nil, fmt.Errorf("not base64")
		}
	}
	value := raw.String()
	if len(value)%4 == 1 {
		value = value[:len(value)-1]
	}
	value += strings.Repeat("=", (4-len(value)%4)%4)
	return base64.StdEncoding.DecodeString(value)
}

func runAppSecretsGet(name, flag, field string) (string, error) {
	args := []string{"get", name, flag}
	environment := daemonEnvironment()
	binary, ok := lookpath.On(pathValue(environment), "secrets")
	if !ok {
		return "", fmt.Errorf("%s: the secrets command is not on PATH, so %s cannot be read", field, name)
	}
	command := exec.Command(binary, args...)
	command.Env = environment
	command.Stdin = os.Stdin
	output, err := command.Output()
	if err != nil {
		return "", commandError(field+": secrets get "+name+" "+flag, err)
	}
	return string(output), nil
}

func daemonEnvironment() []string {
	environment := runtime.WithoutNATSSeeds(os.Environ())
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		if !strings.HasPrefix(entry, "SECRETSD_SESSION_TOKEN_FILE=") {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func commandError(field string, err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		stderr := strings.TrimSpace(string(exit.Stderr))
		if stderr != "" {
			return fmt.Errorf("%s failed (exit %d): %s", field, exit.ExitCode(), stderr)
		}
		return fmt.Errorf("%s failed (exit %d)", field, exit.ExitCode())
	}
	return fmt.Errorf("%s could not run: %w", field, err)
}

func resolveStage3(file fileConfig, configDir string, cfg *Config) error {
	if file.DispatchURL != nil {
		url, err := dispatchBase(*file.DispatchURL, "dispatch_url")
		if err != nil {
			return err
		}
		cfg.DispatchURL = url
	}
	if file.DispatchTokenFile != nil {
		cfg.DispatchTokenFile = underConfig(*file.DispatchTokenFile, configDir)
	}
	workflowConfigured := file.DispatchURL != nil || file.DispatchTokenFile != nil || file.Projects != nil || file.GitHubApps != nil
	if workflowConfigured && file.Projects == nil {
		return errors.New("projects must declare at least one project")
	}
	if file.Projects != nil {
		cfg.Projects = file.Projects
	}
	cfg.Warnings = file.Warnings
	if file.Gates != nil {
		cfg.Gates = *file.Gates
	}
	if workflowConfigured && file.GitHubApps == nil {
		return errors.New("github_apps is required")
	}
	if file.GitHubApps != nil {
		cfg.GitHubApps = *file.GitHubApps
	}
	// A workflow boot needs its Dispatch bearer, its intake, and its own project; each is refused
	// here, so `legion start --check-config` never passes a file `legion start` refuses.
	if file.DispatchURL != nil {
		switch _, ok := cfg.Projects[cfg.Project]; {
		case cfg.DispatchTokenFile == "":
			return errors.New(missingDispatchTokenFile)
		case len(cfg.NatsURLs) == 0:
			return errors.New("nats_urls is required when dispatch_url is configured: the workflow's intake reads Envoy's notification stream")
		case !ok:
			return fmt.Errorf("projects must configure %s, the daemon's own project", cfg.Project)
		}
	}
	if file.Linger != nil {
		cfg.Linger = *file.Linger
	}
	if file.ProductionCheckBound != nil {
		cfg.ProductionCheckBound = *file.ProductionCheckBound
	}
	if file.PodUpdateRetask != nil {
		cfg.PodUpdateRetask = *file.PodUpdateRetask
	}
	if file.PodUpdateNotice != nil {
		cfg.PodUpdateNotice = *file.PodUpdateNotice
	}
	// Each round cap is carried across as the pointer it is: nil through, so a file that names
	// neither key leaves the sweep with no limit rather than with a number nobody chose.
	if file.PodUpdateMaxOpenRounds != nil {
		cfg.PodUpdateMaxOpenRounds = file.PodUpdateMaxOpenRounds
	}
	if file.PodUpdateRoundsPerMinute != nil {
		cfg.PodUpdateRoundsPerMinute = file.PodUpdateRoundsPerMinute
	}
	if file.ReviewRoundCap != nil {
		cfg.ReviewRoundCap = *file.ReviewRoundCap
	}
	return nil
}
