// Command dev-oidc-issuer is the local stand-in for a Kubernetes cluster's ServiceAccount token
// issuer, so a broker run on one machine (scripts/dev-broker.sh, the broker's docs rig) can list
// its callers in BROKER_API_ACCOUNTS and take a pod's token at its RFC 8693 token exchange with no
// cluster and no Docker. It serves internal/oidc/oidctest's issuer, the one the Go tests mint
// against, on a loopback port, and keeps each token it is asked for in a file, as the kubelet
// keeps a pod's projected token: minted for an hour, the longest the broker's API routes take
// (machine.MaxTokenLoginLifetime), and rewritten in place once four fifths of that has passed.
//
//	dev-oidc-issuer [--listen <host:port>] [--token <audience>=<service account>=<file> ...]
//		[--sign-in-client <client id> --sign-in-client-secret-file <file>
//		[--sign-in-domain <domain>] [--sign-in-default <email>]]
//
// Each token is bound to a pod of its own (kubernetes.io → pod → uid), kept across rewrites as a
// pod keeps its uid, so a file serves a pod's enrolment and its token exchange as well as a listed
// caller. Once every file is written it logs "issuer listening url=<issuer URL>", the value
// BROKER_K8S_OIDC_ISSUER takes, and serves until it is interrupted. Its signing key lives in its
// memory alone: a restart publishes another, and no token minted before it verifies again.
//
// --listen fixes the issuer's address, so a harness can name the issuer before it starts; the
// host must be a loopback one, since the issuer mints for and signs in whoever asks. Without it the
// issuer takes a random loopback port.
//
// The sign-in flags make it a person's identity provider as well, the stand-in for Google that a
// secrets broker's approval sign-in goes to outside a Go test (Dispatch's Playwright suite,
// scripts/dev-broker.sh). Its discovery then names an authorization and a token endpoint for the
// one confidential client, whose secret is read from the file and never from the command line; a
// code whose request carried a PKCE challenge is exchanged only with its S256 verifier, and the
// exchange answers an ID token and no refresh token, as Google does for an online sign-in. Each
// authorization request signs a person in with no page to fill in: the next one POST
// /dev/sign-in-as queued, else the request's login_hint, else --sign-in-default, and with none of
// them it is refused 400. The ID token names that email as given, a sub kept per address,
// email_verified true, hd the --sign-in-domain (example.com unless given), and the request's nonce.
// Once sign-in is enabled it logs a line naming the client, never its secret.
//
// Two control endpoints serve a harness:
//
//	POST /dev/sign-in-as	{"email": "..."}, as JSON or a form: the next sign-in's person; 204.
//	POST /dev/mint		{"subject": "system:serviceaccount:<namespace>:<name>", "audience": "...",
//				"pod_uid": "...", "lifetime_seconds": n}: {"token": "..."}, minted as a
//				--token file's token is, for a random pod unless pod_uid names one, living
//				lifetime_seconds, at most and by default the hour.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const exitUsageError = 2

// tokenLifetime is how long each minted token lives: the longest a listed caller's token may live
// from its issue, so the files serve the broker's API routes as well as enrolment and exchange.
const tokenLifetime = machine.MaxTokenLoginLifetime

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stderr))
}

// maxControlBody bounds the body of a control endpoint's request, each a few short strings.
const maxControlBody = 1 << 16

