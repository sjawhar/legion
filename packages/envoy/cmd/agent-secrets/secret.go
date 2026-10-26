// packages/envoy/cmd/agent-secrets/secret.go
//
// The secret forms: a person reads and writes agent secrets in the broker's own secret store -
// AWS Secrets Manager (secret_aws.go) or GCP Secret Manager (secret_gcp.go), chosen by the
// broker's `store` setting - under their own cloud sign-in, then asks the broker to reread each
// one written, so the change is served at once. The broker is asked only for its settings
// (GET /v1/settings: the namespace, key and which store it uses) and for the reread; every read
// and write is the person's own call to the store, which its own IAM decides.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/redact"
)

type secretForm struct {
	help command
	run  func([]string, io.Writer, io.Writer) int
}

// secretForms is the one table for dispatch, form names and help text.
var secretForms []secretForm

func init() {
	// Register after variable initialization: the handlers use commands for help.
	secretForms = []secretForm{
		{command{"secret list", "agent-secrets secret list [--json] [--profile P]",
			"List every agent secret under your own cloud sign-in: its owner, tier, kind, max grant,\nwhether it has a value, and deletion details. Name the store the rows are in."}, cmdSecretList},
		{command{"secret show", "agent-secrets secret show NAME [--json] [--profile P]",
			"Print an agent secret's store, owner, tier, kind, max grant, dates and versions. Never\nprint its value."}, cmdSecretShow},
		{command{"secret create", "agent-secrets secret create NAME --owner me|shared --tier agent|human [--kind github-app|google-login|none] [--github-owners LOGINS] [--max-grant DURATION] [--profile P]",
			"Create an agent secret under your own cloud sign-in. Then ask the broker to serve it. Read\nits value from standard input, or type it at a prompt with echo off."}, cmdSecretCreate},
		{command{"secret set", "agent-secrets secret set NAME [--profile P]",
			"Set a new agent-secret value under your own cloud sign-in. Then ask the broker to serve it.\nRead the value from standard input, or type it at a prompt with echo off."}, cmdSecretSet},
		{command{"secret retag", "agent-secrets secret retag NAME [--owner me|shared] [--tier agent|human] [--kind github-app|google-login|none] [--github-owners LOGINS|none] [--max-grant DURATION|none] [--profile P]",
			"Change an agent secret's owner, tier, kind, GitHub owners or max grant under your own cloud\nsign-in. Then ask the broker to reread it. none removes a kind, GitHub owners or max grant, in a\nretag of its own. An administrator changes a shared secret's owner and tier."}, cmdSecretRetag},
		{command{"secret delete", "agent-secrets secret delete NAME [--profile P]",
			"Schedule the agent secret NAME's deletion, restorable for 30 days, under your own cloud\nsign-in, then ask the broker to stop serving it at once."}, cmdSecretDelete},
		{command{"secret restore", "agent-secrets secret restore NAME [--profile P]",
			"Cancel the agent secret NAME's scheduled deletion under your own cloud sign-in, then ask\nthe broker to serve it again at once."}, cmdSecretRestore},
	}
	execForm := commands[len(commands)-1]
	commands = commands[:len(commands)-1]
	for _, form := range secretForms {
		commands = append(commands, form.help)
	}
	commands = append(commands, execForm)
}

func secretFormNames() string {
	var names strings.Builder
	for i, form := range secretForms {
		if i != 0 {
			names.WriteString(", ")
		}
		names.WriteString(strings.TrimPrefix(form.help.name, "secret "))
	}
	return names.String()
}

// recoveryWindowDays is the recovery window delete schedules every deletion with, 30 days: the
// longest Secrets Manager allows, during which restore brings the secret back.
const recoveryWindowDays = 30

// sharedRetagRefused is what a refused retag of a shared secret, or of one the person asked to make
// shared, adds to AWS's own refusal: IAM lets only an administrator set a shared secret's owner or
// tier.
const sharedRetagRefused = "a shared secret's owner and tier are an administrator's to change"

// cmdSecret dispatches the secret forms.
func cmdSecret(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, "agent-secrets secret: a form is required: %s\n", secretFormNames())
		return exitUsageError
	}
	switch args[0] {
	case "-h", "-help", "--help":
		for i, form := range secretForms {
			if i > 0 {
				fmt.Fprintln(stderr)
			}
			writeCommandHelp(stderr, form.help)
		}
		return 0
	}
	for _, form := range secretForms {
		if form.help.name == "secret "+args[0] {
			return form.run(args[1:], stdout, stderr)
		}
	}
	fmt.Fprintf(stderr, "agent-secrets secret: unknown form %q; the forms are %s\n", args[0], secretFormNames())
	return exitUsageError
}

