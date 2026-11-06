//go:build linux

// packages/envoy/cmd/agent-secrets-helper/main.go

// Command agent-secrets-helper is the machine side of agent secrets: a per-user
// daemon (the systemd user unit `agent-secrets-helper install` writes) that keeps this machine's login
// (AGENT_SECRETS_MACHINE_FILE) and refreshes it at least daily, pins each registered agent session on this
// machine by pidfd, keeps the session's P-256 key in memory, enrolls it with the secrets broker as
// kind "session" under the machine's launcher credential, signs broker proofs for the session's
// descendants, and revokes the enrollment when the session's process exits.
//
//	agent-secrets-helper [serve]     run (the unit's ExecStart)
//	agent-secrets-helper sessions    list live sessions: pid, runtime_id, enrollment_id, state
//	agent-secrets-helper install [--url URL] [--restart-with-sessions]
//	                                 write and enable this binary's systemd user unit
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
	"github.com/sjawhar/envoy/internal/broker/leasetime"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/buildversion"
	"github.com/sjawhar/envoy/internal/redact"
)

type config struct {
	URL          string
	Socket       string
	OperatorFile string
	MachineFile  string
	StatePath    string
	// KeyDir is where this user's session identity lives (AGENT_SECRETS_KEY_DIR): the key and
	// enrollment of a session on this box, which end with the box.
	KeyDir string
	// SignIns is where `agent-secrets login` keeps this user's sign-ins
	// (brokerclient.SignIns: AGENT_SECRETS_SIGNIN_DIR, else the XDG state directory, which a
	// reboot keeps and a key directory does not). The helper reads that one store and no other
	// user's: it runs as its own user, and the records are that user's own, 0600 in a 0700
	// directory.
	SignIns brokerclient.SignInStore
	// Identity is this machine's own identity at the broker's token exchange
	// (AGENT_SECRETS_MACHINE_IDENTITY and the variables it needs), or nil where this machine
	// proves none and is logged in by a person as before. With one, the helper's launcher
	// credential is the one that exchange mints, and nobody approves this machine at all.
	Identity brokerclient.Source
	// SignIn is the device-flow client a person signed in with (AGENT_SECRETS_SIGNIN_*), which
	// mints their next identity token from the refresh token their record holds, or nil where
	// the deployment names none. Without it, and without Identity, no sign-in on this box can
	// be traded and every session enrols under this machine's own credential.
	SignIn *brokerclient.DeviceFlow
	// Person is AGENT_SECRETS_PERSON in the helper's own environment, which picks one of
	// several sign-ins of this user; empty where it says nothing.
	Person string
	// Session is the session name this machine's own trades write under: the helper acts as no
	// session of its own, so it is AGENT_SECRETS_SESSION's, empty where the deployment's entry
	// fixes one (brokerclient.TradeSession).
	Session string
	// IdentityKeyFile is where a machine that proves an identity of its own keeps the key its
	// launcher credential is bound to (helper.LoadOrMakeIdentityKey), beside the machine login
	// a machine that proves none keeps: ~/.config/agent-secrets/identity-key.pem.
	IdentityKeyFile string
}

