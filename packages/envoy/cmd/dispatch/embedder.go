package main

import (
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
	// embedderBedrock and embedderAzure are DISPATCH_EMBEDDER's values: the places meaning search
	// reaches Cohere Embed v4. Unset is embedderBedrock.
	embedderBedrock = "bedrock"
	embedderAzure   = "azure"
	// defaultAzureDeployment is DISPATCH_AZURE_EMBED_DEPLOYMENT when it is unset.
	defaultAzureDeployment = "embed-v-4-0"
	// azureSettingPrefix begins the name of every setting only DISPATCH_EMBEDDER=azure reads.
	azureSettingPrefix = "DISPATCH_AZURE_"
	// azureKeyNames is the key form of Azure credential, as a refusal names it.
	azureKeyNames = "DISPATCH_AZURE_EMBED_KEY_FILE or DISPATCH_AZURE_EMBED_KEY"
	// azureKeylessNames is the keyless form of Azure credential, as a refusal names it.
	azureKeylessNames = "DISPATCH_AZURE_TENANT_ID, DISPATCH_AZURE_CLIENT_ID and DISPATCH_AZURE_FEDERATED_TOKEN_FILE"
)

// embedderConfig is where meaning search embeds text, as resolveEmbedder checked it. Place is
// embedderBedrock or embedderAzure; the rest is Azure's alone, empty under Bedrock. Under Azure
// exactly one credential is set: AzureKey, or the keyless trio AzureTenantID, AzureClientID and
// AzureTokenFile, all three.
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

// resolveEmbedder reads DISPATCH_EMBEDDER and the DISPATCH_AZURE_* settings, and refuses, naming
// the setting, every shape newEmbedder could not build or would silently ignore: a place other
// than bedrock or azure; any DISPATCH_AZURE_* setting under Bedrock, DISPATCH_EMBEDDER's default;
// and under Azure, an endpoint that is missing or not an absolute https URL with a host and no
// query or fragment, a quota that is missing or not a positive integer, a key file that is
// unreadable or blank, both credential forms, neither, or part of the keyless one. It calls
// nothing, so a model service or a sign-in that is down never refuses boot.
func resolveEmbedder(env settingValues) (embedderConfig, error) {
	switch place := strings.TrimSpace(env.get("DISPATCH_EMBEDDER")); place {
	case "", embedderBedrock:
		if set := azureSettingsSet(env); len(set) > 0 {
			chosen := "DISPATCH_EMBEDDER is bedrock"
			if place == "" {
				chosen = "DISPATCH_EMBEDDER is unset, which is bedrock"
			}
			return embedderConfig{}, fmt.Errorf("%s set, but %s: the DISPATCH_AZURE_* settings are read only with DISPATCH_EMBEDDER=azure", strings.Join(set, ", "), chosen)
		}
		return embedderConfig{Place: embedderBedrock}, nil
	case embedderAzure:
		return resolveAzureEmbedder(env)
	default:
		return embedderConfig{}, fmt.Errorf("DISPATCH_EMBEDDER=%q (expected bedrock or azure)", place)
	}
}

// azureSettingsSet names each DISPATCH_AZURE_* variable env sets, a row's _FILE form included,
// in table order.
func azureSettingsSet(env settingValues) []string {
	var set []string
	for _, row := range settings {
		if !strings.HasPrefix(row.Name, azureSettingPrefix) {
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

// resolveAzureEmbedder is resolveEmbedder for DISPATCH_EMBEDDER=azure.
func resolveAzureEmbedder(env settingValues) (embedderConfig, error) {
	cfg := embedderConfig{Place: embedderAzure, AzureDeployment: defaultAzureDeployment}
	endpoint := strings.TrimSpace(env.get("DISPATCH_AZURE_EMBED_ENDPOINT"))
	if endpoint == "" {
		return embedderConfig{}, errors.New("DISPATCH_AZURE_EMBED_ENDPOINT is required when DISPATCH_EMBEDDER is azure")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || strings.ContainsAny(endpoint, "?#") {
		return embedderConfig{}, fmt.Errorf("DISPATCH_AZURE_EMBED_ENDPOINT=%q (expected an absolute https URL with a host and no query or fragment, the base URL the embeddings route is appended to)", endpoint)
	}
	cfg.AzureEndpoint = endpoint
	if deployment := strings.TrimSpace(env.get("DISPATCH_AZURE_EMBED_DEPLOYMENT")); deployment != "" {
		cfg.AzureDeployment = deployment
	}
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
	var set, missing []string
	for _, setting := range []struct {
		name  string
		value *string
	}{
		{"DISPATCH_AZURE_TENANT_ID", &cfg.AzureTenantID},
		{"DISPATCH_AZURE_CLIENT_ID", &cfg.AzureClientID},
		{"DISPATCH_AZURE_FEDERATED_TOKEN_FILE", &cfg.AzureTokenFile},
	} {
		*setting.value = strings.TrimSpace(env.get(setting.name))
		if *setting.value != "" {
			set = append(set, setting.name)
		} else {
			missing = append(missing, setting.name)
		}
	}
	switch {
	case cfg.AzureKey != "" && len(set) > 0:
		return embedderConfig{}, fmt.Errorf("DISPATCH_EMBEDDER=azure takes one credential, and both are set: the key (%s) and the keyless sign-in (%s set)", azureKeyNames, strings.Join(set, ", "))
	case cfg.AzureKey == "" && len(set) == 0:
		return embedderConfig{}, fmt.Errorf("DISPATCH_EMBEDDER=azure needs a credential: the key (%s), or the keyless sign-in (%s)", azureKeyNames, azureKeylessNames)
	case len(set) > 0 && len(missing) > 0:
		return embedderConfig{}, fmt.Errorf("the keyless Azure sign-in needs %s: %s set, %s missing", azureKeylessNames, strings.Join(set, ", "), strings.Join(missing, ", "))
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
	if cfg.Place != embedderAzure {
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
	slog.Info("dispatch: meaning search embeds through Azure AI Foundry", "place", embedderAzure, "identity", client.Identity(), "credential", credential)
	return client, nil
}
