package agenthistory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/backoff"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
)

const (
	// batchEntries and batchBytes are the batch route's bounds: at most 256 entries and a 1 MiB
	// body per request.
	batchEntries = 256
	batchBytes   = 1 << 20
	// heartbeatEvery is how often the project's answering key is written while the loop is
	// configured; the listener hands a project's messages off only while it is younger than three
	// minutes.
	heartbeatEvery = time.Minute
	// notFoundRetry is how soon a route an older listener answers 404 is tried again.
	notFoundRetry = time.Minute
	// failureBackoff is the first wait after any other failure (backoff.Wait), and failureMax the
	// longest.
	failureBackoff = 2 * time.Second
	failureMax     = time.Minute
	requestTimeout = 10 * time.Second
	// refusalBodyLimit bounds how much of a refusal's body a log line carries.
	refusalBodyLimit = 4096
	// registryTime is RFC 3339 in UTC at a fixed millisecond width, so two times order as their
	// strings do.
	registryTime = "2006-01-02T15:04:05.000Z07:00"
)

// keySafe is a session id the registry can key (`session.<id>`): no `.`, which separates a NATS
// key's tokens, and nothing a URL path would need to escape.
var keySafe = regexp.MustCompile(`^[A-Za-z0-9_=-]+$`)

// Entry is one registry entry as the listener's write routes take it (dispatch://INFRA-448, "The
// registry contract"): LegionProject is the Legion project token role tokens carry
// (claim.ProjectToken), never the Dispatch project; StartedAt is RFC 3339 UTC.
type Entry struct {
	SessionID     string `json:"session_id"`
	LegionProject string `json:"legion_project"`
	Issue         string `json:"issue"`
	Role          string `json:"role"`
	RoleToken     string `json:"role_token"`
	Title         string `json:"title"`
	Harness       string `json:"harness"`
	StartedAt     string `json:"started_at"`
}

// entryOf is the registry entry of row, or why it has none: a session id the registry cannot key,
// or a row whose token is not the one its project, issue and role build.
func entryOf(row store.AgentConversation) (Entry, string) {
	if !keySafe.MatchString(row.Session) {
		return Entry{}, "its session id is not one the registry can key"
	}
	if token, err := claim.NewToken(row.Project, row.Issue, row.Role); err != nil || token != row.Token {
		return Entry{}, fmt.Sprintf("its token %s is not the one its project, issue and role build", row.Token)
	}
	if row.Harness != runtime.BackendOmp && row.Harness != runtime.BackendClaude {
		return Entry{}, fmt.Sprintf("its harness %q is neither omp nor claude", row.Harness)
	}
	return Entry{
		SessionID: row.Session, LegionProject: row.Project, Issue: row.Issue, Role: string(row.Role), RoleToken: string(row.Token),
		Title: Title(row.Role, row.Issue), Harness: string(row.Harness), StartedAt: row.StartedAt.UTC().Format(registryTime),
	}, ""
}

// Clock is time as the mirror sees it.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// RealClock is the wall clock.
type RealClock struct{}

func (RealClock) Now() time.Time                         { return time.Now() }
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// MirrorConfig is what a Mirror is built from.
type MirrorConfig struct {
	// BaseURL and Token are the Envoy listener's address and its `/v1` bearer, the daemon's own
	// (envoy_url, envoy_token_file, as notify.HTTPPublisher holds them); Token "" sends none.
	BaseURL, Token string
	// Project is the Legion project token: the heartbeat's key and every entry's legion_project.
	Project string
	Store   Store
	Clock   Clock
	Log     *slog.Logger
	// Client is the HTTP client; nil is one with a ten-second timeout.
	Client *http.Client
}

// Mirror writes the history to the listener's Legion agent registry while the answer loop is
// configured: once Run starts, every recorded conversation of the project in batches (each at most
// 256 entries and 1 MiB), then one PUT for each registration Put hands it, in the order handed; and
// the project's `answering` key every minute. Nothing it does blocks the boot or a registration. A
// route an older listener answers 404 is logged once and tried again each minute; any other failure
// is tried again on a backoff. A write the listener refuses (400 or 409: an entry it will never
// take) is logged and dropped; a refused batch is sent again an entry at a time, so one bad entry
// costs only itself.
type Mirror struct {
	cfg  MirrorConfig
	wake chan struct{}

	mu sync.Mutex
	// puts are the registrations Put handed and Run has not written, one per session, in order;
	// a later registration of a session replaces its entry in place.
	puts []Entry

	// The rest is Run's own.
	booted    bool
	batches   [][]Entry
	writes    lane
	heartbeat lane
	logged404 map[string]bool
}

// lane is one kind of write's schedule: when it is next tried, and how many failures in a row it
// has had.
type lane struct {
	next     time.Time
	failures int
}

