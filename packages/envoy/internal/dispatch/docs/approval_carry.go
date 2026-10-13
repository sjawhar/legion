package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A human's answer to a decision block carries his approval forward. He approved the document at
// version A; the version his answer writes, N, differs from A only in its ask blocks, and every
// block N holds that A did not is one he answered: nothing he has not agreed to is in N, so
// Dispatch records his approval of N in his name, in the transaction that writes N, rather than
// leaving his approval stale and asking him to approve again what he just decided (LEGION-573).

// carryReason is the reason a carried review records, naming the approval it follows.
const carryReason = "Carried forward from the approval of version %d: the answer to a decision block was the whole change since it."

// askBlockState is one ask block's server-owned state as its tree holds it: answeredBy and
// answeredAt empty unless it is answered, copied when it is a copy of another document's ask
// (copied_from), which no answer of this document decides.
type askBlockState struct {
	state      string
	answeredBy string
	answeredAt string
	copied     bool
}

// carryApproval carries a human's approval to version, just written from rendered, when the
// answer he gave a decision block is the whole change since the version he approved (the rule
// above). It returns the events it appended - the open approval request's ask.answered, when one
// is open, then artifact.approved - and nothing when the version carries no approval. It reads
// rendered.tree and never changes it: the tree is the live document's.
func (s *Service) carryApproval(ctx context.Context, tx pgx.Tx, artifactID string, version model.Version, rendered renderedTree) ([]model.Event, error) {
	blocks := askBlockStates(rendered.tree)
	if !holdsAnsweredBlock(blocks) {
		return nil, nil
	}
	previous, readable, err := s.storedVersionTree(ctx, tx, artifactID, version.Number-1)
	if err != nil || !readable {
		return nil, err
	}
	login, newly := newlyAnsweredBlocks(blocks, askBlockStates(previous))
	if !newly {
		return nil, nil
	}
	review, err := latestReviewBy(ctx, tx, artifactID, login)
	if err != nil || review == nil || review.State != "approved" {
		return nil, err
	}
	var changesRequestedSince bool
	if err := tx.QueryRow(ctx, `
		select exists(
			select 1 from artifact_reviews
			where artifact_id = $1 and state = 'changes_requested' and (created_at, id) > ($2, $3::uuid)
		)
	`, artifactID, review.CreatedAt, review.ID).Scan(&changesRequestedSince); err != nil {
		return nil, fmt.Errorf("read reviews since the approval: %w", err)
	}
	if changesRequestedSince {
		return nil, nil
	}
	approved := previous
	if review.Version != version.Number-1 {
		if approved, readable, err = s.storedVersionTree(ctx, tx, artifactID, review.Version); err != nil || !readable {
			return nil, err
		}
	}
	approvedCut, err := renderWithoutAskBlocks(approved)
	if err != nil {
		return nil, skipUnreadableVersion(artifactID, review.Version, err)
	}
	cut, err := CutDecisionBlocks(rendered.markdown)
	if err != nil {
		return nil, skipUnreadableVersion(artifactID, version.Number, err)
	}
	if approvedCut != cut {
		return nil, nil
	}
	approvedBlocks := askBlockStates(approved)
	for id, block := range blocks {
		if _, held := approvedBlocks[id]; held {
			continue
		}
		if block.state != "answered" || model.CanonicalLogin(block.answeredBy) != login {
			return nil, nil
		}
	}
	for id := range approvedBlocks {
		if _, held := blocks[id]; !held {
			return nil, nil
		}
	}

	reason := fmt.Sprintf(carryReason, review.Version)
	var events []model.Event
	var askID *string
	open, err := OpenApprovalAsk(ctx, tx, artifactID)
	if err != nil {
		return nil, err
	}
	if open != nil {
		answered, err := answerApprovalAsk(ctx, tx, s.events, open, review.Actor, reason)
		if err != nil {
			return nil, err
		}
		events = append(events, answered)
		askID = &open.ID
	}
	artifact := model.Artifact{ID: artifactID}
	if err := tx.QueryRow(ctx, `select name, issue_key from artifacts where id = $1`, artifactID).Scan(&artifact.Name, &artifact.IssueKey); err != nil {
		return nil, fmt.Errorf("read the document for its carried approval: %w", err)
	}
	_, event, err := WriteReview(ctx, tx, s.events, artifact, version.Number, "approved", review.Actor, &reason, askID)
	if err != nil {
		return nil, err
	}
	return append(events, event), nil
}

