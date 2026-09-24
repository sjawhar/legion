package modelroute

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests run the real Oh My Pi on the profile Install writes, against a stand-in for the
// gateway, because only the real binary shows how Oh My Pi routes, keys and shapes a request, and
// what it does when the key fails. LEGION_TEST_OMP names the binary (the fork pin in
// packages/daemon/src/daemon/omp-pin.ts: CI installs it; on the devbox,
// `mise where github:sjawhar/oh-my-pi@<pin>`/bin/omp). A run without one skips, except on GitHub
// Actions, where a skip would hide the only check of the route on the binary the image ships.
// (GITHUB_ACTIONS, not CI: agent harnesses on the devbox export CI=true.)
func realOmp(t *testing.T) string {
	t.Helper()
	omp := os.Getenv("LEGION_TEST_OMP")
	switch {
	case omp != "":
		return omp
	case os.Getenv("GITHUB_ACTIONS") == "true":
		t.Fatal("LEGION_TEST_OMP is unset on GitHub Actions: name the pinned Oh My Pi binary (the daemon-go job installs it)")
	}
	t.Skip("LEGION_TEST_OMP names no Oh My Pi binary")
	return ""
}

// gateway is a stand-in for the model gateway's /anthropic route: it answers a Messages request
// keyed with its token for a `-legion` alias with one streamed text reply, refuses any other key
// (401) or model (404) as the gateway does, and records every request it saw.
type gateway struct {
	*httptest.Server
	token string

	mu       sync.Mutex
	requests []request
}

type request struct {
	path   string
	header http.Header
	body   map[string]any
}

func newGateway(t *testing.T, token string) *gateway {
	t.Helper()
	g := &gateway{token: token}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.Close)
	return g
}

func (g *gateway) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	g.mu.Lock()
	g.requests = append(g.requests, request{r.URL.Path, r.Header.Clone(), body})
	g.mu.Unlock()
	model, _ := body["model"].(string)
	switch {
	case r.Header.Get("X-Api-Key") != g.token:
		http.Error(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`, http.StatusUnauthorized)
		return
	case !strings.HasSuffix(model, "-legion"):
		http.Error(w, `{"type":"error","error":{"type":"not_found_error","message":"model not found"}}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range []struct {
		name string
		data any
	}{
		{"message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": "msg_gateway", "type": "message", "role": "assistant", "model": model, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 5, "output_tokens": 1},
		}}},
		{"content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}},
		{"content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "ok"}}},
		{"content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}},
		{"message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 1}}},
		{"message_stop", map[string]any{"type": "message_stop"}},
	} {
		data, _ := json.Marshal(event.data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.name, data)
	}
}

func (g *gateway) seen() []request {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]request(nil), g.requests...)
}

// pod is one Oh My Pi environment: a HOME whose profile Install routed through gateway, keyed by
// the token file Install names. The HOME is shared by a test's subtests, each in its own profile,
// so Oh My Pi unpacks its native modules there once.
type pod struct {
	home, profile, tokenFile string
	// dir is the working directory Oh My Pi starts in, and env its environment: a pod's, as the
	// worker shim hands it to its child.
	dir string
	env []string
}

func routed(t *testing.T, home, profile, gatewayURL string) pod {
	t.Helper()
	p := pod{home: home, profile: profile, tokenFile: filepath.Join(t.TempDir(), "token"), dir: home}
	env := map[string]string{"HOME": home, "OMP_PROFILE": profile, EnvURL: gatewayURL}
	installed, err := InstallKeyedBy(lookup(env), p.tokenFile)
	if err != nil || installed.Route == "" {
		t.Fatalf("install = %+v, %v", installed, err)
	}
	// As the worker shim hands the environment to its child: with the pins as a settings overlay.
	p.env = installed.Environ([]string{"HOME=" + home, "OMP_PROFILE=" + profile, "PATH=/usr/local/bin:/usr/bin:/bin"})
	return p
}

