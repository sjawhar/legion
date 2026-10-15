package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// environment is the environment Load reads holding exactly values: a key present is set, even
// when its value is empty, as os.LookupEnv answers.
func environment(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, set := values[key]
		return value, set
	}
}

// userFile is the user envoy.json under home when XDG_CONFIG_HOME is unset.
func userFile(home string) string {
	return filepath.Join(home, ".config", "legion", "envoy.json")
}

// checkoutFile is the checkout's envoy.json under cwd.
func checkoutFile(cwd string) string {
	return filepath.Join(cwd, ".envoy", "envoy.json")
}

func TestLoadMergesUserAndCheckout(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	mustWrite(t, userFile(home), `{
		"envoy": {"natsUrls": ["nats://user:4222"]},
		"dispatch": {"enabled": true, "serverUrl": "https://user.dispatch.example"}
	}`)
	mustWrite(t, checkoutFile(cwd), `{
		"dispatch": {"serverUrl": "https://checkout.dispatch.example"}
	}`)
	cfg, err := Load(environment(nil), LoadOptions{CWD: cwd, HomeDir: home})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Dispatch == nil {
		t.Fatalf("missing dispatch block")
	}
	if cfg.Dispatch.ServerURL != "https://checkout.dispatch.example" {
		t.Errorf("serverUrl: got %q", cfg.Dispatch.ServerURL)
	}
	if len(cfg.Envoy.NatsURLs) != 1 || cfg.Envoy.NatsURLs[0] != "nats://user:4222" {
		t.Errorf("envoy.natsUrls: %+v", cfg.Envoy.NatsURLs)
	}
}

func TestLoadReadsTheUserFileUnderXDGConfigHome(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	mustWrite(t, filepath.Join(configHome, "legion", "envoy.json"), `{"envoy": {"natsUrls": ["nats://xdg:4222"]}}`)
	mustWrite(t, userFile(home), `{"envoy": {"natsUrls": ["nats://home:4222"]}}`)

	for _, tc := range []struct {
		name     string
		values   map[string]string
		natsURLs string
	}{
		{name: "set", values: map[string]string{"XDG_CONFIG_HOME": configHome}, natsURLs: "nats://xdg:4222"},
		{name: "unset", values: nil, natsURLs: "nats://home:4222"},
		{name: "set but empty", values: map[string]string{"XDG_CONFIG_HOME": ""}, natsURLs: "nats://home:4222"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(environment(tc.values), LoadOptions{CWD: t.TempDir(), HomeDir: home})
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got := strings.Join(cfg.Envoy.NatsURLs, ","); got != tc.natsURLs {
				t.Errorf("envoy.natsUrls: got %q, want %q", got, tc.natsURLs)
			}
		})
	}
}

// A relative XDG_CONFIG_HOME names no directory the XDG spec lets anything use, so Load refuses it
// rather than read a user file found from wherever the process started, even when that file is
// there to read.
func TestLoadRefusesARelativeXDGConfigHome(t *testing.T) {
	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "relative", "legion", "envoy.json"), `{"envoy": {"natsUrls": ["nats://relative:4222"]}}`)
	t.Chdir(cwd)
	_, err := Load(environment(map[string]string{"XDG_CONFIG_HOME": "relative"}), LoadOptions{CWD: cwd, HomeDir: t.TempDir()})
	if err == nil {
		t.Fatal("expected a refusal of a relative XDG_CONFIG_HOME")
	}
	if want := `XDG_CONFIG_HOME is "relative", which is not an absolute path`; err.Error() != want {
		t.Errorf("error: got %q, want %q", err, want)
	}
}

