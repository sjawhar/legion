package workspace

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Every agent of a tree writes the shared clone, so everything below is what a tree agent can plant
// there for the next provisioning of the tree, and what provisioning's credentialed clone and fetch
// — the tmux runtime's, which name the token file in their environment — must never obey. On tmux
// that is defence, not a boundary (config.go); a pod's boundary, two init containers of which only
// the one that touches no tree volume holds the token, is proven by
// internal/runtime/sandbox/boundary_test.go.

// gitHooks is every hook githooks(5) names; a planted script under each name records whether git
// ran it.
var gitHooks = []string{
	"applypatch-msg", "pre-applypatch", "post-applypatch", "pre-commit", "pre-merge-commit",
	"prepare-commit-msg", "commit-msg", "post-commit", "pre-rebase", "post-checkout", "post-merge",
	"pre-push", "pre-receive", "update", "proc-receive", "post-receive", "post-update",
	"reference-transaction", "push-to-checkout", "pre-auto-gc", "post-rewrite", "sendemail-validate",
	"fsmonitor-watchman", "p4-changelist", "p4-prepare-changelist", "p4-post-changelist",
	"p4-pre-submit", "post-index-change",
}

// plantHooks writes every hook into dir; each one that runs appends its name, and the token file
// its environment named, to sink.
func plantHooks(t *testing.T, dir, sink string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s token-file=%s\\n' \"${0##*/}\" \"$LEGION_PROVISIONING_TOKEN_FILE\" >> '" + sink + "'\nexit 0\n"
	for _, hook := range gitHooks {
		if err := os.WriteFile(filepath.Join(dir, hook), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// advanceRemote puts a new commit on the remote's main, so the next fetch updates a ref.
func advanceRemote(t *testing.T, remote string) {
	t.Helper()
	pusher := filepath.Join(t.TempDir(), "pusher")
	runSetup(t, filepath.Dir(pusher), "git", "clone", "--quiet", remote, pusher)
	runSetup(t, pusher, "git", "-c", "user.name=Legion test", "-c", "user.email=legion-test@example.invalid", "commit", "--quiet", "--allow-empty", "-m", "advance")
	runSetup(t, pusher, "git", "push", "--quiet", "origin", "HEAD:main")
}

// provisionTwice provisions WIDGETS-42, lets plant tamper with the shared clone, advances the
// remote, and provisions WIDGETS-43 on the same clone: a credentialed fetch that moves a ref, a
// workspace add, and the configuration writes, all against what plant left. It returns the second
// provision's error.
func provisionTwice(t *testing.T, plant func(t *testing.T, run *recordingRunner, clone string)) error {
	t.Helper()
	run := newLocalRunner(t)
	req := provisionRequest(t)
	if _, err := Provision(context.Background(), run, req); err != nil {
		t.Fatalf("first provision: %v", err)
	}
	plant(t, run, filepath.Join(req.StateDir, "repos", "github.com", "acme", "widgets"))
	advanceRemote(t, run.remote)
	req.Issue = "WIDGETS-43"
	_, err := Provision(context.Background(), run, req)
	return err
}

// requireAbsent fails naming what a planted script recorded, when it recorded anything.
func requireAbsent(t *testing.T, sink, what string) {
	t.Helper()
	if body, err := os.ReadFile(sink); err == nil {
		t.Fatalf("provisioning ran %s:\n%s", what, strings.TrimSpace(string(body)))
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// A hook in the shared clone's hooks directory, or in a directory its core.hooksPath names, never
// runs: not in the credentialed fetch (reference-transaction), not in the workspace add
// (post-index-change), not in any other step. Nor does one that provisioning's own environment
// names through GIT_CONFIG_PARAMETERS (git's `-c`), which git reads after the pins.
func TestProvisionRunsNoHookTheTreePlanted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, clone, sink string)
	}{
		{"in .git/hooks", func(t *testing.T, clone, sink string) {
			plantHooks(t, filepath.Join(clone, ".git", "hooks"), sink)
		}},
		{"under core.hooksPath", func(t *testing.T, clone, sink string) {
			hooks := filepath.Join(t.TempDir(), "hooks")
			plantHooks(t, hooks, sink)
			runSetup(t, clone, "git", "--git-dir="+filepath.Join(clone, ".git"), "config", "core.hooksPath", hooks)
		}},
		{"under a core.hooksPath GIT_CONFIG_PARAMETERS names", func(t *testing.T, _, sink string) {
			hooks := filepath.Join(t.TempDir(), "hooks")
			plantHooks(t, hooks, sink)
			t.Setenv("GIT_CONFIG_PARAMETERS", "'core.hooksPath'='"+hooks+"'")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := filepath.Join(t.TempDir(), "hooks-ran")
			if err := provisionTwice(t, func(t *testing.T, _ *recordingRunner, clone string) { tc.plant(t, clone, sink) }); err != nil {
				t.Fatalf("second provision: %v", err)
			}
			requireAbsent(t, sink, "hooks the tree planted")
		})
	}
}

// recordingScript writes an executable that appends what it is and the token file its environment
// named to sink, then runs tail (a shell fragment).
func recordingScript(t *testing.T, what, sink, tail string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), what)
	script := "#!/bin/sh\nprintf '" + what + " token-file=%s\\n' \"$LEGION_PROVISIONING_TOKEN_FILE\" >> '" + sink + "'\n" + tail + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// plantWorkspaceConfig writes a legacy .jj/workspace-config.toml into the shared clone's own
// working copy. jj migrates such a file into the configuration it reads, even under a config home
// that starts empty in every pod, so this is jj configuration a tree agent can hand the next
// provisioning.
func plantWorkspaceConfig(t *testing.T, clone, toml string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(clone, ".jj", "workspace-config.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// jj starts the git its configuration names. A git.executable-path the tree planted is never the
// git provisioning's jj starts: it starts the one boot resolved.
func TestProvisionStartsOnlyTheGitBootResolved(t *testing.T) {
	sink := filepath.Join(t.TempDir(), "planted-git-ran")
	err := provisionTwice(t, func(t *testing.T, run *recordingRunner, clone string) {
		git, err := exec.LookPath("git")
		if err != nil {
			t.Fatal(err)
		}
		planted := recordingScript(t, "git", sink, "exec '"+git+"' \"$@\"")
		plantWorkspaceConfig(t, clone, "[git]\nexecutable-path = '"+planted+"'\n")
	})
	if err != nil {
		t.Fatalf("second provision: %v", err)
	}
	requireAbsent(t, sink, "the git the tree configured")
}

// A snapshot runs the working-copy filter jj's configuration names on every changed file (the jj
// fork the worker image ships has filters; stock jj has none, and these tests refuse it). The
// credentialed fetch takes no snapshot of the shared clone's working copy, so a filter the tree
// planted there never runs with the token file in its environment — though it does run, in the
// uncredentialed workspace add.
func TestTheCredentialedFetchRunsNoWorkingCopyFilter(t *testing.T) {
	requireFilters(t)
	sink := filepath.Join(t.TempDir(), "filter-ran")
	err := provisionTwice(t, func(t *testing.T, _ *recordingRunner, clone string) {
		filter := recordingScript(t, "filter", sink, "exec cat")
		plantWorkspaceConfig(t, clone, "[git.filter]\nenabled = true\n[git.filter.drivers.planted]\nclean = ['"+filter+"']\nsmudge = ['cat']\nrequired = false\n")
		for name, body := range map[string]string{".gitattributes": "*.txt filter=planted\n", "planted.txt": "changed\n"} {
			if err := os.WriteFile(filepath.Join(clone, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err != nil {
		t.Fatalf("second provision: %v", err)
	}
	body, err := os.ReadFile(sink)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	ran := nonEmptyLines(string(body))
	if len(ran) == 0 {
		t.Fatal("the filter the tree planted never ran, not even in the workspace add: the plant is not live")
	}
	for _, line := range ran {
		if line != "filter token-file=" {
			t.Errorf("the filter the tree planted ran inside a credentialed step: %s", line)
		}
	}
}

// requireFilters fails unless the jj under test, reading no configuration of the user's, has
// working-copy filters: provisioning is proven against the jj the worker image ships.
func requireFilters(t *testing.T) {
	t.Helper()
	probe := exec.Command("jj", "config", "list", "--include-defaults", "git.filter.enabled")
	probe.Env = append(os.Environ(), "JJ_CONFIG="+filepath.Join(t.TempDir(), "none.toml"), "XDG_CONFIG_HOME="+t.TempDir(), "HOME="+t.TempDir())
	if out, _ := probe.Output(); len(strings.TrimSpace(string(out))) == 0 {
		t.Fatal("this jj has no working-copy filters: provisioning is proven against the jj the worker image ships (packages/daemon/docker/worker.Dockerfile, ARG JJ_TOOL), which CI installs")
	}
}

// A url.<base>.insteadOf the tree wrote can rewrite the fetch's remote into an ext:: command, which
// git would run with the fetch's environment. Provisioning reaches a remote over https alone
// (here, and only in the tests, the local bare remote's file transport too), so the rewritten
// fetch fails and the command never runs.
func TestProvisionRefusesATransportTheTreeRewroteTo(t *testing.T) {
	sink := filepath.Join(t.TempDir(), "ext-ran")
	err := provisionTwice(t, func(t *testing.T, run *recordingRunner, clone string) {
		command := recordingScript(t, "ext", sink, "exit 1")
		gitDir := "--git-dir=" + filepath.Join(clone, ".git")
		runSetup(t, clone, "git", gitDir, "config", "protocol.ext.allow", "always")
		runSetup(t, clone, "git", gitDir, "config", "url.ext::"+command+".insteadOf", run.remote)
	})
	if err == nil || !strings.Contains(err.Error(), "transport 'ext' not allowed") {
		t.Errorf("second provision: %v; want the fetch refused for its ext transport", err)
	}
	requireAbsent(t, sink, "the ext:: command the tree rewrote the remote to")
}

// A remote URL the tree rewrote to a host of its own asks that host for credentials; the one-shot
// credential answers github.com alone, so the host never receives the token, and no askpass the
// tree configured is asked in its place.
func TestProvisionSendsTheTokenOnlyToGitHub(t *testing.T) {
	var mu sync.Mutex
	var received []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authorization := r.Header.Get("Authorization"); authorization != "" {
			mu.Lock()
			received = append(received, authorization)
			mu.Unlock()
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="planted"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	sink := filepath.Join(t.TempDir(), "askpass-ran")
	err := provisionTwice(t, func(t *testing.T, _ *recordingRunner, clone string) {
		gitDir := "--git-dir=" + filepath.Join(clone, ".git")
		runSetup(t, clone, "git", gitDir, "remote", "set-url", "origin", server.URL+"/acme/widgets")
		runSetup(t, clone, "git", gitDir, "config", "http.sslVerify", "false")
		runSetup(t, clone, "git", gitDir, "config", "core.askPass", recordingScript(t, "askpass", sink, "exit 1"))
	})
	if err == nil {
		t.Error("the second provision fetched from the planted host")
	}
	requireAbsent(t, sink, "the askpass the tree configured")
	mu.Lock()
	defer mu.Unlock()
	for _, authorization := range received {
		decoded, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(authorization, "Basic "))
		t.Errorf("the planted host received credentials %q", decoded)
	}
}

// The one-shot credential is git's own URL match: a helper for https://github.com, after a reset
// of every helper configured before it — the operator's, and the tree's own in the shared clone.
// git asks it for every repository on github.com, whatever credential.useHttpPath says (an
// operator's global configuration can set it), and never for another scheme, host, or port.
func TestTheProvisioningCredentialAnswersOnlyGitHub(t *testing.T) {
	credential, err := newProvisioningCredential(t.TempDir(), "test-installation-token")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = credential.remove() }()
	repository := t.TempDir()
	runSetup(t, repository, "git", "init", "--quiet")
	treeHelper := "!f() { echo username=tree; echo password=tree-planted; }; f"
	runSetup(t, repository, "git", "config", "credential.helper", treeHelper)
	runSetup(t, repository, "git", "config", "credential.https://github.com.helper", treeHelper)
	for _, useHTTPPath := range []bool{false, true} {
		global := filepath.Join(t.TempDir(), "gitconfig")
		if err := os.WriteFile(global, []byte(fmt.Sprintf("[credential]\n\tuseHttpPath = %t\n", useHTTPPath)), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			url    string
			answer bool
		}{
			{"https://github.com", true},
			{"https://github.com/acme/widgets", true},
			{"https://github.com/acme/other", true},
			{"http://github.com/acme/widgets", false},
			{"https://evil.example/acme/widgets", false},
			{"https://github.com.evil.example/acme/widgets", false},
			{"https://github.com:8443/acme/widgets", false},
		} {
			fill := exec.Command("git", "credential", "fill")
			fill.Dir = repository
			fill.Stdin = strings.NewReader("url=" + tc.url + "\n\n")
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "GIT_") {
					fill.Env = append(fill.Env, entry)
				}
			}
			fill.Env = append(fill.Env, "GIT_CONFIG_GLOBAL="+global, "GIT_CONFIG_NOSYSTEM=1")
			fill.Env = append(fill.Env, credential.env...)
			out, err := fill.Output()
			got := ""
			for _, line := range strings.Split(string(out), "\n") {
				if value, ok := strings.CutPrefix(line, "password="); ok {
					got = value
				}
			}
			switch {
			case tc.answer && (err != nil || got != "test-installation-token"):
				t.Errorf("useHttpPath %t, %s: password %q (%v), want the provisioning token", useHTTPPath, tc.url, got, err)
			case !tc.answer && (err == nil || got != ""):
				t.Errorf("useHttpPath %t, %s: password %q (%v), want no credential", useHTTPPath, tc.url, got, err)
			}
		}
	}
}
