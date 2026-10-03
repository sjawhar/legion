//go:build linux

// packages/envoy/cmd/agent-secrets-helper/main.go

// Command agent-secrets-helper is the host side of agent secrets (AGENTC-393): a per-user
// daemon (dotfiles agent-secrets/agent-secrets-helper.service) that pins each registered host
// agent session by pidfd, keeps its P-256 key in memory, enrolls it with the secrets broker as
// kind "host" under the operator's launcher credential, signs broker proofs for the session's
// descendants, and revokes the enrollment when the session's process exits.
//
//	agent-secrets-helper [serve]     run (the unit's ExecStart)
//	agent-secrets-helper sessions    list live sessions: pid, runtime_id, enrollment_id, state
//	agent-secrets-helper --version   the release this binary was built as
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/buildversion"
)

type config struct {
	URL          string
	Socket       string
	OperatorFile string
	StatePath    string
}

func loadConfig(getenv func(string) string) (config, error) {
	home := getenv("HOME")
	cfg := config{
		URL:          strings.TrimSuffix(getenv("AGENT_SECRETS_URL"), "/"),
		Socket:       getenv("AGENT_SECRETS_HELPER_SOCK"),
		OperatorFile: getenv("AGENT_SECRETS_OPERATOR_FILE"),
	}
	// AGENT_SECRETS_URL is checked by serve, not here: `sessions` needs only the socket, and
	// the installer's restart guard runs it without the unit's environment.
	if cfg.Socket == "" {
		cfg.Socket = helper.DefaultSocket(getenv)
	}
	if cfg.OperatorFile == "" {
		cfg.OperatorFile = filepath.Join(home, ".config", "agent-secrets", "operator")
	}
	cfg.StatePath = filepath.Join(filepath.Dir(cfg.Socket), "sessions.json")
	return cfg, nil
}

func main() {
	sub := "serve"
	if len(os.Args) > 1 {
		sub = os.Args[1]
	}
	if sub == "--version" {
		fmt.Printf("agent-secrets-helper %s\n", buildversion.String())
		return
	}
	cfg, err := loadConfig(os.Getenv)
	fatal(err)
	switch sub {
	case "serve":
		fatal(serve(cfg))
	case "sessions":
		fatal(printSessions(cfg.Socket))
	default:
		fmt.Fprintln(os.Stderr, "usage: agent-secrets-helper [serve|sessions|--version]")
		os.Exit(2)
	}
}

// serve runs the daemon: it always listens and serves, whether or not a machine credential has
// ever been installed. There is no startup gate on TokenFile/OperatorFile: the unit never exits
// for want of a credential; a login is a separate ceremony (`agent-secrets launcher login`) run
// against the already-listening socket, and a missing or empty OperatorFile only surfaces later,
// informatively, the first time Login actually needs it for its login_hint. SIGINT or SIGTERM
// stops it, from systemd or from anything else that signals it, and it exits 0, as a requested
// stop does, after a WARN line naming the signal, how many sessions it had registered and whether
// it held a launcher credential, which the next start does not have: the journal says why it
// stopped even when the unit's own log says only that it ended.
func serve(cfg config) error {
	if strings.TrimSpace(cfg.URL) == "" {
		return errors.New("AGENT_SECRETS_URL is required (the secrets broker, e.g. https://secrets.internal.example)")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Socket), 0o700); err != nil {
		return err
	}
	_ = os.Remove(cfg.Socket)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: cfg.Socket, Net: "unix"})
	if err != nil {
		return err
	}
	if err := os.Chmod(cfg.Socket, 0o600); err != nil {
		return err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv := &helper.Server{
		Registry: helper.NewRegistry(cfg.StatePath),
		Broker:   &helper.Broker{URL: cfg.URL, OperatorFile: cfg.OperatorFile, HTTP: &http.Client{Timeout: 30 * time.Second}, Log: log},
		Hostname: hostname,
		PeerOf:   helper.PeerOf,
		Log:      log,
		MinRenew: 30 * time.Second,
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() {
		select {
		case sig := <-signals:
			log.Warn("agent-secrets-helper stopping on a signal", "signal", sig.String(),
				"sessions", len(srv.Registry.List()), "launcher_credential", srv.Broker.HasCredential())
			stop()
		case <-ctx.Done():
		}
	}()
	srv.Recover(ctx)
	// The version says which release a restart came up on. The credential's state is logged where
	// it changes (the Broker's machine-login and refusal lines); a restart never holds one here.
	log.Info("agent-secrets-helper listening", "socket", cfg.Socket, "broker", cfg.URL, "version", buildversion.String())
	return srv.Serve(ctx, ln)
}

func printSessions(sock string) error {
	resp, err := helper.Call(sock, helper.Request{Op: "sessions"}, 5*time.Second)
	if err != nil {
		return fmt.Errorf("agent-secrets-helper is not answering at %s: %w", sock, err)
	}
	if !resp.OK {
		return fmt.Errorf("%s: %s", resp.Code, resp.Error)
	}
	for _, s := range resp.Sessions {
		fmt.Printf("%d\t%s\t%s\t%s\n", s.PID, s.RuntimeID, s.EnrollmentID, s.State)
	}
	return nil
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent-secrets-helper:", err)
		os.Exit(2)
	}
}