// NewMirror builds a mirror that writes nothing until Run.
func NewMirror(cfg MirrorConfig) *Mirror {
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: requestTimeout}
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Mirror{cfg: cfg, wake: make(chan struct{}, 1), logged404: map[string]bool{}}
}

// Put queues the registration row for one PUT of its session, never waiting on the listener.
func (m *Mirror) Put(row store.AgentConversation) {
	entry, why := entryOf(row)
	if why != "" {
		m.cfg.Log.Warn("legion daemon does not write the conversation to the agent registry: "+why, "session", row.Session, "claim", row.Token)
		return
	}
	m.mu.Lock()
	replaced := false
	for i := range m.puts {
		if m.puts[i].SessionID == entry.SessionID {
			m.puts[i], replaced = entry, true
		}
	}
	if !replaced {
		m.puts = append(m.puts, entry)
	}
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Run writes until ctx ends: the heartbeat at once and then each minute, the recorded history in
// batches, and every Put as it comes, each kind on its own schedule (lane), so a route an older
// listener lacks never holds up one it has.
func (m *Mirror) Run(ctx context.Context) {
	for {
		now := m.cfg.Clock.Now()
		if !now.Before(m.heartbeat.next) {
			m.beat(ctx, now)
		}
		if !now.Before(m.writes.next) && m.pendingWrites() {
			m.write(ctx, now)
		}
		if ctx.Err() != nil {
			return
		}
		next := m.heartbeat.next
		if m.pendingWrites() && m.writes.next.Before(next) {
			next = m.writes.next
		}
		// A registration wakes the loop at once; a lane on a failure's schedule is still not tried
		// before its time.
		select {
		case <-ctx.Done():
			return
		case <-m.cfg.Clock.After(max(next.Sub(m.cfg.Clock.Now()), 0)):
		case <-m.wake:
		}
	}
}

func (m *Mirror) pendingWrites() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.booted || len(m.batches) > 0 || len(m.puts) > 0
}

// beat writes the project's answering key.
func (m *Mirror) beat(ctx context.Context, now time.Time) {
	body := struct {
		Answering bool   `json:"answering"`
		UpdatedAt string `json:"updated_at"`
	}{true, now.UTC().Format(registryTime)}
	result := m.send(ctx, "PUT /v1/legion/projects/{project}", "/v1/legion/projects/"+url.PathEscape(m.cfg.Project), body)
	if result == refused {
		// The key is the listener's to refuse; the next minute's is a new write all the same.
		result = sent
	}
	m.heartbeat.schedule(now, result, heartbeatEvery)
}

// write reads the history once, then sends the batches and the Puts in order, stopping at the
// first write that must be tried again.
func (m *Mirror) write(ctx context.Context, now time.Time) {
	result := m.writeAll(ctx)
	m.writes.schedule(now, result, 0)
}

func (m *Mirror) writeAll(ctx context.Context) outcome {
	if !m.booted {
		rows, err := m.cfg.Store.AgentConversations(ctx, m.cfg.Project)
		if err != nil {
			m.cfg.Log.Warn("legion daemon could not read its conversation history for the agent registry; trying again", "error", err)
			return failed
		}
		batches := m.batchesOf(rows)
		m.mu.Lock()
		m.batches, m.booted = batches, true
		m.mu.Unlock()
	}
	for {
		m.mu.Lock()
		if len(m.batches) == 0 {
			m.mu.Unlock()
			break
		}
		batch := m.batches[0]
		m.mu.Unlock()
		result := m.send(ctx, "PUT /v1/legion/agents", "/v1/legion/agents", struct {
			Entries []Entry `json:"entries"`
		}{batch})
		switch result {
		case sent:
			m.popBatch(nil)
		case refused:
			// One bad entry refuses its whole batch: each is sent alone, and only a bad one is
			// refused again.
			var singles [][]Entry
			if len(batch) > 1 {
				for _, entry := range batch {
					singles = append(singles, []Entry{entry})
				}
			}
			m.popBatch(singles)
		default:
			return result
		}
	}
	for {
		m.mu.Lock()
		if len(m.puts) == 0 {
			m.mu.Unlock()
			return sent
		}
		entry := m.puts[0]
		m.mu.Unlock()
		result := m.send(ctx, "PUT /v1/legion/agents/sessions/{id}", "/v1/legion/agents/sessions/"+url.PathEscape(entry.SessionID), entry)
		if result != sent && result != refused {
			return result
		}
		m.mu.Lock()
		// A registration of the same session that came while this one was sent replaced it, and
		// is still to be written.
		if len(m.puts) > 0 && m.puts[0] == entry {
			m.puts = m.puts[1:]
		}
		m.mu.Unlock()
	}
}

// popBatch drops the batch at the head, putting replace in its place.
func (m *Mirror) popBatch(replace [][]Entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batches = append(replace, m.batches[1:]...)
}