func run(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("dev-oidc-issuer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var tokens tokenFlags
	fs.Var(&tokens, "token", "keep `<audience>=<service account>=<file>`'s token in file (repeatable)")
	listen := fs.String("listen", "", "serve on the loopback `<host:port>` rather than a random port")
	clientID := fs.String("sign-in-client", "", "sign people in for the confidential client `<client id>`")
	secretFile := fs.String("sign-in-client-secret-file", "", "read the sign-in client's secret from `<file>`")
	domain := fs.String("sign-in-domain", "example.com", "the `<domain>` every signed-in person's hd claim names")
	defaultEmail := fs.String("sign-in-default", "", "sign in as `<email>` a request with no login_hint and no one queued")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsageError
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "dev-oidc-issuer: unexpected argument %q\n", fs.Arg(0))
		return exitUsageError
	}
	config := issuerConfig{specs: tokens, lifetime: tokenLifetime, listen: *listen}
	if *listen != "" {
		if err := checkLoopback(*listen); err != nil {
			fmt.Fprintf(stderr, "dev-oidc-issuer: --listen %s: %v\n", *listen, err)
			return exitUsageError
		}
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	switch {
	case given["sign-in-client"] != given["sign-in-client-secret-file"]:
		fmt.Fprintln(stderr, "dev-oidc-issuer: --sign-in-client and --sign-in-client-secret-file are given together or not at all")
		return exitUsageError
	case !given["sign-in-client"] && (given["sign-in-domain"] || given["sign-in-default"]):
		fmt.Fprintln(stderr, "dev-oidc-issuer: --sign-in-domain and --sign-in-default need --sign-in-client")
		return exitUsageError
	case given["sign-in-client"]:
		secret, err := os.ReadFile(*secretFile)
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "dev-oidc-issuer: --sign-in-client-secret-file: %v\n", err)
			return exitUsageError
		case *clientID == "" || *domain == "" || strings.TrimSpace(string(secret)) == "":
			fmt.Fprintln(stderr, "dev-oidc-issuer: the sign-in client, its secret and the sign-in domain must not be empty")
			return exitUsageError
		}
		config.signIn = &signIn{clientID: *clientID, clientSecret: strings.TrimSpace(string(secret)), domain: *domain, defaultEmail: *defaultEmail}
	}
	return serveIssuer(ctx, config, stderr)
}

// issuerConfig is everything the issuer serves: the --token files and how long each token lives,
// the --listen address (empty for a random loopback port), and the sign-in (nil without one).
type issuerConfig struct {
	specs    []tokenSpec
	lifetime time.Duration
	listen   string
	signIn   *signIn
}

// signIn is the sign-in flags: the one confidential client and its secret, the domain every
// person's hd names, and the person a request signs in when it names none and no one is queued.
type signIn struct {
	clientID, clientSecret, domain, defaultEmail string
}

// person is the claims a sign-in as email carries before the issuer adds iss, aud, iat, exp and the
// nonce. Its sub is derived from the address in lower case, as Google keeps one sub per account
// whatever case its address is typed in, while email keeps the case it was given in.
func (s signIn) person(email string) map[string]any {
	sum := sha256.Sum256([]byte(strings.ToLower(email)))
	return map[string]any{
		"sub":            hex.EncodeToString(sum[:12]),
		"email":          email,
		"email_verified": true,
		"hd":             s.domain,
	}
}

// tokenSpec is one --token: the audience and the service account a token is minted for, the file
// it is kept in, and the uid of the pod it is bound to.
type tokenSpec struct {
	audience, account, file, podUID string
}

// tokenFlags is the repeated --token flag.
type tokenFlags []tokenSpec

func (f *tokenFlags) String() string { return "" }

// Set parses <audience>=<service account>=<file>. Neither an audience nor a service account holds
// an "=", so the file is everything after the second one.
func (f *tokenFlags) Set(value string) error {
	audience, rest, _ := strings.Cut(value, "=")
	account, file, _ := strings.Cut(rest, "=")
	if audience == "" || account == "" || file == "" {
		return errors.New("want <audience>=<service account>=<file>")
	}
	if err := checkAccount(account); err != nil {
		return err
	}
	*f = append(*f, tokenSpec{audience: audience, account: account, file: file})
	return nil
}

// checkAccount refuses a subject that is not a ServiceAccount's, the only kind of subject a
// cluster's issuer mints for.
func checkAccount(account string) error {
	namespace, name, _ := strings.Cut(strings.TrimPrefix(account, "system:serviceaccount:"), ":")
	if !strings.HasPrefix(account, "system:serviceaccount:") || namespace == "" || name == "" || strings.Contains(name, ":") {
		return fmt.Errorf("service account %q is not system:serviceaccount:<namespace>:<name>", account)
	}
	return nil
}

