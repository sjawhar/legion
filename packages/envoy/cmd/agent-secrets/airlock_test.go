// packages/envoy/cmd/agent-secrets/airlock_test.go
//
// What a command gets when the broker holds its secret back: the placeholder, the proxy, the
// NO_PROXY that keeps the bound hosts on it, the trust bundle and the wgetrc beside it, and the
// tidying each run does first. Nothing here holds a credential — a placeholder stands for one,
// and the certificate is a public one — so these tests print what they compare.
package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testAirlockPlaceholder is one placeholder in the broker's own shape: its prefix and 32
// characters of the alphabet requests.mintPlaceholder draws from.
const testAirlockPlaceholder = "agent-secrets-placeholder-aBcD1234aBcD1234aBcD1234aBcD1234"

// testAirlockCertificate is the PEM the release carries as the proxy's certificate authority.
// Nothing here parses it: the bundle is a file of concatenated PEM blocks, and what is tested is
// that the command's bundle holds both this and the trust it came with.
const testAirlockCertificate = "-----BEGIN CERTIFICATE-----\nMIIAIRLOCKPROXYCERTIFICATE\n-----END CERTIFICATE-----\n"

// testRelease is a release of one airlock name bound to hosts, with the slots given.
func testRelease(name string, hosts []string, slots map[string]string) *AirlockRelease {
	return &AirlockRelease{
		ProxyURL:      "http://airlock.broker.example:3128",
		CACertificate: testAirlockCertificate,
		ProxyPassword: testAirlockPlaceholder,
		Names:         map[string]AirlockName{name: {Value: testAirlockPlaceholder, Slots: slots, Hosts: hosts}},
	}
}

// childEnvMap is buildChildEnv's answer as a map, for a test that asks after single variables.
func childEnvMap(t *testing.T, environ []string, values map[string]string, release *AirlockRelease, files airlockFiles) map[string]string {
	t.Helper()
	env, err := buildChildEnv(environ, values, release, files)
	if err != nil {
		t.Fatalf("buildChildEnv: %v", err)
	}
	out := make(map[string]string, len(env))
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			out[k] = v
		}
	}
	return out
}

// TestAirlockNoProxyKeepsTheBoundHostsOnTheProxy pins the one list the airlock gets wrong in both
// directions: an entry that stays takes a secret's request past the proxy with a placeholder in
// it, and an entry that is missing sends a request the proxy refuses (a private address) or
// cannot reach (the broker itself) through it.
func TestAirlockNoProxyKeepsTheBoundHostsOnTheProxy(t *testing.T) {
	release := testRelease("AIRLOCK_ME", []string{"api.example.com", "*.cdn.example.com"}, map[string]string{"value": testAirlockPlaceholder})
	for _, row := range []struct {
		name        string
		environ     []string
		wantEntries []string
		wantGone    []string
	}{
		{
			name: "an inherited star is dropped and a plain entry kept",
			environ: []string{
				"AGENT_SECRETS_URL=https://broker.internal.example",
				"NO_PROXY=*,intranet.example.org",
			},
			wantEntries: []string{"intranet.example.org", "broker.internal.example"},
			wantGone:    []string{"*"},
		},
		{
			name: "an entry naming a bound host is dropped, however it is spelled",
			environ: []string{
				"AGENT_SECRETS_URL=https://broker.internal.example",
				"NO_PROXY=api.example.com,.cdn.example.com,example.com,api.example.com:443,intranet.example.org",
			},
			wantEntries: []string{"intranet.example.org"},
			wantGone:    []string{"api.example.com", ".cdn.example.com", "example.com", "api.example.com:443"},
		},
		{
			name: "the lower-case spelling wins when both are set",
			environ: []string{
				"AGENT_SECRETS_URL=https://broker.internal.example",
				"NO_PROXY=upper.example.org",
				"no_proxy=lower.example.org",
			},
			wantEntries: []string{"lower.example.org"},
			wantGone:    []string{"upper.example.org"},
		},
		{
			name:        "the broker's host without a port",
			environ:     []string{"AGENT_SECRETS_URL=https://broker.internal.example"},
			wantEntries: []string{"broker.internal.example"},
		},
		{
			name:        "the broker's host with a port, which the entry drops",
			environ:     []string{"AGENT_SECRETS_URL=https://broker.internal.example:8443"},
			wantEntries: []string{"broker.internal.example"},
			wantGone:    []string{"broker.internal.example:8443"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := airlockNoProxy(row.environ, release)
			entries := strings.Split(got, ",")
			has := func(want string) bool {
				for _, e := range entries {
					if e == want {
						return true
					}
				}
				return false
			}
			for _, want := range append(append([]string{}, row.wantEntries...), airlockFixedNoProxy...) {
				if !has(want) {
					t.Errorf("NO_PROXY = %q, want an entry %q", got, want)
				}
			}
			for _, unwanted := range row.wantGone {
				if has(unwanted) {
					t.Errorf("NO_PROXY = %q, want no entry %q", got, unwanted)
				}
			}
		})
	}
}