// requireSecretBrokerURL refuses a non-https broker URL for the secret forms unless the broker's
// host is loopback (127.0.0.0/8, ::1 or localhost). openSession calls it, so every secret form is
// gated while the exec form and the machine and pod paths (the other newClient callers) keep
// today's behaviour, and a form's -h, which never opens a session, is unaffected. The secret forms
// trust the broker's settings to steer a write, so a plain-http broker a network attacker can answer
// is refused before the first call; an unparseable URL is left to the client, which names it.
func requireSecretBrokerURL(raw string) error {
	base := strings.TrimSuffix(raw, "/")
	if base == "" {
		return nil
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil
	}
	if u.Scheme == "https" || (u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return nil
	}
	return usageErr{fmt.Errorf("AGENT_SECRETS_URL %q must be https, or http only when the broker's host is loopback (127.0.0.1, [::1] or localhost): the secret forms trust the broker's settings, so a plain-http broker on the network is refused", redact.URL(base))}
}

// isLoopbackHost reports whether host is "localhost" (any case) or a loopback IP literal
// (127.0.0.0/8 or ::1).
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

// usageErr is a secret form's usage error, which exits 2.
type usageErr struct{ error }

// secretFail prints a secret form's failure and answers its exit code: 2 for a usage error, 1 for
// anything else.
func secretFail(stderr io.Writer, form string, err error) int {
	fmt.Fprintf(stderr, "agent-secrets %s: %v\n", form, err)
	var usage usageErr
	if errors.As(err, &usage) {
		return exitUsageError
	}
	return 1
}

// secretFlagValues are the secret forms' flags that take a value.
var secretFlagValues = map[string]bool{"owner": true, "tier": true, "kind": true, "github-owners": true, "max-grant": true, "profile": true}

// secretFlags is form's flag set with --profile, which every secret form takes but only the AWS
// backend uses (GCP uses your Google sign-in instead, gcpBackend; a non-empty --profile there is a
// usage error, openSecretBackend).
func secretFlags(form string, stderr io.Writer) (*flag.FlagSet, *string) {
	flags := newFlagSet(form, stderr)
	return flags, flags.String("profile", "", "the AWS profile to sign in with (AWS only; default: AWS_PROFILE, else the AWS SDK's default credential chain)")
}

// secretName is the one NAME a form takes, and its slug: the secret's name under the prefix.
func secretName(positional []string) (name, slug string, err error) {
	if len(positional) != 1 {
		return "", "", usageErr{errors.New("exactly one secret NAME is required")}
	}
	slug, err = policy.NameToSlug(positional[0])
	if err != nil {
		return "", "", usageErr{fmt.Errorf("%q: %w", positional[0], err)}
	}
	return positional[0], slug, nil
}

// secretStdin is where create and set read a secret's value from; tests replace it.
var secretStdin io.Reader = os.Stdin

// stdinTerminal answers r's file descriptor when r is a terminal, where create and set prompt for
// the value instead of reading r to its end; tests replace it.
var stdinTerminal = func(r io.Reader) (fd int, ok bool) {
	f, isFile := r.(*os.File)
	if !isFile || !term.IsTerminal(int(f.Fd())) {
		return 0, false
	}
	return int(f.Fd()), true
}

// Unix core protection stays in place for the process lifetime, including after
// a prompt or piped value reaches the AWS client.
var disableCoreDumps = sync.OnceValue(preventCoreDumps)

// readHidden reads one hidden line from terminal fd, less its line ending; tests
// replace it. It calls prompt to show the label once it holds the terminal with echo
// off, so a prompt started in the background shows nothing over the shell's own line.
// The reader drains bracketed pastes through their end, and input following an
// unbracketed line through a bounded quiet window.
var readHidden = readHiddenAtTerminal

// errMoreThanOneLine is readHidden's answer when more input followed the first line at the prompt,
// as a paste of a value of more than one line does: the prompt reads one line, and the rest would
// reach the shell once the form exits.
var errMoreThanOneLine = errors.New("more than one line was entered at the prompt")

// errPasteCutShort is readHidden's answer when the terminal hung up inside a bracketed paste,
// before its closing mark, or when the paste fell quiet for maxPasteDrain without its closing mark
// coming: what was read is not the whole value, so none of it is.
var errPasteCutShort = errors.New("the paste was cut short before its closing mark")

// errValueCutShort is readHidden's answer when the terminal hung up outside a paste, before the
// person ended the value: what was typed is not necessarily all they meant to enter.
var errValueCutShort = errors.New("the terminal hung up before the value ended")

