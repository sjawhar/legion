// packages/envoy/cmd/dev-airlock-stands/main_test.go
//
// The command as a check script uses it: the real compiled binary, started, read for its JSON,
// called at one stand-in, read back at that stand-in's recording route, and stopped with the
// signal the script sends it. What each stand-in answers is the stand-in package's own rows;
// what this row is about is that a script can find the stands, trust them and read them back.
package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/airlock/airlocktest"
)

// buildStands builds the command into a directory of the test's own and returns its path.
func buildStands(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "dev-airlock-stands")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOTOOLCHAIN=go1.27.3")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/dev-airlock-stands: %v\n%s", err, out)
	}
	return binary
}

// TestTheStandsServeTheirRecordsAndStopOnASignal is the whole of what a check script does with
// this command.
func TestTheStandsServeTheirRecordsAndStopOnASignal(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(buildStands(t), "--dir", dir, "--host", "s=s.airlock.test")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the command: %v", err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}
	}()

	// The report, read as a script reads it: one JSON object, written before the command
	// serves anything else.
	var out report
	if err := json.NewDecoder(bufio.NewReader(stdout)).Decode(&out); err != nil {
		t.Fatalf("read the command's JSON: %v\n%s", err, stderr.String())
	}
	if out.Dir != dir || out.CACertFile != filepath.Join(dir, "ca.crt") || out.CAKeyFile != filepath.Join(dir, "ca.key") {
		t.Fatalf("the report names dir %q and the authority %q, %q; want them in the directory it was given", out.Dir, out.CACertFile, out.CAKeyFile)
	}
	if _, err := tls.LoadX509KeyPair(out.CACertFile, out.CAKeyFile); err != nil {
		t.Fatalf("the authority's files are no key pair: %v", err)
	}
	if out.AWSKeyID == "" || out.AWSSecretKey == "" {
		t.Error("the report names no AWS key pair; a script has nothing to sign its calls with")
	}
	for _, name := range stands {
		stand, served := out.Stands[name]
		if !served || stand.Address == "" || stand.URL == "" || stand.Records == "" {
			t.Fatalf("the report's %s stand is %+v; want an address, a URL and a recording route", name, stand)
		}
		if plainStands[name] {
			continue
		}
		if stand.CertFile != filepath.Join(dir, name+".crt") {
			t.Errorf("the %s stand's certificate is %q; want one written in the directory", name, stand.CertFile)
		}
	}

	// One call to S, verified against the authority's file alone and under the name --host
	// gave it, which is how a command behind the airlock reaches a stand-in.
	roots := x509.NewCertPool()
	pem, err := os.ReadFile(out.CACertFile)
	if err != nil {
		t.Fatal(err)
	}
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatalf("%s holds no PEM certificate", out.CACertFile)
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "s.airlock.test", MinVersion: tls.VersionTLS12},
	}}
	request, err := http.NewRequest(http.MethodGet, out.Stands[standS].URL+"/v1/things?page=2", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Check", "k4")
	answer, err := client.Do(request)
	if err != nil {
		t.Fatalf("call the S stand: %v", err)
	}
	answer.Body.Close()
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("the S stand answered %d; want 200", answer.StatusCode)
	}

	// Every stand's recording route answers, and S's holds the call just made.
	for _, name := range stands {
		records := recordsOf(t, out.Stands[name].Records)
		if name == standS {
			if len(records) != 1 {
				t.Fatalf("the S stand's records hold %d requests; want the one call", len(records))
			}
			got := records[0]
			if got.Method != http.MethodGet || got.Path != "/v1/things" || got.Query != "page=2" || got.Header.Get("X-Check") != "k4" {
				t.Errorf("the S stand recorded %+v; want the call as it was sent", got)
			}
			continue
		}
		if len(records) != 0 {
			t.Errorf("the %s stand's records hold %d requests; want none", name, len(records))
		}
	}

	// And the signal a script sends it: a clean stop.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal the command: %v", err)
	}
	stopped = true
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the command exited %v on SIGTERM; want a clean stop\n%s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("the command did not stop within 10s of SIGTERM\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "stands listening records="+out.RecordsURL) {
		t.Errorf("the command logged %q; want the line naming its recording URL", stderr.String())
	}
}

// recordsOf reads one stand's recording route.
func recordsOf(t *testing.T, url string) []airlocktest.Request {
	t.Helper()
	answer, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer answer.Body.Close()
	body, err := io.ReadAll(answer.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("GET %s answered %d %s; want 200", url, answer.StatusCode, body)
	}
	var records []airlocktest.Request
	if err := json.Unmarshal(body, &records); err != nil {
		t.Fatalf("%s answered no JSON array of records: %v\n%s", url, err, body)
	}
	return records
}