// TestAirlockNoProxyReachesBothSpellings: a client reads one spelling or the other, so the child
// carries the same list under both.
func TestAirlockNoProxyReachesBothSpellings(t *testing.T) {
	release := testRelease("AIRLOCK_ME", []string{"api.example.com"}, map[string]string{"value": testAirlockPlaceholder})
	env := childEnvMap(t, []string{"AGENT_SECRETS_URL=https://broker.internal.example", "NO_PROXY=intranet.example.org"}, nil, release, testAirlockFiles)
	if env["NO_PROXY"] != env["no_proxy"] || env["NO_PROXY"] == "" {
		t.Fatalf("NO_PROXY = %q and no_proxy = %q, want one non-empty list under both", env["NO_PROXY"], env["no_proxy"])
	}
	if env["HTTPS_PROXY"] != env["https_proxy"] || env["HTTPS_PROXY"] == "" {
		t.Fatalf("HTTPS_PROXY = %q and https_proxy = %q, want one non-empty URL under both", env["HTTPS_PROXY"], env["https_proxy"])
	}
}

// TestAirlockAWSSlotsReachTheirOwnVariables: an AWS key pair behind the airlock is the one secret
// whose shape every AWS client reads from three variables of its own rather than from the name.
func TestAirlockAWSSlotsReachTheirOwnVariables(t *testing.T) {
	id, secret, token := testAirlockPlaceholder+"-id", testAirlockPlaceholder+"-secret", testAirlockPlaceholder+"-token"
	for _, row := range []struct {
		name      string
		slots     map[string]string
		wantToken string
	}{
		{
			name:      "a key pair with a session token",
			slots:     map[string]string{"aws-access-key-id": id, "aws-secret-access-key": secret, "aws-session-token": token},
			wantToken: token,
		},
		{
			name:  "a key pair without one",
			slots: map[string]string{"aws-access-key-id": id, "aws-secret-access-key": secret},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			release := testRelease("AWS_KEYS", []string{"*.amazonaws.com"}, row.slots)
			env := childEnvMap(t, []string{"AGENT_SECRETS_URL=https://broker.internal.example"}, nil, release, testAirlockFiles)
			if env["AWS_ACCESS_KEY_ID"] != id || env["AWS_SECRET_ACCESS_KEY"] != secret {
				t.Errorf("AWS_ACCESS_KEY_ID = %q and AWS_SECRET_ACCESS_KEY = %q, want %q and %q", env["AWS_ACCESS_KEY_ID"], env["AWS_SECRET_ACCESS_KEY"], id, secret)
			}
			got, present := env["AWS_SESSION_TOKEN"]
			if row.wantToken == "" && present {
				t.Errorf("AWS_SESSION_TOKEN = %q, want it absent for a pair with no session token", got)
			}
			if row.wantToken != "" && got != row.wantToken {
				t.Errorf("AWS_SESSION_TOKEN = %q, want %q", got, row.wantToken)
			}
		})
	}
}

// TestAirlockRefusesTwoAWSNames: one environment holds one AWS_ACCESS_KEY_ID, so a grant of two
// AWS key pairs behind the airlock is refused rather than silently resolved.
func TestAirlockRefusesTwoAWSNames(t *testing.T) {
	slots := map[string]string{"aws-access-key-id": testAirlockPlaceholder, "aws-secret-access-key": testAirlockPlaceholder}
	release := testRelease("AWS_ONE", []string{"*.amazonaws.com"}, slots)
	release.Names["AWS_TWO"] = AirlockName{Value: testAirlockPlaceholder, Slots: slots, Hosts: []string{"*.amazonaws.com"}}
	_, err := buildChildEnv([]string{"AGENT_SECRETS_URL=https://broker.internal.example"}, nil, release, testAirlockFiles)
	if err == nil {
		t.Fatalf("buildChildEnv accepted two AWS names, want a refusal")
	}
	for _, want := range []string{"AWS_ONE", "AWS_TWO", "AWS_ACCESS_KEY_ID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %s", err, want)
		}
	}
}

