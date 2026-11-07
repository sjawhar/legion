package main

// loadgen stub-embed: the Azure AI Foundry embeddings endpoint, stood in for a deployment that
// has no Azure resource, so meaning search embeds what the seed writes and a load step measures
// a deployment whose search behaves like itself rather than one that embeds nothing.
//
// Dispatch's own client is the contract (internal/dispatch/embed/azure.go): the stub serves the
// route and api-version that client posts to (azureEmbeddingsRoute, azureAPIVersion), answers one
// vector per input text of embed.Dimension floats, and refuses what Azure refuses - a batch past
// 96 texts or 320,000 bytes of input (azureMaxBatchTexts, azureMaxBatchBytes), a width other than
// 1536, and a request whose api-key header is not the key the stub was given. Each vector is
// derived from its text alone (stubEmbedVectorOf), so the same text has the same vector in every
// process and on every machine: a seed embedded twice ranks the same both times.
//
// It serves https with a certificate it generates, because Dispatch refuses an endpoint that is
// not an absolute https URL with a host and no query or fragment
// (cmd/dispatch/embedder.go:resolveAzureEmbedder). The certificate is its own root, written as
// PEM where -cert-out names, so whoever starts Dispatch points SSL_CERT_FILE at that file and
// Dispatch trusts the stub with no other change.
//
// The pace is Dispatch's own, never the stub's: DISPATCH_AZURE_EMBED_TOKENS_PER_MINUTE, which is
// 1,000,000 for the production deployment, bounds what Dispatch spends, and background work gets
// two thirds of it (internal/dispatch/embed/azure.go:123). The stub answers as fast as it can
// hash, which is faster than that pace, so a seed waits on Dispatch's queue rather than on this
// server; -throttle-every makes it answer 429 on purpose, which is the one way to exercise the
// queue's throttle path without a real quota.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// What Dispatch's Azure client sends and accepts, held here because the client holds them
// unexported: the route and api-version it posts to
// (internal/dispatch/embed/azure.go:22-23, azureEmbeddingsRoute and azureAPIVersion), the key
// header it sets for the key form of credential (azure.go:211), the batch limits it is built
// with (azure.go:29 azureMaxBatchTexts, azure.go:36 azureMaxBatchBytes) and the width every
// vector has (internal/dispatch/embed/embed.go:23, embed.Dimension, the width of
// embeddings.embedding and its HNSW index).
const (
	stubEmbedRoute      = "/embeddings"
	stubEmbedAPIVersion = "2024-05-01-preview"
	stubEmbedKeyHeader  = "api-key"
	stubEmbedMaxTexts   = 96
	stubEmbedMaxBytes   = 320_000
	stubEmbedDimension  = 1536
)

// stubEmbedRequestLimit bounds how much of a request body is read. A batch the client means
// carries at most stubEmbedMaxBytes of text plus its JSON, so 16 MiB is generous for anything
// Azure would be asked, and a body past it is refused rather than read without end.
const stubEmbedRequestLimit = 16 << 20

// stubEmbedCertLife is how long the generated certificate is valid. A load test runs for hours
// and a stack is rebuilt for each one, so 30 days is long for its purpose; nothing renews it.
const stubEmbedCertLife = 30 * 24 * time.Hour

// stubEmbedDefaultHosts is what the certificate covers when -host names nothing: the loopback
// names a Dispatch on the same box dials.
var stubEmbedDefaultHosts = []string{"localhost", "127.0.0.1", "::1"}

// stubEmbedRequest is the body Dispatch's client sends (azure.go:azureEmbedRequest).
type stubEmbedRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	InputType      string   `json:"input_type"`
	Dimensions     int      `json:"dimensions"`
	EncodingFormat string   `json:"encoding_format"`
}

// stubEmbedVector is one answered vector: the index of the text it embeds, which the client
// places the vector by rather than trusting the order (azure.go:placeAzureVectors).
type stubEmbedVector struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

// stubEmbedResponse is the body of an answered batch (azure.go:azureEmbedResponse).
type stubEmbedResponse struct {
	Data []stubEmbedVector `json:"data"`
}

