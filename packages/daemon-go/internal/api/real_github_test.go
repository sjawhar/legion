package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/record"
)

const realGitHubProofEnabled = "LEGION_REAL_GITHUB"

// TestRealGitHubCredentialSurface keeps the Stage 3 credential proof beside the real API it
// drives. It is intentionally gated: CI has no Legion App keys, whereas the devbox command is a
// release gate and Task 3.14 reuses the same proof rather than rebuilding an ad-hoc rig.
func TestRealGitHubCredentialSurface(t *testing.T) {
	if os.Getenv(realGitHubProofEnabled) != "1" {
		t.Skip("set LEGION_REAL_GITHUB=1 to run the real GitHub App credential proof")
	}
	apps, err := config.ResolveGitHubApps(config.GitHubApps{
		Implement: config.GitHubApp{
			AppID: "3202636", PrivateKeyCommand: privateKeyCommand("LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64"),
		},
		Review: config.GitHubApp{
			AppID: "3202653", PrivateKeyCommand: privateKeyCommand("GH_REVIEW_APP_PRIVATE_KEY_B64"),
		},
	})
	if err != nil {
		t.Fatalf("resolve the GitHub App private-key commands: %v", err)
	}

	h := newHarness(t)
	tokens := appauth.New(apps, appauth.Options{})
	h.handler = NewServer("127.0.0.1", 0, Options{
		Supervisor:  h.supervisor,
		BootTokens:  h.tokens,
		Project:     testProject,
		Tokens:      tokens,
		GitHubOwner: "sjawhar",
		Grants:      credential.New(nil),
		Pool:        h.store.Pool(),
		Record:      record.NewStore(),
	}).Handler
	server := httptest.NewServer(h.handler)
	defer server.Close()

	binary := buildLegionForRealProof(t)
	implementer := registerRealClaim(t, h, server.URL, claim.RoleImplementer)
	reviewer := registerRealClaim(t, h, server.URL, claim.RoleReviewer)

	implementerLogin := runRealGH(t, binary, server.URL, mintRealGrant(t, server.URL, implementer),
		"api", "graphql", "-f", "query={viewer{login}}")
	if !strings.Contains(implementerLogin.stdout, "legion-implementer[bot]") {
		t.Fatalf("implementer viewer login = %q; stderr %q", implementerLogin.stdout, implementerLogin.stderr)
	}
	t.Logf("implementer GraphQL login: %s", strings.TrimSpace(implementerLogin.stdout))

	reviewerLogin := runRealGH(t, binary, server.URL, mintRealGrant(t, server.URL, reviewer),
		"api", "graphql", "-f", "query={viewer{login}}")
	if !strings.Contains(reviewerLogin.stdout, "legion-reviewer[bot]") {
		t.Fatalf("reviewer viewer login = %q; stderr %q", reviewerLogin.stdout, reviewerLogin.stderr)
	}
	t.Logf("reviewer GraphQL login: %s", strings.TrimSpace(reviewerLogin.stdout))

	listed := runRealGH(t, binary, server.URL, mintRealGrant(t, server.URL, implementer),
		"pr", "list", "--repo", "sjawhar/legion-smoke")
	if listed.code != 0 {
		t.Fatalf("implementer pr list exit %d: %s", listed.code, listed.stderr)
	}

	refused := runRealGH(t, binary, server.URL, "unused-refused-merge-grant",
		"pr", "merge", "1", "--repo", "sjawhar/legion-smoke")
	if refused.code == 0 || !strings.Contains(refused.stderr, "Legion never merges a pull request") {
		t.Fatalf("merge refusal exit %d, stdout %q, stderr %q", refused.code, refused.stdout, refused.stderr)
	}
	t.Logf("refused merge: exit=%d %s", refused.code, strings.TrimSpace(refused.stderr))

	cloneGrant := mintRealGrant(t, server.URL, implementer)
	grantFile := writeRealGrant(t, cloneGrant)
	clone := exec.Command("git", "-c", "credential.helper=", "-c", "credential.helper=!"+binary+" credential",
		"clone", "https://github.com/sjawhar/legion-smoke", filepath.Join(t.TempDir(), "legion-smoke"))
	clone.Env = append(realCLIEnvironment(server.URL, grantFile), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	output, err := clone.CombinedOutput()
	if err != nil {
		t.Fatalf("clone through legion credential helper: %v: %s", err, strings.TrimSpace(string(output)))
	}
	t.Log("clone through legion credential helper: succeeded")
}

type realClaim struct {
	session string
	secret  string
	issue   string
}

func privateKeyCommand(key string) string {
	// secretsd stores the reviewer key in Node-compatible base64: unpadded and with one trailing
	// dangling sextet. Normalize exactly as config.decodeBase64LikeNode before POSIX base64 decodes
	// it; the key remains in this daemon-owned child process.
	return "secrets " + key + ` -- sh -c 'value=$(printf %s "${` + key + `}" | tr "_-" "/+"); case $((${#value} % 4)) in 1) value=${value%?};; esac; case $((${#value} % 4)) in 2) value="${value}==";; 3) value="${value}=";; esac; printf %s "$value" | base64 -d'`
}

func registerRealClaim(t *testing.T, h *harness, baseURL string, role claim.Role) realClaim {
	t.Helper()
	issue := "LEGION-208"
	_, boot := h.launch(issue, role)
	session := "real_" + string(role)
	body, err := json.Marshal(claim.RegisterRequest{
		BootToken: boot, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: "agent-" + session, PluginContract: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(baseURL+"/legion/v1/claims/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("register %s claim: %v", role, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(response.Body)
		t.Fatalf("register %s claim = %d: %s", role, response.StatusCode, strings.TrimSpace(string(payload)))
	}
	var registered claim.RegisterResponse
	if err := json.NewDecoder(response.Body).Decode(&registered); err != nil {
		t.Fatalf("decode %s registration: %v", role, err)
	}
	return realClaim{session: session, secret: registered.Secret, issue: issue}
}

func mintRealGrant(t *testing.T, baseURL string, c realClaim) string {
	t.Helper()
	body, err := json.Marshal(GrantRequest{SessionID: c.session, Secret: c.secret, Tree: c.issue, Issue: c.issue})
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(baseURL+"/legion/v1/grants", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("mint grant: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(response.Body)
		t.Fatalf("mint grant = %d: %s", response.StatusCode, strings.TrimSpace(string(payload)))
	}
	var grant GrantResponse
	if err := json.NewDecoder(response.Body).Decode(&grant); err != nil || grant.GrantID == "" {
		t.Fatalf("decode grant: %v", err)
	}
	return grant.GrantID
}

type realCommandResult struct {
	stdout string
	stderr string
	code   int
}

func runRealGH(t *testing.T, binary, daemonURL, grant string, args ...string) realCommandResult {
	t.Helper()
	grantFile := writeRealGrant(t, grant)
	command := exec.Command(binary, append([]string{"gh", "--"}, args...)...)
	command.Env = realCLIEnvironment(daemonURL, grantFile)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	result := realCommandResult{stdout: stdout.String(), stderr: stderr.String()}
	if err == nil {
		return result
	}
	if exit, ok := err.(*exec.ExitError); ok {
		result.code = exit.ExitCode()
		return result
	}
	t.Fatalf("run legion gh %s: %v", strings.Join(args, " "), err)
	return realCommandResult{}
}

func writeRealGrant(t *testing.T, grant string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grant")
	if err := os.WriteFile(path, []byte(grant+"\n"), 0o600); err != nil {
		t.Fatalf("write grant file: %v", err)
	}
	return path
}

func realCLIEnvironment(daemonURL, grantFile string) []string {
	// legion gh runs the gh a daemon names on its panes; this proof names the one it would resolve.
	gh := os.Getenv("LEGION_GH_PATH")
	if gh == "" {
		gh, _ = exec.LookPath("gh")
	}
	return append(os.Environ(), "LEGION_DAEMON_URL="+daemonURL, "LEGION_GRANT_FILE="+grantFile, "LEGION_GH_PATH="+gh,
		"LEGION_STATE_DIR="+filepath.Dir(grantFile), "GH_CONFIG_DIR="+filepath.Join(filepath.Dir(grantFile), "gh"))
}

func buildLegionForRealProof(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "legion")
	command := exec.Command("go", "build", "-o", binary, "./cmd/legion")
	command.Dir = filepath.Join(root, "packages", "daemon-go")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build legion binary: %v: %s", err, strings.TrimSpace(string(output)))
	}
	return binary
}
