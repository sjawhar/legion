package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/config"
)

// storetestImport is the one package under internal/dispatch that reads the process environment
// for itself: the Postgres fixture of Dispatch's tests (DISPATCH_TEST_DATABASE_URL), which only
// test files may import.
const storetestImport = "github.com/sjawhar/envoy/internal/dispatch/store/storetest"

// Dispatch reads its environment in one place: processSettings, over the settings table. Any
// other reference to the process environment under cmd/dispatch or internal/dispatch is a reader
// the table, `envoy-dispatch settings` and the generated configuration reference do not list.
func TestNoReaderBypassesTheSettingsTable(t *testing.T) {
	environmentReaders := map[string][]string{
		"os":      {"Getenv", "LookupEnv", "Environ", "ExpandEnv"},
		"syscall": {"Getenv", "Environ"},
	}
	fileSet := token.NewFileSet()
	var reads, storetestImporters []string
	for _, root := range []string{".", filepath.Join("..", "..", "internal", "dispatch")} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" || entry.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			// The name each environment-reading package has in this file.
			packages := map[string]string{}
			for _, spec := range file.Imports {
				importPath, _ := strconv.Unquote(spec.Path.Value)
				if importPath == storetestImport {
					storetestImporters = append(storetestImporters, path)
				}
				if _, reads := environmentReaders[importPath]; !reads {
					continue
				}
				name := importPath
				if spec.Name != nil {
					name = spec.Name.Name
				}
				packages[name] = importPath
			}
			if strings.Contains(filepath.ToSlash(path), "store/storetest/") {
				return nil
			}
			ast.Inspect(file, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := selector.X.(*ast.Ident)
				if !ok {
					return true
				}
				if importPath, ok := packages[pkg.Name]; ok && slices.Contains(environmentReaders[importPath], selector.Sel.Name) {
					position := fileSet.Position(selector.Pos())
					reads = append(reads, filepath.ToSlash(position.Filename)+" "+importPath+"."+selector.Sel.Name)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if want := []string{"settings.go os.LookupEnv"}; !slices.Equal(reads, want) {
		t.Errorf("environment reads outside the settings table: %q, want only %q (processSettings). Add the variable as a row of the settings table (cmd/dispatch/settings.go) and hand its reader the value", reads, want)
	}
	if len(storetestImporters) > 0 {
		t.Errorf("non-test files import %s, which reads DISPATCH_TEST_DATABASE_URL outside the settings table: %q", storetestImport, storetestImporters)
	}
}

func TestSettingsTableNamesEachVariableOnce(t *testing.T) {
	name := regexp.MustCompile(`^[A-Z][A-Z0-9]*(_[A-Z0-9]+)*$`)
	seen := map[string]bool{}
	for _, row := range settings {
		variables := []string{row.Name}
		if row.File {
			variables = append(variables, row.fileVariable())
		}
		for _, variable := range variables {
			if seen[variable] {
				t.Errorf("%s is in the settings table twice", variable)
			}
			seen[variable] = true
		}
		if !name.MatchString(row.Name) {
			t.Errorf("%q is not an environment variable name", row.Name)
		}
		if row.Required != "yes" && row.Required != "no" && !strings.HasPrefix(row.Required, "when ") {
			t.Errorf("%s: Required %q, want yes, no, or when <condition>", row.Name, row.Required)
		}
		if row.Description == "" || strings.Contains(row.Description, "\n") {
			t.Errorf("%s: Description %q, want one line", row.Name, row.Description)
		}
	}
}

// `envoy-dispatch settings` is what the docs site's configuration reference is generated from, so
// it carries every row whole: the _FILE form by name, and null where a row has no default.
func TestSettingsSubcommandPrintsEveryRow(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runSubcommand(context.Background(), []string{"settings"}, envGetter(nil), &stdout, &stderr); code != 0 {
		t.Fatalf("envoy-dispatch settings: exit %d, stderr %q", code, stderr.String())
	}
	var printed struct {
		Settings []settingEntry `json:"settings"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &printed); err != nil {
		t.Fatalf("decode %s: %v", stdout.String(), err)
	}
	if len(printed.Settings) != len(settings) {
		t.Fatalf("printed %d settings, want the table's %d", len(printed.Settings), len(settings))
	}
	for index, row := range settings {
		entry := printed.Settings[index]
		file, defaultValue := "", ""
		if entry.File != nil {
			file = *entry.File
		}
		if entry.Default != nil {
			defaultValue = *entry.Default
		}
		wantFile := ""
		if row.File {
			wantFile = row.Name + "_FILE"
		}
		if entry.Name != row.Name || file != wantFile || (entry.File != nil) != row.File || defaultValue != row.Default ||
			(entry.Default != nil) != (row.Default != "") || entry.Required != row.Required || entry.Description != row.Description {
			t.Errorf("printed %+v (file %q, default %q), want row %+v", entry, file, defaultValue, row)
		}
	}
}

func TestSettingValuesRefuseAVariableTheTableDoesNotList(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered == nil || !strings.Contains(recovered.(string), "DISPATCH_NOT_A_SETTING") {
			t.Fatalf("recovered %v, want a panic naming DISPATCH_NOT_A_SETTING", recovered)
		}
	}()
	envGetter(map[string]string{"DISPATCH_NOT_A_SETTING": "x"}).get("DISPATCH_NOT_A_SETTING")
}

// bootEnvironment is an environment resolveBootConfig accepts, with overrides applied; an override
// of "" leaves the variable empty.
func bootEnvironment(overrides map[string]string) settingValues {
	values := map[string]string{
		"DATABASE_URL":            "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":    "agent-token",
		"DISPATCH_ALLOWED_LOGINS": "alice",
	}
	maps.Copy(values, overrides)
	return envGetter(values)
}

func resolveWith(t *testing.T, overrides map[string]string) bootConfig {
	t.Helper()
	boot, err := resolveBootConfig(bootEnvironment(overrides))
	if err != nil {
		t.Fatalf("resolveBootConfig(%v): %v", overrides, err)
	}
	return boot
}

func refusedWith(t *testing.T, overrides map[string]string, naming string) {
	t.Helper()
	if _, err := resolveBootConfig(bootEnvironment(overrides)); err == nil || !strings.Contains(err.Error(), naming) {
		t.Errorf("resolveBootConfig(%v): err = %v, want a refusal naming %s", overrides, err, naming)
	}
}

// writeFile writes contents under a test directory and returns its path.
func writeFile(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// envoyJSONHome is a home directory whose envoy.json names a dashboard origin and NATS servers,
// for the settings that override it.
func envoyJSONHome(t *testing.T) string {
	t.Helper()
	path := writeFile(t, filepath.Join(".config", "opencode", "envoy.json"),
		`{"natsUrls": ["nats://from-envoy-json:4222"], "dispatch": {"serverUrl": "https://from-envoy-json.example"}}`)
	return filepath.Dir(filepath.Dir(filepath.Dir(path)))
}

// Every setting reaches its reader through the table, as it resolved before the table existed:
// each case hands its variable to the reader main hands it to, through the settings read alone,
// never the process environment, so a reader that reads around the table fails its case.
func TestEverySettingReachesItsReader(t *testing.T) {
	appJSON := func(t *testing.T) string {
		return filepath.Dir(writeFile(t, "app.json", `{"clientId":"Iv1.file","clientSecret":"file-secret"}`))
	}
	loadApp := func(t *testing.T, values map[string]string) (string, string, error) {
		t.Helper()
		app, source, err := loadAppCredentials(envGetter(values), appJSON(t))
		if err != nil {
			return "", "", err
		}
		return app.ClientID, source.String(), nil
	}
	envoyJSON := func(t *testing.T, values map[string]string) (*config.EnvoyConfig, error) {
		t.Helper()
		return loadEnvoyConfig(envGetter(values), config.LoadOptions{CWD: t.TempDir(), HomeDir: envoyJSONHome(t)})
	}
	natsConnect := func(values map[string]string, url string) error {
		client, err := bus.Connect([]string{url}, bus.WithEnvironment(envGetter(values).lookup))
		if err == nil {
			client.Close()
		}
		return err
	}
	// A URL nothing listens on: every case below fails before it dials, on the setting it names.
	const localNATS = "nats://127.0.0.1:1"

	cases := map[string]func(t *testing.T){
		"DATABASE_URL": func(t *testing.T) {
			if boot := resolveWith(t, map[string]string{"DATABASE_URL": " postgres://from-the-table "}); boot.DatabaseURL != "postgres://from-the-table" {
				t.Errorf("DatabaseURL = %q", boot.DatabaseURL)
			}
			refusedWith(t, map[string]string{"DATABASE_URL": ""}, "DATABASE_URL")
			var stdout, stderr bytes.Buffer
			unreachable := map[string]string{"DATABASE_URL": "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"}
			if code := runSubcommand(context.Background(), []string{"census"}, envGetter(unreachable), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "census: connect") {
				t.Errorf("census with an unreachable DATABASE_URL: exit %d, stderr %q, want a connect failure", code, stderr.String())
			}
		},
		"DISPATCH_AGENT_TOKEN": func(t *testing.T) {
			if boot := resolveWith(t, map[string]string{"DISPATCH_AGENT_TOKEN": " table-token "}); boot.AgentToken != "table-token" {
				t.Errorf("AgentToken = %q", boot.AgentToken)
			}
			refusedWith(t, map[string]string{"DISPATCH_AGENT_TOKEN": ""}, "DISPATCH_AGENT_TOKEN")
		},
		"DISPATCH_IDENTITY": func(t *testing.T) {
			if boot := resolveWith(t, map[string]string{"DISPATCH_IDENTITY": "header:X-Dispatch-User"}); boot.IdentityHeader != "X-Dispatch-User" {
				t.Errorf("IdentityHeader = %q", boot.IdentityHeader)
			}
			if boot := resolveWith(t, nil); boot.IdentityHeader != "" {
				t.Errorf("unset: IdentityHeader = %q, want cookie identity", boot.IdentityHeader)
			}
			refusedWith(t, map[string]string{"DISPATCH_IDENTITY": "basic"}, "DISPATCH_IDENTITY")
		},
		"DISPATCH_ALLOWED_LOGINS": func(t *testing.T) {
			boot := resolveWith(t, map[string]string{"DISPATCH_ALLOWED_LOGINS": " Alice ,bob,"})
			if !maps.Equal(boot.AllowedLogins, map[string]struct{}{"alice": {}, "bob": {}}) {
				t.Errorf("AllowedLogins = %v", boot.AllowedLogins)
			}
			refusedWith(t, map[string]string{"DISPATCH_ALLOWED_LOGINS": ""}, "DISPATCH_ALLOWED_LOGINS")
			resolveWith(t, map[string]string{"DISPATCH_ALLOWED_LOGINS": "", "DISPATCH_IDENTITY": "header:X-Dispatch-User"})
		},
		"DISPATCH_IDENTITY_HEADER_TRUSTED": func(t *testing.T) {
			header := map[string]string{"DISPATCH_IDENTITY": "header:X-Dispatch-User", "DISPATCH_APP_CLIENT_ID": "Iv1.app"}
			refusedWith(t, header, "DISPATCH_IDENTITY_HEADER_TRUSTED")
			header["DISPATCH_IDENTITY_HEADER_TRUSTED"] = "1"
			resolveWith(t, header)
		},
		"DISPATCH_LISTEN_HOST": func(t *testing.T) {
			if boot := resolveWith(t, map[string]string{"DISPATCH_LISTEN_HOST": "[::1]"}); boot.ListenAddr != "[::1]:8766" {
				t.Errorf("ListenAddr = %q", boot.ListenAddr)
			}
			if boot := resolveWith(t, nil); boot.ListenAddr != ":8766" {
				t.Errorf("unset: ListenAddr = %q, want every interface", boot.ListenAddr)
			}
		},
		"DISPATCH_PORT": func(t *testing.T) {
			if boot := resolveWith(t, map[string]string{"DISPATCH_PORT": "9123"}); boot.ListenAddr != ":9123" {
				t.Errorf("ListenAddr = %q", boot.ListenAddr)
			}
			refusedWith(t, map[string]string{"DISPATCH_PORT": "0"}, "DISPATCH_PORT")
		},
		"DISPATCH_SERVER_URL": func(t *testing.T) {
			if cfg, err := envoyJSON(t, map[string]string{"DISPATCH_SERVER_URL": "https://from-the-table.example"}); err != nil || cfg.Dispatch.ServerURL != "https://from-the-table.example" {
				t.Errorf("set: %+v, %v; want the variable over envoy.json", cfg, err)
			}
			if cfg, err := envoyJSON(t, nil); err != nil || cfg.Dispatch.ServerURL != "https://from-envoy-json.example" {
				t.Errorf("unset: %+v, %v; want envoy.json's", cfg, err)
			}
			if _, err := envoyJSON(t, map[string]string{"DISPATCH_SERVER_URL": ""}); err == nil || !strings.Contains(err.Error(), "DISPATCH_SERVER_URL") {
				t.Errorf("set but empty: err = %v, want a refusal naming DISPATCH_SERVER_URL", err)
			}
		},
		"NATS_URLS": func(t *testing.T) {
			if cfg, err := envoyJSON(t, map[string]string{"NATS_URLS": "nats://one:4222, nats://two:4222"}); err != nil || !slices.Equal(cfg.NatsURLs, []string{"nats://one:4222", "nats://two:4222"}) {
				t.Errorf("set: %+v, %v; want the variable over envoy.json", cfg, err)
			}
			if cfg, err := envoyJSON(t, nil); err != nil || !slices.Equal(cfg.NatsURLs, []string{"nats://from-envoy-json:4222"}) {
				t.Errorf("unset: %+v, %v; want envoy.json's", cfg, err)
			}
			if _, err := envoyJSON(t, map[string]string{"NATS_URLS": ""}); err == nil || !strings.Contains(err.Error(), "NATS_URLS") {
				t.Errorf("set but empty: err = %v, want a refusal naming NATS_URLS", err)
			}
		},
		"ENVOY_ALLOW_REMOTE_NATS": func(t *testing.T) {
			const remote = "nats://nats.example:4222"
			if err := natsConnect(nil, remote); !errors.Is(err, bus.ErrRemoteNATS) {
				t.Errorf("unset: connect %s = %v, want a refusal", remote, err)
			}
			// Allowed, the connect goes on to the credential, which here fails before any dial.
			if err := natsConnect(map[string]string{"ENVOY_ALLOW_REMOTE_NATS": "1", "NATS_NKEY_SEED": "not a seed"}, remote); err == nil || errors.Is(err, bus.ErrRemoteNATS) || !strings.Contains(err.Error(), "NATS_NKEY_SEED") {
				t.Errorf("ENVOY_ALLOW_REMOTE_NATS=1: connect %s = %v, want it past the refusal", remote, err)
			}
			if _, err := resolveBootConfig(devSignInEnvironment(map[string]string{"ENVOY_ALLOW_REMOTE_NATS": "1"})); err == nil || !strings.Contains(err.Error(), "ENVOY_ALLOW_REMOTE_NATS") {
				t.Errorf("dev sign-in with ENVOY_ALLOW_REMOTE_NATS=1: err = %v, want a refusal naming it", err)
			}
		},
		"NATS_NKEY_SEED": func(t *testing.T) {
			if err := natsConnect(map[string]string{"NATS_NKEY_SEED": "not a seed"}, localNATS); err == nil || !strings.Contains(err.Error(), "NATS_NKEY_SEED does not hold a valid nkey seed") {
				t.Errorf("NATS_NKEY_SEED: err = %v, want the seed refused", err)
			}
			file := writeFile(t, "seed", "  not a seed either\n")
			if err := natsConnect(map[string]string{"NATS_NKEY_SEED_FILE": file, "NATS_NKEY_SEED": "not a seed"}, localNATS); err == nil || !strings.Contains(err.Error(), "NATS_NKEY_SEED_FILE ("+file+")") {
				t.Errorf("both set: err = %v, want the file to win", err)
			}
			if err := natsConnect(map[string]string{"NATS_NKEY_SEED_FILE": ""}, localNATS); err == nil || !strings.Contains(err.Error(), "NATS_NKEY_SEED_FILE is set but empty") {
				t.Errorf("NATS_NKEY_SEED_FILE set but empty: err = %v, want it refused", err)
			}
		},
		"DISPATCH_NATS_DISABLED": func(t *testing.T) {
			if boot := resolveWith(t, map[string]string{"DISPATCH_NATS_DISABLED": "1"}); !boot.NATSDisabled {
				t.Error("DISPATCH_NATS_DISABLED=1: NATS on")
			}
			if boot := resolveWith(t, map[string]string{"DISPATCH_NATS_DISABLED": "true"}); boot.NATSDisabled {
				t.Error("DISPATCH_NATS_DISABLED=true: NATS off, want only 1 to turn it off")
			}
		},
		"ENVOY_URL": func(t *testing.T) {
			if boot := resolveWith(t, nil); boot.EnvoyURL != "http://127.0.0.1:9020" {
				t.Errorf("unset: EnvoyURL = %q", boot.EnvoyURL)
			}
			if boot := resolveWith(t, map[string]string{"ENVOY_URL": "http://listener.example:9020/"}); boot.EnvoyURL != "http://listener.example:9020" {
				t.Errorf("EnvoyURL = %q", boot.EnvoyURL)
			}
			refusedWith(t, map[string]string{"ENVOY_URL": "listener.example:9020"}, "ENVOY_URL")
		},
		"ENVOY_TOKEN": func(t *testing.T) {
			if boot := resolveWith(t, map[string]string{"ENVOY_TOKEN": "listener-token"}); boot.EnvoyToken != "listener-token" {
				t.Errorf("EnvoyToken = %q", boot.EnvoyToken)
			}
		},
		"DISPATCH_REPO_PROJECTS": func(t *testing.T) {
			if boot := resolveWith(t, map[string]string{"DISPATCH_REPO_PROJECTS": " acme/widgets=WID "}); boot.RepoProjects != "acme/widgets=WID" {
				t.Errorf("RepoProjects = %q", boot.RepoProjects)
			}
		},
		"DISPATCH_DEFAULT_PROJECT": func(t *testing.T) {
			if boot := resolveWith(t, map[string]string{"DISPATCH_DEFAULT_PROJECT": " WID "}); boot.DefaultProject != "WID" {
				t.Errorf("DefaultProject = %q", boot.DefaultProject)
			}
		},
		"DISPATCH_APP_CLIENT_ID": func(t *testing.T) {
			if clientID, source, err := loadApp(t, map[string]string{"DISPATCH_APP_CLIENT_ID": "Iv1.env", "DISPATCH_APP_CLIENT_SECRET": "secret"}); err != nil || clientID != "Iv1.env" || source != "env" {
				t.Errorf("set: client %q from %q, %v; want the variable over app.json", clientID, source, err)
			}
			if clientID, source, err := loadApp(t, nil); err != nil || clientID != "Iv1.file" || !strings.HasPrefix(source, "file:") {
				t.Errorf("unset: client %q from %q, %v; want app.json's", clientID, source, err)
			}
		},
		"DISPATCH_APP_CLIENT_SECRET": func(t *testing.T) {
			if _, _, err := loadApp(t, map[string]string{"DISPATCH_APP_CLIENT_ID": "Iv1.env"}); err == nil || !strings.Contains(err.Error(), "DISPATCH_APP_CLIENT_SECRET") {
				t.Errorf("client ID without a secret: err = %v, want a refusal naming DISPATCH_APP_CLIENT_SECRET", err)
			}
			app, _, err := loadAppCredentials(envGetter(map[string]string{"DISPATCH_APP_CLIENT_ID": "Iv1.env", "DISPATCH_APP_CLIENT_SECRET": "table-secret"}), t.TempDir())
			if err != nil || app.ClientSecret != "table-secret" {
				t.Errorf("app %+v, %v", app, err)
			}
		},
		"DISPATCH_APP_PEM_B64": func(t *testing.T) {
			app, _, err := loadAppCredentials(envGetter(map[string]string{"DISPATCH_APP_CLIENT_ID": "Iv1.env", "DISPATCH_APP_CLIENT_SECRET": "secret", "DISPATCH_APP_PEM_B64": base64.StdEncoding.EncodeToString([]byte("app private key"))}), t.TempDir())
			if err != nil || app.PEM != "app private key" {
				t.Errorf("app %+v, %v", app, err)
			}
			if _, _, err := loadApp(t, map[string]string{"DISPATCH_APP_CLIENT_ID": "Iv1.env", "DISPATCH_APP_CLIENT_SECRET": "secret", "DISPATCH_APP_PEM_B64": "not base64!"}); err == nil || !strings.Contains(err.Error(), "DISPATCH_APP_PEM_B64") {
				t.Errorf("malformed: err = %v, want a refusal naming DISPATCH_APP_PEM_B64", err)
			}
		},
		"DISPATCH_APP_ID": func(t *testing.T) {
			app, _, err := loadAppCredentials(envGetter(map[string]string{"DISPATCH_APP_CLIENT_ID": "Iv1.env", "DISPATCH_APP_CLIENT_SECRET": "secret", "DISPATCH_APP_ID": "4242"}), t.TempDir())
			if err != nil || app.ID != 4242 {
				t.Errorf("app %+v, %v", app, err)
			}
			if _, _, err := loadApp(t, map[string]string{"DISPATCH_APP_CLIENT_ID": "Iv1.env", "DISPATCH_APP_CLIENT_SECRET": "secret", "DISPATCH_APP_ID": "app"}); err == nil || !strings.Contains(err.Error(), "DISPATCH_APP_ID") {
				t.Errorf("not an integer: err = %v, want a refusal naming DISPATCH_APP_ID", err)
			}
		},
		"DISPATCH_APP_SLUG": func(t *testing.T) {
			app, _, err := loadAppCredentials(envGetter(map[string]string{"DISPATCH_APP_CLIENT_ID": "Iv1.env", "DISPATCH_APP_CLIENT_SECRET": "secret", "DISPATCH_APP_SLUG": "table-slug"}), t.TempDir())
			if err != nil || app.Slug != "table-slug" {
				t.Errorf("app %+v, %v", app, err)
			}
		},
		"DISPATCH_APP_NAME": func(t *testing.T) {
			app, _, err := loadAppCredentials(envGetter(map[string]string{"DISPATCH_APP_CLIENT_ID": "Iv1.env", "DISPATCH_APP_CLIENT_SECRET": "secret", "DISPATCH_APP_NAME": "Table App"}), t.TempDir())
			if err != nil || app.Name != "Table App" {
				t.Errorf("app %+v, %v", app, err)
			}
		},
		"DISPATCH_GITHUB_API_BASE": func(t *testing.T) {
			if boot := resolveWith(t, map[string]string{"DISPATCH_GITHUB_API_BASE": " http://127.0.0.1:9022 "}); boot.GitHubAPIBase != "http://127.0.0.1:9022" {
				t.Errorf("GitHubAPIBase = %q", boot.GitHubAPIBase)
			}
		},
		"DISPATCH_SIGNING_KEY": func(t *testing.T) {
			dataDir := t.TempDir()
			if key, err := sessionSigningKey(resolveWith(t, map[string]string{"DISPATCH_SIGNING_KEY": "table-key"}), dataDir); err != nil || key != "table-key" {
				t.Errorf("set: key %q, %v", key, err)
			}
			if _, err := os.Stat(filepath.Join(dataDir, "signing-key")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("set: the data dir's signing-key file was touched (%v)", err)
			}
			first, err := sessionSigningKey(resolveWith(t, nil), dataDir)
			if err != nil || first == "" {
				t.Fatalf("unset: key %q, %v", first, err)
			}
			if again, err := sessionSigningKey(resolveWith(t, nil), dataDir); err != nil || again != first {
				t.Errorf("unset again: key %q, %v; want the file's %q", again, err, first)
			}
			if _, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DISPATCH_SIGNING_KEY": "table-key"})); err == nil || !strings.Contains(err.Error(), "DISPATCH_SIGNING_KEY") {
				t.Errorf("dev sign-in with a key: err = %v, want a refusal naming DISPATCH_SIGNING_KEY", err)
			}
		},
		"DISPATCH_INSECURE_COOKIE": func(t *testing.T) {
			for value, insecure := range map[string]bool{"": false, "1": true, "false": true} {
				if boot := resolveWith(t, map[string]string{"DISPATCH_INSECURE_COOKIE": value}); boot.InsecureCookie != insecure {
					t.Errorf("DISPATCH_INSECURE_COOKIE=%q: InsecureCookie = %t, want %t", value, boot.InsecureCookie, insecure)
				}
			}
		},
		"DISPATCH_OIDC_ISSUER": func(t *testing.T) {
			boot := resolveWith(t, map[string]string{"DISPATCH_OIDC_ISSUER": "https://issuer.example", "DISPATCH_OIDC_AUDIENCE": "dispatch"})
			if boot.OIDCIssuer != "https://issuer.example" {
				t.Errorf("OIDCIssuer = %q", boot.OIDCIssuer)
			}
			refusedWith(t, map[string]string{"DISPATCH_OIDC_AUDIENCE": "dispatch"}, "DISPATCH_OIDC_ISSUER")
		},
		"DISPATCH_OIDC_AUDIENCE": func(t *testing.T) {
			boot := resolveWith(t, map[string]string{"DISPATCH_OIDC_ISSUER": "https://issuer.example", "DISPATCH_OIDC_AUDIENCE": "dispatch"})
			if boot.OIDCAudience != "dispatch" {
				t.Errorf("OIDCAudience = %q", boot.OIDCAudience)
			}
			refusedWith(t, map[string]string{"DISPATCH_OIDC_ISSUER": "https://issuer.example"}, "DISPATCH_OIDC_AUDIENCE")
		},
		"DISPATCH_AGENT_SECRETS_URL": func(t *testing.T) {
			if boot := resolveWith(t, map[string]string{"DISPATCH_AGENT_SECRETS_URL": "https://broker.example/", "DISPATCH_AGENT_SECRETS_TOKEN": "t"}); boot.AgentSecretsURL != "https://broker.example" {
				t.Errorf("AgentSecretsURL = %q", boot.AgentSecretsURL)
			}
			refusedWith(t, map[string]string{"DISPATCH_AGENT_SECRETS_URL": "https://broker.example/v1", "DISPATCH_AGENT_SECRETS_TOKEN": "t"}, "DISPATCH_AGENT_SECRETS_URL")
		},
		"DISPATCH_AGENT_SECRETS_TOKEN": func(t *testing.T) {
			broker := map[string]string{"DISPATCH_AGENT_SECRETS_URL": "https://broker.example"}
			refusedWith(t, broker, "DISPATCH_AGENT_SECRETS_TOKEN")
			broker["DISPATCH_AGENT_SECRETS_TOKEN"] = " bare-token "
			if boot := resolveWith(t, broker); boot.AgentSecretsToken != "bare-token" {
				t.Errorf("bare: AgentSecretsToken = %q", boot.AgentSecretsToken)
			}
			broker["DISPATCH_AGENT_SECRETS_TOKEN_FILE"] = writeFile(t, "token", "  file-token\n")
			if boot := resolveWith(t, broker); boot.AgentSecretsToken != "file-token" {
				t.Errorf("both: AgentSecretsToken = %q, want the file's", boot.AgentSecretsToken)
			}
			missing := filepath.Join(t.TempDir(), "missing")
			broker["DISPATCH_AGENT_SECRETS_TOKEN_FILE"] = missing
			refusedWith(t, broker, "DISPATCH_AGENT_SECRETS_TOKEN_FILE")
			// Without a broker URL the token is never read, so an unreadable file refuses nothing.
			if boot := resolveWith(t, map[string]string{"DISPATCH_AGENT_SECRETS_TOKEN_FILE": missing}); boot.AgentSecretsToken != "" {
				t.Errorf("no broker: AgentSecretsToken = %q", boot.AgentSecretsToken)
			}
		},
		"DISPATCH_WEB_DIST": func(t *testing.T) {
			if dir, err := defaultWebDistDir(resolveWith(t, map[string]string{"DISPATCH_WEB_DIST": "/srv/dispatch/dist"}).WebDist); err != nil || dir != "/srv/dispatch/dist" {
				t.Errorf("web dist = %q, %v", dir, err)
			}
		},
		"DISPATCH_DEV_SIGNIN": func(t *testing.T) {
			if boot, err := resolveBootConfig(devSignInEnvironment(nil)); err != nil || !boot.DevSignIn {
				t.Errorf("DISPATCH_DEV_SIGNIN=1: DevSignIn %t, %v", boot.DevSignIn, err)
			}
			refusedWith(t, map[string]string{"DISPATCH_DEV_SIGNIN": "yes"}, "DISPATCH_DEV_SIGNIN")
		},
		"DISPATCH_TEST_HOOKS": func(t *testing.T) {
			if boot := resolveWith(t, map[string]string{"DISPATCH_TEST_HOOKS": "1"}); !boot.TestHooksEnabled {
				t.Error("DISPATCH_TEST_HOOKS=1: hooks off")
			}
			if boot := resolveWith(t, map[string]string{"DISPATCH_TEST_HOOKS": "true"}); boot.TestHooksEnabled {
				t.Error("DISPATCH_TEST_HOOKS=true: hooks on, want only 1 to mount them")
			}
		},
	}
	for _, row := range settings {
		test, ok := cases[row.Name]
		if !ok {
			t.Errorf("%s has no case here: add one that hands it to its reader through the table", row.Name)
			continue
		}
		t.Run(row.Name, test)
		delete(cases, row.Name)
	}
	for name := range cases {
		t.Errorf("a case for %s, which the settings table does not list", name)
	}
}
