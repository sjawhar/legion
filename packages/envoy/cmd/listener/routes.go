package main

import (
	"io"
	"net/http"

	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/webhook"
)

// The paths main mounts before NATS is up. Both answer during startup too, and apiAuth lets them
// through without a credential (isAPIAuthExemptPath).
const (
	healthzPath = "/healthz"
	metricsPath = "/metrics"
)

// routeAuth names what a caller of a route presents. It describes the check; the handler, or
// apiAuth in front of /v1, stays the enforcement point.
type routeAuth string

const (
	// authNone needs no credential.
	authNone routeAuth = "none"
	// authSignature is the webhook provider's signature over the body, checked by the route's
	// handler against the secret its settings name.
	authSignature routeAuth = "signature"
	// authBearer is apiAuth's: ENVOY_API_TOKEN, or a service-account token for the OIDC pair, when
	// either is configured, and nothing when neither is.
	authBearer routeAuth = "bearer"
)

// routeOperation is one method and path a route answers, and what it does there.
type routeOperation struct {
	Method      string
	Path        string
	Description string
}

// v1Route is one pattern registerV1Routes mounts under /v1, every operation its handler answers,
// and the handler, built over the listener's dependencies. registerV1Routes mounts this table and
// `envoy-listener routes` prints it, so the two cannot disagree about what /v1 serves;
// TestEveryV1RouteAnswersExactlyItsMethods holds each handler to the methods its row names. Add a
// row here, never a mux.Handle line.
type v1Route struct {
	pattern    string
	operations []routeOperation
	handler    func(d *listenerDeps, machineID string, logger *logging.Logger) http.Handler
}

// v1Routes is every /v1 route, in the order a session meets them: register and subscribe, claim a
// role, find sessions, send.
var v1Routes = []v1Route{
	{
		pattern: "/v1/interests/subscribe",
		operations: []routeOperation{{http.MethodPost, "/v1/interests/subscribe",
			"Registers a session and the topics it wants. Body: `session_id` (required), `topics`, `dir`, `title`, `port`, `self_subscribed`, `capabilities` (the targeted-delivery modes it accepts: `aside`, `btw`, `steer`) and `driving`. The topics are added to those the session already holds, and its own `notifications.agent.<session_id>` is always among them. A body with a `port` or `self_subscribed: true` also refreshes the session's live entry, which lapses five minutes after its last refresh. Answers the session's interest, with `warnings` for a GitHub topic that spells its repository wrong or names one with no event in the stream."}},
		handler: func(d *listenerDeps, machineID string, logger *logging.Logger) http.Handler {
			return subscribeHandler(d, machineID, logger)
		},
	},
	{
		pattern: "/v1/interests/unsubscribe",
		operations: []routeOperation{{http.MethodPost, "/v1/interests/unsubscribe",
			"Removes topics from a session's interest. Body: `session_id` (required) and `topics`; a role topic among them gives up that role's claim. With no `topics` the whole interest goes, and every role the session holds. Answers `removed`, the topics it was given."}},
		handler: func(d *listenerDeps, _ string, logger *logging.Logger) http.Handler {
			return unsubscribeHandler(d, logger)
		},
	},
	{
		pattern: "/v1/interests/",
		operations: []routeOperation{
			{http.MethodGet, "/v1/interests/", "Lists every registered interest: each session's id, machine, directory, topics and `updated_at`."},
			{http.MethodGet, "/v1/interests/{session_id}", "One session's interest; `404` when it has none."},
			{http.MethodDelete, "/v1/interests/{session_id}", "Removes a session's interest and gives up every role it holds. Answers `204` whether or not it had one."},
		},
		handler: func(d *listenerDeps, _ string, _ *logging.Logger) http.Handler {
			return adminInterestsHandler(d.registry)
		},
	},
	{
		pattern: "/v1/roles/set",
		operations: []routeOperation{{http.MethodPost, "/v1/roles/set",
			"Claims a role for a registered session and adds `notifications.role.<role>` to its interest. Body: `session_id` and `role` (required; lowercase letters, digits, `_` and `-`, starting with a letter or digit), `soft` and `previous_session_id`. A claim takes the role from whoever holds it. A `soft` claim takes it only when the role is unheld, already this session's, held by a session that is no longer live, or held by `previous_session_id`, the session this one continues; any other holder answers `409` with `role` and `holder`. A session that is not registered answers `404`."}},
		handler: func(d *listenerDeps, machineID string, _ *logging.Logger) http.Handler {
			return roleSetHandler(d, machineID)
		},
	},
	{
		pattern: "/v1/roles/",
		operations: []routeOperation{{http.MethodGet, "/v1/roles/{role}",
			"The role's live holder: `role`, `holder`, `title`, `dir`, `machine_id`, `capabilities` and `last_seen`. A `404` carries `reason`: `unclaimed`, or `holder_lapsed` with the lapsed `holder`, `claim_released`, and its `last_seen` when known. A lookup that finds the holder lapsed releases its claim."}},
		handler: func(d *listenerDeps, _ string, _ *logging.Logger) http.Handler {
			return roleGetHandler(d)
		},
	},
	{
		pattern: "/v1/sessions",
		operations: []routeOperation{{http.MethodGet, "/v1/sessions",
			"Lists live sessions, each with its `machine_id`, `dir`, `port`, `title`, `capabilities`, `self_subscribed`, `topics`, the `roles` it holds and `last_seen`. The `dir` and `title` query parameters keep only the sessions whose value contains them."}},
		handler: func(d *listenerDeps, _ string, _ *logging.Logger) http.Handler {
			return sessionsHandler(d.registry, d.sessions)
		},
	},
	{
		pattern: "/v1/sessions/",
		operations: []routeOperation{{http.MethodDelete, "/v1/sessions/{session_id}",
			"Removes a session's live entry, as a session does when it shuts down. Its interest is left to the listener's reaper, which removes the interests of sessions that are no longer live."}},
		handler: func(d *listenerDeps, _ string, _ *logging.Logger) http.Handler {
			return deleteSessionHandler(d.sessions)
		},
	},
	{
		pattern: "/v1/registry/",
		operations: []routeOperation{{http.MethodGet, "/v1/registry/{session_id}",
			"A live session's entry: `port`, `machine_id`, `dir`, `title`, `capabilities` and `updated_at`; `404` when the session is not live."}},
		handler: func(d *listenerDeps, _ string, _ *logging.Logger) http.Handler {
			return registryHandler(d)
		},
	},
	{
		pattern: "/v1/messages/send",
		operations: []routeOperation{{http.MethodPost, "/v1/messages/send",
			"Sends a message to one live session on its `notifications.agent.<session_id>`. Body: `target_session` and `message` (required); `payload`; `source` (`agent` when omitted) and `source_session`; `idempotency_key`; and `in_reply_to`, `supersedes`, `urgency` (`low`, `med`, `high` or `blocking`), `expects_reply` (`none`, `optional` or `required`) and `expires_at` (Unix milliseconds). The message's first line is the envelope's summary, and its whole text the payload unless `payload` is given. A target that is not live answers `404`, and a targeted Dispatch frame whose delivery mode the target does not advertise `403`. Answers the published envelope, `recipient`, and `duplicate: true` when the stream already held this message."}},
		handler: func(d *listenerDeps, _ string, _ *logging.Logger) http.Handler {
			return sendHandler(d)
		},
	},
	{
		pattern: "/v1/messages/publish",
		operations: []routeOperation{{http.MethodPost, "/v1/messages/publish",
			"Publishes a message on any topic but a session's inbox, which takes `/v1/messages/send`. Body: `topic` and `message` (required), the optional fields `/v1/messages/send` takes, and `dedupe_key`, the envelope's dedupe key as given, which may not be combined with `idempotency_key`. A role topic needs a live holder: with none it answers `404` with the `reason`, and otherwise names the `holder` it went to. Answers the published envelope."}},
		handler: func(d *listenerDeps, _ string, _ *logging.Logger) http.Handler {
			return publishHandler(d)
		},
	},
}

