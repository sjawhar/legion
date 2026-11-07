package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
)

// syncBuffer collects the stub's log while its server goroutines write it and the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startStubEmbed starts a stub on a loopback port and gives back what an operator has: the https
// endpoint Dispatch would be pointed at, and a client that trusts the certificate the stub wrote
// as PEM, which is what SSL_CERT_FILE gives Dispatch. Nothing here trusts the stub any other way:
// the client's root pool holds the written file and nothing else.
func startStubEmbed(t *testing.T, key string, throttleEvery int) (endpoint string, client *http.Client, logged *syncBuffer) {
	t.Helper()
	certOut := filepath.Join(t.TempDir(), "stub-embed.pem")
	logged = &syncBuffer{}
	stub, err := newStubEmbed(stubEmbedOptions{
		listen: "127.0.0.1:0", certOut: certOut, key: key, throttleEvery: throttleEvery,
		lg: log.New(logged, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- stub.serve() }()
	t.Cleanup(func() {
		if err := stub.close(); err != nil {
			t.Errorf("close the stub: %v", err)
		}
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	certPEM, err := os.ReadFile(certOut)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatalf("%s holds no certificate:\n%s", certOut, certPEM)
	}
	client = &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}},
		Timeout:   30 * time.Second,
	}
	return "https://" + stub.addr(), client, logged
}

// azureClientFor is Dispatch's own Azure client pointed at the stub, built as cmd/dispatch builds
// it for the key form of credential (cmd/dispatch/embedder.go:newEmbedder).
func azureClientFor(t *testing.T, endpoint, key string, client *http.Client) *embed.AzureClient {
	t.Helper()
	azure, err := embed.NewAzure(embed.AzureConfig{
		Endpoint:        endpoint,
		Deployment:      "embed-v-4-0",
		TokensPerMinute: 1_000_000,
		APIKey:          key,
		HTTPClient:      client,
	})
	if err != nil {
		t.Fatal(err)
	}
	return azure
}

// norm is a vector's length, which the stub's vectors are scaled to 1.
func norm(vector []float32) float64 {
	square := 0.0
	for _, component := range vector {
		square += float64(component) * float64(component)
	}
	return math.Sqrt(square)
}

// TestStubEmbedAnswersDispatchsOwnAzureClient: embed.AzureClient, over https it trusts only
// through the PEM the stub wrote, gets one unit-length vector of embed.Dimension floats per text,
// and the same text embeds to the same vector in a later call.
func TestStubEmbedAnswersDispatchsOwnAzureClient(t *testing.T) {
	endpoint, client, logged := startStubEmbed(t, "stub-key", 0)
	azure := azureClientFor(t, endpoint, "stub-key", client)
	texts := []string{"the seeded spec loop", "a comment on a quote row", "the seeded spec loop, again"}
	vectors, err := azure.Embed(context.Background(), texts, embed.InputDocument)
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != len(texts) {
		t.Fatalf("%d vectors for %d texts", len(vectors), len(texts))
	}
	for i, vector := range vectors {
		if len(vector) != embed.Dimension {
			t.Fatalf("vector %d is %d wide, want %d", i, len(vector), embed.Dimension)
		}
		if length := norm(vector); math.Abs(length-1) > 1e-6 {
			t.Errorf("vector %d has length %v, want 1", i, length)
		}
	}
	if slices.Equal(vectors[0], vectors[1]) {
		t.Error("two different texts embed to the same vector")
	}
	again, err := azure.Embed(context.Background(), []string{texts[0]}, embed.InputDocument)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(again[0], vectors[0]) {
		t.Error("the same text embedded to a different vector in a second call")
	}
	if identity := azure.Identity(); !strings.HasPrefix(identity, "azure:127.0.0.1:") || !strings.HasSuffix(identity, "/embed-v-4-0") {
		t.Errorf("identity %q, want azure:<stub host>/embed-v-4-0", identity)
	}
	t.Logf("%d vectors of %d floats, each of length %v; %q embedded to the same vector twice (first components %v); identity %s",
		len(vectors), len(vectors[0]), norm(vectors[0]), texts[0], vectors[0][:3], azure.Identity())
	if want := fmt.Sprintf("%d texts, %d bytes, HTTP 200", len(texts), len(texts[0])+len(texts[1])+len(texts[2])); !strings.Contains(logged.String(), want) {
		t.Errorf("the stub's log lacks %q:\n%s", want, logged.String())
	}
	t.Logf("the stub logged: %s", strings.TrimSpace(logged.String()))
}

