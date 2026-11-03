package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

const (
	// maxArtifactBlobSize is the file store's own bound, so an upload this route takes is one
	// the store accepts and serves back.
	maxArtifactBlobSize      = files.MaxObjectSize
	maxDocumentMarkdownBytes = pmdoc.MaxDocumentBytes // A Markdown document, and what a write may grow one to (docs' refuseGrowth); maxJSONRequestBytes bounds an issue's spec and every edit the same way (LEGION-465).
	// maxNewArtifactKeyBytes bounds, in bytes, a new artifact's name and the reference key
	// `<owner>/<slug>` that name gives it (newArtifactNameRefusal, newArtifactKeyRefusal).
	// artifacts_ref_key_key is a B-tree index on ref_key, and a Postgres B-tree entry holds at
	// most 2704 bytes: an 8-byte tuple header, then the value's 4-byte length word and its bytes,
	// so incompressible text of 2692 bytes fits and one byte more fails the insert (SQLSTATE
	// 54000). No index holds the name, but it is held to the same bytes: its slug is the part of
	// the key a caller writes, so one number names what a new artifact may be called. An artifact
	// already stored past it, under a key Postgres compressed into the index, keeps its name and
	// key: an upload naming it stores a version as before.
	maxNewArtifactKeyBytes = 2692
)

// artifactColumns is the canonical artifact column list scanArtifact reads.
const artifactColumns = `id::text, issue_key, project_key, session_id, ref_key, slug, name, kind, is_primary, created_by, created_at`

func (s *server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifacts, err := s.loadArtifacts(r.Context(), s.deps.Store.Pool, r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	attachApprovalTrees(wantsDisplayTrees(r), artifacts)
	WriteJSON(w, http.StatusOK, artifacts)
}

type artifactUploadInput struct {
	name        string
	content     []byte
	contentType string
	summary     string
	supplied    *model.Actor
	inline      bool
	blank       bool
}

// artifactTarget is the owner an upload or a slug lookup names: an issue (IssueKey), a project
// (Project alone), or an agent's conversation (Session alone), which holds files and images only.
// Exactly one of the three is set; every reader tests Session first, then IssueKey, and
// agent_artifacts.go holds what a conversation does differently.
type artifactTarget struct {
	IssueKey *string
	Project  string
	Session  string
}

// refPrefix is what the target's artifacts' ref_key holds before `/<slug>`.
func (target artifactTarget) refPrefix() string {
	if target.Session != "" {
		return agentOwnerKey(target.Session)
	}
	if target.IssueKey != nil {
		return *target.IssueKey
	}
	return target.Project
}

type jsonArtifactUpload struct {
	Name    string          `json:"name"`
	Content *string         `json:"content"`
	Primary json.RawMessage `json:"primary"`
	Summary string          `json:"summary"`
	Actor   *model.Actor    `json:"actor"`
}

func (s *server) uploadArtifact(w http.ResponseWriter, r *http.Request) {
	issueKey := r.PathValue("key")
	s.uploadArtifactFor(w, r, artifactTarget{IssueKey: &issueKey})
}

// issueUploadRoute is the upload to an issue, which refuseUploadToAClosedIssue guards.
const issueUploadRoute = http.MethodPost + " /api/v1/issues/{key}/artifacts"

// refuseUploadToAClosedIssue answers an upload to an issue that is closed, or missing, with the
// refusal its handler would make (requireOpenOwnerStatus) before anything spools or parses the
// upload's body: the keyed layer spools a keyed body whole to disk before its handler runs
// (readKeyedBody), and the multipart parser spools an unkeyed one whole before the handler reaches
// the issue, so an upload to a closed issue would otherwise be written out in full only to be
// refused. The body is still read off the connection, and discarded, before the refusal is written
// (drainBeforeAnswer), so a client that sends it whole before it reads the answer gets the
// answer. register mounts it ahead of the keyed layer, so the refusal claims no key. The handler's
// locked check still decides an issue that closes meanwhile; this one only spares the spool. A
// caller that resolves to nobody is left to the route's own 401, and a malformed Idempotency-Key
// to the keyed layer's 400, which spools no body either and refuses the request's form before
// anything is looked up.
//
// The refusal stands in for the handler, so it answers only a request whose handler would run. A
// repeat under a key the keyed layer answers itself (requestKeyAnswersItself) - its first upload's
// stored answer, a first upload still running, one that committed and lost its answer - goes on to
// that layer whatever the issue's state now: an upload that landed before the issue closed is
// replayed its 201, and one still running is told KEY_IN_FLIGHT, unspooled, until the first
// answers.
func (s *server) refuseUploadToAClosedIssue(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestKey, keyed, err := requestKeyHeader(r)
		if keyed && err != nil {
			handler(w, r)
			return
		}
		actor, human, err := s.optionalActor(r)
		if err != nil {
			handler(w, r)
			return
		}
		// The caller is resolved once a request: the key layer and the handler read this.
		r = r.WithContext(context.WithValue(r.Context(), resolvedCallerKey{}, resolvedCaller{actor: actor, human: human}))
		refusal := s.refuseClosedOwnerBeforeResolving(r.Context(), issueOwner(r.PathValue("key")))
		if refusal == nil {
			handler(w, r)
			return
		}
		// Only the issue's own refusal (closed, missing) stands aside for the key: a check that
		// failed is answered as it always was.
		var ownerRefusal *apiError
		if keyed && errors.As(refusal, &ownerRefusal) && ownerRefusal.status < http.StatusInternalServerError {
			answers, err := s.requestKeyAnswersItself(r.Context(), requestKeyLogin(actor, human), requestKey)
			if err != nil {
				// As the keyed layer answers its own failed lookup: under the key, which nothing has
				// claimed, so the client asks again under it.
				w.Header().Set(contracts.IdempotencyKeyHeader, requestKey)
				s.writeHandlerError(w, err)
				return
			}
			if answers {
				handler(w, r)
				return
			}
		}
		s.writeHandlerError(w, refusal)
	}
}

func (s *server) uploadArtifactFor(w http.ResponseWriter, r *http.Request, target artifactTarget) {
	r.Body = http.MaxBytesReader(w, r.Body, maxArtifactBlobSize+(1<<20))
	requestType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		writeError(w, "ARTIFACT_INPUT", http.StatusBadRequest, "invalid artifact input type")
		return
	}

	var input artifactUploadInput
	var ok bool
	switch requestType {
	case "application/json":
		input, ok = s.jsonArtifactUpload(w, r)
	case "multipart/form-data":
		input, ok = s.multipartArtifactUpload(w, r)
	default:
		writeError(w, "ARTIFACT_INPUT", http.StatusBadRequest, "artifact input must be JSON or multipart")
		return
	}
	if !ok {
		return
	}
	actor, ok := s.requireActor(w, r, input.supplied)
	if !ok {
		return
	}
	if input.name == "" {
		writeError(w, "INVALID_ARTIFACT", http.StatusBadRequest, "artifact name is required")
		return
	}
	if input.inline && input.blank {
		writeError(w, "ARTIFACT_INPUT", http.StatusBadRequest, "inline artifact content is required")
		return
	}
	if len(input.content) > maxArtifactBlobSize {
		s.writeHandlerError(w, errUploadTooLarge)
		return
	}
	mediaType, _, err := mime.ParseMediaType(input.contentType)
	if err != nil {
		writeError(w, "INVALID_ARTIFACT", http.StatusBadRequest, "invalid artifact content type")
		return
	}
	kind, storedType := artifactKind(mediaType, input.contentType, input.content)
	if kind == "doc" && target.Session != "" {
		writeError(w, "ARTIFACT_INPUT", http.StatusBadRequest, agentOwnsFilesOnly)
		return
	}
	if kind == "doc" && len(input.content) > maxDocumentMarkdownBytes {
		writeError(w, "CAP_EXCEEDED", http.StatusRequestEntityTooLarge, "a markdown document is at most 1 MiB; a larger file is stored as a binary artifact under another content type")
		return
	}
	// A markdown document is stored as text. Its inline content was read as a JSON string, which
	// holds no byte that is not UTF-8 and was read for U+0000, so only a multipart file reaches here
	// holding either. Any other file is stored as bytes, beside its part's Content-Type as text; the
	// document path stores no part header.
	if kind == "doc" && !text.StorableBytes(input.content) {
		s.writeHandlerError(w, unstorableText("file", string(input.content)))
		return
	}
	if kind != "doc" {
		if refusal := unstorableText("file Content-Type", input.contentType); refusal != nil {
			s.writeHandlerError(w, refusal)
			return
		}
	}
	input.contentType = storedType
	s.storeArtifact(w, r, input, actor, kind, target)
}