// TestAirlockProxyURLCarriesThePasswordAndParsesBack: the proxy reads the password of a Basic
// credential, so the URL must carry it and every client must be able to parse it out again,
// whether or not the placeholder holds a character a URL must escape.
func TestAirlockProxyURLCarriesThePasswordAndParsesBack(t *testing.T) {
	for _, password := range []string{
		testAirlockPlaceholder,
		"needs:escaping/@ and?more#",
	} {
		release := testRelease("AIRLOCK_ME", nil, nil)
		release.ProxyPassword = password
		got, err := airlockProxyURL(release)
		if err != nil {
			t.Fatalf("airlockProxyURL: %v", err)
		}
		parsed, err := url.Parse(got)
		if err != nil {
			t.Fatalf("the proxy URL %q does not parse: %v", got, err)
		}
		if parsed.User == nil {
			t.Fatalf("the proxy URL %q carries no credential", got)
		}
		if parsed.User.Username() != "agent-secrets" {
			t.Errorf("the proxy URL's user is %q, want agent-secrets", parsed.User.Username())
		}
		back, set := parsed.User.Password()
		if !set || back != password {
			t.Errorf("the proxy URL's password reads back as %q (set %v), want %q", back, set, password)
		}
		if parsed.Host != "airlock.broker.example:3128" {
			t.Errorf("the proxy URL's host is %q, want the broker's proxy", parsed.Host)
		}
	}
}

// TestAirlockProxyURLRefusesAnUnusableOne: a URL with no host would leave every client with a
// proxy setting it cannot dial, so it is a refusal rather than a broken environment.
func TestAirlockProxyURLRefusesAnUnusableOne(t *testing.T) {
	release := testRelease("AIRLOCK_ME", nil, nil)
	release.ProxyURL = "not a url at all"
	if _, err := airlockProxyURL(release); err == nil {
		t.Fatalf("airlockProxyURL accepted %q", release.ProxyURL)
	}
}