// A stop discards what the terminal holds unread (the watcher's flush) without a count of
// how much. The entire entry must be refused, even when the reader had already received a prefix.
var errPromptStopped = errors.New("the terminal stopped while reading the value")

// errNoForeground is readHidden's answer when its process group is orphaned while it waits in the
// background (the shell that started it has gone): no shell can ever hand it the terminal, so it
// refuses before showing anything rather than waiting forever.
var errNoForeground = errors.New("no shell can bring the value prompt to the foreground of this terminal")

// errNoValuePrompt is readHidden's answer on a platform the value prompt does not run on
// (secret_prompt_other.go): the value must be piped in.
var errNoValuePrompt = errors.New("no value prompt is available on this platform")

// promptControlByteError is an unhandled control byte typed at the prompt. Its byte lets the form
// turn the reader error into a usage error that names the invisible input.
type promptControlByteError byte

func (e promptControlByteError) Error() string {
	return fmt.Sprintf("the control byte 0x%02x cannot be typed at the prompt", byte(e))
}

func (e promptControlByteError) ControlByte() byte { return byte(e) }

// shellUnsafe is any character outside the set a POSIX shell reads literally in a bare word.
var shellUnsafe = regexp.MustCompile(`[^A-Za-z0-9_./:@-]`)

// shellWord is s as one shell word: bare when every character is literal, else single-quoted with
// each `'` closed, escaped and reopened.
func shellWord(s string) string {
	if s != "" && !shellUnsafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// pipeCommand is the command line a form names for piping a value in: prefix (the backend's
// signInPrefix), agent-secrets, then words, then --profile as the person gave it, each
// shell-quoted, so running it signs in the same way.
func pipeCommand(prefix, profile string, words ...string) string {
	if profile != "" {
		words = append(words, "--profile", profile)
	}
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = shellWord(w)
	}
	return prefix + "agent-secrets " + strings.Join(quoted, " ")
}

// readSecretValue reads the value of the secret name for a form. At a terminal it prompts on
// stderr, once it holds the terminal with echo off, and reads one line (readHidden), as
// `gh secret set` does, so the value never shows on the screen; a value of more than one line is
// refused there, naming pipeTo, the form's own command line (pipeCommand) with the value piped in.
// Otherwise it reads all of secretStdin, less one trailing newline, so `echo` and a file ending
// in a newline give the value without one. An empty value is a usage error either way.
func readSecretValue(name, pipeTo string, stderr io.Writer) (string, error) {
	if err := disableCoreDumps(); err != nil {
		return "", err
	}
	if fd, ok := stdinTerminal(secretStdin); ok {
		prompted := false
		line, err := readHidden(fd, func() {
			prompted = true
			fmt.Fprintf(stderr, "Value for %s: ", name)
		}, func() {
			fmt.Fprintf(stderr, "\nNothing was stored. Press Enter to finish discarding this entry, then run %s again and type the whole value.\n", pipeTo)
		})
		if prompted {
			// Echo is off, so the line ending the person typed never reached the screen.
			fmt.Fprintln(stderr)
		}
		if errors.Is(err, errMoreThanOneLine) {
			return "", usageErr{fmt.Errorf("a value of more than one line must be piped in: %s < FILE", pipeTo)}
		}
		if errors.Is(err, errPromptStopped) {
			return "", usageErr{fmt.Errorf("nothing was stored: %w; run %s again and type the whole value", err, pipeTo)}
		}
		if errors.Is(err, errNoForeground) || errors.Is(err, errNoValuePrompt) {
			return "", usageErr{fmt.Errorf("%w; pipe the value in: %s < FILE", err, pipeTo)}
		}
		var control interface{ ControlByte() byte }
		if errors.As(err, &control) {
			return "", usageErr{fmt.Errorf("the control byte 0x%02x cannot be typed at the prompt; pipe the value in: %s < FILE", control.ControlByte(), pipeTo)}
		}
		if err != nil {
			return "", fmt.Errorf("read the value at the terminal: %w", err)
		}
		if len(line) == 0 {
			return "", usageErr{errors.New("no value was entered at the prompt")}
		}
		if !utf8.Valid(line) {
			return "", usageErr{errors.New("the value must be valid UTF-8")}
		}
		return string(line), nil
	}
	data, err := io.ReadAll(secretStdin)
	if err != nil {
		return "", fmt.Errorf("read the value from standard input: %w", err)
	}
	if !utf8.Valid(data) {
		return "", usageErr{errors.New("the value must be valid UTF-8")}
	}
	value := strings.TrimSuffix(string(data), "\n")
	if value == "" {
		return "", usageErr{errors.New("the value is read from standard input, which was empty")}
	}
	return value, nil
}

