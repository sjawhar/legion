package embed

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEntra answers Entra's token endpoint with "token-<n>" for its n-th exchange and records
// every form it received. expiresIn is sent verbatim as expires_in's JSON value.
type fakeEntra struct {
	expiresIn string
	delay     time.Duration

	hits  atomic.Int32
	mu    sync.Mutex
	forms []map[string][]string
	paths []string
}

func (f *fakeEntra) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := f.hits.Add(1)
	_ = r.ParseForm()
	f.mu.Lock()
	f.forms = append(f.forms, r.PostForm)
	f.paths = append(f.paths, r.Method+" "+r.URL.Path+" "+r.Header.Get("Content-Type"))
	f.mu.Unlock()
	time.Sleep(f.delay)
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"token_type":"Bearer","expires_in":%s,"access_token":"token-%d"}`, f.expiresIn, n)
}

func (f *fakeEntra) form(i int) map[string][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.forms[i]
}

// testEntra starts fake behind a test server and returns a token source pointed at it, reading
// assertion from a token file the test may rewrite, on a clock the test moves.
func testEntra(t *testing.T, fake http.Handler, assertion string) (*EntraTokenSource, string, *time.Time) {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(assertion), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	source, err := NewEntraTokenSource(EntraConfig{
		TenantID:   "test-tenant",
		ClientID:   "test-client",
		TokenFile:  tokenFile,
		Authority:  server.URL + "/",
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("NewEntraTokenSource: %v", err)
	}
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	source.now = func() time.Time { return now }
	return source, tokenFile, &now
}

func TestEntraTokenSourceSendsTheClientCredentialsForm(t *testing.T) {
	fake := &fakeEntra{expiresIn: "3599"}
	source, _, _ := testEntra(t, fake, "federated-assertion\n")
	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "token-1" {
		t.Errorf("Token() = %q, want token-1", token)
	}
	if want := "POST /test-tenant/oauth2/v2.0/token application/x-www-form-urlencoded"; fake.paths[0] != want {
		t.Errorf("request = %q, want %q", fake.paths[0], want)
	}
	want := map[string]string{
		"grant_type":            "client_credentials",
		"client_id":             "test-client",
		"client_assertion_type": "urn:ietf:params:oauth:client-assertion-type:jwt-bearer",
		"client_assertion":      "federated-assertion",
		"scope":                 "https://cognitiveservices.azure.com/.default",
	}
	form := fake.form(0)
	if len(form) != len(want) {
		t.Errorf("form = %v, want exactly the fields %v", form, want)
	}
	for field, value := range want {
		if got := form[field]; len(got) != 1 || got[0] != value {
			t.Errorf("form %s = %v, want [%s]", field, got, value)
		}
	}
}

// TestEntraTokenSourceReusesItsTokenUntilFiveMinutesBeforeExpiry moves the clock to just inside
// and then exactly at the refresh point, with expires_in sent as a string.
func TestEntraTokenSourceReusesItsTokenUntilFiveMinutesBeforeExpiry(t *testing.T) {
	fake := &fakeEntra{expiresIn: `"3600"`}
	source, _, now := testEntra(t, fake, "assertion")
	ctx := context.Background()
	start := *now
	for _, step := range []struct {
		at   time.Duration
		want string
	}{
		{0, "token-1"},
		{time.Minute, "token-1"},
		{55*time.Minute - time.Second, "token-1"},
		{55 * time.Minute, "token-2"},
		{56 * time.Minute, "token-2"},
	} {
		*now = start.Add(step.at)
		got, err := source.Token(ctx)
		if err != nil {
			t.Fatalf("Token at +%v: %v", step.at, err)
		}
		if got != step.want {
			t.Errorf("Token at +%v = %q, want %q", step.at, got, step.want)
		}
	}
	if hits := fake.hits.Load(); hits != 2 {
		t.Errorf("exchanges = %d, want 2 (one at start, one at the refresh point)", hits)
	}
}

func TestEntraTokenSourceRereadsTheTokenFileOnEveryExchange(t *testing.T) {
	fake := &fakeEntra{expiresIn: "3600"}
	source, tokenFile, now := testEntra(t, fake, "assertion-one")
	ctx := context.Background()
	if _, err := source.Token(ctx); err != nil {
		t.Fatalf("first Token: %v", err)
	}
	if err := os.WriteFile(tokenFile, []byte("  assertion-two\n"), 0o600); err != nil {
		t.Fatalf("rewrite token file: %v", err)
	}
	*now = now.Add(time.Hour)
	if _, err := source.Token(ctx); err != nil {
		t.Fatalf("second Token: %v", err)
	}
	for i, want := range []string{"assertion-one", "assertion-two"} {
		if got := fake.form(i)["client_assertion"]; len(got) != 1 || got[0] != want {
			t.Errorf("exchange %d client_assertion = %v, want [%s]", i+1, got, want)
		}
	}
}

// TestEntraTokenSourceRunsOneExchangeForConcurrentCallers holds the one exchange open while every
// caller arrives: they wait for it and share its token rather than each signing in.
func TestEntraTokenSourceRunsOneExchangeForConcurrentCallers(t *testing.T) {
	fake := &fakeEntra{expiresIn: "3600", delay: 100 * time.Millisecond}
	source, _, _ := testEntra(t, fake, "assertion")
	const callers = 16
	tokens := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() { tokens[i], errs[i] = source.Token(context.Background()) })
	}
	wg.Wait()
	for i := range callers {
		if errs[i] != nil || tokens[i] != "token-1" {
			t.Errorf("caller %d: Token() = %q, %v; want token-1", i, tokens[i], errs[i])
		}
	}
	if hits := fake.hits.Load(); hits != 1 {
		t.Errorf("exchanges = %d, want 1", hits)
	}
}

func TestEntraTokenSourceStopsWaitingWhenItsContextEnds(t *testing.T) {
	fake := &fakeEntra{expiresIn: "3600", delay: 300 * time.Millisecond}
	source, _, _ := testEntra(t, fake, "assertion")
	go func() { _, _ = source.Token(context.Background()) }()
	for fake.hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := source.Token(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Token while another exchange runs, context ending = %v, want context.DeadlineExceeded", err)
	}
}

// TestEntraFailuresArePlainErrors: an Entra refusal, even a 429, names the status and Entra's
// error field and is never a throttle; it never carries the assertion.
func TestEntraFailuresArePlainErrors(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			source, _, _ := testEntra(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"AADSTS70021: no match for secret-assertion"}`))
			}), "secret-assertion")
			_, err := source.Token(context.Background())
			if err == nil {
				t.Fatal("Token: want an error, got nil")
			}
			text := err.Error()
			if !strings.Contains(text, http.StatusText(status)) || !strings.Contains(text, "invalid_client") {
				t.Errorf("error %q does not name status %d and Entra's error", text, status)
			}
			if strings.Contains(text, "secret-assertion") {
				t.Errorf("error %q carries the assertion", text)
			}
			if IsThrottled(err) {
				t.Errorf("IsThrottled(%v) = true, want false", err)
			}
		})
	}
}

