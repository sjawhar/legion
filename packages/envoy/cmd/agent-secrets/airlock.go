// packages/envoy/cmd/agent-secrets/airlock.go
// airlock.go is the command side of the airlock: what `agent-secrets NAME -- command` gives the
// command when the broker holds NAME's value back and answers with a proxy to reach it through
// (client.go's AirlockRelease). The command gets a placeholder where the secret was, the proxy in
// HTTPS_PROXY with that grant's proxy password, a NO_PROXY that keeps the bound hosts going
// through the proxy and everything else direct, and a trust bundle that holds the trust it would
// have had plus the proxy's own certificate. The broker side is internal/broker/airlockproxy and
// internal/broker/requests/airlock.go.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The slots of an airlock name, spelled as the broker's answer spells them
// (requests.SlotAWSAccessKeyID and its neighbours). They are three short strings of a wire
// contract, written out here rather than imported: internal/broker/requests is the broker's
// database machine, and this CLI — which every agent box and pod installs — would link a
// Postgres driver into its binary to borrow three constants. The set is open, and a slot this
// file does not know is not a mistake: it is passed through in the name's own value and read by
// whatever takes that value apart.
const (
	slotAWSAccessKeyID     = "aws-access-key-id"
	slotAWSSecretAccessKey = "aws-secret-access-key"
	slotAWSSessionToken    = "aws-session-token"
)

// airlockProxyUser is the user name of the Proxy-Authorization credential. The proxy reads the
// password alone and names the user part unread (airlockproxy.proxyPassword), so it is a fixed
// word that says which program built the URL rather than the secret's name — a name in a proxy
// URL reaches a proxy log and a client's own error text.
const airlockProxyUser = "agent-secrets"

// airlockFixedNoProxy are the entries every airlock child's NO_PROXY carries beside the
// inherited ones: the loopback addresses, the suffixes of a cluster's and a tailnet's own names,
// the two metadata addresses and the four private ranges. They never reach the airlock proxy,
// which refuses a private address anyway, and a request to one of them must not wait on a proxy
// that will not carry it.
var airlockFixedNoProxy = []string{
	"localhost",
	"127.0.0.1",
	"::1",
	".svc",
	".cluster.local",
	".internal",
	".ts.net",
	"metadata.google.internal",
	"169.254.169.254",
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"100.64.0.0/10",
}

// systemTrustFiles are the usual system trust bundles, in the order they are looked for: Debian
// and its derivatives, Red Hat, SUSE, and the BSD/Alpine layout. The first that exists is the
// trust a command would have had when nothing named one for it.
var systemTrustFiles = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/ca-bundle.pem",
	"/etc/ssl/cert.pem",
}

// airlockFiles are the two files one run writes for its command: the trust bundle every
// bundle variable points at, and the wgetrc that carries that bundle to wget, which reads no
// bundle variable at all.
//
// Neither file holds a credential — the bundle is the public roots plus the proxy's public
// certificate, and the wgetrc is the inherited one plus a path — so the tidying in
// tidyAirlockDirs is hygiene, not containment: nothing is disclosed by a directory that outlives
// its grant.
type airlockFiles struct {
	bundle string // the trust bundle: the inherited or system roots, plus the proxy's certificate
	wgetrc string // a wgetrc naming that bundle, after the lines of an inherited one
}

// envValue is key's value in environ, the environment as os.Environ writes it, and "" when
// environ holds no entry for it. A later entry wins, as execve's own last-wins reading does.
func envValue(environ []string, key string) string {
	value := ""
	for _, e := range environ {
		if k, v, ok := strings.Cut(e, "="); ok && k == key {
			value = v
		}
	}
	return value
}