// minRecoveryWindowDays is the shortest recovery window Secrets Manager allows a deletion, 7 days.
const minRecoveryWindowDays = 7

// earliestPurge is the earliest moment Secrets Manager can purge a secret scheduled for deletion,
// read back from the DeletedDate it answers for it (DescribeSecret's and ListSecrets'); nil while
// it is not scheduled. DeletedDate is the moment the delete ran (measured against Secrets
// Manager), and neither call answers the recovery window the delete used, which can be anything
// from 7 to 30 days, so the earliest the secret can be gone is DeletedDate plus the 7-day minimum:
// a reader told this date never waits past the purge to restore it. It is the one place that reads
// DeletedDate. delete itself prints DeleteSecret's own DeletionDate, the exact end of the window it
// scheduled.
func earliestPurge(deletedDate *time.Time) *time.Time {
	if deletedDate == nil {
		return nil
	}
	t := deletedDate.AddDate(0, 0, minRecoveryWindowDays)
	return &t
}

// formatTime is t in UTC as RFC 3339, or "-" when Secrets Manager answered none.
func formatTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

// orDash is s, or "-" when it is empty.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// secretBackend is the per-store backend the secret forms call: AWS Secrets Manager
// (secrets.StoreAWS, or "" from a broker that predates the `store` setting) or GCP Secret
// Manager (secrets.StoreGCP), chosen from the broker's GET /v1/settings `store` field. Every
// method takes the secret's name under the prefix (secretName's slug); the full store-side name
// (each backend's own id) is each backend's own concern.
type secretBackend interface {
	// requireAccount refuses, before any read, a sign-in whose account does not hold the agent
	// secrets: a read there would list another account's secrets as if they were the agent
	// secrets. AWS only: GCP needs no equivalent (gcpBackend.requireAccount says why).
	requireAccount(ctx context.Context) error
	// requireWriteSignIn makes sure a write runs under a person's own sign-in, before anything
	// is written - AWS refuses a machine's own role; GCP signs in with the person's gcloud login
	// in place of a machine's Application Default Credentials, and refuses where it cannot - and
	// answers the signed-in person's lowercased email for --owner me.
	requireWriteSignIn(ctx context.Context) (email string, err error)
	// signInPrefix is the shell assignments a command line the form prints for running again
	// (pipeCommand) needs to sign in as this one did when the sign-in came from a variable that
	// may have been set for this command alone: GCP's GOOGLE_APPLICATION_CREDENTIALS when set, and
	// CLOUDSDK_CORE_ACCOUNT when it chose the gcloud login; else "". It ends in a space when it is
	// not empty.
	signInPrefix() string

	list(ctx context.Context) ([]secretView, error)
	show(ctx context.Context, slug string) (secretView, error)
	// create makes the secret holding value with tags, which always hold owner and tier.
	create(ctx context.Context, slug string, tags map[string]string, value string) error
	set(ctx context.Context, slug, value string) error
	// currentTags answers the secret's tags as one read holds them (AWS tags, or Secret Manager
	// annotations with that read's etag), from which retag builds the tags the secret will hold once
	// its write lands, and against which it makes that write.
	currentTags(ctx context.Context, slug string) (heldTags, error)
	// setTags writes tags onto the secret in one call and leaves every other tag as it is.
	// removeTags removes keys from it in one call and leaves every other tag as it is. Each writes
	// against held, and answers errTagsChanged when another write raced it: GCP sends held's
	// annotations, changed, with held's etag, which a raced write fails; AWS's tag calls take no
	// condition, so its write reads the tags back and restores held's (awsBackend.raceCheck).
	setTags(ctx context.Context, slug string, held heldTags, tags map[string]string) error
	removeTags(ctx context.Context, slug string, held heldTags, keys []string) error
	// delete schedules the secret's deletion and answers the moment it happens (AWS:
	// DeleteSecret's own recovery window end; GCP: the expire_time it sets), nil only on error.
	delete(ctx context.Context, slug string) (deletion *time.Time, err error)
	restore(ctx context.Context, slug string) error

	// isAccessDenied reports whether err is the store's own access-denied refusal, used by
	// retag's shared-secret message.
	isAccessDenied(err error) bool

	// close releases the backend's own connection (GCP's gRPC client; AWS's HTTP-based SDK
	// clients need no release). A short CLI process would reclaim it on exit regardless, but a
	// test building many backends in one process leaks goroutines without it.
	close() error
}

// secretSession is one secret form's connection: the broker and the backend (requireAccount,
// requireWriteSignIn) opened for it, and src, the store those reads and writes go to, which
// every answer and every refusal of the form names (source.go).
type secretSession struct {
	ctx     context.Context
	broker  *client
	backend secretBackend
	src     source
}

