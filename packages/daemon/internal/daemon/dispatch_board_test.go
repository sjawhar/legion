package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// liveRepo is the repository the project's Dispatch mapping serves (workflowConfig's
// servedMapping), which every issue a live proof and the idle rig seed is on.
const liveRepo = "acme/widgets"

// liveDispatch is workflowConfig's Dispatch stand-in — the repository mapping and an empty issue
// list — grown into the board the daemon reads and writes: it holds each issue's status, parent and
// labels, as Dispatch's own board does, answers the issue reads the daemon makes (the outbox's
// before a status write, a release's walk up an issue's parent chain, an event's walk up its
// Dispatch chain to the top) and 404s an issue it does not hold, records every status and message
// the daemon posted, so a step can read back what the daemon told Dispatch and tell a write that
// never came from one it cannot see, and serves the handoff records a step seeds (handoff), which a
// phase completion names and the daemon reads back. Anything else is a call a live proof does not
// expect, and fails the test as workflowConfig's stand-in does.
type liveDispatch struct {
	t       *testing.T
	project string
	// url is the board's own server, which a caller's DispatchURL names.
	url string
	// lists is whether GET /api/v1/issues answers the issues the board holds or an empty list.
	// The idle rig sets it, because its daemon's boot lingers closed every recorded issue the
	// listing does not hold; a live proof leaves it false, seeding its own admission instead, so
	// the boot's reconcile admits nothing of its own behind the driver's back.
	lists bool
	mu    sync.Mutex
	// issues is each issue as this board holds it, the daemon's writes applied.
	issues map[string]liveDispatchIssue
	posted []liveDispatchWrite
	// handoffs is each seeded handoff record by its id, as GET /api/v1/handoffs/{id} answers it.
	handoffs map[string]map[string]any
}

// liveDispatchIssue is one issue as the board holds it: its status, its parent ("" for none), and
// whether it is handed to Legion, which its labels say.
type liveDispatchIssue struct {
	status, parent string
	handed         bool
}

// liveDispatchWrite is one write the daemon made to Dispatch: a status it set on an issue, or a
// message it posted on one.
type liveDispatchWrite struct {
	Issue  string
	Status string
	Body   string
}

// newLiveDispatch is the board on a server of its own, closed with the test: a caller points its
// configuration at url.
func newLiveDispatch(t *testing.T, project string) *liveDispatch {
	t.Helper()
	b := &liveDispatch{t: t, project: project, issues: map[string]liveDispatchIssue{}, handoffs: map[string]map[string]any{}}
	server := httptest.NewServer(b)
	t.Cleanup(server.Close)
	b.url = server.URL
	return b
}

// show puts key on the board at status, a root handed to Legion, as the seeded fixture stands on
// Dispatch before the daemon writes anything.
func (b *liveDispatch) show(key, status string) {
	b.place(key, status, "", true)
}

// place puts key on the board at status under parent ("" for none), labelled for Legion when
// handed.
func (b *liveDispatch) place(key, status, parent string, handed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.issues[key] = liveDispatchIssue{status: status, parent: parent, handed: handed}
}

// liveDispatchLabels is an issue's labels: Legion's when it is handed to Legion, none otherwise.
func liveDispatchLabels(handed bool) []string {
	if handed {
		return []string{dispatch.LegionLabel}
	}
	return []string{}
}

// handoff seeds version 1 of key's handoff record of phase word, written by session, as a worker's
// `dispatch handoff-write` leaves it, and answers its id for the completion to name.
func (b *liveDispatch) handoff(key, word, session string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", len(b.handoffs)+1)
	b.handoffs[id] = map[string]any{
		"id": id, "issue_key": key, "phase": word, "version": 1, "created_at": "2026-10-27T00:00:00Z",
		"author": map[string]any{"kind": "session", "id": session}, "body": "{}",
	}
	return id
}