// TestStubEmbedRefusesABatchPastAzuresByteLimit: more than 320,000 bytes of input is refused with
// a code and a status that is not 2xx, which Dispatch's client reports rather than decodes.
func TestStubEmbedRefusesABatchPastAzuresByteLimit(t *testing.T) {
	endpoint, client, _ := startStubEmbed(t, "stub-key", 0)
	azure := azureClientFor(t, endpoint, "stub-key", client)
	// 11 texts of 30,000 bytes: 330,000, past the limit and under the 96-text one. Each is also
	// under the 32,000 runes the client truncates a text to, so the stub sees every byte.
	texts := make([]string, 11)
	for i := range texts {
		texts[i] = strings.Repeat("a", 30_000)
	}
	_, err := azure.Embed(context.Background(), texts, embed.InputDocument)
	if err == nil || !strings.Contains(err.Error(), "input_too_large") || !strings.Contains(err.Error(), "413") {
		t.Fatalf("error %v, want a 413 naming input_too_large", err)
	}
	if embed.IsThrottled(err) {
		t.Error("a batch past the byte limit was reported as a throttle")
	}
	t.Logf("11 texts of 30,000 bytes: %v", err)
}

// TestStubEmbedRefusesAWrongAPIKey: the stub checks the api-key header Dispatch sends for the key
// form of credential (internal/dispatch/embed/azure.go:211), and 401 says nothing about the text.
func TestStubEmbedRefusesAWrongAPIKey(t *testing.T) {
	endpoint, client, _ := startStubEmbed(t, "stub-key", 0)
	azure := azureClientFor(t, endpoint, "another-key", client)
	_, err := azure.Embed(context.Background(), []string{"a text"}, embed.InputDocument)
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("error %v, want a 401 naming unauthorized", err)
	}
	t.Logf("a wrong api-key: %v", err)
}

// TestStubEmbedThrottlesEveryNthBatch: -throttle-every makes the stub answer 429, which
// embed.IsThrottled reads as the throttle the queue backs off from and retries.
func TestStubEmbedThrottlesEveryNthBatch(t *testing.T) {
	endpoint, client, _ := startStubEmbed(t, "stub-key", 2)
	azure := azureClientFor(t, endpoint, "stub-key", client)
	if _, err := azure.Embed(context.Background(), []string{"the first batch"}, embed.InputDocument); err != nil {
		t.Fatalf("the first batch was refused: %v", err)
	}
	_, err := azure.Embed(context.Background(), []string{"the second batch"}, embed.InputDocument)
	if err == nil || !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "rate_limit_exceeded") {
		t.Fatalf("error %v, want a 429 naming rate_limit_exceeded", err)
	}
	if !embed.IsThrottled(err) {
		t.Errorf("error %v is not a throttle, so the queue would not back off", err)
	}
	t.Logf("every second batch: %v (a throttle: %t)", err, embed.IsThrottled(err))
}

// postStubEmbed sends one request the way Dispatch's client does and returns the status and the
// error code the body names. It is how the refusals that client never reaches are exercised: it
// refuses a batch past 96 texts itself (boundBatch), and it always asks for embed.Dimension.
func postStubEmbed(t *testing.T, client *http.Client, endpoint, key string, body stubEmbedRequest) (int, string) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	url := endpoint + stubEmbedRoute + "?api-version=" + stubEmbedAPIVersion
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(stubEmbedKeyHeader, key)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var refusal stubEmbedRefusal
	if err := json.NewDecoder(response.Body).Decode(&refusal); err != nil {
		t.Fatalf("decode the answer: %v", err)
	}
	return response.StatusCode, refusal.Error.Code
}

