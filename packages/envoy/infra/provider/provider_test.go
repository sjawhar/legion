package provider_test

import (
	"encoding/json"
	"testing"

	"github.com/blang/semver"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/integration"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/provider"
)

func TestSchemaNamesTheReleaseDownloadURLAndTheGoImportPath(t *testing.T) {
	prov, err := provider.New()
	require.NoError(t, err)
	server, err := integration.NewServer(t.Context(), "envoy-infra", semver.MustParse("0.0.1"),
		integration.WithProvider(prov))
	require.NoError(t, err)

	resp, err := server.GetSchema(p.GetSchemaRequest{})
	require.NoError(t, err)

	var schema struct {
		PluginDownloadURL string `json:"pluginDownloadURL"`
		Language          struct {
			Go struct {
				ImportBasePath string `json:"importBasePath"`
			} `json:"go"`
		} `json:"language"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Schema), &schema))
	require.Equal(t, "https://github.com/sjawhar/legion/releases/download/envoy-infra-v${VERSION}",
		schema.PluginDownloadURL)
	require.Equal(t, "github.com/sjawhar/envoy/infra/sdk/go/envoyinfra", schema.Language.Go.ImportBasePath)
}