// checkLoopback refuses an address whose host is not a loopback one: the issuer mints a token for
// and signs in whoever asks, so it answers this machine alone.
func checkLoopback(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("host %q is not a loopback address", host)
	}
	return nil
}

// serve runs the issuer for specs alone, on a random loopback port and with no sign-in.
func serve(ctx context.Context, specs []tokenSpec, lifetime time.Duration, stderr io.Writer) int {
	return serveIssuer(ctx, issuerConfig{specs: specs, lifetime: lifetime}, stderr)
}

// serveIssuer runs the issuer until ctx ends, writing every spec's token before it reports its URL
// and again each time four fifths of the lifetime has passed. A token it cannot write, or an
// address it cannot listen on, ends it, 1.
func serveIssuer(ctx context.Context, config issuerConfig, stderr io.Writer) int {
	specs, lifetime := config.specs, config.lifetime
	p := &process{stderr: stderr}
	defer p.stop()
	var issuer *oidctest.Issuer
	if config.listen == "" {
		issuer = oidctest.New(p)
	} else {
		listener, err := net.Listen("tcp", config.listen)
		if err != nil {
			fmt.Fprintf(stderr, "dev-oidc-issuer: %v\n", err)
			return 1
		}
		issuer = oidctest.NewOnListener(p, listener)
	}
	key := issuer.PublishKey(p, "dev-signing-key")
	issuer.Handle("POST /dev/mint", handleMint(issuer, key, p, lifetime))
	if config.signIn != nil {
		enableSignIn(issuer, key, *config.signIn)
	}
	// Each token's pod, a random UUID as the API server assigns a pod's uid.
	for i := range specs {
		specs[i].podUID = uuid.NewString()
	}
	writeAll := func() error {
		for _, spec := range specs {
			if err := writeToken(issuer, key, p, spec, lifetime); err != nil {
				return err
			}
		}
		return nil
	}
	if err := writeAll(); err != nil {
		fmt.Fprintf(stderr, "dev-oidc-issuer: %v\n", err)
		return 1
	}
	for _, spec := range specs {
		fmt.Fprintf(stderr, "dev-oidc-issuer: %s holds %s's token for audience %s (pod %s)\n", spec.file, spec.account, spec.audience, spec.podUID)
	}
	if config.signIn != nil {
		fmt.Fprintf(stderr, "dev-oidc-issuer: signs people in for client %s with hd %s\n", config.signIn.clientID, config.signIn.domain)
	}
	fmt.Fprintf(stderr, "dev-oidc-issuer: issuer listening url=%s\n", issuer.URL())
	ticker := time.NewTicker(lifetime * 4 / 5)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
			if err := writeAll(); err != nil {
				fmt.Fprintf(stderr, "dev-oidc-issuer: %v\n", err)
				return 1
			}
		}
	}
}

