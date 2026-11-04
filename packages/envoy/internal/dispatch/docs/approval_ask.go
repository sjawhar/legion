package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// ApprovalDocumentsJSONPath is the artifact ids of every document an approval names, over the
// ask aliased a: the expression asks_open_approval_documents indexes, so `?` over it finds the
// open request one document is named by and `?|` the requests naming any of several, wherever
// each stands in the list.
const ApprovalDocumentsJSONPath = `jsonb_path_query_array(a.approval, '$.documents[*].artifact_id')`

// OpenApprovalAsk returns the open approval ask naming artifactID, locked, or nil. A request
// that names two documents is found by either of them. The caller holds the document owner's
// row, so it observes the same version the review and approval routes use.
func OpenApprovalAsk(ctx context.Context, tx pgx.Tx, artifactID string) (*model.Ask, error) {
	return OpenApprovalAskForDocuments(ctx, tx, []string{artifactID})
}

// OpenApprovalAskForDocuments returns the newest open approval ask naming any of artifactIDs,
// locked, or nil. A request about to name several documents is found by any one of them, so a
// request opened over an issue's Verification document alone is the one a later request over
// the spec and that document renews: two open requests naming one document would ask a person
// twice about it and strand whichever they did not answer.
func OpenApprovalAskForDocuments(ctx context.Context, tx pgx.Tx, artifactIDs []string) (*model.Ask, error) {
	row := tx.QueryRow(ctx, `
		select `+AskColumns+`
		from asks a
		where a.kind = 'approval' and a.state = 'open' and `+ApprovalDocumentsJSONPath+` ?| $1::text[]
		order by a.created_at desc, a.id desc
		limit 1
		for no key update
	`, artifactIDs)
	ask, err := ScanAsk(row)
	if err == nil {
		return &ask, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return nil, fmt.Errorf("read open approval ask: %w", err)
}

// MoveApprovalAsk advances the open approval ask naming the document to a new settled version of
// it; a request that names two documents moves whichever of them was written. mover is the actor
// whose edit moved the request: the caller knows it even when the version credits more than one
// author. The row and its thread stay open; requested_version records that the agent must hand it
// back before a human sees it in Waiting on you again. Only the first move since the request was
// opened or handed back wakes anyone, even when a thread reply already left it waiting on its
// agent; a later one, while a document's requested_version is already below the version it named,
// is quiet (RewriteApprovalAsk).
func MoveApprovalAsk(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID string, version model.Version, mover model.Actor, serverURL string) ([]model.Event, error) {
	ask, err := OpenApprovalAsk(ctx, tx, artifactID)
	if err != nil {
		return nil, err
	}
	if ask == nil {
		return nil, nil
	}
	moved := ask.Approval.Document(artifactID)
	if moved == nil || moved.Version == version.Number {
		return nil, nil
	}
	summary, err := ApprovalAskSummary(*ask)
	if err != nil {
		return nil, err
	}
	documents := slices.Clone(ask.Approval.Documents)
	for index := range documents {
		if documents[index].ArtifactID == artifactID {
			documents[index].Version = version.Number
		}
	}
	event, err := RewriteApprovalAsk(ctx, tx, broker, ask, mover, documents, summary, serverURL)
	if err != nil {
		return nil, err
	}
	return []model.Event{event}, nil
}

// RewriteApprovalAsk rewords one open approval row to name documents with summary, indexes its
// new question, stamps edited_at, and records its ask.edited event. A document version move, a
// request with a new summary and a request that changes which documents it covers all reword;
// none hands the request back, so each document's requested_version stays as it was. A move of a
// request that an earlier version already moved since it was opened or handed back, a document's
// requested_version below the version it named, is recorded quiet
// (model.AskEditEventPayload.Quiet): it changes only that version, so like a human's unnamed version
// it wakes nobody.
func RewriteApprovalAsk(
	ctx context.Context,
	tx pgx.Tx,
	broker *events.Broker,
	ask *model.Ask,
	actor model.Actor,
	documents []model.AskApprovalDocument,
	summary, serverURL string,
) (model.Event, error) {
	previous := model.NewAskEditPrevious(*ask)
	quiet := approvalMoves(ask.Approval.Documents, documents) && approvalAwaitsHandBack(ask.Approval.Documents)
	ask.Question = ApprovalQuestion(documents, summary)
	ask.Approval.Documents = documents
	approval, err := EncodeApproval(ask.Approval)
	if err != nil {
		return model.Event{}, err
	}
	var editedAt time.Time
	if err := tx.QueryRow(ctx, `
		update asks
		set question = $2, approval = $3, edited_at = clock_timestamp()
		where id = $1
		returning edited_at
	`, ask.ID, ask.Question, approval).Scan(&editedAt); err != nil {
		return model.Event{}, fmt.Errorf("rewrite approval ask: %w", err)
	}
	ask.EditedAt = askTimestamp(&editedAt)
	changes, err := refs.ReplaceCountedEdit(ctx, tx, "ask", ask.ID, previous.Question, ask.Question, serverURL)
	if err != nil {
		return model.Event{}, fmt.Errorf("index approval ask: %w", err)
	}
	// The rewording is a revision of the request's question part, which its ask.edited names
	// beside the request's own references.
	changes, err = refs.IndexAskPartsCounted(ctx, tx, *ask, serverURL, editedAt, changes)
	if err != nil {
		return model.Event{}, fmt.Errorf("index the parts of approval ask %s: %w", ask.ID, err)
	}
	payload := model.NewAskEditEventPayload(*ask, previous, actor, changes)
	payload.Quiet = quiet
	event, err := broker.Append(ctx, tx, model.Event{
		IssueKey: ask.IssueKey, ArtifactID: ask.ArtifactID,
		Type: "ask.edited", Actor: actor, Payload: payload,
	})
	if err != nil {
		return model.Event{}, fmt.Errorf("append approval ask edit: %w", err)
	}
	if err := refs.Stamp(ctx, tx, "ask", ask.ID, event.ID); err != nil {
		return model.Event{}, fmt.Errorf("stamp approval ask references: %w", err)
	}
	return event, nil
}

// EncodeApproval is an approval ask's approval as asks.approval stores it.
func EncodeApproval(approval *model.AskApproval) ([]byte, error) {
	encoded, err := json.Marshal(approval)
	if err != nil {
		return nil, fmt.Errorf("encode approval ask: %w", err)
	}
	return encoded, nil
}

// ApprovalQuestion renders the server-owned question prefix - every document the request names,
// each with the version it names - with its optional summary.
func ApprovalQuestion(documents []model.AskApprovalDocument, summary string) string {
	named := make([]string, 0, len(documents))
	for _, document := range documents {
		named = append(named, fmt.Sprintf("%s (version %d)", document.Name, document.Version))
	}
	question := "Approve " + joinWithAnd(named) + "?"
	if summary == "" {
		return question
	}
	return question + " " + summary
}

// joinWithAnd lists parts as English does: "a", "a and b", "a, b and c".
func joinWithAnd(parts []string) string {
	if len(parts) < 2 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// ApprovalAskSummary parses the summary after the server-owned approval question prefix. A
// question naming the request's first document alone is read too: that is what the release before
// the one that grew a request to two documents writes when a version move rewords such a request
// during a rolling deploy.
func ApprovalAskSummary(ask model.Ask) (string, error) {
	candidates := [][]model.AskApprovalDocument{ask.Approval.Documents}
	// The lead alone is a second candidate only when the request names more than one document:
	// over one it is the same list, and over none there is nothing to slice.
	if len(ask.Approval.Documents) > 1 {
		candidates = append(candidates, ask.Approval.Documents[:1])
	}
	for _, documents := range candidates {
		prefix := ApprovalQuestion(documents, "")
		if ask.Question == prefix {
			return "", nil
		}
		if summary, ok := strings.CutPrefix(ask.Question, prefix+" "); ok && summary != "" {
			return summary, nil
		}
	}
	return "", fmt.Errorf("approval ask %s has a question not written by the approval route", ask.ID)
}

// approvalMoves reports whether rewriting an approval's documents as next moves any document to
// another version.
func approvalMoves(current, next []model.AskApprovalDocument) bool {
	for _, document := range next {
		for _, before := range current {
			if before.ArtifactID == document.ArtifactID && before.Version != document.Version {
				return true
			}
		}
	}
	return false
}

// approvalAwaitsHandBack reports whether a version already moved one of the documents past the
// version its agent last handed to the human, which is the state in which a further move is quiet.
func approvalAwaitsHandBack(documents []model.AskApprovalDocument) bool {
	for _, document := range documents {
		if document.RequestedVersion < document.Version {
			return true
		}
	}
	return false
}