// airlockRootParts is where every grant's directory lives, in the two pieces makeAirlockRoot
// works on: the base directory, and the elements under it this CLI makes. The base is
// XDG_RUNTIME_DIR when the session has one (a tmpfs the session owns, which goes when the
// session does), else TMPDIR, else the system temporary directory. Never the command's working
// directory: a check of this change greps every file there for the secret's value, and these
// files must not be among them.
//
// The first two are named by the session's own environment and are the session's own, where the
// third is shared with every other user of the machine: a directory called "agent-secrets" there
// is one whoever gets there first holds, and what goes in it is the trust bundle this command
// verifies every direct call against. So the fallback carries this user's id in its first
// element, which gives each user a directory nobody else's run would make, and makeAirlockRoot
// checks that each element it finds is this user's.
//
// The base itself is neither this CLI's to make nor its to judge. XDG_RUNTIME_DIR and TMPDIR
// are the session's own and were there before this run, the system temporary directory is the
// machine's and is world-writable on purpose, and a CLI that refused any of the three would
// refuse the ordinary machine. What it answers for is what it makes itself, which is every
// element below the base.
func airlockRootParts(environ []string) (string, [2]string) {
	base := envValue(environ, "XDG_RUNTIME_DIR")
	if base == "" {
		base = envValue(environ, "TMPDIR")
	}
	if base != "" {
		return base, [2]string{"agent-secrets", "airlock"}
	}
	return os.TempDir(), [2]string{"agent-secrets-" + strconv.Itoa(os.Getuid()), "airlock"}
}

// makeAirlockRoot makes each element of the root below the base, outermost first, checks each
// one as it is made (checkAirlockRoot) and answers the root.
//
// os.Mkdir per element rather than one os.MkdirAll over the lot, because MkdirAll follows
// whatever stands in the way — a directory another user made, or their symbolic link to one —
// and following it is the hole. A run that checked the last element alone would still write
// under a parent somebody else holds, and that user can rename the checked element away
// between the check and the write and leave their own wgetrc and trust bundle in its place,
// which is the pair this command hands its child. An element already there is checked and kept,
// since that is every second run of this user.
//
// Both elements are checked under a session-named base as well, not only under the shared
// temporary directory: XDG_RUNTIME_DIR is a session's own, but TMPDIR is whatever the session
// was started with, and one two users share puts that same rename in the same place.
func makeAirlockRoot(environ []string) (string, error) {
	base, elements := airlockRootParts(environ)
	root := base
	for _, element := range elements {
		root = filepath.Join(root, element)
		if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("make the airlock directory: %w", err)
		}
		if err := checkAirlockRoot(root); err != nil {
			return "", err
		}
	}
	return root, nil
}

// airlockBuildPrefix opens the name of a directory a run is still building. A grant's files are
// written under such a name and renamed into place, so no name that is not a grant id of a
// finished directory is ever read as one, and a tidy can tell the two apart.
const airlockBuildPrefix = ".building-"

// airlockBuildLifetime is how long a directory under airlockBuildPrefix may stand before another
// run's tidy takes it for the leftover of a run that died. Building one is three writes of a few
// hundred kilobytes, which no machine takes a minute over, so an hour of it is a leftover and
// nothing else. The grant directories themselves are never aged out this way: they are meant to
// outlive their run, and how long a grant lasts is in the expires file.
const airlockBuildLifetime = time.Hour