// TestPrepareAirlockWritesTheTrustBundleAndWgetrc pins the two files: where they are, their
// modes and their directory's, that the bundle holds the trust the command came with as well as
// the proxy's certificate, that the wgetrc keeps the lines of an inherited one and names the
// bundle, and that neither file is in the working directory, which a check of this change greps
// for the secret's value.
func TestPrepareAirlockWritesTheTrustBundleAndWgetrc(t *testing.T) {
	runtimeDir := t.TempDir()
	inheritedRoots := filepath.Join(t.TempDir(), "roots.pem")
	const rootPEM = "-----BEGIN CERTIFICATE-----\nMIIINHERITEDROOT\n-----END CERTIFICATE-----\n"
	if err := os.WriteFile(inheritedRoots, []byte(rootPEM), 0o644); err != nil {
		t.Fatalf("write the inherited roots: %v", err)
	}
	inheritedWgetrc := filepath.Join(t.TempDir(), "wgetrc")
	if err := os.WriteFile(inheritedWgetrc, []byte("tries = 3\n"), 0o644); err != nil {
		t.Fatalf("write the inherited wgetrc: %v", err)
	}
	environ := []string{
		"XDG_RUNTIME_DIR=" + runtimeDir,
		"SSL_CERT_FILE=" + inheritedRoots,
		"WGETRC=" + inheritedWgetrc,
	}
	expires := time.Now().Add(time.Hour)
	files, err := prepareAirlock(environ, testRelease("AIRLOCK_ME", nil, nil), "grant-one", &expires)
	if err != nil {
		t.Fatalf("prepareAirlock: %v", err)
	}

	dir := filepath.Join(runtimeDir, "agent-secrets", "airlock", "grant-one")
	if filepath.Dir(files.bundle) != dir || filepath.Dir(files.wgetrc) != dir {
		t.Fatalf("the files are at %q and %q, want both under %q", files.bundle, files.wgetrc, dir)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for _, path := range []string{files.bundle, files.wgetrc} {
		if filepath.Dir(path) == cwd {
			t.Fatalf("%q is in the working directory, where nothing of a grant may be written", path)
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat the grant's directory: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("the grant's directory is mode %o, want 700", info.Mode().Perm())
	}
	for _, path := range []string{files.bundle, files.wgetrc, filepath.Join(dir, "expires")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s is mode %o, want 600", filepath.Base(path), info.Mode().Perm())
		}
	}

	bundle, err := os.ReadFile(files.bundle)
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	if !strings.Contains(string(bundle), rootPEM) {
		t.Errorf("the bundle does not hold the trust the command came with:\n%s", bundle)
	}
	if !strings.Contains(string(bundle), testAirlockCertificate) {
		t.Errorf("the bundle does not hold the proxy's certificate:\n%s", bundle)
	}
	wgetrc, err := os.ReadFile(files.wgetrc)
	if err != nil {
		t.Fatalf("read the wgetrc: %v", err)
	}
	if want := "tries = 3\nca_certificate = " + files.bundle + "\n"; string(wgetrc) != want {
		t.Errorf("the wgetrc is %q, want %q", wgetrc, want)
	}
	stamp, err := os.ReadFile(filepath.Join(dir, "expires"))
	if err != nil {
		t.Fatalf("read the expires file: %v", err)
	}
	if string(stamp) != expires.Format(time.RFC3339) {
		t.Errorf("the expires file holds %q, want the grant's expiry in RFC 3339", stamp)
	}
}

// TestTrustBundleWithoutASystemOneIsTheProxyAlone: a machine with no trust file to name leaves
// the command the proxy's certificate and nothing else, which is what it had.
func TestTrustBundleWithoutASystemOneIsTheProxyAlone(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there.pem")
	got := string(trustBundle([]string{"SSL_CERT_FILE=" + missing}, testAirlockCertificate))
	if got != testAirlockCertificate && !strings.Contains(got, testAirlockCertificate) {
		t.Fatalf("the bundle is %q, want the proxy's certificate in it", got)
	}
	if strings.Contains(got, "not-there") {
		t.Fatalf("the bundle names a file that is not there: %q", got)
	}
}

// TestTrustBundleKeepsEveryBundleVariablesTrust: the bundle is the trust the command would have
// had, and a command that names a private authority in any bundle variable - not SSL_CERT_FILE
// alone - would have had that authority. Its calls to that authority's hosts go direct, since
// NO_PROXY sends every host no entry binds direct, so a bundle without it fails them. A variable
// naming a file that cannot be read names no trust the command would have had either, and the
// run goes on without it; a second variable naming a file already in the bundle adds nothing.
func TestTrustBundleKeepsEveryBundleVariablesTrust(t *testing.T) {
	const (
		systemRoots = "-----BEGIN CERTIFICATE-----\nMIIINHERITEDROOT\n-----END CERTIFICATE-----\n"
		nodeRoot    = "-----BEGIN CERTIFICATE-----\nMIIICORPORATEROOT\n-----END CERTIFICATE-----\n"
		pythonRoot  = "-----BEGIN CERTIFICATE-----\nMIIILABORATORYROOT\n-----END CERTIFICATE-----\n"
	)
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	roots, node, python := write("roots.pem", systemRoots), write("corp.pem", nodeRoot), write("lab.pem", pythonRoot)
	unreadable := filepath.Join(dir, "not-there.pem")
	runtimeDir := t.TempDir()
	environ := []string{
		"XDG_RUNTIME_DIR=" + runtimeDir,
		"SSL_CERT_FILE=" + roots,
		"NODE_EXTRA_CA_CERTS=" + node,
		"REQUESTS_CA_BUNDLE=" + python,
		"AWS_CA_BUNDLE=" + node, // the same file twice: its certificate goes in once
		"CURL_CA_BUNDLE=" + unreadable,
	}
	expires := time.Now().Add(time.Hour)
	files, err := prepareAirlock(environ, testRelease("AIRLOCK_ME", nil, nil), "grant-one", &expires)
	if err != nil {
		t.Fatalf("prepareAirlock with a bundle variable naming a file that cannot be read: %v", err)
	}
	data, err := os.ReadFile(files.bundle)
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	bundle := string(data)
	t.Logf("the bundle is:\n%s", bundle)
	for what, want := range map[string]string{
		"the trust SSL_CERT_FILE named":       systemRoots,
		"the trust NODE_EXTRA_CA_CERTS named": nodeRoot,
		"the trust REQUESTS_CA_BUNDLE named":  pythonRoot,
		"the proxy's own certificate":         testAirlockCertificate,
	} {
		if !strings.Contains(bundle, want) {
			t.Errorf("the bundle does not hold %s:\n%s", what, bundle)
		}
	}
	if n := strings.Count(bundle, nodeRoot); n != 1 {
		t.Errorf("the certificate two variables name is in the bundle %d times, want once:\n%s", n, bundle)
	}
	if strings.Contains(bundle, "not-there") {
		t.Errorf("the bundle names a file that cannot be read: %s", bundle)
	}
	if !strings.HasSuffix(bundle, testAirlockCertificate) {
		t.Errorf("the bundle does not end in the proxy's certificate, which goes in after the trust the command came with:\n%s", bundle)
	}
}

// TestPrepareAirlockTidiesWhatEarlierRunsLeft: nothing of this CLI runs once the command starts,
// so each run tidies first. A grant that has expired goes, one whose run never finished writing
// (no expires file) goes, the directory a run that died left part-built goes once it is older
// than any build, one that lasts until its session ends stays, the directory another run is
// building now stays, and this run's own directory is there when it returns.
func TestPrepareAirlockTidiesWhatEarlierRunsLeft(t *testing.T) {
	runtimeDir := t.TempDir()
	root := filepath.Join(runtimeDir, "agent-secrets", "airlock")
	leave := func(name, expires string, writeExpires bool) string {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("make %s: %v", dir, err)
		}
		if writeExpires {
			if err := os.WriteFile(filepath.Join(dir, "expires"), []byte(expires), 0o600); err != nil {
				t.Fatalf("write %s/expires: %v", dir, err)
			}
		}
		return dir
	}
	expired := leave("grant-expired", time.Now().Add(-time.Minute).Format(time.RFC3339), true)
	unfinished := leave("grant-unfinished", "", false)
	unreadable := leave("grant-unreadable", "not a time at all", true)
	live := leave("grant-live", time.Now().Add(time.Hour).Format(time.RFC3339), true)
	session := leave("grant-session", "", true)
	stale := leave(airlockBuildPrefix+"of-a-run-that-died", "", false)
	if err := os.Chtimes(stale, time.Now().Add(-2*airlockBuildLifetime), time.Now().Add(-2*airlockBuildLifetime)); err != nil {
		t.Fatalf("age %s: %v", stale, err)
	}
	building := leave(airlockBuildPrefix+"of-another-run-right-now", "", false)

	expires := time.Now().Add(time.Hour)
	files, err := prepareAirlock([]string{"XDG_RUNTIME_DIR=" + runtimeDir}, testRelease("AIRLOCK_ME", nil, nil), "grant-one", &expires)
	if err != nil {
		t.Fatalf("prepareAirlock: %v", err)
	}
	for _, dir := range []string{expired, unfinished, unreadable, stale} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s is still there, want it tidied away", filepath.Base(dir))
		}
	}
	for _, dir := range []string{live, session, building, filepath.Dir(files.bundle)} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s is gone, want it kept: %v", filepath.Base(dir), err)
		}
	}
}