// skipUnreadableVersion is carryApproval's answer to a version this parser or renderer refuses: a
// version an earlier release stored is not this write's to fail for, so the approval is not
// carried and the refusal is logged; any other error is the write's.
func skipUnreadableVersion(artifactID string, number int, err error) error {
	if !errors.Is(err, pmdoc.ErrSchema) {
		return err
	}
	slog.Warn("dispatch: skip carrying an approval over a document version outside the Proof schema",
		"room", artifactID, "version", number, "error", err)
	return nil
}

// askBlockStates is the server-owned state of every ask block tree holds, by block id, wherever
// the block stands: in a blockquote, a list item or another typed block. A repeated id keeps the
// first block's state, as settlement indexes the first; a nil tree holds none.
func askBlockStates(tree *pmdoc.Node) map[string]askBlockState {
	var states map[string]askBlockState
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if _, seen := states[id]; seen {
			return true
		}
		if states == nil {
			states = map[string]askBlockState{}
		}
		state, _ := node.Attrs["state"].(string)
		answeredBy, _ := node.Attrs["answered_by"].(string)
		answeredAt, _ := node.Attrs["answered_at"].(string)
		copied, _ := node.Attrs["copied_from"].(string)
		states[id] = askBlockState{state: state, answeredBy: answeredBy, answeredAt: answeredAt, copied: copied != ""}
		return true
	})
	return states
}

// holdsAnsweredBlock reports whether blocks holds an answered block of this document's own: one
// that is no copy. A version holding none carries no approval, whatever the comparison would say,
// and reads nothing more.
func holdsAnsweredBlock(blocks map[string]askBlockState) bool {
	for _, block := range blocks {
		if block.state == "answered" && !block.copied {
			return true
		}
	}
	return false
}

// newlyAnsweredBlocks finds the blocks of this version, blocks, answered since the version before
// it, previous: answered here and absent there, or there with another answered_at. Copies are
// left out on both sides. It returns the one canonical login that answered them, and whether
// there is exactly one: no newly answered block, or two people's answers in one version, is no
// carry.
func newlyAnsweredBlocks(blocks, previous map[string]askBlockState) (string, bool) {
	login, found := "", false
	for id, block := range blocks {
		if block.state != "answered" || block.copied {
			continue
		}
		if before, held := previous[id]; held && !before.copied && before.answeredAt == block.answeredAt {
			continue
		}
		answeredBy := model.CanonicalLogin(block.answeredBy)
		if found && answeredBy != login {
			return "", false
		}
		login, found = answeredBy, true
	}
	return login, found
}

// storedVersionTree is the tree of the stored version number of artifactID, and whether that
// version could be read: a version outside the Proof schema is logged and not readable, and
// number 0, before the document's first version, is a nil tree holding nothing.
func (s *Service) storedVersionTree(ctx context.Context, tx pgx.Tx, artifactID string, number int) (tree *pmdoc.Node, readable bool, err error) {
	if number < 1 {
		return nil, true, nil
	}
	var markdown string
	if err := tx.QueryRow(ctx, `select markdown from artifact_versions where artifact_id = $1 and number = $2`, artifactID, number).Scan(&markdown); err != nil {
		return nil, false, fmt.Errorf("read document version %d: %w", number, err)
	}
	tree, err = pmdoc.ParseRendering(markdown)
	if err != nil {
		return nil, false, skipUnreadableVersion(artifactID, number, err)
	}
	return tree, true, nil
}

