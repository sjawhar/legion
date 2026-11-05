package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
)

const (
	// defaultAzureDeployment is DISPATCH_AZURE_EMBED_DEPLOYMENT when it is unset.
	defaultAzureDeployment = "embed-v-4-0"
	// azureEmbedSettingPrefix begins the name of every setting only DISPATCH_EMBEDDER=azure
	// reads; the shared Azure sign-in is three rows of its own, which the answerer reads too.
	azureEmbedSettingPrefix = "DISPATCH_AZURE_EMBED_"
	// azureKeyNames is the key form of Azure credential, as a refusal names it.
	azureKeyNames = "DISPATCH_AZURE_EMBED_KEY_FILE or DISPATCH_AZURE_EMBED_KEY"
	// azureKeylessNames is the keyless form of Azure credential, as a refusal names it.
	azureKeylessNames = "DISPATCH_AZURE_TENANT_ID, DISPATCH_AZURE_CLIENT_ID and DISPATCH_AZURE_FEDERATED_TOKEN_FILE"
)

// embedderConfig is where meaning search embeds text, as resolveEmbedder checked it. Place is
// embed.PlaceBedrock (DISPATCH_EMBEDDER unset or bedrock) or embed.PlaceAzure; the rest is Azure's
// alone, empty under Bedrock. Under Azure exactly one credential is set: AzureKey, or the keyless
// trio AzureTenantID, AzureClientID and AzureTokenFile, all three.
type embedderConfig struct {
	Place                string
	AzureEndpoint        string
	AzureDeployment      string
	AzureTokensPerMinute int
	AzureKey             string
	AzureTenantID        string
	AzureClientID        string
	AzureTokenFile       string
}

// resolveEmbedder reads DISPATCH_EMBEDDER, the DISPATCH_AZURE_EMBED_* settings and the shared
// Azure sign-in, and refuses, naming the setting, every shape newEmbedder could not build or
// would silently ignore: a place other than bedrock or azure; any DISPATCH_AZURE_EMBED_* setting
// under Bedrock, DISPATCH_EMBEDDER's default; and under Azure, an endpoint that is missing or not
// an absolute https URL with a host and no query or fragment, a quota that is missing or not a
// positive integer, a key file that is unreadable or blank, both credential forms, neither, or
// part of the keyless one. The shared sign-in under Bedrock is resolveAnswerer's refusal, since
// an answerer on Azure reads it while the embedder is on Bedrock. It calls nothing, so a model
// service or a sign-in that is down never refuses boot.
func resolveEmbedder(env settingValues) (embedderConfig, error) {
	switch place := strings.TrimSpace(env.get("DISPATCH_EMBEDDER")); place {
	case "", embed.PlaceBedrock:
		if set := settingsSet(env, azureEmbedSettingPrefix); len(set) > 0 {
			chosen := "DISPATCH_EMBEDDER is bedrock"
			if place == "" {
				chosen = "DISPATCH_EMBEDDER is unset, which is bedrock"
			}
			return embedderConfig{}, fmt.Errorf("%s set, but %s: the DISPATCH_AZURE_EMBED_* settings are read only with DISPATCH_EMBEDDER=azure", strings.Join(set, ", "), chosen)
		}
		return embedderConfig{Place: embed.PlaceBedrock}, nil
	case embed.PlaceAzure:
		return resolveAzureEmbedder(env)
	default:
		return embedderConfig{}, fmt.Errorf("DISPATCH_EMBEDDER=%q (expected bedrock or azure)", place)
	}
}

// settingsSet names each variable of one group env sets, in table order: every row whose name
// begins with prefix, a row's _FILE form included. A refusal lists them, so an operator sees
// every setting the place it chose would have ignored.
func settingsSet(env settingValues, prefix string) []string {
	var set []string
	for _, row := range settings {
		if !strings.HasPrefix(row.Name, prefix) {
			continue
		}
		variables := []string{row.Name}
		if row.File {
			variables = append(variables, row.fileVariable())
		}
		for _, variable := range variables {
			if strings.TrimSpace(env.get(variable)) != "" {
				set = append(set, variable)
			}
		}
	}
	return set
}

// azureSignIn is the Azure sign-in both model services take, as readAzureSignIn found it: the
// three values, and the names of the rows the deployment set and of those it left unset, in
// table order, which is the order every refusal of the group names them in.
type azureSignIn struct {
	TenantID  string
	ClientID  string
	TokenFile string
	set       []string
	missing   []string
}

// readAzureSignIn reads the three rows of the shared Azure sign-in. They are named rows rather
// than a prefix because the embedder's own settings begin with DISPATCH_AZURE_ too, and
// whichever of the embedder and the answerer is azure reads these three. One reader, because
// the embedder's resolution, the answerer's and the refusal of the group with neither place on
// Azure must all read the same rows in the same order, and a second table could drift from the
// first.
func readAzureSignIn(env settingValues) azureSignIn {
	var signIn azureSignIn
	for _, row := range []struct {
		name  string
		value *string
	}{
		{"DISPATCH_AZURE_TENANT_ID", &signIn.TenantID},
		{"DISPATCH_AZURE_CLIENT_ID", &signIn.ClientID},
		{"DISPATCH_AZURE_FEDERATED_TOKEN_FILE", &signIn.TokenFile},
	} {
		*row.value = strings.TrimSpace(env.get(row.name))
		if *row.value != "" {
			signIn.set = append(signIn.set, row.name)
		} else {
			signIn.missing = append(signIn.missing, row.name)
		}
	}
	return signIn
}

