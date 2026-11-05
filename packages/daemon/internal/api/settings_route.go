package api

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/oidc"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// SettingsChangeRequest is the body of `PUT /legion/v1/operator/settings`: the dotted
// `legion.yaml` paths this caller is changing with the value each takes, and the person who asked
// for it, which the daemon records as the change's author and its log line names.
//
// A path mapped to `null` is withdrawn: the route change the daemon kept for it is over and the
// file's own value takes the key back.
type SettingsChangeRequest struct {
	Changes map[string]json.RawMessage `json:"changes"`
	By      string                     `json:"by,omitempty"`
}

// SettingsChangeResponse is what the route answers: one result per path the request named, keyed
// as the request keyed it.
type SettingsChangeResponse struct {
	Settings map[string]SettingResult `json:"settings"`
}

// SettingResult is one path's verdict. Exactly one of Applied and Refused is set.
//
// Applied is when the change takes effect — `at-once` for a value every decision reads,
// `rebuilt` for one a client or listener holds, which the daemon has already built again, and
// `next-launch` for one a pod's or a pane's launch reads (config.Outcome) — and Holder names
// what was built again, absent when nothing holds the value. Old and New are the values as the
// daemon's log line shows them, `(withheld)` for a credential and `(unset)` for a path the
// configuration did not carry. Unchanged says the daemon was already running on that value: the
// path is the route's from now on, and nothing moved.
//
// Refused is why the daemon will not change the path: a path fixed for the process is refused
// with the reason its row gives, and a path the class table does not hold is an unknown setting
// (config.Refusal).
type SettingResult struct {
	Applied   string `json:"applied,omitempty"`
	Refused   string `json:"refused,omitempty"`
	Holder    string `json:"holder,omitempty"`
	Old       string `json:"old,omitempty"`
	New       string `json:"new,omitempty"`
	Unchanged bool   `json:"unchanged,omitempty"`
}

// SettingsVerifier verifies the projected ServiceAccount token a caller presents instead of the
// operator bearer, against the cluster issuer and audience the deployment named
// (`settings_route`). *oidc.LazyVerifier is one, and is what the daemon builds: it reads the
// issuer's discovery document at the first token it verifies, so a daemon boots and a rebuild
// settles with no network read at all.
type SettingsVerifier interface {
	Verify(ctx context.Context, raw string) (oidc.Claims, error)
}

// SettingsRouteAuth is the settings route's projected-token arm as the API server holds it: the
// verifier of the deployment's cluster issuer and audience, and the ServiceAccounts it admits.
// The zero value admits nobody, which leaves the route to the operator bearer alone.
type SettingsRouteAuth struct {
	Verifier        SettingsVerifier
	ServiceAccounts []string
}

// configured is whether this deployment named a projected-token caller at all.
func (a SettingsRouteAuth) configured() bool {
	return a.Verifier != nil && len(a.ServiceAccounts) > 0
}

// ApplySettings is how the route reaches the daemon's one settings holder (internal/settings'
// Keeper.Route): it applies the change, records it so it survives the daemon's next restart, and
// answers what it applied and what it refused. caller is the arm the request came in on, which
// decides what it may set: the operator bearer sets every path the loader takes, and a projected
// ServiceAccount token only a path whose value is plain data (config.Caller). Nil is a daemon
// that changes no setting, and the route answers 503.
type ApplySettings func(ctx context.Context, caller config.Caller, by string, changes map[string]any) ([]config.Change, []config.Refusal, error)

// invalidSettingsCaller is the settings route's one refusal of a caller it does not admit: the
// operator bearer, or a ServiceAccount token this deployment named, and nothing else. It never
// says which of the two failed or why — that is the daemon's log, not the caller's business.
const invalidSettingsCaller = "Invalid operator token or ServiceAccount token"