func (s *server) jsonArtifactUpload(w http.ResponseWriter, r *http.Request) (artifactUploadInput, bool) {
	var body jsonArtifactUpload
	var read bytes.Buffer
	decoder := json.NewDecoder(io.TeeReader(r.Body, &read))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			s.writeHandlerError(w, errUploadTooLarge)
		} else {
			writeError(w, "INVALID_JSON", http.StatusBadRequest, "invalid JSON body")
		}
		return artifactUploadInput{}, false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, "INVALID_JSON", http.StatusBadRequest, "request body must contain one JSON value")
		return artifactUploadInput{}, false
	}
	if refusal := unstorableJSON("", read.Bytes()); refusal != nil {
		s.writeHandlerError(w, refusal)
		return artifactUploadInput{}, false
	}
	if body.Primary != nil {
		writeError(w, "ARTIFACT_INPUT", http.StatusBadRequest, "primary is fixed at issue creation")
		return artifactUploadInput{}, false
	}
	blank := body.Content == nil || strings.TrimSpace(*body.Content) == ""
	var content []byte
	if body.Content != nil {
		content = []byte(*body.Content)
	}
	return artifactUploadInput{
		name:        strings.TrimSpace(body.Name),
		content:     content,
		contentType: "text/markdown; charset=utf-8",
		summary:     strings.TrimSpace(body.Summary),
		supplied:    body.Actor,
		inline:      true,
		blank:       blank,
	}, true
}

func (s *server) multipartArtifactUpload(w http.ResponseWriter, r *http.Request) (artifactUploadInput, bool) {
	// Only the size limits are 413: the body past maxArtifactBlobSize and a megabyte of fields
	// (uploadArtifactFor's MaxBytesReader), or fields past what the parser holds in memory. A file
	// the server cannot spool to its temporary directory is the server's failure, 500. Any other
	// refusal is a body the parser cannot read, answered with its reason.
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		var spool *fs.PathError
		switch {
		case errors.As(err, &tooLarge):
			s.writeHandlerError(w, errUploadTooLarge)
		case errors.Is(err, multipart.ErrMessageTooLarge):
			writeError(w, "CAP_EXCEEDED", http.StatusRequestEntityTooLarge, "the upload's form fields are too large to read")
		case errors.As(err, &spool):
			s.writeHandlerError(w, fmt.Errorf("spool multipart upload: %w", err))
		default:
			writeError(w, "ARTIFACT_INPUT", http.StatusBadRequest, "invalid multipart body: "+err.Error())
		}
		return artifactUploadInput{}, false
	}
	// The parser spools a file part past a megabyte to a temporary file. net/http removes the
	// files of the request it handed its handler, but this is a copy (the access log and the
	// transaction tracking each wrap it with WithContext), so the server never sees them; left
	// alone, every such upload kept its file on the pod's disk until the pod's ephemeral storage
	// ran out. The content is read into memory below, so the files go when this returns, stored
	// or refused.
	defer func() {
		if err := r.MultipartForm.RemoveAll(); err != nil {
			slog.Error("dispatch: remove a multipart upload's spooled files", "error", err)
		}
	}()
	if refusal := unstorableForm(r.MultipartForm.Value); refusal != nil {
		s.writeHandlerError(w, refusal)
		return artifactUploadInput{}, false
	}
	var supplied *model.Actor
	if raw := r.FormValue("actor"); raw != "" {
		var actor model.Actor
		if err := json.Unmarshal([]byte(raw), &actor); err != nil {
			writeError(w, "INVALID_JSON", http.StatusBadRequest, "invalid multipart actor")
			return artifactUploadInput{}, false
		}
		if refusal := unstorableJSON("actor", []byte(raw)); refusal != nil {
			s.writeHandlerError(w, refusal)
			return artifactUploadInput{}, false
		}
		supplied = &actor
	}
	if _, exists := r.MultipartForm.Value["primary"]; exists {
		writeError(w, "ARTIFACT_INPUT", http.StatusBadRequest, "primary is fixed at issue creation")
		return artifactUploadInput{}, false
	}
	name := strings.TrimSpace(r.FormValue("name"))
	file, header, err := r.FormFile("file")
	if len(r.MultipartForm.Value["content"]) > 0 {
		if err == nil {
			_ = file.Close()
		}
		writeError(w, "ARTIFACT_INPUT", http.StatusBadRequest, "provide either a file or inline JSON content")
		return artifactUploadInput{}, false
	}
	if err != nil {
		writeError(w, "INVALID_ARTIFACT", http.StatusBadRequest, "artifact file is required")
		return artifactUploadInput{}, false
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxArtifactBlobSize+1))
	if err != nil {
		s.writeHandlerError(w, err)
		return artifactUploadInput{}, false
	}
	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = mime.TypeByExtension(path.Ext(name))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return artifactUploadInput{
		name:        name,
		content:     content,
		contentType: contentType,
		summary:     strings.TrimSpace(r.FormValue("summary")),
		supplied:    supplied,
	}, true
}

// writeDocumentVersion stores the document-version row uploads write directly, returning the same
// decoded version the route returns to its caller. Uploads keep this separate from
// docs.writeVersionTx because they own their artifact event and approval move. stamp is what the
// markdown was rendered from (docs.Ledger.RenderStampOf), stored with the head this transaction's
// own write left, so a text read of a document still standing there serves the stored markdown
// rather than rendering the document again.
func writeDocumentVersion(ctx context.Context, tx pgx.Tx, artifactID string, number int, markdown string, authors []model.Actor, summary *string, stamp docs.RenderStamp) (model.Version, error) {
	encodedAuthors, err := json.Marshal(authors)
	if err != nil {
		return model.Version{}, fmt.Errorf("encode document version authors: %w", err)
	}
	var version model.Version
	var authorsRaw []byte
	if err := tx.QueryRow(ctx, docs.RenderStampHead+`
		insert into artifact_versions as v (artifact_id, number, markdown, authors, named, summary, doc_update_version, `+docs.RenderStampColumns+`)
		values ($1, $2, $3, $4, $5, $6, coalesce((select max(version) from doc_updates where artifact_id = $1), 0),
			`+docs.RenderStampValues+`, $7, $8)
		returning number, named, summary, authors, created_at, `+model.VersionSize+`
	`, artifactID, number, markdown, encodedAuthors, summary != nil, summary, stamp.Token, stamp.Renderer).Scan(
		&version.Number, &version.Named, &version.Summary, &authorsRaw, &version.CreatedAt, &version.Size,
	); err != nil {
		return model.Version{}, fmt.Errorf("write document version: %w", err)
	}
	if err := json.Unmarshal(authorsRaw, &version.Authors); err != nil {
		return model.Version{}, fmt.Errorf("decode document version authors: %w", err)
	}
	return version, nil
}

