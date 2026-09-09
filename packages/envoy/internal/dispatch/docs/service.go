package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// ErrIssueClosed rejects a document mutation after its issue is closed.
var ErrIssueClosed = errors.New("issue is closed")

// Deps configures the live document service.
type Deps struct {
	Store      *store.Store
	Events     *events.Broker
	Identity   identity.Identity
	AgentToken string
	Settle     time.Duration
}

// Service owns live Yjs documents and their durable Dispatch versions.
type Service struct {
	srv         *websocket.Server
	store       *store.Store
	persistence *PgVersioned
	events      *events.Broker
	identity    identity.Identity
	agentToken  string
	settle      time.Duration
	rooms       sync.Map
}

type roomState struct {
	actors map[string]model.Actor
	settle *time.Timer
	mu     sync.Mutex
}

type connectionState struct {
	room  string
	actor model.Actor
	added bool
}

type connectionContextKey struct{}

// New constructs the ygo server used by Dispatch's document API and websocket
// endpoint.
func New(deps Deps) *Service {
	settle := deps.Settle
	if settle <= 0 {
		settle = 2 * time.Second
	}
	if deps.Events == nil {
		deps.Events = events.NewBroker()
	}
	persist := NewPgVersioned(deps.Store)
	adapter := persistence.NewLegacyAdapter(persist)
	adapter.KeepVersions = 500
	srv := websocket.NewServerWithPersistence(adapter)
	srv.HocuspocusFraming = true
	srv.CompactEvery = 200

	service := &Service{
		srv:         srv,
		store:       deps.Store,
		persistence: persist,
		events:      deps.Events,
		identity:    deps.Identity,
		agentToken:  deps.AgentToken,
		settle:      settle,
	}
	srv.Authorize = service.authorize
	srv.OnInject = service.allowInject
	srv.OnLoadDocument = service.onLoadDocument
	return service
}

// ServeHTTP serves the Hocuspocus-framed document websocket endpoint.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	connection := &connectionState{}
	ctx := context.WithValue(r.Context(), connectionContextKey{}, connection)
	s.srv.ServeHTTP(w, r.WithContext(ctx))
	if connection.added {
		s.removeConnection(connection.room, connection.actor)
	}
}

// Shutdown stops timers and flushes ygo's document persistence workers.
func (s *Service) Shutdown(ctx context.Context) error {
	s.rooms.Range(func(_, value any) bool {
		room := value.(*roomState)
		room.mu.Lock()
		if room.settle != nil {
			room.settle.Stop()
		}
		room.mu.Unlock()
		return true
	})
	return s.srv.Shutdown(ctx)
}

// SeedText writes a new room's first Yjs update inside the caller's artifact
// creation transaction. A new room is loaded from this update on first use.
func (s *Service) SeedText(ctx context.Context, tx pgx.Tx, artifactID, markdown string) error {
	doc := crdt.New()
	content := doc.GetText("content")
	doc.Transact(func(transaction *crdt.Transaction) {
		content.Insert(transaction, 0, markdown, nil)
	})
	if _, err := s.persistence.AppendUpdateTx(ctx, tx, artifactID, crdt.EncodeStateAsUpdateV1(doc, nil)); err != nil {
		return fmt.Errorf("seed live document: %w", err)
	}
	return nil
}

// ReplaceText replaces the entire live Yjs text so connected clients receive
// document uploads as a regular server-side transaction.
func (s *Service) ReplaceText(ctx context.Context, artifactID, markdown string, actor model.Actor) error {
	s.recordActor(artifactID, actor)
	if err := s.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		content := doc.GetText("content")
		length := content.Len()
		transact(func(transaction *crdt.Transaction) {
			if length > 0 {
				content.Delete(transaction, 0, length)
			}
			content.Insert(transaction, 0, markdown, nil)
		})
	}); err != nil {
		return fmt.Errorf("replace live document: %w", err)
	}
	return nil
}

// Text returns the current Yjs text, loading and rendering persisted state when
// the document room is not resident.
func (s *Service) Text(ctx context.Context, artifactID string) (string, error) {
	if doc := s.srv.GetDoc(artifactID); doc != nil {
		return doc.GetText("content").ToString(), nil
	}
	loaded, err := s.persistence.Load(ctx, artifactID)
	if err != nil {
		return "", fmt.Errorf("load live document: %w", err)
	}
	if len(loaded.Update) == 0 {
		return "", nil
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		return "", fmt.Errorf("decode live document: %w", err)
	}
	return doc.GetText("content").ToString(), nil
}

