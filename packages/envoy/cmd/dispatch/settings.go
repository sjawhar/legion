package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// setting is one row of envoy-dispatch's settings table: a Dispatch setting, an environment
// variable the server or one of its subcommands reads. The table is the only way Dispatch's own
// code reads its environment: main looks every row up once (processSettings), and each reader
// takes its value from that read through settingValues, which refuses a name the table does not
// list. The libraries Dispatch links read a few variables for themselves, outside the table:
// `HOME` (os.UserHomeDir), libpq's `PG*` (pgx), `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`
// (net/http), `SSL_CERT_FILE` and `SSL_CERT_DIR` (crypto/x509), and the Go runtime's `GO*` and
// `TZ`. `envoy-dispatch settings` prints the table, and the docs site's configuration reference is
// generated from that output. A new setting is a new row here, never an os.Getenv;
// TestNoReaderBypassesTheSettingsTable holds cmd/dispatch and internal/dispatch to that.
type setting struct {
	// Name is the environment variable.
	Name string
	// Default is what the server does when the variable is unset, in an operator's words, with
	// backticks around a literal value; empty when there is nothing in its place.
	Default string
	// File says the setting has a _FILE form: Name+"_FILE" names a file whose trimmed contents are
	// the value and win over Name.
	File bool
	// Required is "yes", "no", or "when " followed by the condition that makes it required.
	Required string
	// Description is one line saying what the setting does.
	Description string
}

// settings is every Dispatch setting, in the order an operator meets them: storage, identity,
// where the server listens, NATS, the Envoy listener, projects, the GitHub App, cookies,
// service-account tokens, the secrets broker, the dashboard, and local and test runs.
var settings = []setting{
	{Name: "DATABASE_URL", Required: "yes",
		Description: "Postgres connection string. The server migrates the database before serving; `census`, `backfill-block-ids`, `backfill-anchor-blocks` and `rebuild-refs` read it too. A `pool_max_conns` parameter is refused."},
	{Name: "DISPATCH_AGENT_TOKEN", Required: "yes",
		Description: "Shared bearer token an agent may authenticate with; personal tokens minted in Settings are the usual agent credential."},
	{Name: "DISPATCH_IDENTITY", Default: "`cookie`", Required: "no",
		Description: "How a browser request names its human: `cookie` (GitHub sign-in and a signed session cookie) or `header:<Header-Name>` (a trusted proxy's header)."},
	{Name: "DISPATCH_ALLOWED_LOGINS", Required: "when `DISPATCH_IDENTITY` is `cookie`",
		Description: "Comma-separated GitHub logins Dispatch lets in, compared case-insensitively; both identity modes refuse any other login."},
	{Name: "DISPATCH_IDENTITY_HEADER_TRUSTED", Required: "when `DISPATCH_IDENTITY` is a header and `DISPATCH_APP_CLIENT_ID` is set",
		Description: "Set to `1` to confirm the identity header comes from a trusted proxy while GitHub sign-in is configured too."},
	{Name: "DISPATCH_LISTEN_HOST", Default: "every interface", Required: "no",
		Description: "Host the server binds; an IPv6 host may be written with or without brackets."},
	{Name: "DISPATCH_PORT", Default: "`" + defaultListenPort + "`", Required: "no",
		Description: "Port the server binds."},
	{Name: "DISPATCH_SERVER_URL", Default: "`dispatch.serverUrl` from `envoy.json`", Required: "no",
		Description: "The origin people open in a browser and the GitHub OAuth callback origin: an absolute http(s) URL with no path. Set, it overrides `envoy.json`; set but empty, it refuses startup."},
	{Name: "NATS_URLS", Default: "`natsUrls` from `envoy.json`", Required: "no",
		Description: "Comma-separated NATS server URLs. Set, it overrides `envoy.json`; set but empty, it refuses startup."},
	{Name: "ENVOY_ALLOW_REMOTE_NATS", Required: "no",
		Description: "Set to `1` to let Dispatch reach a NATS server that is not on this machine; without it Dispatch refuses one, and every deployment sets it."},
	{Name: "NATS_NKEY_SEED", File: true, Required: "no",
		Description: "Nkey seed of the NATS user Dispatch connects as; unset, it connects with no credential. A set but unusable seed refuses startup."},
	{Name: "DISPATCH_NATS_DISABLED", Required: "no",
		Description: "Set to `1` to run without NATS: no event is published, and the agent conversation relay and webhook redelivery are off."},
	{Name: "ENVOY_URL", Default: "`" + defaultEnvoyURL + "`", Required: "no",
		Description: "Base URL of the Envoy listener Dispatch delivers mentions and messages through and reads live sessions from."},
	{Name: "ENVOY_TOKEN", Required: "no",
		Description: "Bearer token Dispatch sends on every Envoy listener call; unset, it sends none."},
	{Name: "DISPATCH_REPO_PROJECTS", Required: "no",
		Description: "Comma-separated `owner/repo=KEY` mappings seeded at startup; a mapping saved in the dashboard wins."},
	{Name: "DISPATCH_DEFAULT_PROJECT", Required: "no",
		Description: "Project an external issue from an unmapped repository is created in; it must already exist."},
	{Name: "DISPATCH_APP_CLIENT_ID", Required: "no",
		Description: "GitHub App OAuth client ID. Set, the App comes from the `DISPATCH_APP_*` variables instead of `~/.local/share/dispatch/app.json`."},
	{Name: "DISPATCH_APP_CLIENT_SECRET", Required: "when `DISPATCH_APP_CLIENT_ID` is set",
		Description: "GitHub App OAuth client secret."},
	{Name: "DISPATCH_APP_PEM_B64", Required: "no",
		Description: "Base64-encoded GitHub App private key, which signs every App call: the architecture import and webhook redelivery are off without one."},
	{Name: "DISPATCH_APP_ID", Required: "no",
		Description: "The GitHub App's numeric ID."},
	{Name: "DISPATCH_APP_SLUG", Required: "no",
		Description: "The GitHub App's slug."},
	{Name: "DISPATCH_APP_NAME", Required: "no",
		Description: "The GitHub App's display name."},
	{Name: "DISPATCH_GITHUB_API_BASE", Default: "`https://api.github.com`", Required: "no",
		Description: "GitHub API origin the App calls; tests point it at a fake. `redeliver-webhooks` reads it too."},
	{Name: "DISPATCH_SIGNING_KEY", Default: "a key kept in `~/.local/share/dispatch/signing-key`, created on first start", Required: "no",
		Description: "HMAC key that signs session cookies; changing it signs everyone out."},
	{Name: "DISPATCH_INSECURE_COOKIE", Required: "no",
		Description: "Any value drops the `Secure` attribute from Dispatch's cookies, for browsers that reach it over plain http."},
	{Name: "DISPATCH_OIDC_ISSUER", Required: "when `DISPATCH_OIDC_AUDIENCE` is set",
		Description: "OIDC issuer whose projected service-account tokens authenticate as agents."},
	{Name: "DISPATCH_OIDC_AUDIENCE", Required: "when `DISPATCH_OIDC_ISSUER` is set",
		Description: "Audience those service-account tokens must carry."},
	{Name: "DISPATCH_AGENT_SECRETS_URL", Required: "no",
		Description: "Base URL of the secrets broker's API, an absolute http(s) URL with no path; set, the credential-request pages are on."},
	{Name: "DISPATCH_AGENT_SECRETS_TOKEN", File: true, Required: "when `DISPATCH_AGENT_SECRETS_URL` is set",
		Description: "Bearer token Dispatch sends the secrets broker."},
	{Name: "DISPATCH_WEB_DIST", Default: "the `packages/dispatch/web/dist` directory found from the binary's location", Required: "no",
		Description: "Directory of the built dashboard the server serves."},
	{Name: "DISPATCH_DEV_SIGNIN", Required: "no",
		Description: "Set to `1` on a loopback-only local server to sign any allowed login in at `/auth/_dev/signin` without GitHub."},
	{Name: "DISPATCH_TEST_HOOKS", Required: "no",
		Description: "Set to `1` to mount the end-to-end tests' hook routes; never in a real deployment."},
}

