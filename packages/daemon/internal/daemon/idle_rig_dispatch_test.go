package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// idleRigDispatch is the board the rig's daemon reads: each issue of the rig as Dispatch holds
// it, handed to Legion and in the workflow, so the daemon's boot leaves the trees live rather
// than lingering them closed. It is live_proof_test.go's liveDispatch (that file is `//go:build
// e2e`, so this is its copy for the default build), with the repository mapping and the webhook
// App check workflowConfig's stand-in answers (servedMapping).
type idleRigDispatch struct {
	t       *testing.T
	project string
	url     string
	mu      sync.Mutex
	issues  map[string]idleRigDispatchIssue
	// posted is every status the daemon set and every message it posted, oldest first.
	posted []idleRigDispatchWrite
}

// idleRigDispatchIssue is one issue as the board holds it: its status, and whether it is handed
// to Legion, which its labels say.
type idleRigDispatchIssue struct {
	status string
	handed bool
}

// idleRigDispatchWrite is one write the daemon made: a status it set, or a message it posted.
type idleRigDispatchWrite struct{ Issue, Status, Body string }

func newIdleRigDispatch(t *testing.T, project string) *idleRigDispatch {
	t.Helper()
	b := &idleRigDispatch{t: t, project: project, issues: map[string]idleRigDispatchIssue{}}
	server := httptest.NewServer(b)
	t.Cleanup(server.Close)
	b.url = server.URL
	return b
}

// show puts key on the board at status, a root handed to Legion.
func (b *idleRigDispatch) show(key, status string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.issues[key] = idleRigDispatchIssue{status: status, handed: true}
}

func (b *idleRigDispatch) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if servedMapping(b.t, w, req, b.project) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	path := req.URL.Path
	key := issueOfIdleRigDispatchPath(path)
	switch {
	case req.Method == http.MethodGet && path == "/api/v1/issues":
		b.listIssues(w)
	case req.Method == http.MethodGet && path == "/healthz":
		_, _ = io.WriteString(w, `{"status":"ok","commit":null}`)
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/events"):
		_, _ = io.WriteString(w, `[]`)
	case req.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/issues/") && key != "":
		b.showIssue(key, w)
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/messages"):
		b.record(idleRigDispatchWrite{Issue: key, Body: b.decode(req, w)}, w)
	case req.Method == http.MethodPatch && strings.HasPrefix(path, "/api/v1/issues/"):
		write := idleRigDispatchWrite{Issue: key, Status: b.decode(req, w)}
		b.mu.Lock()
		issue := b.issues[key]
		issue.status = write.Status
		b.issues[key] = issue
		b.mu.Unlock()
		b.record(write, w)
	default:
		b.t.Errorf("Dispatch request = %s %s", req.Method, path)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}
}

func (b *idleRigDispatch) listIssues(w http.ResponseWriter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	listed := make([]map[string]any, 0, len(b.issues))
	for key := range b.issues {
		listed = append(listed, b.issueBody(key))
	}
	if err := json.NewEncoder(w).Encode(listed); err != nil {
		b.t.Errorf("write the Dispatch issue list: %v", err)
	}
}

func (b *idleRigDispatch) showIssue(key string, w http.ResponseWriter) {
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
func (b *idleRigDispatch) issueBody(key string) map[string]any {
	issue := b.issues[key]
	labels := []string{}
	if issue.handed {
		labels = []string{dispatch.LegionLabel}
	}
	return map[string]any{
		"key": key, "project": b.project, "title": key, "status": issue.status, "parent": nil, "rank": "U",
		"labels": labels, "last_seq": 1, "resolved_repo": idleRigRepo,
	}
}

func (b *idleRigDispatch) decode(req *http.Request, w http.ResponseWriter) string {
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

func (b *idleRigDispatch) record(write idleRigDispatchWrite, w http.ResponseWriter) {
	b.mu.Lock()
	b.posted = append(b.posted, write)
	b.mu.Unlock()
	_, _ = io.WriteString(w, `{}`)
}

// issueOfIdleRigDispatchPath is the issue key in /api/v1/issues/<key>[/...].
func issueOfIdleRigDispatchPath(path string) string {
	rest := strings.TrimPrefix(path, "/api/v1/issues/")
	key, _, _ := strings.Cut(rest, "/")
	return key
}