// prepareAirlock writes the grant's directory — the trust bundle, the wgetrc and the expires file
// that says how long they are worth keeping — and answers the two paths the child's environment
// names. Nothing of this CLI runs once the command starts, so each run first tidies away what
// earlier runs left (tidyAirlockDirs).
//
// The directory is built under a name of its own and renamed into place once every file is in
// it, and that rename is the guard between two runs that share a runtime directory — parallel
// tool calls of one agent do: a tidy reads a directory with no expires file as one a run left
// unfinished and removes it, so a directory that stood under its final name half written would
// be swept away while the run writing it still needed it, and that run would either fail
// outright or hand its command a bundle variable naming a file that is no longer there. Under
// the rename the final name appears complete or not at all, however long a run takes.
//
// Every element of the root this CLI makes is checked before any of this (makeAirlockRoot): a
// directory another user holds is one the command's trust bundle may not be written in or read
// from, and the run answers an error rather than writing one.
func prepareAirlock(environ []string, release *AirlockRelease, grantID string, expiresAt *time.Time) (airlockFiles, error) {
	// The grant id becomes one path element, so it must be one: the broker's ids are opaque, and
	// a "." or a ".." would make this run tidy or write somewhere else entirely.
	if grantID == "" || grantID == "." || grantID == ".." || grantID != filepath.Base(grantID) {
		return airlockFiles{}, fmt.Errorf("the grant id %q is not a single path element, so its airlock files have nowhere to go", grantID)
	}
	// Before the tidy, which removes directories under the root, and before anything is
	// written there: a root this user does not hold is one this run neither sweeps nor writes
	// in.
	root, err := makeAirlockRoot(environ)
	if err != nil {
		return airlockFiles{}, err
	}
	tidyAirlockDirs(root, grantID, time.Now())

	build, err := os.MkdirTemp(root, airlockBuildPrefix)
	if err != nil {
		return airlockFiles{}, fmt.Errorf("make the airlock directory: %w", err)
	}
	// A no-op once the rename has taken it, and the clean-up of a run that answers an error
	// before that.
	defer os.RemoveAll(build)
	// MkdirTemp takes the umask off the mode it asks for, so the mode is set rather than asked
	// for; the directory carries it through the rename.
	if err := os.Chmod(build, 0o700); err != nil {
		return airlockFiles{}, fmt.Errorf("make the airlock directory private: %w", err)
	}

	dir := filepath.Join(root, grantID)
	files := airlockFiles{bundle: filepath.Join(dir, "ca-bundle.pem"), wgetrc: filepath.Join(dir, "wgetrc")}
	// Each file is written under the build directory and names the paths the command will read,
	// which are the ones under dir: the wgetrc carries the bundle's path to wget, and a path
	// under the build directory would be gone by the time the command read it.
	if err := writeFileAtomic(filepath.Join(build, "ca-bundle.pem"), trustBundle(environ, release.CACertificate), 0o600); err != nil {
		return airlockFiles{}, fmt.Errorf("write the trust bundle: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(build, "wgetrc"), wgetrcFor(environ, files.bundle), 0o600); err != nil {
		return airlockFiles{}, fmt.Errorf("write the wgetrc: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(build, "expires"), []byte(expiresText(expiresAt)), 0o600); err != nil {
		return airlockFiles{}, fmt.Errorf("write the airlock expiry: %w", err)
	}
	if err := os.Rename(build, dir); err != nil {
		// The final name is this grant's, so what holds it is a run of this same grant that
		// got there first: a finished directory, with this grant's expiry and this proxy's
		// certificate in it. Its files are left as they are rather than written over, since
		// the command of the run that put them there may be reading them now, and they are
		// this run's answer too. A final name held by anything else — a directory no run of
		// this grant finished — is this run's failure to report, and the command is not run
		// on files it cannot trust.
		if _, statErr := os.Stat(filepath.Join(dir, "expires")); statErr != nil {
			return airlockFiles{}, fmt.Errorf("put the airlock directory in place: %w", err)
		}
	}
	return files, nil
}

// checkAirlockRoot refuses a root whose directory this user does not hold alone. Every run of
// this user shares the root, so a run reads and sweeps whatever it finds there, and what it
// writes under it is the trust bundle the command verifies each of its direct calls against: a
// root another user holds is that user's own certificate authority in the command's bundle, and
// the command is not run on files it cannot trust.
//
// Lstat rather than Stat, because a symbolic link is a root whose real directory somebody else
// chose and Stat would answer for what it points at rather than for what is checked here. The
// owner and the mode are the two other ways another user comes to hold it: one who made the
// directory before this run did, and one let in by a mode wider than this user alone.
//
// It is run on each element of the root rather than on the last alone, since a parent another
// user holds is a parent they can rename the element below it out of.
func checkAirlockRoot(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("read the airlock directory %s: %w", root, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("the airlock directory %s is a symbolic link, so its files would go wherever whoever made the link pointed it; remove it", root)
	}
	if !info.IsDir() {
		return fmt.Errorf("the airlock directory %s is not a directory, so the grant's files have nowhere to go; remove it", root)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("the airlock directory %s is owned by uid %d, not this user (uid %d), so the trust bundle written there is one its owner can replace; remove it, or run as its owner", root, st.Uid, os.Getuid())
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("the airlock directory %s is mode %04o, which lets other users write the trust bundle this command verifies its direct calls against; run: chmod 700 %s", root, perm, root)
	}
	return nil
}

// expiresText is what the expires file holds: the grant's expiry in RFC 3339, and the empty
// string for a grant that lasts until its session ends — a directory the next run keeps, since
// its files hold nothing secret and a later run for the same grant writes the same bytes.
func expiresText(expiresAt *time.Time) string {
	if expiresAt == nil {
		return ""
	}
	return expiresAt.Format(time.RFC3339)
}

// tidyAirlockDirs removes every directory under root but keep whose grant has expired, every one
// a run left unfinished (no expires file, or one that cannot be read or parsed), and every
// directory under airlockBuildPrefix that has stood longer than a build can take, which is a
// leftover of a run that died between making it and renaming it into place. A directory whose
// expires file is empty lasts until its session ends and is left.
//
// Tidying is not the run: nothing here is logged, nothing is refused and no error is answered,
// because a command must still run when the runtime directory is read-only, is shared with
// another user's leftovers, or holds something this version does not recognize.
func tidyAirlockDirs(root, keep string, now time.Time) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == keep {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if strings.HasPrefix(entry.Name(), airlockBuildPrefix) {
			// A directory another run is building now, or one a run that died left behind:
			// age is what tells them apart, and taking a young one would be the very race
			// the rename is there to close.
			if info, err := entry.Info(); err == nil && now.Sub(info.ModTime()) > airlockBuildLifetime {
				_ = os.RemoveAll(dir)
			}
			continue
		}
		if airlockDirWorthKeeping(dir, now) {
			continue
		}
		_ = os.RemoveAll(dir)
	}
}

// airlockDirWorthKeeping reads one directory's expires file: an empty one is a grant that lasts
// until its session ends (keep), a time still ahead is a grant still live (keep), and anything
// else — a time past, a file that is not there, unreadable, or not an RFC 3339 time — is not.
func airlockDirWorthKeeping(dir string, now time.Time) bool {
	data, err := os.ReadFile(filepath.Join(dir, "expires"))
	if err != nil {
		return false
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return true
	}
	expires, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return false
	}
	return !expires.Before(now)
}

