package api

import (
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// newIssueSettings is what the dashboard's search bar needs to file a person's typed words as an
// issue when those words match nothing it found: the label that issue carries and the session it
// is routed to, as the deployment configured them (cmd/dispatch: DISPATCH_NEW_ISSUE_LABEL,
// DISPATCH_NEW_ISSUE_SESSION). Each is empty where the deployment set none: with no label the
// issue carries none, and with no session the bar makes the issue and starts no conversation on
// it. The bar writes the issue through the routes every other client writes through
// (POST /api/v1/issues and the issue's own artifact, patch and message routes), so this read is
// the only thing the configuration adds to the API.
type newIssueSettings struct {
	Label   string `json:"label"`
	Session string `json:"session"`
}

// newIssueSettingsFrom trims the two settings and answers them with the name of the first field
// the bar could not use: "label" where the label is longer than one an issue may carry, "session"
// where the session id names no route, and "" where both are usable. The first problem wins, as
// each boundary refused on its own before. Both trust boundaries check here - the deployment's
// environment at boot (NewDeps) and the test hook's request body (setNewIssueSettings) - and each
// states its own refusal, because a startup error and a 400 are read by different people.
func newIssueSettingsFrom(label, session string) (newIssueSettings, string) {
	set := newIssueSettings{Label: strings.TrimSpace(label), Session: strings.TrimSpace(session)}
	if itemblocks.Units16(set.Label) > maxIssueLabel16 {
		return set, "label"
	}
	if set.Session != "" {
		if _, err := model.ParseRoute("session:" + set.Session); err != nil {
			return set, "session"
		}
	}
	return set, ""
}

// configuredNewIssue is what the two settings stand at now: what the test hook last set, else
// what the deployment configured.
func (s *server) configuredNewIssue() newIssueSettings {
	if set := s.newIssueOverride.Load(); set != nil {
		return *set
	}
	return newIssueSettings{Label: s.deps.NewIssueLabel, Session: s.deps.NewIssueSession}
}

// getNewIssueSettings is GET /api/v1/settings/new-issue.
func (s *server) getNewIssueSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	WriteJSON(w, http.StatusOK, s.configuredNewIssue())
}

// setNewIssueSettings is POST /api/v1/settings/new-issue/_test, {label, session}: from now on
// this process answers GET /api/v1/settings/new-issue with these two values in place of the ones
// its environment set, so a browser test can read the bar with a label and a session configured
// and then with the session cleared, which no deployment setting can change while the server
// runs. It acts on the process the call lands on, as the slow-write hook does. Test-only:
// mounted only under Deps.TestHooksEnabled.
func (s *server) setNewIssueSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var input newIssueSettings
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	set, problem := newIssueSettingsFrom(input.Label, input.Session)
	switch problem {
	case "label":
		writeError(w, "INVALID_BODY", http.StatusBadRequest,
			"label must be a label an issue may carry, or empty for none")
		return
	case "session":
		writeError(w, "INVALID_BODY", http.StatusBadRequest,
			"session must be a session id, or empty for no conversation")
		return
	}
	s.newIssueOverride.Store(&set)
	WriteJSON(w, http.StatusOK, set)
}
