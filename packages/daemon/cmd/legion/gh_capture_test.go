package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
)

// capturedRequest is one request a real gh or git sent to GitHub through the capture proxy: the
// owner of the repository it targets, read from its path or its GraphQL variables ("" for a
// request naming none), and the credential it carried.
type capturedRequest struct {
	host, path, owner, auth string
}

// captureProxy is an HTTPS proxy that answers CONNECT for any host with a certificate of its own
// CA, so a real gh or git told HTTPS_PROXY and that CA sends it every request it means for GitHub,
// which it records and refuses (401 to an unauthenticated git request, so git asks its credential
// helper, 403 or 404 otherwise). Nothing reaches GitHub.
type captureProxy struct {
	url, caFile string
	mu          sync.Mutex
	requests    []capturedRequest
}

var capturedRESTOwner = regexp.MustCompile(`^/repos/([^/]+)/`)
var capturedGitOwner = regexp.MustCompile(`^/([^/]+)/[^/]+/info/refs$`)

func startCaptureProxy(t *testing.T) *captureProxy {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "legion capture CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	p := &captureProxy{caFile: filepath.Join(t.TempDir(), "ca.pem")}
	if err := os.WriteFile(p.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	var serial int64 = 1
	var certMu sync.Mutex
	leaf := func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		certMu.Lock()
		serial++
		n := serial
		certMu.Unlock()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
			SerialNumber: big.NewInt(n), Subject: pkix.Name{CommonName: hello.ServerName}, DNSNames: []string{hello.ServerName},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
		}, ca, &key.PublicKey, caKey)
		if err != nil {
			return nil, err
		}
		return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
	}

	inner := &chanListener{conns: make(chan net.Conn), done: make(chan struct{})}
	server := &http.Server{Handler: http.HandlerFunc(p.serve), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(inner) }()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
		_ = server.Close()
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				reader := bufio.NewReader(conn)
				req, err := http.ReadRequest(reader)
				if err != nil || req.Method != http.MethodConnect {
					_ = conn.Close()
					return
				}
				if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
					_ = conn.Close()
					return
				}
				tlsConn := tls.Server(&bufferedConn{Conn: conn, reader: reader}, &tls.Config{GetCertificate: leaf, NextProtos: []string{"http/1.1"}})
				select {
				case inner.conns <- tlsConn:
				case <-inner.done:
					_ = conn.Close()
				}
			}()
		}
	}()
	p.url = "http://" + ln.Addr().String()
	return p
}

// serve records every request, whatever its host, and refuses it. The test runs gh with its
// telemetry off, so a request for any host but GitHub's API and git host is one the wrapper let
// through to a host it never means.
func (p *captureProxy) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	request := capturedRequest{host: r.Host, path: r.URL.Path, auth: r.Header.Get("Authorization")}
	switch {
	case capturedRESTOwner.MatchString(r.URL.Path):
		request.owner = strings.ToLower(capturedRESTOwner.FindStringSubmatch(r.URL.Path)[1])
	case capturedGitOwner.MatchString(r.URL.Path):
		request.owner = strings.ToLower(capturedGitOwner.FindStringSubmatch(r.URL.Path)[1])
	case r.URL.Path == "/graphql":
		var query struct {
			Variables map[string]any `json:"variables"`
		}
		if json.Unmarshal(body, &query) == nil {
			if owner, ok := query.Variables["owner"].(string); ok {
				request.owner = strings.ToLower(owner)
			}
		}
	}
	p.mu.Lock()
	p.requests = append(p.requests, request)
	p.mu.Unlock()
	switch {
	case r.Host == "github.com" && request.auth == "":
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		w.WriteHeader(http.StatusUnauthorized)
	case r.Host == "github.com":
		w.WriteHeader(http.StatusForbidden)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}
}

// take is every request recorded since the last take.
func (p *captureProxy) take() []capturedRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	requests := p.requests
	p.requests = nil
	return requests
}