// TestPrepareAirlockIsNeverSeenHalfWrittenByAnotherRunsTidy drives the interleaving two runs
// that share a runtime directory really have - parallel tool calls of one agent do - with one
// run preparing while another run's tidy sweeps the same root. A directory under its final name
// must hold every file this run's command will read, the expires file among them: a tidy that
// finds no expires file reads the directory as one a run left unfinished and removes it, which
// takes away the other run's bundle while it is being written.
func TestPrepareAirlockIsNeverSeenHalfWrittenByAnotherRunsTidy(t *testing.T) {
	runtimeDir := t.TempDir()
	root := filepath.Join(runtimeDir, "agent-secrets", "airlock")
	environ := []string{"XDG_RUNTIME_DIR=" + runtimeDir}
	expires := time.Now().Add(time.Hour)

	// The other run: its tidy keeps its own grant alone, and after each sweep it reads what is
	// left under a final name. Each complaint is sent once; the preparations go on either way,
	// since a run that fails outright is the same bug seen from the other side.
	complaints := make(chan string, 1)
	stop, swept := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(swept)
		for {
			select {
			case <-stop:
				return
			default:
			}
			tidyAirlockDirs(root, "the-other-runs-grant", time.Now())
			entries, err := os.ReadDir(root)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
					continue // a directory a run is still building, which carries no final name
				}
				for _, name := range []string{"ca-bundle.pem", "wgetrc", "expires"} {
					if _, err := os.Stat(filepath.Join(root, entry.Name(), name)); err != nil {
						select {
						case complaints <- fmt.Sprintf("%s is under its final name without %s: %v", entry.Name(), name, err):
						default:
						}
					}
				}
			}
		}
	}()

	for i := range 150 {
		grant := fmt.Sprintf("grant-%03d", i)
		files, err := prepareAirlock(environ, testRelease("AIRLOCK_ME", nil, nil), grant, &expires)
		if err != nil {
			close(stop)
			<-swept
			t.Fatalf("%s: prepareAirlock while another run tidied the same root: %v", grant, err)
		}
		if filepath.Dir(files.bundle) != filepath.Join(root, grant) {
			t.Fatalf("%s: the bundle is at %q, want it under %q", grant, files.bundle, filepath.Join(root, grant))
		}
	}
	close(stop)
	<-swept
	select {
	case complaint := <-complaints:
		t.Fatalf("another run's tidy saw an unfinished directory under its final name: %s", complaint)
	default:
	}
}

