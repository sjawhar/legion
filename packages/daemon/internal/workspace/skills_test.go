package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// testCatalog is a marketplace catalog of the shape every theorem-labs skills repository carries:
// a name, an owner, and plugins whose sources are directories of the repository.
const testCatalog = `{
  "name": "verification-skills",
  "owner": {"name": "acme"},
  "plugins": [
    {"name": "verification-skills", "source": "./plugins/verification-skills"},
    {"name": "dev-infra", "source": "./plugins/dev-infra"}
  ]
}
`

// seedSkillsRemote is a bare remote holding one commit with catalog at path, pushed to main: the
// skills marketplace a test's CloneSkills clones. It returns the remote and that commit.
func seedSkillsRemote(t *testing.T, path, catalog string) (remote, commit string) {
	t.Helper()
	root := t.TempDir()
	remote = filepath.Join(root, "remote.git")
	runSetup(t, root, fixturegit.InitBare(remote, "main")...)
	seed := filepath.Join(root, "seed")
	runSetup(t, root, fixturegit.InitWorkTree(seed, "main")...)
	if path != "" {
		file := filepath.Join(seed, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(catalog), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("skills\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"verification-skills", "dev-infra"} {
		plugin := filepath.Join(seed, "plugins", name, "skills", "example.md")
		if err := os.MkdirAll(filepath.Dir(plugin), 0o755); err != nil {
			t.Fatal(err)
		}
		// Each plugin's tree holds its own bytes, so two plugins never digest alike and a check
		// that compared the wrong plugin's tree would be caught (SkillsTreeDigest).
		if err := os.WriteFile(plugin, []byte("A local skill of "+name+".\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runSetup(t, seed, "git", "add", "-A")
	runSetup(t, seed, "git", "-c", "commit.gpgsign=false", "commit", "-m", "the skills marketplace")
	runSetup(t, seed, "git", "push", remote, "main")
	return remote, strings.TrimSpace(runSetup(t, seed, "git", "rev-parse", "HEAD"))
}

// skillsRequest is an issue pod's skills init container's request: the skills volume root and the
// credential directory are the container's own.
func skillsRequest(t *testing.T) SkillsRequest {
	t.Helper()
	return SkillsRequest{
		Repo: ghrepo.MustParse("acme/widgets"), Token: staticToken(testInstallationToken),
		CredentialDir: t.TempDir(), Dir: filepath.Join(t.TempDir(), "skills"),
		Log: func(line string) { t.Logf("skills logged: %s", line) },
	}
}

// CloneSkills clones the marketplace shallow with the one-shot credential, into a directory of its
// own under the skills volume, and writes the three records beside it: the commit, the
// marketplace's name and its plugins, one per line and in the catalog's order. The credential is
// gone once it returns, and the clone is one commit deep — a pod needs the files, never the
// history.
func TestCloneSkillsClonesShallowAndRecordsWhatItCloned(t *testing.T) {
	run := newLocalRunner(t)
	request := skillsRequest(t)
	remote, commit := seedSkillsRemote(t, ".claude-plugin/marketplace.json", testCatalog)
	run.remote = remote

	report, err := CloneSkills(context.Background(), run, request)
	if err != nil {
		t.Fatalf("CloneSkills: %v", err)
	}
	if report.Commit != commit {
		t.Errorf("CloneSkills cloned %s, want the remote's %s", report.Commit, commit)
	}
	if report.Marketplace != "verification-skills" {
		t.Errorf("the marketplace is %q, want verification-skills", report.Marketplace)
	}
	if want := []string{"verification-skills", "dev-infra"}; !slices.Equal(report.Plugins, want) {
		t.Errorf("the plugins are %q, want the catalog's %q", report.Plugins, want)
	}
	clone := SkillsClone(request.Dir, request.Repo)
	if _, err := os.Stat(filepath.Join(clone, "README.md")); err != nil {
		t.Errorf("the clone holds no working tree: %v", err)
	}
	for _, record := range []struct{ path, want string }{
		{SkillsCommitFile(request.Dir), commit + "\n"},
		{SkillsMarketplaceFile(request.Dir), "verification-skills\n"},
		{SkillsPluginsFile(request.Dir), "verification-skills\ndev-infra\n"},
	} {
		content, err := os.ReadFile(record.path)
		if err != nil {
			t.Errorf("read %s: %v", record.path, err)
			continue
		}
		if string(content) != record.want {
			t.Errorf("%s holds %q, want %q", record.path, content, record.want)
		}
	}
	depth := strings.TrimSpace(runSetup(t, clone, "git", "rev-list", "--count", "HEAD"))
	if depth != "1" {
		t.Errorf("the clone holds %s commits, want the one a --depth 1 clone brings", depth)
	}
	if entries, err := os.ReadDir(request.CredentialDir); err != nil || len(entries) != 0 {
		t.Errorf("the one-shot credential outlived CloneSkills: %v (%v)", entries, err)
	}
	// The clone ran under the one-shot credential and read no configuration but its own.
	var cloned *Command
	for _, call := range run.Calls() {
		if commandWith(call.Argv, "git", "clone", "--depth", "1", "--single-branch") {
			cloned = &call
		}
	}
	if cloned == nil {
		t.Fatalf("no shallow clone ran: %q", run.Calls())
	}
	for _, want := range []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_ASKPASS=", "GIT_TERMINAL_PROMPT=0"} {
		if !slices.Contains(cloned.Env, want) {
			t.Errorf("the skills clone's environment lacks %s: %q", want, cloned.Env)
		}
	}
}

// A retry of the init container in the same pod works on the volume its killed first attempt left,
// so CloneSkills removes a clone already there rather than cloning into it.
func TestCloneSkillsReplacesWhatAnEarlierAttemptLeft(t *testing.T) {
	run := newLocalRunner(t)
	request := skillsRequest(t)
	remote, commit := seedSkillsRemote(t, ".claude-plugin/marketplace.json", testCatalog)
	run.remote = remote
	clone := SkillsClone(request.Dir, request.Repo)
	if err := os.MkdirAll(filepath.Join(clone, "half-written"), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := CloneSkills(context.Background(), run, request)
	if err != nil {
		t.Fatalf("CloneSkills over a partial clone: %v", err)
	}
	if report.Commit != commit {
		t.Errorf("CloneSkills cloned %s, want %s", report.Commit, commit)
	}
	if _, err := os.Stat(filepath.Join(clone, "half-written")); !os.IsNotExist(err) {
		t.Errorf("the partial clone's directory survived: %v", err)
	}
}

// Oh My Pi's own catalog spelling is read first, and the Claude-plugin spelling after it.
func TestCloneSkillsReadsEitherCatalogSpelling(t *testing.T) {
	for _, path := range []string{".omp-plugin/marketplace.json", ".claude-plugin/marketplace.json"} {
		t.Run(path, func(t *testing.T) {
			run := newLocalRunner(t)
			request := skillsRequest(t)
			remote, _ := seedSkillsRemote(t, path, testCatalog)
			run.remote = remote
			report, err := CloneSkills(context.Background(), run, request)
			if err != nil {
				t.Fatalf("CloneSkills: %v", err)
			}
			if report.Marketplace != "verification-skills" {
				t.Errorf("the marketplace is %q, want verification-skills", report.Marketplace)
			}
		})
	}
}

// A clone a role could not install from is the container's failure, by name: no catalog, a catalog
// that does not parse, one naming no plugin, and one whose name is not a word the role's start
// wrapper can read as one shell word. A pod whose skills volume holds such a tree would start its
// roles without their skills, which is what this container exists to prevent.
func TestCloneSkillsRefusesACloneNoRoleCouldInstallFrom(t *testing.T) {
	for name, tc := range map[string]struct {
		path, catalog, want string
	}{
		"no catalog":                    {"", "", "holds no marketplace catalog"},
		"a catalog that does not parse": {".claude-plugin/marketplace.json", "{not json", "parse the marketplace catalog"},
		"no plugin":                     {".claude-plugin/marketplace.json", `{"name":"skills","owner":{"name":"acme"},"plugins":[]}`, "names no plugin"},
		"a marketplace name that is no word": {
			".claude-plugin/marketplace.json", `{"name":"skills; rm -rf /","owner":{"name":"acme"},"plugins":[{"name":"a"}]}`,
			"the marketplace catalog's name",
		},
		"a plugin name that is no word": {
			".claude-plugin/marketplace.json", `{"name":"skills","owner":{"name":"acme"},"plugins":[{"name":"a b"}]}`,
			"a plugin name",
		},
	} {
		t.Run(name, func(t *testing.T) {
			run := newLocalRunner(t)
			request := skillsRequest(t)
			remote, _ := seedSkillsRemote(t, tc.path, tc.catalog)
			run.remote = remote
			_, err := CloneSkills(context.Background(), run, request)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CloneSkills = %v, want a refusal naming %q", err, tc.want)
			}
			if entries, readErr := os.ReadDir(request.CredentialDir); readErr != nil || len(entries) != 0 {
				t.Errorf("the one-shot credential outlived the refusal: %v (%v)", entries, readErr)
			}
		})
	}
}

// Everything is checked before the volume or the credential directory is touched: the zero
// repository, no directory, and no log.
func TestCloneSkillsRefusesAnIncompleteRequestBeforeItTouchesAnything(t *testing.T) {
	run := newLocalRunner(t)
	for name, tc := range map[string]struct {
		edit func(*SkillsRequest)
		want string
	}{
		"no repository": {func(r *SkillsRequest) { r.Repo = ghrepo.Repository{} }, "skills repository is required"},
		"no directory":  {func(r *SkillsRequest) { r.Dir = "" }, "skills directory is required"},
		"no log":        {func(r *SkillsRequest) { r.Log = nil }, "names no log"},
		"no token":      {func(r *SkillsRequest) { r.Token = nil }, "provisioning token is required"},
		"no credential directory": {
			func(r *SkillsRequest) { r.CredentialDir = "" }, "credential directory is required",
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := skillsRequest(t)
			tc.edit(&request)
			if _, err := CloneSkills(context.Background(), run, request); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CloneSkills = %v, want a refusal naming %q", err, tc.want)
			}
			if request.Dir != "" {
				if _, err := os.Stat(request.Dir); !os.IsNotExist(err) {
					t.Errorf("the refusal made the skills directory: %v", err)
				}
			}
		})
	}
}

// skillsFailureRunner injects a failure at one command boundary while the real runner still
// creates the clone. The credential itself is real, including its cleanup on every return path.
type skillsFailureRunner struct {
	Runner
	before func(Command) (Result, error, bool)
}

func (r skillsFailureRunner) Run(ctx context.Context, command Command) (Result, error) {
	if result, err, handled := r.before(command); handled {
		return result, err
	}
	return r.Runner.Run(ctx, command)
}

// The token is needed for the transfer alone: reading the cloned catalog and recording its commit
// must happen after it has gone, not merely without its file pointer in the command environment.
func TestCloneSkillsRemovesTheCredentialBeforeReadingTheClone(t *testing.T) {
	run := newLocalRunner(t)
	request := skillsRequest(t)
	run.remote, _ = seedSkillsRemote(t, ".claude-plugin/marketplace.json", testCatalog)
	probe := &tokenProbe{Runner: run, dir: request.CredentialDir}
	if _, err := CloneSkills(context.Background(), probe, request); err != nil {
		t.Fatal(err)
	}
	for _, ran := range probe.ran {
		if strings.Contains(ran, "rev-parse HEAD") && !strings.HasSuffix(ran, "token=false") {
			t.Errorf("the credential outlived the clone transfer: %s", ran)
		}
	}
}

// A failed command and a failed removal are independent failures. Keeping the command's error
// must not hide that the token directory remains, and the named return must not turn either one
// into success. Read-only directories force the real RemoveAll to fail without a cleanup mock.
func TestCloneSkillsReportsCredentialRemovalFailureAlongsideTheTransferFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory permissions this cleanup failure requires")
	}
	for _, failTransfer := range []bool{false, true} {
		t.Run(map[bool]string{false: "successful transfer", true: "failed transfer"}[failTransfer], func(t *testing.T) {
			run := newLocalRunner(t)
			request := skillsRequest(t)
			run.remote, _ = seedSkillsRemote(t, ".claude-plugin/marketplace.json", testCatalog)
			var credentialDir string
			defer func() {
				if credentialDir != "" {
					_ = os.Chmod(credentialDir, 0o700)
					_ = os.RemoveAll(credentialDir)
				}
			}()
			probe := skillsFailureRunner{Runner: run, before: func(command Command) (Result, error, bool) {
				if !commandWith(command.Argv, "git", "clone") {
					return Result{}, nil, false
				}
				credentialDir = filepath.Dir(commandEnv(command, provisioningTokenFileEnv))
				if err := os.Chmod(credentialDir, 0o500); err != nil {
					t.Fatal(err)
				}
				if failTransfer {
					return Result{ExitCode: 7, Stderr: "transfer interrupted"}, nil, true
				}
				return Result{}, nil, false
			}}
			_, err := CloneSkills(context.Background(), probe, request)
			if err == nil || !strings.Contains(err.Error(), "remove provisioning credential directory") {
				t.Errorf("CloneSkills hid the credential removal failure: %v", err)
			}
			if failTransfer && (err == nil || !strings.Contains(err.Error(), "transfer interrupted")) {
				t.Errorf("CloneSkills lost the transfer failure: %v", err)
			}
		})
	}
}

