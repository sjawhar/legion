package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// DesignGate is the human design-review policy the workflow applies to root issues.
type DesignGate string

const (
	DesignGateRootIssues DesignGate = "root-issues"
	DesignGateOff        DesignGate = "off"
)

// Gates contains the workflow gates the daemon enforces.
type Gates struct {
	Design DesignGate
}

// Project maps a Dispatch project prefix to its repository and optional merge-queue role.
type Project struct {
	Repo           string
	MergeQueueRole string
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

// GitHubApps holds the two App identities Legion needs. Review and implement are distinct because
// only the implement App has repository contents permission.
type GitHubApps struct {
	Implement GitHubApp
	Review    GitHubApp
}

var (
	projectKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]*$`)
	roleNamePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

func readProjects(value *yaml.Node, key string) (map[string]Project, error) {
	if value.Tag == "!!null" || value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping", key)
	}
	projects := make(map[string]Project, len(value.Content)/2)
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
	var project Project
	seen := map[string]bool{}
	for index := 0; index+1 < len(value.Content); index += 2 {
		fieldNode, fieldValue := value.Content[index], value.Content[index+1]
		field := fieldNode.Value
		if seen[field] {
			return Project{}, fmt.Errorf("%s names %s twice", key, field)
		}
		seen[field] = true
		switch field {
		case "repo":
			read, err := readString(fieldValue, key+".repo")
			if err != nil {
				return Project{}, err
			}
			if read != nil {
				project.Repo = *read
			}
		case "merge_queue_role", "mergeQueueRole":
			read, err := readString(fieldValue, key+".merge_queue_role")
			if err != nil {
				return Project{}, err
			}
			if read != nil {
				project.MergeQueueRole = *read
			}
		default:
			return Project{}, fmt.Errorf(`Unknown key %q`, key+"."+field)
		}
	}
	if strings.Count(project.Repo, "/") != 1 || strings.HasPrefix(project.Repo, "/") || strings.HasSuffix(project.Repo, "/") {
		got := project.Repo
		if got == "" {
			got = "undefined"
		}
		return Project{}, fmt.Errorf(`%s.repo must be "owner/name" (got %q)`, key, got)
	}
	if project.MergeQueueRole != "" && !roleNamePattern.MatchString(project.MergeQueueRole) {
		return Project{}, fmt.Errorf("%s.merge_queue_role is a bare role name (no notifications.role. prefix)", key)
	}
	return project, nil
}

func readGates(value *yaml.Node, key string) (*Gates, error) {
	if value.Tag == "!!null" || value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping", key)
	}
	gates := Gates{Design: DesignGateRootIssues}
	for index := 0; index+1 < len(value.Content); index += 2 {
		field, fieldValue := value.Content[index].Value, value.Content[index+1]
		switch field {
		case "merge":
			return nil, errors.New(gatesMergeMessage)
		case "design":
			read, err := readString(fieldValue, key+".design")
			if err != nil {
				return nil, err
			}
			if read != nil {
				gates.Design = DesignGate(*read)
			}
		default:
			return nil, fmt.Errorf("unknown key %s.%s", key, field)
		}
	}
	if gates.Design != DesignGateRootIssues && gates.Design != DesignGateOff {
		return nil, fmt.Errorf("%s.design must be 'root-issues' or 'off'", key)
	}
	return &gates, nil
}

func readGitHubApps(value *yaml.Node, key string) (*GitHubApps, error) {
	if value.Tag == "!!null" || value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping", key)
	}
	roles := map[string]*yaml.Node{}
	for index := 0; index+1 < len(value.Content); index += 2 {
		name := value.Content[index].Value
		if name != "implement" && name != "review" {
			return nil, fmt.Errorf("unknown key %s.%s", key, name)
		}
		if _, exists := roles[name]; exists {
			return nil, fmt.Errorf("%s names %s twice", key, name)
		}
		roles[name] = value.Content[index+1]
	}
	for _, role := range []string{"implement", "review"} {
		if roles[role] == nil || roles[role].Tag == "!!null" {
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
	var app GitHubApp
	seen := map[string]bool{}
	for index := 0; index+1 < len(value.Content); index += 2 {
		field, fieldValue := value.Content[index].Value, value.Content[index+1]
		if seen[field] {
			return GitHubApp{}, fmt.Errorf("%s names %s twice", key, field)
		}
		seen[field] = true
		switch field {
		case "app_id":
			read, err := readString(fieldValue, key+".app_id")
			if err != nil {
				return GitHubApp{}, err
			}
			if read != nil {
				app.AppID = *read
			}
		case "private_key":
			read, err := readString(fieldValue, key+".private_key")
			if err != nil {
				return GitHubApp{}, err
			}
			if read != nil {
				app.PrivateKey = *read
			}
		case "private_key_command":
			read, err := readString(fieldValue, key+".private_key_command")
			if err != nil {
				return GitHubApp{}, err
			}
			if read != nil {
				app.PrivateKeyCommand = *read
			}
		case "private_key_secret":
			read, err := readString(fieldValue, key+".private_key_secret")
			if err != nil {
				return GitHubApp{}, err
			}
			if read != nil {
				app.PrivateKeySecret = *read
			}
			if strings.ContainsAny(app.PrivateKeySecret, " \t\n\r") {
				return GitHubApp{}, fmt.Errorf("%s.private_key_secret must be a single secretsd key name (no whitespace)", key)
			}
		case "installations":
			installations, err := readInstallations(fieldValue, key+".installations")
			if err != nil {
				return GitHubApp{}, err
			}
			app.Installations = installations
		default:
			return GitHubApp{}, fmt.Errorf("unknown key %s.%s", key, field)
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
	result, err := exec.Command("sh", "-c", command).Output()
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
	decoded, err := decodeBase64LikeNode(encoded)
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(decoded)), "-----BEGIN") {
		return "", fmt.Errorf("%s: %s did not decode to a PEM private key (expected base64 of a -----BEGIN block)", field, name)
	}
	return strings.TrimSpace(string(decoded)), nil
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
	binary, err := lookPathIn("secrets", environment)
	if err != nil {
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
	environment := os.Environ()
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
		url, err := baseURL(*file.DispatchURL, "dispatch_url")
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
	if file.LingerHours != nil {
		cfg.LingerHours = *file.LingerHours
	}
	if file.ReviewRoundCap != nil {
		cfg.ReviewRoundCap = *file.ReviewRoundCap
	}
	return nil
}

