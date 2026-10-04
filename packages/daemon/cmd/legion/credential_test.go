package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCredentialAcceptsGitGetAndWritesTheDaemonCredentialProtocol(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/legion/v1/git-credential" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "username=x-access-token\npassword=scoped-token")
	}))
	defer daemon.Close()
	grantFile := filepath.Join(t.TempDir(), "grant")
	if err := os.WriteFile(grantFile, []byte(" one-command-grant\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGION_DAEMON_URL", daemon.URL)
	t.Setenv("LEGION_GRANT_FILE", grantFile)

	previous := os.Stdin
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, "protocol=https\nhost=github.com\n\n"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdin = input
	t.Cleanup(func() {
		os.Stdin = previous
		_ = input.Close()
	})

	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "credential", "get"}, &out, &errb); code != 0 {
		t.Fatalf("credential = %d: %s", code, errb.String())
	}
	if got, want := out.String(), "username=x-access-token\npassword=scoped-token\n"; got != want {
		t.Fatalf("credential output = %q, want %q", got, want)
	}
}
