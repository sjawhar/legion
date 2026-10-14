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
// block of N that is not as A held it - added since, answered or re-answered since, or resolved
// since - is one he answered: nothing he has not agreed to is in N, so Dispatch records his
// approval of N in his name, in the transaction that writes N, rather than leaving his approval
// stale and asking him to approve again what he just decided (LEGION-573).
//
// Who answered what is read from the asks rows of this document, never from N's blocks: a peer on
// the document socket writes a block's attributes as it likes, and settlement writes them back
// from the row only for a block it can read. A block of N that no row of this document backs - a
// forged one, a copy (copied_from, whose row is another document's), or one whose update the room
// has not credited yet - or one settlement could not read (invalid) leaves N's decisions unknown,
// and nothing is carried.

// carryReason is the reason a carried review records, naming the approval it follows.
const carryReason = "Carried forward from the approval of version %d: the answer to a decision block was the whole change since it."

// askBlockState is one ask block's server-owned state: answeredBy and answeredAt empty unless it
// is answered. For N it is read from the block's ask row (askBlockStateOf); for a stored version
// from the attributes settlement wrote from the rows at the time (askBlockStates).
type askBlockState struct {
	state      string
	answeredBy string
	answeredAt string
}

// askBlockStateOf is ask's state as setAskServerAttributes writes it into its block - who
// answered, and when in the attribute's own format - so a block a stored version holds unchanged
// compares equal to its row.
func askBlockStateOf(ask model.Ask) askBlockState {
	if ask.State == "answered" && ask.Answer != nil {
		return askBlockState{state: ask.State, answeredBy: ask.Answer.User, answeredAt: ask.Answer.At.UTC().Format(time.RFC3339Nano)}
	}
	return askBlockState{state: ask.State}
}

// askNode is one ask node of N: copied when it is a copy of another document's ask
// (copied_from), invalid when settlement could not read it (invalid, which the live tree holds
// and no rendering writes).
type askNode struct {
	copied  bool
	invalid bool
}

// carryInput is what the rule is decided on once H and the version he approved, A, are known: the
// blocks of N by block id, from their rows; the blocks of A as its stored version holds them; and
// the two versions cut of their decision blocks (CutDecisionBlocks).
type carryInput struct {
	login            string
	blocks, approved map[string]askBlockState
	approvedCut, cut string
}

