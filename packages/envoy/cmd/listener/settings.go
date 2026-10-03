package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/webhook"
)

// setting is one row of envoy-listener's settings table: an environment variable the listener
// reads. The table is the only way the listener's own code reads its environment: main looks every
// row up once (processSettings), and each reader (internal/config, internal/webhook, the bus's
// connect, and main itself) takes its value through settingValues, which refuses a name the table
// does not list. The libraries the listener links read a few variables for themselves, outside
// the table: `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` (net/http, for the OIDC discovery read),
// `SSL_CERT_FILE` and `SSL_CERT_DIR` (crypto/x509), and the Go runtime's `GO*` and `TZ`.
// `envoy-listener settings` prints the table, and the docs site's settings reference is generated
// from that output. A new setting is a new row here, never an os.Getenv;
// TestNoReaderBypassesTheSettingsTable holds cmd/listener, internal/config and internal/webhook to
// that.
type setting struct {
	// Name is the environment variable.
	Name string
	// Default is what the listener does when the variable is unset, in an operator's words, with
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

// settings is every listener setting, in the order an operator meets them: what the listener is
// called and where it listens, NATS, the /v1 credential, the webhook routes, and delivery.
var settings = []setting{
	{Name: "ENVOY_MACHINE_ID", Required: "yes",
		Description: "This listener's name. It is stamped on every session and interest the listener registers, and names its durable consumer `listener-<id>` and its role-lane queue group `envoy-listener-<id>`, each space written `-`, so a listener that replaces another under the same id takes over both."},
	{Name: "ENVOY_LISTEN_HOST", Default: "`127.0.0.1`", Required: "no",
		Description: "Host the HTTP server binds. A host that is not loopback needs a `/v1` credential: `ENVOY_API_TOKEN`, the OIDC pair, or `ENVOY_API_ALLOW_UNAUTHENTICATED`."},
	{Name: "PORT", Default: "`9020`", Required: "no",
		Description: "Port the HTTP server binds."},
	{Name: "NATS_URLS", Required: "yes",
		Description: "Comma-separated NATS server URLs. The listener owns the `ENVOY_NOTIFICATIONS` stream there: every start creates it or reconciles its subjects and retention."},
	{Name: "ENVOY_ALLOW_REMOTE_NATS", Required: "no",
		Description: "Set to `1` to let the listener reach a NATS server that is not on this machine; without it, a `NATS_URLS` naming another host refuses startup, naming the URL."},
	{Name: "NATS_NKEY_SEED", File: true, Required: "no",
		Description: "Nkey seed of the NATS user the listener connects as; unset, it connects with no credential. A set but unusable seed refuses startup, naming the variable."},
	{Name: "ENVOY_NATS_REPLICAS", Default: "`1`", Required: "no",
		Description: "Replicas of the stream and of each key-value bucket the listener creates; set it to the size of a clustered NATS."},
	{Name: "ENVOY_API_TOKEN", Required: "when `ENVOY_LISTEN_HOST` is not loopback and neither the OIDC pair nor `ENVOY_API_ALLOW_UNAUTHENTICATED` is set",
		Description: "Bearer token a `/v1` caller must send as `Authorization: Bearer <token>`. With no token and no OIDC pair, `/v1` answers every caller."},
	{Name: "ENVOY_OIDC_ISSUER", Required: "when `ENVOY_OIDC_AUDIENCE` is set",
		Description: "Issuer of the Kubernetes service-account tokens `/v1` also accepts. The listener reads the issuer's discovery document at startup and refuses to start when it does not answer."},
	{Name: "ENVOY_OIDC_AUDIENCE", Required: "when `ENVOY_OIDC_ISSUER` is set",
		Description: "Audience a service-account token must carry for `/v1` to accept it."},
	{Name: "ENVOY_API_ALLOW_UNAUTHENTICATED", Required: "no",
		Description: "Set to `1` to start on a host that is not loopback with neither `ENVOY_API_TOKEN` nor the OIDC pair, which leaves `/v1` open to anyone who can reach it."},
	{Name: "ENVOY_WEBHOOKS", Default: "no webhook route", Required: "no",
		Description: "Comma-separated webhook routes to mount: `github`, `slack`, `ghostwispr`. A name outside those refuses startup. Without `github` the listener records no check run and publishes no `pr.<n>.checks` settlement."},
	{Name: "ENVOY_GITHUB_WEBHOOK_SECRET", Required: "when `ENVOY_WEBHOOKS` names `github`",
		Description: "The GitHub webhook's secret. `/webhook/github` answers `401` to a delivery whose `X-Hub-Signature-256` it did not sign."},
	{Name: "ENVOY_REVIEWER_APP_ID", Required: "when `ENVOY_WEBHOOKS` names `github`",
		Description: "Id of the GitHub App whose `tester` and `architect` check runs count as verdicts; a check run of either name from any other App is ignored."},
	{Name: "ENVOY_GITHUB_MENTION_TRIGGER", Default: "`@legion`", Required: "no",
		Description: "Text that makes a comment or review a mention: such a comment also publishes a `.mention` event. Matched case-insensitively, and not inside a longer word or an email address."},
	{Name: "ENVOY_SLACK_SIGNING_SECRET", Required: "when `ENVOY_WEBHOOKS` names `slack`",
		Description: "The Slack app's signing secret, which `/webhook/slack` checks every request against."},
	{Name: "ENVOY_GHOSTWISPR_SIGNING_SECRET", Default: "no signature check", Required: "no",
		Description: "Ghost Wispr's signing secret, which `/webhook/ghostwispr` checks every request against."},
	{Name: "ENVOY_CI_DEBOUNCE", Default: "`5s`", Required: "no",
		Description: "How long a commit's checks must stay quiet after their last event before the listener publishes their `pr.<n>.checks` settlement, as a Go duration such as `10s`. A value that does not parse is logged and the default used."},
	{Name: "ENVOY_HOST_BRIDGE", Default: "`host.docker.internal`", Required: "no",
		Description: "Host the listener posts to for a session on its own machine that registered a port, such as an OpenCode server's `prompt_async`. A session another listener registered is reached at that listener's machine id as a host name."},
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

// processSettings reads the table from the process environment. It is the one place the
// listener's own code reads its environment.
func processSettings() settingValues {
	return readSettings(os.LookupEnv)
}

// lookup answers as os.LookupEnv does, for a variable the table lists. Any other name panics: a
// reader of a variable missing from the table is a bug the settings tests catch, never a value.
func (values settingValues) lookup(name string) (string, bool) {
	value, ok := values[name]
	if !ok {
		panic(fmt.Sprintf("envoy-listener reads %s, which its settings table (cmd/listener/settings.go) does not list", name))
	}
	return value.value, value.set
}

// get answers as os.Getenv does, for a variable the table lists.
func (values settingValues) get(name string) string {
	value, _ := values.lookup(name)
	return value
}

// settingEntry is a row as `envoy-listener settings` prints it. Default and File are null when the
// row has none.
type settingEntry struct {
	Name        string  `json:"name"`
	File        *string `json:"file"`
	Default     *string `json:"default"`
	Required    string  `json:"required"`
	Description string  `json:"description"`
}

// writeSettings writes the table as `envoy-listener settings` prints it: {"settings": [...]}, in
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
	return writeJSONTable(w, "settings", entries)
}

// listenerSettings is everything main reads from the settings table before it binds: all but the
// NATS credential and reach, which the bus reads at its connect through the table's lookup
// (bus.WithEnvironment).
type listenerSettings struct {
	service              config.Service
	apiToken             string
	allowUnauthenticated string
	oidcIssuer           string
	oidcAudience         string
	webhooks             *webhook.WebhookConfig
	hostBridge           string
	ciDebounce           string
}

// readListenerSettings reads main's settings through env, refusing what config.Load,
// resolveListenerOIDCConfig and webhook.LoadWebhookConfig refuse.
func readListenerSettings(env settingValues) (listenerSettings, error) {
	service, err := config.Load(env.get, 9020)
	if err != nil {
		return listenerSettings{}, err
	}
	issuer, audience, err := resolveListenerOIDCConfig(env.get)
	if err != nil {
		return listenerSettings{}, err
	}
	hooks, err := webhook.LoadWebhookConfig(env.get)
	if err != nil {
		return listenerSettings{}, err
	}
	return listenerSettings{
		service:              service,
		apiToken:             env.get("ENVOY_API_TOKEN"),
		allowUnauthenticated: env.get("ENVOY_API_ALLOW_UNAUTHENTICATED"),
		oidcIssuer:           issuer,
		oidcAudience:         audience,
		webhooks:             hooks,
		hostBridge:           env.get("ENVOY_HOST_BRIDGE"),
		ciDebounce:           env.get("ENVOY_CI_DEBOUNCE"),
	}, nil
}

// writeJSONTable writes rows under key, indented and with HTML left unescaped, as both tables the
// listener prints are written.
func writeJSONTable(w io.Writer, key string, rows any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(map[string]any{key: rows})
}