// refusePartial is the one refusal of a sign-in set in part, which the embedder and the answerer
// both write: the keyless form is all three rows or none, and an operator is told which of them
// it has and which it is missing.
func (s azureSignIn) refusePartial() error {
	return fmt.Errorf("the keyless Azure sign-in needs %s: %s set, %s missing", azureKeylessNames, strings.Join(s.set, ", "), strings.Join(s.missing, ", "))
}

// resolveAzureEmbedder is resolveEmbedder for DISPATCH_EMBEDDER=azure.
func resolveAzureEmbedder(env settingValues) (embedderConfig, error) {
	cfg := embedderConfig{Place: embed.PlaceAzure}
	endpoint := strings.TrimSpace(env.get("DISPATCH_AZURE_EMBED_ENDPOINT"))
	if endpoint == "" {
		return embedderConfig{}, errors.New("DISPATCH_AZURE_EMBED_ENDPOINT is required when DISPATCH_EMBEDDER is azure")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || strings.ContainsAny(endpoint, "?#") {
		return embedderConfig{}, fmt.Errorf("DISPATCH_AZURE_EMBED_ENDPOINT=%q (expected an absolute https URL with a host and no query or fragment, the base URL the embeddings route is appended to)", endpoint)
	}
	cfg.AzureEndpoint = endpoint
	cfg.AzureDeployment = cmp.Or(strings.TrimSpace(env.get("DISPATCH_AZURE_EMBED_DEPLOYMENT")), defaultAzureDeployment)
	quota := strings.TrimSpace(env.get("DISPATCH_AZURE_EMBED_TOKENS_PER_MINUTE"))
	if quota == "" {
		return embedderConfig{}, errors.New("DISPATCH_AZURE_EMBED_TOKENS_PER_MINUTE is required when DISPATCH_EMBEDDER is azure: the deployment's tokens-per-minute quota")
	}
	cfg.AzureTokensPerMinute, err = strconv.Atoi(quota)
	if err != nil || cfg.AzureTokensPerMinute <= 0 {
		return embedderConfig{}, fmt.Errorf("DISPATCH_AZURE_EMBED_TOKENS_PER_MINUTE=%q (expected a positive integer, the deployment's tokens-per-minute quota)", quota)
	}

	cfg.AzureKey, err = fileSetting(env, "DISPATCH_AZURE_EMBED_KEY")
	if err != nil {
		return embedderConfig{}, err
	}
	signIn := readAzureSignIn(env)
	cfg.AzureTenantID, cfg.AzureClientID, cfg.AzureTokenFile = signIn.TenantID, signIn.ClientID, signIn.TokenFile
	switch {
	case cfg.AzureKey != "" && len(signIn.set) > 0:
		return embedderConfig{}, fmt.Errorf("DISPATCH_EMBEDDER=azure takes one credential, and both are set: the key (%s) and the keyless sign-in (%s set)", azureKeyNames, strings.Join(signIn.set, ", "))
	case cfg.AzureKey == "" && len(signIn.set) == 0:
		return embedderConfig{}, fmt.Errorf("DISPATCH_EMBEDDER=azure needs a credential: the key (%s), or the keyless sign-in (%s)", azureKeyNames, azureKeylessNames)
	case len(signIn.set) > 0 && len(signIn.missing) > 0:
		return embedderConfig{}, signIn.refusePartial()
	}
	return cfg, nil
}

// newEmbedder builds the embedder cfg names, or nil when meaning search is off. Bedrock is built
// as it always was: with no AWS region reaching the process (a CI job, a devbox with no
// AWS_REGION) embed.NewBedrock fails, which is a warning and keyword-only search, never a refusal
// to boot. Azure is built from settings resolveEmbedder has already checked, so an error here is
// a shape that check missed; it calls nothing, so Azure or Entra being down fails an embedding
// later, never the boot. The Azure line it logs names the place and the identity, never the key.
func newEmbedder(ctx context.Context, cfg embedderConfig) (embed.Embedder, error) {
	if cfg.Place != embed.PlaceAzure {
		client, err := embed.NewBedrock(ctx)
		if err != nil {
			slog.Warn("dispatch: meaning search unavailable", "error", err)
			return nil, nil
		}
		return client, nil
	}
	azure := embed.AzureConfig{
		Endpoint:        cfg.AzureEndpoint,
		Deployment:      cfg.AzureDeployment,
		TokensPerMinute: cfg.AzureTokensPerMinute,
		APIKey:          cfg.AzureKey,
	}
	credential := "key"
	if cfg.AzureKey == "" {
		tokens, err := embed.NewEntraTokenSource(embed.EntraConfig{
			TenantID:  cfg.AzureTenantID,
			ClientID:  cfg.AzureClientID,
			TokenFile: cfg.AzureTokenFile,
			Scope:     embed.DefaultEntraScope,
		})
		if err != nil {
			return nil, err
		}
		azure.Tokens = tokens
		credential = "keyless"
	}
	client, err := embed.NewAzure(azure)
	if err != nil {
		return nil, err
	}
	slog.Info("dispatch: meaning search embeds through Azure AI Foundry", "place", embed.PlaceAzure, "identity", client.Identity(), "credential", credential)
	return client, nil
}