// trustBundle is the trust the command gets: the trust it would have had, then the proxy's own
// certificate.
//
// "Would have had" is every bundle variable the command inherited (airlockBundleVars, the same
// variables airlockVars will point at this one file) whose value names a file this process can
// read, each file's bytes once and in the variables' own order, with the first system bundle
// that exists standing behind an SSL_CERT_FILE that names nothing readable. A command whose
// environment puts a private authority in NODE_EXTRA_CA_CERTS or REQUESTS_CA_BUNDLE rather than
// in SSL_CERT_FILE keeps it: NO_PROXY sends its calls to that authority's hosts direct, and
// those calls are verified against this bundle and nothing else.
//
// When nothing is readable at all the bundle is the proxy's certificate alone, and that costs
// the command every other certificate authority: a request the proxy tunnels rather than carries
// (a host no airlock name is bound to) is verified against this bundle and fails. That is the
// same trust the command would have had — none it could name — made visible, and a machine with
// no system bundle at all is one where a command that verifies anything already brings its own.
func trustBundle(environ []string, caPEM string) []byte {
	var bundle []byte
	var held [][]byte
	add := func(data []byte) {
		// One file is usually named by several of these variables, and an authority held
		// twice says no more than the same authority held once.
		if slices.ContainsFunc(held, func(have []byte) bool { return bytes.Equal(have, data) }) {
			return
		}
		held = append(held, data)
		bundle = append(bundle, data...)
		if len(bundle) > 0 && !bytes.HasSuffix(bundle, []byte("\n")) {
			bundle = append(bundle, '\n')
		}
	}
	for _, key := range airlockBundleVars {
		if data, ok := readTrustFile(environ, key); ok {
			add(data)
		}
	}
	bundle = append(bundle, caPEM...)
	if !bytes.HasSuffix(bundle, []byte("\n")) {
		bundle = append(bundle, '\n')
	}
	return bundle
}

// readTrustFile is the trust one inherited bundle variable names, read whole, and for
// SSL_CERT_FILE the first system bundle that exists where it names no file this process can
// read — the trust a command with no variable of its own would have had, which is OpenSSL's own
// answer and so stands behind OpenSSL's variable rather than behind each client family's.
//
// A variable naming a file this process cannot read is skipped, and that costs the command
// whatever trust that file held: a direct call to a host that authority signed fails
// verification, exactly as it does on a machine with no system bundle. It is skipped rather than
// refused because such a variable names no trust the command would have had either — the file is
// as unreadable to the command as it is here — and refusing would take away a run that works
// today.
func readTrustFile(environ []string, key string) ([]byte, bool) {
	var candidates []string
	if named := envValue(environ, key); named != "" {
		candidates = append(candidates, named)
	}
	if key == "SSL_CERT_FILE" {
		candidates = append(candidates, systemTrustFiles...)
	}
	for _, path := range candidates {
		if data, err := os.ReadFile(path); err == nil {
			return data, true
		}
	}
	return nil, false
}