func (b *liveDispatch) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if servedMapping(b.t, w, req, b.project) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	path := req.URL.Path
	key := issueOfDispatchPath(path)
	switch {
	case req.Method == http.MethodGet && path == "/api/v1/issues":
		b.listIssues(w)
	case req.Method == http.MethodGet && path == "/healthz":
		_, _ = io.WriteString(w, `{"status":"ok","commit":null}`)
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/events"):
		// The outbox reads an issue's events to see whether its message is already posted; this
		// board keeps none, so every message the daemon decides to post is posted.
		_, _ = io.WriteString(w, `[]`)
	case req.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/issues/") && key != "":
		b.showIssue(key, w)
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/messages"):
		b.record(liveDispatchWrite{Issue: key, Body: b.decode(req, w)}, w)
	case req.Method == http.MethodPatch && strings.HasPrefix(path, "/api/v1/issues/"):
		write := liveDispatchWrite{Issue: key, Status: b.decode(req, w)}
		b.mu.Lock()
		issue := b.issues[key]
		issue.status = write.Status
		b.issues[key] = issue
		b.mu.Unlock()
		b.record(write, w)
	case req.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/handoffs/"):
		b.showHandoff(strings.TrimPrefix(path, "/api/v1/handoffs/"), w)
	default:
		b.t.Errorf("Dispatch request = %s %s", req.Method, path)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}
}

// listIssues answers Dispatch's project listing: every issue the board holds when it lists, and
// an empty list otherwise.
func (b *liveDispatch) listIssues(w http.ResponseWriter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	listed := make([]map[string]any, 0, len(b.issues))
	if b.lists {
		for key := range b.issues {
			listed = append(listed, b.issueBody(key))
		}
	}
	if err := json.NewEncoder(w).Encode(listed); err != nil {
		b.t.Errorf("write the Dispatch issue list: %v", err)
	}
}

// showIssue answers Dispatch's issue read of key from the board, and 404 for an issue it does not
// hold.
func (b *liveDispatch) showIssue(key string, w http.ResponseWriter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.issues[key]; !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":"NOT_FOUND","error":"no such issue"}`)
		return
	}
	if err := json.NewEncoder(w).Encode(b.issueBody(key)); err != nil {
		b.t.Errorf("write the Dispatch issue %s: %v", key, err)
	}
}

// issueBody is one issue as Dispatch answers it. The caller holds the lock.
func (b *liveDispatch) issueBody(key string) map[string]any {
	issue := b.issues[key]
	var parent any
	if issue.parent != "" {
		parent = issue.parent
	}
	return map[string]any{
		"key": key, "project": b.project, "title": key, "status": issue.status, "parent": parent, "rank": "U",
		"labels": liveDispatchLabels(issue.handed), "last_seq": 1, "resolved_repo": liveRepo,
	}
}

// showHandoff answers the handoff record read a completion makes: the seeded record, or Dispatch's
// 404 HANDOFF_NOT_FOUND.
func (b *liveDispatch) showHandoff(id string, w http.ResponseWriter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	handoff, ok := b.handoffs[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":"HANDOFF_NOT_FOUND","error":"handoff `+id+` not found"}`)
		return
	}
	if err := json.NewEncoder(w).Encode(handoff); err != nil {
		b.t.Errorf("write the Dispatch handoff %s: %v", id, err)
	}
}

// decode is the write's status or body, whichever the daemon sent.
func (b *liveDispatch) decode(req *http.Request, w http.ResponseWriter) string {
	var write struct {
		Status string `json:"status"`
		Body   string `json:"body"`
	}
	if err := json.NewDecoder(req.Body).Decode(&write); err != nil {
		b.t.Errorf("decode the Dispatch write to %s: %v", req.URL.Path, err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return ""
	}
	if write.Status != "" {
		return write.Status
	}
	return write.Body
}

func (b *liveDispatch) record(write liveDispatchWrite, w http.ResponseWriter) {
	b.mu.Lock()
	b.posted = append(b.posted, write)
	b.mu.Unlock()
	_, _ = io.WriteString(w, `{}`)
}

// writes is every status the daemon has set and every message it has posted, oldest first.
func (b *liveDispatch) writes() []liveDispatchWrite {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]liveDispatchWrite(nil), b.posted...)
}

// wrote says whether the daemon set issue to status.
func (b *liveDispatch) wrote(issue, status string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, write := range b.posted {
		if write.Issue == issue && write.Status == status {
			return true
		}
	}
	return false
}

// issueOfDispatchPath is the issue key in /api/v1/issues/<key>[/...].
func issueOfDispatchPath(path string) string {
	rest := strings.TrimPrefix(path, "/api/v1/issues/")
	key, _, _ := strings.Cut(rest, "/")
	return key
}
