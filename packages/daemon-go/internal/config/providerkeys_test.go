package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubSecrets is a `secrets` stand-in: it answers `get <NAME> --no-request` with a status and
// `get <NAME> --value` with a value for the names below, and records how it was run — its
// arguments, its stdin, and the two secretsd variables the daemon treats differently.
const stubSecrets = `#!/bin/sh
echo "$*" >> "$STUB_DIR/calls"
readlink "/proc/$$/fd/0" > "$STUB_DIR/stdin"
if [ -n "${SECRETSD_SESSION_TOKEN_FILE+set}" ]; then echo set; else echo unset; fi > "$STUB_DIR/session"
echo "${SECRETSD_SOCK-}" > "$STUB_DIR/sock"
case "$2:$3" in
  AGENT_KEY:--no-request) echo '{"key":"AGENT_KEY","tier":"agent"}' ;;
  AGENT_KEY:--value) printf 'agent-value\n' ;;
  HUMAN_KEY:--no-request) echo '{"key":"HUMAN_KEY","tier":"human"}' ;;
  HUMAN_KEY:--value) printf 'human-value\n' ;;
  MISSING_KEY:*) echo "AGENT NOTICE: secret 'MISSING_KEY' not found" >&2; exit 3 ;;
  GARBLED_KEY:--no-request) echo 'not json' ;;
  OTHER_KEY:--no-request) echo '{"key":"SOMETHING_ELSE","tier":"agent"}' ;;
  ODD_TIER_KEY:--no-request) echo '{"key":"ODD_TIER_KEY","tier":"admin"}' ;;
  EMPTY_KEY:--no-request) echo '{"key":"EMPTY_KEY","tier":"agent"}' ;;
  EMPTY_KEY:--value) printf '\n' ;;
  FAILING_VALUE_KEY:--no-request) echo '{"key":"FAILING_VALUE_KEY","tier":"human"}' ;;
  FAILING_VALUE_KEY:--value) echo 'AGENT NOTICE: TIMEOUT' >&2; exit 4 ;;
  *) echo "stub: unexpected $*" >&2; exit 99 ;;
esac
`

// secretsStub installs the stub as `secrets` in a directory of its own and returns the daemon
// environment that finds it, carrying a session token file and a broker socket override.
func secretsStub(t *testing.T) (environ []string, stubDir string) {
	t.Helper()
	stubDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(stubDir, "secrets"), []byte(stubSecrets), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{
		"PATH=" + stubDir + ":/usr/bin:/bin",
		"STUB_DIR=" + stubDir,
		"HOME=" + t.TempDir(),
		"SECRETSD_SESSION_TOKEN_FILE=/run/user/1000/secretsd/session.token",
		"SECRETSD_SOCK=/run/user/1000/secretsd.sock",
	}, stubDir
}

func readStub(t *testing.T, stubDir, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(stubDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(body))
}

// Boot resolves each provider key the way the shipped daemon resolves an App key it holds in
// secretsd (config.ts:834-902): the tier first, never costing a tap; then the value, under the
// daemon's own environment less its session token, with the daemon's stdin — secretsd scopes a
// tokenless caller by its terminal. Each value lands in its own 0600 file in a 0700 directory,
// named for the variable OMP reads — not for the secretsd key, which a deployment names per
// environment. A human-tier key is announced first, and nothing logged carries a value.
func TestMaterializeProviderKeysWritesEachValueAsADaemonHeldFile(t *testing.T) {
	environ, stubDir := secretsStub(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	stdin, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() { os.Stdin = previous })
	logged := &strings.Builder{}
	log := slog.New(slog.NewTextHandler(logged, nil))

	dir, err := MaterializeProviderKeys([]ProviderKey{
		{Env: "AGENT_ENV", Secret: "AGENT_KEY"},
		{Env: "HUMAN_ENV", Secret: "HUMAN_KEY"},
	}, stateDir, environ, log)
	if err != nil {
		t.Fatalf("MaterializeProviderKeys: %v", err)
	}

	if want := filepath.Join(stateDir, "secrets", "provider-env"); dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	if info, err := os.Stat(dir); err != nil {
		t.Errorf("provider-env directory: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Errorf("provider-env directory mode %v, want 0700", info.Mode().Perm())
	}
	for name, want := range map[string]string{"AGENT_ENV": "agent-value", "HUMAN_ENV": "human-value"} {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		} else if info.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v, want 0600", path, info.Mode().Perm())
		}
		if got, _ := os.ReadFile(path); string(got) != want {
			t.Errorf("%s holds the wrong bytes", name)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("provider-env holds %d entries, want exactly the two variables (no temporary files, nothing named for a secretsd key)", len(entries))
	}

	if calls := readStub(t, stubDir, "calls"); calls != "get AGENT_KEY --no-request\nget AGENT_KEY --value\nget HUMAN_KEY --no-request\nget HUMAN_KEY --value" {
		t.Errorf("secrets was run as:\n%s", calls)
	}
	if got := readStub(t, stubDir, "session"); got != "unset" {
		t.Errorf("secrets ran with SECRETSD_SESSION_TOKEN_FILE %s; the daemon's request must not land on a session it was started from", got)
	}
	if got := readStub(t, stubDir, "sock"); got != "/run/user/1000/secretsd.sock" {
		t.Errorf("secrets ran with SECRETSD_SOCK %q, want the daemon's own", got)
	}
	if got := readStub(t, stubDir, "stdin"); got != stdin.Name() {
		t.Errorf("secrets ran with stdin %q, want the daemon's own %q", got, stdin.Name())
	}
	if !strings.Contains(logged.String(), "requesting HUMAN_KEY from secretsd (human tier; a YubiKey tap may be needed)") {
		t.Errorf("no human-tier announcement for HUMAN_KEY in:\n%s", logged)
	}
	if strings.Contains(logged.String(), "requesting AGENT_KEY") {
		t.Errorf("an agent-tier key was announced as needing a tap:\n%s", logged)
	}
	if strings.Contains(logged.String(), "-value") {
		t.Errorf("a value reached the log:\n%s", logged)
	}
}