// recordDocumentTaskProgress records the task count of the document a write just stored.
// readBack is the rendering that write stored, read back by its own parse (docs'
// Ledger.ReadBackOf): the count reads that tree rather than parsing the same markdown again, and
// parses markdown itself when the write left none.
func recordDocumentTaskProgress(ctx context.Context, tx pgx.Tx, artifactID string, readBack *pmdoc.Node, markdown string) error {
	if readBack != nil {
		return docs.RecordTaskProgress(ctx, tx, artifactID, readBack)
	}
	return docs.RecordTaskProgressMarkdown(ctx, tx, artifactID, markdown)
}

func (s *server) storeArtifact(
	w http.ResponseWriter,
	r *http.Request,
	input artifactUploadInput,
	actor model.Actor,
	kind string,
	target artifactTarget,
) {
	checksum := sha256.Sum256(input.content)
	sha := hex.EncodeToString(checksum[:])
	// A file's object is written before the transaction opens, once the refusals the transaction
	// would make have been made (putUploadedFile, artifact_files.go).
	if !s.putUploadedFile(w, r, kind, sha, input, target) {
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var issueStatus *string
	var project, session *string
	switch {
	case target.Session != "":
		session = &target.Session
		if err := lockAgentArtifacts(r.Context(), tx, target.Session); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	case target.IssueKey != nil:
		issueStatus, err = s.requireOpenOwnerStatus(r.Context(), tx, issueOwner(*target.IssueKey))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := tx.QueryRow(r.Context(), `select project_key from issues where key = $1`, *target.IssueKey).Scan(&project); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	default:
		loaded, err := s.loadProject(r.Context(), tx, target.Project)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		project = &loaded.Key
		if err := lockProjectArtifacts(r.Context(), tx, loaded.Key); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	var artifact model.Artifact
	var created bool
	artifact, err = s.findArtifactByNameOrSlug(r.Context(), tx, target, input.name)
	if errors.Is(err, pgx.ErrNoRows) {
		created = true
		if refusal := newArtifactNameRefusal(input.name); refusal != nil {
			s.writeHandlerError(w, refusal)
			return
		}
		slug, err := s.nextArtifactSlug(r.Context(), tx, target, input.name)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if refusal := newArtifactKeyRefusal(target, slug); refusal != nil {
			s.writeHandlerError(w, refusal)
			return
		}
		actorJSON, err := encodeJSON(actor)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		artifact, err = scanArtifact(tx.QueryRow(r.Context(), `
			insert into artifacts (issue_key, project_key, session_id, slug, name, kind, is_primary, created_by)
			values ($1, $2, $3, $4, $5, $6, false, $7)
			returning `+artifactColumns,
			target.IssueKey, project, session, slug, input.name, kind, actorJSON))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	} else if err != nil {
		s.writeHandlerError(w, err)
		return
	} else if artifact.Kind != kind {
		s.writeHandlerError(w, artifactKindMismatch(input.name, artifact, kind))
		return
	}
	// A project document's owner row is the artifact itself, and until here this transaction
	// has not locked it: the issue branch above locks its issue, but this one only read its
	// project. Every writer that takes the owner row at all takes it before the room lock -
	// the durable writers never take it - and the event this upload appends takes it after the
	// document write has taken the room. Without this line the upload would run room -> owner
	// against a settlement's owner -> room, and Postgres would break the cycle with a 500.
	if target.IssueKey == nil && target.Session == "" && !created {
		if err := s.requireOpenOwner(r.Context(), tx, ownerForArtifact(artifact)); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}

	var nextNumber int
	if err := tx.QueryRow(r.Context(), `select coalesce(max(number), 0) + 1 from artifact_versions where artifact_id = $1`, artifact.ID).Scan(&nextNumber); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var version model.Version
	var summaryValue any
	var documentSummary *string
	if input.summary != "" {
		summaryValue = input.summary
		documentSummary = &input.summary
	}
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	var documentMarkdown string
	// documentReadBack is the rendering this write stored, read back by its own parse (docs'
	// Ledger.ReadBackOf), which the task count and the advice's blocks read in place of parsing
	// the same markdown again (recordDocumentTaskProgress, documentBlocksFor).
	var documentReadBack *pmdoc.Node
	var documentChanges model.ReferenceChanges
	var movedEvents []model.Event
	if kind == "doc" {
		if created {
			documentMarkdown, err = s.deps.Docs.SeedText(documentCtx, artifact.ID, string(input.content), actor)
		} else {
			documentMarkdown, err = s.deps.Docs.ReplaceText(documentCtx, artifact.ID, string(input.content), actor)
		}
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		version, err = writeDocumentVersion(r.Context(), tx, artifact.ID, nextNumber, documentMarkdown, []model.Actor{actor}, documentSummary, ledger.RenderStampOf(artifact.ID, documentMarkdown))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		// The version credits the uploader alone, and holds the upload's write to the document,
		// which clears the credit of every edit that write read when it changed the document.
		ledger.WroteVersion(artifact.ID, version)
		documentReadBack = ledger.ReadBackOf(artifact.ID, documentMarkdown)
		// The version row exists now, so the count names it (LEGION-542).
		err = recordDocumentTaskProgress(r.Context(), tx, artifact.ID, documentReadBack, documentMarkdown)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		documentChanges, err = s.deps.Docs.IndexDocumentReferences(documentCtx, artifact.ID, version.Number, documentMarkdown)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	} else {
		authors, err := encodeJSON([]model.Actor{actor})
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		size := len(input.content)
		content := input.content
		if s.deps.Files != nil {
			content = nil
		}
		var versionAuthors []byte
		if err := tx.QueryRow(r.Context(), `
			insert into artifact_versions (artifact_id, number, content, mime, size, sha256, authors, named, summary)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			returning number, named, summary, authors, created_at, size, mime, sha256
		`, artifact.ID, nextNumber, content, input.contentType, size, sha, authors, input.summary != "", summaryValue).Scan(
			&version.Number, &version.Named, &version.Summary, &versionAuthors, &version.CreatedAt,
			&version.Size, &version.MIME, &version.SHA256,
		); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := json.Unmarshal(versionAuthors, &version.Authors); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	var diff *string
	if !created && kind == "doc" {
		// This route writes its version itself rather than through the document service, so its
		// open approval request follows that version just as every other version write does.
		movedEvents, err = docs.MoveApprovalAsk(
			r.Context(), tx, s.deps.Events, artifact.ID, version, actor, s.deps.ServerURL,
		)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		// It carries the document's newest approval when its text is the approved text again and
		// its decision blocks are as their rows have them, as every version the document service
		// writes does (docs.carryApproval): an exact undo uploaded is the approved document.
		carried, err := docs.CarryRestoredApproval(
			r.Context(), tx, s.deps.Events, artifact.ID, version.Number, documentMarkdown, documentReadBack, s.deps.ServerURL,
		)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		movedEvents = append(movedEvents, carried...)
		diff, err = s.namedVersionDiff(r.Context(), tx, artifact.ID, version)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	// An uploaded document's body cites nodes whose rows carry a backlink count, exactly as a
	// message or comment body does, so both payload shapes name what the upload moved.
	eventType := "artifact.version"
	payload := docs.ArtifactVersionEventPayload(artifact.ID, artifact.Name, version, diff, documentChanges)
	if created {
		eventType = "artifact.created"
		artifact.Versions = []model.Version{version}
		payload = artifactCreatedEventPayload(artifact, documentChanges)
	}
	published := movedEvents
	if target.ownsEvents() {
		event, err := s.appendEvent(r.Context(), tx, ownerForArtifact(artifact).event(eventType, actor, payload))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if kind == "doc" {
			if err := refs.Stamp(r.Context(), tx, "artifact", artifact.ID, event.ID); err != nil {
				s.writeHandlerError(w, err)
				return
			}
		}
		published = append(published, event)
	}
	// The approval this upload's own version superseded, read in its transaction as the edit
	// route reads it: an upload to an existing document always writes a version, so a
	// replacement of an approved spec closes its design gate as an edit does, unless the restore
	// above carried the approval to it. A new document has no review of its own to supersede.
	var superseded *supersededApproval
	if kind == "doc" && !created {
		superseded = s.supersededApprovalAdvice(
			r.Context(), tx, "POST /api/v1/issues/{key}/artifacts", artifactIssueKey(artifact), artifact.ID, version.Number,
		)
	}
	var advice *writeAdvice
	if target.IssueKey != nil && issueStatus != nil {
		advice = s.writeAdvice(
			r.Context(), tx, "POST /api/v1/issues/{key}/artifacts", *target.IssueKey, actor, "", *issueStatus,
		)
	}
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if kind == "doc" {
		// The seeded or replaced text was written inside this transaction, which suppresses
		// the live settlement the edits path relies on; queue the closer now so the
		// document's ask blocks are indexed and its block ids repaired.
		s.deps.Docs.ScheduleSettlement(artifact.ID)
	}
	s.publish(published...)
	var blocks *documentBlocks
	if kind == "doc" {
		blocks = documentBlocksFor(documentReadBack, documentMarkdown)
	}
	if advice != nil {
		advice.documentBlocks = blocks
		advice.SupersededApproval = superseded
	}
	responsePayload := map[string]any{"artifact": artifact, "version": version}
	if target.IssueKey != nil {
		WriteJSON(w, http.StatusCreated, withAdvice(responsePayload, advice))
	} else if blocks != nil || superseded != nil {
		WriteJSON(w, http.StatusCreated, withRawAdvice(responsePayload, documentWriteAdvice{
			documentBlocks: blocks, SupersededApproval: superseded,
		}))
	} else {
		WriteJSON(w, http.StatusCreated, responsePayload)
	}
}

func (s *server) getArtifact(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The artifact detail is the artifact. Its inbound edges are the graph's to read
	// (`GET /api/v1/references?to=`, and `GET /api/v1/artifacts/{id}/references` for agents),
	// so this read does not pay for them.
	attachApprovalTrees(wantsDisplayTrees(r), []model.Artifact{artifact})
	WriteJSON(w, http.StatusOK, artifact)
}

func (s *server) getArtifactText(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, "NOT_DOCUMENT", http.StatusBadRequest, "artifact is not a document")
		return
	}
	markdown, token, err := s.deps.Docs.TextWithToken(r.Context(), artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The markdown and the version are two unsynchronised reads, in both directions: the live
	// markdown can be ahead of the version while browser text is unsettled, and a version minted
	// between the two reads can be ahead of the markdown. `token` is the route's concurrency
	// primitive; `version` says which version the document has, not which one this markdown is.
	version, err := latestVersionNumber(r.Context(), s.deps.Store.Pool, artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	answer := map[string]any{"markdown": markdown, "version": version, "token": token}
	if tree := displayTree(markdown); wantsDisplayTrees(r) && tree != nil {
		answer["markdown_tree"] = tree
	}
	WriteJSON(w, http.StatusOK, answer)
}

func (s *server) getArtifactBlocks(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, "NOT_DOCUMENT", http.StatusBadRequest, "artifact is not a document")
		return
	}
	blocks, err := s.deps.Docs.Blocks(r.Context(), artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	references, err := s.blockReferences(r.Context(), artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	for index := range blocks {
		counts := references[blocks[index].ID]
		for _, descendantID := range blocks[index].DescendantIDs {
			descendant := references[descendantID]
			counts.Comments += descendant.Comments
			counts.Asks += descendant.Asks
		}
		blocks[index].References = counts
	}
	WriteJSON(w, http.StatusOK, blocks)
}

func (s *server) getArtifactBlockPath(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, "NOT_DOCUMENT", http.StatusBadRequest, "artifact is not a document")
		return
	}
	path, err := s.deps.Docs.BlockPath(r.Context(), artifact.ID, r.PathValue("block_id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, path)
}

// blockReferences counts the comments and asks anchored directly to each block.
// getArtifactBlocks adds a table's descendant-cell counts before it serves the
// table block. The predicate is two conjuncts (is not null, then not the empty
// string) rather than nullif(...) is not null so the planner can use the partial
// comments_/asks_anchor_block_id_idx indexes.
func (s *server) blockReferences(ctx context.Context, artifactID string) (map[string]model.BlockReferences, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select anchor_block_id,
		       count(*) filter (where kind = 'comment'),
		       count(*) filter (where kind = 'ask')
		from (
			select anchor->>'block_id' as anchor_block_id, 'comment' as kind
			from comments
			where anchor->>'artifact_id' = $1 and anchor->>'block_id' is not null and anchor->>'block_id' <> ''
			union all
			select anchor->>'block_id' as anchor_block_id, 'ask' as kind
			from asks
			where anchor->>'artifact_id' = $1 and anchor->>'block_id' is not null and anchor->>'block_id' <> ''
		) anchored
		group by anchor_block_id
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("list block references: %w", err)
	}
	defer rows.Close()

	references := make(map[string]model.BlockReferences)
	for rows.Next() {
		var blockID string
		var comments, asks int
		if err := rows.Scan(&blockID, &comments, &asks); err != nil {
			return nil, fmt.Errorf("scan block references: %w", err)
		}
		references[blockID] = model.BlockReferences{Comments: comments, Asks: asks}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate block references: %w", err)
	}
	return references, nil
}

func (s *server) getArtifactVersion(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number < 1 {
		writeError(w, "INVALID_VERSION", http.StatusBadRequest, "version number must be positive")
		return
	}
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind == "doc" {
		var markdown string
		version, err := s.loadVersion(r.Context(), s.deps.Store.Pool, artifact.ID, number, &markdown)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		// A past version's text is read in the version mode: the attributes the server wrote
		// into that text are part of what that version said (pmdoc.DisplayVersion).
		answer := struct {
			Markdown     string          `json:"markdown"`
			MarkdownTree json.RawMessage `json:"markdown_tree,omitempty"`
			model.Version
		}{Markdown: markdown, Version: version}
		if wantsDisplayTrees(r) {
			answer.MarkdownTree = pmdoc.DisplayJSON(markdown, pmdoc.DisplayVersion)
		}
		WriteJSON(w, http.StatusOK, answer)
		return
	}
	var content []byte
	var contentType, sha *string
	var size *int64
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `
		select content, mime, sha256, size from artifact_versions where artifact_id = $1 and number = $2
	`, artifact.ID, number).Scan(&content, &contentType, &sha, &size); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.serveFileVersion(w, r, artifact.ID, number, content, contentType, sha, size)
}

// deref is the string a nullable column holds, or "".
func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// commitArtifactVersionEvent appends the artifact.version event of a version the transaction wrote
// and stamps the document's new mention edges with it (refs.Stamp), after the append so stamping
// never touches sequence allocation.
func (s *server) commitArtifactVersionEvent(
	ctx context.Context,
	tx pgx.Tx,
	eventOwner owner,
	actor model.Actor,
	artifactID, name string,
	written docs.VersionResult,
	diff *string,
) (model.Event, error) {
	event, err := s.appendEvent(ctx, tx, eventOwner.event(
		"artifact.version",
		actor,
		docs.ArtifactVersionEventPayload(artifactID, name, written.Version, diff, written.Changes),
	))
	if err != nil {
		return model.Event{}, err
	}
	if err := refs.Stamp(ctx, tx, "artifact", artifactID, event.ID); err != nil {
		return model.Event{}, err
	}
	return event, nil
}

func (s *server) createNamedVersion(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var input struct {
		Summary string       `json:"summary"`
		Actor   *model.Actor `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	summary := strings.TrimSpace(input.Summary)
	if summary == "" {
		writeError(w, "INVALID_VERSION", http.StatusBadRequest, "named versions require a summary")
		return
	}
	if err := validateArtifactRequestRef(r); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	artifact, err := s.loadArtifactForRequest(r.Context(), tx, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	eventOwner := ownerForArtifact(artifact)
	if err := s.requireOpenOwner(r.Context(), tx, eventOwner); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, "NOT_DOCUMENT", http.StatusBadRequest, "artifact is not a document")
		return
	}
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	named, err := s.deps.Docs.NamedVersion(documentCtx, artifact.ID, summary, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	version := named.Version
	diff, err := s.namedVersionDiff(r.Context(), tx, artifact.ID, version)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.commitArtifactVersionEvent(r.Context(), tx, eventOwner, actor, artifact.ID, artifact.Name, named, diff)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	WriteJSON(w, http.StatusCreated, version)
}

func (s *server) editArtifact(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var input struct {
		Ops          []model.EditOp          `json:"ops"`
		Summary      string                  `json:"summary"`
		Precondition *model.EditPrecondition `json:"precondition"`
		Actor        *model.Actor            `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, "NOT_DOCUMENT", http.StatusBadRequest, "artifact is not a document")
		return
	}
	// release ends the conditional edit gate once: right after the commit, which is all a later
	// conditional edit of this document waits on, so the checks of this edit's new decision blocks
	// never hold it; on every earlier return, here.
	release := func() {}
	if input.Precondition != nil {
		acquired, err := s.deps.Docs.AcquireConditionalEdit(r.Context(), artifact.ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		release = sync.OnceFunc(acquired)
		defer release()
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	eventOwner := ownerForArtifact(artifact)
	status, err := s.requireOpenOwnerStatus(r.Context(), tx, eventOwner)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	edit, err := s.deps.Docs.ApplyOps(documentCtx, artifact.ID, input.Ops, actor, input.Precondition)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var written *docs.VersionResult
	var published []model.Event
	// A batch that left the document as it was names no version, however deliberate its summary,
	// so edits that change nothing never pile up byte-identical versions.
	if edit.Changed {
		summary := strings.TrimSpace(input.Summary)
		if summary != "" {
			namedVersion, err := s.deps.Docs.NamedVersion(documentCtx, artifact.ID, summary, actor)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			written = &namedVersion
		} else {
			snapshot, err := s.deps.Docs.SnapshotVersion(documentCtx, artifact.ID, actor)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			if snapshot.Wrote {
				written = &snapshot
			}
		}
		if written != nil {
			var diff *string
			if written.Version.Named {
				diff, err = s.namedVersionDiff(r.Context(), tx, artifact.ID, written.Version)
				if err != nil {
					s.writeHandlerError(w, err)
					return
				}
			}
			event, err := s.commitArtifactVersionEvent(r.Context(), tx, eventOwner, actor, artifact.ID, artifact.Name, *written, diff)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			published = append(published, event)
		}
	}
	// The approval this edit's own version superseded, read in the edit's transaction after that
	// version and any carry it made: a person's approval of a spec is Legion's design gate, and
	// the writer is the only one who learns in time that this write closed it.
	var superseded *supersededApproval
	if written != nil {
		superseded = s.supersededApprovalAdvice(
			r.Context(), tx, "POST /api/v1/artifacts/{id}/edits", artifactIssueKey(artifact), artifact.ID, written.Version.Number,
		)
	}
	var advice *writeAdvice
	if artifact.IssueKey != nil && status != nil {
		advice = s.writeAdvice(
			r.Context(), tx, "POST /api/v1/artifacts/{id}/edits", *artifact.IssueKey, actor, "", *status,
		)
	}
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	release()
	var version *model.Version
	if written != nil {
		version = &written.Version
	}
	if edit.Applied > 0 {
		s.deps.Docs.ScheduleSettlement(artifact.ID)
	}
	s.publish(published...)
	unchanged := edit.Unchanged
	if unchanged == nil {
		unchanged = []int{}
	}
	responsePayload := map[string]any{
		"applied":       edit.Applied,
		"version":       version,
		"changed":       edit.Changed,
		"unchanged_ops": unchanged,
		// Which operations the live document did not hold once this write reached it: a
		// concurrent change that landed after the version was rendered and before the publish is
		// past undoing, so the edit reports it rather than refusing (LEGION-269). null means the
		// check reached no verdict - the publish failed and the room is reloading, or the room
		// holds a tree too deep to read - which is not the same statement as the empty list.
		"lost_ops": lostOps(ledger, artifact.ID),
		"token":    edit.Token,
	}
	// The ask blocks the edit added, read from the documents the batch was applied to: whether an
	// operation's markdown makes a block depends on where it lands and on the parser's reading of
	// code and containers, which a caller's own reading of that markdown would only approximate.
	blocks := &editBlocks{
		DecisionBlocksAdded: len(edit.AddedAskBlocks),
		DecisionBlockChecks: s.decisionBlockChecks(r.Context(), artifact, edit.AddedAskBlocks, actor),
	}
	if artifact.IssueKey == nil || advice == nil {
		WriteJSON(w, http.StatusOK, withRawAdvice(responsePayload, documentWriteAdvice{
			editBlocks: blocks, SupersededApproval: superseded,
		}))
		return
	}
	advice.editBlocks = blocks
	advice.SupersededApproval = superseded
	WriteJSON(w, http.StatusOK, withAdvice(responsePayload, advice))
}

// artifactIssueKey is the issue a document belongs to, or the empty string for a project
// document, for a log line that names where a write ran.
func artifactIssueKey(artifact model.Artifact) string {
	if artifact.IssueKey == nil {
		return ""
	}
	return *artifact.IssueKey
}

// lostOps is the edit response's lost_ops: the operations the live document did not hold after
// the write was published, or null when the write reached no verdict.
func lostOps(ledger *docs.Ledger, artifactID string) []int {
	lost, known := ledger.LostOps(artifactID)
	if !known {
		return nil
	}
	if lost == nil {
		return []int{}
	}
	return lost
}

func participantsOrEmpty(participants []model.Actor) []model.Actor {
	if participants == nil {
		return []model.Actor{}
	}
	return participants
}

func (s *server) loadArtifacts(ctx context.Context, q queryer, issueKey string) ([]model.Artifact, error) {
	rows, err := q.Query(ctx, `
		select `+artifactColumns+`
		from artifacts where issue_key = $1 order by created_at, id
	`, issueKey)
	if err != nil {
		return nil, err
	}
	artifacts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Artifact, error) {
		return scanArtifact(row)
	})
	if err != nil {
		return nil, err
	}
	if artifacts == nil {
		artifacts = []model.Artifact{}
	}
	pointers := make([]*model.Artifact, len(artifacts))
	for index := range artifacts {
		versions, err := s.loadVersions(ctx, q, artifacts[index].ID)
		if err != nil {
			return nil, err
		}
		artifacts[index].Versions = versions
		pointers[index] = &artifacts[index]
	}
	if err := s.attachApprovals(ctx, q, pointers); err != nil {
		return nil, err
	}
	return artifacts, nil
}

func validateArtifactRequestRef(r *http.Request) error {
	if r.PathValue("key") != "" {
		return nil
	}
	_, err := parseArtifactID(r.PathValue("id"))
	return err
}

func parseArtifactID(id string) (string, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return "", errorf(http.StatusNotFound, "ARTIFACT_NOT_FOUND", "artifact not found")
	}
	return parsed.String(), nil
}

