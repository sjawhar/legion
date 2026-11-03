// Package provider builds the envoy-infra plugin: its schema metadata and the components it serves.
package provider

import (
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"

	"github.com/sjawhar/envoy/infra/aws"
	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/host"
)

// downloadURL is where Pulumi fetches the plugin: it fills in ${VERSION} and requests the asset
// pulumi-resource-envoy-infra-v<version>-<os>-<arch>.tar.gz from the release of that tag. The
// release build overwrites it with the download URL of the repository the run is in, which
// .github/scripts/repo-paths.sh plugin_download_url derives, through
// `-ldflags -X 'github.com/sjawhar/envoy/infra/provider.downloadURL=<that URL>'`; that is why it
// is a var, since -X writes a variable and never a constant. A build with no such flag - a
// developer's, and provider_test.go's - serves the address below, this repository's own today.
var downloadURL = "https://github.com/sjawhar/legion/releases/download/envoy-infra-v${VERSION}"

// New returns the envoy-infra provider serving every component the plugin registers, refusing a
// fractional number in any integer input before a component decodes it (refuseFractionalIntegers).
func New() (p.Provider, error) {
	prov, err := infer.NewProviderBuilder().
		WithNamespace("sjawhar").
		WithDisplayName("Envoy infrastructure").
		WithRepository("https://github.com/sjawhar/legion").
		WithLicense("Apache-2.0").
		WithPluginDownloadURL(downloadURL).
		// The default, <repository>/sdk/go/<package>, is not a path Go accepts as a module.
		WithGoImportPath("github.com/sjawhar/envoy/infra/sdk/go/envoyinfra").
		WithComponents(
			infer.ComponentF(host.NewListener),
			infer.ComponentF(aws.NewNatsService),
			infer.ComponentF(aws.NewWebhookListener),
			infer.ComponentF(gcp.NewNatsHub),
			infer.ComponentF(gcp.NewClusteredNatsHub),
			infer.ComponentF(gcp.NewDispatch),
		).
		Build()
	if err != nil {
		return p.Provider{}, err
	}
	return refuseFractionalIntegers(prov), nil
}
