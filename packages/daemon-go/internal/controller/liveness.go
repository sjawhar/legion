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

// Heartbeat is the pi-envoy plugin's registration heartbeat (`DEFAULT_HEARTBEAT_MS`,
// packages/envoy-client/src/defaults.ts): what refreshes a session's `last_seen` in the Envoy
// listener's role lookup.
const Heartbeat = 120 * time.Second

// probeTimeout bounds one role lookup, so a hung listener answers Unknown rather than holding the
// caller.
const probeTimeout = 10 * time.Second

// LivenessWindow is how stale the controller holder's `last_seen` may be before the controller
// counts as gone: two heartbeats at least, since one would flap on every tick, and never less than
// the boot timeout — 240 s at the defaults (packages/daemon/src/daemon/runtime-kubernetes.ts:59-69).
func LivenessWindow(bootTimeout time.Duration) time.Duration {
	return max(bootTimeout, 2*Heartbeat)
}

// Liveness is what the Envoy listener says of the registered controller session.
type Liveness string

const (
	// Alive: the listener names the session as the controller role's holder and saw it within
	// the liveness window.
	Alive Liveness = "alive"
	// Gone: the role is unheld or expired, held by another session, or its holder was last seen
	// at or beyond the window.
	Gone Liveness = "gone"
	// Unknown: the listener could not answer — unreachable, a refusal other than 404, or a body
	// the probe cannot read. Never a death verdict.
	Unknown Liveness = "unknown"
)

// ProberOptions are what a Prober reads the listener with.
type ProberOptions struct {
	// EnvoyURL is the listener's base URL; EnvoyToken its bearer ("" sends none).
	EnvoyURL, EnvoyToken string
	// Project is the project token (claim.ProjectToken): the lookup is its controller token.
	Project string
	// BootTimeout sets the liveness window (LivenessWindow).
	BootTimeout time.Duration
	// Now is the clock `last_seen` is aged against; nil is time.Now.
	Now func() time.Time
	// Log receives each non-alive verdict's reason; nil is slog.Default().
	Log *slog.Logger
}

// Prober reads an operator-launched controller's liveness from the Envoy role registry, the one
// record of it the daemon can read: the operator's session is on their own machine, with no
// process of the daemon's to probe (the shipped probeOperatorController,
// packages/daemon/src/daemon/runtime-kubernetes.ts:627-675).
type Prober struct {
	lookup string
	token  string
	window time.Duration
	now    func() time.Time
	log    *slog.Logger
	client *http.Client
}

// NewProber is a Prober over opts.
func NewProber(opts ProberOptions) *Prober {
	p := &Prober{
		lookup: strings.TrimRight(opts.EnvoyURL, "/") + "/v1/roles/" + url.PathEscape(string(claim.ControllerToken(opts.Project))),
		token:  opts.EnvoyToken,
		window: LivenessWindow(opts.BootTimeout),
		now:    opts.Now,
		log:    opts.Log,
		client: &http.Client{Timeout: probeTimeout},
	}
	if p.now == nil {
		p.now = time.Now
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

// Probe says whether session — the one registered with the current controller capability — is
// alive: Gone and Unknown each log why, naming the listener and, where the listener named them,
// both sessions or the holder's age.
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
	if response.StatusCode == http.StatusNotFound {
		p.log.Info("controller liveness: the controller role has no live holder; the controller is gone", "url", p.lookup, "session", session)
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
	body, err := io.ReadAll(response.Body)
	var answer roleHolder
	if err != nil || json.Unmarshal(body, &answer) != nil || answer.Holder == "" || answer.LastSeen == nil {
		p.log.Warn("controller liveness: the Envoy listener answered the controller role lookup with an unreadable body", "url", p.lookup)
		return Unknown
	}
	if answer.Holder != session {
		p.log.Info("controller liveness: another session holds the controller role; the recorded controller is gone",
			"holder", answer.Holder, "session", session)
		return Gone
	}
	age := p.now().Sub(time.UnixMilli(*answer.LastSeen))
	if age >= p.window {
		p.log.Info("controller liveness: the controller session was last seen outside the liveness window; it is gone",
			"session", session, "ageSeconds", int64(age/time.Second), "windowSeconds", int64(p.window/time.Second))
		return Gone
	}
	return Alive
}
