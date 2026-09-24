package workspace

import (
	"context"
	"encoding/base64"
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
// there for the next provisioning of the tree, and what provisioning must never obey: its
// credentialed clone and fetch name the token file in their environment, and in a pod every other
// step runs where the mounted provisioning Secret is readable.

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
// (post-index-change), not in any other step.
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
// fork the worker image ships has filters; stock jj has none). The credentialed fetch takes no
// snapshot of the shared clone's working copy, so a filter the tree planted there never runs with
// the token file in its environment.
func TestTheCredentialedFetchRunsNoWorkingCopyFilter(t *testing.T) {
	if out, _ := exec.Command("jj", "config", "list", "--include-defaults", "git.filter.enabled").Output(); len(strings.TrimSpace(string(out))) == 0 {
		t.Skip("this jj has no working-copy filters to plant")
	}
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
	for _, line := range nonEmptyLines(string(body)) {
		if line != "filter token-file=" {
			t.Errorf("the filter the tree planted ran inside a credentialed step: %s", line)
		}
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
// credential answers github.com alone, so the host never receives the token.
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

	err := provisionTwice(t, func(t *testing.T, _ *recordingRunner, clone string) {
		gitDir := "--git-dir=" + filepath.Join(clone, ".git")
		runSetup(t, clone, "git", gitDir, "remote", "set-url", "origin", server.URL+"/acme/widgets")
		runSetup(t, clone, "git", gitDir, "config", "http.sslVerify", "false")
	})
	if err == nil {
		t.Error("the second provision fetched from the planted host")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, authorization := range received {
		decoded, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(authorization, "Basic "))
		t.Errorf("the planted host received credentials %q", decoded)
	}
}

// The askpass git asks for the one-shot credential answers the two prompts git writes for
// github.com, and refuses every other prompt.
func TestProvisioningAskpassAnswersOnlyGitHub(t *testing.T) {
	credential, err := newProvisioningCredential(t.TempDir(), "test-installation-token")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = credential.remove() }()
	var askpass string
	for _, entry := range credential.env {
		if value, ok := strings.CutPrefix(entry, "GIT_ASKPASS="); ok {
			askpass = value
		}
	}
	for _, tc := range []struct{ prompt, answer string }{
		{"Username for 'https://github.com': ", "x-access-token\n"},
		{"Password for 'https://x-access-token@github.com': ", "test-installation-token"},
		{"Username for 'https://evil.example': ", ""},
		{"Password for 'https://x-access-token@evil.example': ", ""},
		{"Password for 'https://x-access-token@github.com.evil.example': ", ""},
		{"Password for 'https://x-access-token@github.com:8443': ", ""},
		{"Password for 'https://x-access-token@github.com/acme/widgets': ", ""},
	} {
		command := exec.Command(askpass, tc.prompt)
		command.Env = append(os.Environ(), credential.env...)
		out, err := command.Output()
		if tc.answer == "" {
			if err == nil {
				t.Errorf("askpass answered %q with %q, want a refusal", tc.prompt, out)
			}
			continue
		}
		if err != nil || string(out) != tc.answer {
			t.Errorf("askpass answered %q with %q (%v), want %q", tc.prompt, out, err, tc.answer)
		}
	}
}