func (s *server) loadArtifactForRequest(ctx context.Context, q queryer, r *http.Request) (model.Artifact, error) {
	if r.PathValue("session_id") != "" {
		return s.loadAgentArtifact(ctx, q, r)
	}
	if key := r.PathValue("key"); key != "" {
		target := artifactTarget{Project: key}
		if strings.HasPrefix(r.URL.Path, "/api/v1/issues/") {
			target = artifactTarget{IssueKey: &key}
		}
		return s.loadArtifactByDocumentReference(ctx, q, target, r.PathValue("slug"))
	}
	return s.loadArtifact(ctx, q, r.PathValue("id"))
}

const documentHintLimit = 8

// loadArtifactByDocumentReference reads an issue's or a project's artifact at slug reference, or
// the one document whose filename it is.
func (s *server) loadArtifactByDocumentReference(ctx context.Context, q queryer, target artifactTarget, reference string) (model.Artifact, error) {
	return resolveDocumentReference(ctx, s, q, target, reference,
		func(refKey string) (model.Artifact, error) { return s.loadArtifactByRefKey(ctx, q, refKey) },
		func(id string) (model.Artifact, error) { return s.loadArtifact(ctx, q, id) },
	)
}

// resolveDocumentReference is the one resolution of an issue's or a project's document reference,
// shared by the full read and the summary: the artifact at slug reference (byRefKey, given the
// ref_key), else the one document whose filename it is (byID, given its id). Each reader answers
// ARTIFACT_NOT_FOUND for no row, and reads only the shape its route serves.
func resolveDocumentReference[T any](
	ctx context.Context,
	s *server,
	q queryer,
	target artifactTarget,
	reference string,
	byRefKey func(refKey string) (T, error),
	byID func(id string) (T, error),
) (T, error) {
	found, err := byRefKey(target.refPrefix() + "/" + reference)
	if err == nil || !isArtifactNotFound(err) {
		return found, err
	}
	var none T
	documents, listErr := s.loadDocumentReferences(ctx, q, target)
	if listErr != nil {
		return none, listErr
	}
	matches := make([]model.Artifact, 0, 1)
	for index := range documents {
		if documents[index].Name == reference {
			matches = append(matches, documents[index])
		}
	}
	switch len(matches) {
	case 0:
		return none, documentReferenceError(target, reference, documents)
	case 1:
		return byID(matches[0].ID)
	default:
		return none, ambiguousDocumentReferenceError(target, reference, matches)
	}
}

