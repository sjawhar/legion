package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// bootReads is what boot reads from the configuration, the daemon's environment and the files they
// name before it writes anything (readBoot).
type bootReads struct {
	project, operatorToken, dispatchToken string
	secrets                               map[string]string
	paneNatsUser                          string         // the pane seed's user's public key, "" with no pane seed
	nats                                  natsConnection // the user the daemon's own NATS connection authenticates as
	instructions                          []byte         // nil when the configuration names none
	tmux                                  tmuxReads      // runtime: tmux
	sandbox                               sandboxReads   // runtime: kubernetes
	answers                               answersReads   // with `answers` set, under either runtime
}

// readBoot is every refusal boot makes from the configuration, the daemon's environment (lookup,
// and getenv for the OMP invocation) and the files they name, writing nothing: the project token,
// the operator bearer's file, operator configuration colliding with Legion's own
// (checkOperatorConfig), the launch secrets (readLaunchSecrets), the daemon's own NATS nkey seed
// (natsauth.DaemonSeed), the instructions file, the Dispatch bearer's file, the runtime's own
// reads (readTmux, readSandbox), and, with `answers` set, the answer loop's (readAnswersBoot).
// hostOMP is whether boot runs the host's Oh My Pi, false only for a test that replaced the
// runtime. prepare runs it first; CheckStart is it for `legion start --check-config`, so the check
// refuses whatever boot refuses before its first write. What boot does after it is not a check's to
// do: create the state directory, write the instructions copy and the Dispatch token file, read
// provider keys from secretsd, and run the plugin gate or the image probe.
func readBoot(cfg config.Config, lookup func(string) (string, bool), getenv func(string) string, hostOMP bool, log *slog.Logger) (bootReads, error) {
	var r bootReads
	var err error
	if r.project, err = claim.ProjectToken(cfg.Project); err != nil {
		return bootReads{}, err
	}
	if cfg.OperatorTokenFile == "" {
		return bootReads{}, errors.New("operator_token_file is required: the operator routes that spawn and drive claims authenticate against the bearer it names")
	}
	if r.operatorToken, err = config.ReadSecretPointer("operator_token_file", cfg.OperatorTokenFile); err != nil {
		return bootReads{}, err
	}
	if err := checkOperatorConfig(cfg, lookup); err != nil {
		return bootReads{}, err
	}
	if r.secrets, err = readLaunchSecrets(cfg, lookup); err != nil {
		return bootReads{}, err
	}
	paneSeed := r.secrets[natsauth.SeedVariable]
	if paneSeed != "" {
		if r.paneNatsUser, err = natsauth.PublicKey(paneSeed); err != nil {
			return bootReads{}, err
		}
	}
	daemonSeed, err := natsauth.DaemonSeed(cfg.NatsDaemonNkeySeedFile, lookup)
	if err != nil {
		return bootReads{}, err
	}
	if r.nats, err = chooseNATSConnection(daemonSeed, paneSeed, r.paneNatsUser); err != nil {
		return bootReads{}, err
	}
	if cfg.InstructionsPath != "" {
		if r.instructions, err = config.ReadDeploymentInstructions(cfg.InstructionsPath); err != nil {
			return bootReads{}, err
		}
	}
	if cfg.DispatchURL != "" {
		if r.dispatchToken, err = config.ReadSecretPointer("dispatch_token_file", cfg.DispatchTokenFile); err != nil {
			return bootReads{}, err
		}
	}
	switch cfg.Runtime.Name {
	case "tmux":
		r.tmux, err = readTmux(cfg, lookup, getenv, hostOMP)
	case "kubernetes":
		r.sandbox, err = readSandbox(cfg, r.project, r.dispatchToken, r.paneNatsUser, lookup, log)
	default:
		err = fmt.Errorf("runtime %q is neither tmux nor kubernetes", cfg.Runtime.Name)
	}
	if err != nil {
		return bootReads{}, err
	}
	if cfg.Answers != nil {
		if r.answers, err = readAnswersBoot(*cfg.Answers, r.tmux, lookup); err != nil {
			return bootReads{}, err
		}
	}
	return r, nil
}

// answersReads is what the answer loop needs that readBoot reads: gh, the real gh on the daemon's
// own host its gh tool runs, under either runtime (answersGH); and conversationDSN, the URL of the
// database the conversations live in: answers.conversation_store.dsn_file's under
// runtime.kubernetes.session_store postgres, runtime.tmux.session_dsn_file's under
// runtime.tmux.session_store postgres ("" under any other store). It is the one place the loop
// gets that URL from, whichever key named its file.
type answersReads struct {
	gh              string
	conversationDSN string
}

// readAnswersBoot is the answer loop's share of readBoot, tmux what readTmux read (the zero value
// under kubernetes): its tools, the tmux daemon's own (nil under tmux with no repository), and its
// session database's URL, which under runtime.tmux.session_store postgres is the conversation
// store's (config.ConversationStore.DSNFile names the same file). No provider's key is read: the
// loop reads it for every model call (config.AnswerCredential), so neither boot nor `legion start
// --check-config` runs an api_key_command.
func readAnswersBoot(answers config.Answers, tmux tmuxReads, lookup func(string) (string, bool)) (answersReads, error) {
	var r answersReads
	var err error
	if r.gh, err = answersGH(lookup, tmux.tools); err != nil {
		return answersReads{}, err
	}
	switch file := answers.ConversationStore.DSNFile; {
	case tmux.sessionDSN != "":
		r.conversationDSN = tmux.sessionDSN
	case file != "":
		if r.conversationDSN, err = readPostgresURL("answers.conversation_store.dsn_file", file); err != nil {
			return answersReads{}, err
		}
	}
	return r, nil
}