// wgetrcFor is the wgetrc the child gets: the lines of an inherited WGETRC, when it names a file
// that can be read, and then the bundle. wget reads none of the bundle variables, so this file is
// the only way the proxy's certificate reaches it.
func wgetrcFor(environ []string, bundle string) []byte {
	var out []byte
	if named := envValue(environ, "WGETRC"); named != "" {
		if data, err := os.ReadFile(named); err == nil {
			out = data
			if len(out) > 0 && !bytes.HasSuffix(out, []byte("\n")) {
				out = append(out, '\n')
			}
		}
	}
	return append(out, "ca_certificate = "+bundle+"\n"...)
}

// airlockBundleVars are the variables that each name the trust bundle: one per client family that
// reads its own. Every one of them takes a file holding a concatenation of PEM certificates.
var airlockBundleVars = []string{
	"SSL_CERT_FILE",                      // OpenSSL, and everything that links it
	"CURL_CA_BUNDLE",                     // curl
	"REQUESTS_CA_BUNDLE",                 // Python requests, and httpx behind it
	"NODE_EXTRA_CA_CERTS",                // Node, which adds these to its built-in roots
	"GIT_SSL_CAINFO",                     // git over HTTPS
	"CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE", // gcloud
	"AWS_CA_BUNDLE",                      // the AWS CLI and SDKs
}

// airlockVars are the environment variables an airlock release adds to the command's
// environment: one per name held back, the AWS pair's own three when a name has them, and then
// the grant's own — the proxy, the NO_PROXY the proxy needs, Node's switch for reading them, and
// the bundle every client family reads.
//
// It refuses a grant that holds two names with AWS slots: one environment has one
// AWS_ACCESS_KEY_ID, so one of the two pairs would silently win. Nobody has that configuration
// today, and a refusal is the answer a person can act on where a silent choice is not.
func airlockVars(environ []string, release *AirlockRelease, files airlockFiles) (map[string]string, error) {
	vars := make(map[string]string, len(release.Names)+len(airlockBundleVars)+6)
	awsFrom := ""
	for _, name := range slices.Sorted(maps.Keys(release.Names)) {
		held := release.Names[name]
		vars[name] = held.Value
		id, hasID := held.Slots[slotAWSAccessKeyID]
		secret, hasSecret := held.Slots[slotAWSSecretAccessKey]
		if !hasID || !hasSecret {
			continue
		}
		if awsFrom != "" {
			return nil, fmt.Errorf("the grant releases an AWS key pair through the airlock for both %s and %s, and one environment holds one AWS_ACCESS_KEY_ID; ask for them in separate calls", awsFrom, name)
		}
		awsFrom = name
		vars["AWS_ACCESS_KEY_ID"] = id
		vars["AWS_SECRET_ACCESS_KEY"] = secret
		if token, ok := held.Slots[slotAWSSessionToken]; ok {
			vars["AWS_SESSION_TOKEN"] = token
		}
	}

	proxy, err := airlockProxyURL(release)
	if err != nil {
		return nil, err
	}
	vars["HTTPS_PROXY"], vars["https_proxy"] = proxy, proxy
	// Node 24 reads HTTPS_PROXY only when this is set; every other client family reads the
	// proxy variables on their own.
	vars["NODE_USE_ENV_PROXY"] = "1"
	noProxy := airlockNoProxy(environ, release)
	vars["NO_PROXY"], vars["no_proxy"] = noProxy, noProxy
	for _, key := range airlockBundleVars {
		vars[key] = files.bundle
	}
	vars["WGETRC"] = files.wgetrc
	return vars, nil
}

// airlockProxyURL is the proxy URL the command gets: the broker's own, with this grant's proxy
// password as the password of its credential. net/url writes the credential, so a password
// holding a character that must be percent-encoded is encoded once and exactly, and the URL
// parses back into the same password.
func airlockProxyURL(release *AirlockRelease) (string, error) {
	parsed, err := url.Parse(release.ProxyURL)
	if err != nil {
		return "", fmt.Errorf("the broker's airlock proxy URL %q cannot be read: %w", release.ProxyURL, err)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("the broker's airlock proxy URL %q names no host", release.ProxyURL)
	}
	parsed.User = url.UserPassword(airlockProxyUser, release.ProxyPassword)
	return parsed.String(), nil
}