// The init container's in-memory TMPDIR survives its restart too. A killed attempt's credential
// must be removed even when the new attempt cannot get a token, and old records cannot advertise
// a clone that this attempt has already discarded.
func TestCloneSkillsRetryRemovesKilledAttemptsCredentialAndRecords(t *testing.T) {
	request := skillsRequest(t)
	stale := filepath.Join(request.CredentialDir, "provisioning-credential-killed")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "token"), []byte(testInstallationToken), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(request.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{SkillsCommitFile(request.Dir), SkillsMarketplaceFile(request.Dir), SkillsPluginsFile(request.Dir)} {
		if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := errors.New("token unavailable")
	request.Token = func() (string, error) { return "", want }
	if _, err := CloneSkills(context.Background(), newLocalRunner(t), request); !errors.Is(err, want) {
		t.Errorf("CloneSkills = %v, want the token failure", err)
	}
	for _, path := range []string{stale, SkillsCommitFile(request.Dir), SkillsMarketplaceFile(request.Dir), SkillsPluginsFile(request.Dir)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("retry left stale material at %s: %v", path, err)
		}
	}
}

// A local marketplace can still point a plugin at GitHub. Reject those sources in the init
// container, rather than letting the role-side install clone and end the sandbox.
func TestSkillsCatalogRefusesPluginsThatWouldFetchAtRoleStart(t *testing.T) {
	for name, source := range map[string]string{
		"github":           `{"source":"github","repo":"acme/another"}`,
		"git URL":          `{"source":"url","url":"https://github.com/acme/another.git"}`,
		"git subdirectory": `{"source":"git-subdir","url":"acme/another","path":"plugin"}`,
		"missing":          `null`,
		"absolute":         `"/tmp/plugin"`,
		"escaping":         `"./../plugin"`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			catalog := filepath.Join(dir, ".claude-plugin", "marketplace.json")
			if err := os.MkdirAll(filepath.Dir(catalog), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(catalog, []byte(`{"name":"skills","owner":{"name":"acme"},"plugins":[{"name":"one","source":`+source+`}]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := readSkillsCatalog(dir); err == nil || !strings.Contains(err.Error(), "source") {
				t.Errorf("catalog admitted a role-side fetch or non-local source: %v", err)
			}
		})
	}
}

// The token may rotate between reads. Read it once and scrub that exact value rather than a new
// value that the helper never held; otherwise the failed transfer puts its old token in the log.
func TestCloneSkillsReadsAndScrubsTheSameTokenOnce(t *testing.T) {
	request := skillsRequest(t)
	reads := 0
	request.Token = func() (string, error) {
		reads++
		if reads > 1 {
			return "", errors.New("the token must be read only once")
		}
		return testInstallationToken, nil
	}
	run := skillsFailureRunner{Runner: newLocalRunner(t), before: func(command Command) (Result, error, bool) {
		if commandWith(command.Argv, "git", "clone") {
			return Result{ExitCode: 1, Stderr: "remote: invalid token " + testInstallationToken}, nil, true
		}
		return Result{}, nil, false
	}}
	_, err := CloneSkills(context.Background(), run, request)
	if err == nil || !strings.Contains(err.Error(), "remote: invalid token ***") || strings.Contains(err.Error(), testInstallationToken) {
		t.Errorf("CloneSkills did not scrub the transfer's actual credential: %v", err)
	}
	if reads != 1 {
		t.Errorf("CloneSkills read the token %d times, want once", reads)
	}
	if entries, err := os.ReadDir(request.CredentialDir); err != nil || len(entries) != 0 {
		t.Errorf("transfer failure left the credential: %v (%v)", entries, err)
	}
}

// Oh My Pi prepends metadata.pluginRoot to every relative plugin source. Validate that same
// directory, including links, so a valid nested marketplace works and an escaped one fails in
// skills-fetch rather than after the roles start.
func TestSkillsCatalogUsesTheActualPluginRoot(t *testing.T) {
	for name, tc := range map[string]struct {
		root, source string
		valid        bool
	}{
		"nested":           {"plugins", "./one", true},
		"root":             {"", "./plugins/one", true},
		"escaping root":    {"../outside", "./one", false},
		"escaping symlink": {"linked", "./one", false},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			clone := filepath.Join(dir, "clone")
			for _, path := range []string{filepath.Join(clone, ".claude-plugin"), filepath.Join(clone, "plugins", "one"), filepath.Join(dir, "outside", "one")} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(filepath.Join(dir, "outside"), filepath.Join(clone, "linked")); err != nil {
				t.Fatal(err)
			}
			catalog := `{"name":"skills","owner":{"name":"acme"},"metadata":{"pluginRoot":"` + tc.root + `"},"plugins":[{"name":"one","source":"` + tc.source + `"}]}`
			if err := os.WriteFile(filepath.Join(clone, ".claude-plugin", "marketplace.json"), []byte(catalog), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, _, err := readSkillsCatalog(clone)
			if (err == nil) != tc.valid {
				t.Errorf("readSkillsCatalog = %v, want valid=%t", err, tc.valid)
			}
		})
	}
}

// Oh My Pi's install CLI interprets these right-hand sides as npm tags or versions before asking
// whether they are known marketplaces. None may reach a role-side `<plugin>@<marketplace>` call.
func TestSkillsCatalogRefusesMarketplaceNamesThatOMPReadsAsNPM(t *testing.T) {
	for _, name := range []string{"latest", "next", "beta", "alpha", "canary", "rc", "dev", "stable", "nightly", "experimental", "1.2.3"} {
		t.Run(name, func(t *testing.T) {
			clone := t.TempDir()
			if err := os.MkdirAll(filepath.Join(clone, ".claude-plugin"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(clone, "one"), 0o700); err != nil {
				t.Fatal(err)
			}
			catalog := `{"name":"` + name + `","plugins":[{"name":"one","source":"./one"}]}`
			if err := os.WriteFile(filepath.Join(clone, ".claude-plugin", "marketplace.json"), []byte(catalog), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := readSkillsCatalog(clone); err == nil {
				t.Errorf("marketplace %q would be interpreted as an npm spec at role start", name)
			}
		})
	}
}