// batchesOf is the history's entries in batches within the route's bounds, oldest first. A
// scanned conversation that would read as its role's newest over a registered one (its header
// time not before that role's newest registration) is left out: a conversation found by its title
// never outranks one a registration recorded.
func (m *Mirror) batchesOf(rows []store.AgentConversation) [][]Entry {
	registered := map[claim.Token]time.Time{}
	for _, row := range rows {
		if row.Source == store.ConversationRegistered && row.StartedAt.After(registered[row.Token]) {
			registered[row.Token] = row.StartedAt
		}
	}
	var batches [][]Entry
	var batch []Entry
	size := 0
	const envelope = len(`{"entries":[]}`)
	for _, row := range rows {
		if newest, ok := registered[row.Token]; ok && row.Source == store.ConversationScanned && !row.StartedAt.Before(newest) {
			m.cfg.Log.Info("legion daemon does not write a scanned conversation to the agent registry: it would read as its role's newest over a registered one",
				"session", row.Session, "claim", row.Token, "path", row.Location)
			continue
		}
		entry, why := entryOf(row)
		if why != "" {
			m.cfg.Log.Warn("legion daemon does not write the conversation to the agent registry: "+why, "session", row.Session, "claim", row.Token)
			continue
		}
		encoded, err := json.Marshal(entry)
		if err != nil || envelope+len(encoded) > batchBytes {
			m.cfg.Log.Warn("legion daemon does not write the conversation to the agent registry: its entry exceeds the batch route's 1 MiB", "session", row.Session)
			continue
		}
		grown := size + len(encoded)
		if len(batch) > 0 {
			grown++ // the comma before it
		}
		if len(batch) == batchEntries || envelope+grown > batchBytes {
			batches = append(batches, batch)
			batch, grown = nil, len(encoded)
		}
		batch, size = append(batch, entry), grown
	}
	if len(batch) > 0 {
		batches = append(batches, batch)
	}
	return batches
}

// outcome is what one write came to.
type outcome int

const (
	sent outcome = iota
	// notFound is a 404: a listener that predates the route.
	notFound
	// refused is a 400 or 409: the listener will never take what was sent.
	refused
	// failed is everything else, tried again on a backoff.
	failed
)

// schedule sets when the lane is next tried after a write that came to result at now: after every
// in a success (0 for whenever there is something to write), each minute after a 404, and on a
// backoff after any other failure.
func (l *lane) schedule(now time.Time, result outcome, every time.Duration) {
	switch result {
	case sent, refused:
		l.failures, l.next = 0, now.Add(every)
		if every == 0 {
			l.next = time.Time{}
		}
	case notFound:
		l.failures, l.next = 0, now.Add(notFoundRetry)
	default:
		l.failures++
		l.next = now.Add(min(backoff.Wait(failureBackoff, l.failures), failureMax))
	}
}

// send PUTs body to path, logging what is not a success: a route's first 404, a refusal, and every
// other failure.
func (m *Mirror) send(ctx context.Context, route, path string, body any) outcome {
	encoded, err := json.Marshal(body)
	if err != nil {
		m.cfg.Log.Error("legion daemon could not encode an agent registry write", "route", route, "error", err)
		return refused
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, m.cfg.BaseURL+path, bytes.NewReader(encoded))
	if err != nil {
		m.cfg.Log.Error("legion daemon could not build an agent registry write", "route", route, "error", err)
		return refused
	}
	request.Header.Set("Content-Type", "application/json")
	if m.cfg.Token != "" {
		request.Header.Set("Authorization", "Bearer "+m.cfg.Token)
	}
	response, err := m.cfg.Client.Do(request)
	if err != nil {
		if ctx.Err() == nil {
			m.cfg.Log.Warn("legion daemon could not write to the agent registry; trying again", "route", route, "error", err)
		}
		return failed
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, refusalBodyLimit))
	switch status := response.StatusCode; {
	case status >= 200 && status < 300:
		return sent
	case status == http.StatusNotFound:
		if !m.logged404[route] {
			m.logged404[route] = true
			m.cfg.Log.Warn("the Envoy listener answers 404 on "+route+": it predates the Legion agent registry; the daemon tries again each minute",
				"route", route, "answer", strings.TrimSpace(string(answer)))
		}
		return notFound
	case status == http.StatusBadRequest || status == http.StatusConflict:
		m.cfg.Log.Warn("the Envoy listener refused an agent registry write; it is not sent again", "route", route, "path", path,
			"status", status, "answer", strings.TrimSpace(string(answer)))
		return refused
	default:
		m.cfg.Log.Warn("the Envoy listener failed an agent registry write; trying again", "route", route, "path", path,
			"status", status, "answer", strings.TrimSpace(string(answer)))
		return failed
	}
}