func TestLoadEnvironmentOverridesMergedConfig(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	mustWrite(t, userFile(home), `{
		"envoy": {"natsUrls": ["nats://user:4222"]},
		"dispatch": {"enabled": true, "serverUrl": "https://user.dispatch.example"}
	}`)
	mustWrite(t, checkoutFile(cwd), `{
		"dispatch": {"serverUrl": "https://checkout.dispatch.example"}
	}`)

	for _, tc := range []struct {
		name        string
		environment map[string]string
		serverURL   string
		natsURLs    []string
	}{
		{
			name:      "no environment preserves merged config",
			serverURL: "https://checkout.dispatch.example",
			natsURLs:  []string{"nats://user:4222"},
		},
		{
			name: "environment overrides both files",
			environment: map[string]string{
				"DISPATCH_SERVER_URL": "https://environment.dispatch.example",
				"NATS_URLS":           "nats://environment-one:4222, nats://environment-two:4222",
			},
			serverURL: "https://environment.dispatch.example",
			natsURLs:  []string{"nats://environment-one:4222", "nats://environment-two:4222"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(environment(tc.environment), LoadOptions{CWD: cwd, HomeDir: home})
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.Dispatch == nil || cfg.Dispatch.ServerURL != tc.serverURL {
				t.Fatalf("dispatch.serverUrl: got %+v, want %q", cfg.Dispatch, tc.serverURL)
			}
			if strings.Join(cfg.Envoy.NatsURLs, ",") != strings.Join(tc.natsURLs, ",") {
				t.Errorf("envoy.natsUrls: got %q, want %q", cfg.Envoy.NatsURLs, tc.natsURLs)
			}
		})
	}
}

func TestLoadRejectsInvalidEnvironmentOverrides(t *testing.T) {
	for _, tc := range []struct {
		name        string
		environment map[string]string
		variable    string
	}{
		{name: "empty server URL", environment: map[string]string{"DISPATCH_SERVER_URL": ""}, variable: "DISPATCH_SERVER_URL"},
		{name: "non-http scheme", environment: map[string]string{"DISPATCH_SERVER_URL": "ftp://dispatch.example"}, variable: "DISPATCH_SERVER_URL"},
		{name: "userinfo", environment: map[string]string{"DISPATCH_SERVER_URL": "https://user@dispatch.example"}, variable: "DISPATCH_SERVER_URL"},
		{name: "query", environment: map[string]string{"DISPATCH_SERVER_URL": "https://dispatch.example?next=oauth"}, variable: "DISPATCH_SERVER_URL"},
		{name: "fragment", environment: map[string]string{"DISPATCH_SERVER_URL": "https://dispatch.example#oauth"}, variable: "DISPATCH_SERVER_URL"},
		{name: "path", environment: map[string]string{"DISPATCH_SERVER_URL": "https://dispatch.example/auth/callback"}, variable: "DISPATCH_SERVER_URL"},
		{name: "empty NATS URLs", environment: map[string]string{"NATS_URLS": ""}, variable: "NATS_URLS"},
		{name: "empty NATS URL element", environment: map[string]string{"NATS_URLS": "nats://one:4222, "}, variable: "NATS_URLS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(environment(tc.environment), LoadOptions{CWD: t.TempDir(), HomeDir: t.TempDir()})
			if err == nil {
				t.Fatalf("expected %s to be rejected", tc.variable)
			}
			if !strings.Contains(err.Error(), tc.variable) {
				t.Fatalf("error: got %q, want %s", err, tc.variable)
			}
		})
	}
}

// The NATS URLs live in the envoy section now, so a file that still carries the top-level key is
// refused, in either file: the user file's naming where they go, the checkout's saying it holds
// only Dispatch.
func TestLoadRefusesTopLevelNatsURLs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		path  func(home, cwd string) string
		issue string
	}{
		{name: "user file", path: func(home, _ string) string { return userFile(home) },
			issue: "natsUrls: no longer read; write it as envoy.natsUrls"},
		{name: "checkout file", path: func(_, cwd string) string { return checkoutFile(cwd) },
			issue: "natsUrls: not read, and a checkout's envoy.json holds only a dispatch section"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			cwd := t.TempDir()
			path := tc.path(home, cwd)
			mustWrite(t, path, `{"natsUrls": ["nats://user:4222"]}`)
			_, err := Load(environment(nil), LoadOptions{CWD: cwd, HomeDir: home})
			var invalid *InvalidConfigError
			if !errors.As(err, &invalid) {
				t.Fatalf("err = %v, want an *InvalidConfigError", err)
			}
			if invalid.Path != path || len(invalid.Issues) != 1 || invalid.Issues[0] != tc.issue {
				t.Errorf("err = %+v, want %s refused with %q", invalid, path, tc.issue)
			}
		})
	}
}