func isArtifactNotFound(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.code == "ARTIFACT_NOT_FOUND"
}

func (s *server) loadDocumentReferences(ctx context.Context, q queryer, target artifactTarget) ([]model.Artifact, error) {
	query := `
		select ` + artifactColumns + `
		from artifacts where issue_key = $1 and kind = 'doc' order by created_at, id`
	value := target.Project
	if target.IssueKey != nil {
		value = *target.IssueKey
	} else {
		query = `
			select ` + artifactColumns + `
			from artifacts where project_key = $1 and issue_key is null and kind = 'doc' order by created_at, id`
	}
	rows, err := q.Query(ctx, query, value)
	if err != nil {
		return nil, err
	}
	documents, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Artifact, error) {
		return scanArtifact(row)
	})
	if err != nil {
		return nil, err
	}
	return documents, nil
}

// documentReferenceError refuses a reference that names none of the owner's documents. An owner
// with more documents than the hint lists (documentHintLimit) is named with its count, the
// documents nearest the reference first (documentsNearest), and the route that lists them all.
func documentReferenceError(target artifactTarget, reference string, documents []model.Artifact) error {
	scope := "this project's"
	everyDocument := "/api/v1/projects/" + target.Project + "/artifacts?unlinked=true"
	if target.IssueKey != nil {
		scope = "this issue's"
		everyDocument = "/api/v1/issues/" + *target.IssueKey + "/artifacts"
	}
	if len(documents) <= documentHintLimit {
		return errorf(
			http.StatusNotFound,
			"ARTIFACT_NOT_FOUND",
			`document %q not found by slug; %s documents: %s`,
			reference,
			scope,
			documentReferenceLabels(documents),
		)
	}
	return errorf(
		http.StatusNotFound,
		"ARTIFACT_NOT_FOUND",
		`document %q not found by slug; %s %d documents include: %s; GET %s lists them all`,
		reference,
		scope,
		len(documents),
		documentReferenceLabels(documentsNearest(reference, documents)),
		everyDocument,
	)
}