// fileVariable is the _FILE form of a setting that has one.
func (s setting) fileVariable() string {
	return s.Name + "_FILE"
}

// settingValues is the environment as the table read it: each row's variable, and the _FILE form
// of a row that has one, with whether it was set.
type settingValues map[string]settingValue

type settingValue struct {
	value string
	set   bool
}

// readSettings looks up every variable the table lists, once.
func readSettings(lookup func(string) (string, bool)) settingValues {
	values := make(settingValues, len(settings))
	read := func(name string) {
		value, set := lookup(name)
		values[name] = settingValue{value: value, set: set}
	}
	for _, row := range settings {
		read(row.Name)
		if row.File {
			read(row.fileVariable())
		}
	}
	return values
}

// processSettings reads the table from the process environment. It is the one place Dispatch's own
// code reads its environment.
func processSettings() settingValues {
	return readSettings(os.LookupEnv)
}

// lookup answers as os.LookupEnv does, for a variable the table lists. Any other name panics: a
// reader of a variable missing from the table is a bug the settings tests catch, never a value.
func (values settingValues) lookup(name string) (string, bool) {
	value, ok := values[name]
	if !ok {
		panic(fmt.Sprintf("envoy-dispatch reads %s, which its settings table (cmd/dispatch/settings.go) does not list", name))
	}
	return value.value, value.set
}

// get answers as os.Getenv does, for a variable the table lists.
func (values settingValues) get(name string) string {
	value, _ := values.lookup(name)
	return value
}

// settingEntry is a row as `envoy-dispatch settings` prints it. Default and File are null when the
// row has none.
type settingEntry struct {
	Name        string  `json:"name"`
	File        *string `json:"file"`
	Default     *string `json:"default"`
	Required    string  `json:"required"`
	Description string  `json:"description"`
}

// writeSettings writes the table as `envoy-dispatch settings` prints it: {"settings": [...]}, in
// table order.
func writeSettings(w io.Writer) error {
	entries := make([]settingEntry, len(settings))
	for index, row := range settings {
		entry := settingEntry{Name: row.Name, Required: row.Required, Description: row.Description}
		if row.File {
			file := row.fileVariable()
			entry.File = &file
		}
		if row.Default != "" {
			value := row.Default
			entry.Default = &value
		}
		entries[index] = entry
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(map[string]any{"settings": entries})
}