// The OpenCode-era user file is not read. While it is the only user file beside the current one's
// directory, Load refuses, naming both paths and the move, whatever the environment overrides; once
// the new file exists, the old one is ignored. An XDG_CONFIG_HOME naming another directory is not
// where the old file belongs, so nothing refuses there.
func TestLoadRefusesTheOldUserFile(t *testing.T) {
	home := t.TempDir()
	oldPath := filepath.Join(home, ".config", "opencode", "envoy.json")
	mustWrite(t, oldPath, `{"natsUrls": ["nats://old:4222"], "dispatch": {"serverUrl": "https://old.dispatch.example"}}`)

	for _, tc := range []struct {
		name   string
		values map[string]string
	}{
		{name: "no environment"},
		{name: "XDG_CONFIG_HOME is ~/.config", values: map[string]string{"XDG_CONFIG_HOME": filepath.Join(home, ".config") + "/"}},
		{name: "both overrides set", values: map[string]string{
			"DISPATCH_SERVER_URL": "https://environment.dispatch.example",
			"NATS_URLS":           "nats://environment:4222",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(environment(tc.values), LoadOptions{CWD: t.TempDir(), HomeDir: home})
			if err == nil {
				t.Fatalf("expected a refusal while only the old user file exists")
			}
			want := oldPath + " is not read: mkdir -p " + filepath.Dir(userFile(home)) + " && mv " + oldPath + " " + userFile(home)
			if err.Error() != want {
				t.Errorf("error: got %q, want %q", err, want)
			}
		})
	}

	configHome := t.TempDir()
	cfg, err := Load(environment(map[string]string{"XDG_CONFIG_HOME": configHome}), LoadOptions{CWD: t.TempDir(), HomeDir: home})
	if err != nil {
		t.Fatalf("load under another XDG_CONFIG_HOME: %v", err)
	}
	if cfg.Dispatch != nil || cfg.Envoy.NatsURLs != nil {
		t.Errorf("got %+v (dispatch %+v), want nothing from the old file", cfg.Envoy, cfg.Dispatch)
	}

	mustWrite(t, userFile(home), `{"envoy": {"natsUrls": ["nats://new:4222"]}}`)
	cfg, err = Load(environment(nil), LoadOptions{CWD: t.TempDir(), HomeDir: home})
	if err != nil {
		t.Fatalf("load with both files: %v", err)
	}
	if strings.Join(cfg.Envoy.NatsURLs, ",") != "nats://new:4222" || cfg.Dispatch != nil {
		t.Errorf("got %+v (dispatch %+v), want only the new file's values", cfg.Envoy, cfg.Dispatch)
	}
}

