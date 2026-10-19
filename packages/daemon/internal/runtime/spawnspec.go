package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
)

// envName is a name a shell accepts as a variable, which is also a valid Kubernetes Secret key.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// IsEnvName reports whether name is one a shell accepts as an environment variable's.
func IsEnvName(name string) bool { return envName.MatchString(name) }

// SecretsDir is `<state_dir>/secrets`, where secret files live under a state directory. Under a
// daemon's state directory the daemon prunes it (internal/daemon/secrets.go): a claim's files go
// when the claim is written with no process, and at boot every regular file that no claim with a
// process owns goes, except the Dispatch token file; subdirectories are never pruned, except a
// claim's `-gh` directory (GHConfigDir), which goes with the claim's files. So a claim's file is
// named for its claim token (`<claim>` or `<claim>-<name>`), and a file the daemon holds across
// claims is a subdirectory (config.ProviderEnvDir) or is kept by name in boot's prune. In a
// Sandbox pod it is on the worker container's memory-backed state volume, which goes with the pod.
func SecretsDir(stateDir string) string { return filepath.Join(stateDir, "secrets") }

// SecretFilePath is the secret file name in SecretsDir: `<state_dir>/secrets/<name>`.
func SecretFilePath(stateDir, name string) string { return filepath.Join(SecretsDir(stateDir), name) }

// GitHubCredential is the daemon's one function a runtime calls for a role's GitHub credential
// files: a gh bundle (ghconfig.Bundle), one directory of gh files per GitHub owner of the project
// — the role's App token for owner, the one the agent writes to (its issue's repository's owner),
// and a token of the same App narrowed to reading for every other owner — with owner's directory
// the bundle's default, which GH_CONFIG_DIR names. The runtime writes it under the agent's bundle
// root (LEGION_GH_OWNERS_DIR) and rewrites it from the same function, asked for the same owner, as
// the leases near their expiry. The controller writes to no owner and acts as the review App
// (appauth.AppRoleFor): its bundle holds every owner read-only, the owner it is asked for its
// default, or the first owner when it is asked for none. A project with no GitHub owner at all to
// mint for is ErrNoGitHubOwner.
type GitHubCredential func(ctx context.Context, role claim.Role, owner string) (ghconfig.Bundle, error)

// ErrNoGitHubOwner is a GitHubCredential's refusal when there is no GitHub owner to mint for: a
// project whose repositories none of the Apps can act on, and a caller that names no owner. A
// controller then runs with gh acting as nobody, its gh directory empty.
var ErrNoGitHubOwner = errors.New("no GitHub owner to mint an App lease for")

// GHConfigDir is the tmux pane's bundle root (LEGION_GH_OWNERS_DIR): `<state_dir>/secrets/<claim>-gh`,
// the directory holding the claim's gh bundle (ghconfig.WriteBundle), named for its claim token so
// the daemon prunes it with the claim's other secret files. Its default owner's directory
// (ghconfig.OwnerDir) is the pane's GH_CONFIG_DIR. A pod has no use for it: there the files are a
// Secret volume the manifest mounts.
func GHConfigDir(stateDir string, token claim.Token) string {
	return SecretFilePath(stateDir, string(token)+"-gh")
}

