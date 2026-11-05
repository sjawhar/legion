// packages/envoy/internal/googleidentity/googleidentity.go
//
// Package googleidentity reads a machine's own Google identity token from the Compute Engine
// metadata server: the token a Google Cloud VM, and a pod on Google Kubernetes Engine, presents
// as the subject of the broker's RFC 8693 exchange. It asks for format=full, so a token minted on
// an instance carries the google.compute_engine claims an exchange entry may pin on, and it reads
// the token alone: nothing here verifies a signature, an issuer, an audience or an expiry, which
// is the broker's work, and nothing here parses a claim.
//
// The transport has no proxy at all, as cloud.google.com/go/compute/metadata's does.
// http.DefaultClient honours HTTP_PROXY, and the metadata server's address is link-local rather
// than loopback, so the default transport would hand whatever a proxy variable names the
// machine's identity token — a credential that acts as that machine until it expires, fetched
// again before every renewal, so one misread variable would hand over a fresh one each time.
package googleidentity

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// MetadataHostVar replaces the metadata server's host (and port), as Google's own client
	// libraries read it. Its value is a host, with no scheme.
	MetadataHostVar = "GCE_METADATA_HOST"
	// defaultMetadataHost is the metadata server every Google machine answers on.
	defaultMetadataHost = "metadata.google.internal"
	// identityPath mints an identity token for the instance's default service account.
	identityPath = "/computeMetadata/v1/instance/service-accounts/default/identity"
	// timeout bounds one token fetch. The metadata server is on the host's own link, so a fetch
	// that takes longer than this is a fault, not a slow network.
	timeout = 10 * time.Second
)

// client is the proxy-free transport this package's reason for being rests on; the package
// comment says why it may never be http.DefaultClient.
var client = &http.Client{Transport: &http.Transport{}}

// tokenURL is the identity-token URL for audience, honouring MetadataHostVar.
func tokenURL(audience string) string {
	host := os.Getenv(MetadataHostVar)
	if host == "" {
		host = defaultMetadataHost
	}
	return "http://" + host + identityPath + "?audience=" + url.QueryEscape(audience) + "&format=full"
}

// Token asks the metadata server for this machine's identity token for audience. format=full is
// required: without it Google mints a token carrying no google.compute_engine claims, which names
// no project, zone or instance, so an exchange entry pinned to either refuses it. Every error
// names the URL it asked and never the token.
func Token(ctx context.Context, audience string) (string, error) {
	callURL := tokenURL(audience)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, callURL, nil)
	if err != nil {
		return "", fmt.Errorf("ask the metadata server at %s: %w", callURL, err)
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("the metadata server at %s could not be reached (%s names its host): %w", callURL, MetadataHostVar, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read the metadata server's answer from %s: %w", callURL, err)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("the metadata server at %s answered HTTP %d", callURL, resp.StatusCode)
	}
	token := strings.TrimSpace(string(body))
	if token == "" {
		return "", fmt.Errorf("the metadata server at %s answered an empty identity token", callURL)
	}
	return token, nil
}
