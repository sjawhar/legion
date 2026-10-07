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
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
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

// plantWorkspaceConfig writes toml as the configuration jj keeps for the working copy at dir, in
// the config home this test's runner and fixtures share (newLocalRunner's): the file `jj config
// path --workspace` names. On the tmux runtime that is a channel a pane has, since panes share
// the daemon's config home; a pod's config home starts empty every launch, and the one way a file
// on the tree volume becomes jj configuration there, a legacy .jj/workspace-config.toml, is the
// runner's to remove (disarmLegacyConfig; TestNoJJCommandReadsALegacyConfigurationTheTreePlanted).
// Either way it is jj configuration provisioning must not obey.
func plantWorkspaceConfig(t *testing.T, dir, toml string) {
	t.Helper()
	printed := nonEmptyLines(runSetup(t, dir, "jj", "config", "path", "--workspace", "--ignore-working-copy", "-R", dir))
	if len(printed) == 0 {
		t.Fatalf("jj config path --workspace -R %s printed nothing", dir)
	}
	if err := os.WriteFile(printed[len(printed)-1], []byte(toml), 0o644); err != nil {
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

// The repository's legacy configuration file, planted with its config-id gone (the one case jj
// migrates it into the configuration it reads), reaches no jj command the runner starts, here one
// run in a workspace whose .jj/repo names the shared clone's repository; the runner removes it.
// A workspace's legacy file beside a workspace-config-id, which jj reads instead, is ignored by jj
// and left in place (dispatch://LEGION-583). The cmd/legion test
// TestWorkspaceInitKeepsAnUnpushedChildDespiteARevsetAliasPlantedInTheSharedClone covers the
// shared clone's own workspace file end to end.
func TestNoJJCommandReadsALegacyConfigurationTheTreePlanted(t *testing.T) {
	run := newLocalRunner(t)
	ws, err := Provision(context.Background(), run, provisionRequest(t))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	planted := []byte("[revset-aliases]\n\"empty()\" = \"all()\"\n")
	repo := filepath.Join(ws.Clone, ".jj", "repo")
	if err := os.Remove(filepath.Join(repo, "config-id")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "config.toml"), planted, 0o644); err != nil {
		t.Fatal(err)
	}
	beside := filepath.Join(ws.Dir, ".jj", "workspace-config.toml")
	if err := os.WriteFile(filepath.Join(ws.Dir, ".jj", "workspace-config-id"), []byte(strings.Repeat("ab", 10)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(beside, planted, 0o644); err != nil {
		t.Fatal(err)
	}

	listed, err := RunCheckedIn(context.Background(), run, ws, []string{"jj", "config", "list", "--include-defaults", "revset-aliases", "--ignore-working-copy", "--color=never"})
	if err != nil {
		t.Fatalf("jj config list: %v", err)
	}
	if strings.Contains(listed.Stdout, "all()") {
		t.Errorf("jj read a planted alias:\n%s", listed.Stdout)
	}
	if _, err := os.Lstat(filepath.Join(repo, "config.toml")); !os.IsNotExist(err) {
		t.Errorf("the repository's legacy config.toml with no config-id remains (%v), want it removed", err)
	}
	if _, err := os.Stat(beside); err != nil {
		t.Errorf("the workspace's legacy file beside its workspace-config-id was touched: %v", err)
	}
}

// A workspace's .jj/repo is a file a tree agent can rewrite. One that names a directory other
// than the shared clone's .jj/repo refuses the command, naming both, before jj opens that
// directory and before anything in it is removed (a config.toml with no config-id beside it is
// removed only in the shared clone's own repository directory). That holds for a pointer to
// another directory outright, and for one a lexical join reads as the clone's own while jj, which
// canonicalizes it physically, opens another repository (pointThroughALink): there, RemoveFinished
// keeps a pushed workspace holding an edit no jj command ever snapshotted, where a snapshot run
// against the other repository would have hidden the edit and let the workspace be removed.
func TestTheRunnerRefusesAWorkspaceWhoseRepoPointerNamesAnotherDirectory(t *testing.T) {
	t.Run("an absolute pointer to another directory", func(t *testing.T) {
		run := newLocalRunner(t)
		ws, err := Provision(context.Background(), run, provisionRequest(t))
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		elsewhere := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.Mkdir(elsewhere, 0o755); err != nil {
			t.Fatal(err)
		}
		untouched := filepath.Join(elsewhere, "config.toml")
		if err := os.WriteFile(untouched, []byte("[user]\nname = \"not the clone's\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws.Dir, ".jj", "repo"), []byte(elsewhere), 0o644); err != nil {
			t.Fatal(err)
		}
		clone, err := filepath.EvalSymlinks(filepath.Join(ws.Clone, ".jj", "repo"))
		if err != nil {
			t.Fatal(err)
		}
		named, err := filepath.EvalSymlinks(elsewhere)
		if err != nil {
			t.Fatal(err)
		}

		_, err = RunCheckedIn(context.Background(), run, ws, []string{"jj", "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", "commit_id"})
		if err == nil || !strings.Contains(err.Error(), named) || !strings.Contains(err.Error(), clone) {
			t.Errorf("jj in a workspace whose .jj/repo names %s = %v, want a refusal naming it and the shared clone's %s", named, err, clone)
		}
		if _, err := os.Stat(untouched); err != nil {
			t.Errorf("%s, outside the shared clone, was touched: %v", untouched, err)
		}
	})

	t.Run("a symlink then .. that a lexical join cancels", func(t *testing.T) {
		run := newLocalRunner(t)
		ws, err := Provision(context.Background(), run, provisionRequest(t))
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		decoy := pointThroughALink(t, ws)
		if err := os.Remove(filepath.Join(decoy, "config-id")); err != nil {
			t.Fatal(err)
		}
		untouched := filepath.Join(decoy, "config.toml")
		if err := os.WriteFile(untouched, []byte("[revset-aliases]\n\"empty()\" = \"all()\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		clone, err := filepath.EvalSymlinks(filepath.Join(ws.Clone, ".jj", "repo"))
		if err != nil {
			t.Fatal(err)
		}
		named, err := filepath.EvalSymlinks(decoy)
		if err != nil {
			t.Fatal(err)
		}

		listed, err := RunCheckedIn(context.Background(), run, ws, []string{"jj", "config", "list", "--include-defaults", "revset-aliases", "--ignore-working-copy", "--color=never"})
		if err == nil || !strings.Contains(err.Error(), "refusing to run jj") || !strings.Contains(err.Error(), named) || !strings.Contains(err.Error(), clone) {
			t.Errorf("jj in a workspace whose .jj/repo resolves to %s = %v, want a refusal naming it and the shared clone's %s", named, err, clone)
		}
		if strings.Contains(listed.Stdout, "all()") {
			t.Errorf("jj read the alias planted in the other repository:\n%s", listed.Stdout)
		}
		if _, err := os.Stat(untouched); err != nil {
			t.Errorf("%s, outside the shared clone, was touched: %v", untouched, err)
		}
	})

	t.Run("a pushed workspace holding an unsnapshotted edit under that pointer is never removed", func(t *testing.T) {
		run := newLocalRunner(t)
		ws, err := Provision(context.Background(), run, provisionRequest(t))
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		if err := os.WriteFile(filepath.Join(ws.Dir, "feature.txt"), []byte("finished work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runSetup(t, ws.Dir, "jj", "status")
		runSetup(t, ws.Clone, "jj", "git", "push", "--remote", "origin", "--bookmark", ws.Bookmark, "--allow-empty-description")
		pointThroughALink(t, ws)
		pending := filepath.Join(ws.Dir, "pending.txt")
		if err := os.WriteFile(pending, []byte("never snapshotted\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		var logged []string
		err = RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", time.Hour, func(line string) { logged = append(logged, line) })
		if _, statErr := os.Stat(pending); statErr != nil {
			t.Fatalf("the unsnapshotted edit is gone (%v), want the workspace kept or the pass refused; RemoveFinished = %v, logged %v", statErr, err, logged)
		}
		for _, line := range logged {
			if strings.Contains(line, "removed WIDGETS-42's workspace") {
				t.Errorf("logged %q, want the workspace kept", line)
			}
		}
	})
}

// pointThroughALink rewrites ws's .jj/repo into a pointer a lexical join reads as the shared
// clone's own repository while jj, which canonicalizes it physically, opens a copy of the clone
// elsewhere: `ln/../` and then jj's own pointer, where `.jj/ln` links to a directory one level
// deeper than that pointer's `..`s climb, under a root that holds the copy. It returns the copy's
// .jj/repo, and fails the test unless a lexical join of the new pointer is the clone's own.
func pointThroughALink(t *testing.T, ws Workspace) string {
	t.Helper()
	jjDir := filepath.Join(ws.Dir, ".jj")
	written, err := os.ReadFile(filepath.Join(jjDir, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	pointer := string(written)
	if filepath.IsAbs(pointer) || !strings.HasSuffix(pointer, "/.jj/repo") {
		t.Fatalf("test setup: jj wrote .jj/repo as %q, want a relative path to a .jj/repo", pointer)
	}
	climbs := 0
	for _, segment := range strings.Split(pointer, "/") {
		if segment != ".." {
			break
		}
		climbs++
	}
	link := t.TempDir()
	for i := 0; i <= climbs; i++ {
		link = filepath.Join(link, fmt.Sprintf("d%d", i))
	}
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(filepath.Dir(link), pointer)
	copied := filepath.Dir(filepath.Dir(decoy))
	if err := os.MkdirAll(filepath.Dir(copied), 0o755); err != nil {
		t.Fatal(err)
	}
	runSetup(t, "", "cp", "-a", ws.Clone, copied)
	// jj warns once, on stderr, that a repository was copied; the next command in it is silent, as
	// in a copy a tree agent made and used before it rewrote the pointer.
	runSetup(t, copied, "jj", "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", "commit_id")
	if err := os.Symlink(link, filepath.Join(jjDir, "ln")); err != nil {
		t.Fatal(err)
	}
	crafted := "ln/../" + pointer
	if err := os.WriteFile(filepath.Join(jjDir, "repo"), []byte(crafted), 0o644); err != nil {
		t.Fatal(err)
	}
	lexical, err := filepath.EvalSymlinks(filepath.Join(jjDir, crafted))
	if err != nil {
		t.Fatal(err)
	}
	clone, err := filepath.EvalSymlinks(filepath.Join(ws.Clone, ".jj", "repo"))
	if err != nil {
		t.Fatal(err)
	}
	if lexical != clone {
		t.Fatalf("test setup: %q joined lexically is %s, want the shared clone's %s", crafted, lexical, clone)
	}
	return decoy
}

// The shared clone's own .jj/repo must be a real directory. A symlink there, to a directory
// holding a config.toml and no config-id, refuses both a command on the clone and one in a
// workspace of it, naming the symlink, and nothing in the directory it points to is removed.
func TestTheRunnerRefusesASharedCloneWhoseRepositoryIsASymlink(t *testing.T) {
	run := newLocalRunner(t)
	ws, err := Provision(context.Background(), run, provisionRequest(t))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Mkdir(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	untouched := filepath.Join(elsewhere, "config.toml")
	if err := os.WriteFile(untouched, []byte("[user]\nname = \"not the clone's\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(ws.Clone, ".jj", "repo")
	if err := os.Rename(repo, repo+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, repo); err != nil {
		t.Fatal(err)
	}
	log := []string{"jj", "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", "commit_id"}

	for name, command := range map[string]func() error{
		"a command on the clone": func() error {
			_, err := RunChecked(context.Background(), run, append(log, "-R", ws.Clone), nil, "")
			return err
		},
		"a command in a workspace of it": func() error {
			_, err := RunCheckedIn(context.Background(), run, ws, log)
			return err
		},
	} {
		if err := command(); err == nil || !strings.Contains(err.Error(), "is not the shared clone's own repository directory (a symlink)") {
			t.Errorf("%s with the clone's .jj/repo a symlink = %v, want a refusal naming the symlink", name, err)
		}
	}
	if _, err := os.Stat(untouched); err != nil {
		t.Errorf("%s, outside the shared clone, was touched: %v", untouched, err)
	}
}

// A workspace with no .jj of its own is refused, never run in: jj with no -R walks up to the
// nearest ancestor holding a .jj and opens that repository instead. Here a tree agent removed a
// pushed workspace's .jj and made its parent a repository: a jj command in the workspace is refused
// and never reads the parent's planted legacy configuration, and RemoveFinished keeps the
// workspace and the edit no jj command snapshotted, where a snapshot run against the parent's
// repository would have hidden the edit and let the workspace be removed.
func TestTheRunnerRefusesAWorkspaceWithNoJJOfItsOwn(t *testing.T) {
	run := newLocalRunner(t)
	ws, err := Provision(context.Background(), run, provisionRequest(t))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.Dir, "feature.txt"), []byte("finished work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSetup(t, ws.Dir, "jj", "status")
	runSetup(t, ws.Clone, "jj", "git", "push", "--remote", "origin", "--bookmark", ws.Bookmark, "--allow-empty-description")
	if err := os.RemoveAll(filepath.Join(ws.Dir, ".jj")); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(ws.Dir)
	runSetup(t, parent, "jj", "git", "init", "--no-colocate", ".")
	planted := filepath.Join(parent, ".jj", "workspace-config.toml")
	if err := os.Remove(filepath.Join(parent, ".jj", "workspace-config-id")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(planted, []byte("[revset-aliases]\n\"empty()\" = \"all()\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(ws.Dir, "pending.txt")
	if err := os.WriteFile(pending, []byte("never snapshotted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	listed, err := RunCheckedIn(context.Background(), run, ws, []string{"jj", "config", "list", "--include-defaults", "revset-aliases", "--ignore-working-copy", "--color=never"})
	if err == nil || !strings.Contains(err.Error(), "it has no .jj") {
		t.Errorf("jj in a workspace with no .jj below a repository = %v, want a refusal naming the missing .jj", err)
	}
	// RunCheckedIn names the workspace with -R, under which jj never walks up to the parent's
	// repository, even when the .jj goes between the runner's check and jj's start.
	calls := run.Calls()
	if last := calls[len(calls)-1].Argv; len(last) < 2 || last[len(last)-2] != "-R" || last[len(last)-1] != ws.Dir {
		t.Errorf("RunCheckedIn ran %q, want it to end with -R %s", last, ws.Dir)
	}
	if strings.Contains(listed.Stdout, "all()") {
		t.Errorf("jj read the alias planted in the parent's repository:\n%s", listed.Stdout)
	}
	if _, err := os.Stat(planted); err != nil {
		t.Errorf("the parent's planted %s was touched: %v", planted, err)
	}

	var logged []string
	err = RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", time.Hour, func(line string) { logged = append(logged, line) })
	if _, statErr := os.Stat(pending); statErr != nil {
		t.Fatalf("the unsnapshotted edit is gone (%v), want the workspace kept or the pass refused; RemoveFinished = %v, logged %v", statErr, err, logged)
	}
	for _, line := range logged {
		if strings.Contains(line, "removed WIDGETS-42's workspace") {
			t.Errorf("logged %q, want the workspace kept", line)
		}
	}
}

// A .jj that is a symlink, the shared clone's or a workspace's, is refused, never followed: jj
// would open, and the runner disarm, a directory outside the volume's layout. Each points to a copy
// of the original holding a legacy configuration file with no id beside it, and neither file is
// touched.
func TestTheRunnerRefusesASymlinkedJJ(t *testing.T) {
	log := []string{"jj", "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", "commit_id"}
	// symlinkJJ moves dir's .jj aside, copies it outside the volume and links dir/.jj to the copy,
	// whose legacy file (relative to the copied .jj) it plants with its id removed.
	symlinkJJ := func(t *testing.T, dir, id, legacy string) string {
		t.Helper()
		original := filepath.Join(dir, ".jj")
		copied := filepath.Join(t.TempDir(), "copied-jj")
		runSetup(t, "", "cp", "-a", original, copied)
		if err := os.Rename(original, original+"-moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(copied, original); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(copied, id)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		file := filepath.Join(copied, legacy)
		if err := os.WriteFile(file, []byte("[user]\nname = \"not the volume's\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return file
	}

	t.Run("the shared clone's .jj", func(t *testing.T) {
		run := newLocalRunner(t)
		ws, err := Provision(context.Background(), run, provisionRequest(t))
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		untouched := symlinkJJ(t, ws.Clone, filepath.Join("repo", "config-id"), filepath.Join("repo", "config.toml"))
		for name, command := range map[string]func() error{
			"a command on the clone": func() error {
				_, err := RunChecked(context.Background(), run, append(log, "-R", ws.Clone), nil, "")
				return err
			},
			"a command in a workspace of it": func() error {
				_, err := RunCheckedIn(context.Background(), run, ws, log)
				return err
			},
		} {
			if err := command(); err == nil || !strings.Contains(err.Error(), "(a symlink)") {
				t.Errorf("%s with the clone's .jj a symlink = %v, want a refusal naming the symlink", name, err)
			}
		}
		if _, err := os.Stat(untouched); err != nil {
			t.Errorf("%s, outside the volume, was touched: %v", untouched, err)
		}
	})

	t.Run("a workspace's .jj", func(t *testing.T) {
		run := newLocalRunner(t)
		ws, err := Provision(context.Background(), run, provisionRequest(t))
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		untouched := symlinkJJ(t, ws.Dir, "workspace-config-id", "workspace-config.toml")
		// The copy names the shared clone's repository by absolute path, so the pointer itself
		// holds: only the symlinked .jj is wrong.
		if err := os.WriteFile(filepath.Join(filepath.Dir(untouched), "repo"), []byte(filepath.Join(ws.Clone, ".jj", "repo")), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err = RunCheckedIn(context.Background(), run, ws, log)
		if err == nil || !strings.Contains(err.Error(), "its .jj is not a real directory (a symlink)") {
			t.Errorf("jj in a workspace whose .jj is a symlink = %v, want a refusal naming the symlink", err)
		}
		if _, err := os.Stat(untouched); err != nil {
			t.Errorf("%s, outside the volume, was touched: %v", untouched, err)
		}
	})
}

// A directory of the tree volume's layout above a .jj, the shared clone's own directory, the
// repos directory or a workspace's repository directory, replaced with a symlink to a copy
// outside the layout, refuses every jj command there, naming the symlink, before jj opens the copy
// or the runner removes its planted legacy file. A pushed workspace holding an edit no jj command
// snapshotted, reached through a symlinked parent, is kept, never removed. The state directory
// itself may be a symlink (TestGitWorktreeEntriesThroughASymlinkedClone).
func TestTheRunnerRefusesASymlinkedLayoutDirectory(t *testing.T) {
	log := []string{"jj", "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", "commit_id"}
	// moveBehindASymlink copies dir outside the layout, moves dir aside and links dir to the
	// copy; it returns the copy.
	moveBehindASymlink := func(t *testing.T, dir string) string {
		t.Helper()
		copied := filepath.Join(t.TempDir(), "copied")
		runSetup(t, "", "cp", "-a", dir, copied)
		if err := os.Rename(dir, dir+"-moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(copied, dir); err != nil {
			t.Fatal(err)
		}
		return copied
	}
	// plant writes a legacy configuration file in dir with its id removed, so the runner would
	// remove it were dir disarmed; it returns the file.
	plant := func(t *testing.T, dir, id, legacy string) string {
		t.Helper()
		if err := os.Remove(filepath.Join(dir, id)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		file := filepath.Join(dir, legacy)
		if err := os.WriteFile(file, []byte("[user]\nname = \"not the volume's\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return file
	}
	refused := func(t *testing.T, name string, err error, symlink string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), symlink+" is a symlink") {
			t.Errorf("%s = %v, want a refusal naming the symlink %s", name, err, symlink)
		}
	}

	for _, tc := range []struct {
		name string
		link func(ws Workspace) string
	}{
		{"the shared clone's own directory", func(ws Workspace) string { return ws.Clone }},
		{"the repos directory above it", func(ws Workspace) string {
			return filepath.Dir(filepath.Dir(filepath.Dir(ws.Clone)))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			ws, err := Provision(context.Background(), run, provisionRequest(t))
			if err != nil {
				t.Fatalf("provision: %v", err)
			}
			symlink := tc.link(ws)
			copied := moveBehindASymlink(t, symlink)
			rel, err := filepath.Rel(symlink, filepath.Join(ws.Clone, ".jj", "repo"))
			if err != nil {
				t.Fatal(err)
			}
			untouched := plant(t, filepath.Join(copied, rel), "config-id", "config.toml")
			_, err = RunChecked(context.Background(), run, append(slices.Clip(log), "-R", ws.Clone), nil, "")
			refused(t, "a command on the clone", err, symlink)
			_, err = RunCheckedIn(context.Background(), run, ws, log)
			refused(t, "a command in a workspace of it", err, symlink)
			if _, err := os.Stat(untouched); err != nil {
				t.Errorf("%s, outside the layout, was touched: %v", untouched, err)
			}
		})
	}

	t.Run("a workspace's repository directory", func(t *testing.T) {
		run := newLocalRunner(t)
		ws, err := Provision(context.Background(), run, provisionRequest(t))
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		if err := os.WriteFile(filepath.Join(ws.Dir, "feature.txt"), []byte("finished work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runSetup(t, ws.Dir, "jj", "status")
		runSetup(t, ws.Clone, "jj", "git", "push", "--remote", "origin", "--bookmark", ws.Bookmark, "--allow-empty-description")
		symlink := filepath.Dir(ws.Dir)
		copied := moveBehindASymlink(t, symlink)
		jjDir := filepath.Join(copied, filepath.Base(ws.Dir), ".jj")
		// The copy names the shared clone's repository by absolute path, so only the symlinked
		// parent is wrong.
		if err := os.WriteFile(filepath.Join(jjDir, "repo"), []byte(filepath.Join(ws.Clone, ".jj", "repo")), 0o644); err != nil {
			t.Fatal(err)
		}
		untouched := plant(t, jjDir, "workspace-config-id", "workspace-config.toml")
		pending := filepath.Join(ws.Dir, "pending.txt")
		if err := os.WriteFile(pending, []byte("never snapshotted\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err = RunCheckedIn(context.Background(), run, ws, log)
		refused(t, "a command in the workspace", err, symlink)
		if _, err := os.Stat(untouched); err != nil {
			t.Errorf("%s, outside the layout, was touched: %v", untouched, err)
		}
		before := len(run.Calls())
		err = Remove(context.Background(), run, ws)
		refused(t, "Remove", err, symlink)
		if calls := len(run.Calls()); calls != before {
			t.Errorf("Remove ran %d command(s) after the layout guard, want %d", calls, before)
		}
		if _, statErr := os.Stat(pending); statErr != nil {
			t.Fatalf("Remove touched the unsnapshotted edit (%v), want the layout guard to refuse before rename or RemoveAll", statErr)
		}

		before = len(run.Calls())
		var logged []string
		err = RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", time.Hour, func(line string) { logged = append(logged, line) })
		refused(t, "RemoveFinished", err, symlink)
		if calls := len(run.Calls()); calls != before {
			t.Errorf("RemoveFinished ran %d command(s) after the layout guard, want %d", calls, before)
		}
		if _, statErr := os.Stat(pending); statErr != nil {
			t.Fatalf("the unsnapshotted edit is gone (%v), want the workspace kept or the pass refused; RemoveFinished = %v, logged %v", statErr, err, logged)
		}
		for _, line := range logged {
			if strings.Contains(line, "removed WIDGETS-42's workspace") {
				t.Errorf("logged %q, want the workspace kept", line)
			}
		}
	})
}

// A workspace's .jj/repo that is neither a directory nor a regular file is refused before anything
// reads it: a FIFO there would otherwise block the runner, which reads .jj/repo before the
// command's deadline starts.
func TestTheRunnerRefusesAWorkspaceWhoseRepoIsNotARegularFile(t *testing.T) {
	run := newLocalRunner(t)
	ws, err := Provision(context.Background(), run, provisionRequest(t))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	fifo := filepath.Join(ws.Dir, ".jj", "repo")
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := RunCheckedIn(context.Background(), run, ws, []string{"jj", "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", "commit_id"})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "neither a directory nor a regular file") {
			t.Errorf("jj in a workspace whose .jj/repo is a FIFO = %v, want a refusal naming what it is", err)
		}
	case <-time.After(20 * time.Second):
		// Release the blocked reader so the test process can finish, then fail.
		if writer, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
			writer.Close()
		}
		t.Fatal("the runner blocked reading a FIFO at .jj/repo for 20 s")
	}
}