// registerV1Routes mounts v1Routes over the listener's dependencies, which main hands over only
// once every store is open, so no handler can see a store that is not. Anything else under /v1 is
// a JSON 404, as every /v1 answer is JSON.
func registerV1Routes(v1 *http.ServeMux, d *listenerDeps, machineID string, logger *logging.Logger) {
	for _, route := range v1Routes {
		v1.Handle(route.pattern, route.handler(d, machineID, logger))
	}
	notFound := func(w http.ResponseWriter, _ *http.Request) {
		writeJSONError(w, http.StatusNotFound, "not found")
	}
	v1.HandleFunc("/v1", notFound)
	v1.HandleFunc("/v1/", notFound)
}

// operationalRoutes are the routes main mounts at healthzPath and metricsPath.
var operationalRoutes = []routeOperation{
	{http.MethodGet, healthzPath, "The listener's health, as JSON with a `status`. `200` `starting` until its durable consumer binds; `200` `healthy` once every dependency answers, with the consumer's `num_pending` and `num_ack_pending` and the session cache's `session_cache_ready`, `session_cache_size` and `session_watch_error`; `200` `degraded` while a key-value read fails in a way the listener is retrying; `503` `unhealthy` when NATS does not answer, the subscription or a cache's watcher has stopped, or the durable consumer is gone."},
	{http.MethodGet, metricsPath, "Prometheus metrics in the text format: messages received, delivery attempts, messages sent back for a retry, delivery duration, live sessions and registered interests, and, on a listener with the GitHub route, the CI records it held back."},
}

// allWebhookRoutes is every webhook route webhookRoutes can mount, for the printed table.
func allWebhookRoutes() []webhookRoute {
	return webhookRoutes(&webhook.WebhookConfig{
		GitHub:     &webhook.GitHubWebhook{},
		Slack:      &webhook.SlackWebhook{},
		GhostWispr: &webhook.GhostWisprWebhook{},
	})
}

// routeEntry is a route as `envoy-listener routes` prints it. When is null for a route every
// listener mounts.
type routeEntry struct {
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	Auth        routeAuth `json:"auth"`
	When        *string   `json:"when"`
	Description string    `json:"description"`
}

// writeRoutes writes every route the listener can serve as `envoy-listener routes` prints it:
// {"routes": [...]}, the operational routes first, then the webhooks, then /v1.
func writeRoutes(w io.Writer) error {
	var entries []routeEntry
	for _, operation := range operationalRoutes {
		entries = append(entries, routeEntry{operation.Method, operation.Path, authNone, nil, operation.Description})
	}
	for _, hook := range allWebhookRoutes() {
		when := "`ENVOY_WEBHOOKS` names `" + hook.provider + "`"
		entries = append(entries, routeEntry{http.MethodPost, hook.path, authSignature, &when, hook.description})
	}
	for _, route := range v1Routes {
		for _, operation := range route.operations {
			entries = append(entries, routeEntry{operation.Method, operation.Path, authBearer, nil, operation.Description})
		}
	}
	return writeJSONTable(w, "routes", entries)
}
