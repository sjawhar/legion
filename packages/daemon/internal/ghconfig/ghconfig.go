// Package ghconfig renders the two files a plain `gh` reads its GitHub credential from, so a
// Legion role's GitHub App token reaches `gh` and `git` as gh's own configuration rather than
// through a wrapper of the daemon's.
//
// The files are gh's `hosts.yml`, which holds the token for github.com, and its `config.yml`, in
// the directory GH_CONFIG_DIR names. gh reads both on every invocation and holds nothing between
// invocations, so the daemon rewriting hosts.yml from a fresh lease is what the next `gh` runs
// with, and `gh auth git-credential`, the helper every shared clone's `credential.helper` names
// (workspace.GitHubCredentialHelper), answers git from the same file. In a pod the directory is a
// read-only Secret volume, which gh never needs to write: it writes under GH_CONFIG_DIR only when
// told to (`gh auth login`, `gh config set`) or when migrating a pre-2.40 hosts.yml into its
// multi-account shape, and its git-credential helper treats git's `store` and `erase` as no-ops,
// so nothing a worker's git does tries to write there either. A `config.yml` carrying
// `version: "1"` is what tells gh that migration has already happened, and Hosts renders the
// migrated shape it then expects.
package ghconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// HostsFile is the name of gh's per-host credential file under GH_CONFIG_DIR.
const HostsFile = "hosts.yml"

// ConfigFile is the name of gh's settings file under GH_CONFIG_DIR.
const ConfigFile = "config.yml"

// GitUser is the username GitHub documents for an installation token over HTTPS, and the user
// hosts.yml records for it, so `gh auth git-credential` answers git with it.
const GitUser = "x-access-token"

// Config is config.yml as gh >= 2.40 writes it after its multi-account migration: the version
// alone, which is what stops gh from attempting that migration write of its own.
const Config = "version: \"1\"\n"

// host is the github.com entry of hosts.yml in gh's multi-account shape: the active user's token
// and name at the top, the protocol git uses, and the same token again under `users`, where gh
// keeps every account logged in to the host. Decoding a pre-2.40 hosts.yml, which has no `users`
// block, fills the top-level fields alone, which is where TokenFromHosts reads.
type host struct {
	OAuthToken  string          `yaml:"oauth_token"`
	User        string          `yaml:"user"`
	GitProtocol string          `yaml:"git_protocol"`
	Users       map[string]user `yaml:"users,omitempty"`
}

// user is one account under a host's `users` block.
type user struct {
	OAuthToken string `yaml:"oauth_token"`
}

// hostsFile is hosts.yml: one entry, github.com, since Legion's Apps live on github.com alone.
type hostsFile struct {
	GitHub host `yaml:"github.com"`
}

// Hosts renders hosts.yml for token in the shape gh >= 2.40 writes after its multi-account
// migration, with GitUser as the one account:
//
//	github.com:
//	    oauth_token: <token>
//	    user: x-access-token
//	    git_protocol: https
//	    users:
//	        x-access-token:
//	            oauth_token: <token>
//
// yaml.v3 renders it, so a token the encoder would quote is quoted; an installation token is a
// `ghs_` string with no YAML-special character, so it never is.
func Hosts(token string) string {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(4)
	if err := encoder.Encode(hostsFile{GitHub: host{
		OAuthToken:  token,
		User:        GitUser,
		GitProtocol: "https",
		Users:       map[string]user{GitUser: {OAuthToken: token}},
	}}); err != nil {
		// Encoding a struct of strings cannot fail; a failure here is a bug, never a condition a
		// caller handles.
		panic(fmt.Sprintf("render %s: %v", HostsFile, err))
	}
	return buf.String()
}

// TokenFromHosts reads the github.com token out of a hosts.yml, the one Hosts renders or gh's own
// pre-2.40 shape without a `users` block. A file that does not parse, or that names no
// github.com oauth_token (an empty file included), is an error naming the key.
func TokenFromHosts(data []byte) (string, error) {
	var hosts hostsFile
	if err := yaml.Unmarshal(data, &hosts); err != nil {
		return "", fmt.Errorf("parse %s: %w", HostsFile, err)
	}
	if hosts.GitHub.OAuthToken == "" {
		return "", fmt.Errorf("%s names no github.com oauth_token", HostsFile)
	}
	return hosts.GitHub.OAuthToken, nil
}

// Rendered is one role's two gh files, ready to write under its GH_CONFIG_DIR, with the label of
// the App the token belongs to ("implement" or "review") and the lease's expiry, so the daemon's
// log line can say whose token it wrote and how long it lasts.
type Rendered struct {
	Hosts     string
	Config    string
	App       string
	ExpiresAt time.Time
}

// Render is Rendered for one lease: hosts.yml for token, the fixed config.yml, and the App and
// expiry carried through for the log.
func Render(token, app string, expiresAt time.Time) Rendered {
	return Rendered{Hosts: Hosts(token), Config: Config, App: app, ExpiresAt: expiresAt}
}

// Write brings dir, a role's GH_CONFIG_DIR, to rendered: the directory made 0700 if it is gone,
// config.yml written once when absent or different, and hosts.yml replaced — a temporary file in
// the directory (0600, as os.CreateTemp makes it) renamed over it, so the gh reading it never sees
// a half-written file — only when its content differs. It reports whether hosts.yml changed. It is
// the one writer of a role's gh directory: the tmux runtime calls it at spawn and at each refresh
// tick, and `legion controller start` calls it for the operator-launched controller's directory.
func Write(dir string, rendered Rendered) (changed bool, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return false, err
	}
	config := filepath.Join(dir, ConfigFile)
	if current, err := os.ReadFile(config); err != nil || string(current) != rendered.Config {
		if err := os.WriteFile(config, []byte(rendered.Config), 0o600); err != nil {
			return false, err
		}
	}
	hosts := filepath.Join(dir, HostsFile)
	current, err := os.ReadFile(hosts)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err == nil && bytes.Equal(current, []byte(rendered.Hosts)) {
		return false, nil
	}
	temporary, err := os.CreateTemp(dir, "."+HostsFile+"-")
	if err != nil {
		return false, err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := temporary.WriteString(rendered.Hosts); err != nil {
		_ = temporary.Close()
		return false, err
	}
	if err := temporary.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(temporaryPath, hosts); err != nil {
		return false, err
	}
	return true, nil
}
