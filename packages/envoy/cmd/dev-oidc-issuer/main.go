// Command dev-oidc-issuer is the local stand-in for a Kubernetes cluster's ServiceAccount token
// issuer, so a broker run on one machine (scripts/dev-broker.sh, the broker's docs rig) can list
// its callers in BROKER_API_ACCOUNTS and take a pod's token at its RFC 8693 token exchange with no
// cluster and no Docker. It serves internal/oidc/oidctest's issuer, the one the Go tests mint
// against, on a loopback port, and keeps each token it is asked for in a file, as the kubelet
// keeps a pod's projected token: minted for an hour, the longest the broker's API routes take
// (machine.MaxTokenLoginLifetime), and rewritten in place once four fifths of that has passed.
//
//	dev-oidc-issuer --token <audience>=<service account>=<file> [--token ...]
//
// Each token is bound to a pod of its own (kubernetes.io → pod → uid), kept across rewrites as a
// pod keeps its uid, so a file serves a pod's enrolment and its token exchange as well as a listed
// caller. Once every file is written it logs "issuer listening url=<issuer URL>", the value
// BROKER_K8S_OIDC_ISSUER takes, and serves until it is interrupted. Its signing key lives in its
// memory alone: a restart publishes another, and no token minted before it verifies again.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
	return serve(ctx, tokens, tokenLifetime, stderr)
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
// again each time four fifths of lifetime has passed. A token it cannot write ends it, 1.
func serve(ctx context.Context, specs []tokenSpec, lifetime time.Duration, stderr io.Writer) int {
	p := &process{stderr: stderr}
	defer p.stop()
	issuer := oidctest.New(p)
	key := issuer.PublishKey(p, "dev-signing-key")
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
