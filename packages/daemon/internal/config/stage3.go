package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/lookpath"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// DesignGate is the human design-review policy the workflow applies to root issues.
type DesignGate string

const (
	DesignGateRootIssues DesignGate = "root-issues"
	DesignGateOff        DesignGate = "off"
)

// Valid says whether gate is one of the two policies a daemon runs, which `legion.yaml`'s
// `gates.design` accepts and `legion controller start` requires the daemon to answer.
func (gate DesignGate) Valid() bool {
	return gate == DesignGateRootIssues || gate == DesignGateOff
}

// Gates contains the workflow gates the daemon enforces.
type Gates struct {
	Design DesignGate
}

// Project maps a Dispatch project prefix to its repository, parsed at load, its optional
// merge-queue role, merge rules, and the Slack channels whose @-mentions reach its controller.
type Project struct {
	Repo           ghrepo.Repository
	MergeQueueRole string
	Merge          MergeRules
	// SlackChannels are `slack_channels`, the Slack channel ids whose app mentions the project's
	// controller subscribes to, in the file's order; nil when the file names none.
	SlackChannels []string
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

// GitHubApp is one App's configuration. Load resolves the configured private-key source into
// PrivateKey; LoadForValidation preserves command and secret sources without executing them.
type GitHubApp struct {
	AppID             string
	PrivateKey        string
	PrivateKeyCommand string
	PrivateKeySecret  string
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

func readProjects(value *yaml.Node, key string) (map[string]Project, error) {
	if value.Tag == "!!null" || value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping", key)
	}
	projects := make(map[string]Project, len(value.Content)/2)
	// channelOwner is the project whose slack_channels named a channel first, in file order.
	channelOwner := map[string]string{}
	for index := 0; index+1 < len(value.Content); index += 2 {
		keyNode, entryNode := value.Content[index], value.Content[index+1]
		name := keyNode.Value
		if keyNode.Kind != yaml.ScalarNode || !projectKeyPattern.MatchString(name) {
			return nil, fmt.Errorf(`%s key %q must match ^[A-Z][A-Z0-9]*$`, key, name)
		}
		if _, exists := projects[name]; exists {
			return nil, fmt.Errorf("%s names %s twice", key, name)
		}
		project, err := readProject(entryNode, key+"."+name)
		if err != nil {
			return nil, err
		}
		for _, channel := range project.SlackChannels {
			if owner, taken := channelOwner[channel]; taken {
				return nil, fmt.Errorf("%s.%s.slack_channels names %q, which %s.%s.slack_channels already names: a channel's mentions reach one project's controller",
					key, name, channel, key, owner)
			}
			channelOwner[channel] = name
		}
		projects[name] = project
	}
	if len(projects) == 0 {
		return nil, errors.New("projects must declare at least one project")
	}
	return projects, nil
}

func readProject(value *yaml.Node, key string) (Project, error) {
	if value.Kind != yaml.MappingNode {
		return Project{}, fmt.Errorf("%s must be a mapping with repo", key)
	}
	fields, err := members(value, key, "repo", "merge", "merge_queue_role", "mergeQueueRole", "slack_channels")
	var unknown unknownKeyError
	if errors.As(err, &unknown) {
		// The shipped loader's own words (validateProjectEntry, config.ts).
		return Project{}, fmt.Errorf(`Unknown key %q`, key+"."+unknown.name)
	}
	if err != nil {
		return Project{}, err
	}
	var project Project
	repo, err := stringOf(fields["repo"], key+".repo")
	if err != nil {
		return Project{}, err
	}
	role := fields["merge_queue_role"]
	if role == nil {
		role = fields["mergeQueueRole"]
	}
	if project.MergeQueueRole, err = stringOf(role, key+".merge_queue_role"); err != nil {
		return Project{}, err
	}
	if repo == "" {
		return Project{}, fmt.Errorf(`%s.repo must be "owner/name" (got "undefined")`, key)
	}
	if project.Repo, err = ghrepo.Parse(key+".repo", repo); err != nil {
		return Project{}, err
	}
	if project.MergeQueueRole != "" && !roleNamePattern.MatchString(project.MergeQueueRole) {
		return Project{}, fmt.Errorf("%s.merge_queue_role is a bare role name (no notifications.role. prefix)", key)
	}
	if merge := fields["merge"]; merge != nil {
		if project.Merge, err = readMergeRules(merge, key+".merge"); err != nil {
			return Project{}, err
		}
	}
	if channels := fields["slack_channels"]; channels != nil {
		if project.SlackChannels, err = readSlackChannels(channels, key+".slack_channels"); err != nil {
			return Project{}, err
		}
	}
	return project, nil
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
	gates := Gates{Design: DesignGateRootIssues}
	design, err := stringOf(fields["design"], key+".design")
	if err != nil {
		return nil, err
	}
	if design != "" {
		gates.Design = DesignGate(design)
	}
	if !gates.Design.Valid() {
		return nil, fmt.Errorf("%s.design must be 'root-issues' or 'off'", key)
	}
	return &gates, nil
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
	fields, err := members(value, key, "app_id", "private_key", "private_key_command", "private_key_secret", "installations")
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
	for _, source := range []string{app.PrivateKey, app.PrivateKeyCommand, app.PrivateKeySecret} {
		if source != "" {
			sources++
		}
	}
	if sources != 1 {
		return GitHubApp{}, fmt.Errorf("%s requires exactly one of private_key, private_key_command, or private_key_secret", key)
	}
	if app.Installations == nil {
		app.Installations = map[string]string{}
	}
	return app, nil
}

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
	case app.PrivateKey != "":
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
		return GitHubApp{}, fmt.Errorf("%s requires exactly one of private_key, private_key_command, or private_key_secret", field)
	}
}

func executePrivateKeyCommand(command, field string) (string, error) {
	keyCommand := exec.Command("sh", "-c", command)
	keyCommand.Env = runtime.WithoutNATSSeeds(os.Environ())
	result, err := keyCommand.Output()
	if err != nil {
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
	if file.Gates != nil {
		cfg.Gates = *file.Gates
	}
	if workflowConfigured && file.GitHubApps == nil {
		return errors.New("github_apps is required")
	}
	if file.GitHubApps != nil {
		cfg.GitHubApps = *file.GitHubApps
	}
	// A workflow boot needs its Dispatch bearer, its intake, and its own project's repository; each
	// is refused here, so `legion start --check-config` never passes a file `legion start` refuses.
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
	if file.ReviewRoundCap != nil {
		cfg.ReviewRoundCap = *file.ReviewRoundCap
	}
	if file.MaxFixAttempts != nil {
		cfg.MaxFixAttempts = *file.MaxFixAttempts
	}
	return nil
}