// TestStubEmbedRefusesABatchAndAWidthAzureWouldRefuse: 97 texts and a width other than 1536, each
// answered with a code and a status that is not 2xx.
func TestStubEmbedRefusesABatchAndAWidthAzureWouldRefuse(t *testing.T) {
	endpoint, client, _ := startStubEmbed(t, "stub-key", 0)
	tooMany := make([]string, stubEmbedMaxTexts+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("text %d", i)
	}
	for _, c := range []struct {
		name    string
		request stubEmbedRequest
		status  int
		code    string
	}{
		{"97 texts", stubEmbedRequest{Model: "embed-v-4-0", Input: tooMany, InputType: "document", Dimensions: stubEmbedDimension, EncodingFormat: "float"}, http.StatusBadRequest, "too_many_inputs"},
		{"1024 wide", stubEmbedRequest{Model: "embed-v-4-0", Input: []string{"a text"}, InputType: "document", Dimensions: 1024, EncodingFormat: "float"}, http.StatusBadRequest, "unsupported_dimensions"},
		{"no input", stubEmbedRequest{Model: "embed-v-4-0", Input: nil, InputType: "document", Dimensions: stubEmbedDimension, EncodingFormat: "float"}, http.StatusBadRequest, "no_input"},
	} {
		status, code := postStubEmbed(t, client, endpoint, "stub-key", c.request)
		if status != c.status || code != c.code {
			t.Errorf("%s: HTTP %d %q, want HTTP %d %q", c.name, status, code, c.status, c.code)
		}
		t.Logf("%s: HTTP %d %q", c.name, status, code)
	}
}

// TestStubEmbedVectorOfIsDeterministicAndUnitLength: a text's vector is the same every time and
// on every machine (nothing but the text goes into it), different texts differ, and every vector
// is unit length, which is what cosine distance expects.
func TestStubEmbedVectorOfIsDeterministicAndUnitLength(t *testing.T) {
	// The first components of "spec loop", fixed here so a change to the derivation is noticed:
	// a vector that drifts would silently re-rank a seed embedded by an earlier build.
	want := []float32{0.023218982, -0.014978888, 0.024942707}
	vector := stubEmbedVectorOf("spec loop")
	if len(vector) != stubEmbedDimension {
		t.Fatalf("a vector is %d wide, want %d", len(vector), stubEmbedDimension)
	}
	if !slices.Equal(vector[:len(want)], want) {
		t.Errorf("the first components of \"spec loop\" are %v, want %v", vector[:len(want)], want)
	}
	if !slices.Equal(vector, stubEmbedVectorOf("spec loop")) {
		t.Error("the same text gave two different vectors")
	}
	if slices.Equal(vector, stubEmbedVectorOf("spec loop ")) {
		t.Error("two texts a space apart gave the same vector")
	}
	for _, text := range []string{"", "spec loop", strings.Repeat("x", 40_000)} {
		if length := norm(stubEmbedVectorOf(text)); math.Abs(length-1) > 1e-6 {
			t.Errorf("the vector of a %d-byte text has length %v, want 1", len(text), length)
		}
	}
}

// TestStubEmbedCertificateCoversItsHostsAndIsItsOwnRoot: every -host value becomes a name or an
// IP on the certificate, and the certificate verifies against itself, which is what makes the
// written PEM enough for Dispatch's SSL_CERT_FILE.
func TestStubEmbedCertificateCoversItsHostsAndIsItsOwnRoot(t *testing.T) {
	hosts := []string{"dispatch-loadtest.internal", "127.0.0.1", "10.4.1.7"}
	certificate, certPEM, err := stubEmbedCertificate(hosts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(leaf.DNSNames, []string{"dispatch-loadtest.internal"}) {
		t.Errorf("certificate names %q, want the one name", leaf.DNSNames)
	}
	var ips []string
	for _, ip := range leaf.IPAddresses {
		ips = append(ips, ip.String())
	}
	if !slices.Equal(ips, []string{"127.0.0.1", "10.4.1.7"}) {
		t.Errorf("certificate covers IPs %q, want 127.0.0.1 and 10.4.1.7", ips)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatalf("the PEM holds no certificate:\n%s", certPEM)
	}
	for _, host := range []string{"dispatch-loadtest.internal", "10.4.1.7"} {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			t.Errorf("verify as %s: %v", host, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "another.host", Roots: roots}); err == nil {
		t.Error("the certificate verified for a host it does not cover")
	}
}

// TestRunStubEmbedRefusesWhatItCannotServe: every input is checked before the port is taken, so a
// run that cannot serve says why rather than listening and refusing every request.
func TestRunStubEmbedRefusesWhatItCannotServe(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{}, "-key or -key-file is required"},
		{[]string{"-key", "k", "-listen", ""}, "-listen is required"},
		{[]string{"-key", "k", "-throttle-every", "-1"}, "-throttle-every must be 0 or more"},
		{[]string{"-key", "k", "extra"}, `unexpected argument "extra"`},
	} {
		err := runStubEmbed(c.args)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("loadgen stub-embed %q: error %v, want %q", c.args, err, c.want)
		}
	}
}

