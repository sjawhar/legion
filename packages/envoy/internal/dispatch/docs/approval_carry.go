package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A human's answer to a decision block carries his approval forward. He approved the document at
// version A; the version his answer writes, N, differs from A only in its ask blocks, every block
// of N that is not as A held it - added since, answered or re-answered since, or resolved since -
// is one he answered, after his approval, and at least one is: nothing he has not agreed to is in
// N, and something of his is, so Dispatch records his approval of N in his name, in the
// transaction that writes N, rather than leaving his approval stale and asking him to approve
// again what he just decided (LEGION-573). N is compared with A alone: the versions between them
// are read for nothing, so a block that left and came back as A held it is no change of his, and a
// block A did not hold that comes back with an answer he gave before A is his but no action of his
// since, so it carries nothing either.
//
// Who answered what is read from the asks rows of this document, never from N's blocks: a peer on
// the document socket writes a block's attributes as it likes, and settlement writes them back
// from the row only for a block it can read. A block of N that no row of this document backs - a
// forged one, a copy (copied_from, whose row is another document's), or one whose update the room
// has not credited yet - or one settlement could not read (invalid), or a block id N repeats,
// which settlement reads as no block at all, leaves N's decisions unknown, and nothing is carried.
// Nor is anything carried while a block's server attributes are other than settlement's own
// writer (setAskServerAttributes) writes from its row - its state, who answered and when, and the
// answer itself, which the browser shows as the human's decision - save an answer settlement
// withheld (withholdAnswers), or while a decided block asks other wording than its row was decided
// on, which settlement writes back (reconcileAskBlocks): until it does, the block shows a decision
// nobody made, or one under words nobody decided.
//
// The version he approved, A, is held to the same row as N. A block of N is as A held it only
// while A holds a block under its id, read as N's blocks are (askNodes), and that stored block is
// as settlement's own writer leaves it from the row of N's block (blockAsSettlementLeavesIt: the
// whole server-owned set, and a decided row's wording, as for N). A stored block that is not -
// one showing a selection, an answer, a copy's provenance or a question other than its row's,
// approved before settlement wrote the row back over it - is a change of N's, which must then be
// his own answer since his approval, or nothing is carried. The row is the row at N's write, the
// only row there is, and holding A's block to it is exact: a decided row's wording is frozen (the
// edit route's ASK_NOT_OPEN), its selection and text move only with a new answer, which moves
// answered_at, and its state moves only with a resolution, so a stored block that is as the
// current row has it over the whole set was as the row stood when A was written, and one the row
// has moved away from since fails on answered_at or state (LEGION-752). A stored A that held one
// block id twice carries nothing either: its read-back (storedVersionTree) repairs the repeat,
// keeping the id on the first block and minting one for the next, so A shows a block N never had,
// and a block of A that no longer stands in N refuses the carry.

// carryReason is the reason a carried review records, naming the approval it follows.
const carryReason = "Carried forward from the approval of version %d: the answer to a decision block was the whole change since it."

// carryInput is what the rule is decided on once H, the version he approved, A, and when he
// approved it are known: the blocks of N by block id, each its row (rowBackedBlocks); the block
// ids of A, each whether its stored block is as that row has it (heldAsItsRow); and the two
// versions cut of their decision blocks (CutDecisionBlocks).
type carryInput struct {
	login            string
	approvedAt       time.Time
	blocks           map[string]model.Ask
	approved         map[string]bool
	approvedCut, cut string
}

// approvedVersion is a version a candidate approved as the rule reads it: its ask nodes as its
// stored markdown holds them, read as N's are (askNodes, each block id once), and its cut. Its
// nodes are held to the rows of N's blocks once those are known (heldAsItsRow): the rule for A
// is the rule for N, over the same rows.
type approvedVersion struct {
	nodes map[string]*pmdoc.Node
	cut   string
}

