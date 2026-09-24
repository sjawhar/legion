package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A project document upload and another write to the same document, started together, both
// finish. The upload is held inside its transactional append, holding whatever it took before
// it, until the second request has either entered its document operation (it holds the owner
// row by then) or queued behind a lock. A write that waits for the in-memory writer slot while
// holding a lock the slot's holder needs is a cycle Postgres cannot see, so both would hang.
func TestProjectDocumentUploadAndAConcurrentWriteBothFinish(t *testing.T) {
	for _, test := range []struct {
		name  string
		write func(f *uploadRaceFixture, ctx context.Context, token string) *httptest.ResponseRecorder
		want  int
	}{
		{
			name: "conditional edit",
			write: func(f *uploadRaceFixture, ctx context.Context, token string) *httptest.ResponseRecorder {
				return f.request(ctx, http.MethodPost, "/api/v1/artifacts/"+f.document.ID+"/edits", map[string]any{
					"ops":          []map[string]string{{"op": "replace", "find": "before", "with": "edited"}},
					"precondition": map[string]string{"document": token},
				})
			},
			// The token predates the upload, which the edit sees committed.
			want: http.StatusConflict,
		},
		{
			name: "unconditional edit",
			write: func(f *uploadRaceFixture, ctx context.Context, _ string) *httptest.ResponseRecorder {
				return f.request(ctx, http.MethodPost, "/api/v1/artifacts/"+f.document.ID+"/edits", map[string]any{
					"ops": []map[string]string{{"op": "replace", "find": "before", "with": "edited"}},
				})
			},
			want: http.StatusOK,
		},
		{
			name: "anchored comment",
			write: func(f *uploadRaceFixture, ctx context.Context, _ string) *httptest.ResponseRecorder {
				return f.request(ctx, http.MethodPost, "/api/v1/artifacts/"+f.document.ID+"/comments", map[string]any{
					"body":   "Why before?",
					"anchor": map[string]string{"artifact": f.document.Slug, "quote": "before"},
				})
			},
			want: http.StatusCreated,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newUploadRaceFixture(t)
			token := readDocumentPrecondition(t, f.handler, f.document.ID).Token
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			f.persistence.holdNextAppend()
			uploads := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				uploads <- f.request(ctx, http.MethodPost, "/api/v1/projects/TEST/artifacts", map[string]string{
					"name": f.document.Name, "content": "before\n\nuploaded",
				})
			}()
			f.persistence.waitHeld(t)
			f.docs.arm()
			writes := make(chan *httptest.ResponseRecorder, 1)
			go func() { writes <- test.write(f, ctx, token) }()
			f.waitForWriteToEngage(t)
			f.persistence.release()

			upload := awaitWithin(t, uploads, cancel, "project document upload")
			write := awaitWithin(t, writes, cancel, test.name)
			if upload.Code != http.StatusCreated {
				t.Fatalf("upload: status=%d body=%s", upload.Code, upload.Body.String())
			}
			if write.Code != test.want {
				t.Fatalf("%s: status=%d body=%s, want %d", test.name, write.Code, write.Body.String(), test.want)
			}
		})
	}
}

type uploadRaceFixture struct {
	handler     http.Handler
	database    *store.Store
	docs        *engagingDocs
	persistence *heldAppendStore
	probe       *pgx.Conn
	document    model.Artifact
}

func newUploadRaceFixture(t *testing.T) *uploadRaceFixture {
	t.Helper()
	f := &uploadRaceFixture{}
	f.handler, f.database = newInteractionHandler(t, func(database *store.Store) docs.API {
		f.persistence = &heldAppendStore{PgVersioned: docs.NewPgVersioned(database)}
		service := docs.New(docs.Deps{
			Store:       database,
			Persistence: f.persistence,
			Identity: identity.HeaderIdentity{
				Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}},
			},
			Settle: time.Hour,
		})
		// Runs before the service shuts down: a failed assertion must not leave the upload
		// held inside its transaction.
		t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
		t.Cleanup(f.persistence.release)
		f.docs = &engagingDocs{API: service, engaged: make(chan struct{})}
		return f.docs
	})
	if response := dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	f.document = createProjectDocument(t, f.handler, "TEST", "notes", "before")
	probe, err := pgx.ConnectConfig(context.Background(), f.database.Pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("connect database probe: %v", err)
	}
	t.Cleanup(func() { _ = probe.Close(context.Background()) })
	f.probe = probe
	return f
}