// ValidateSpawnSpec is the refusal every runtime makes before anything touches its disk, its
// server, or its cluster: a spec it could not honour exactly. runtimeOwned is the runtime's own
// set of the variables it sets in the agent's environment itself — each runtime ties it by a test
// to the environment it builds — so a spec that sets one, in Env or as a secret's `<NAME>_FILE`
// pointer, is refused rather than one silently winning. Above all, nothing reaches the agent as a
// plain value that is shaped like a credential: a secret travels in Secrets, and reaches the
// process as a file. A runtime adds its own refusals after this one.
func ValidateSpawnSpec(spec SpawnSpec, runtimeOwned map[string]bool) error {
	if spec.Claim == "" {
		return errors.New("spawn: no claim token")
	}
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("spawn %s: "+format, append([]any{spec.Claim}, args...)...)
	}
	if spec.Role == claim.RoleController {
		if err := validateControllerShape(spec, refuse); err != nil {
			return err
		}
	} else if err := validateClaimShape(spec, refuse); err != nil {
		return err
	}
	if len(spec.Prompt.RolePromptPaths) == 0 {
		return refuse("no role prompt")
	}
	backend, err := LookupBackend(spec.Backend)
	if err != nil {
		return refuse("%v", err)
	}
	if !backend.Runs(spec.Role) {
		return refuse("the %s does not run on %s (%s), which runs only %s: %s", spec.Role, Of(spec.Backend), backend.Name, JoinRoles(backend.Roles), backend.Why)
	}
	// A workspace recovered after its volume was lost is recreated with a fresh session: the one
	// it had went with the volume, so a resume of it fails every attempt (workspace-init checks the
	// session before it recreates anything).
	if spec.ResumeSessionFile != "" && spec.WorkspaceRecoveredFrom != "" {
		return refuse("ResumeSessionFile %s and WorkspaceRecoveredFrom %s are both set: a workspace recovered after its volume was lost holds no session to resume",
			spec.ResumeSessionFile, spec.WorkspaceRecoveredFrom)
	}
	for _, name := range slices.Sorted(maps.Keys(spec.Env)) {
		switch {
		case !IsEnvName(name):
			return refuse("Env name %q is not an environment variable name", name)
		case runtimeOwned[name]:
			return refuse("Env sets %s, which the runtime sets itself", name)
		case HoldsSecretValue(name):
			return refuse("Env carries %s, a credential-shaped name; a secret travels in Secrets, as a file", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(spec.Secrets)) {
		pointer := name + "_FILE"
		switch {
		case !IsEnvName(name):
			return refuse("secret %q is not an environment variable name", name)
		case runtimeOwned[pointer]:
			return refuse("secret %s's pointer %s is a variable the runtime sets itself", name, pointer)
		case spec.Env[pointer] != "":
			return refuse("secret %s's pointer %s is also set in Env", name, pointer)
		}
	}
	return nil
}

// validateClaimShape is a workflow claim's launch: on an issue of a tree, with a role, under the
// token those derive.
func validateClaimShape(spec SpawnSpec, refuse func(string, ...any) error) error {
	for _, field := range []struct{ name, value string }{
		{"project", spec.Project}, {"tree", spec.Tree}, {"issue", spec.Issue},
		{"role", string(spec.Role)}, {"boot token", spec.BootToken},
	} {
		if field.value == "" {
			return refuse("no %s", field.name)
		}
	}
	// Each runtime builds the workspace's path from the issue, so the issue is the one shape a
	// Dispatch key has, never a path.
	for _, key := range []struct{ name, value string }{{"tree", spec.Tree}, {"issue", spec.Issue}} {
		if !claim.IsIssueKey(key.value) {
			return refuse("%s %q is not an issue key", key.name, key.value)
		}
	}
	if !claim.IsRole(spec.Role) {
		return refuse("%q is not a role", spec.Role)
	}
	// The token is what every runtime names the claim's workspace, Secret and pod after, and it is
	// derived from the project, issue and role rather than carried beside them. A spec whose token
	// is another claim's would run this claim's work under that claim's name, on its workspace and
	// beside its credentials.
	token, err := claim.NewToken(spec.Project, spec.Issue, spec.Role)
	if err != nil {
		return refuse("%w", err)
	}
	if token != spec.Claim {
		return refuse("the claim token of %s/%s/%s is %s", spec.Project, spec.Issue, spec.Role, token)
	}
	// Each runtime provisions the issue's workspace at the repository's default branch, by name.
	if !spec.Repository.IsZero() && spec.DefaultBranch == "" {
		return refuse("no default branch for repository %s", spec.Repository)
	}
	return nil
}

// validateControllerShape is the project controller's launch (`controller: daemon`): no issue, no
// tree, no repository and no workspace — the controller works Dispatch, never a checkout — under
// the project's controller token, which every runtime names its Secret and pod after.
func validateControllerShape(spec SpawnSpec, refuse func(string, ...any) error) error {
	for _, field := range []struct{ name, value string }{{"project", spec.Project}, {"boot token", spec.BootToken}} {
		if field.value == "" {
			return refuse("no %s", field.name)
		}
	}
	if !claim.IsController(spec.Role, spec.Issue, spec.Tree) {
		return refuse("the controller's claim is on no issue and no tree (issue %q, tree %q)", spec.Issue, spec.Tree)
	}
	if token := claim.ControllerToken(spec.Project); token != spec.Claim {
		return refuse("the controller's claim token of project %s is %s", spec.Project, token)
	}
	if !spec.Repository.IsZero() {
		return refuse("the controller works no repository (got %s)", spec.Repository)
	}
	if spec.DefaultBranch != "" {
		return refuse("the controller works no repository, so it has no default branch (got %s)", spec.DefaultBranch)
	}
	if spec.WorkspaceRecoveredFrom != "" {
		return refuse("the controller has no workspace to recover (got %s)", spec.WorkspaceRecoveredFrom)
	}
	return nil
}