// airlockNoProxy is the child's NO_PROXY: the inherited list, less everything that would take a
// bound host's request past the proxy, plus what must never reach the proxy at all.
//
// The last of those is the broker's own host, which is not in the design: the exec form leaves
// AGENT_SECRETS_URL in the child's environment on purpose (sessionIdentityVars), so a nested
// `agent-secrets` call reaches the broker as this same session. Without this entry that nested
// call would go to the broker through the proxy, and the proxy refuses a private address — which
// a broker reached over a tailnet or inside a cluster always is.
func airlockNoProxy(environ []string, release *AirlockRelease) string {
	inherited := envValue(environ, "no_proxy")
	if inherited == "" {
		inherited = envValue(environ, "NO_PROXY")
	}
	var bound []string
	for _, held := range release.Names {
		bound = append(bound, held.Hosts...)
	}

	entries := make([]string, 0, len(airlockFixedNoProxy)+8)
	for _, entry := range strings.Split(inherited, ",") {
		entry = strings.TrimSpace(entry)
		// "*" turns the proxy off for everything, so no bound host would reach it. A list that
		// holds it holds nothing else that matters.
		if entry == "" || entry == "*" || overlapsBoundHost(entry, bound) {
			continue
		}
		entries = append(entries, entry)
	}
	entries = append(entries, airlockFixedNoProxy...)
	if host := brokerHostname(envValue(environ, "AGENT_SECRETS_URL")); host != "" {
		entries = append(entries, host)
	}

	seen := make(map[string]bool, len(entries))
	out := entries[:0]
	for _, entry := range entries {
		key := strings.ToLower(entry)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, entry)
	}
	return strings.Join(out, ",")
}

// brokerHostname is the host of the broker's base URL, without its port, and "" when the URL
// names none. The port is dropped so the entry covers the broker however it is reached, and
// source.go's brokerHost is not reused: that one keeps the port and redacts a URL this one must
// read, because its answer is printed and this one's only ever goes into NO_PROXY.
func brokerHostname(base string) string {
	parsed, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// overlapsBoundHost reports whether a NO_PROXY entry could take a bound host's request past the
// proxy. An entry and a bound host overlap when they name the same host or one is a parent domain
// of the other, each read without its port, its leading dot or its "*." — the two spellings of a
// suffix in a NO_PROXY list and in an airlock entry's hosts. The test is deliberately generous in
// both directions: "api.example.com" in the list is dropped against the bound "*.example.com"
// because it names a host the proxy must carry, and "example.com" is dropped against the bound
// "api.example.com" because every reader of NO_PROXY but none takes a bare domain to cover its
// subdomains. Dropping one entry too many sends a request through the proxy, which carries it;
// keeping one too many sends a secret's request straight out with a placeholder in it.
func overlapsBoundHost(entry string, hosts []string) bool {
	e := noProxyHost(entry)
	if e == "" {
		return false
	}
	for _, host := range hosts {
		h := noProxyHost(host)
		if h == "" {
			continue
		}
		if h == e || strings.HasSuffix(h, "."+e) || strings.HasSuffix(e, "."+h) {
			return true
		}
	}
	return false
}

// noProxyHost is the host a NO_PROXY entry or an airlock entry's host names, lower-cased and
// without the port, the leading dot or the "*." a suffix is written with. A CIDR block or an
// address keeps its own text, which matches no host name.
func noProxyHost(entry string) string {
	host := strings.ToLower(strings.TrimSpace(entry))
	host = strings.TrimPrefix(host, "*")
	host = strings.TrimPrefix(host, ".")
	if strings.HasPrefix(host, "[") {
		// A bracketed IPv6 address, with or without a port.
		if end := strings.Index(host, "]"); end >= 0 {
			return host[1:end]
		}
		return host
	}
	if colon := strings.LastIndex(host, ":"); colon >= 0 && !strings.Contains(host[colon+1:], ":") && !strings.Contains(host, "/") {
		if _, err := strconv.Atoi(host[colon+1:]); err == nil {
			host = host[:colon]
		}
	}
	return host
}