// The directory holds exactly the configured keys: one dropped from the file is removed at the
// next boot, and no keys at all is no directory — a pane is never handed a key nobody configured.
func TestMaterializeProviderKeysKeepsExactlyTheConfiguredKeys(t *testing.T) {
	environ, _ := secretsStub(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	log := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))

	agent := ProviderKey{Env: "AGENT_ENV", Secret: "AGENT_KEY"}
	dir, err := MaterializeProviderKeys([]ProviderKey{agent, {Env: "HUMAN_ENV", Secret: "HUMAN_KEY"}}, stateDir, environ, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeProviderKeys([]ProviderKey{agent}, stateDir, environ, log); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "HUMAN_ENV")); !os.IsNotExist(err) {
		t.Errorf("a key dropped from provider_keys is still on disk (stat: %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "AGENT_ENV")); err != nil {
		t.Errorf("a configured key is gone: %v", err)
	}

	none, err := MaterializeProviderKeys(nil, stateDir, environ, log)
	if err != nil || none != "" {
		t.Fatalf("no provider keys = (%q, %v), want no directory", none, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the provider-env directory outlived its last key (stat: %v)", err)
	}
}

// Every way a key cannot be resolved is a refusal naming the entry — the variable and its secretsd
// key, never its value — and a boot that refuses writes nothing: the files are written only once
// every key has resolved.
func TestMaterializeProviderKeysRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		path bool
		want string
	}{
		{"the status fails", "MISSING_KEY", true, "provider_keys.ENV_MISSING_KEY: secrets get MISSING_KEY --no-request failed (exit 3): AGENT NOTICE: secret 'MISSING_KEY' not found"},
		{"the status is not a status", "GARBLED_KEY", true, `provider_keys.ENV_GARBLED_KEY: secrets get GARBLED_KEY --no-request printed an unparsable status (expected {"key","tier"})`},
		{"the status is another key's", "OTHER_KEY", true, `provider_keys.ENV_OTHER_KEY: secrets get OTHER_KEY --no-request printed an unparsable status (expected {"key","tier"})`},
		{"the tier is neither", "ODD_TIER_KEY", true, `provider_keys.ENV_ODD_TIER_KEY: secrets get ODD_TIER_KEY --no-request reported tier "admin", which is neither agent nor human`},
		{"the value is empty", "EMPTY_KEY", true, "provider_keys.ENV_EMPTY_KEY: secrets get EMPTY_KEY --value printed no value"},
		{"the value fails", "FAILING_VALUE_KEY", true, "provider_keys.ENV_FAILING_VALUE_KEY: secrets get FAILING_VALUE_KEY --value failed (exit 4): AGENT NOTICE: TIMEOUT"},
		{"no secrets command", "AGENT_KEY", false, "provider_keys.AGENT_ENV: the secrets command is not on PATH, so AGENT_KEY cannot be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			environ, _ := secretsStub(t)
			if !tc.path {
				environ[0] = "PATH=" + t.TempDir()
			}
			stateDir := filepath.Join(t.TempDir(), "state")
			log := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
			_, err := MaterializeProviderKeys([]ProviderKey{
				{Env: "AGENT_ENV", Secret: "AGENT_KEY"},
				{Env: "ENV_" + tc.key, Secret: tc.key},
			}, stateDir, environ, log)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "-value\n") || strings.Contains(err.Error(), "agent-value") {
				t.Errorf("the refusal carries a value: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(stateDir, "secrets", "provider-env", "AGENT_ENV")); !os.IsNotExist(statErr) {
				t.Errorf("a refused boot still wrote AGENT_ENV (stat: %v)", statErr)
			}
		})
	}
}