// documentsNearest orders documents, given oldest first, for a hint about reference: those whose
// slug or filename is close to it (documentCloseness) first, nearest first, then every other one,
// each group newest first. envoy-client's documentsNearest orders an issue's documents by the
// same rule for the hint the dispatch command gives.
func documentsNearest(reference string, documents []model.Artifact) []model.Artifact {
	type ranked struct {
		document model.Artifact
		index    int
		distance int
		near     bool
	}
	rankedDocuments := make([]ranked, len(documents))
	for index, document := range documents {
		distance, near := documentCloseness(reference, document.Slug, document.Name)
		rankedDocuments[index] = ranked{document: document, index: index, distance: distance, near: near}
	}
	slices.SortStableFunc(rankedDocuments, func(a, b ranked) int {
		if a.near != b.near {
			if a.near {
				return -1
			}
			return 1
		}
		if a.near && a.distance != b.distance {
			return a.distance - b.distance
		}
		return b.index - a.index
	})
	ordered := make([]model.Artifact, len(rankedDocuments))
	for index, document := range rankedDocuments {
		ordered[index] = document.document
	}
	return ordered
}

// documentCloseness compares a reference with a document's slug and filename, ASCII letter case
// ignored: the smaller edit distance of the two, and whether the document is close to it - within
// a third of the reference's length (at least 2 edits), or one holding the other where the one held
// is at least 4 characters long, whose distance is then exactly their difference in length. A
// spelling more than limit edits away gives no distance (boundedEditDistance knows only that it is
// past limit), so it never ties a document that is close through its other spelling. Only ASCII
// letters are folded because Go and JavaScript lowercase other letters differently, and
// envoy-client's documentCloseness must rank alike.
func documentCloseness(reference, slug, name string) (int, bool) {
	wanted := []rune(asciiLower(reference))
	limit := max(2, len(wanted)/3)
	distance := -1
	for _, candidate := range []string{slug, name} {
		label := []rune(asciiLower(candidate))
		edits := boundedEditDistance(wanted, label, limit)
		if (len(wanted) >= 4 && strings.Contains(string(label), string(wanted))) ||
			(len(label) >= 4 && strings.Contains(string(wanted), string(label))) {
			edits = max(len(label)-len(wanted), len(wanted)-len(label))
		} else if edits > limit {
			continue
		}
		if distance < 0 || edits < distance {
			distance = edits
		}
	}
	if distance < 0 {
		return limit + 1, false
	}
	return distance, true
}

