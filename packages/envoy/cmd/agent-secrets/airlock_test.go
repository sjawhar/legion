// packages/envoy/cmd/agent-secrets/airlock_test.go
//
// What a command gets when the broker holds its secret back: the placeholder, the proxy, the
// NO_PROXY that keeps the bound hosts on it, the trust bundle and the wgetrc beside it, and the
// tidying each run does first. Nothing here holds a credential — a placeholder stands for one,
// and the certificate is a public one — so these tests print what they compare.
package main

import (
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

// TestPrepareAirlockTidiesWhatEarlierRunsLeft: nothing of this CLI runs once the command starts,
// so each run tidies first. A grant that has expired goes, one whose run never finished writing
// (no expires file) goes, one that lasts until its session ends stays, and this run's own
// directory is there when it returns.
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

	expires := time.Now().Add(time.Hour)
	files, err := prepareAirlock([]string{"XDG_RUNTIME_DIR=" + runtimeDir}, testRelease("AIRLOCK_ME", nil, nil), "grant-one", &expires)
	if err != nil {
		t.Fatalf("prepareAirlock: %v", err)
	}
	for _, dir := range []string{expired, unfinished, unreadable} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s is still there, want it tidied away", filepath.Base(dir))
		}
	}
	for _, dir := range []string{live, session, filepath.Dir(files.bundle)} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s is gone, want it kept: %v", filepath.Base(dir), err)
		}
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
