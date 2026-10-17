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

// serve records and refuses a request for GitHub's API or its git host. A request for any other
// host (gh's own telemetry) carries no credential of the bundle's and is answered empty, unrecorded.
func (p *captureProxy) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.Host != "api.github.com" && r.Host != "github.com" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
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
// request `legion gh` and `legion git-credential` let through carries the token of the owner of
// the repository it targets — -R before the command, a selector after `--`, an option's value read
// as that option's from the leaf command's own help, GH_REPO under an explicit endpoint, -R under
// GH_REPO, --help=false, a name in --repos under --org, -R after an optional-value shorthand, every
// argument of a plural slot — the issue owner's write token only for the issue owner's repository
// and for a command naming none, another owner's read-only token for that owner's. A command the
// wrapper cannot read for certain (a gh alias or an extension, run or as `gh help`'s target, a
// search qualifier it cannot read, GH_HOST naming another host, an argument a later gh's usage
// names otherwise), one naming two owners, and one naming an owner the bundle does not hold are
// refused with exit 2 and send nothing; no alias or extension runs, and gh's own help answers.
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
	base := []string{
		"PATH=" + bin + ":" + filepath.Dir(git) + ":/usr/bin:/bin", "HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "XDG_STATE_HOME=" + filepath.Join(home, ".state"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".data"), "XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"HTTPS_PROXY=" + proxy.url, "https_proxy=" + proxy.url, "NO_PROXY=", "no_proxy=",
		"SSL_CERT_FILE=" + proxy.caFile, "GIT_SSL_CAINFO=" + proxy.caFile,
		"GH_NO_UPDATE_NOTIFIER=1", "GH_PROMPT_DISABLED=1", "GH_SPINNER_DISABLED=1", "NO_COLOR=1",
		"GH_TOKEN=", "GITHUB_TOKEN=", "GH_HOST=",
		"GH_CONFIG_DIR=" + ghconfig.OwnerDir(root, "acme"), "LEGION_GH_OWNERS_DIR=" + root, "LEGION_GH_PATH=" + gh,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
	}
	run := func(env []string, name string, args ...string) (int, string) {
		cmd := exec.Command(name, args...)
		cmd.Env, cmd.Dir = append(append([]string{}, base...), env...), home
		out, err := cmd.CombinedOutput()
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode(), string(out)
		}
		if err != nil {
			t.Fatalf("%s %q: %v", name, args, err)
		}
		return 0, string(out)
	}
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
		// gh 2.65.0 has no copilot, and refuses it as a command it does not know.
		{nil, []string{"copilot"}, "", "`gh copilot`"},
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
		// Controls: the issue owner's repository, a command naming none, an owner outside the
		// bundle, and an option the leaf's help does not list.
		{nil, []string{"pr", "view", "--json", "title", "https://github.com/sjawhar/legion/pull/2"}, "sjawhar", ""},
		{nil, []string{"api", "repos/acme/widgets"}, "acme", ""},
		{nil, []string{"api", "graphql", "-f", "query={viewer{login}}"}, "acme", ""},
		{nil, []string{"api", "repos/other/thing"}, "", `"other" is not an owner of this project`},
		{nil, []string{"pr", "view", "1", "-R", "sjawhar/legion", "--bogus"}, "", `"--bogus" is no option`},
	} {
		what := "legion gh " + strings.Join(tc.args, " ")
		code, out := run(append(ghEnv, tc.env...), os.Args[0], append([]string{"gh"}, tc.args...)...)
		if tc.refuse != "" {
			if code != 2 || !strings.Contains(out, tc.refuse) {
				t.Errorf("%s = exit %d, %q; want exit 2 containing %q", what, code, out, tc.refuse)
			}
			if sent := proxy.take(); len(sent) != 0 {
				t.Errorf("%s sent %+v, want nothing", what, sent)
			}
			continue
		}
		if code == 0 {
			t.Errorf("%s exit 0 against the capture proxy, which answers nothing: %s", what, out)
		}
		wantSent(what, tc.owner, false)
	}
	// gh's own help and its topics still answer, exit 0, and send nothing.
	for _, args := range [][]string{{"help"}, {"help", "pr", "view"}, {"help", "environment"}, {"pr", "view", "--help"}} {
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
