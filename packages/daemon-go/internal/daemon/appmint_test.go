package daemon

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// gitHubStandIn serves the GitHub App endpoints a boot's token mint calls, for the owner acme:
// installation discovery answers through discover, and the exchange and identity lookups succeed.
// It counts each App's installation discoveries by the App id its JWT names.
type gitHubStandIn struct {
	mu          sync.Mutex
	discoveries map[string]int
}

func (g *gitHubStandIn) Discoveries() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return maps.Clone(g.discoveries)
}

// jwtIssuer is the App id a request's App JWT names.
func jwtIssuer(t *testing.T, r *http.Request) string {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
	if len(parts) != 3 {
		t.Errorf("installation discovery without an App JWT: %q", r.Header.Get("Authorization"))
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Issuer string `json:"iss"`
	}
	if err == nil {
		err = json.Unmarshal(payload, &claims)
	}
	if err != nil {
		t.Errorf("decode the App JWT: %v", err)
	}
	return claims.Issuer
}

// appTokens is a real App token manager whose GitHub is a stand-in, the implement App id 1 and the
// review App id 2; discover answers each installation discovery, with its App and its number, from 1,
// among that App's.
func appTokens(t *testing.T, discover func(w http.ResponseWriter, r *http.Request, app string, n int)) (appauth.Tokens, *gitHubStandIn) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	standIn := &gitHubStandIn{discoveries: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations":
			app := jwtIssuer(t, r)
			standIn.mu.Lock()
			standIn.discoveries[app]++
			n := standIn.discoveries[app]
			standIn.mu.Unlock()
			discover(w, r, app, n)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"installation-token","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case r.URL.Path == "/app":
			fmt.Fprint(w, `{"slug":"legion-test"}`)
		case strings.HasPrefix(r.URL.Path, "/users/"):
			fmt.Fprint(w, `{"id":42}`)
		default:
			t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return appauth.New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "1", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "2", PrivateKey: privatePEM},
	}, appauth.Options{BaseURL: server.URL, HTTPClient: server.Client()}), standIn
}

// quickAppMints shortens a boot mint's attempt and its retry for the test, restoring them after.
func quickAppMints(t *testing.T) {
	attempt, retry := appMintAttempt, appMintRetry
	appMintAttempt, appMintRetry = 500*time.Millisecond, bootprobe.Retry{Initial: 10 * time.Millisecond, Max: 10 * time.Millisecond, Attempts: 3}
	t.Cleanup(func() { appMintAttempt, appMintRetry = attempt, retry })
}

func installed(w http.ResponseWriter) {
	fmt.Fprint(w, `[{"id":77,"account":{"login":"acme"}}]`)
}

// bootsOrExits runs the daemon until it answers /healthz or run returns, and says which.
func bootsOrExits(t *testing.T, cfg config.Config, tokens appauth.Tokens, within time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, quietLogger(), overrides{
			listen:         heldListen,
			runtime:        fakeRuntime(fake.NewRuntime(), &built{}).runtime,
			clock:          stillClock{},
			workflowTokens: tokens,
		})
	}()
	stopped := false
	t.Cleanup(func() {
		cancel()
		if stopped {
			return
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run after the test: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			stopped = true
			if err == nil {
				t.Fatal("run returned nil before the daemon answered /healthz")
			}
			return err
		default:
		}
		if response, err := pollClient.Get("http://127.0.0.1:" + strconv.Itoa(cfg.Port) + "/healthz"); err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the daemon neither answered /healthz nor exited within %s", within)
	return nil
}

// A GitHub request at boot that never answers is GitHub's trouble, not the App's: the boot mints
// again and comes up. Before, the one request ran until the boot budget ran out, and the daemon
// exited (a Stage 3 restart died exactly so: "mint implement GitHub App token at boot: GitHub App
// installation discovery: ... context deadline exceeded", #1347).
func TestABootWaitsOutAGitHubRequestThatNeverAnswers(t *testing.T) {
	quickAppMints(t)
	tokens, standIn := appTokens(t, func(w http.ResponseWriter, r *http.Request, app string, n int) {
		if app == "1" && n == 1 {
			<-r.Context().Done()
			return
		}
		installed(w)
	})
	if err := bootsOrExits(t, workflowConfig(t, workflowNATS(t)), tokens, 60*time.Second); err != nil {
		t.Fatalf("boot = %v, want it to mint again after one GitHub request that never answered", err)
	}
	if got := standIn.Discoveries(); !maps.Equal(got, map[string]int{"1": 2, "2": 1}) {
		t.Errorf("installation discoveries by App = %v, want the implement App's that never answered and one more, and the review App's one", got)
	}
}

// A definitive answer (the App's key refused) is refused at once, naming it: no retry changes it.
func TestABootRefusesADefinitiveGitHubAnswerAtOnce(t *testing.T) {
	quickAppMints(t)
	tokens, standIn := appTokens(t, func(w http.ResponseWriter, r *http.Request, _ string, _ int) {
		http.Error(w, `{"message":"A JSON web token could not be decoded"}`, http.StatusUnauthorized)
	})
	err := bootsOrExits(t, workflowConfig(t, workflowNATS(t)), tokens, 60*time.Second)
	if err == nil || !strings.Contains(err.Error(), "mint implement GitHub App token at boot") || !strings.Contains(err.Error(), "(401)") {
		t.Fatalf("boot = %v, want the refusal naming the implement mint and GitHub's 401", err)
	}
	if got := standIn.Discoveries(); !maps.Equal(got, map[string]int{"1": 1}) {
		t.Errorf("installation discoveries by App = %v, want the implement App's one: a 401 is not asked again", got)
	}
}

// Both Apps' tokens share one retry budget, so a boot waits at most appMintRetry before it refuses,
// however the failures fall between the two mints. Here the implement App's discovery fails twice
// and then answers, and the review App's never does: the budget's three attempts are spent before
// the review token is asked for more than once. With a budget per App the boot would wait both out,
// twice as long, which is what outlasted Stage 3's wait for /healthz.
func TestABootGivesBothAppTokensOneRetryBudget(t *testing.T) {
	quickAppMints(t)
	tokens, standIn := appTokens(t, func(w http.ResponseWriter, r *http.Request, app string, n int) {
		if app == "1" && n > 2 {
			installed(w)
			return
		}
		http.Error(w, "{}", http.StatusBadGateway)
	})
	err := bootsOrExits(t, workflowConfig(t, workflowNATS(t)), tokens, 60*time.Second)
	if err == nil || !strings.Contains(err.Error(), "retry budget (3 attempts)") || !strings.Contains(err.Error(), "mint review GitHub App token at boot") {
		t.Fatalf("boot = %v, want the refusal after the one budget's three attempts, naming the review mint", err)
	}
	if got := standIn.Discoveries(); !maps.Equal(got, map[string]int{"1": 3, "2": 1}) {
		t.Errorf("installation discoveries by App = %v, want the implement App's three and the review App's one", got)
	}
}
