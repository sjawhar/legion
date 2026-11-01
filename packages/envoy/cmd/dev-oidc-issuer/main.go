// Command dev-oidc-issuer is the local stand-in for a Kubernetes cluster's ServiceAccount token
// issuer, so a broker run on one machine (scripts/dev-broker.sh, the broker's docs rig) can list
// its callers in BROKER_API_ACCOUNTS and take a pod's token at its RFC 8693 token exchange with no
// cluster and no Docker, and for the identity provider a person signs in to Dispatch at, so a
// Dispatch run on one machine can sign a browser in with no Google. It serves
// internal/oidc/oidctest's issuer, the one the Go tests mint against, on a loopback port, and keeps
// each token it is asked for in a file, as the kubelet keeps a pod's projected token: minted for an
// hour, the longest the broker's API routes take (machine.MaxTokenLoginLifetime), and rewritten in
// place once four fifths of that has passed.
//
//	dev-oidc-issuer [--token <audience>=<service account>=<file> ...]
//	                [--code-flow <client id>=<client secret file> [--sign-in-as <email> [--sign-in-group <group>]]]
//
// Each service account's tokens are bound to one pod of its own (kubernetes.io → pod → uid), kept
// across rewrites as a pod keeps its uid, so a file serves a pod's enrolment and its token
// exchange as well as a listed caller. Two --token files for one account name one pod, as a pod's
// projected tokens for two audiences do, so a registration made with the one file (POST
// /v1/oauth/exchange/pod-sessions, which takes the broker's own audience) names the pod the other
// exchanges as.
//
// --code-flow serves the authorization code flow for one confidential client, whose secret is the
// file's content less surrounding whitespace: the values DISPATCH_SIGNIN_CLIENT_ID and
// DISPATCH_SIGNIN_CLIENT_SECRET take. Each authorization signs in the next person queued at the
// loopback-only control route POST <issuer URL>/_dev/sign-in-as, whose body is the JSON object of
// that person's ID token claims; with none queued, it signs in --sign-in-as's person, and without
// --sign-in-as it refuses the authorization. That person carries both claim shapes Dispatch reads:
// a Google Workspace account of the email's domain (identity.DomainRule), and a Cognito sign-in
// pool user federated from the provider DevIdP, a member of --sign-in-group's group when it is
// given (identity.GroupRule). Dispatch takes a domain only from Google's own issuer, so a Dispatch
// signing people in here runs under DISPATCH_SIGNIN_GROUP set to --sign-in-group's group.
//
// Once every file is written it logs "issuer listening url=<issuer URL>", the value
// BROKER_K8S_OIDC_ISSUER and DISPATCH_SIGNIN_ISSUER take, and serves until it is interrupted. Its
// signing key lives in its memory alone: a restart publishes another, and no token minted before
// it verifies again.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
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