// carryApproval carries a human's approval to version, just written from rendered, when the
// answer he gave a decision block is the whole change since the version he approved (the rule
// above). It returns the events it appended - the open approval request's ask.answered, when one
// is open, then artifact.approved - and nothing when the version carries no approval. It reads
// rendered.tree and never changes it: the tree is the live document's.
func (s *Service) carryApproval(ctx context.Context, tx pgx.Tx, artifactID string, version model.Version, rendered renderedTree) ([]model.Event, error) {
	nodes, readable := askNodes(rendered.tree)
	if len(nodes) == 0 || !readable {
		return nil, nil
	}
	// Every version write runs this, and a document keeps its ask blocks for the rest of its life:
	// the rows are read only once a carry is possible at all, which needs an approval of the
	// document by someone.
	if approvedByAnyone, err := LatestApprovedReview(ctx, tx, artifactID); err != nil || approvedByAnyone == nil {
		return nil, err
	}
	rows, err := loadAskBlocks(ctx, tx, artifactID)
	if err != nil {
		return nil, err
	}
	blocks, backed := rowBackedBlocks(nodes, rows)
	if !backed {
		return nil, nil
	}
	review, err := s.carriedReview(ctx, tx, artifactID, version.Number, rendered.markdown, blocks)
	if err != nil || review == nil {
		return nil, err
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

// carriedReview is the review version number of artifactID, whose markdown is markdown and whose
// blocks are blocks, each its row, carries, or nil: the one review among the candidates' - each
// person who answered a block of N (answerers) - that stands as an approval (candidateApproval)
// and whose approval the rule carries (carryVerdict, with the approved version's blocks held to
// N's rows by heldAsItsRow). Two candidates whose approved versions straddle a block's absence
// can both pass; should two pass, nothing is carried rather than one guessed.
func (s *Service) carriedReview(ctx context.Context, tx pgx.Tx, artifactID string, number int, markdown string, blocks map[string]model.Ask) (*model.ArtifactReview, error) {
	candidates := answerers(blocks)
	if len(candidates) == 0 {
		return nil, nil
	}
	cutOnce := sync.OnceValues(func() (string, error) { return CutDecisionBlocks(markdown) })
	var carried *model.ArtifactReview
	for _, login := range candidates {
		review, approved, standing, err := s.candidateApproval(ctx, tx, artifactID, login)
		if err != nil {
			return nil, err
		}
		if !standing {
			continue
		}
		cut, err := cutOnce()
		if err != nil {
			return nil, skipUnreadableVersion(artifactID, number, err)
		}
		if !carryVerdict(carryInput{login: login, approvedAt: review.CreatedAt, blocks: blocks, approved: heldAsItsRow(approved.nodes, blocks), approvedCut: approved.cut, cut: cut}) {
			continue
		}
		if carried != nil {
			return nil, nil
		}
		carried = review
	}
	return carried, nil
}

// candidateApproval is login's standing approval of artifactID and the version it approved, A,
// as the rule reads it, and whether he has one: his latest review of the document must be an
// approval with no request for changes by anyone newer than it, and A readable.
func (s *Service) candidateApproval(ctx context.Context, tx pgx.Tx, artifactID, login string) (*model.ArtifactReview, approvedVersion, bool, error) {
	review, err := latestReviewBy(ctx, tx, artifactID, login)
	if err != nil || review == nil || review.State != "approved" {
		return nil, approvedVersion{}, false, err
	}
	requested, err := changesRequestedSince(ctx, tx, *review)
	if err != nil || requested {
		return nil, approvedVersion{}, false, err
	}
	approved, readable, err := s.readApprovedVersion(ctx, tx, artifactID, review.Version)
	if err != nil || !readable {
		return nil, approvedVersion{}, false, err
	}
	return review, approved, true, nil
}

// readApprovedVersion reads the stored version number of artifactID as the rule compares N with
// it (storedVersionTree), and whether it could be read. Its ask nodes are read as N's are
// (askNodes), which refuses a tree holding one block id twice - no measure of what he agreed to,
// as nothing is carried from an N that repeats an id. A stored version's read-back
// (pmdoc.ParseRendering) has already repaired such a repeat before this reads it, keeping the id
// on the first block and minting one for the next, so from a stored version the refusal is never
// reached: A then shows a block under the minted id that N never had, and carryVerdict refuses
// the carry for a block of A that no longer stands. Either way nothing is carried.
func (s *Service) readApprovedVersion(ctx context.Context, tx pgx.Tx, artifactID string, number int) (approvedVersion, bool, error) {
	tree, readable, err := s.storedVersionTree(ctx, tx, artifactID, number)
	if err != nil || !readable {
		return approvedVersion{}, false, err
	}
	nodes, readable := askNodes(tree)
	if !readable {
		return approvedVersion{}, false, nil
	}
	cut, err := renderWithoutAskBlocks(tree)
	if err != nil {
		return approvedVersion{}, false, skipUnreadableVersion(artifactID, number, err)
	}
	return approvedVersion{nodes: nodes, cut: cut}, true, nil
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
// item or another typed block; and whether each id is held once. A repeated id is one settlement
// reads no block under (collectAskBlocksForSettlement), so the nodes are not returned, and the
// caller carries nothing; a nil tree holds none. Both versions are read through it: N's live tree
// (carryApproval) and the stored tree of the version he approved, A (readApprovedVersion).
func askNodes(tree *pmdoc.Node) (map[string]*pmdoc.Node, bool) {
	var nodes map[string]*pmdoc.Node
	repeated := false
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if _, repeated = nodes[id]; repeated {
			return false
		}
		if nodes == nil {
			nodes = map[string]*pmdoc.Node{}
		}
		nodes[id] = node
		return true
	})
	if repeated {
		return nil, false
	}
	return nodes, true
}

// rowBackedBlocks is every ask node of N as its row, the row of this document that indexes it
// (rows, by block id), and whether every node is as settlement leaves it from that row
// (blockAsSettlementLeavesIt). A node no row backs leaves N's decisions unknown; a node whose
// server attributes are other than settlement's own writer writes from its row - a copy, a node
// settlement could not read, a forged or stale state, answerer, time, selection or answer - is a
// forgery or a stale write, and the block of a decided row that does not parse or asks other
// wording than the row was decided on shows the decision under words nobody decided. For any of
// them nothing is returned, and the caller carries nothing. An open row's body is free: its
// question is still being written. The rows returned are what the rule reads of N (answerers,
// carryVerdict), and the rows the blocks of A are held to in turn (heldAsItsRow).
func rowBackedBlocks(nodes map[string]*pmdoc.Node, rows map[string]model.Ask) (map[string]model.Ask, bool) {
	blocks := make(map[string]model.Ask, len(nodes))
	for id, node := range nodes {
		ask, backed := rows[id]
		if !backed || !blockAsSettlementLeavesIt(node, ask) {
			return nil, false
		}
		blocks[id] = ask
	}
	return blocks, true
}

// blockAsSettlementLeavesIt reports whether node is as settlement leaves a block of this document
// from its row, ask: its server attributes are what the writer, setAskServerAttributes, writes
// from the row (blockAsItsRowHasIt), with the answer or without it, as settlement leaves out an
// answer the document has no room for; and, for a row that is not open, its wording is what
// reconcileAskBlocks writes back over a decided block - the block parses (parseAskBlock) and asks
// what the row was decided on (askBlock.asks, settlement's own test). An open row's body is free:
// its question is still being written, and the row takes it. Both versions are held to it, each
// node of N against its own row (rowBackedBlocks) and each stored node of A against the row of
// N's block under the same id (heldAsItsRow), so a stored block that passes is as settlement
// would have left it from that row, and one that fails shows something settlement never wrote.
func blockAsSettlementLeavesIt(node *pmdoc.Node, ask model.Ask) bool {
	if !blockAsItsRowHasIt(node, ask) {
		return false
	}
	if ask.State == "open" {
		return true
	}
	block, err := parseAskBlock(node)
	return err == nil && block.asks(model.NewAskEditPrevious(ask))
}

// blockAsItsRowHasIt reports whether node's server attributes (askServerAttributeNames) are what
// settlement's own writer, setAskServerAttributes, writes for ask on a block of this document -
// with the answer, or without it, as settlement leaves out an answer the document has no room for
// (withholdAnswers): the writer, run on a copy of the attributes, changes nothing one way or the
// other. A copy's copied_from, an unreadable block's invalid, and a state, answerer, time,
// selection or answer other than the row's are each a change the writer would make.
func blockAsItsRowHasIt(node *pmdoc.Node, ask model.Ask) bool {
	for _, withholdAnswer := range [...]bool{false, true} {
		clone := &pmdoc.Node{Type: node.Type, Attrs: make(pmdoc.Attrs, len(node.Attrs)+len(askServerAttributeNames))}
		maps.Copy(clone.Attrs, node.Attrs)
		if changed, _ := setAskServerAttributes(clone, ask, "", withholdAnswer); !changed {
			return true
		}
	}
	return false
}

// heldAsItsRow is, for each block id of A (approved, its stored ask nodes), whether the stored
// block is as the row of N's block under that id has it (blockAsSettlementLeavesIt over blocks,
// N's rows): the blocks of A the rule counts as unchanged since his approval. The row at N's write
// is the only row there is, and holding A's stored block to it is exact rather than approximate: a
// decided row's wording is frozen (the edit route's ASK_NOT_OPEN, which only an open ask passes),
// its selection and text move only with a new answer, which moves answered_at, and its state
// moves only with a resolution, so a stored block that is as the current row has it over the
// whole server-owned set was as the row stood when A was written, and one the row has moved away
// from since fails on answered_at or state. An id of A that no block of N has a row for is false,
// and the writer is not run for it (no empty row reaches setAskServerAttributes); it decides no
// carry either way: either N holds a block under the id, and rowBackedBlocks refused N before
// this ran, or N does not, and carryVerdict refuses for a block of A that no longer stands.
func heldAsItsRow(approved map[string]*pmdoc.Node, blocks map[string]model.Ask) map[string]bool {
	held := make(map[string]bool, len(approved))
	for id, node := range approved {
		row, backed := blocks[id]
		held[id] = backed && blockAsSettlementLeavesIt(node, row)
	}
	return held
}

// answerers is everyone who answered a block of N, blocks, each its row, by canonical login
// (model.CanonicalLogin), each once, sorted: the candidates whose approval N may carry.
func answerers(blocks map[string]model.Ask) []string {
	var logins []string
	for _, block := range blocks {
		if block.State != "answered" || block.Answer == nil {
			continue
		}
		if login := model.CanonicalLogin(block.Answer.User); !slices.Contains(logins, login) {
			logins = append(logins, login)
		}
	}
	slices.Sort(logins)
	return logins
}

// carryVerdict is the rule over the rows alone: N carries H's approval of A when the two cut to
// the same text, every block of N that is not as A held it is answered by H after his approval, at
// least one block of N is not as A held it, and every block of A still stands in N. A block of N
// is as A held it when A holds a block under its id and that stored block is as the row has it
// (input.approved, heldAsItsRow): content he approved, whoever answered it before his approval,
// which stays out of the test. Every other block of N is a change of N's - one added since A, one
// answered or re-answered since by anyone, one resolved since, which is nobody's answer, or one
// whose stored block in A is not as its row has it - and must be H's own answer, given after his
// approval (the row's answered_at after the approval's time). So a block A showed other than its
// row carries when H himself answered it again after approving, his later answer being his action
// since whatever A showed, and carries nothing otherwise. A version whose every block A held as
// its row has it holds nothing new of his and carries nothing; and a block A did not hold, back
// with an answer he gave before his approval, is his but no action of his since, so it carries
// nothing either.
func carryVerdict(input carryInput) bool {
	if input.approvedCut != input.cut {
		return false
	}
	changed := false
	for id, block := range input.blocks {
		if input.approved[id] {
			continue
		}
		if block.State != "answered" || block.Answer == nil || model.CanonicalLogin(block.Answer.User) != input.login || !block.Answer.At.After(input.approvedAt) {
			return false
		}
		changed = true
	}
	if !changed {
		return false
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
