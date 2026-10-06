package workspace

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pacedWriter wraps an http.ResponseWriter so a response body can be driven at a controlled,
// sustained rate instead of however large a chunk the handler underneath happens to write at
// once: it sends one byte at a time, flushing each, sleeping step between every byte.
type pacedWriter struct {
	http.ResponseWriter
	step time.Duration
}

func (w *pacedWriter) Write(p []byte) (int, error) {
	flusher, _ := w.ResponseWriter.(http.Flusher)
	for i, b := range p {
		if _, err := w.ResponseWriter.Write([]byte{b}); err != nil {
			return i, err
		}
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(w.step)
	}
	return len(p), nil
}

// newPacedGitServer serves a tiny bare repository over HTTPS through the real git http-backend,
// pacing the git-upload-pack response one byte at a time, step apart: enough above
// FetchLowSpeedLimit to prove the fetch's clone survives a connection that keeps trickling past
// FetchLowSpeedTime, the way GitHub's own keepalive does while it prepares a large packfile.
// Curl's low-speed timer is per HTTP request, not per clone, so only this one request — the one a
// real slow-starting packfile actually stalls on — is paced; the info/refs advertisement that
// precedes it runs at full speed. The response for this tiny, single-empty-commit repository is
// a few hundred bytes (measured: ~413), so step is sized generously (see the caller) to keep the
// whole paced response well past FetchLowSpeedTime without an unpredictable cutoff.
func newPacedGitServer(t *testing.T, step time.Duration) *httptest.Server {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	projects := t.TempDir()
	remote := filepath.Join(projects, "widgets.git")
	seed := t.TempDir()
	for _, argv := range [][]string{
		{"init", "--quiet", "--bare", "--initial-branch=main", remote},
		{"init", "--quiet", "--initial-branch=main", seed},
		{"-C", seed, "-c", "user.name=Legion test", "-c", "user.email=legion-test@example.invalid", "commit", "--quiet", "--allow-empty", "-m", "seed"},
		{"-C", seed, "push", "--quiet", remote, "HEAD:main"},
	} {
		command := exec.Command(git, argv...)
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", argv, err, output)
		}
	}

	backend := &cgi.Handler{
		Path: git, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + projects, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"},
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
			w = &pacedWriter{ResponseWriter: w, step: step}
		}
		backend.ServeHTTP(w, r)
	}))
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

// trustServerCA makes the fetch's own clone (which reads no config file, GIT_CONFIG_GLOBAL and
// GIT_CONFIG_NOSYSTEM both pinned) trust server's self-signed certificate: GIT_SSL_CAINFO is an
// environment variable git's http transport reads directly, independent of any config file.
func trustServerCA(t *testing.T, server *httptest.Server) {
	t.Helper()
	caFile := filepath.Join(t.TempDir(), "server-ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", caFile)
}

// stallRunner is newLocalRunner's runner, but for the fetch's clone retargeted at remote instead
// of a local bare path: remote is reached over real HTTPS, so GIT_HTTP_LOW_SPEED_LIMIT/TIME (the
// stall detector fetchLowSpeedEnvironment sets) actually governs the transfer, which a file-path
// remote's transport never would.
type stallRunner struct {
	remote string
	run    Runner
}

func (r *stallRunner) Timeout() time.Duration { return r.run.Timeout() }

func (r *stallRunner) Run(ctx context.Context, command Command) (Result, error) {
	if commandWith(command.Argv, "git", "clone", "--bare", "--quiet", "https://github.com/acme/widgets") {
		rewritten := append([]string(nil), command.Argv...)
		rewritten[4] = r.remote
		command.Argv = rewritten
	}
	return r.run.Run(ctx, command)
}

// The reviewer measured a real large repository (chromium/chromium) spending 114-122 s between
// its packfile header and its first pack byte, with GitHub sending nothing but a keepalive every
// few seconds meanwhile: a 100 KiB/s floor aborted that clone at 63 s, before any pack byte
// arrived, so the floor is now 1 B/s (FetchLowSpeedLimit). This proves a connection that keeps
// trickling — here, one byte every 400 ms, 2.5 B/s, for the whole (small) response — still
// completes: the floor does not mistake a slow-starting but live transfer for a stall. 400 ms is
// calibrated empirically: at this response's size (measured ~413 bytes through git-upload-pack),
// a 100 KiB/s floor over a 60 s window took about 121 s to actually abort, roughly twice the
// window itself, so the whole paced response (about 165 s at 400 ms/byte) comfortably outlasts
// where the old floor would have given up, while the 1 B/s floor survives it.
func TestFetchSurvivesAConnectionThatTricklesAboveTheFloor(t *testing.T) {
	run := newLocalRunner(t)
	server := newPacedGitServer(t, 400*time.Millisecond)
	trustServerCA(t, server)
	req := fetchRequest(t)

	if _, err := Fetch(context.Background(), &stallRunner{remote: server.URL + "/widgets.git", run: run}, req); err != nil {
		t.Fatalf("Fetch: %v, want it to survive a trickle above the floor", err)
	}
}

// silentServer never writes a single byte of its response body: curl's own low-speed timer sees
// 0 bytes/second from the moment the request is sent, which is below any positive floor
// immediately, so it aborts once FetchLowSpeedTime has passed with nothing received — the same
// "curl 28 Operation too slow" the reviewer's own reproduction hit.
func silentServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(FetchLowSpeedTime + 5*time.Second)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestFetchAbortsAConnectionThatGoesSilent(t *testing.T) {
	run := newLocalRunner(t)
	server := silentServer(t)
	trustServerCA(t, server)
	req := fetchRequest(t)

	_, err := Fetch(context.Background(), &stallRunner{remote: server.URL + "/widgets.git", run: run}, req)
	if err == nil {
		t.Fatal("Fetch: no error, want the stall detector to abort a connection that never sends a byte")
	}
}