func loadConfig(getenv func(string) string) (config, error) {
	home := getenv("HOME")
	cfg := config{
		URL:          brokerclient.WithoutTrailingSlashes(getenv(brokerclient.BrokerURLVar)),
		Socket:       getenv("AGENT_SECRETS_HELPER_SOCK"),
		OperatorFile: getenv("AGENT_SECRETS_OPERATOR_FILE"),
		MachineFile:  getenv("AGENT_SECRETS_MACHINE_FILE"),
		KeyDir:       getenv("AGENT_SECRETS_KEY_DIR"),
		Person:       strings.TrimSpace(getenv(brokerclient.PersonVar)),
		Session:      brokerclient.TradeSession(getenv, ""),
	}
	cfg.SignIns = brokerclient.SignIns(getenv)
	identity, err := brokerclient.MachineIdentity(getenv)
	if err != nil {
		return config{}, err
	}
	cfg.Identity = identity
	signIn, err := brokerclient.SignInFlow(getenv)
	if err != nil {
		return config{}, err
	}
	cfg.SignIn = signIn
	// AGENT_SECRETS_URL is checked by serve, not here: `sessions` needs only the socket, and
	// the installer's restart guard runs it without the unit's environment.
	if cfg.Socket == "" {
		cfg.Socket = helper.DefaultSocket(getenv)
	}
	if cfg.OperatorFile == "" {
		cfg.OperatorFile = filepath.Join(home, ".config", "agent-secrets", "operator")
	}
	if cfg.MachineFile == "" {
		cfg.MachineFile = filepath.Join(home, ".config", "agent-secrets", "machine.json")
	}
	// Beside the machine login, under the one directory a person's own machine credentials
	// already live in, and never in the key directory, which a reboot empties and which a box
	// is given a copy of.
	cfg.IdentityKeyFile = filepath.Join(home, ".config", "agent-secrets", "identity-key.pem")
	if cfg.KeyDir == "" {
		// The default `agent-secrets` itself reads (its keyDir()): the directory the helper's
		// own socket lives in, so both find one place with neither variable set.
		cfg.KeyDir = filepath.Dir(helper.DefaultSocket(getenv))
	}
	cfg.StatePath = filepath.Join(filepath.Dir(cfg.Socket), "sessions.json")
	return cfg, nil
}

const usage = `usage:
  agent-secrets-helper [serve]     run the helper: listen on its socket, sign for the machine sessions
                                   registered with it, and enroll each with the broker
  agent-secrets-helper sessions    list live sessions: pid, runtime id, enrollment id, state
  agent-secrets-helper install [--url URL] [--restart-with-sessions]
                                   write this binary's systemd user unit, daemon-reload and
                                   enable it; restart a running helper only if it has no
                                   sessions, or --restart-with-sessions is given
  agent-secrets-helper --version   print the release this binary was built as

environment:
  AGENT_SECRETS_URL            the broker's base URL; serve needs it, install writes it into the unit
  AGENT_SECRETS_HELPER_SOCK    the helper's socket; install writes it into the unit when set
                               (default $XDG_RUNTIME_DIR/agent-secrets/helper.sock)
  AGENT_SECRETS_OPERATOR_FILE  a file holding the operator's Dispatch login, which a machine login
                               names as its approver; install writes it into the unit when set
                               (default ~/.config/agent-secrets/operator)
  AGENT_SECRETS_MACHINE_FILE   where the helper keeps this machine's login (its key and refresh
                               token, mode 0600), so a restart resumes it; install writes it into
                               the unit when set (default ~/.config/agent-secrets/machine.json).
                               A machine that proves an identity of its own keeps no such file;
                               it keeps the key it trades with in
                               ~/.config/agent-secrets/identity-key.pem, mode 0600
  AGENT_SECRETS_MACHINE_IDENTITY
                               the identity this machine proves at the broker's token exchange:
                               kubernetes (AGENT_SECRETS_MACHINE_TOKEN_FILE), metadata
                               (AGENT_SECRETS_MACHINE_AUDIENCE) or pool (both, with
                               AGENT_SECRETS_POOL_AUDIENCE and AGENT_SECRETS_POOL_ACCOUNT).
                               With it the helper's launcher credential is the one that exchange
                               mints, traded again before it lapses with the key it kept, and no
                               person approves this machine; unset, a person logs it in as before
  AGENT_SECRETS_KEY_DIR        where this user's session identity lives; install writes it into
                               the unit when set (default $XDG_RUNTIME_DIR/agent-secrets)
  AGENT_SECRETS_SIGNIN_DIR     where agent-secrets login keeps this user's sign-ins, which the
                               helper reads to decide whose authority each session enrols under,
                               and reads no other user's; install writes it into the unit when
                               set (default $XDG_STATE_HOME/agent-secrets/signins, which a
                               reboot keeps, where the key dir above does not)
  AGENT_SECRETS_SIGNIN_CLIENT_ID, AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE, AGENT_SECRETS_SIGNIN_ISSUER
                               the device-flow client a person signed in with, which mints their
                               next identity token; without it a sign-in on this box cannot be
                               traded and every session enrols under this machine's credential
  AGENT_SECRETS_PERSON         which of this user's sign-ins a session enrols under, where more
                               than one person has signed in here
  AGENT_SECRETS_SESSION        the session name a trade writes under, where the deployment's
                               entry fixes none

install refuses a relative path in any of these variables: the unit's helper runs elsewhere.
`