func (f *uploadRaceFixture) request(ctx context.Context, method, target string, body any) *httptest.ResponseRecorder {
	data, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(data)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response
}

// waitForWriteToEngage returns once the second request has entered its document operation or
// waits on a database lock.
func (f *uploadRaceFixture) waitForWriteToEngage(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case <-f.docs.engaged:
			return
		default:
		}
		var waiting int
		if err := f.probe.QueryRow(context.Background(), `
			select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock'
		`).Scan(&waiting); err != nil {
			t.Fatalf("inspect database locks: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the second write neither entered its document operation nor waited on a lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// awaitWithin waits for a request that must not hang. On a timeout it cancels every request
// the test started, so a deadlocked pair unwinds before the test fails.
func awaitWithin(t *testing.T, responses <-chan *httptest.ResponseRecorder, cancel context.CancelFunc, name string) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case response := <-responses:
		return response
	case <-time.After(10 * time.Second):
		cancel()
		select {
		case <-responses:
		case <-time.After(10 * time.Second):
		}
		t.Fatalf("%s did not finish within 10 s", name)
		return nil
	}
}

// heldAppendStore holds the next transactional append it sees, once asked to, until release.
type heldAppendStore struct {
	*docs.PgVersioned
	mu       sync.Mutex
	hold     bool
	held     chan struct{}
	releaser chan struct{}
}

func (s *heldAppendStore) holdNextAppend() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold = true
	s.held = make(chan struct{})
	s.releaser = make(chan struct{})
}

func (s *heldAppendStore) waitHeld(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	held := s.held
	s.mu.Unlock()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the upload never reached its transactional append")
	}
}

func (s *heldAppendStore) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.releaser != nil {
		closeTestGate(s.releaser)
	}
}

func (s *heldAppendStore) AppendUpdateTx(ctx context.Context, tx pgx.Tx, room string, update []byte, contentChanged bool) (persistence.Version, error) {
	s.mu.Lock()
	hold := s.hold
	s.hold = false
	held, releaser := s.held, s.releaser
	s.mu.Unlock()
	if hold {
		close(held)
		<-releaser
	}
	return s.PgVersioned.AppendUpdateTx(ctx, tx, room, update, contentChanged)
}

// engagingDocs reports, once armed, the first edit or anchor mark that reaches the document
// service: the handler making it has locked the document's owner row by then.
type engagingDocs struct {
	docs.API
	arming  bool
	mu      sync.Mutex
	engaged chan struct{}
	once    sync.Once
}

func (d *engagingDocs) arm() {
	d.mu.Lock()
	d.arming = true
	d.mu.Unlock()
}

func (d *engagingDocs) engage() {
	d.mu.Lock()
	armed := d.arming
	d.mu.Unlock()
	if armed {
		d.once.Do(func() { close(d.engaged) })
	}
}

func (d *engagingDocs) ApplyOps(ctx context.Context, artifactID string, ops []model.EditOp, actor model.Actor, precondition *model.EditPrecondition) (int, error) {
	d.engage()
	return d.API.ApplyOps(ctx, artifactID, ops, actor, precondition)
}

func (d *engagingDocs) MarkQuote(ctx context.Context, artifactID string, mark docs.MarkSpec, quote string, occurrence *int) (docs.Anchored, error) {
	d.engage()
	return d.API.MarkQuote(ctx, artifactID, mark, quote, occurrence)
}