// settingsChange is `PUT /legion/v1/operator/settings`: the second way a setting changes, beside
// the configuration file under the daemon, and the same way — both arms call the one settings
// holder, which resolves the whole file under the change (internal/settings).
func (s *server) settingsChange(w http.ResponseWriter, r *http.Request) {
	identity, caller, authorized := s.settingsCaller(r)
	if !authorized {
		writeJSON(w, http.StatusForbidden, errorBody(invalidSettingsCaller))
		return
	}
	var req SettingsChangeRequest
	if !s.readBody(w, r, &req, nil) {
		return
	}
	if len(req.Changes) == 0 {
		writeJSON(w, http.StatusBadRequest, errorBody("changes is required: name each dotted legion.yaml path with the value it takes"))
		return
	}
	by := authoredBy(caller, identity, req.By)
	changes := make(map[string]any, len(req.Changes))
	for path, raw := range req.Changes {
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("changes."+path+" is not a value: "+err.Error()))
			return
		}
		changes[path] = value
	}
	if s.applySettings == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorBody("this daemon holds no settings it can change"))
		return
	}
	applied, refused, err := s.applySettings(r.Context(), caller, by, changes)
	var notRecorded *config.NotRecorded
	switch {
	case errors.As(err, &notRecorded):
		// The daemon applied the change and could not keep it: it is running on the new value,
		// and its next restart reads the file's own value for those paths. Saying the change was
		// refused would be false, so the answer names each key's verdict as it stands and the
		// status says the daemon did not finish the work the caller asked for.
		s.log.Error("api: a settings change was applied and not recorded; the daemon runs on it and its next restart will not",
			"by", by, "paths", slices.Sorted(maps.Keys(changes)), "error", err)
		writeJSON(w, http.StatusInternalServerError, SettingsChangeResponse{Settings: settingResults(changes, applied, refused)})
		return
	case err != nil:
		// The loader refused the configuration the change makes — a value of the wrong shape, a
		// key whose neighbours now contradict it — or the file could not be read. Either way the
		// daemon changed nothing and keeps running on what it had.
		s.log.Warn("api: a settings change did not settle; the daemon keeps the settings it has",
			"by", by, "paths", slices.Sorted(maps.Keys(changes)), "error", err)
		writeJSON(w, http.StatusBadRequest, errorBody("the configuration does not resolve under this change: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, SettingsChangeResponse{Settings: settingResults(changes, applied, refused)})
}

// authoredBy is the change's author as the daemon records it and its log line names: the person
// the body named, and on the projected arm the ServiceAccount of the token that presented it
// beside them.
//
// Nothing verifies `by`. The operator bearer is the deployment's own secret, so what it names
// stands; a projected ServiceAccount token is held by a whole service — Dispatch's path, whose
// caller is any deployment-scoped Dispatch user — and a copy of it captured anywhere could
// otherwise write any person's name into the daemon's record of who changed a setting. The
// verified identity is therefore kept beside the named one, and the one the body chose is never
// the whole answer.
func authoredBy(caller config.Caller, identity, named string) string {
	switch {
	case named == "":
		return identity
	case caller == config.CallerProjected:
		return named + " (" + identity + ")"
	default:
		return named
	}
}

// settingResults is the answer the caller reads: one result per path it named, whether that path
// changed a value, changed nothing, or was refused.
func settingResults(changes map[string]any, applied []config.Change, refused []config.Refusal) map[string]SettingResult {
	results := make(map[string]SettingResult, len(changes))
	for path := range changes {
		// A path the class table does not hold, and one fixed for the process, come back as
		// refusals below; every other named path is the route's from now on, whether or not its
		// own value moved.
		if row, known := config.ClassOf(path); known && row.Class != config.ClassFixed {
			results[path] = SettingResult{Applied: string(row.Class.Outcome()), Holder: row.Holder, Unchanged: true}
		}
	}
	// A change the caller did not name is one its own change moved: a key of a block it set
	// whole, or a key whose default another key decides. It is answered for too, since it is
	// part of what this call did.
	for _, change := range applied {
		results[change.Path] = SettingResult{
			Applied: string(change.Outcome), Holder: change.Holder, Old: change.Old, New: change.New,
		}
	}
	for _, refusal := range refused {
		results[refusal.Path] = SettingResult{Refused: refusal.Reason}
	}
	return results
}

// settingsCaller is who this request is, which arm it came in on, and whether the daemon admits
// it at all: the operator bearer (api/operator.go's own comparison), or a ServiceAccount token
// projected with this daemon as its audience, signed by the cluster issuer the deployment named
// and naming a ServiceAccount it named (`settings_route`). A call with no bearer is nobody.
//
// The identity is the caller's own — "operator", or the token's ServiceAccount — which the
// change's author defaults to when the body names no person. The arm is what that caller may
// set: the operator bearer every path the loader takes, and a projected token only a path whose
// value is plain data, whoever the body names as the person (config.Caller). A Dispatch caller
// with deployment scope, a person or an agent, reaches the daemon on the projected arm.
func (s *server) settingsCaller(r *http.Request) (string, config.Caller, bool) {
	presented, carried := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !carried || presented == "" {
		return "", "", false
	}
	if s.operatorBearer(presented, carried) {
		return "operator", config.CallerOperator, true
	}
	auth := s.settings.load().SettingsRoute
	if !auth.configured() || !oidc.LooksLikeJWT(presented) {
		return "", "", false
	}
	claims, err := auth.Verifier.Verify(r.Context(), presented)
	if err != nil {
		s.log.Warn("api: a settings call presented a token this daemon will not take",
			"reason", oidc.Reason(err), "route", "settings")
		return "", "", false
	}
	if !slices.Contains(auth.ServiceAccounts, claims.Subject) {
		s.log.Warn("api: a settings call presented a token of a ServiceAccount this deployment does not name",
			"subject", claims.Subject, "route", "settings")
		return "", "", false
	}
	return claims.Subject, config.CallerProjected, true
}
