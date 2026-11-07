package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/goingtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
)

// azureChatScope is the scope this deployment configures for its answerer, which is not the one
// the embedder asks for (embed.DefaultEntraScope): one Azure AI Foundry resource serves inference
// families that accept different scopes, so the answerer's is configuration and the embedder's
// default proves nothing about it.
const azureChatScope = "https://ai.azure.com/.default"

// azureChatAccessToken is the bearer the local authority issues, which the chat call must then
// present: it is what says the call was signed by the sign-in these settings describe.
const azureChatAccessToken = "chat-access-token"

// entraAuthorityHost is Entra's host, the one newAnswerer's token source signs in at;
// serveHostsLocally stands a test server in for it.
const entraAuthorityHost = "login.microsoftonline.com"

// azureChatEndpointHost is the host azureChatEnvironment's DISPATCH_AZURE_CHAT_ENDPOINT names.
const azureChatEndpointHost = "chat.example"

// signInRecorder is Entra's token endpoint as a local server: it answers one bearer and keeps
// every form it was sent, the scope among them.
type signInRecorder struct {
	mu    sync.Mutex
	forms []url.Values
}

func (s *signInRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	s.mu.Lock()
	s.forms = append(s.forms, r.PostForm)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"token_type":"Bearer","expires_in":3599,"access_token":%q}`, azureChatAccessToken)
}

// exchange is the form of the i-th sign-in, and fails the test when there was none.
func (s *signInRecorder) exchange(t *testing.T, i int) url.Values {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.forms) {
		t.Fatalf("the authority served %d sign-ins, want at least %d: the answerer never signed in", len(s.forms), i+1)
	}
	return s.forms[i]
}

// localHosts sends each named host's calls to the local server standing in for it, and refuses
// every other host, so a test that reached a service would fail rather than call one.
type localHosts struct{ byHost map[string]*url.URL }

func (h localHosts) RoundTrip(request *http.Request) (*http.Response, error) {
	local, served := h.byHost[request.URL.Host]
	if !served {
		return nil, fmt.Errorf("no local server stands in for %s: a test calls no service", request.URL.Host)
	}
	routed := request.Clone(request.Context())
	routed.URL.Scheme, routed.URL.Host = local.Scheme, local.Host
	return http.DefaultTransport.RoundTrip(routed)
}

// serveHostsLocally points http.DefaultClient at the local servers standing in for byHost's hosts
// for the rest of the test, and puts its transport back afterwards.
//
// It is the one client because it is the one newAnswerer's chat client and its token source take:
// neither going.NewHTTPClient nor embed.NewEntraTokenSource is handed a client there, and both
// fall back to http.DefaultClient, so this is how the sign-in a deployment's settings describe is
// driven without a test-only seam in the boot path. No test in this package runs in parallel.
func serveHostsLocally(t *testing.T, byHost map[string]string) {
	t.Helper()
	routes := make(map[string]*url.URL, len(byHost))
	for host, local := range byHost {
		parsed, err := url.Parse(local)
		if err != nil {
			t.Fatalf("parse the local server %q for %s: %v", local, host, err)
		}
		routes[host] = parsed
	}
	previous := http.DefaultClient.Transport
	http.DefaultClient.Transport = localHosts{byHost: routes}
	t.Cleanup(func() { http.DefaultClient.Transport = previous })
}

// A deployment that embeds on Bedrock and answers on Azure is one deployment: the two places are
// settled apart, the shared sign-in belongs to whichever of them is azure, and here that is the
// answerer. The run below is what nothing else asserts end to end from the settings: the
// answerer's sign-in asks for DISPATCH_AZURE_CHAT_SCOPE, not the embedder's
// embed.DefaultEntraScope, and the chat call it signs carries that sign-in's bearer.
//
// The embedder's own rows stand refused beside it - DISPATCH_AZURE_EMBED_* is read only with
// DISPATCH_EMBEDDER=azure - which TestResolveBootConfigRefusesEveryEmbedderShapeItCannotUse holds
// for every one of them, under the default and under bedrock alike.
func TestNewAnswererOnAzureBesideABedrockEmbedderSignsInAtTheConfiguredScope(t *testing.T) {
	if azureChatScope == embed.DefaultEntraScope {
		t.Fatalf("the configured scope is %s, the embedder's own: this test says nothing unless they differ", azureChatScope)
	}
	assertion := writeFile(t, "federation-token", "federated-assertion\n")
	for name, embedder := range map[string]string{"DISPATCH_EMBEDDER unset": "", "DISPATCH_EMBEDDER=bedrock": "bedrock"} {
		t.Run(name, func(t *testing.T) {
			boot := resolveWith(t, azureChatEnvironment(map[string]string{
				"DISPATCH_EMBEDDER":                   embedder,
				"DISPATCH_AZURE_CHAT_SCOPE":           azureChatScope,
				"DISPATCH_AZURE_FEDERATED_TOKEN_FILE": assertion,
			}))
			if boot.Embedder.Place != embed.PlaceBedrock || boot.Embedder.AzureTenantID != "" {
				t.Fatalf("Embedder = %+v, want Bedrock holding none of the shared sign-in", boot.Embedder)
			}
			if boot.Answerer.Place != placeAnswererAzure || boot.Answerer.Scope != azureChatScope || boot.Answerer.FederatedToken != assertion {
				t.Fatalf("Answerer = %+v, want the Azure answerer at %s signing in with %s", boot.Answerer, azureChatScope, assertion)
			}

			authority := &signInRecorder{}
			signIn := httptest.NewServer(authority)
			t.Cleanup(signIn.Close)
			model := goingtest.NewModel(t, goingtest.Reply{Text: "Dispatch holds nothing on that."})
			serveHostsLocally(t, map[string]string{
				entraAuthorityHost:    signIn.URL,
				azureChatEndpointHost: model.URL,
			})

			answerer, err := newAnswerer(boot.Answerer)
			if err != nil || answerer == nil {
				t.Fatalf("newAnswerer = %v, %v, want the Azure answerer these settings name", answerer, err)
			}
			var frames []string
			ending := ""
			err = answerer.Run(t.Context(), going.Request{
				Words:  "what scope does this deployment sign in at?",
				Search: func(context.Context, string) ([]going.Hit, error) { return nil, nil },
				Bodies: func(context.Context, []going.Hit) (map[string]string, error) { return nil, nil },
			}, func(event going.Event) error {
				frames = append(frames, event.Name)
				if event.Terminal() {
					ending = event.Name
				}
				return nil
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if ending != contracts.GoingEventEnd {
				t.Fatalf("the run ended %s (%v), want %s: the sign-in and the call it signs both went through",
					ending, frames, contracts.GoingEventEnd)
			}

			if scope := authority.exchange(t, 0).Get("scope"); scope != azureChatScope {
				t.Errorf("the answerer signed in for scope %q, want DISPATCH_AZURE_CHAT_SCOPE %q (the embedder's own is %q)",
					scope, azureChatScope, embed.DefaultEntraScope)
			}
			calls := model.Requests()
			if len(calls) != 1 {
				t.Fatalf("the model served %d calls, want the one the run made", len(calls))
			}
			if got, want := calls[0].Authorization, "Bearer "+azureChatAccessToken; got != want {
				t.Errorf("the chat call presented %q, want %q: the bearer that sign-in issued", got, want)
			}
			if calls[0].Model != boot.Answerer.Model {
				t.Errorf("the chat call named model %q, want the configured deployment %q", calls[0].Model, boot.Answerer.Model)
			}
		})
	}

	// The negative control, under the Azure answerer this time: an embedder row is still read only
	// with DISPATCH_EMBEDDER=azure, and an answerer on Azure changes nothing about that.
	refusedWith(t, azureChatEnvironment(map[string]string{"DISPATCH_AZURE_EMBED_ENDPOINT": "https://embed.example/models"}),
		"DISPATCH_AZURE_EMBED_ENDPOINT set, but DISPATCH_EMBEDDER is unset, which is bedrock")
}