// stubEmbedRefusal is the body of a refusal: the shape the client reads an error code out of
// (azure.go:azureErrorResponse, azureStatusError). The codes are the stub's own words; the
// client quotes whatever code it is given in the error it returns.
type stubEmbedRefusal struct {
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

// stubEmbedder answers the embeddings route. key is the api-key every request must carry;
// throttleEvery makes every nth batch a 429, and 0 never does; batches counts the batches that
// decoded, which is what throttleEvery counts.
type stubEmbedder struct {
	key           string
	throttleEvery int
	batches       atomic.Int64
	lg            *log.Logger
}

// stubEmbedAnswer is one request's outcome: the status and body to write, and the batch the
// request carried, which the log line reports.
type stubEmbedAnswer struct {
	status int
	body   any
	texts  int
	bytes  int
}

// stubEmbedRefuse is an answer that carries only an error code, as Azure refuses.
func stubEmbedRefuse(status int, code string) stubEmbedAnswer {
	var body stubEmbedRefusal
	body.Error.Code = code
	return stubEmbedAnswer{status: status, body: body}
}

func (s *stubEmbedder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	answer := s.answer(r)
	payload, err := json.Marshal(answer.body)
	if err != nil {
		answer = stubEmbedRefuse(http.StatusInternalServerError, "encode_failed")
		payload, _ = json.Marshal(answer.body)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(answer.status)
	if _, err := w.Write(payload); err != nil {
		s.lg.Printf("write the answer: %v", err)
	}
	s.lg.Printf("%s %s: %d texts, %d bytes, HTTP %d", r.Method, r.URL.Path, answer.texts, answer.bytes, answer.status)
}

// answer is what the stub replies to one request, and what that request carried. It refuses in
// the order Azure would notice: the route, the method and the api-version first, then the
// credential, then the batch, so a wrong key never tells the caller anything about its texts.
func (s *stubEmbedder) answer(r *http.Request) stubEmbedAnswer {
	switch {
	case !strings.HasSuffix(r.URL.Path, stubEmbedRoute):
		return stubEmbedRefuse(http.StatusNotFound, "no_such_route")
	case r.Method != http.MethodPost:
		return stubEmbedRefuse(http.StatusMethodNotAllowed, "method_not_allowed")
	case r.URL.Query().Get("api-version") != stubEmbedAPIVersion:
		return stubEmbedRefuse(http.StatusBadRequest, "unsupported_api_version")
	case r.Header.Get(stubEmbedKeyHeader) != s.key:
		return stubEmbedRefuse(http.StatusUnauthorized, "unauthorized")
	}
	payload, err := io.ReadAll(io.LimitReader(r.Body, stubEmbedRequestLimit+1))
	switch {
	case err != nil:
		return stubEmbedRefuse(http.StatusBadRequest, "unreadable_request")
	case len(payload) > stubEmbedRequestLimit:
		return stubEmbedRefuse(http.StatusRequestEntityTooLarge, "request_too_large")
	}
	var request stubEmbedRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return stubEmbedRefuse(http.StatusBadRequest, "invalid_request")
	}
	bytesIn := 0
	for _, text := range request.Input {
		bytesIn += len(text)
	}
	// Every answer from here on names the batch it saw, so one log line reports every request
	// that carried one, refused or not.
	batch := func(answer stubEmbedAnswer) stubEmbedAnswer {
		answer.texts, answer.bytes = len(request.Input), bytesIn
		return answer
	}
	if n := s.batches.Add(1); s.throttleEvery > 0 && n%int64(s.throttleEvery) == 0 {
		return batch(stubEmbedRefuse(http.StatusTooManyRequests, "rate_limit_exceeded"))
	}
	switch {
	case len(request.Input) == 0:
		return batch(stubEmbedRefuse(http.StatusBadRequest, "no_input"))
	case len(request.Input) > stubEmbedMaxTexts:
		return batch(stubEmbedRefuse(http.StatusBadRequest, "too_many_inputs"))
	case bytesIn > stubEmbedMaxBytes:
		return batch(stubEmbedRefuse(http.StatusRequestEntityTooLarge, "input_too_large"))
	case request.Dimensions != stubEmbedDimension:
		return batch(stubEmbedRefuse(http.StatusBadRequest, "unsupported_dimensions"))
	case request.InputType != "document" && request.InputType != "query":
		return batch(stubEmbedRefuse(http.StatusBadRequest, "unsupported_input_type"))
	}
	data := make([]stubEmbedVector, len(request.Input))
	for i, text := range request.Input {
		data[i] = stubEmbedVector{Index: i, Embedding: stubEmbedVectorOf(text)}
	}
	return batch(stubEmbedAnswer{status: http.StatusOK, body: stubEmbedResponse{Data: data}})
}

// stubEmbedVectorOf is text's vector: SHA-256 of the text, then of that digest with a block
// counter, expanded to stubEmbedDimension floats in [-1, 1) and scaled to unit length. Nothing
// here reads a clock, a map's order or a random source, and SHA-256 and IEEE floats are the same
// everywhere, so one text has one vector in every process and on every machine - which is what
// lets a search over a seed the stub embedded be compared with the same search tomorrow. Unit
// length is what meaning search's cosine distance expects of a model's output; a vector of
// different lengths would make distance depend on text length rather than on text.
func stubEmbedVectorOf(text string) []float32 {
	seed := sha256.Sum256([]byte(text))
	vector := make([]float32, stubEmbedDimension)
	var input [sha256.Size + 8]byte
	copy(input[:sha256.Size], seed[:])
	square := 0.0
	for block := 0; block*8 < len(vector); block++ {
		binary.BigEndian.PutUint64(input[sha256.Size:], uint64(block))
		digest := sha256.Sum256(input[:])
		for i := 0; i < 8 && block*8+i < len(vector); i++ {
			word := binary.BigEndian.Uint32(digest[i*4 : i*4+4])
			component := float32(float64(word)/float64(math.MaxUint32)*2 - 1)
			vector[block*8+i] = component
			square += float64(component) * float64(component)
		}
	}
	// A zero vector would need 1,536 words to be exactly half of 2^32, which SHA-256 does not
	// do; the guard is here so a scale by zero is impossible rather than improbable.
	norm := math.Sqrt(square)
	if norm == 0 {
		vector[0] = 1
		return vector
	}
	for i, component := range vector {
		vector[i] = float32(float64(component) / norm)
	}
	return vector
}

// stubEmbedCertificate generates the certificate the stub serves and the PEM whoever starts
// Dispatch points SSL_CERT_FILE at. One self-signed certificate is both: marked a CA so it
// verifies as a root, and carrying every name and IP given as a subject alternative name, which
// is what Go's verifier matches the dialed host against (it reads no common name).
func stubEmbedCertificate(hosts []string, now time.Time) (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("generate a key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("generate a serial number: %w", err)
	}
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "loadgen stub-embed"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(stubEmbedCertLife),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("generate a certificate: %w", err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, block, nil
}

// stubEmbedOptions is what `loadgen stub-embed` was given, as newStubEmbed takes it.
type stubEmbedOptions struct {
	// listen is the address to serve https on, and certOut where the certificate is written as
	// PEM; an empty certOut writes none, which only a test does.
	listen, certOut string
	// key is the api-key every request must carry.
	key string
	// hosts are the names and IPs the certificate covers; empty means stubEmbedDefaultHosts.
	hosts []string
	// throttleEvery answers 429 to every nth batch; 0 never does.
	throttleEvery int
	lg            *log.Logger
}

// stubEmbed is a stub that is listening: the server, its listener, and the certificate it wrote.
type stubEmbed struct {
	server  *http.Server
	ln      net.Listener
	certPEM []byte
}

// newStubEmbed takes the port and writes the certificate, so a port already in use or a
// certificate path that cannot be written fails before anything is served. It does not serve:
// the caller calls serve.
func newStubEmbed(opts stubEmbedOptions) (*stubEmbed, error) {
	if opts.key == "" {
		return nil, errors.New("the stub needs the api-key it is to require")
	}
	hosts := opts.hosts
	if len(hosts) == 0 {
		hosts = stubEmbedDefaultHosts
	}
	certificate, certPEM, err := stubEmbedCertificate(hosts, time.Now())
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", opts.listen)
	if err != nil {
		return nil, err
	}
	if opts.certOut != "" {
		if err := os.MkdirAll(filepath.Dir(opts.certOut), 0o755); err != nil {
			_ = ln.Close()
			return nil, err
		}
		if err := os.WriteFile(opts.certOut, certPEM, 0o644); err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("write %s: %w", opts.certOut, err)
		}
	}
	server := &http.Server{
		Handler:           &stubEmbedder{key: opts.key, throttleEvery: opts.throttleEvery, lg: opts.lg},
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
	}
	return &stubEmbed{server: server, ln: ln, certPEM: certPEM}, nil
}

// addr is the address the stub listens on, with the port the kernel gave a :0 listen.
func (s *stubEmbed) addr() string { return s.ln.Addr().String() }

// serve answers until close; a closed server is how it ends, not a failure.
func (s *stubEmbed) serve() error {
	if err := s.server.ServeTLS(s.ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *stubEmbed) close() error { return s.server.Close() }

// stubEmbedHosts is the repeatable -host flag: each value is a name or an IP the certificate
// covers.
type stubEmbedHosts []string

func (h *stubEmbedHosts) String() string { return strings.Join(*h, ",") }

func (h *stubEmbedHosts) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("a host is a name or an IP, not empty")
	}
	*h = append(*h, strings.TrimSpace(value))
	return nil
}