// SnapshotVersion returns the current immutable version, adding an unnamed
// version only when the live text has diverged since the previous one.
func (s *Service) SnapshotVersion(ctx context.Context, tx pgx.Tx, artifactID string, actor model.Actor) (model.Version, bool, error) {
	state := s.room(artifactID)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.actors[actorKey(actor)] = actor
	markdown, err := s.Text(ctx, artifactID)
	if err != nil {
		return model.Version{}, false, err
	}
	latest, err := latestVersion(ctx, tx, artifactID)
	if err != nil {
		return model.Version{}, false, err
	}
	if latest.markdown != markdown {
		version, err := writeVersion(ctx, tx, artifactID, markdown, false, nil, actorSlice(state.actors))
		if err != nil {
			return model.Version{}, false, err
		}
		state.actors = make(map[string]model.Actor)
		return version, true, nil
	}
	return latest.Version, false, nil
}

// ApplyOps resolves every requested operation before it changes the Yjs room,
// so an ambiguous or missing later target cannot leave a partial edit behind.
func (s *Service) ApplyOps(ctx context.Context, artifactID string, ops []model.EditOp, actor model.Actor) (int, error) {
	if len(ops) == 0 {
		return 0, nil
	}
	current, err := s.Text(ctx, artifactID)
	if err != nil {
		return 0, err
	}
	for index, op := range ops {
		current, err = applyOp(current, op)
		if err != nil {
			return 0, fmt.Errorf("operation %d: %w", index, err)
		}
	}
	s.recordActor(artifactID, actor)
	if err := s.replaceLiveText(ctx, artifactID, current); err != nil {
		return 0, err
	}
	return len(ops), nil
}

// ApplyReplace resolves a stored anchor against current text and replaces it as
// one server-side Yjs transaction.
func (s *Service) ApplyReplace(ctx context.Context, artifactID string, anchor model.Anchor, with string, actor model.Actor) error {
	current, err := s.Text(ctx, artifactID)
	if err != nil {
		return err
	}
	resolved := text.Reresolve(current, anchor)
	if resolved.Orphaned {
		return text.ErrTargetNotFound
	}
	updated := replaceRange(current, resolved.From, resolved.To, with)
	s.recordActor(artifactID, actor)
	return s.replaceLiveText(ctx, artifactID, updated)
}

// NamedVersion records the live text as a deliberately named immutable version.
func (s *Service) NamedVersion(ctx context.Context, artifactID, summary string, actor model.Actor) (model.Version, error) {
	state := s.room(artifactID)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.actors[actorKey(actor)] = actor

	var version model.Version
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var open bool
		if err := tx.QueryRow(ctx, `
			select i.closed_at is null
			from artifacts a join issues i on i.key = a.issue_key
			where a.id = $1 for update
		`, artifactID).Scan(&open); err != nil {
			return fmt.Errorf("lock document artifact: %w", err)
		}
		if !open {
			return ErrIssueClosed
		}
		markdown, err := s.Text(ctx, artifactID)
		if err != nil {
			return err
		}
		version, err = writeVersion(ctx, tx, artifactID, markdown, true, new(summary), actorSlice(state.actors))
		return err
	})
	if err == nil {
		state.actors = make(map[string]model.Actor)
	}
	return version, err
}