// TestPrepareAirlockTwiceForOneGrantKeepsTheDirectoryThatIsThere: two runs can be given the same
// grant, and the second finds the first's finished directory under the name its own would take.
// It answers that directory rather than writing over files the first run's command may be
// reading at that moment, and the directory it built for itself is gone.
func TestPrepareAirlockTwiceForOneGrantKeepsTheDirectoryThatIsThere(t *testing.T) {
	runtimeDir := t.TempDir()
	environ := []string{"XDG_RUNTIME_DIR=" + runtimeDir}
	expires := time.Now().Add(time.Hour)
	first, err := prepareAirlock(environ, testRelease("AIRLOCK_ME", nil, nil), "grant-one", &expires)
	if err != nil {
		t.Fatalf("the first run: %v", err)
	}
	second, err := prepareAirlock(environ, testRelease("AIRLOCK_ME", nil, nil), "grant-one", &expires)
	if err != nil {
		t.Fatalf("the second run for the same grant: %v", err)
	}
	if second != first {
		t.Fatalf("the second run answers %+v, want the first run's %+v", second, first)
	}
	for _, path := range []string{first.bundle, first.wgetrc, filepath.Join(filepath.Dir(first.bundle), "expires")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s is not there after the second run: %v", filepath.Base(path), err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(runtimeDir, "agent-secrets", "airlock"))
	if err != nil {
		t.Fatalf("read the airlock root: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "grant-one" {
		names := make([]string, len(entries))
		for i, entry := range entries {
			names[i] = entry.Name()
		}
		t.Fatalf("the airlock root holds %v, want the one grant directory: the second run's own is left behind", names)
	}
}

// TestPrepareAirlockFallsBackToTheTemporaryDirectory: a session with no XDG_RUNTIME_DIR writes
// under TMPDIR, and never into the working directory.
func TestPrepareAirlockFallsBackToTheTemporaryDirectory(t *testing.T) {
	tmp := t.TempDir()
	files, err := prepareAirlock([]string{"TMPDIR=" + tmp}, testRelease("AIRLOCK_ME", nil, nil), "grant-one", nil)
	if err != nil {
		t.Fatalf("prepareAirlock: %v", err)
	}
	want := filepath.Join(tmp, "agent-secrets", "airlock", "grant-one")
	if filepath.Dir(files.bundle) != want {
		t.Fatalf("the bundle is at %q, want it under %q", files.bundle, want)
	}
	stamp, err := os.ReadFile(filepath.Join(want, "expires"))
	if err != nil {
		t.Fatalf("read the expires file: %v", err)
	}
	if string(stamp) != "" {
		t.Errorf("the expires file holds %q, want the empty string for a grant that lasts until its session ends", stamp)
	}
}

// TestTheSharedTemporaryRootNamesThisUser: a session with neither XDG_RUNTIME_DIR nor TMPDIR in
// the environment it hands this CLI falls back to the system temporary directory, which every
// other user of the machine writes in too, and what goes under the root is the trust bundle the
// command verifies each of its direct calls against. The uid in the first element is what keeps
// a plain "agent-secrets" another user made from being the directory this run writes in. A base
// the session named itself is the session's own, so it carries no uid.
//
// The row names the system temporary directory by setting this process's own TMPDIR, which is
// where os.TempDir reads it, rather than calling os.TempDir itself: a test file of this module
// that calls it is one internal/testtmp's placement check refuses, since that is how a test
// comes to place a directory wherever it runs.
func TestTheSharedTemporaryRootNamesThisUser(t *testing.T) {
	joined := func(environ []string) string {
		base, elements := airlockRootParts(environ)
		return filepath.Join(base, elements[0], elements[1])
	}
	system := t.TempDir()
	t.Setenv("TMPDIR", system)
	root := joined(nil)
	want := filepath.Join(system, fmt.Sprintf("agent-secrets-%d", os.Getuid()), "airlock")
	if root != want {
		t.Errorf("the root of a session with neither variable set is %q, want %q", root, want)
	}
	named := joined([]string{"XDG_RUNTIME_DIR=/run/user/1000"})
	if want := filepath.Join("/run/user/1000", "agent-secrets", "airlock"); named != want {
		t.Errorf("the root under the session's own runtime directory is %q, want %q, which names no uid", named, want)
	}
}

// TestPrepareAirlockRefusesARootOthersCanWriteIn: a mode that lets a group or the world in is how
// another user comes to hold a root without the directory ever changing hands, and what they
// would hold is the trust bundle every direct call of the command is verified against. The run is
// refused with the path, and nothing is written under it.
func TestPrepareAirlockRefusesARootOthersCanWriteIn(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "agent-secrets", "airlock")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("make the root: %v", err)
	}
	// Chmod rather than a mode asked of MkdirAll, which takes the umask off what it is given.
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatalf("open the root to everybody: %v", err)
	}
	_, err := prepareAirlock([]string{"XDG_RUNTIME_DIR=" + base}, testRelease("AIRLOCK_ME", nil, nil), "grant-one", nil)
	if err == nil {
		t.Fatalf("prepareAirlock wrote a grant's files under a root everybody can write in")
	}
	if !strings.Contains(err.Error(), root) {
		t.Errorf("the refusal is %q, want it to name %q", err, root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read the root: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the refused run left %d entries under the root; want none", len(entries))
	}
}