// run runs Oh My Pi in the pod with args, in its working directory and environment.
func (p pod) run(t *testing.T, omp string, args ...string) (stdout, stderr string, exit int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, omp, args...)
	cmd.Env = p.env
	cmd.Dir = p.dir
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("%s %v did not finish in 3 minutes: %s", omp, args, errOut.String())
	}
	if _, isExit := err.(*exec.ExitError); err != nil && !isExit {
		t.Fatalf("run %s: %v", omp, err)
	}
	return out.String(), errOut.String(), cmd.ProcessState.ExitCode()
}

// turn is one print-mode turn on the profile's default role, as the image probe runs it.
func (p pod) turn(t *testing.T, omp string) (answers []map[string]any, stderr string, exit int) {
	t.Helper()
	stdout, stderr, exit := p.run(t, omp, "-p", "--mode", "json", "--no-session", "--no-tools", "--no-extensions",
		"--no-skills", "--no-rules", "--no-lsp", "--no-title", "Reply with the single word ok.")
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		var event struct {
			Type    string         `json:"type"`
			Message map[string]any `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Type == "message_end" && event.Message["role"] == "assistant" {
			answers = append(answers, event.Message)
		}
	}
	return answers, stderr, exit
}

func TestTheRouteOnTheRealOhMyPi(t *testing.T) {
	omp := realOmp(t)
	home := t.TempDir()
	provider, model, _ := strings.Cut(DefaultModel, "/")

	// A turn reaches the gateway, and only the gateway: on the default role's alias, keyed by the
	// token file as x-api-key, as a plain API-key caller — never in Oh My Pi's Claude Code OAuth
	// shape, which middleman does not speak.
	t.Run("a turn goes to the gateway as an API-key caller", func(t *testing.T) {
		gw := newGateway(t, "gateway-token-1")
		p := routed(t, home, "turn", gw.URL)
		if err := os.WriteFile(p.tokenFile, []byte("gateway-token-1\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		answers, stderr, exit := p.turn(t, omp)

		if exit != 0 || len(answers) != 1 || answers[0]["provider"] != provider || answers[0]["model"] != model || answers[0]["stopReason"] != "stop" {
			t.Fatalf("the turn exited %d with answers %v, want one from %s; stderr:\n%s", exit, answers, DefaultModel, stderr)
		}
		// Oh My Pi also lists the provider's models (GET /anthropic/v1/models) before the turn;
		// every request, that one included, goes to the gateway with the token.
		var messages []request
		for _, r := range gw.seen() {
			if r.header.Get("X-Api-Key") != "gateway-token-1" {
				t.Errorf("a request to %s carries another x-api-key than the token file's value", r.path)
			}
			if r.path == "/anthropic/v1/messages" {
				messages = append(messages, r)
			}
		}
		if len(messages) != 1 {
			t.Fatalf("the gateway saw %d Messages requests, want the turn's one", len(messages))
		}
		r := messages[0]
		if r.body["model"] != model {
			t.Errorf("the request names model %v, want %s", r.body["model"], model)
		}
		if agent := r.header.Get("User-Agent"); strings.HasPrefix(agent, "claude-cli") {
			t.Errorf("the request's user agent is Claude Code's (%s): Oh My Pi sent its OAuth shape", agent)
		}
		if betas := r.header.Get("Anthropic-Beta"); strings.Contains(betas, "oauth-") {
			t.Errorf("the request carries the oauth beta (%s)", betas)
		}
		if system, _ := json.Marshal(r.body["system"]); bytes.Contains(system, []byte("x-anthropic-billing-header")) {
			t.Errorf("the request's system prompt carries Claude Code's billing header")
		}
	})

	// With the key command failing — no token volume — Oh My Pi exits naming the provider it has
	// no key for, and answers from nothing: never from another provider (the devbox's instance
	// role would otherwise have Amazon Bedrock answer, exit 0).
	t.Run("a key failure is a named exit", func(t *testing.T) {
		gw := newGateway(t, "gateway-token-2")
		p := routed(t, home, "nokey", gw.URL)

		answers, stderr, exit := p.turn(t, omp)

		if exit == 0 || len(answers) != 0 {
			t.Fatalf("the turn exited %d with answers %v, want a refusal and no answer", exit, answers)
		}
		if want := "No API key found for anthropic"; !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, stderr)
		}
		if n := len(gw.seen()); n != 0 {
			t.Errorf("the gateway saw %d requests without a key", n)
		}
	})

	// Each alias carries the catalog metadata of the model it names, so a pod's context window,
	// output budget, thinking levels, image input and cost accounting are the model's own.
	t.Run("each alias is its model", func(t *testing.T) {
		gw := newGateway(t, "gateway-token-3")
		p := routed(t, home, "catalog", gw.URL)
		if err := os.WriteFile(p.tokenFile, []byte("gateway-token-3\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		stdout, stderr, exit := p.run(t, omp, "models", "--json")

		var listed struct {
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal([]byte(stdout), &listed); exit != 0 || err != nil {
			t.Fatalf("omp models --json exited %d (%v): %s", exit, err, stderr)
		}
		rows := map[string]map[string]any{}
		for _, row := range listed.Models {
			if row["provider"] == provider {
				rows[row["id"].(string)] = row
			}
		}
		aliases := 0
		for id, alias := range rows {
			bare, isAlias := strings.CutSuffix(id, "-legion")
			if !isAlias {
				continue
			}
			aliases++
			original, ok := rows[bare]
			if !ok {
				t.Errorf("%s names %s, which the catalog does not hold", id, bare)
				continue
			}
			for _, field := range []string{"contextWindow", "maxTokens", "reasoning", "thinking", "input", "cost"} {
				got, _ := json.Marshal(alias[field])
				want, _ := json.Marshal(original[field])
				if !bytes.Equal(got, want) {
					t.Errorf("%s %s = %s, want %s's %s", id, field, got, bare, want)
				}
			}
		}
		if aliases != 5 {
			t.Errorf("omp lists %d -legion aliases, want the gateway's five", aliases)
		}
	})
	// A repository's own Oh My Pi settings (.omp/config.yml in the agent's working directory) sit
	// above the profile's in Oh My Pi's precedence, and replace a list rather than merge it: one that
	// empties disabledProviders and enables Amazon Bedrock would bring Bedrock back to a worker with
	// ambient AWS credentials. The pins still hold: only the gateway's aliases are listed, and the
	// turn is answered through the gateway.
	t.Run("a repository's own settings cannot unpin the profile", func(t *testing.T) {
		gw := newGateway(t, "gateway-token-4")
		p := routed(t, home, "repository", gw.URL)
		if err := os.WriteFile(p.tokenFile, []byte("gateway-token-4\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		p.dir = t.TempDir()
		if err := os.MkdirAll(filepath.Join(p.dir, ".omp"), 0o755); err != nil {
			t.Fatal(err)
		}
		settings := "enabledModels:\n  - amazon-bedrock/*\n  - anthropic/*\ndisabledProviders: []\nmodelRoles:\n  default: amazon-bedrock/us.anthropic.claude-opus-4-8\n"
		if err := os.WriteFile(filepath.Join(p.dir, ".omp", "config.yml"), []byte(settings), 0o644); err != nil {
			t.Fatal(err)
		}
		// Ambient AWS credentials, as a node's instance role would supply: they make Bedrock usable.
		p.env = append(p.env, "AWS_ACCESS_KEY_ID=AKIAEXAMPLE", "AWS_SECRET_ACCESS_KEY=example", "AWS_REGION=us-east-1")

		stdout, stderr, exit := p.run(t, omp, "models", "--json")

		var listed struct {
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal([]byte(stdout), &listed); exit != 0 || err != nil {
			t.Fatalf("omp models --json exited %d (%v): %s", exit, err, stderr)
		}
		// Every anthropic model goes through the gateway's baseUrl (enabledModels holds sessions to
		// its aliases); any other provider is a route past it.
		unpinned := map[string]int{}
		for _, row := range listed.Models {
			if row["provider"] != provider {
				unpinned[fmt.Sprint(row["provider"])]++
			}
		}
		if len(unpinned) > 0 {
			t.Errorf("a repository's settings put models past the gateway in reach, by provider: %v", unpinned)
		}
		answers, stderr, exit := p.turn(t, omp)
		if exit != 0 || len(answers) != 1 || answers[0]["provider"] != provider || answers[0]["model"] != model {
			t.Errorf("the turn exited %d with answers %v, want one from %s through the gateway; stderr:\n%s", exit, answers, DefaultModel, stderr)
		}
	})
}