// latestReviewBy is the newest review of artifactID a person gave, in any state, by his canonical
// login (model.CanonicalLogin), or nil when he has reviewed the document at no version.
func latestReviewBy(ctx context.Context, tx pgx.Tx, artifactID, login string) (*model.ArtifactReview, error) {
	review, err := ScanReview(tx.QueryRow(ctx, `
		select `+ReviewColumns+`
		from artifact_reviews
		where artifact_id = $1 and actor->>'kind' = 'user' and lower(trim(actor->>'id')) = $2
		order by created_at desc, id desc
		limit 1
	`, artifactID, login))
	if err == nil {
		return &review, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return nil, fmt.Errorf("read latest review by %q: %w", login, err)
}

// answerApprovalAsk answers the open approval request ask with Approve in actor's name, giving
// reason as the answer's text, as a human's answer through the answer route is written, and
// appends its ask.answered event on the ask's owner.
func answerApprovalAsk(ctx context.Context, tx pgx.Tx, broker *events.Broker, ask *model.Ask, actor model.Actor, reason string) (model.Event, error) {
	answer := model.AskAnswer{User: actor.ID, Selected: []string{ApprovalOptionApprove}, Text: &reason, At: time.Now().UTC()}
	encoded, err := json.Marshal(answer)
	if err != nil {
		return model.Event{}, fmt.Errorf("encode approval answer: %w", err)
	}
	if _, err := tx.Exec(ctx, `update asks set state = 'answered', answer = $2 where id = $1`, ask.ID, encoded); err != nil {
		return model.Event{}, fmt.Errorf("answer approval ask: %w", err)
	}
	ask.State = "answered"
	ask.Answer = &answer
	event, err := broker.Append(ctx, tx, model.Event{
		IssueKey: ask.IssueKey, ArtifactID: ask.ArtifactID,
		Type: "ask.answered", Actor: actor,
		Payload: model.AskAnsweredEventPayload{AskEventPayload: model.NewAskEventPayload(*ask, model.ReferenceChanges{})},
	})
	if err != nil {
		return model.Event{}, fmt.Errorf("append approval ask answer: %w", err)
	}
	return event, nil
}

// CutDecisionBlocks is markdown rendered again with every ask block left out, wherever it stands,
// so two versions that differ only in their decision blocks cut to the same text. Both sides of
// the comparison go through it, so a version an earlier renderer stored compares as today's
// renderer writes it. A container the cut empties - a blockquote or a callout that held only
// decision blocks - is left holding one empty paragraph, which is what both parsers read an empty
// container as.
func CutDecisionBlocks(markdown string) (string, error) {
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		return "", err
	}
	return renderWithoutAskBlocks(tree)
}

// renderWithoutAskBlocks renders a copy of tree with its ask blocks left out (withoutAskBlocks).
func renderWithoutAskBlocks(tree *pmdoc.Node) (string, error) {
	return pmdoc.Render(withoutAskBlocks(tree))
}

// withoutAskBlocks is a copy of node's tree with every ask block left out. Each container on the
// way is a fresh node with its own children; the nodes below the last container - text, inline
// nodes, leaves - are the parsed tree's own, which nothing here changes.
func withoutAskBlocks(node *pmdoc.Node) *pmdoc.Node {
	if node.Type == "text" || len(node.Children) == 0 {
		return node
	}
	copied := *node
	copied.Children = make([]*pmdoc.Node, 0, len(node.Children))
	for _, child := range node.Children {
		if child.Type == "ask" {
			continue
		}
		copied.Children = append(copied.Children, withoutAskBlocks(child))
	}
	if len(copied.Children) == 0 {
		copied.Children = append(copied.Children, &pmdoc.Node{Type: "paragraph", Attrs: pmdoc.Attrs{}})
	}
	return &copied
}
