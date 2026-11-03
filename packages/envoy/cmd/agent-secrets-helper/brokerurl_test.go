//go:build linux

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/brokerclient"
)

// The broker's address as an operator writes it, on the helper's side of the same one rule the
// clients keep. AGENT_SECRETS_URL is typed by hand into a unit file, a pod's environment or an
// install command, so a URL ending in more than one slash is a typo rather than a shape to
// refuse, and brokerclient.WithoutTrailingSlashes is the one reading of it this change gives both
// halves of the token client. The helper is the third reader, and it decides what a whole box
// can do: it holds the launcher credential every session on the machine enrols under.
//
// Driven on the dev stack at 29e9bce8, a helper whose AGENT_SECRETS_URL ends in two slashes
// trades its own credential (logged credential_id=a75da7aa) and is then refused on the first
// enrolment, which clears it: "the broker refused the launcher credential ...; cleared: no
// session can enroll until a human approves a new machine login", and the session is answered
// "ENROLL_FAILED: no machine credential; run: agent-secrets machine login". One typed slash
// turns a machine login that needs no person into one that does.

// TestTheHelperDropsEveryTrailingSlashOfTheBrokerURL: loadConfig reads AGENT_SECRETS_URL the way
// the clients do, so the proof the helper signs names the path the broker serves.
func TestTheHelperDropsEveryTrailingSlashOfTheBrokerURL(t *testing.T) {
	const want = "https://secrets.internal.example"
	for _, given := range []string{
		want,
		want + "/",
		want + "//",
		want + "///",
	} {
		cfg, err := loadConfig(env(map[string]string{
			"HOME":                    "/home/u",
			"XDG_RUNTIME_DIR":         "/run/user/7",
			brokerclient.BrokerURLVar: given,
		}))
		if err != nil {
			t.Fatalf("loadConfig with %s=%q: %v", brokerclient.BrokerURLVar, given, err)
		}
		if cfg.URL != want {
			t.Errorf("loadConfig with %s=%q gave URL %q, want %q: a proof signed for that path is refused LAUNCHER_INVALID and the helper clears the credential",
				brokerclient.BrokerURLVar, given, cfg.URL, want)
		}
	}
}

// TestInstallWritesTheBrokerURLWithNoTrailingSlash: the unit is where that typo persists, since
// every later start of the helper reads the line install wrote.
func TestInstallWritesTheBrokerURLWithNoTrailingSlash(t *testing.T) {
	for _, given := range []string{
		"https://secrets.example",
		"https://secrets.example/",
		"https://secrets.example//",
		"https://secrets.example///",
	} {
		r := newInstallRig(t)
		if err := r.run("--url", given); err != nil {
			t.Fatalf("install --url %q: %v", given, err)
		}
		data, err := os.ReadFile(r.unit)
		if err != nil {
			t.Fatal(err)
		}
		const want = "\nEnvironment=AGENT_SECRETS_URL=https://secrets.example\n"
		if !strings.Contains(string(data), want) {
			line := "<none>"
			for _, l := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(l, "Environment=AGENT_SECRETS_URL=") {
					line = l
				}
			}
			t.Errorf("install --url %q wrote %q, want %q: the unit is read at every start of the helper",
				given, line, strings.TrimSpace(want))
		}
	}
}
