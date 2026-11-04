// settings_legion.go serves the Legion view's own settings and the one call Dispatch relays to a
// Legion daemon (dispatch://INFRA-1077).
//
// A project's setting names the daemon Dispatch reads that project's state from
// (internal/dispatch/legion). Writing one belongs to the deployment rather than to the project it
// names — it points Dispatch at a host, which a credential that may write one project's issues
// has no business doing — so the PUT and the DELETE are classed scopeDeployment beside the
// repository-mapping rows, not scopeProject (routes_table.go, writeClasses).
//
// Unlike putRepoProject, and like putDeliverySettings, these write no event. The write belongs to
// the deployment, so there is no project whose event it would be, and events/broker.go's
// eventOwnerKey refuses an ownerless event; the row's own created_by and updated_at are the
// record of who set it and when.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// listLegionSettings answers GET /api/v1/settings/legion with every project's Legion setting, by
// project key; a deployment with none answers an empty list.
func (s *server) listLegionSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	settings, err := legion.ListSettings(r.Context(), s.deps.Store.Pool)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, settings)
}

// putLegionSetting answers PUT /api/v1/settings/legion/{project}: name the daemon that project's
// state is read from. An unknown project is 404 NOT_FOUND, and a state_url that is not an
// absolute http(s) URL with a host and no path is 400 LEGION_SETTING_INPUT.
func (s *server) putLegionSetting(w http.ResponseWriter, r *http.Request) {
	var input struct {
		StateURL string       `json:"state_url"`
		Actor    *model.Actor `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	stateURL, refusal := legionStateURL(input.StateURL)
	if refusal != "" {
		writeError(w, "LEGION_SETTING_INPUT", http.StatusBadRequest, refusal)
		return
	}
	project, ok := s.legionProject(w, r)
	if !ok {
		return
	}
	settings, err := legion.PutSettings(r.Context(), s.deps.Store.Pool, project, stateURL, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// An open Legion board of this project was told LEGION_NOT_CONFIGURED and otherwise waits for
	// the sampler's next read to hear that it now has a daemon. The write announces itself on the
	// same channel a read does, so the board appears as soon as the URL is set.
	if err := legion.NotifyRead(r.Context(), s.deps.Store.Pool, project); err != nil {
		slog.Warn("dispatch: announce a Legion setting write", "project", project, "error", err)
	}
	WriteJSON(w, http.StatusOK, settings)
}

// deleteLegionSetting answers DELETE /api/v1/settings/legion/{project}: stop reading that
// project's daemon. What was already read stays; nothing re-reads it. A project with no setting
// is 404 NOT_FOUND, as an unknown one is.
func (s *server) deleteLegionSetting(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireActor(w, r, nil); !ok {
		return
	}
	project, ok := s.legionProject(w, r)
	if !ok {
		return
	}
	deleted, err := legion.DeleteSettings(r.Context(), s.deps.Store.Pool, project)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !deleted {
		writeError(w, "NOT_FOUND", http.StatusNotFound, "this project has no Legion daemon configured")
		return
	}
	if err := legion.NotifyRead(r.Context(), s.deps.Store.Pool, project); err != nil {
		slog.Warn("dispatch: announce a Legion setting removal", "project", project, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// putLegionProjectSettings answers PUT /api/v1/legion/{project}/settings: relay an operator's
// legion.yaml change to that project's daemon and answer what the daemon said.
//
// The body is {changes: {<dotted legion.yaml path>: <value>}}. Dispatch sends those changes and
// the caller's own login to PUT {state_url}/legion/v1/operator/settings, presenting the projected
// ServiceAccount token DISPATCH_LEGION_DAEMON_TOKEN_FILE names where the deployment names one,
// and passes the daemon's status and body back unchanged: the daemon decides each key — applied
// at-once, rebuilt or next-launch, or refused with a reason — and Dispatch re-reads no verdict of
// it. With no token file the relay sends no bearer, and whatever the daemon answers is what the
// caller sees.
func (s *server) putLegionProjectSettings(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Changes map[string]json.RawMessage `json:"changes"`
		Actor   *model.Actor               `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	if len(input.Changes) == 0 {
		writeError(w, "LEGION_SETTINGS_INPUT", http.StatusBadRequest, "changes must name at least one legion.yaml setting")
		return
	}
	project, ok := s.legionProject(w, r)
	if !ok {
		return
	}
	settings, err := legion.GetSettings(r.Context(), s.deps.Store.Pool, project)
	if err != nil {
		if errors.Is(err, legion.ErrNoSettings) {
			writeError(w, legionNotConfiguredCode, http.StatusNotFound, legionNotConfiguredMessage)
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	client := legion.New(settings.StateURL, s.deps.LegionDaemonTokenFile)
	answer, err := client.PutOperatorSettings(r.Context(), legion.SettingsChange{
		Changes: input.Changes,
		By:      legionAuthor(actor),
	})
	if err != nil {
		var tokenErr *brokertoken.AccountTokenError
		if errors.As(err, &tokenErr) {
			slog.Error("dispatch: cannot read its ServiceAccount token for the Legion daemon", "path", tokenErr.Path, "error", tokenErr.Err)
			writeError(w, "LEGION_DAEMON_TOKEN_UNREADABLE", http.StatusInternalServerError,
				"Dispatch cannot read its own ServiceAccount token for the Legion daemon")
			return
		}
		slog.Warn("dispatch: relay a Legion settings change", "project", project, "error", err)
		writeError(w, "LEGION_DAEMON_UNAVAILABLE", http.StatusServiceUnavailable, "the Legion daemon could not be reached")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(answer.Status)
	_, _ = w.Write(answer.Body)
}

// legionProject is the {project} of the path, once it is a project key and a project of that key
// exists; an unknown one is the 404 NOT_FOUND every unknown row is.
func (s *server) legionProject(w http.ResponseWriter, r *http.Request) (string, bool) {
	return s.legionProjectNamed(w, r, strings.TrimSpace(r.PathValue("project")), "LEGION_SETTING_INPUT")
}

// legionProjectNamed is project once it is a project key and a project of that key exists. code
// is the refusal a badly spelled key carries, which the two routes that ask differ on: the
// metrics route's `INVALID_QUERY` for a query parameter, and the settings route's
// `LEGION_SETTING_INPUT` for a path segment. An unknown project is the 404 NOT_FOUND every
// unknown row is.
func (s *server) legionProjectNamed(w http.ResponseWriter, r *http.Request, project, code string) (string, bool) {
	if !projectKeyPattern.MatchString(project) {
		writeError(w, code, http.StatusBadRequest, projectKeyProblem("project", project))
		return "", false
	}
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `select key from projects where key = $1`, project).Scan(new(string)); err != nil {
		s.writeHandlerError(w, err)
		return "", false
	}
	return project, true
}

// legionStateURL is the daemon base URL a setting may hold, trimmed, or the refusal that names
// what is wrong with it. The rule is the one Dispatch holds DISPATCH_AGENT_SECRETS_URL to: an
// absolute http(s) URL with a host and no path, carrying no user, password, query or fragment,
// since every call this server makes to the daemon is that base with a fixed path after it.
func legionStateURL(raw string) (string, string) {
	trimmed := strings.TrimSuffix(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return "", "state_url is required"
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Path != "" {
		return "", "state_url must be an absolute http(s) URL with a host and no path"
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", "state_url must carry no user, password, query or fragment"
	}
	return trimmed, ""
}

// legionAuthor is the person a relayed change is recorded under: the caller's own login where the
// caller is a person, the human an agent token acts for where it has one, and the session's own
// id where no person stands behind it, so the daemon always records a caller rather than nobody.
func legionAuthor(actor model.Actor) string {
	if actor.Kind == "user" {
		return model.CanonicalLogin(actor.ID)
	}
	if actor.Owner != nil && strings.TrimSpace(*actor.Owner) != "" {
		return model.CanonicalLogin(*actor.Owner)
	}
	return actor.ID
}
