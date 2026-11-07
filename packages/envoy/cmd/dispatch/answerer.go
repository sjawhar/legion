package main

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
)

const (
	// placeAnswererAzure is DISPATCH_ANSWERER=azure: a chat deployment of an Azure AI Foundry
	// resource, signed in to with the shared federated credential.
	placeAnswererAzure = "azure"
	// placeAnswererEndpoint is DISPATCH_ANSWERER=endpoint: any OpenAI-shaped chat completions
	// service, which presents the bearer token a file holds.
	placeAnswererEndpoint = "endpoint"
	// azureChatSettingPrefix begins the name of every setting only DISPATCH_ANSWERER=azure reads.
	azureChatSettingPrefix = "DISPATCH_AZURE_CHAT_"
	// chatSettingPrefix begins the name of every setting only DISPATCH_ANSWERER=endpoint reads.
	chatSettingPrefix = "DISPATCH_CHAT_"
)

// answererConfig is where Dispatch's server generates text, as resolveAnswerer checked it. Place
// is "" (no model configured), placeAnswererAzure or placeAnswererEndpoint; the rest belongs to
// one place and is empty under the other.
type answererConfig struct {
	Place           string
	Endpoint        string
	Model           string
	APIVersion      string
	Scope           string
	TokensPerMinute int
	TokenFile       string
	TenantID        string
	ClientID        string
	FederatedToken  string
}

// resolveAnswerer reads DISPATCH_ANSWERER and the settings of the place it names, and refuses,
// naming the setting, every shape the answerer could not be built from or would silently ignore:
// a place other than azure or endpoint; a setting of the group the chosen place does not read;
// the shared Azure sign-in with neither model service on Azure; and, under each place, a setting
// that is missing or malformed. embedderPlace is where meaning search embeds, since the embedder
// and the answerer share that sign-in: a deployment may embed on Bedrock and answer on Azure, and
// then the answerer is what reads the three. It calls nothing and reads no file, so a model
// service that is down, or a token file that is not written yet, never refuses boot.
func resolveAnswerer(env settingValues, embedderPlace string) (answererConfig, error) {
	place := strings.TrimSpace(env.get("DISPATCH_ANSWERER"))
	switch place {
	case "", placeAnswererAzure, placeAnswererEndpoint:
	default:
		return answererConfig{}, fmt.Errorf("DISPATCH_ANSWERER=%q (expected azure or endpoint)", place)
	}
	chosen := "DISPATCH_ANSWERER is unset"
	if place != "" {
		chosen = "DISPATCH_ANSWERER is " + place
	}
	if place != placeAnswererAzure {
		if set := settingsSet(env, azureChatSettingPrefix); len(set) > 0 {
			return answererConfig{}, fmt.Errorf("%s set, but %s: the DISPATCH_AZURE_CHAT_* settings are read only with DISPATCH_ANSWERER=azure", strings.Join(set, ", "), chosen)
		}
		if embedderPlace != embed.PlaceAzure {
			if set := readAzureSignIn(env).set; len(set) > 0 {
				return answererConfig{}, fmt.Errorf("%s set, but neither DISPATCH_EMBEDDER nor DISPATCH_ANSWERER is azure: the shared Azure sign-in is read by whichever of them is azure", strings.Join(set, ", "))
			}
		}
	}
	if place != placeAnswererEndpoint {
		if set := settingsSet(env, chatSettingPrefix); len(set) > 0 {
			return answererConfig{}, fmt.Errorf("%s set, but %s: the DISPATCH_CHAT_* settings are read only with DISPATCH_ANSWERER=endpoint", strings.Join(set, ", "), chosen)
		}
	}
	switch place {
	case placeAnswererAzure:
		return resolveAzureAnswerer(env)
	case placeAnswererEndpoint:
		return resolveEndpointAnswerer(env)
	}
	return answererConfig{}, nil
}