// stubEmbedArgs is what `loadgen stub-embed` was given, as options newStubEmbed takes: every
// input checked and the key read out of its file where -key-file gave it, before the port is
// taken. The caller sets lg. It is a function of its own so a test reads the options a command
// line resolves to without serving on them.
func stubEmbedArgs(args []string) (stubEmbedOptions, error) {
	fs := flag.NewFlagSet("stub-embed", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8443", "address to serve https on")
	key := fs.String("key", "", "the api-key header every request must carry (required, or -key-file)")
	keyFile := fs.String("key-file", "", "file holding the -key value, so no api-key reaches the command line")
	certOut := fs.String("cert-out", "", "where the certificate is written as PEM, for Dispatch's SSL_CERT_FILE (default $LOADTEST_ROOT/stub-embed.pem)")
	var hosts stubEmbedHosts
	fs.Var(&hosts, "host", "a name or IP the certificate covers; repeatable (default localhost, 127.0.0.1, ::1)")
	throttleEvery := fs.Int("throttle-every", 0, "answer 429 to every nth batch, to exercise the queue's throttle path; 0 never does")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: loadgen stub-embed (-key <api-key> | -key-file <path>) [-listen 127.0.0.1:8443] [-cert-out <pem>] [-host <name or ip>]... [-throttle-every 0]")
		fmt.Fprintln(fs.Output(), "Serves Azure's embeddings route over https with a certificate it generates: point Dispatch's")
		fmt.Fprintln(fs.Output(), "DISPATCH_AZURE_EMBED_ENDPOINT at it and its SSL_CERT_FILE at -cert-out, so Dispatch trusts it.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		return stubEmbedOptions{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	// The key is a credential, so it has a file form and is read before anything else is judged
	// (readCredentials): an api-key on a command line is in the argv of this process and of any
	// ssh that carried it, for as long as the stub serves.
	if err := readCredentials(credentialFile{"-key", "-key-file", *keyFile, key}); err != nil {
		return stubEmbedOptions{}, err
	}
	switch {
	case *key == "":
		return stubEmbedOptions{}, fmt.Errorf("-key or -key-file is required: the api-key Dispatch is configured with (DISPATCH_AZURE_EMBED_KEY)")
	case *listen == "":
		return stubEmbedOptions{}, fmt.Errorf("-listen is required")
	case *throttleEvery < 0:
		return stubEmbedOptions{}, fmt.Errorf("-throttle-every must be 0 or more, not %d", *throttleEvery)
	}
	if *certOut == "" {
		*certOut = filepath.Join(loadtestRoot(), "stub-embed.pem")
	}
	return stubEmbedOptions{listen: *listen, certOut: *certOut, key: *key, hosts: hosts, throttleEvery: *throttleEvery}, nil
}

func runStubEmbed(args []string) error {
	opts, err := stubEmbedArgs(args)
	if err != nil {
		return err
	}
	opts.lg = log.New(os.Stderr, "loadgen stub-embed: ", log.LstdFlags)
	stub, err := newStubEmbed(opts)
	if err != nil {
		return err
	}
	opts.lg.Printf("serving https://%s%s?api-version=%s; certificate %s (SSL_CERT_FILE), %d-wide vectors, throttle every %d",
		stub.addr(), stubEmbedRoute, stubEmbedAPIVersion, opts.certOut, stubEmbedDimension, opts.throttleEvery)
	return stub.serve()
}