// TestPrepareAirlockRefusesARootThatIsASymbolicLink: a link is a root whose real directory
// whoever made the link chose, so the files would be written and read somewhere this run never
// named. It is refused with the path, and the directory the link points at is left empty.
func TestPrepareAirlockRefusesARootThatIsASymbolicLink(t *testing.T) {
	base := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "agent-secrets"), 0o700); err != nil {
		t.Fatalf("make the root's parent: %v", err)
	}
	root := filepath.Join(base, "agent-secrets", "airlock")
	if err := os.Symlink(elsewhere, root); err != nil {
		t.Fatalf("link the root somewhere else: %v", err)
	}
	_, err := prepareAirlock([]string{"XDG_RUNTIME_DIR=" + base}, testRelease("AIRLOCK_ME", nil, nil), "grant-one", nil)
	if err == nil {
		t.Fatalf("prepareAirlock wrote a grant's files through a root that is a symbolic link")
	}
	if !strings.Contains(err.Error(), root) {
		t.Errorf("the refusal is %q, want it to name %q", err, root)
	}
	entries, err := os.ReadDir(elsewhere)
	if err != nil {
		t.Fatalf("read where the link points: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the refused run left %d entries where the link points; want none", len(entries))
	}
}

// TestPrepareAirlockRefusesAParentOthersCanWriteIn: the root is two directories this CLI makes
// — "agent-secrets" under a base the session named, "agent-secrets-<uid>" under the shared
// temporary one, and "airlock" under either — and the check reaches both. A parent another user
// can write in is one they can rename the checked directory out of between the check and the
// write, and put their own wgetrc and trust bundle where the command will read them, so the run
// is refused with that parent's path and nothing is written under it.
//
// A parent another user owns is the same hole and cannot be made here, since this test runs as
// one unprivileged user; the mode is the shape of it this pod can stand up.
func TestPrepareAirlockRefusesAParentOthersCanWriteIn(t *testing.T) {
	for _, row := range []struct {
		name   string
		shared bool
	}{
		{name: "under the session's own runtime directory"},
		{name: "under the shared temporary directory", shared: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			base := t.TempDir()
			var environ []string
			parent := filepath.Join(base, "agent-secrets")
			if row.shared {
				// The system temporary directory is this process's own TMPDIR, which is
				// where os.TempDir reads it; the CLI is handed an environment naming
				// neither variable, which is the session that falls back to it.
				t.Setenv("TMPDIR", base)
				parent = filepath.Join(base, fmt.Sprintf("agent-secrets-%d", os.Getuid()))
			} else {
				environ = []string{"XDG_RUNTIME_DIR=" + base}
			}
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatalf("make the root's parent: %v", err)
			}
			// Chmod rather than a mode asked of Mkdir, which takes the umask off what it is
			// given.
			if err := os.Chmod(parent, 0o777); err != nil {
				t.Fatalf("open the root's parent to everybody: %v", err)
			}
			_, err := prepareAirlock(environ, testRelease("AIRLOCK_ME", nil, nil), "grant-one", nil)
			if err == nil {
				t.Fatalf("prepareAirlock wrote a grant's files under a parent everybody can write in")
			}
			if !strings.Contains(err.Error(), parent) {
				t.Errorf("the refusal is %q, want it to name %q", err, parent)
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatalf("read the root's parent: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("the refused run left %d entries under the parent; want none", len(entries))
			}
		})
	}
}