// Close releases the session's backend connection.
func (s *secretSession) Close() error { return s.backend.close() }

// storeName is the store this session reads and writes, for a line that names where an answer
// came from: the secret forms call the store directly under the person's own cloud sign-in, so
// the store, not the broker, is the source of what they print.
func (s *secretSession) storeName() string { return s.src.storeName() }

// secretFailIn is secretFail for a failure met once the session is open: it names the store the
// form looked in, so no refusal of this CLI's reads as another secrets CLI's answer about the
// name. It is a function rather than a method on secretSession because broker-refgen reads every
// exit code this package returns, and it follows a plain call, never a method's.
func secretFailIn(stderr io.Writer, form string, s *secretSession, err error) int {
	return secretFail(stderr, form, fmt.Errorf("%w; looked in %s", err, s.storeName()))
}

// openSession reads the broker's settings from AGENT_SECRETS_URL and opens the backend its
// `store` field names (openSecretBackend). It checks nothing about the sign-in: connectReader
// and connectWriter do.
func openSession(ctx context.Context, profile string) (*secretSession, error) {
	base := brokerBaseURL()
	if base == "" {
		return nil, usageErr{fmt.Errorf("%s is required: it names the secrets broker whose store these forms read and write", brokerURLVar)}
	}
	if err := requireSecretBrokerURL(base); err != nil {
		return nil, err
	}
	broker := newClient(base)
	settings, err := broker.Settings(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the settings of %s: %w", brokerSource(base), err)
	}
	backend, err := openSecretBackend(ctx, profile, settings)
	if err != nil {
		return nil, err
	}
	return &secretSession{ctx: ctx, broker: broker, backend: backend, src: brokerSource(base).withSettings(settings)}, nil
}

// openSecretBackend builds the backend settings.Store names: AWS Secrets Manager, pinned to the
// key ARN's own account and region (checkSettingsKeyARN), for "" or secrets.StoreAWS - a
// broker that predates `store` names neither - or GCP Secret Manager, pinned to the project
// secrets_prefix and kms_key_name's own shape agree on (checkSettingsKMSKeyName), for
// secrets.StoreGCP. Any other value is refused before any cloud call.
func openSecretBackend(ctx context.Context, profile string, settings Settings) (secretBackend, error) {
	switch settings.Store {
	case "", secrets.StoreAWS:
		if err := checkSettingsKeyARN(settings); err != nil {
			return nil, err
		}
		sm, st, err := awsClients(ctx, profile, settings.AWSRegion)
		if err != nil {
			return nil, err
		}
		return &awsBackend{settings: settings, sm: sm, st: st}, nil
	case secrets.StoreGCP:
		if profile != "" {
			return nil, usageErr{fmt.Errorf("--profile names an AWS profile; this broker's store is %s, which signs in with your Google sign-in (your Application Default Credentials, or your gcloud login), not --profile", settings.Store)}
		}
		if err := checkSettingsKMSKeyName(settings); err != nil {
			return nil, err
		}
		return newGCPBackend(ctx, settings)
	default:
		return nil, fmt.Errorf("the broker's settings name an unknown secret store %q", settings.Store)
	}
}

