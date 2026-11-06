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
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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

// airlockRoot is where every grant's directory lives: under XDG_RUNTIME_DIR when the session has
// one (a tmpfs the session owns, which goes when the session does), else under TMPDIR, else
// under the system temporary directory. Never the command's working directory: a check of this
// change greps every file there for the secret's value, and these files must not be among them.
func airlockRoot(environ []string) string {
	base := envValue(environ, "XDG_RUNTIME_DIR")
	if base == "" {
		base = envValue(environ, "TMPDIR")
	}
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "agent-secrets", "airlock")
}

// prepareAirlock writes the grant's directory — the trust bundle, the wgetrc and the expires file
// that says how long they are worth keeping — and answers the two paths the child's environment
// names. Nothing of this CLI runs once the command starts, so each run first tidies away what
// earlier runs left (tidyAirlockDirs).
//
// The expires file is written last, after both files it describes: a run killed halfway leaves a
// directory with no expires file, which the next run's tidy reads as unfinished and removes.
func prepareAirlock(environ []string, release *AirlockRelease, grantID string, expiresAt *time.Time) (airlockFiles, error) {
	// The grant id becomes one path element, so it must be one: the broker's ids are opaque, and
	// a "." or a ".." would make this run tidy or write somewhere else entirely.
	if grantID == "" || grantID == "." || grantID == ".." || grantID != filepath.Base(grantID) {
		return airlockFiles{}, fmt.Errorf("the grant id %q is not a single path element, so its airlock files have nowhere to go", grantID)
	}
	root := airlockRoot(environ)
	tidyAirlockDirs(root, grantID, time.Now())

	dir := filepath.Join(root, grantID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return airlockFiles{}, fmt.Errorf("make the airlock directory: %w", err)
	}
	// MkdirAll leaves a directory that already existed alone and takes the umask off a new one,
	// so the mode is set rather than asked for.
	if err := os.Chmod(dir, 0o700); err != nil {
		return airlockFiles{}, fmt.Errorf("make the airlock directory private: %w", err)
	}

	files := airlockFiles{bundle: filepath.Join(dir, "ca-bundle.pem"), wgetrc: filepath.Join(dir, "wgetrc")}
	if err := writeFileAtomic(files.bundle, trustBundle(environ, release.CACertificate), 0o600); err != nil {
		return airlockFiles{}, fmt.Errorf("write the trust bundle: %w", err)
	}
	if err := writeFileAtomic(files.wgetrc, wgetrcFor(environ, files.bundle), 0o600); err != nil {
		return airlockFiles{}, fmt.Errorf("write the wgetrc: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "expires"), []byte(expiresText(expiresAt)), 0o600); err != nil {
		return airlockFiles{}, fmt.Errorf("write the airlock expiry: %w", err)
	}
	return files, nil
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

// tidyAirlockDirs removes every directory under root but keep whose grant has expired, and every
// one a run left unfinished (no expires file, or one that cannot be read or parsed). A directory
// whose expires file is empty lasts until its session ends and is left.
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
// certificate. "Would have had" is the file an inherited SSL_CERT_FILE names, and otherwise the
// first system bundle that exists.
//
// When neither is there the bundle is the proxy's certificate alone, and that costs the command
// every other certificate authority: a request the proxy tunnels rather than carries (a host no
// airlock name is bound to) is verified against this bundle and fails. That is the same trust the
// command would have had — none it could name — made visible, and a machine with no system
// bundle at all is one where a command that verifies anything already brings its own.
func trustBundle(environ []string, caPEM string) []byte {
	var bundle []byte
	if inherited, ok := readTrustFile(environ); ok {
		bundle = inherited
		if len(bundle) > 0 && !strings.HasSuffix(string(bundle), "\n") {
			bundle = append(bundle, '\n')
		}
	}
	bundle = append(bundle, caPEM...)
	if !strings.HasSuffix(string(bundle), "\n") {
		bundle = append(bundle, '\n')
	}
	return bundle
}

// readTrustFile is the trust the command would have had, read whole. An SSL_CERT_FILE that names
// a file this process cannot read names no trust the command would have had either, so it falls
// through to the system bundles rather than refusing the run.
func readTrustFile(environ []string) ([]byte, bool) {
	candidates := systemTrustFiles
	if named := envValue(environ, "SSL_CERT_FILE"); named != "" {
		candidates = append([]string{named}, systemTrustFiles...)
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
			if len(out) > 0 && !strings.HasSuffix(string(out), "\n") {
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