// chanListener is a net.Listener over the TLS connections the proxy hands it, so an http.Server
// serves each decrypted tunnel.
type chanListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

// bufferedConn is a connection whose first bytes the proxy's CONNECT reader already buffered.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.reader.Read(b) }

// realGHForCapture is the real gh the capture test drives: LEGION_GH_PATH when the test runs in a
// Legion pane or pod, whose `gh` on PATH is the wrapper, else the gh on PATH; a machine with
// neither skips, as the other gh-backed tests do.
func realGHForCapture(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("LEGION_GH_PATH"); path != "" {
		return path
	}
	if path, err := exec.LookPath("gh"); err == nil {
		return path
	}
	t.Skip("no real gh on this machine: the capture test drives one")
	return ""
}

// Through the real gh and the real git, against a capture proxy standing in for GitHub: every
// request `legion gh` and `legion git-credential` let through goes to GitHub alone and carries the
// token of the owner of the repository it targets — -R before the command, a selector after `--`,
// an option's value read as that option's from the leaf command's own help, GH_REPO under an
// explicit endpoint, -R under GH_REPO, --help=false, a name in --repos under --org, -R after an
// optional-value shorthand, every argument of a plural slot, a placeholder gh fills from GH_REPO,
// the working directory's own repository as gh reads its remotes whatever their scheme — the issue
// owner's write token only for the issue owner's repository and for a command naming none run
// where only that owner's remotes are, another owner's read-only token for that owner's, and the
// git gh runs itself asking the wrapper for the owner in its path. A command the wrapper cannot
// read for certain (a gh alias or an extension, run, as `gh help`'s target or after --version, a
// command gh's own help does not list, `gh extension exec`, a search qualifier it cannot read,
// GH_HOST naming another host, an argument a later gh's usage names otherwise), one naming two
// owners — an option or another repository naming one owner while gh takes the command's own from
// the working directory or GH_REPO, or a workspace whose remotes name two — and one naming an owner
// the bundle does not hold are refused with exit 2 and send nothing; no alias or extension runs,
// none is installed, and gh's own help and version answer.
func TestRealGhAndGitSendTheTargetOwnersToken(t *testing.T) {
	gh := realGHForCapture(t)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on this machine")
	}
	proxy := startCaptureProxy(t)
	root := filepath.Join(t.TempDir(), "gh")
	expires := time.Now().Add(time.Hour)
	tokens := map[string]string{"acme": "ghs_acme_write", "sjawhar": "ghs_sjawhar_read"}
	if _, err := ghconfig.WriteBundle(root, ghconfig.Bundle{Default: "acme", Owners: map[string]ghconfig.Rendered{
		"acme":    ghconfig.Render(tokens["acme"], "implement", expires),
		"sjawhar": ghconfig.Render(tokens["sjawhar"], "implement", expires),
	}}); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "legion"), []byte("#!/bin/sh\n"+testMainEnv+"=1 exec '"+os.Args[0]+"' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The pane's `gh` wrapper, first on PATH, as the runtimes install it.
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nexec '"+filepath.Join(bin, "legion")+"' gh \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	base := []string{
		"PATH=" + bin + ":" + filepath.Dir(git) + ":/usr/bin:/bin", "HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "XDG_STATE_HOME=" + filepath.Join(home, ".state"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".data"), "XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"HTTPS_PROXY=" + proxy.url, "https_proxy=" + proxy.url, "NO_PROXY=", "no_proxy=",
		"SSL_CERT_FILE=" + proxy.caFile, "GIT_SSL_CAINFO=" + proxy.caFile,
		"GH_NO_UPDATE_NOTIFIER=1", "GH_PROMPT_DISABLED=1", "GH_SPINNER_DISABLED=1", "NO_COLOR=1",
		"GH_TELEMETRY=false", "DO_NOT_TRACK=1",
		"GH_TOKEN=", "GITHUB_TOKEN=", "GH_HOST=",
		"GH_CONFIG_DIR=" + ghconfig.OwnerDir(root, "acme"), "LEGION_GH_OWNERS_DIR=" + root, "LEGION_GH_PATH=" + gh,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
	}
	runIn := func(dir string, env []string, name string, args ...string) (int, string) {
		cmd := exec.Command(name, args...)
		cmd.Env, cmd.Dir = append(append([]string{}, base...), env...), dir
		out, err := cmd.CombinedOutput()
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode(), string(out)
		}
		if err != nil {
			t.Fatalf("%s %q: %v", name, args, err)
		}
		return 0, string(out)
	}
	run := func(env []string, name string, args ...string) (int, string) { return runIn(home, env, name, args...) }
	wantSent := func(what, owner string, git bool) {
		t.Helper()
		requests := proxy.take()
		if len(requests) == 0 {
			t.Errorf("%s sent GitHub nothing", what)
			return
		}
		want := "token " + tokens[owner]
		if git {
			want = "Basic " + base64.StdEncoding.EncodeToString([]byte(ghconfig.GitUser+":"+tokens[owner]))
		}
		for _, r := range requests {
			if r.host != "api.github.com" && r.host != "github.com" {
				t.Errorf("%s sent %s%s with %q, to a host that is not GitHub's", what, r.host, r.path, r.auth)
				continue
			}
			if git && r.auth == "" {
				continue
			}
			if r.auth != want || (r.owner != "" && r.owner != owner) {
				t.Errorf("%s sent %s%s (target owner %q) with %q, want %s's %q", what, r.host, r.path, r.owner, r.auth, owner, want)
			}
		}
	}

	// The control: the gh under test, run directly with acme's directory, sends acme's token. A gh
	// on PATH that does not — a wrapper of the machine's own that routes credentials elsewhere —
	// is no real gh to prove the wrapper against, and LEGION_GH_PATH names the real one.
	run(nil, gh, "api", "repos/acme/widgets")
	if control := proxy.take(); len(control) == 0 || control[0].auth != "token "+tokens["acme"] {
		t.Skipf("%s sent %+v for acme's directory: it does not read GH_CONFIG_DIR; set LEGION_GH_PATH to the real gh", gh, control)
	}

	// acme's directory, the issue owner's, holds gh aliases of its own beside gh's default co
	// (which an aliases key of the file's own replaces, so it is listed again): one naming
	// sjawhar's repository, one nested under pr, and a shell alias; and gh has an extension.
	// Neither alias nor extension may run: each would touch its marker. A gh printing pr view's
	// usage with <URL> for <url>, as no gh this was read from does, stands for a later gh's help.
	ran := filepath.Join(home, "ran")
	config := filepath.Join(ghconfig.OwnerDir(root, "acme"), ghconfig.ConfigFile)
	aliases := ghconfig.Config + "aliases:\n    co: pr checkout\n    pv: pr view -R sjawhar/legion\n    pr pv2: pr view -R sjawhar/legion\n    shx: '!touch " + ran + "-shx'\n"
	if err := os.WriteFile(config, []byte(aliases), 0o600); err != nil {
		t.Fatal(err)
	}
	extension := filepath.Join(home, ".data", "gh", "extensions", "gh-hello")
	if err := os.MkdirAll(extension, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extension, "gh-hello"), []byte("#!/bin/sh\ntouch '"+ran+"-hello'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	drift := filepath.Join(home, "gh-drift")
	if err := os.WriteFile(drift, []byte("#!/bin/sh\nfor last; do :; done\nif [ \"$last\" = --help ]; then\n  '"+gh+"' \"$@\" | sed 's/<url>/<URL>/g'\n  exit\nfi\nexec '"+gh+"' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Every command of the three reviews' reproductions, and controls: each either sends GitHub
	// the token of the owner it targets, and only that token, or is refused with exit 2 and sends
	// nothing. Each asks for a field only the server answers (title, nameWithOwner), so no gh
	// version answers it locally without a request. deploy-key add reads its key file from the
	// working directory.
	if err := os.WriteFile(filepath.Join(home, "key.pub"), []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl legion-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ghEnv := []string{testMainEnv + "=1"}
	// check runs `legion gh args…` in dir: refused with exit 2 and refuse's text, sending nothing,
	// or sending GitHub alone owner's token on every request. A refusal of an option a gh version
	// lacks names both texts, joined by "||", either of which passes.
	check := func(dir string, env, args []string, owner, refuse string) {
		t.Helper()
		what := "legion gh " + strings.Join(args, " ")
		code, out := runIn(dir, append(append([]string{}, ghEnv...), env...), os.Args[0], append([]string{"gh"}, args...)...)
		if refuse != "" {
			if code != 2 || !slices.ContainsFunc(strings.Split(refuse, "||"), func(text string) bool { return strings.Contains(out, text) }) {
				t.Errorf("%s = exit %d, %q; want exit 2 containing %q", what, code, out, refuse)
			}
			if sent := proxy.take(); len(sent) != 0 {
				t.Errorf("%s sent %+v, want nothing", what, sent)
			}
			return
		}
		if code == 0 {
			t.Errorf("%s exit 0 against the capture proxy, which answers nothing: %s", what, out)
		}
		wantSent(what, owner, false)
	}
	for _, tc := range []struct {
		env    []string
		args   []string
		owner  string // the owner whose token every request carries; "" when refused
		refuse string // the refusal's text, exit 2 and no request
	}{
		// The first re-review's reproductions.
		{nil, []string{"-R", "sjawhar/legion", "pr", "view", "1", "--json", "title"}, "sjawhar", ""},
		{nil, []string{"--repo=sjawhar/legion", "pr", "view", "1", "--json", "title"}, "sjawhar", ""},
		{nil, []string{"api", "--", "repos/sjawhar/legion"}, "sjawhar", ""},
		{nil, []string{"pr", "view", "--json", "title", "--", "https://github.com/sjawhar/legion/pull/1"}, "sjawhar", ""},
		{nil, []string{"repo", "deploy-key", "add", "key.pub", "-R", "sjawhar/legion", "--title", "-Racme/widgets"}, "sjawhar", ""},
		{nil, []string{"pr", "--json", "title", "view", "https://github.com/sjawhar/legion/pull/1"}, "", `"--json" comes before the command's name`},
		// The first review's reproductions.
		{[]string{"GH_REPO=acme/widgets"}, []string{"api", "repos/sjawhar/legion"}, "sjawhar", ""},
		{[]string{"GH_REPO=acme/widgets"}, []string{"pr", "view", "1", "-R", "sjawhar/legion", "--json", "title"}, "sjawhar", ""},
		{nil, []string{"pr", "view", "https://github.com/sjawhar/legion/pull/1", "-R", "acme/widgets", "--json", "title"}, "", "more than one owner"},
		{nil, []string{"repo", "view", "--json", "nameWithOwner", "sjawhar/legion"}, "sjawhar", ""},
		// The second re-review's reproductions: --help=false, before the command's name or
		// after; a name in --repos, which gh reads under --org; -c taking its optional value.
		{nil, []string{"api", "repos/sjawhar/legion", "--help=false"}, "sjawhar", ""},
		{nil, []string{"pr", "view", "1", "-R", "sjawhar/legion", "--help=false", "--json", "title"}, "sjawhar", ""},
		{nil, []string{"--help", "pr", "view", "1", "-R", "sjawhar/legion", "--help=false", "--json", "title"}, "sjawhar", ""},
		{nil, []string{"secret", "set", "TOKEN", "--org", "sjawhar", "--repos", "legion", "--body", "dummy", "-R", "sjawhar/legion"}, "sjawhar", ""},
		{nil, []string{"browse", "-c", "-R", "sjawhar/legion", "--no-browser"}, "sjawhar", ""},
		// The gaps it found that fail open: a gh alias at any depth, gh's default co among them;
		// a shell alias; an extension; the Copilot CLI; a search qualifier gh and GitHub read
		// otherwise; GH_HOST naming another host; and a usage spelling an argument otherwise.
		{nil, []string{"pv", "1", "--json", "title"}, "", `"pv" is a gh alias`},
		{nil, []string{"pr", "pv2", "1", "--json", "title"}, "", `"pv2" is a gh alias`},
		{nil, []string{"co", "https://github.com/sjawhar/legion/pull/1"}, "", `"co" is a gh alias`},
		{nil, []string{"shx"}, "", `"shx" is a gh alias`},
		{nil, []string{"hello", "-R", "sjawhar/legion"}, "", `"hello" is a gh extension`},
		// gh 2.65.0 has no copilot; both refuse it before reading its help.
		{nil, []string{"copilot"}, "", "`gh copilot` runs another program"},
		{nil, []string{"search", "issues", "(repo:sjawhar/legion)"}, "", "is a search qualifier this cannot read for certain"},
		{nil, []string{"search", "issues", "repo:sjawhar/legion", "is:open"}, "sjawhar", ""},
		{[]string{"GH_HOST=ghe.example.com"}, []string{"api", "repos/sjawhar/legion"}, "", `GH_HOST is "ghe.example.com"`},
		{[]string{"LEGION_GH_PATH=" + drift}, []string{"pr", "view", "--json", "title", "https://github.com/sjawhar/legion/pull/1"}, "", "<URL>"},
		// The third re-review's: gh's help of an extension runs the extension, so a help target is
		// read as a command, a gh alias or an extension refused; and a plural slot takes one or
		// more arguments, every one a selector.
		{nil, []string{"help", "hello"}, "", `"hello" is a gh extension`},
		{nil, []string{"help", "pr", "pv2"}, "", `"pv2" is a gh alias`},
		{nil, []string{"help", "co"}, "", `"co" is a gh alias`},
		{nil, []string{"issue", "edit", "1", "2", "--title", "capture", "-R", "sjawhar/legion"}, "sjawhar", ""},
		{nil, []string{"issue", "edit", "https://github.com/sjawhar/legion/issues/1", "https://github.com/sjawhar/legion/issues/2", "--title", "capture"}, "sjawhar", ""},
		{nil, []string{"issue", "edit", "https://github.com/sjawhar/legion/issues/1", "https://github.com/acme/widgets/issues/2", "--title", "capture"}, "", "more than one owner"},
		// The final gates' at 96e1672cd: --version before a word gh runs as an alias or an
		// extension; `gh extension exec`; a first word gh's help does not list, an official
		// extension's stub gh installs under CI; an option naming a repository; gh api's colon
		// placeholders, filled from GH_REPO.
		{nil, []string{"--version", "hello"}, "", `"--version" comes before the command's name`},
		{nil, []string{"--version", "shx"}, "", `"--version" comes before the command's name`},
		{nil, []string{"-R", "sjawhar/legion", "--version", "hello"}, "", `"--version" comes before the command's name`},
		{nil, []string{"extension", "exec", "hello"}, "", "`gh extension exec` runs another program"},
		{[]string{"CI=1"}, []string{"stack"}, "", `"stack" is no command gh's own help lists`},
		{nil, []string{"repo", "sync", "acme/widgets", "--source", "sjawhar/legion"}, "", "more than one owner"},
		{[]string{"GH_REPO=sjawhar/legion"}, []string{"api", "graphql", "-F", "o=:owner", "-F", "r=:repo", "-f", "query=query($o:String!,$r:String!){repository(owner:$o,name:$r){id}}"}, "sjawhar", ""},
		// Controls: the issue owner's repository, a command naming none, an owner outside the
		// bundle, and an option the leaf's help does not list.
		{nil, []string{"pr", "view", "--json", "title", "https://github.com/sjawhar/legion/pull/2"}, "sjawhar", ""},
		{nil, []string{"api", "repos/acme/widgets"}, "acme", ""},
		{nil, []string{"api", "graphql", "-f", "query={viewer{login}}"}, "acme", ""},
		{nil, []string{"api", "repos/other/thing"}, "", `"other" is not an owner of this project`},
		{nil, []string{"pr", "view", "1", "-R", "sjawhar/legion", "--bogus"}, "", `"--bogus" is no option`},
	} {
		check(home, tc.env, tc.args, tc.owner, tc.refuse)
	}

	// A command naming no repository, or naming another while gh takes the command's own from the
	// working directory, acts on the repository gh reads from the working directory's git remotes,
	// as gh reads them whatever their scheme: in the issue's own workspace it carries acme's
	// token; in a clone of sjawhar's, sjawhar's; in a clone outside the project, or a workspace
	// whose remotes name two owners, or an option naming another owner than the one gh acts on, it
	// is refused; -R names the repository there.
	workspace := func(remotes [][]string) string {
		dir := t.TempDir()
		for _, args := range append([][]string{{"init", "-q"}}, remotes...) {
			if code, out := runIn(dir, nil, git, args...); code != 0 {
				t.Fatalf("git %q: %s", args, out)
			}
		}
		return dir
	}
	origin := []string{"remote", "add", "origin", "https://github.com/acme/widgets"}
	issueWorkspace := workspace([][]string{origin})
	otherClone := workspace([][]string{{"remote", "add", "origin", "https://github.com/sjawhar/legion"}})
	outsideClone := workspace([][]string{{"remote", "add", "origin", "https://github.com/octocat/hello"}})
	upstream := workspace([][]string{origin, {"remote", "add", "upstream", "git@github.com:sjawhar/legion.git"}})
	setDefault := workspace([][]string{origin, {"config", "remote.origin.gh-resolved", "sjawhar/legion"}})
	gitPlusHTTPS := workspace([][]string{{"remote", "add", "origin", "git+https://github.com/sjawhar/legion"}})
	type row struct {
		dir    string
		env    []string
		args   []string
		owner  string
		refuse string
	}
	rows := []row{
		{issueWorkspace, nil, []string{"pr", "view", "1", "--json", "title"}, "acme", ""},
		{issueWorkspace, nil, []string{"api", "repos/{owner}/{repo}"}, "acme", ""},
		{otherClone, nil, []string{"pr", "view", "1", "--json", "title"}, "sjawhar", ""},
		{otherClone, nil, []string{"repo", "view", "--json", "nameWithOwner"}, "sjawhar", ""},
		{otherClone, nil, []string{"api", "repos/{owner}/{repo}"}, "sjawhar", ""},
		{otherClone, nil, []string{"-R", "acme/widgets", "pr", "view", "1", "--json", "title"}, "acme", ""},
		{outsideClone, nil, []string{"pr", "list", "--json", "title"}, "", `"octocat" is not an owner of this project`},
		{upstream, nil, []string{"pr", "list", "--json", "title"}, "", `remote "upstream" (git@github.com:sjawhar/legion.git)`},
		{setDefault, nil, []string{"pr", "list", "--json", "title"}, "", "remote.origin.gh-resolved sjawhar/legion"},
		// Round 4's: remote spellings gh reads as github.com's (deep 2, quality A, final B1).
		{gitPlusHTTPS, nil, []string{"pr", "view", "1", "--json", "title"}, "sjawhar", ""},
		// Round 4's: an option or another repository naming one owner while gh acts on the
		// working directory's (deep 1, quality B, final B2), and the options of deep 3.
		{issueWorkspace, nil, []string{"pr", "comment", "5", "--body", "https://github.com/sjawhar/legion/pull/2"}, "acme", ""},
		{issueWorkspace, nil, []string{"issue", "edit", "5", "--add-blocked-by", "https://github.com/sjawhar/legion/issues/9"}, "", "more than one owner||is no option"},
		{issueWorkspace, nil, []string{"repo", "fork", "--org", "sjawhar", "--clone=false", "--remote=false"}, "", "more than one owner"},
		{issueWorkspace, nil, []string{"issue", "create", "--title", "t", "--body", "b", "--blocked-by", "1,https://github.com/sjawhar/legion/issues/5"}, "", "more than one owner||is no option"},
		{otherClone, nil, []string{"pr", "comment", "1", "--body", "https://github.com/acme/widgets/pull/9"}, "sjawhar", ""},
		{otherClone, nil, []string{"issue", "close", "3", "--duplicate-of", "https://github.com/acme/widgets/issues/1"}, "", "more than one owner||is no option"},
		{otherClone, nil, []string{"repo", "sync", "--source", "acme/widgets"}, "", "more than one owner"},
		{otherClone, nil, []string{"issue", "develop", "3", "--branch-repo", "acme/widgets", "--name", "x"}, "", "more than one owner"},
		{outsideClone, nil, []string{"pr", "comment", "1", "--body", "https://github.com/sjawhar/legion/pull/2"}, "", `"octocat" is not an owner of this project`},
		{home, []string{"GH_REPO=acme/widgets"}, []string{"issue", "close", "3", "--duplicate-of", "https://github.com/sjawhar/legion/issues/5"}, "", "more than one owner||is no option"},
		{issueWorkspace, []string{"GH_REPO=sjawhar/legion"}, []string{"project", "link", "1", "--owner", "acme"}, "", "more than one owner"},
		// Round 5's: a passthrough leaf's repository after `--` (deep 1); repo set-default's
		// remotes whatever its argument names (deep 2, quality 1); a pr command's OWNER/REPO#N or
		// non-pull-request URL, a branch of its own repository (final F1).
		{issueWorkspace, nil, []string{"repo", "fork", "--", "sjawhar/legion"}, "sjawhar", ""},
		{issueWorkspace, nil, []string{"repo", "fork", "--org", "acme", "--", "sjawhar/legion"}, "", "more than one owner"},
		{issueWorkspace, nil, []string{"repo", "clone", "--", "sjawhar/legion"}, "sjawhar", ""},
		{issueWorkspace, nil, []string{"repo", "set-default", "sjawhar/legion"}, "", "more than one owner"},
		{outsideClone, nil, []string{"pr", "comment", "sjawhar/legion#2", "--body", "hi"}, "", `"octocat" is not an owner of this project`},
		{issueWorkspace, nil, []string{"pr", "view", "sjawhar/legion#2", "--json", "title"}, "acme", ""},
		{issueWorkspace, nil, []string{"pr", "view", "https://github.com/sjawhar/legion/issues/5", "--json", "title"}, "", "more than one owner"},
		{otherClone, nil, []string{"pr", "view", "https://github.com/acme/widgets/tree/x", "--json", "title"}, "", "more than one owner"},
		// Round 6's: agent-task view reads a URL through gh's pr finder too (final, non-blocking);
		// gh 2.65.0 has no agent-task and refuses it as no command its help lists.
		{issueWorkspace, nil, []string{"agent-task", "view", "https://github.com/sjawhar/legion/issues/1"}, "", "more than one owner||no command gh's own help lists"},
	}
	for _, scheme := range []string{"git+https://github.com/sjawhar/legion.git", "ftp://github.com/sjawhar/legion", "ftps://github.com/sjawhar/legion", "file://github.com/sjawhar/legion", "github.com://sjawhar/legion", "git@github.com/sjawhar:legion"} {
		rows = append(rows, row{workspace([][]string{origin, {"remote", "add", "upstream", scheme}}), nil, []string{"pr", "list", "--json", "title"}, "", "more than one owner"})
	}
	for _, tc := range rows {
		check(tc.dir, tc.env, tc.args, tc.owner, tc.refuse)
	}
	// gh's own help, its topics and its version still answer, exit 0, and send nothing.
	for _, args := range [][]string{{"help"}, {"help", "pr", "view"}, {"help", "environment"}, {"pr", "view", "--help"}, {"--version"}} {
		code, out := run(ghEnv, os.Args[0], append([]string{"gh"}, args...)...)
		if code != 0 || out == "" {
			t.Errorf("legion gh %s = exit %d, %q; want gh's help, exit 0", strings.Join(args, " "), code, out)
		}
		if sent := proxy.take(); len(sent) != 0 {
			t.Errorf("legion gh %s sent %+v, want nothing", strings.Join(args, " "), sent)
		}
	}
	for _, marker := range []string{"-shx", "-hello"} {
		if _, err := os.Stat(ran + marker); err == nil {
			t.Errorf("%s ran: `legion gh` ran a gh alias or extension", strings.TrimPrefix(marker, "-"))
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".data", "gh", "extensions", "gh-stack")); err == nil {
		t.Errorf("legion gh stack installed gh's stack extension")
	}

	// The git gh runs itself (a clone, a fork's parent, `pr checkout`) authenticates through the
	// credential helper gh names after its own executable, GH_PATH (quality C). `legion gh` sets it
	// to the pane's wrapper, so the helper gh writes is the wrapper's `auth git-credential`, and
	// git, told useHttpPath as `legion gh` tells gh's git, gets the token of the owner in each
	// request's path from it, never GH_CONFIG_DIR's acme token for sjawhar's repository.
	gitConfig := filepath.Join(home, "gh-gitconfig")
	if code, out := run(append([]string{"GIT_CONFIG_GLOBAL=" + gitConfig}, ghEnv...), os.Args[0], "gh", "auth", "setup-git", "--hostname", "github.com"); code != 0 {
		t.Fatalf("legion gh auth setup-git = exit %d: %s", code, out)
	}
	if sent := proxy.take(); len(sent) != 0 {
		t.Errorf("legion gh auth setup-git sent %+v, want nothing", sent)
	}
	written, err := os.ReadFile(gitConfig)
	if wrapperHelper := "helper = !" + filepath.Join(bin, "gh") + " auth git-credential"; err != nil || !strings.Contains(string(written), wrapperHelper) {
		t.Fatalf("the helper gh wrote = %q, %v; want %q", written, err, wrapperHelper)
	}
	ghGit := []string{"GIT_CONFIG_GLOBAL=" + gitConfig, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.https://github.com.useHttpPath", "GIT_CONFIG_VALUE_0=true"}
	for repo, owner := range map[string]string{"sjawhar/legion": "sjawhar", "Acme/widgets": "acme"} {
		run(ghGit, git, "-c", "credential.interactive=false", "ls-remote", "https://github.com/"+repo)
		wantSent("git ls-remote "+repo+" through the helper gh wrote", owner, true)
	}

	helper := []string{"-c", "credential.helper=", "-c", "credential.helper=!legion git-credential", "-c", "credential.useHttpPath=true", "-c", "credential.interactive=false"}
	for repo, owner := range map[string]string{"sjawhar/legion": "sjawhar", "Acme/widgets": "acme"} {
		run(nil, git, append(helper, "ls-remote", "https://github.com/"+repo)...)
		wantSent("git ls-remote "+repo, owner, true)
	}
	code, out := run(nil, git, append(helper, "ls-remote", "https://github.com/other/thing")...)
	if code == 0 {
		t.Errorf("git ls-remote of an owner the bundle does not hold exit 0: %s", out)
	}
	for _, r := range proxy.take() {
		if r.auth != "" {
			t.Errorf("git ls-remote of other/thing sent %s%s with %q, want no credential", r.host, r.path, r.auth)
		}
	}
}