// connectReader is openSession for a read: on AWS it refuses a sign-in in another account
// (backend.requireAccount), whose secrets are not the agent secrets; GCP needs no equivalent. Any
// sign-in that can read there reads: IAM decides what it may read.
func connectReader(ctx context.Context, profile string) (*secretSession, error) {
	s, err := openSession(ctx, profile)
	if err != nil {
		return nil, err
	}
	if err := s.backend.requireAccount(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// connectWriter is openSession for a write: it makes sure the write runs under a person's own
// sign-in (backend.requireWriteSignIn) before anything is written, and answers their email.
func connectWriter(ctx context.Context, profile string) (*secretSession, string, error) {
	s, err := openSession(ctx, profile)
	if err != nil {
		return nil, "", err
	}
	email, err := s.backend.requireWriteSignIn(ctx)
	if err != nil {
		s.Close()
		return nil, "", err
	}
	return s, email, nil
}

// displayName is the NAME a session asks for the secret named id, or id itself when its name
// under the prefix maps to none (the broker refuses such a secret as name-malformed). Both
// backends share it (awsBackend.displayName, gcpBackend.displayName): their store-side name
// differs, but it maps back to NAME the same way, from settings.SecretsPrefix alone.
func displayName(prefix, id string) string {
	slug := strings.TrimPrefix(id, prefix)
	name := policy.SlugToName(slug)
	if back, err := policy.NameToSlug(name); err != nil || back != slug {
		return id
	}
	return name
}

// rereadAfter asks the broker to reread name after a form's write and prints its answer: exit 0
// when the broker now does what the write meant (serves the secret, or, after a delete, does not),
// 1 when its answer contradicts the write or it could not be asked. restorable is when a deleted
// secret stops being restorable, which only delete names.
func rereadAfter(s *secretSession, stdout, stderr io.Writer, form, name string, wantServed bool, restorable *time.Time) int {
	if _, ok := reread(s, stdout, stderr, form, name, wantServed, restorable); !ok {
		return 1
	}
	return 0
}

// reread is rereadAfter's reread: it asks the broker, prints its answer, and answers it, with ok
// false when the answer contradicts the write or the broker could not be asked, which it printed.
func reread(s *secretSession, stdout, stderr io.Writer, form, name string, wantServed bool, restorable *time.Time) (Reread, bool) {
	r, err := s.broker.RereadSecret(s.ctx, name)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets %s: the write to %s stands, but %s could not be asked to reread %s, so it serves the change only from its next reload: %v\n", form, s.storeName(), s.src.brokerAt(), name, err)
		return r, false
	}
	switch {
	case r.Served && r.Kind != "":
		fmt.Fprintf(stdout, "broker: serving %s from %s (kind %s)\n", name, s.storeName(), r.Kind)
	case r.Served:
		fmt.Fprintf(stdout, "broker: serving %s from %s\n", name, s.storeName())
	case !wantServed && r.Reason == policy.ReasonAbsent:
		fmt.Fprintf(stdout, "broker: %s is deleted in %s (restorable until %s)\n", name, s.storeName(), formatTime(restorable))
	default:
		fmt.Fprintf(stdout, "broker: refusing %s from %s (reason=%s)\n", name, s.storeName(), r.Reason)
	}
	switch {
	case wantServed && !r.Served:
		fmt.Fprintf(stderr, "agent-secrets %s: %s refuses %s after the write to %s (reason=%s)\n", form, s.src.brokerAt(), name, s.storeName(), r.Reason)
		return r, false
	case !wantServed && r.Served:
		fmt.Fprintf(stderr, "agent-secrets %s: %s still serves %s after its delete in %s\n", form, s.src.brokerAt(), name, s.storeName())
		return r, false
	}
	return r, true
}

// secretView is what list and show print of one secret: never its value.
type secretView struct {
	// Name is the NAME a session asks for it as (or its name in the secret store, when it maps to none).
	Name string `json:"name"`
	// Store is the secret store this row was read from, as the broker's settings describe it
	// (source.storeName): a machine can hold another secrets CLI answering the same names from
	// another store, and a reader of these rows must never take one for the other.
	Store string `json:"store"`
	// SecretName is its whole name in the secret store.
	SecretName string `json:"secret_name"`
	Owner      string `json:"owner"`
	Tier       string `json:"tier"`
	Kind       string `json:"kind"`
	// GitHubOwners is a github-app secret's github-owners tag, the accounts the broker mints for.
	GitHubOwners string `json:"github_owners"`
	MaxGrant     string `json:"max_grant"`
	// HasValue is whether it holds a current, servable version (AWS: a version carries AWSCURRENT;
	// GCP: versions/latest is ENABLED), so the broker can release a value.
	HasValue bool `json:"has_value"`
	// Created and LastChanged are absent when the secret store answers none.
	Created     *time.Time `json:"created,omitempty"`
	LastChanged *time.Time `json:"last_changed,omitempty"`
	// Deleted is set while the secret is scheduled for deletion: AWS, the moment the delete ran;
	// GCP, the expire_time it set, the future moment GCP deletes it for good (GCP has no separate
	// scheduling moment to report, so this is not "when its deletion was scheduled" there).
	Deleted *time.Time `json:"deleted,omitempty"`
	// EarliestPurge is the earliest Secrets Manager can purge it, set while it is scheduled for
	// deletion (earliestPurge): restorable at least until then, perhaps longer.
	EarliestPurge *time.Time `json:"earliest_purge,omitempty"`
	// Versions maps each version id to its AWS staging labels, or each GCP version number to its
	// one-element state (ENABLED, DISABLED, DESTROYED). Only show answers it.
	Versions map[string][]string `json:"versions,omitempty"`
}

// withTags is v with the fields a secret's tags (AWS tags, or Secret Manager annotations) give.
func (v secretView) withTags(tags map[string]string) secretView {
	v.Owner, v.Tier = tags[policy.TagOwner], tags[policy.TagTier]
	v.Kind, v.GitHubOwners, v.MaxGrant = tags[policy.TagKind], tags[policy.TagGitHubOwners], tags[policy.TagMaxGrant]
	return v
}

// writeIndentedJSON writes v as indented JSON.
func writeIndentedJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

func cmdSecretList(args []string, stdout, stderr io.Writer) int {
	const form = "secret list"
	flagArgs, positional := splitArgs(args, secretFlagValues)
	flags, profile := secretFlags(form, stderr)
	asJSON := flags.Bool("json", false, "print the secrets as JSON")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) > 0 {
		return secretFail(stderr, form, usageErr{fmt.Errorf("unexpected argument %q", positional[0])})
	}
	s, err := connectReader(context.Background(), *profile)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	defer s.Close()
	views, err := s.backend.list(s.ctx)
	if err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	// sort.Slice's less answers a bool: an int-returning comparator would be read by broker-refgen
	// as an exit code, which every int-returning function in this package is.
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	for i := range views {
		views[i].Store = s.storeName()
	}
	if *asJSON {
		if err := writeIndentedJSON(stdout, views); err != nil {
			return secretFailIn(stderr, form, s, err)
		}
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tOWNER\tTIER\tKIND\tMAX_GRANT\tVALUE\tDELETED\tEARLIEST_PURGE")
	for _, v := range views {
		value := "no"
		if v.HasValue {
			value = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", v.Name, orDash(v.Owner), orDash(v.Tier), orDash(v.Kind), orDash(v.MaxGrant), value, formatTime(v.Deleted), formatTime(v.EarliestPurge))
	}
	if err := tw.Flush(); err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	// The source line and the legend go to stderr, so the table on stdout stays one row per
	// secret for a script. The source names the one store every row was read from, since a
	// machine can hold another secrets CLI listing other secrets under the same names.
	fmt.Fprintf(stderr, "Every row is a secret in %s.\n", s.storeName())
	// The legend names Secrets Manager's own recovery window, so it is printed only for a row
	// that has one (EarliestPurge, AWS-only: GCP's expire_time is the exact purge moment, with
	// no separate recovery window to explain).
	if slices.ContainsFunc(views, func(v secretView) bool { return v.EarliestPurge != nil }) {
		fmt.Fprintf(stderr, "EARLIEST_PURGE is the deletion plus Secrets Manager's %d-day minimum recovery window: restore works at least until then; the window the delete chose may be longer.\n", minRecoveryWindowDays)
	}
	return 0
}

// ---------------------------------------------------------------------------
// show
// ---------------------------------------------------------------------------

func cmdSecretShow(args []string, stdout, stderr io.Writer) int {
	const form = "secret show"
	flagArgs, positional := splitArgs(args, secretFlagValues)
	flags, profile := secretFlags(form, stderr)
	asJSON := flags.Bool("json", false, "print the secret as JSON")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	_, slug, err := secretName(positional)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	s, err := connectReader(context.Background(), *profile)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	defer s.Close()
	view, err := s.backend.show(s.ctx, slug)
	if err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	view.Store = s.storeName()

	if *asJSON {
		if err := writeIndentedJSON(stdout, view); err != nil {
			return secretFailIn(stderr, form, s, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "name: %s\nstore: %s\nsecret_name: %s\nowner: %s\ntier: %s\nkind: %s\ngithub_owners: %s\nmax_grant: %s\ncreated: %s\nlast_changed: %s\n",
		view.Name, view.Store, view.SecretName, orDash(view.Owner), orDash(view.Tier), orDash(view.Kind), orDash(view.GitHubOwners), orDash(view.MaxGrant), formatTime(view.Created), formatTime(view.LastChanged))
	switch {
	case view.EarliestPurge != nil:
		fmt.Fprintf(stdout, "deleted: %s\nearliest_purge: %s (the deletion plus Secrets Manager's %d-day minimum recovery window: restorable at least until then; the window the delete chose may be longer)\n",
			formatTime(view.Deleted), formatTime(view.EarliestPurge), minRecoveryWindowDays)
	case view.Deleted != nil:
		fmt.Fprintf(stdout, "deleted: %s\n", formatTime(view.Deleted))
	}
	versions := make([]string, 0, len(view.Versions))
	for version := range view.Versions {
		versions = append(versions, version)
	}
	slices.Sort(versions)
	if len(versions) == 0 {
		fmt.Fprintln(stdout, "versions: none")
	}
	for _, version := range versions {
		fmt.Fprintf(stdout, "version: %s %s\n", version, strings.Join(view.Versions[version], ","))
	}
	return 0
}

// ---------------------------------------------------------------------------
// create
// ---------------------------------------------------------------------------

func cmdSecretCreate(args []string, stdout, stderr io.Writer) int {
	const form = "secret create"
	flagArgs, positional := splitArgs(args, secretFlagValues)
	flags, profile := secretFlags(form, stderr)
	owner := flags.String("owner", "", "me (your email, from your AWS or Google sign-in) or shared")
	tier := flags.String("tier", "", "agent (its owner's agents use it without asking) or human (a person approves each use)")
	kind := flags.String("kind", "", "github-app, google-login or none (the default)")
	githubOwners := flags.String("github-owners", "", `the GitHub accounts a github-app secret mints for, separated by spaces ("<login> <login>")`)
	maxGrant := flags.String("max-grant", "", "the longest grant duration this secret allows")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	name, slug, err := secretName(positional)
	if err == nil && (*owner == "" || *tier == "") {
		err = usageErr{errors.New("--owner and --tier are required")}
	}
	if err == nil {
		err = checkOwnerTier(*owner, *tier)
	}
	if err == nil {
		err = checkKind(*kind)
	}
	if err == nil {
		*githubOwners, err = checkGitHubOwners(*githubOwners, false)
	}
	if err == nil {
		err = checkGitHubOwnersFor(*kind, *githubOwners, *githubOwners != "")
	}
	if err == nil {
		err = checkMaxGrant(*maxGrant, false)
	}
	if err != nil {
		return secretFail(stderr, form, err)
	}
	s, email, err := connectWriter(context.Background(), *profile)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	defer s.Close()
	ownerValue, err := ownerTag(*owner, email)
	if err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	tags := optionalTags(*kind, *githubOwners, *maxGrant)
	command := []string{"secret", "create", name, "--owner", *owner, "--tier", *tier}
	if *kind != "" {
		command = append(command, "--kind", *kind)
	}
	if *githubOwners != "" {
		command = append(command, "--github-owners", *githubOwners)
	}
	if *maxGrant != "" {
		command = append(command, "--max-grant", *maxGrant)
	}
	value, err := readSecretValue(name, pipeCommand(s.backend.signInPrefix(), *profile, command...), stderr)
	if err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	tags[policy.TagOwner], tags[policy.TagTier] = ownerValue, *tier
	if err := s.backend.create(s.ctx, slug, tags, value); err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	fmt.Fprintf(stdout, "created %s in %s (owner=%s, tier=%s)\n", name, s.storeName(), ownerValue, *tier)
	return rereadAfter(s, stdout, stderr, form, name, true, nil)
}

// ---------------------------------------------------------------------------
// set
// ---------------------------------------------------------------------------

func cmdSecretSet(args []string, stdout, stderr io.Writer) int {
	const form = "secret set"
	flagArgs, positional := splitArgs(args, secretFlagValues)
	flags, profile := secretFlags(form, stderr)
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	name, slug, err := secretName(positional)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	s, _, err := connectWriter(context.Background(), *profile)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	defer s.Close()
	value, err := readSecretValue(name, pipeCommand(s.backend.signInPrefix(), *profile, "secret", "set", name), stderr)
	if err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	if err := s.backend.set(s.ctx, slug, value); err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	fmt.Fprintf(stdout, "set a new value of %s in %s\n", name, s.storeName())
	return rereadAfter(s, stdout, stderr, form, name, true, nil)
}

// ---------------------------------------------------------------------------
// delete, restore
// ---------------------------------------------------------------------------

// cmdSecretDelete schedules a secret's deletion with the longest recovery window (AWS) or at
// recoveryWindowDays from now (GCP's expire_time), never forcing it, so restore can bring the
// secret back until the window ends.
func cmdSecretDelete(args []string, stdout, stderr io.Writer) int {
	const form = "secret delete"
	flagArgs, positional := splitArgs(args, secretFlagValues)
	flags, profile := secretFlags(form, stderr)
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	name, slug, err := secretName(positional)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	s, _, err := connectWriter(context.Background(), *profile)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	defer s.Close()
	deletion, err := s.backend.delete(s.ctx, slug)
	if err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	fmt.Fprintf(stdout, "deleted %s in %s\n", name, s.storeName())
	return rereadAfter(s, stdout, stderr, form, name, false, deletion)
}

func cmdSecretRestore(args []string, stdout, stderr io.Writer) int {
	const form = "secret restore"
	flagArgs, positional := splitArgs(args, secretFlagValues)
	flags, profile := secretFlags(form, stderr)
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	name, slug, err := secretName(positional)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	s, _, err := connectWriter(context.Background(), *profile)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	defer s.Close()
	if err := s.backend.restore(s.ctx, slug); err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	fmt.Fprintf(stdout, "restored %s in %s\n", name, s.storeName())
	return rereadAfter(s, stdout, stderr, form, name, true, nil)
}