// asciiLower lowercases the ASCII letters of text and leaves every other character as it is.
func asciiLower(text string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, text)
}

// boundedEditDistance is the Levenshtein distance between a and b in characters, or limit+1 once
// it is known to be more than limit.
func boundedEditDistance(a, b []rune, limit int) int {
	if len(a)-len(b) > limit || len(b)-len(a) > limit {
		return limit + 1
	}
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		smallest := current[0]
		for j := 1; j <= len(b); j++ {
			substitution := previous[j-1]
			if a[i-1] != b[j-1] {
				substitution++
			}
			current[j] = min(previous[j]+1, current[j-1]+1, substitution)
			smallest = min(smallest, current[j])
		}
		if smallest > limit {
			return limit + 1
		}
		previous, current = current, previous
	}
	return min(previous[len(b)], limit+1)
}

func ambiguousDocumentReferenceError(target artifactTarget, reference string, documents []model.Artifact) error {
	scope := "project"
	if target.IssueKey != nil {
		scope = "issue"
	}
	return errorf(
		http.StatusBadRequest,
		"ARTIFACT_REFERENCE_AMBIGUOUS",
		`%q names %d documents on this %s; use a slug: %s`,
		reference,
		len(documents),
		scope,
		documentReferenceLabels(documents),
	)
}

func documentReferenceLabels(documents []model.Artifact) string {
	labels := make([]string, 0, min(len(documents), documentHintLimit))
	for index, artifact := range documents {
		if index == documentHintLimit {
			break
		}
		labels = append(labels, fmt.Sprintf("%s (%s)", artifact.Slug, artifact.Name))
	}
	if len(labels) == 0 {
		return "none"
	}
	return strings.Join(labels, ", ")
}

func (s *server) loadArtifact(ctx context.Context, q queryer, id string) (model.Artifact, error) {
	parsed, err := parseArtifactID(id)
	if err != nil {
		return model.Artifact{}, err
	}
	return s.loadArtifactRow(ctx, q, q.QueryRow(ctx, `
		select `+artifactColumns+`
		from artifacts where id = $1
	`, parsed))
}

func (s *server) loadArtifactByRefKey(ctx context.Context, q queryer, refKey string) (model.Artifact, error) {
	return s.loadArtifactRow(ctx, q, q.QueryRow(ctx, `
		select `+artifactColumns+`
		from artifacts where ref_key = $1
	`, refKey))
}

func (s *server) loadArtifactRow(ctx context.Context, q queryer, row pgx.Row) (model.Artifact, error) {
	artifact, err := scanArtifact(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Artifact{}, errorf(http.StatusNotFound, "ARTIFACT_NOT_FOUND", "artifact not found")
	}
	if err != nil {
		return model.Artifact{}, err
	}
	versions, err := s.loadVersions(ctx, q, artifact.ID)
	if err != nil {
		return model.Artifact{}, err
	}
	artifact.Versions = versions
	if err := s.attachApproval(ctx, q, &artifact); err != nil {
		return model.Artifact{}, err
	}
	return artifact, nil
}

// scanArtifact reads artifactColumns. A session-owned artifact's project is "".
func scanArtifact(row pgx.Row) (model.Artifact, error) {
	var artifact model.Artifact
	var project *string
	var createdBy []byte
	if err := row.Scan(
		&artifact.ID, &artifact.IssueKey, &project, &artifact.SessionID, &artifact.RefKey, &artifact.Slug,
		&artifact.Name, &artifact.Kind, &artifact.Primary, &createdBy, &artifact.CreatedAt,
	); err != nil {
		return model.Artifact{}, err
	}
	artifact.Project = deref(project)
	if err := json.Unmarshal(createdBy, &artifact.CreatedBy); err != nil {
		return model.Artifact{}, fmt.Errorf("decode artifact author: %w", err)
	}
	return artifact, nil
}

// artifactOwnerKey is the owner part of ref_key (artifactTarget.refPrefix), which the 0071
// trigger fills ref_key from, so a lookup by owner compares what ref_key was built from. Its
// session arm, 'agent/' || session_id, has a second reader: an agent's conversation's lookups
// (agentArtifactByNameOrSlugSQL, agentArtifactSlugTakenSQL in agent_artifacts.go) compare session_id
// itself, so a change to ref_key's owner part changes both.
const artifactOwnerKey = `coalesce(issue_key, project_key, 'agent/' || session_id)`

// artifactByNameOrSlugSQL and artifactSlugTakenSQL are an upload's two lookups by owner, $1 the
// owner key and $2 the name or the slug; an agent's conversation runs its own pair
// (artifactTarget.ownerLookup). An upload's name finds the artifact whose name or slug it is, so a
// name that is one artifact's slug is that artifact's next version rather than a second artifact
// the same reference would also name. An owner that already holds two such artifacts (one whose
// slug and another whose name it is, which uploads stored before this lookup read slugs) gets the
// one whose slug it is, the one a read by that reference resolves to (resolveDocumentReference).
const (
	artifactByNameOrSlugSQL = `select ` + artifactColumns + ` from artifacts where ` + artifactOwnerKey +
		` = $1 and $2 in (name, slug) order by slug = $2 desc limit 1`
	artifactSlugTakenSQL = `select exists(select 1 from artifacts where ` + artifactOwnerKey + ` = $1 and slug = $2)`
)

func (s *server) findArtifactByNameOrSlug(ctx context.Context, q queryer, target artifactTarget, name string) (model.Artifact, error) {
	artifact, err := lookupArtifactByNameOrSlug(ctx, q, target, name)
	if err != nil {
		return model.Artifact{}, err
	}
	versions, err := s.loadVersions(ctx, q, artifact.ID)
	if err != nil {
		return model.Artifact{}, err
	}
	artifact.Versions = versions
	return artifact, nil
}

// lookupArtifactByNameOrSlug reads the artifact name finds under target's owner, without its versions.
func lookupArtifactByNameOrSlug(ctx context.Context, q queryer, target artifactTarget, name string) (model.Artifact, error) {
	query, owner := target.ownerLookup(artifactByNameOrSlugSQL, agentArtifactByNameOrSlugSQL)
	return scanArtifact(q.QueryRow(ctx, query, owner, name))
}