// A checkout's file is the repository's: it may configure Dispatch, never the NATS servers the
// process connects to, so any envoy section in it is refused, naming the user file it goes in,
// whatever the environment overrides.
func TestLoadRefusesAnEnvoySectionInTheCheckoutFile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		envoy  string
		values map[string]string
	}{
		{name: "NATS servers", envoy: `{"natsUrls": ["nats://repo.internal.example:4222"]}`},
		{name: "NATS servers beside NATS_URLS", envoy: `{"natsUrls": ["nats://repo.internal.example:4222"]}`,
			values: map[string]string{"NATS_URLS": "nats://environment:4222"}},
		{name: "off", envoy: `{"enabled": false}`},
		{name: "an unknown key", envoy: `{"natsUrl": "nats://repo.internal.example:4222"}`},
		{name: "not an object", envoy: `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			cwd := t.TempDir()
			mustWrite(t, userFile(home), `{"envoy": {"natsUrls": ["nats://user:4222"]}}`)
			path := checkoutFile(cwd)
			mustWrite(t, path, `{"dispatch": {"serverUrl": "https://checkout.dispatch.example"}, "envoy": `+tc.envoy+`}`)
			_, err := Load(environment(tc.values), LoadOptions{CWD: cwd, HomeDir: home})
			var invalid *InvalidConfigError
			if !errors.As(err, &invalid) {
				t.Fatalf("err = %v, want an *InvalidConfigError", err)
			}
			want := "envoy: not read from a checkout's envoy.json; put it in " + userFile(home)
			if invalid.Path != path || len(invalid.Issues) != 1 || invalid.Issues[0] != want {
				t.Errorf("err = %+v, want %s refused with %q", invalid, path, want)
			}
		})
	}
}

// envoy.enabled switches a session's Envoy, not the server's, so the server reads envoy.natsUrls
// from a section that is off.
func TestLoadReadsNatsURLsFromAnEnvoySectionThatIsOff(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, userFile(home), `{"envoy": {"enabled": false, "natsUrls": ["nats://user:4222"]}}`)
	cfg, err := Load(environment(nil), LoadOptions{CWD: t.TempDir(), HomeDir: home})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := strings.Join(cfg.Envoy.NatsURLs, ","); got != "nats://user:4222" {
		t.Errorf("envoy.natsUrls: got %q, want nats://user:4222", got)
	}
}

// The OpenCode-era checkout file is not read, whatever it holds, and nothing in it is refused: while
// the checkout has no .envoy/envoy.json, one Warning names the rename.
func TestLoadIgnoresTheOldCheckoutFile(t *testing.T) {
	cwd := t.TempDir()
	oldPath := filepath.Join(cwd, ".opencode", "envoy.json")
	mustWrite(t, oldPath, `{
		"dispatch": {"serverUrl": "https://old.dispatch.example"},
		"natsUrls": ["nats://checkout:4222"],
		"envoy": {"natsUrls": ["nats://other-checkout:4222"]}
	}`)
	cfg, err := Load(environment(nil), LoadOptions{CWD: cwd, HomeDir: t.TempDir()})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Dispatch != nil || cfg.Envoy.NatsURLs != nil {
		t.Errorf("got %+v (dispatch %+v), want nothing from .opencode/envoy.json", cfg.Envoy, cfg.Dispatch)
	}
	want := Warning{Message: WarnOpenCodeCheckout, Path: oldPath,
		Fix: oldPath + " is not read: mkdir -p " + filepath.Join(cwd, ".envoy") + " && mv " + oldPath + " " + checkoutFile(cwd)}
	if len(cfg.Warnings) != 1 || cfg.Warnings[0] != want {
		t.Errorf("warnings: got %+v, want only %+v", cfg.Warnings, want)
	}

	// A .envoy/envoy.json beside it is read, and the old file is not named.
	mustWrite(t, checkoutFile(cwd), `{"dispatch": {"serverUrl": "https://new.dispatch.example"}}`)
	cfg, err = Load(environment(nil), LoadOptions{CWD: cwd, HomeDir: t.TempDir()})
	if err != nil {
		t.Fatalf("load with both checkout files: %v", err)
	}
	if cfg.Dispatch == nil || cfg.Dispatch.ServerURL != "https://new.dispatch.example" || len(cfg.Warnings) != 0 {
		t.Errorf("got %+v (dispatch %+v), want the new checkout file and no warning", cfg, cfg.Dispatch)
	}
}

// An agent box bind-mounts the host's ~/.config/opencode: a mv inside it would move the host's file
// into the box's own throwaway filesystem. Where the old file's directory and the nearest existing
// ancestor of the new file's directory are on different devices, the refusal and the warning name
// the file to create and say to move the old one on the machine that owns it, with no mv.
func TestLoadNamesNoMoveAcrossFilesystems(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	oldUser := filepath.Join(home, ".config", "opencode", "envoy.json")
	oldCheckout := filepath.Join(cwd, ".opencode", "envoy.json")
	mustWrite(t, oldUser, `{}`)
	mustWrite(t, oldCheckout, `{}`)
	mounted := map[string]bool{filepath.Dir(oldUser): true, filepath.Dir(oldCheckout): true}
	var asked []string
	deviceOf := func(directory string) (uint64, bool) {
		asked = append(asked, directory)
		if !exists(directory) {
			return 0, false
		}
		if mounted[directory] {
			return 2, true
		}
		return 1, true
	}
	across := func(oldPath, newPath string) string {
		return oldPath + " is not read, and is on another filesystem than " + newPath + " (a mount): create " + newPath + ", and move " + oldPath + " on the machine that owns it"
	}

	_, err := Load(environment(nil), LoadOptions{CWD: cwd, HomeDir: home, DeviceOf: deviceOf})
	if err == nil || err.Error() != across(oldUser, userFile(home)) {
		t.Errorf("error: got %v, want %q", err, across(oldUser, userFile(home)))
	}
	if !slices.Contains(asked, filepath.Join(home, ".config")) {
		t.Errorf("devices asked: %v, want ~/.config's, the nearest existing ancestor of ~/.config/legion", asked)
	}

	mustWrite(t, userFile(home), `{}`)
	cfg, err := Load(environment(nil), LoadOptions{CWD: cwd, HomeDir: home, DeviceOf: deviceOf})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := Warning{Message: WarnOpenCodeCheckout, Path: oldCheckout, Fix: across(oldCheckout, checkoutFile(cwd))}
	if len(cfg.Warnings) != 1 || cfg.Warnings[0] != want {
		t.Errorf("warnings: got %+v, want only %+v", cfg.Warnings, want)
	}
	if strings.Contains(cfg.Warnings[0].Fix, "mv ") {
		t.Errorf("warning %q gives a mv across filesystems", cfg.Warnings[0].Fix)
	}
}

// A repository may hold a regular file named .envoy: no checkout file is there to read.
func TestLoadReadsNoCheckoutFileThroughARegularFile(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	mustWrite(t, userFile(home), `{"dispatch": {"serverUrl": "https://user.dispatch.example"}}`)
	mustWrite(t, filepath.Join(cwd, ".envoy"), "not a directory")
	cfg, err := Load(environment(nil), LoadOptions{CWD: cwd, HomeDir: home})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Dispatch == nil || cfg.Dispatch.ServerURL != "https://user.dispatch.example" {
		t.Errorf("dispatch: got %+v, want the user file's", cfg.Dispatch)
	}
}

// The clients read envoy.natsUrls trimmed, so the server does too.
func TestLoadTrimsNatsURLs(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, userFile(home), `{"envoy": {"natsUrls": [" nats://one:4222 ", "nats://two:4222\n"]}}`)
	cfg, err := Load(environment(nil), LoadOptions{CWD: t.TempDir(), HomeDir: home})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := strings.Join(cfg.Envoy.NatsURLs, ","); got != "nats://one:4222,nats://two:4222" {
		t.Errorf("envoy.natsUrls: got %q, want them trimmed", got)
	}
}

// A file that is not JSON is refused by its line and column, and the error quotes none of it: the
// decoder's own message names the character it stopped at, the start of an unquoted token.
func TestLoadRefusesMalformedJSONWithoutQuotingIt(t *testing.T) {
	home := t.TempDir()
	path := userFile(home)
	mustWrite(t, path, "{\n  \"dispatch\": {\"token\": hunter2secret}\n}")
	_, err := Load(environment(nil), LoadOptions{CWD: t.TempDir(), HomeDir: home})
	var invalid *InvalidConfigError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v, want an *InvalidConfigError", err)
	}
	if want := ": invalid JSON at line 2, column 25"; invalid.Path != path || len(invalid.Issues) != 1 || invalid.Issues[0] != want {
		t.Errorf("err = %+v, want %s refused with %q", invalid, path, want)
	}
}

// JSON null is no value for any key of envoy.json, as the clients' schema reads it: a file the
// server starts on is one every client reads.
func TestLoadRefusesNull(t *testing.T) {
	for _, tc := range []struct {
		contents string
		issue    string
	}{
		{`null`, ": Expected object"},
		{`[]`, ": Expected object"},
		{`{"$schema": null}`, "$schema: Expected string"},
		{`{"dispatch": null}`, "dispatch: Expected object"},
		{`{"dispatch": {"enabled": null}}`, "dispatch.enabled: Expected boolean"},
		{`{"dispatch": {"serverUrl": null}}`, "dispatch.serverUrl: Expected string"},
		{`{"dispatch": {"token": null}}`, "dispatch.token: Expected string"},
		{`{"envoy": null}`, "envoy: Expected object"},
		{`{"envoy": {"enabled": null}}`, "envoy.enabled: Expected boolean"},
		{`{"envoy": {"url": null}}`, "envoy.url: Expected string"},
		{`{"envoy": {"natsUrls": null}}`, "envoy.natsUrls: Expected string array"},
		{`{"envoy": {"natsUrls": [null]}}`, "envoy.natsUrls.0: must not be empty"},
		{`{"envoy": {"token": null}}`, "envoy.token: Expected string"},
		{`{"envoy": {"natsSeed": null}}`, "envoy.natsSeed: Expected string"},
	} {
		t.Run(tc.contents, func(t *testing.T) {
			home := t.TempDir()
			path := userFile(home)
			mustWrite(t, path, tc.contents)
			_, err := Load(environment(nil), LoadOptions{CWD: t.TempDir(), HomeDir: home})
			var invalid *InvalidConfigError
			if !errors.As(err, &invalid) {
				t.Fatalf("err = %v, want an *InvalidConfigError", err)
			}
			if invalid.Path != path || len(invalid.Issues) != 1 || invalid.Issues[0] != tc.issue {
				t.Errorf("err = %+v, want %s refused with %q", invalid, path, tc.issue)
			}
		})
	}
}

func TestLoadAcceptsEveryEnvoyKey(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, userFile(home), `{
		"envoy": {
			"enabled": true,
			"url": "http://127.0.0.1:9020",
			"natsUrls": ["nats://envoy-nats.internal.example:4222"],
			"token": "envoy-token",
			"natsSeed": "!secrets get NATS_NKEY_SEED"
		}
	}`)
	cfg, err := Load(environment(nil), LoadOptions{CWD: t.TempDir(), HomeDir: home})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if strings.Join(cfg.Envoy.NatsURLs, ",") != "nats://envoy-nats.internal.example:4222" {
		t.Errorf("envoy.natsUrls: got %q", cfg.Envoy.NatsURLs)
	}
}

func TestLoadRejectsAMalformedEnvoySection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		envoy string
		issue string
	}{
		{name: "not an object", envoy: `[]`, issue: "envoy: Expected object"},
		{name: "unknown key", envoy: `{"natsUrl": "nats://one:4222"}`, issue: "envoy.natsUrl: Unrecognized key"},
		{name: "enabled", envoy: `{"enabled": "yes"}`, issue: "envoy.enabled: Expected boolean"},
		{name: "url type", envoy: `{"url": 9020}`, issue: "envoy.url: Expected string"},
		{name: "url empty", envoy: `{"url": ""}`, issue: "envoy.url: must be an http or https URL"},
		{name: "url not a URL", envoy: `{"url": "not a url"}`, issue: "envoy.url: must be an http or https URL"},
		{name: "url relative", envoy: `{"url": "/relative"}`, issue: "envoy.url: must be an http or https URL"},
		{name: "url without a scheme", envoy: `{"url": "localhost:9020"}`, issue: "envoy.url: must be an http or https URL"},
		{name: "url of another scheme", envoy: `{"url": "ftp://listener.internal.example"}`, issue: "envoy.url: must be an http or https URL"},
		{name: "url without a host", envoy: `{"url": "http://"}`, issue: "envoy.url: must be an http or https URL"},
		{name: "natsUrls", envoy: `{"natsUrls": "nats://one:4222"}`, issue: "envoy.natsUrls: Expected string array"},
		{name: "natsUrls empty", envoy: `{"natsUrls": ["nats://one:4222", " "]}`, issue: "envoy.natsUrls.1: must not be empty"},
		{name: "token", envoy: `{"token": 1}`, issue: "envoy.token: Expected string"},
		{name: "natsSeed", envoy: `{"natsSeed": false}`, issue: "envoy.natsSeed: Expected string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := userFile(home)
			mustWrite(t, path, `{"envoy": `+tc.envoy+`}`)
			_, err := Load(environment(nil), LoadOptions{CWD: t.TempDir(), HomeDir: home})
			var invalid *InvalidConfigError
			if !errors.As(err, &invalid) {
				t.Fatalf("err = %v, want an *InvalidConfigError", err)
			}
			if invalid.Path != path || !strings.Contains(err.Error(), tc.issue) {
				t.Errorf("err = %v, want %s naming %q", err, path, tc.issue)
			}
		})
	}
}

func TestLoadRejectsRemovedDefaultRepoKey(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, userFile(home), `{
		"dispatch": {"defaultRepo": "user/repo"}
	}`)
	_, err := Load(environment(nil), LoadOptions{CWD: t.TempDir(), HomeDir: home})
	if err == nil {
		t.Fatalf("expected an error for the removed dispatch.defaultRepo key")
	}
	if !strings.Contains(err.Error(), "dispatch.defaultRepo") {
		t.Errorf("error should name the key, got %q", err.Error())
	}
}

func TestLoadRejectsRemovedAppClientIDKey(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, userFile(home), `{
		"dispatch": {"appClientId": "user-id"}
	}`)
	_, err := Load(environment(nil), LoadOptions{CWD: t.TempDir(), HomeDir: home})
	if err == nil {
		t.Fatalf("expected an error for the removed dispatch.appClientId key")
	}
	if !strings.Contains(err.Error(), "dispatch.appClientId") {
		t.Errorf("error should name the key, got %q", err.Error())
	}
}

func TestLoadRejectsInvalidServerURL(t *testing.T) {
	for _, serverURL := range []string{"", "not a url", "/relative", "localhost:8766", "mailto:a@b", "http://"} {
		t.Run(serverURL, func(t *testing.T) {
			home := t.TempDir()
			mustWrite(t, userFile(home), `{"dispatch": {"serverUrl": "`+serverURL+`"}}`)
			_, err := Load(environment(nil), LoadOptions{CWD: t.TempDir(), HomeDir: home})
			var invalid *InvalidConfigError
			if !errors.As(err, &invalid) || len(invalid.Issues) != 1 || invalid.Issues[0] != "dispatch.serverUrl: must be an http or https URL" {
				t.Errorf("err = %v, want dispatch.serverUrl refused as not an http or https URL", err)
			}
		})
	}
}

func TestLoadMissingFilesReturnEmpty(t *testing.T) {
	cfg, err := Load(environment(nil), LoadOptions{CWD: t.TempDir(), HomeDir: t.TempDir()})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg == nil {
		t.Fatalf("expected non-nil config")
	}
	if cfg.Dispatch != nil {
		t.Errorf("expected nil dispatch, got %+v", cfg.Dispatch)
	}
	if cfg.Envoy.NatsURLs != nil {
		t.Errorf("expected no NATS URLs, got %q", cfg.Envoy.NatsURLs)
	}
}

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}