func (s *Service) CompactAll(ctx context.Context, keep int) error {
	rows, err := s.store.Pool.Query(ctx, `select id::text from artifacts where kind = 'doc'`)
	if err != nil {
		return fmt.Errorf("list document rooms: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var artifactID string
		if err := rows.Scan(&artifactID); err != nil {
			return fmt.Errorf("scan document room: %w", err)
		}
		if _, err := s.persistence.Compact(ctx, artifactID, keep); err != nil {
			return fmt.Errorf("compact document %s: %w", artifactID, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list document rooms: %w", err)
	}
	return nil
}

func (s *Service) replaceLiveText(ctx context.Context, artifactID, markdown string) error {
	if err := s.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		content := doc.GetText("content")
		length := content.Len()
		transact(func(transaction *crdt.Transaction) {
			if length > 0 {
				content.Delete(transaction, 0, length)
			}
			if markdown != "" {
				content.Insert(transaction, 0, markdown, nil)
			}
		})
	}); err != nil {
		return fmt.Errorf("apply live document edit: %w", err)
	}
	return nil
}

func (s *Service) authorize(r *http.Request) (websocket.ConnectionConfig, bool) {
	actor, err := s.requestActor(r)
	if err != nil {
		return websocket.ConnectionConfig{}, false
	}
	room := r.PathValue("room")
	if room == "" {
		room = path.Base(r.URL.Path)
	}
	open, err := s.issueOpen(r.Context(), room)
	if err != nil {
		return websocket.ConnectionConfig{}, false
	}
	connection, _ := r.Context().Value(connectionContextKey{}).(*connectionState)
	if connection == nil {
		return websocket.ConnectionConfig{}, false
	}
	connection.room = room
	connection.actor = actor
	connection.added = true
	s.addConnection(room, actor)
	return websocket.ConnectionConfig{ReadOnly: !open}, true
}

func (s *Service) requestActor(r *http.Request) (model.Actor, error) {
	if authorization := strings.TrimSpace(r.Header.Get("Authorization")); authorization != "" {
		if s.agentToken == "" || authorization != "Bearer "+s.agentToken {
			return model.Actor{}, errors.New("invalid document bearer token")
		}
		var actor model.Actor
		if err := json.Unmarshal([]byte(r.Header.Get("X-Dispatch-Actor")), &actor); err != nil {
			return model.Actor{}, fmt.Errorf("decode document bearer actor: %w", err)
		}
		if actor.Kind != "session" || strings.TrimSpace(actor.ID) == "" {
			return model.Actor{}, errors.New("document bearer requires a session actor")
		}
		return actor, nil
	}
	if s.identity == nil {
		return model.Actor{}, errors.New("document identity required")
	}
	login, err := s.identity.Login(r)
	if err != nil {
		return model.Actor{}, err
	}
	return model.Actor{Kind: "user", ID: login}, nil
}

func (s *Service) allowInject(ctx context.Context, info websocket.InjectInfo) error {
	open, err := s.issueOpen(ctx, info.Room)
	if err != nil {
		return err
	}
	if !open {
		return ErrIssueClosed
	}
	return nil
}

func (s *Service) onLoadDocument(_ context.Context, room string, doc *crdt.Doc) error {
	doc.OnUpdate(func(_ []byte, _ any) {
		s.scheduleSettle(room)
	})
	return nil
}

func (s *Service) scheduleSettle(room string) {
	state := s.room(room)
	state.mu.Lock()
	if state.settle != nil {
		state.settle.Stop()
	}
	state.settle = time.AfterFunc(s.settle, func() { s.settleRoom(room) })
	state.mu.Unlock()
}

func (s *Service) settleRoom(room string) {
	state := s.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()

	markdown, err := s.Text(context.Background(), room)
	if err != nil {
		return
	}
	tx, err := s.store.Pool.Begin(context.Background())
	if err != nil {
		return
	}
	defer tx.Rollback(context.Background())
	var issueKey, artifactName string
	var open bool
	if err := tx.QueryRow(context.Background(), `
		select a.issue_key, a.name, i.closed_at is null
		from artifacts a join issues i on i.key = a.issue_key
		where a.id = $1 for update
	`, room).Scan(&issueKey, &artifactName, &open); err != nil || !open {
		return
	}
	latest, err := latestVersion(context.Background(), tx, room)
	if err != nil || latest.markdown == markdown {
		return
	}
	authors := actorSlice(state.actors)
	if len(authors) == 0 {
		return
	}
	version, err := writeVersion(context.Background(), tx, room, markdown, false, nil, authors)
	if err != nil {
		return
	}
	if err := s.reresolveAnchors(context.Background(), tx, room, markdown); err != nil {
		return
	}
	event, err := s.events.Append(context.Background(), tx, model.Event{
		IssueKey: issueKey,
		Type:     "artifact.version",
		Actor:    authors[0],
		Payload:  map[string]any{"artifact_id": room, "name": artifactName, "version": version},
	})
	if err != nil {
		return
	}
	if err := tx.Commit(context.Background()); err != nil {
		return
	}
	state.actors = make(map[string]model.Actor)
	s.events.Publish(event)
}

func (s *Service) reresolveAnchors(ctx context.Context, tx pgx.Tx, artifactID, markdown string) error {
	for _, target := range []struct {
		table string
		where string
	}{
		{table: "asks", where: "state = 'open'"},
		{table: "comments", where: "not resolved"},
	} {
		rows, err := tx.Query(ctx, fmt.Sprintf(`
			select id::text, anchor from %s
			where anchor is not null and anchor->>'artifact_id' = $1 and %s
		`, target.table, target.where), artifactID)
		if err != nil {
			return fmt.Errorf("list %s anchors: %w", target.table, err)
		}
		for rows.Next() {
			var id string
			var encoded []byte
			if err := rows.Scan(&id, &encoded); err != nil {
				rows.Close()
				return fmt.Errorf("scan %s anchor: %w", target.table, err)
			}
			var anchor model.Anchor
			if err := json.Unmarshal(encoded, &anchor); err != nil {
				rows.Close()
				return fmt.Errorf("decode %s anchor: %w", target.table, err)
			}
			encoded, err = json.Marshal(text.Reresolve(markdown, anchor))
			if err != nil {
				rows.Close()
				return fmt.Errorf("encode %s anchor: %w", target.table, err)
			}
			if _, err := tx.Exec(ctx, fmt.Sprintf(`update %s set anchor = $2 where id = $1`, target.table), id, encoded); err != nil {
				rows.Close()
				return fmt.Errorf("update %s anchor: %w", target.table, err)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("list %s anchors: %w", target.table, err)
		}
		rows.Close()
	}
	return nil
}

func (s *Service) issueOpen(ctx context.Context, artifactID string) (bool, error) {
	var open bool
	if err := s.store.Pool.QueryRow(ctx, `
		select i.closed_at is null
		from artifacts a join issues i on i.key = a.issue_key
		where a.id = $1
	`, artifactID).Scan(&open); err != nil {
		return false, fmt.Errorf("check document issue: %w", err)
	}
	return open, nil
}

func (s *Service) room(name string) *roomState {
	value, _ := s.rooms.LoadOrStore(name, &roomState{
		actors: make(map[string]model.Actor),
	})
	return value.(*roomState)
}

func (s *Service) recordActor(room string, actor model.Actor) {
	state := s.room(room)
	state.mu.Lock()
	state.actors[actorKey(actor)] = actor
	state.mu.Unlock()
}

func (s *Service) addConnection(room string, actor model.Actor) {
	state := s.room(room)
	state.mu.Lock()
	state.actors[actorKey(actor)] = actor
	state.mu.Unlock()
}

func (s *Service) removeConnection(room string, actor model.Actor) {
	state := s.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
	delete(state.actors, actorKey(actor))
}

func latestVersion(ctx context.Context, tx pgx.Tx, artifactID string) (struct {
	model.Version
	markdown string
}, error) {
	var version struct {
		model.Version
		markdown string
	}
	var authors []byte
	if err := tx.QueryRow(ctx, `
		select number, named, summary, authors, created_at, markdown
		from artifact_versions where artifact_id = $1
		order by number desc limit 1
	`, artifactID).Scan(&version.Number, &version.Named, &version.Summary, &authors, &version.CreatedAt, &version.markdown); err != nil {
		return version, fmt.Errorf("read latest document version: %w", err)
	}
	if err := json.Unmarshal(authors, &version.Authors); err != nil {
		return version, fmt.Errorf("decode latest document version authors: %w", err)
	}
	return version, nil
}

func writeVersion(ctx context.Context, tx pgx.Tx, artifactID, markdown string, named bool, summary *string, authors []model.Actor) (model.Version, error) {
	encodedAuthors, err := json.Marshal(authors)
	if err != nil {
		return model.Version{}, fmt.Errorf("encode document version authors: %w", err)
	}
	var version model.Version
	var authorsRaw []byte
	if err := tx.QueryRow(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors, named, summary)
		select $1, coalesce(max(number), 0) + 1, $2, $3, $4, $5
		from artifact_versions where artifact_id = $1
		returning number, named, summary, authors, created_at
	`, artifactID, markdown, encodedAuthors, named, summary).Scan(
		&version.Number, &version.Named, &version.Summary, &authorsRaw, &version.CreatedAt,
	); err != nil {
		return model.Version{}, fmt.Errorf("write document version: %w", err)
	}
	if err := json.Unmarshal(authorsRaw, &version.Authors); err != nil {
		return model.Version{}, fmt.Errorf("decode document version authors: %w", err)
	}
	return version, nil
}

func applyOp(markdown string, op model.EditOp) (string, error) {
	switch op.Op {
	case "replace":
		if op.Find == "" {
			return "", invalidOp("find")
		}
		from, to, err := text.Resolve(markdown, op.Find, op.Occurrence)
		if err != nil {
			return "", err
		}
		return replaceRange(markdown, from, to, op.With), nil
	case "delete":
		if op.Find == "" {
			return "", invalidOp("find")
		}
		from, to, err := text.Resolve(markdown, op.Find, op.Occurrence)
		if err != nil {
			return "", err
		}
		return replaceRange(markdown, from, to, ""), nil
	case "insert":
		if op.Markdown == "" {
			return "", invalidOp("markdown")
		}
		if (op.After == "" && op.Before == "") || (op.After != "" && op.Before != "") {
			return "", invalidOp("after or before")
		}
		anchor := op.After
		after := anchor != ""
		if !after {
			anchor = op.Before
		}
		position, err := insertPosition(markdown, anchor, after, op.Occurrence)
		if err != nil {
			return "", err
		}
		return replaceRange(markdown, position, position, op.Markdown), nil
	default:
		return "", invalidOp("op")
	}
}

func insertPosition(markdown, anchor string, after bool, occurrence *int) (int, error) {
	switch anchor {
	case "start":
		return 0, nil
	case "end":
		return text.Len16(markdown), nil
	}
	if title, ok := strings.CutPrefix(anchor, "heading:"); ok {
		if title == "" {
			return 0, invalidOp("heading")
		}
		return headingPosition(markdown, title, after, occurrence)
	}
	from, to, err := text.Resolve(markdown, anchor, occurrence)
	if err != nil {
		return 0, err
	}
	if after {
		return to, nil
	}
	return from, nil
}

func headingPosition(markdown, title string, after bool, occurrence *int) (int, error) {
	position := 0
	var positions []int
	for _, line := range strings.SplitAfter(markdown, "\n") {
		trimmed := strings.TrimSuffix(line, "\n")
		heading := strings.TrimLeft(trimmed, " ")
		if strings.HasPrefix(heading, "#") {
			marker := strings.TrimLeft(heading, "#")
			if len(marker) < len(heading) && strings.HasPrefix(marker, " ") && strings.TrimSpace(marker) == title {
				if after {
					positions = append(positions, position+text.Len16(line))
				} else {
					positions = append(positions, position)
				}
			}
		}
		position += text.Len16(line)
	}
	if len(positions) == 0 {
		return 0, text.ErrTargetNotFound
	}
	if occurrence != nil {
		if *occurrence < 0 || *occurrence >= len(positions) {
			return 0, text.ErrTargetNotFound
		}
		return positions[*occurrence], nil
	}
	if len(positions) > 1 {
		return 0, &text.ErrTargetAmbiguous{}
	}
	return positions[0], nil
}

func replaceRange(markdown string, from, to int, with string) string {
	return text.Slice16(markdown, 0, from) + with + text.Slice16(markdown, to, text.Len16(markdown))
}

func actorKey(actor model.Actor) string {
	return actor.Kind + "\x00" + actor.ID
}

func actorSlice(actors map[string]model.Actor) []model.Actor {
	keys := make([]string, 0, len(actors))
	for key := range actors {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]model.Actor, 0, len(keys))
	for _, key := range keys {
		result = append(result, actors[key])
	}
	return result
}

func invalidOp(field string) error {
	return fmt.Errorf("invalid document operation field %q", field)
}

func (s *Service) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	if tx, ok := txFromContext(ctx); ok {
		return fn(tx)
	}
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin document transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit document transaction: %w", err)
	}
	return nil
}

var _ API = (*Service)(nil)
