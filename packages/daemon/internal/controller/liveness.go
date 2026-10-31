package controller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// probeTimeout bounds one role lookup, so a hung listener answers Unknown rather than holding the
// caller.
const probeTimeout = 10 * time.Second

// stateUnresponsive is the `holder_state` the listener names in its 404 for a holder whose last
// registration is past the live window but still inside the gone horizon (writeRoleHolderError,
// packages/envoy/cmd/listener/api.go): the session is stalled, not dead — it keeps its role claim,
// nothing is released for it, and it answers for itself once its heartbeats land. A probe of such
// a holder is never a death verdict (dispatch://INFRA-392).
const stateUnresponsive = "unresponsive"

// Liveness is what the Envoy listener says of the registered controller session.
type Liveness string

const (
	// Alive: the listener names the session as the controller role's holder, which it does only
	// for a holder inside its live window.
	Alive Liveness = "alive"
	// Gone: the role is unheld or expired, held by another session, or its holder reached the
	// listener's gone horizon.
	Gone Liveness = "gone"
	// Unknown: the listener could not answer — unreachable, a refusal other than 404, or a body
	// the probe cannot read — or it named a holder that is only unresponsive. Never a death
	// verdict.
	Unknown Liveness = "unknown"
)

// ProberOptions are what a Prober reads the listener with.
type ProberOptions struct {
	// EnvoyURL is the listener's base URL; EnvoyToken its bearer ("" sends none).
	EnvoyURL, EnvoyToken string
	// Project is the project token (claim.ProjectToken): the lookup is its controller token.
	Project string
	// Log receives each non-alive verdict's reason; nil is slog.Default().
	Log *slog.Logger
}

// Prober reads the registered controller session's liveness from the Envoy role registry, under
// either `controller` mode: the one record of the operator's controller the daemon can read, since
// its session is on the operator's own machine with no process of the daemon's to probe, and the
// role the daemon's own controller must hold to take its wakes.
//
// The listener owns the windows: it answers the lookup only for a holder inside its live window
// (`ENVOY_SESSION_LIVE_WINDOW`), and names the holder's state in the 404 it answers for any
// other, so the daemon keeps no staleness window of its own to drift from it.
type Prober struct {
	lookup string
	token  string
	log    *slog.Logger
	client *http.Client
}

// NewProber is a Prober over opts.
func NewProber(opts ProberOptions) *Prober {
	p := &Prober{
		lookup: strings.TrimRight(opts.EnvoyURL, "/") + "/v1/roles/" + url.PathEscape(string(claim.ControllerToken(opts.Project))),
		token:  opts.EnvoyToken,
		log:    opts.Log,
		client: &http.Client{Timeout: probeTimeout},
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	return p
}

// roleHolder is the listener's live-holder answer (roleGetHandler,
// packages/envoy/cmd/listener/api.go): `last_seen` in unix milliseconds. Other members are not
// read.
type roleHolder struct {
	Holder   string `json:"holder"`
	LastSeen *int64 `json:"last_seen"`
}

// roleRefusal is the listener's 404 for a role it will not name a holder of (writeRoleHolderError,
// packages/envoy/cmd/listener/api.go): `reason` is `unclaimed` when nobody holds the role and
// `holder_lapsed` when its holder is past the live window, and `holder_state` is that holder's
// state, `unresponsive` or `gone`. A listener of an earlier release names neither.
type roleRefusal struct {
	Reason      string `json:"reason"`
	HolderState string `json:"holder_state"`
	LastSeen    *int64 `json:"last_seen"`
}

// Probe says whether session — the one registered with the current controller capability — is
// alive: Gone and Unknown each log why, naming the listener and, where the listener named them,
// both sessions or the holder's state.
func (p *Prober) Probe(ctx context.Context, session string) Liveness {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.lookup, nil)
	if err != nil {
		p.log.Warn("controller liveness: build the role lookup", "url", p.lookup, "error", err)
		return Unknown
	}
	if p.token != "" {
		request.Header.Set("Authorization", "Bearer "+p.token)
	}
	response, err := p.client.Do(request)
	if err != nil {
		p.log.Warn("controller liveness: the Envoy listener is unreachable", "url", p.lookup, "error", err)
		return Unknown
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(response.Body)
	if response.StatusCode == http.StatusNotFound {
		var refusal roleRefusal
		if readErr == nil {
			_ = json.Unmarshal(body, &refusal)
		}
		if refusal.HolderState == stateUnresponsive {
			p.log.Info("controller liveness: the controller role's holder is unresponsive, not gone; the controller is left as it is",
				"url", p.lookup, "session", session, "reason", refusal.Reason, "lastSeen", refusal.LastSeen)
			return Unknown
		}
		p.log.Info("controller liveness: the controller role has no live holder; the controller is gone",
			"url", p.lookup, "session", session, "reason", refusal.Reason, "holderState", refusal.HolderState)
		return Gone
	}
	if response.StatusCode != http.StatusOK {
		reason := "the Envoy listener refused the controller role lookup"
		if p.token == "" {
			reason += " with no bearer token sent"
		}
		p.log.Warn("controller liveness: "+reason, "url", p.lookup, "status", response.StatusCode)
		return Unknown
	}
	var answer roleHolder
	if readErr != nil || json.Unmarshal(body, &answer) != nil || answer.Holder == "" || answer.LastSeen == nil {
		p.log.Warn("controller liveness: the Envoy listener answered the controller role lookup with an unreadable body", "url", p.lookup)
		return Unknown
	}
	if answer.Holder != session {
		p.log.Info("controller liveness: another session holds the controller role; the recorded controller is gone",
			"holder", answer.Holder, "session", session)
		return Gone
	}
	return Alive
}