// carryApproval carries a human's approval to version, just written from rendered, when the
// answer he gave a decision block is the whole change since the version he approved (the rule
// above). It returns the events it appended - the open approval request's ask.answered, when one
// is open, then artifact.approved - and nothing when the version carries no approval. It reads
// rendered.tree and never changes it: the tree is the live document's.
func (s *Service) carryApproval(ctx context.Context, tx pgx.Tx, artifactID string, version model.Version, rendered renderedTree) ([]model.Event, error) {
	nodes := askNodes(rendered.tree)
	if len(nodes) == 0 {
		return nil, nil
	}
	// Every version write runs this, and a document keeps its ask blocks for the rest of its life:
	// the rows and the version before this one are read only once a carry is possible at all,
	// which needs an approval of the document by someone.
	if approvedByAnyone, err := LatestApprovedReview(ctx, tx, artifactID); err != nil || approvedByAnyone == nil {
		return nil, err
	}
	rows, err := askBlockRows(ctx, tx, artifactID)
	if err != nil {
		return nil, err
	}
	blocks, backed := rowBackedStates(nodes, rows)
	if !backed {
		return nil, nil
	}
	previous, readable, err := s.storedVersionTree(ctx, tx, artifactID, version.Number-1)
	if err != nil || !readable {
		return nil, err
	}
	previousBlocks := askBlockStates(previous)
	login, newly := newlyAnsweredBlocks(blocks, previousBlocks)
	if !newly {
		return nil, nil
	}
	review, err := latestReviewBy(ctx, tx, artifactID, login)
	if err != nil || review == nil || review.State != "approved" {
		return nil, err
	}
	if requested, err := changesRequestedSince(ctx, tx, *review); err != nil || requested {
		return nil, err
	}
	approved, approvedBlocks := previous, previousBlocks
	if review.Version != version.Number-1 {
		if approved, readable, err = s.storedVersionTree(ctx, tx, artifactID, review.Version); err != nil || !readable {
			return nil, err
		}
		approvedBlocks = askBlockStates(approved)
	}
	approvedCut, err := renderWithoutAskBlocks(approved)
	if err != nil {
		return nil, skipUnreadableVersion(artifactID, review.Version, err)
	}
	cut, err := CutDecisionBlocks(rendered.markdown)
	if err != nil {
		return nil, skipUnreadableVersion(artifactID, version.Number, err)
	}
	if !carryVerdict(carryInput{login: login, blocks: blocks, approved: approvedBlocks, approvedCut: approvedCut, cut: cut}) {
		return nil, nil
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
	artifact, err := reviewedArtifact(ctx, tx, artifactID)
	if err != nil {
		return nil, err
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

// askNodes is every ask node tree holds, by block id, wherever it stands: in a blockquote, a list
// item or another typed block. A repeated id keeps every flag the nodes carrying it set, so a copy
// or an unreadable block under the id is seen; a nil tree holds none.
func askNodes(tree *pmdoc.Node) map[string]askNode {
	var nodes map[string]askNode
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		if nodes == nil {
			nodes = map[string]askNode{}
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		copied, _ := node.Attrs["copied_from"].(string)
		invalid, _ := node.Attrs["invalid"].(string)
		seen := nodes[id]
		nodes[id] = askNode{copied: seen.copied || copied != "", invalid: seen.invalid || invalid != ""}
		return true
	})
	return nodes
}

// askBlockRows reads every ask of this document anchored to a block, by block id. A plain read:
// every writer of these rows takes the document's owner row first, which this write holds, and
// nothing here writes a row (lock_order_test.go).
func askBlockRows(ctx context.Context, q Queryer, artifactID string) (map[string]model.Ask, error) {
	rows, err := q.Query(ctx, `
		select `+AskColumns+`
		from asks a where a.block_artifact_id = $1 and a.block_id is not null
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read ask blocks: %w", err)
	}
	defer rows.Close()
	asks := map[string]model.Ask{}
	for rows.Next() {
		ask, err := ScanAsk(rows)
		if err != nil {
			return nil, fmt.Errorf("scan ask block: %w", err)
		}
		if ask.BlockID == nil {
			return nil, fmt.Errorf("read ask block without a block id")
		}
		asks[*ask.BlockID] = ask
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ask blocks: %w", err)
	}
	return asks, nil
}

// rowBackedStates is the state of every ask node of N, each from the row of this document that
// indexes it (rows, by block id), and whether every node has one. A node no row backs, a copy, or
// a node settlement could not read leaves N's decisions unknown: nothing is returned, and the
// caller carries nothing.
func rowBackedStates(nodes map[string]askNode, rows map[string]model.Ask) (map[string]askBlockState, bool) {
	states := make(map[string]askBlockState, len(nodes))
	for id, node := range nodes {
		ask, backed := rows[id]
		if node.copied || node.invalid || !backed {
			return nil, false
		}
		states[id] = askBlockStateOf(ask)
	}
	return states, true
}

// askBlockStates is the server-owned state of every ask block a stored version's tree holds, by
// block id, wherever the block stands, as settlement wrote it from the rows when the version was
// written. A repeated id keeps the first block's state, as settlement indexes the first; a nil
// tree holds none.
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
		states[id] = askBlockState{state: state, answeredBy: answeredBy, answeredAt: answeredAt}
		return true
	})
	return states
}

// newlyAnsweredBlocks finds the blocks of N, blocks, answered since the version before it,
// previous: answered here and absent there, or there with another answered_at. It returns the one
// canonical login that answered them, and whether there is exactly one: no newly answered block,
// or two people's answers in one version, is no carry.
func newlyAnsweredBlocks(blocks, previous map[string]askBlockState) (string, bool) {
	login, found := "", false
	for id, block := range blocks {
		if block.state != "answered" {
			continue
		}
		if before, held := previous[id]; held && before.answeredAt == block.answeredAt {
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

// carryVerdict is the rule over the states alone: N carries H's approval of A when the two cut to
// the same text, every block of N that is not as A held it - added since A, answered or
// re-answered since by anyone, or resolved since, which is nobody's answer - is answered by H, and
// every block of A still stands in N. A block A held unchanged, whoever answered it before his
// approval, is content he approved and stays out of the test.
func carryVerdict(input carryInput) bool {
	if input.approvedCut != input.cut {
		return false
	}
	for id, block := range input.blocks {
		if before, held := input.approved[id]; held && before == block {
			continue
		}
		if block.state != "answered" || model.CanonicalLogin(block.answeredBy) != input.login {
			return false
		}
	}
	for id := range input.approved {
		if _, held := input.blocks[id]; !held {
			return false
		}
	}
	return true
}

// storedVersionTree is the tree of the stored version number of artifactID, and whether that
// version could be read: a version outside the Proof schema is logged and not readable, and a
// number below 1, before the document's first version, is no version and not readable either.
func (s *Service) storedVersionTree(ctx context.Context, tx pgx.Tx, artifactID string, number int) (tree *pmdoc.Node, readable bool, err error) {
	if number < 1 {
		return nil, false, nil
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

// answerApprovalAsk answers the open approval request ask with Approve in actor's name, giving
// reason as the answer's text, as a human's answer through the answer route is written, and
// appends its ask.answered event on the ask's owner. The write names the open state it answers
// from: a request that is no longer open is an error, not a second answer.
func answerApprovalAsk(ctx context.Context, tx pgx.Tx, broker *events.Broker, ask *model.Ask, actor model.Actor, reason string) (model.Event, error) {
	answer := model.AskAnswer{User: actor.ID, Selected: []string{ApprovalOptionApprove}, Text: &reason, At: time.Now().UTC()}
	encoded, err := json.Marshal(answer)
	if err != nil {
		return model.Event{}, fmt.Errorf("encode approval answer: %w", err)
	}
	answered, err := tx.Exec(ctx, `update asks set state = 'answered', answer = $2 where id = $1 and state = 'open'`, ask.ID, encoded)
	if err != nil {
		return model.Event{}, fmt.Errorf("answer approval ask: %w", err)
	}
	if answered.RowsAffected() != 1 {
		return model.Event{}, fmt.Errorf("answer approval ask %s: it is no longer open", ask.ID)
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