// TestPrepareAirlockRefusesAParentThatIsASymbolicLink: a link in the parent's place sends the
// root, and everything written under it, wherever whoever made the link pointed it. Making the
// root with one Mkdir per element rather than following the link is what sees it, so the run is
// refused with the parent's path and the directory the link points at is left empty.
func TestPrepareAirlockRefusesAParentThatIsASymbolicLink(t *testing.T) {
	base := t.TempDir()
	elsewhere := t.TempDir()
	parent := filepath.Join(base, "agent-secrets")
	if err := os.Symlink(elsewhere, parent); err != nil {
		t.Fatalf("link the root's parent somewhere else: %v", err)
	}
	_, err := prepareAirlock([]string{"XDG_RUNTIME_DIR=" + base}, testRelease("AIRLOCK_ME", nil, nil), "grant-one", nil)
	if err == nil {
		t.Fatalf("prepareAirlock wrote a grant's files through a parent that is a symbolic link")
	}
	if !strings.Contains(err.Error(), parent) {
		t.Errorf("the refusal is %q, want it to name %q", err, parent)
	}
	entries, err := os.ReadDir(elsewhere)
	if err != nil {
		t.Fatalf("read where the link points: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the refused run left %d entries where the link points; want none", len(entries))
	}
}

// TestExecFormRunsACommandForANameBehindTheAirlock is the whole form end to end against the
// package's fake broker: a grant whose answer carries an airlock name and no value for it is not
// the "released no value" refusal, the command runs, its source line is the one it has always
// been, and its environment holds the placeholder and the proxy.
func TestExecFormRunsACommandForANameBehindTheAirlock(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	runtimeDir := t.TempDir()
	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir,
		[]string{
			"XDG_RUNTIME_DIR=" + runtimeDir, "AGENT_SECRETS_HELPER_SOCK=",
			// Both spellings, since the machine running this test may hold either and the
			// lower-case one wins: what is under test is the list the child gets.
			"NO_PROXY=api.example.com,intranet.example.org",
			"no_proxy=api.example.com,intranet.example.org",
		},
		"AIRLOCK_ME", "--", "env")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if strings.Contains(stderr, "released no value") {
		t.Fatalf("the airlock name was taken for an unreleased one: stderr=%q", stderr)
	}
	first, _, _ := strings.Cut(stderr, "\n")
	if want := "agent-secrets: AIRLOCK_ME from " + fakeSourceText(broker); first != want {
		t.Fatalf("the first stderr line is %q, want %q", first, want)
	}

	env := make(map[string]string)
	for _, line := range strings.Split(stdout, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	proxy := "http://agent-secrets:" + testAirlockPlaceholder + "@airlock.broker.example:3128"
	for key, want := range map[string]string{
		"AIRLOCK_ME":         testAirlockPlaceholder,
		"HTTPS_PROXY":        proxy,
		"https_proxy":        proxy,
		"NODE_USE_ENV_PROXY": "1",
	} {
		if env[key] != want {
			t.Errorf("the command's %s is %q, want %q", key, env[key], want)
		}
	}
	// The bound host is off the inherited list and the entry that names nothing bound is on it.
	if strings.Contains(env["NO_PROXY"], "api.example.com") || !strings.Contains(env["NO_PROXY"], "intranet.example.org") {
		t.Errorf("the command's NO_PROXY is %q, want the bound host gone and the other inherited entry kept", env["NO_PROXY"])
	}
	bundle := filepath.Join(runtimeDir, "agent-secrets", "airlock", "grant-airlock", "ca-bundle.pem")
	for _, key := range airlockBundleVars {
		if env[key] != bundle {
			t.Errorf("the command's %s is %q, want %q", key, env[key], bundle)
		}
	}
	if want := filepath.Join(runtimeDir, "agent-secrets", "airlock", "grant-airlock", "wgetrc"); env["WGETRC"] != want {
		t.Errorf("the command's WGETRC is %q, want %q", env["WGETRC"], want)
	}
	if env["AGENT_SECRETS_URL"] != broker.URL {
		t.Errorf("the command's AGENT_SECRETS_URL is %q, want the session's broker %q", env["AGENT_SECRETS_URL"], broker.URL)
	}
}