func run(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("dev-oidc-issuer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var tokens tokenFlags
	fs.Var(&tokens, "token", "keep `<audience>=<service account>=<file>`'s token in file (repeatable)")
	var flow codeFlowSpec
	fs.Var(&flow, "code-flow", "serve the sign-in code flow for `<client id>=<client secret file>`")
	fs.StringVar(&flow.signInAs, "sign-in-as", "", "sign each authorization in as `<email>` when none is queued (needs --code-flow)")
	fs.StringVar(&flow.signInGroup, "sign-in-group", "", "make --sign-in-as's person a member of the sign-in pool group `<group>` (needs --sign-in-as)")
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
	if flow.signInAs != "" {
		local, domain, _ := strings.Cut(flow.signInAs, "@")
		switch {
		case flow.clientID == "":
			fmt.Fprintln(stderr, "dev-oidc-issuer: --sign-in-as needs --code-flow")
			return exitUsageError
		case local == "" || domain == "" || strings.Contains(domain, "@"):
			fmt.Fprintf(stderr, "dev-oidc-issuer: --sign-in-as %q is not an email address\n", flow.signInAs)
			return exitUsageError
		}
	}
	if flow.signInGroup != "" && flow.signInAs == "" {
		fmt.Fprintln(stderr, "dev-oidc-issuer: --sign-in-group needs --sign-in-as")
		return exitUsageError
	}
	if flow.clientID == "" {
		return serve(ctx, tokens, nil, tokenLifetime, stderr)
	}
	return serve(ctx, tokens, &flow, tokenLifetime, stderr)
}

// codeFlowSpec is --code-flow, --sign-in-as and --sign-in-group: the one client the code flow
// serves, its secret, the email of the person an authorization signs in as when none is queued,
// empty for none, and the sign-in pool group that person is a member of, empty for none.
type codeFlowSpec struct {
	clientID, clientSecret, signInAs, signInGroup string
}

func (f *codeFlowSpec) String() string { return "" }

// Set parses <client id>=<client secret file> and reads the secret from the file now, so a file
// that cannot be read, or holds nothing but whitespace, is refused before any issuer runs rather
// than serving a client no caller can authenticate as.
func (f *codeFlowSpec) Set(value string) error {
	clientID, file, _ := strings.Cut(value, "=")
	switch {
	case f.clientID != "":
		return errors.New("the code flow serves one client; --code-flow is given twice")
	case clientID == "" || file == "":
		return errors.New("want <client id>=<client secret file>")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read the client secret: %w", err)
	}
	secret := strings.TrimSpace(string(raw))
	if secret == "" {
		return fmt.Errorf("the client secret file %s is empty", file)
	}
	f.clientID, f.clientSecret = clientID, secret
	return nil
}

// devProvider is the federated identity provider every default person's sign-in pool username
// names, as a Cognito pool names the SAML or OIDC provider a person signed in through.
const devProvider = "DevIdP"

// person is the ID token claims of the person email, in both shapes Dispatch reads. The Google
// Workspace account its domain rule (identity.DomainRule) reads: a subject, the email,
// email_verified true, and hd the email's domain. The subject is the address itself, lowercased,
// so every sign-in as one address is one Google account, as it is at Google: a subject that
// changed between sign-ins would read to Dispatch as another account taking the address over.
// And the Cognito sign-in pool user its group rule (identity.GroupRule) reads: the username
// `<provider>_<email>` of a user federated from devProvider, which the identities name, and, when
// group is set, cognito:groups naming it.
func person(email, group string) map[string]any {
	email = strings.ToLower(email)
	_, domain, _ := strings.Cut(email, "@")
	claims := map[string]any{
		"sub": email, "email": email, "email_verified": true, "hd": domain,
		"cognito:username": devProvider + "_" + email,
		"identities":       []map[string]any{{"providerName": devProvider}},
	}
	if group != "" {
		claims["cognito:groups"] = []string{group}
	}
	return claims
}

// signInAsHandler is POST /_dev/sign-in-as: it queues the JSON object of claims its body holds as
// the person the next authorization signs in as. It answers loopback callers only: whoever can
// reach it chooses who signs in, so a listener opened wider than loopback must not open it too.
func signInAsHandler(issuer *oidctest.Issuer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
			http.Error(w, "the sign-in control answers loopback callers only", http.StatusForbidden)
			return
		}
		var claims map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&claims); err != nil || len(claims) == 0 {
			http.Error(w, "want a JSON object of the next sign-in's ID token claims", http.StatusBadRequest)
			return
		}
		issuer.SignInAs(claims)
		w.WriteHeader(http.StatusNoContent)
	})
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
	namespace, name, _ := strings.Cut(strings.TrimPrefix(account, "system:serviceaccount:"), ":")
	switch {
	case audience == "" || account == "" || file == "":
		return errors.New("want <audience>=<service account>=<file>")
	case !strings.HasPrefix(account, "system:serviceaccount:") || namespace == "" || name == "" || strings.Contains(name, ":"):
		return fmt.Errorf("service account %q is not system:serviceaccount:<namespace>:<name>", account)
	}
	*f = append(*f, tokenSpec{audience: audience, account: account, file: file})
	return nil
}

// serve runs the issuer until ctx ends, writing every spec's token before it reports its URL and
// again each time four fifths of lifetime has passed, and serving flow's code flow when flow is not
// nil. A token it cannot write ends it, 1.
func serve(ctx context.Context, specs []tokenSpec, flow *codeFlowSpec, lifetime time.Duration, stderr io.Writer) int {
	p := &process{stderr: stderr}
	defer p.stop()
	issuer := oidctest.New(p)
	key := issuer.PublishKey(p, "dev-signing-key")
	if flow != nil {
		issuer.EnableCodeFlow(key, flow.clientID, flow.clientSecret)
		if flow.signInAs != "" {
			issuer.SignInByDefault(person(flow.signInAs, flow.signInGroup))
		}
		issuer.Handle("POST /_dev/sign-in-as", signInAsHandler(issuer))
	}
	// Each account's pod, a random UUID as the API server assigns a pod's uid, shared by every
	// token of that account: one pod carries one projected token per audience.
	uids := map[string]string{}
	for i, spec := range specs {
		if _, known := uids[spec.account]; !known {
			uids[spec.account] = uuid.NewString()
		}
		specs[i].podUID = uids[spec.account]
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
	if flow != nil {
		signsIn := "nobody until one is queued"
		if flow.signInAs != "" {
			signsIn = strings.ToLower(flow.signInAs)
		}
		fmt.Fprintf(stderr, "dev-oidc-issuer: code flow serves client %s; an authorization with none queued at /_dev/sign-in-as signs in %s\n", flow.clientID, signsIn)
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

// writeToken mints spec's token, issued now and expiring lifetime from now, and replaces spec.file
// with it in one rename, so a reader sees the old token or the new one and never a part of either.
func writeToken(issuer *oidctest.Issuer, key *oidctest.Key, p *process, spec tokenSpec, lifetime time.Duration) error {
	now := time.Now()
	claims := issuer.Claims(spec.account, spec.audience)
	claims["iat"] = now.Unix()
	claims["exp"] = now.Add(lifetime).Unix()
	claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": spec.podUID}}
	token := issuer.Mint(p, key, claims)
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
