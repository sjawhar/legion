package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// TestTheExecFormReleasesANarrowedGitHubAppTokenEndToEnd drives the real broker (its handlers on a
// real Postgres, through brokertest's rig), a real helper and this binary, with the broker's own
// GitHub App minter aimed at a stub GitHub installed on acme and ada. A session key the machine
// enrolled reads an agent-tier github-app secret: narrowed to ada's proofs repository, read-only,
// the command gets ada's token alone, minted with that access_tokens body; with no flag, the same
// grant gives every listed owner's owner-wide token, each minted with no body; and narrowed to an
// account github-owners does not list, nothing runs and GitHub is asked for no token.
func TestTheExecFormReleasesANarrowedGitHubAppTokenEndToEnd(t *testing.T) {
	type tokenRequest struct{ installation, body string }
	var mu sync.Mutex
	var asked []tokenRequest
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 7, "account": map[string]string{"login": "acme"}},
				{"id": 8, "account": map[string]string{"login": "ada"}},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			body, _ := io.ReadAll(r.Body)
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
			mu.Lock()
			asked = append(asked, tokenRequest{id, string(body)})
			mu.Unlock()
			token := "ghs-" + id
			if len(body) > 0 {
				token += "-narrowed"
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": token, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
		default:
			t.Errorf("unexpected request to GitHub: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(github.Close)
	take := func() []tokenRequest {
		mu.Lock()
		defer mu.Unlock()
		got := asked
		asked = nil
		return got
	}

	rig := brokertest.NewRig(t, brokertest.WithMinters(map[string]requests.Minter{
		policy.KindGitHubApp: secrets.GitHubApp{Base: github.URL, HTTP: github.Client()}.Mint,
	}))
	appKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	value, err := json.Marshal(map[string]string{
		"app_id":      "123456",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(appKey)})),
	})
	if err != nil {
		t.Fatal(err)
	}
	rig.Secrets.Put(policytest.KindSecret("APP_KEY", rig.Operator, policy.TierAgent, policy.KindGitHubApp, string(value)))
	if status, body := rig.Req(t, http.MethodPost, "/v1/secrets/APP_KEY/reread", nil, nil); status != http.StatusOK {
		t.Fatalf("reread APP_KEY = %d %s", status, body)
	}

	binary := buildAgentSecrets(t)
	_, sock := serveRealHelper(t, rig.URL, rig.Operator)
	if code, exit, stderr := machineLoginThen(t, rig, binary, sock, "approve", nil); exit != 0 {
		t.Fatalf("machine login (code %s): exit %d, stderr %q", code, exit, stderr)
	}
	boxKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&boxKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := helper.Call(sock, helper.Request{Op: "enroll-session-key", RuntimeID: "narrow-box-" + t.Name(), Thumbprint: thumbprint}, 10*time.Second)
	if err != nil || !enrolled.OK {
		t.Fatalf("enroll-session-key: %+v %v", enrolled, err)
	}
	keyDir := writeKeyDir(t, boxKey, enrolled.EnrollmentID)
	release := func(flags ...string) (map[string]mintedRelease, string, int) {
		t.Helper()
		args := append(append([]string{"APP_KEY", "--reason", "narrowing end to end"}, flags...), "--", "sh", "-c", `printf %s "$APP_KEY"`)
		stdout, stderr, exit := runAgentSecrets(t, binary, rig.URL, keyDir, nil, args...)
		if exit != 0 {
			return nil, stderr, exit
		}
		var got struct {
			Installations map[string]mintedRelease `json:"installations"`
		}
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("the command printed %q, not the release JSON: %v", stdout, err)
		}
		return got.Installations, stderr, exit
	}

	installations, stderr, exit := release("--github-owner", "Ada", "--github-repo", "proofs", "--github-read-only")
	if exit != 0 || len(installations) != 1 || installations["ada"].Token != "ghs-8-narrowed" || installations["ada"].InstallationID != 8 {
		t.Fatalf("narrowed release: exit %d, installations %+v, stderr %q; want ada's narrowed token alone", exit, installations, stderr)
	}
	tokens := take()
	if len(tokens) != 1 || tokens[0].installation != "8" {
		t.Fatalf("GitHub was asked for tokens %+v, want installation 8's alone", tokens)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(tokens[0].body), &body); err != nil {
		t.Fatalf("the access_tokens body %q: %v", tokens[0].body, err)
	}
	if want := map[string]any{"repositories": []any{"proofs"}, "permissions": map[string]any{"contents": "read", "metadata": "read"}}; !reflect.DeepEqual(body, want) {
		t.Fatalf("the access_tokens body = %v, want %v", body, want)
	}

	installations, stderr, exit = release()
	if exit != 0 || len(installations) != 2 || installations["acme"].Token != "ghs-7" || installations["ada"].Token != "ghs-8" {
		t.Fatalf("owner-wide release of the same grant: exit %d, installations %+v, stderr %q; want acme's and ada's owner-wide tokens", exit, installations, stderr)
	}
	if tokens := take(); len(tokens) != 2 || tokens[0].body != "" || tokens[1].body != "" {
		t.Fatalf("GitHub was asked for tokens %+v, want two with no body", tokens)
	}

	_, stderr, exit = release("--github-owner", "outsider", "--github-read-only")
	if exit != 1 || !strings.Contains(stderr, "MINT_FAILED") || !strings.Contains(stderr, "github-owners") {
		t.Fatalf("release narrowed to an unlisted owner: exit %d, stderr %q; want exit 1 and MINT_FAILED naming github-owners", exit, stderr)
	}
	if tokens := take(); len(tokens) != 0 {
		t.Fatalf("GitHub was asked for tokens %+v for an unlisted owner, want none", tokens)
	}
}

// mintedRelease is one installation of a github-app release.
type mintedRelease struct {
	InstallationID int64  `json:"installation_id"`
	Token          string `json:"token"`
}