func main() {
	sub := "serve"
	if len(os.Args) > 1 {
		sub = os.Args[1]
	}
	switch sub {
	case "--version":
		fmt.Printf("agent-secrets-helper %s\n", buildversion.String())
		return
	case "-h", "-help", "--help", "help":
		fmt.Print(usage)
		return
	}
	cfg, err := loadConfig(os.Getenv)
	fatal(err)
	switch sub {
	case "serve":
		fatal(serve(cfg))
	case "sessions":
		fatal(printSessions(cfg.Socket))
	case "install":
		exe, err := os.Executable()
		fatal(err)
		fatal(install(os.Args[2:], os.Getenv, exe, os.Stdout))
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// serve runs the daemon: it always listens and serves, whether or not a machine credential has
// ever been installed. It refuses to start while another helper holds or answers on its socket
// (claimSocket) or holds its machine file (claimMachineFile). It then resumes the machine login
// kept at MachineFile, if there is one, refreshing it before it installs it and then at least
// daily (Broker.Start); a machine file that is there but does not parse stops it,
// naming the file. There is no startup gate on OperatorFile: a login is a separate ceremony
// (`agent-secrets machine login`) run against the already-listening socket, and a missing or
// empty OperatorFile only surfaces later, informatively, the first time Login actually needs it
// for its login_hint. SIGINT or SIGTERM stops it, from systemd or from anything else that signals
// it, and it exits 0, as a requested stop does, after a WARN line naming the signal, how many
// sessions it had registered and whether it held a launcher credential: the journal says why it
// stopped even when the unit's own log says only that it ended.
func serve(cfg config) error {
	if strings.TrimSpace(cfg.URL) == "" {
		return errors.New("AGENT_SECRETS_URL is required (the secrets broker, e.g. https://secrets.internal.example)")
	}
	hostname, err := os.Hostname()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	identity, err := identityClient(cfg, log)
	if err != nil {
		return err
	}
	srv := &helper.Server{
		Registry: helper.NewRegistry(cfg.StatePath),
		// The two waits the broker's lease bound counts this renewer by, read from the one
		// package that holds them (internal/broker/leasetime): the whole-call timeout a
		// renewal an outage black-holes costs, and the floor under the renew cadence.
		Broker: &helper.Broker{URL: cfg.URL, OperatorFile: cfg.OperatorFile, MachineFile: cfg.MachineFile,
			Identity: identity, SignIns: signInAuthority(cfg, log),
			HTTP: &http.Client{Timeout: leasetime.RenewClientTimeout}, Log: log},
		Hostname: hostname,
		PeerOf:   helper.PeerOf,
		Log:      log,
		MinRenew: leasetime.HelperMinRenew,
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	// The socket and then the machine file are claimed, and held until the process exits, before
	// the kept login is resumed, so a second helper, on this socket or any other, never spends the
	// refresh token the running one holds.
	if err := os.MkdirAll(filepath.Dir(cfg.Socket), 0o700); err != nil {
		return err
	}
	lock, err := claimSocket(cfg.Socket)
	if err != nil {
		return err
	}
	defer lock.Close()
	machineLock, err := claimMachineFile(cfg.MachineFile)
	if err != nil {
		return err
	}
	defer machineLock.Close()
	if err := srv.Broker.Start(ctx); err != nil {
		return err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: cfg.Socket, Net: "unix"})
	if err != nil {
		return err
	}
	if err := os.Chmod(cfg.Socket, 0o600); err != nil {
		return err
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
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
	// it changes (the Broker's resume, machine-login, refresh, expiry-warning and drop lines). The
	// broker's URL is logged without a credential it may carry.
	log.Info("agent-secrets-helper listening", "socket", cfg.Socket, "broker", redact.URL(cfg.URL), "version", buildversion.String())
	return srv.Serve(ctx, ln)
}

// identityClient is the token client for the identity this machine proves, or nil where it
// proves none. The key is this machine's own and only its public half ever reaches the broker:
// it is the key every launcher proof the helper signs is signed with, and it is the key the
// broker names this machine's one launcher credential by (exchange.MachineSubject).
//
// It is read back from IdentityKeyFile, and made and kept there at this machine's first trade
// (helper.LoadOrMakeIdentityKey), so a restart trades the same subject and moves the one
// credential this machine already holds rather than leaving it and its sessions behind. A
// reboot empties neither that file nor this machine's credential; what a reboot does end is
// every session, which is enrolled afresh by the processes that come back.
func identityClient(cfg config, log *slog.Logger) (*brokerclient.Client, error) {
	if cfg.Identity == nil {
		return nil, nil
	}
	key, made, err := helper.LoadOrMakeIdentityKey(cfg.IdentityKeyFile)
	if err != nil {
		return nil, err
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	log.Info("this machine's identity key", "file", cfg.IdentityKeyFile, "thumbprint", thumbprint, "made", made)
	return brokerclient.New(brokerclient.Config{
		BrokerURL: cfg.URL, Key: key, Subject: cfg.Identity, Session: cfg.Session,
		HTTP: brokerclient.HTTPClient(), Log: log,
	})
}

// signInAuthority is the people signed in on this box as this helper's own user, which the
// helper asks before every enrolment, or nil where no sign-in here could be traded: a machine
// that proves no identity of its own has no actor token to present beside a person's, and a
// deployment that names no sign-in client has nothing to mint a person's next identity token
// with. Those are the two halves `agent-secrets login` itself requires, so where either is
// missing there is no sign-in on this box to read and every session enrols under this machine's
// own credential.
//
// The store is this user's own key directory and never another's: a second user on this machine
// runs a helper of their own, as their own user, reading their own 0700 directory, and nothing
// here takes a directory or a person from a session.
func signInAuthority(cfg config, log *slog.Logger) helper.SignIns {
	if cfg.Identity == nil || cfg.SignIn == nil {
		return nil
	}
	return &brokerclient.SignInAuthority{
		Store: cfg.SignIns, Person: cfg.Person,
		Machine: cfg.Identity, Flow: cfg.SignIn,
		BrokerURL: cfg.URL, HTTP: brokerclient.HTTPClient(), Log: log,
	}
}

// claimSocket takes the helper's claim on sock: an exclusive flock on sock+".lock" (0600, beside
// it), which the returned file holds until it is closed or the process exits. Another helper
// holding it refuses the start, naming the socket. With the claim held, a path at sock that is
// not a unix socket is refused, naming it and left as it is; a socket a helper still answers on
// (one started before helpers took the claim) is refused the same way, since unlinking it would
// leave that helper's sessions on a helper nobody reaches; and a socket that refuses connections
// (a helper that died) is stale and removed. Any other dial failure (a timeout, a full accept
// queue) refuses too: only a definite refusal proves nothing listens.
func claimSocket(sock string) (*os.File, error) {
	lock, err := os.OpenFile(sock+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another agent-secrets-helper already holds %s; stop it first", sock)
		}
		return nil, fmt.Errorf("claiming %s: %w", sock, err)
	}
	if err := clearStaleSocket(sock); err != nil {
		lock.Close()
		return nil, err
	}
	return lock, nil
}

// claimMachineFile takes the helper's claim on the machine login at path: an exclusive flock on
// path+".lock" (0600, beside it, its directory created 0700 if absent), which the returned file
// holds until it is closed or the process exits. Another helper holding it, whatever its socket,
// refuses the start, naming the machine file.
func claimMachineFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another agent-secrets-helper already holds the machine login %s; stop it first", path)
		}
		return nil, fmt.Errorf("claiming %s: %w", path, err)
	}
	return lock, nil
}

// clearStaleSocket removes sock when it is a unix socket nothing listens on, and refuses anything
// else there, naming it; nothing there is fine.
func clearStaleSocket(sock string) error {
	info, err := os.Lstat(sock)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	case info.Mode().Type() != os.ModeSocket:
		return fmt.Errorf("%s is there and is not a unix socket (%s); move it, or set AGENT_SECRETS_HELPER_SOCK elsewhere", sock, info.Mode().Type())
	}
	conn, err := net.DialTimeout("unix", sock, time.Second)
	switch {
	case err == nil:
		_ = conn.Close()
		return fmt.Errorf("another agent-secrets-helper already answers at %s; stop it first", sock)
	case !errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("cannot tell whether a helper still listens at %s: %w", sock, err)
	}
	return os.Remove(sock)
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