// versionColumns is the column list a version read selects over artifact_versions aliased v, in
// the order scanned into model.Version: number, named, summary, authors, created_at, size, mime,
// sha256. The size never reads a document's markdown (model.VersionSize).
const versionColumns = `v.number, v.named, v.summary, v.authors, v.created_at, ` + model.VersionSize + `, v.mime, v.sha256`

func (s *server) loadVersions(ctx context.Context, q queryer, artifactID string) ([]model.Version, error) {
	rows, err := q.Query(ctx, `
		select `+versionColumns+`
		from artifact_versions v where v.artifact_id = $1 order by v.number
	`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	versions := []model.Version{}
	for rows.Next() {
		var version model.Version
		var authors []byte
		if err := rows.Scan(&version.Number, &version.Named, &version.Summary, &authors, &version.CreatedAt, &version.Size, &version.MIME, &version.SHA256); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(authors, &version.Authors); err != nil {
			return nil, fmt.Errorf("decode version authors: %w", err)
		}
		versions = append(versions, version)
	}
	return versions, rows.Err()
}

// loadVersion reads one version of a document with its markdown, which the content route answers,
// so its size is that markdown's length rather than a second read of the column.
func (s *server) loadVersion(ctx context.Context, q queryer, artifactID string, number int, markdown *string) (model.Version, error) {
	var version model.Version
	var authors []byte
	if err := q.QueryRow(ctx, `
		select v.number, v.named, v.summary, v.authors, v.created_at, v.mime, v.sha256, v.markdown
		from artifact_versions v where v.artifact_id = $1 and v.number = $2
	`, artifactID, number).Scan(
		&version.Number, &version.Named, &version.Summary, &authors, &version.CreatedAt,
		&version.MIME, &version.SHA256, markdown,
	); err != nil {
		return model.Version{}, err
	}
	if err := json.Unmarshal(authors, &version.Authors); err != nil {
		return model.Version{}, fmt.Errorf("decode version authors: %w", err)
	}
	size := len(*markdown)
	version.Size = &size
	return version, nil
}

// artifactKind is what an upload is stored as - doc, image or file - and the type a file or an
// image is stored and served under. A declared text/markdown is a document. Any other upload is an
// image when, and only when, its bytes are one of the pictures a model is shown (PNG, JPEG, GIF,
// WebP), read from the bytes rather than the declared type, which a client sets at will; an image
// is stored under the type its bytes are. Anything else, an SVG for one, is a file under the
// declared type, so no client stores other bytes as an image.
func artifactKind(mediaType, declared string, content []byte) (kind, storedType string) {
	if mediaType == "text/markdown" {
		return "doc", declared
	}
	switch sniffed := http.DetectContentType(content); sniffed {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return "image", sniffed
	}
	return "file", declared
}

func artifactSlug(name string) string {
	var slug strings.Builder
	previousDash := false
	for _, r := range strings.ToLower(name) {
		// ASCII only: every grammar that reads a slug back - text.artifactSlugPattern, the
		// dashboard's own artifactSlugPattern, 0009's cleanup of malformed references - spells it
		// [a-z0-9]+(-[a-z0-9]+)*, so a slug holding another letter would be stored and then name
		// nothing, and the picture a `![name](dispatch://.../artifact/<slug>@vN)` line shows would
		// never load (LEGION-541).
		if ('a' <= r && r <= 'z') || ('0' <= r && r <= '9') {
			slug.WriteRune(r)
			previousDash = false
			continue
		}
		if slug.Len() > 0 && !previousDash {
			slug.WriteByte('-')
			previousDash = true
		}
	}
	result := strings.Trim(slug.String(), "-")
	if result == "" {
		return "artifact"
	}
	return result
}

// artifactKindMismatch is the 400 refusing an upload under name, of kind, as the next version of
// artifact, the one the name or slug finds, whose kind is another.
func artifactKindMismatch(name string, artifact model.Artifact, kind string) error {
	return errorf(http.StatusBadRequest, "ARTIFACT_KIND_MISMATCH",
		"%q names artifact %s (%s), whose kind is %s; this upload's kind is %s, so it cannot be its next version: give it another name",
		name, artifact.Slug, artifact.Name, artifact.Kind, kind)
}

// newArtifactNameRefusal is the 400 refusing a new artifact's name past maxNewArtifactKeyBytes,
// or nil. An upload naming an artifact that already exists is never held to it.
func newArtifactNameRefusal(name string) error {
	if len(name) <= maxNewArtifactKeyBytes {
		return nil
	}
	return errorf(http.StatusBadRequest, "INVALID_ARTIFACT",
		"artifact name is %d bytes, past the %d bytes a new artifact's name may hold; give it a shorter name",
		len(name), maxNewArtifactKeyBytes)
}

// newArtifactKeyRefusal is the 400 refusing a new artifact whose reference key, the target's
// owner, `/` and slug (the one nextArtifactSlug took, suffix and all), is past
// maxNewArtifactKeyBytes, or nil.
func newArtifactKeyRefusal(target artifactTarget, slug string) error {
	key := len(target.refPrefix()) + 1 + len(slug)
	if key <= maxNewArtifactKeyBytes {
		return nil
	}
	return errorf(http.StatusBadRequest, "INVALID_ARTIFACT",
		"the reference key `%s/<slug>` this name gives a new artifact is %d bytes, past the %d bytes a new artifact's reference key may hold; give it a shorter name",
		target.refPrefix(), key, maxNewArtifactKeyBytes)
}

// lockProjectArtifacts serialises one project's own uploads (its documents, not its issues') for
// the rest of tx, as lockAgentArtifacts does a conversation's. A project's upload otherwise locks
// no row before it looks its artifact up by name or slug and takes a new slug, so two uploads at
// once - `plan.md` and `plan-md` - could each miss the other and leave two documents that one
// reference names. Only these uploads take the lock, and they take it before any row or room, so
// it joins no lock cycle.
func lockProjectArtifacts(ctx context.Context, tx pgx.Tx, project string) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('project-artifacts:' || $1))`, project); err != nil {
		return fmt.Errorf("lock the project's documents: %w", err)
	}
	return nil
}

func (s *server) nextArtifactSlug(ctx context.Context, q queryer, target artifactTarget, name string) (string, error) {
	base := artifactSlug(name)
	query, owner := target.ownerLookup(artifactSlugTakenSQL, agentArtifactSlugTakenSQL)
	for suffix := 1; ; suffix++ {
		candidate := base
		if suffix > 1 {
			candidate = fmt.Sprintf("%s-%d", base, suffix)
		}
		var inUse bool
		if err := q.QueryRow(ctx, query, owner, candidate).Scan(&inUse); err != nil {
			return "", err
		}
		if !inUse {
			return candidate, nil
		}
	}
}

// quiesceDocuments closes every live document on every process, flushing each through the store,
// and waits for the settlements in flight. Test-only: mounted by routes() only when
// Deps.TestHooksEnabled is set, so a browser-test harness can truncate its database between
// scenarios with no document the previous scenario had open still writing to it. It answers only
// once every process has closed its documents (runTestHookEverywhere): a room still open on
// another process would write to the tables the harness truncates next.
func (s *server) quiesceDocuments(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	s.runTestHookEverywhere(w, r, testHookCall{Hook: hookQuiesceDocuments})
}