// TestEntraFailureReachesAzureEmbedAsANonThrottle wires the token source into an AzureClient: a
// refused sign-in fails the batch as ordinary infrastructure, not a throttle, and Azure is never
// called without a token.
func TestEntraFailureReachesAzureEmbedAsANonThrottle(t *testing.T) {
	source, _, _ := testEntra(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"temporarily_unavailable"}`))
	}), "assertion")
	var azureCalls atomic.Int32
	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { azureCalls.Add(1) }))
	defer azure.Close()
	client := testAzureClient(t, azure, AzureConfig{Tokens: source})
	_, err := client.Embed(context.Background(), []string{"x"}, InputDocument)
	if err == nil || !strings.Contains(err.Error(), "temporarily_unavailable") || IsThrottled(err) {
		t.Fatalf("Embed error = %v, want Entra's failure, not throttled", err)
	}
	if azureCalls.Load() != 0 {
		t.Error("Embed called Azure after sign-in failed")
	}
}

func TestEntraTokenSourceRefusesAnUnreadableOrEmptyTokenFile(t *testing.T) {
	fake := &fakeEntra{expiresIn: "3600"}
	source, tokenFile, _ := testEntra(t, fake, " \n")
	if _, err := source.Token(context.Background()); err == nil || IsThrottled(err) {
		t.Errorf("Token with an empty token file = %v, want a plain error", err)
	}
	if err := os.Remove(tokenFile); err != nil {
		t.Fatalf("remove token file: %v", err)
	}
	if _, err := source.Token(context.Background()); err == nil || IsThrottled(err) {
		t.Errorf("Token with no token file = %v, want a plain error", err)
	}
	if hits := fake.hits.Load(); hits != 0 {
		t.Errorf("exchanges = %d, want 0: nothing to assert, nothing to send", hits)
	}
}

func TestEntraTokenSourceRefusesAnAnswerWithoutALifetime(t *testing.T) {
	for _, expiresIn := range []string{`null`, `"soon"`, `0`, `-5`} {
		fake := &fakeEntra{expiresIn: expiresIn}
		source, _, _ := testEntra(t, fake, "assertion")
		_, err := source.Token(context.Background())
		if err == nil || strings.Contains(err.Error(), "token-1") {
			t.Errorf("Token with expires_in %s = %v, want an error that does not carry the token", expiresIn, err)
		}
	}
}

func TestNewEntraTokenSourceRefusesAMissingSetting(t *testing.T) {
	valid := EntraConfig{TenantID: "t", ClientID: "c", TokenFile: "/var/run/token"}
	if _, err := NewEntraTokenSource(valid); err != nil {
		t.Fatalf("NewEntraTokenSource(a valid config): %v", err)
	}
	for name, change := range map[string]func(*EntraConfig){
		"no tenant":          func(c *EntraConfig) { c.TenantID = "" },
		"no client":          func(c *EntraConfig) { c.ClientID = "" },
		"no token file":      func(c *EntraConfig) { c.TokenFile = "" },
		"relative authority": func(c *EntraConfig) { c.Authority = "login.example" },
	} {
		cfg := valid
		change(&cfg)
		if _, err := NewEntraTokenSource(cfg); err == nil {
			t.Errorf("NewEntraTokenSource(%s): want an error, got nil", name)
		}
	}
}
