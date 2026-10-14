// Package provider builds the envoy-infra plugin: its schema metadata and the components it serves.
package provider

import (
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"
)

// downloadURL is where Pulumi fetches the plugin: it fills in ${VERSION} and requests the asset
// pulumi-resource-envoy-infra-v<version>-<os>-<arch>.tar.gz from the release of that tag.
const downloadURL = "https://github.com/sjawhar/legion/releases/download/envoy-infra-v${VERSION}"

// New returns the envoy-infra provider serving every component the plugin registers.
func New() (p.Provider, error) {
	builder := infer.NewProviderBuilder().
		WithNamespace("sjawhar").
		WithDisplayName("Envoy infrastructure").
		WithRepository("https://github.com/sjawhar/legion").
		WithLicense("Apache-2.0").
		WithPluginDownloadURL(downloadURL).
		// The default, <repository>/sdk/go/<package>, is not a path Go accepts as a module.
		WithGoImportPath("github.com/sjawhar/envoy/infra/sdk/go/envoyinfra")
	// The builder's Build refuses a provider that registers no component yet.
	return infer.Provider(builder.BuildOptions()), nil
}