// TestStubEmbedReadsItsKeyFromAFile: the embedder's api-key has a file form, because a key given
// as -key is in this process's argv, and in the argv of the ssh that started it, for the whole
// life of the deployment (dispatch://INFRA-988). The file's content with its trailing white
// space trimmed is the key, and the four refusals are the ones every credential of this package
// is read with (readCredentials).
func TestStubEmbedReadsItsKeyFromAFile(t *testing.T) {
	const key = "stub-embed-api-key"
	// The file ends in white space, as one written with printf does (vm/vm.sh writes it on the
	// load machine).
	path := credentialFileAt(t, "embed-key", key+" \t\n")
	opts, err := stubEmbedArgs([]string{"-key-file", path})
	if err != nil {
		t.Fatalf("-key-file was refused: %v", err)
	}
	if opts.key != key {
		t.Errorf("the key is %q, want the file's content with its trailing white space trimmed", opts.key)
	}

	_, err = stubEmbedArgs([]string{"-key", key, "-key-file", path})
	var both *bothCredentialFormsError
	if !errors.As(err, &both) || both.Flag != "-key" || both.FileFlag != "-key-file" {
		t.Errorf("-key given with -key-file too: refusal %v, want one naming the two forms", err)
	}

	// A file that cannot be read, and one that holds no credential: each refused by the flag and
	// the path, before the port is taken.
	absent := filepath.Join(t.TempDir(), "absent-key")
	for _, bad := range []string{absent, credentialFileAt(t, "empty-key", " \n")} {
		if _, err := stubEmbedArgs([]string{"-key-file", bad}); err == nil ||
			!strings.Contains(err.Error(), "-key-file") || !strings.Contains(err.Error(), bad) {
			t.Errorf("-key-file %s: refusal %v, want one naming the flag and the path", bad, err)
		}
	}
}

// TestNewStubEmbedWritesItsCertificateAndTakesItsPort: the certificate is on disk and the port is
// taken before anything is served, so neither a path that cannot be written nor a port already in
// use is found only once Dispatch is already pointed at the stub.
func TestNewStubEmbedWritesItsCertificateAndTakesItsPort(t *testing.T) {
	certOut := filepath.Join(t.TempDir(), "certs", "stub-embed.pem")
	lg := log.New(&syncBuffer{}, "", 0)
	stub, err := newStubEmbed(stubEmbedOptions{listen: "127.0.0.1:0", certOut: certOut, key: "stub-key", lg: lg})
	if err != nil {
		t.Fatal(err)
	}
	defer stub.close()
	written, err := os.ReadFile(certOut)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, stub.certPEM) || !bytes.HasPrefix(written, []byte("-----BEGIN CERTIFICATE-----")) {
		t.Errorf("%s does not hold the served certificate as PEM:\n%s", certOut, written)
	}
	if _, _, err := net.SplitHostPort(stub.addr()); err != nil {
		t.Errorf("the stub listens on %q: %v", stub.addr(), err)
	}
	if _, err := newStubEmbed(stubEmbedOptions{listen: stub.addr(), certOut: certOut, key: "stub-key", lg: lg}); err == nil {
		t.Error("a second stub took the same port")
	}
}