// enableSignIn arms the issuer's code flow for s's client, answering no refresh token, signs in a
// request that finds no one queued as its login_hint or else s's default, and serves
// /dev/sign-in-as, which queues the next request's person.
func enableSignIn(issuer *oidctest.Issuer, key *oidctest.Key, s signIn) {
	issuer.EnableCodeFlow(key, s.clientID, s.clientSecret)
	issuer.OmitRefreshTokens()
	issuer.SignInUnqueued(func(query url.Values) (map[string]any, error) {
		email := query.Get("login_hint")
		if email == "" {
			email = s.defaultEmail
		}
		if email == "" {
			return nil, errors.New("no one to sign in: queue one with POST /dev/sign-in-as, send a login_hint, or start the issuer with --sign-in-default")
		}
		return s.person(email), nil
	})
	issuer.Handle("POST /dev/sign-in-as", func(w http.ResponseWriter, r *http.Request) {
		email, err := readEmail(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		issuer.SignInAs(s.person(email))
		w.WriteHeader(http.StatusNoContent)
	})
}

// readEmail reads the email of a /dev/sign-in-as request's body, a JSON object or a form.
func readEmail(w http.ResponseWriter, r *http.Request) (string, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxControlBody)
	var email string
	if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType == "application/json" {
		var body struct {
			Email string `json:"email"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			return "", fmt.Errorf("read the request: %w", err)
		}
		email = body.Email
	} else {
		if err := r.ParseForm(); err != nil {
			return "", fmt.Errorf("read the request: %w", err)
		}
		email = r.PostForm.Get("email")
	}
	if email == "" {
		return "", errors.New(`want {"email": "..."}`)
	}
	return email, nil
}

// mintRequest is a /dev/mint request's body.
type mintRequest struct {
	Subject         string `json:"subject"`
	Audience        string `json:"audience"`
	PodUID          string `json:"pod_uid"`
	LifetimeSeconds *int64 `json:"lifetime_seconds"`
}

// handleMint answers /dev/mint: the token of the request's ServiceAccount for its audience, bound
// to its pod or a random one, living its lifetime_seconds or else maxLifetime, which it may not
// exceed.
func handleMint(issuer *oidctest.Issuer, key *oidctest.Key, p *process, maxLifetime time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request mintRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxControlBody))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			http.Error(w, "read the request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := checkAccount(request.Subject); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if request.Audience == "" {
			http.Error(w, "want an audience", http.StatusBadRequest)
			return
		}
		lifetime := maxLifetime
		if seconds := request.LifetimeSeconds; seconds != nil {
			// Compared in seconds before the conversion, which a huge value would overflow.
			if *seconds <= 0 || *seconds > int64(maxLifetime/time.Second) {
				http.Error(w, fmt.Sprintf("lifetime_seconds %d is not between 1 and %d", *seconds, int64(maxLifetime/time.Second)), http.StatusBadRequest)
				return
			}
			lifetime = time.Duration(*seconds) * time.Second
		}
		podUID := request.PodUID
		if podUID == "" {
			podUID = uuid.NewString()
		}
		token := mintToken(issuer, key, p, request.Subject, request.Audience, podUID, lifetime)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
	}
}

// mintToken mints account's token for audience, bound to the pod podUID, issued now and expiring
// lifetime from now: the token a kubelet projects into that pod.
func mintToken(issuer *oidctest.Issuer, key *oidctest.Key, p *process, account, audience, podUID string, lifetime time.Duration) string {
	now := time.Now()
	claims := issuer.Claims(account, audience)
	claims["iat"] = now.Unix()
	claims["exp"] = now.Add(lifetime).Unix()
	claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": podUID}}
	return issuer.Mint(p, key, claims)
}

// writeToken mints spec's token, issued now and expiring lifetime from now, and replaces spec.file
// with it in one rename, so a reader sees the old token or the new one and never a part of either.
func writeToken(issuer *oidctest.Issuer, key *oidctest.Key, p *process, spec tokenSpec, lifetime time.Duration) error {
	token := mintToken(issuer, key, p, spec.account, spec.audience, spec.podUID, lifetime)
	temporary, err := os.CreateTemp(filepath.Dir(spec.file), "."+filepath.Base(spec.file)+".*")
	if err != nil {
		return fmt.Errorf("write %s: %w", spec.file, err)
	}
	_, err = temporary.WriteString(token + "\n")
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporary.Name(), spec.file)
	}
	if err != nil {
		_ = os.Remove(temporary.Name())
		return fmt.Errorf("write %s: %w", spec.file, err)
	}
	return nil
}

// process is oidctest.TB for a command rather than a test: a failure ends the process, as it ends
// a test, and the cleanups run when the command stops serving.
type process struct {
	stderr   io.Writer
	cleanups []func()
}

func (*process) Helper() {}

func (p *process) Fatalf(format string, args ...any) {
	fmt.Fprintf(p.stderr, "dev-oidc-issuer: "+format+"\n", args...)
	os.Exit(1)
}

func (p *process) Cleanup(f func()) { p.cleanups = append(p.cleanups, f) }

func (p *process) stop() {
	for i := len(p.cleanups) - 1; i >= 0; i-- {
		p.cleanups[i]()
	}
}