// readPostgresURL is the postgres:// URL the file a key names holds, refused, naming the key and
// the file and never what it holds, unless it is one.
func readPostgresURL(key, file string) (string, error) {
	dsn, err := config.ReadSecretPointer(key, file)
	if err != nil {
		return "", err
	}
	if parsed, err := url.Parse(dsn); err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		return "", fmt.Errorf("%s names %s, which holds no postgres:// URL", key, file)
	}
	return dsn, nil
}

// natsConnection is the NATS user the daemon's own connection authenticates as: its own seed
// (natsauth.DaemonSeed) when it has one, else the pane seed every pane receives (launchSecrets),
// else no credential. No launch carries it.
type natsConnection struct {
	seed     string         // "" for no credential
	user     string         // seed's user's public key, "" with no seed
	source   natsSeedSource // where seed came from
	paneUser bool           // whether user is the pane seed's user, as when the daemon seed is the pane seed
}

// natsSeedSource is where the daemon's own connection's seed came from, as its boot line names it.
type natsSeedSource string

const (
	natsSeedDaemon natsSeedSource = "daemon" // natsauth.DaemonSeed
	natsSeedPane   natsSeedSource = "pane"   // the pane seed every pane receives
	natsSeedNone   natsSeedSource = "none"   // no seed: no credential
)

// chooseNATSConnection is the connection over daemonSeed and paneSeed, each "" for none, where
// paneUser is paneSeed's user's public key, already read.
func chooseNATSConnection(daemonSeed, paneSeed, paneUser string) (natsConnection, error) {
	switch {
	case daemonSeed != "":
		user, err := natsauth.PublicKey(daemonSeed)
		if err != nil {
			return natsConnection{}, err
		}
		return natsConnection{seed: daemonSeed, user: user, source: natsSeedDaemon, paneUser: user == paneUser}, nil
	case paneSeed != "":
		return natsConnection{seed: paneSeed, user: paneUser, source: natsSeedPane, paneUser: true}, nil
	default:
		return natsConnection{source: natsSeedNone}, nil
	}
}

// log logs, once, the user the connection authenticates as, by public key, whether that is the pane
// user, and where its seed came from; never the seed.
func (c natsConnection) log(log *slog.Logger) {
	log.Info("legion daemon connects to NATS", "user", c.user, "paneUser", c.paneUser, "seed", string(c.source))
}

// tmuxReads is what a daemon on this host needs that readBoot reads: a launch for each harness its
// roles run on (resolveLaunchers), the host's git and jj its own provisioning runs (daemonTools),
// which no pane is told, and, under runtime.tmux.session_store postgres, the URL of the session
// database every pane's Oh My Pi keeps its sessions in, which the boot gate probes (verifyPanes).
type tmuxReads struct {
	launches   map[runtime.Backend]launchReads // Oh My Pi's absent when boot does not run the host's Oh My Pi
	tools      map[string]string               // nil without a repository
	sessionDSN string                          // "" under the files store
}

// readTmux is tmux's share of readBoot: each backend's launch (resolveLaunchers), Oh My Pi's only
// when boot runs the host's Oh My Pi (hostOMP), for a configuration with a repository, the host's
// git and jj Legion runs itself, and under runtime.tmux.session_store postgres the session
// database's URL from runtime.tmux.session_dsn_file, which must be a postgres:// URL.
func readTmux(cfg config.Config, lookup func(string) (string, bool), getenv func(string) string, hostOMP bool) (tmuxReads, error) {
	var r tmuxReads
	var err error
	if r.launches, err = resolveLaunchers(cfg, lookup, getenv, hostOMP); err != nil {
		return tmuxReads{}, err
	}
	if _, ok := cfg.Projects[cfg.Project]; ok {
		if r.tools, err = resolveTools(lookup); err != nil {
			return tmuxReads{}, err
		}
	}
	if file := cfg.Runtime.Tmux.SessionDSNFile; file != "" {
		if r.sessionDSN, err = readPostgresURL("runtime.tmux.session_dsn_file", file); err != nil {
			return tmuxReads{}, err
		}
	}
	return r, nil
}

// CheckStart is `legion start --check-config`'s reading of what boot reads before it writes
// anything (readBoot), over the daemon's environment (lookup, and the process's for the OMP
// invocation): nil when boot would get past every such refusal, and the public keys of the pane
// seed's user and the daemon seed's user, each "" when the daemon has no such seed, never a seed.
func CheckStart(cfg config.Config, lookup func(string) (string, bool)) (paneNatsUser, daemonNatsUser string, err error) {
	r, err := readBoot(cfg, lookup, os.Getenv, true, slog.New(slog.DiscardHandler))
	if err != nil {
		return "", "", err
	}
	if r.nats.source == natsSeedDaemon {
		daemonNatsUser = r.nats.user
	}
	return r.paneNatsUser, daemonNatsUser, nil
}