// chatEndpoint is the base URL /chat/completions is appended to, as the row name names it: it is
// required, and an absolute https URL with a host and no query or fragment, since the client
// builds the route and the query itself.
func chatEndpoint(env settingValues, name, place string) (string, error) {
	endpoint := strings.TrimSpace(env.get(name))
	if endpoint == "" {
		return "", fmt.Errorf("%s is required when DISPATCH_ANSWERER is %s", name, place)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || strings.ContainsAny(endpoint, "?#") {
		return "", fmt.Errorf("%s=%q (expected an absolute https URL with a host and no query or fragment, the base URL the chat completions route is appended to)", name, endpoint)
	}
	return endpoint, nil
}

// resolveAzureAnswerer is resolveAnswerer for DISPATCH_ANSWERER=azure. The Azure chat form takes
// one credential, the federated sign-in: there is no chat API key.
func resolveAzureAnswerer(env settingValues) (answererConfig, error) {
	cfg := answererConfig{Place: placeAnswererAzure}
	endpoint, err := chatEndpoint(env, "DISPATCH_AZURE_CHAT_ENDPOINT", placeAnswererAzure)
	if err != nil {
		return answererConfig{}, err
	}
	cfg.Endpoint = endpoint
	cfg.Model = strings.TrimSpace(env.get("DISPATCH_AZURE_CHAT_DEPLOYMENT"))
	if cfg.Model == "" {
		return answererConfig{}, errors.New("DISPATCH_AZURE_CHAT_DEPLOYMENT is required when DISPATCH_ANSWERER is azure: the chat deployment's name, which each request sends as its model")
	}
	cfg.APIVersion = strings.TrimSpace(env.get("DISPATCH_AZURE_CHAT_API_VERSION"))
	cfg.Scope = cmp.Or(strings.TrimSpace(env.get("DISPATCH_AZURE_CHAT_SCOPE")), embed.DefaultEntraScope)
	quota := strings.TrimSpace(env.get("DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE"))
	if quota == "" {
		return answererConfig{}, errors.New("DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE is required when DISPATCH_ANSWERER is azure: the deployment's tokens-per-minute quota")
	}
	cfg.TokensPerMinute, err = strconv.Atoi(quota)
	if err != nil || cfg.TokensPerMinute <= 0 {
		return answererConfig{}, fmt.Errorf("DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE=%q (expected a positive integer, the deployment's tokens-per-minute quota)", quota)
	}
	signIn := readAzureSignIn(env)
	cfg.TenantID, cfg.ClientID, cfg.FederatedToken = signIn.TenantID, signIn.ClientID, signIn.TokenFile
	switch {
	case len(signIn.set) == 0:
		return answererConfig{}, fmt.Errorf("DISPATCH_ANSWERER=azure needs the keyless Azure sign-in (%s): the chat form takes no API key", azureKeylessNames)
	case len(signIn.missing) > 0:
		return answererConfig{}, signIn.refusePartial()
	}
	return cfg, nil
}

// resolveEndpointAnswerer is resolveAnswerer for DISPATCH_ANSWERER=endpoint. The token file's
// path is handed on unread, because the endpoint's bearer is read on every call.
func resolveEndpointAnswerer(env settingValues) (answererConfig, error) {
	cfg := answererConfig{Place: placeAnswererEndpoint}
	endpoint, err := chatEndpoint(env, "DISPATCH_CHAT_ENDPOINT", placeAnswererEndpoint)
	if err != nil {
		return answererConfig{}, err
	}
	cfg.Endpoint = endpoint
	cfg.TokenFile = strings.TrimSpace(env.get("DISPATCH_CHAT_TOKEN_FILE"))
	if cfg.TokenFile == "" {
		return answererConfig{}, errors.New("DISPATCH_CHAT_TOKEN_FILE is required when DISPATCH_ANSWERER is endpoint: the file holding the bearer token every call presents")
	}
	cfg.Model = strings.TrimSpace(env.get("DISPATCH_CHAT_MODEL"))
	if cfg.Model == "" {
		return answererConfig{}, errors.New("DISPATCH_CHAT_MODEL is required when DISPATCH_ANSWERER is endpoint: the model each request names")
	}
	return cfg, nil
}

// newAnswerer builds the answerer cfg names, or nil when the deployment configured no model
// (DISPATCH_ANSWERER unset), which is a server that boots and says it cannot answer rather than
// one that refuses to boot: the capability route then says no model is configured, and a
// question asked of it is recorded and its stream carries that. It is built from settings
// resolveAnswerer has already checked, so an error here is a shape that check missed; it calls
// nothing and reads no token file, so a model service or a sign-in being down fails an answer
// later, never the boot. The one line it logs names the place and the model that answers, never
// a credential, as the embedder's does.
func newAnswerer(cfg answererConfig) (*going.Answerer, error) {
	if cfg.Place == "" {
		slog.Warn("dispatch: no text model configured, so this server cannot answer questions", "setting", "DISPATCH_ANSWERER")
		return nil, nil
	}
	chat := going.HTTPConfig{
		Place:      cfg.Place,
		Endpoint:   cfg.Endpoint,
		APIVersion: cfg.APIVersion,
		Model:      cfg.Model,
	}
	switch cfg.Place {
	case placeAnswererAzure:
		// The chat form of Azure signs in keylessly, at the scope the settings name: an Azure
		// resource's inference families differ in it, so it is configuration here and not
		// embed.DefaultEntraScope.
		tokens, err := embed.NewEntraTokenSource(embed.EntraConfig{
			TenantID:  cfg.TenantID,
			ClientID:  cfg.ClientID,
			TokenFile: cfg.FederatedToken,
			Scope:     cfg.Scope,
		})
		if err != nil {
			return nil, err
		}
		chat.Tokens = tokens
	case placeAnswererEndpoint:
		// The endpoint form's bearer is whatever DISPATCH_CHAT_TOKEN_FILE holds at the moment
		// of the call: the source reads it then, so a token rewritten under this process is
		// the one the next call presents.
		chat.Tokens = going.FileTokenSource{Path: cfg.TokenFile}
	}
	client, err := going.NewHTTPClient(chat)
	if err != nil {
		return nil, err
	}
	answerer, err := going.NewAnswerer(going.AnswererConfig{Model: client, TokensPerMinute: cfg.TokensPerMinute})
	if err != nil {
		return nil, err
	}
	// The identity is read from the client just built, which is the thing that knows it: the
	// pacing an answerer wraps a client in says nothing about which model answers.
	slog.Info("dispatch: answers questions with a configured text model", "place", cfg.Place, "identity", client.Identity())
	return answerer, nil
}
