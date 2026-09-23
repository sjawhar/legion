package shim_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/shim"
)

// --connect names the daemon's worker stream in one of its two address families. Anything else
// is refused naming the flag and the value, before the shim dials anything.
func TestParseAddressAcceptsTheListenersTwoFamiliesOnly(t *testing.T) {
	for _, tc := range []struct {
		value, network, address string
	}{
		{"unix:///run/legion/worker-stream.sock", "unix", "/run/legion/worker-stream.sock"},
		{"tcp://legion-daemon.legion.svc:13371", "tcp", "legion-daemon.legion.svc:13371"},
		{"tcp://127.0.0.1:1/", "tcp", "127.0.0.1:1"},
		{"tcp://[::1]:65535", "tcp", "[::1]:65535"},
	} {
		network, address, err := shim.ParseAddress(tc.value)
		if err != nil || network != tc.network || address != tc.address {
			t.Errorf("ParseAddress(%q) = %q, %q, %v; want %q, %q", tc.value, network, address, err, tc.network, tc.address)
		}
	}
	for _, value := range []string{
		"",
		"/run/legion/worker-stream.sock",
		"unix://run/legion/worker-stream.sock", // a host, not an absolute path
		"unix:/run/legion/worker-stream.sock",
		"unix:///",
		"unix:///run/legion/",
		"unix:///run/s.sock?x=1",
		"tcp://127.0.0.1",
		"tcp://127.0.0.1:",
		"tcp://127.0.0.1:0",
		"tcp://127.0.0.1:65536",
		"tcp://:13371",
		"tcp://127.0.0.1:13371/path",
		"tcp://user@127.0.0.1:13371",
		"tcp://127.0.0.1:13371#frag",
		"http://127.0.0.1:13371",
	} {
		_, _, err := shim.ParseAddress(value)
		if err == nil {
			t.Errorf("ParseAddress(%q) accepted a malformed address", value)
			continue
		}
		if !strings.Contains(err.Error(), "--connect") || !strings.Contains(err.Error(), value) {
			t.Errorf("ParseAddress(%q) = %v; the refusal must name --connect and the value", value, err)
		}
	}
}

func TestReadBootTokenRefusesAnUnreadableOrBlankFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "token")
	if err := os.WriteFile(good, []byte("  the-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if token, err := shim.ReadBootToken(good); err != nil || token != "the-token" {
		t.Fatalf("ReadBootToken = %q, %v; want the trimmed token", token, err)
	}

	blank := filepath.Join(dir, "blank")
	if err := os.WriteFile(blank, []byte(" \n\t"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")
	for path, want := range map[string]string{blank: "is blank", missing: "is unreadable"} {
		_, err := shim.ReadBootToken(path)
		if err == nil || !strings.Contains(err.Error(), "--boot-token-file "+path+" "+want) {
			t.Errorf("ReadBootToken(%s) = %v; want a refusal naming the flag, the path, and %q", path, err, want)
		}
	}
}

// --provider-env-dir exactly as shipped (worker-shim.ts:578-620): every regular file becomes
// NAME=trimmed contents for the child, a NAME whose NAME_FILE pointer the shim's environment
// already carries is left to its file's reader, and a NAME that is already a variable of the
// shim's own environment — an empty value included — is refused naming the key and its file.
func TestReadProviderEnvExportsFilesSkipsFilePointersAndRefusesShadows(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("ANTHROPIC_API_KEY", "sk-provider\n")
	write("DISPATCH_TOKEN", "read-through-its-file")
	// A Kubernetes Secret mount: KEY -> ..data/KEY, and ..data -> a timestamped directory.
	data := filepath.Join(dir, "..2026_09_22")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "OPENAI_API_KEY"), []byte(" sk-mounted "), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..2026_09_22", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..data", "OPENAI_API_KEY"), filepath.Join(dir, "OPENAI_API_KEY")); err != nil {
		t.Fatal(err)
	}

	shimEnv := map[string]string{"DISPATCH_TOKEN_FILE": "/var/run/secrets/DISPATCH_TOKEN", "PATH": "/usr/bin"}
	lookup := func(name string) (string, bool) { v, ok := shimEnv[name]; return v, ok }
	env, err := shim.ReadProviderEnv(dir, lookup)
	if err != nil {
		t.Fatalf("ReadProviderEnv: %v", err)
	}
	if want := []string{"ANTHROPIC_API_KEY=sk-provider", "OPENAI_API_KEY=sk-mounted"}; !reflect.DeepEqual(env, want) {
		t.Fatalf("ReadProviderEnv = %q, want %q", env, want)
	}

	shimEnv["ANTHROPIC_API_KEY"] = ""
	_, err = shim.ReadProviderEnv(dir, lookup)
	if err == nil || !strings.Contains(err.Error(), "--provider-env-dir key ANTHROPIC_API_KEY ("+filepath.Join(dir, "ANTHROPIC_API_KEY")+")") {
		t.Fatalf("ReadProviderEnv = %v; want a refusal naming the key and its file", err)
	}
	if strings.Contains(err.Error(), "sk-provider") {
		t.Fatalf("the refusal leaks the secret: %v", err)
	}

	missing := filepath.Join(dir, "nowhere")
	if _, err := shim.ReadProviderEnv(missing, lookup); err == nil ||
		!strings.Contains(err.Error(), "--provider-env-dir "+missing+" is unreadable") {
		t.Fatalf("ReadProviderEnv(missing) = %v; want a refusal naming the flag and the directory", err)
	}
}
