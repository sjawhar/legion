package docs

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// opIndex is the batch position of the operation an error came from. The API renders these
// errors from their own text rather than the wrapped chain, so each carries its own index; an
// error raised outside a batch leaves it nil and reads exactly as it did before.
type opIndex struct{ operation *int }

func (o *opIndex) setOperation(index int) { o.operation = &index }

func (o opIndex) prefix() string {
	if o.operation == nil {
		return ""
	}
	return fmt.Sprintf("operation %d: ", *o.operation)
}

// operationStamped is every error that renders its own operation index, so a new one joins by
// embedding opIndex rather than by being listed here.
type operationStamped interface {
	error
	setOperation(int)
}

// ErrInvalidOp identifies the malformed user-facing operation field.
type ErrInvalidOp struct {
	opIndex
	Field  string
	Reason string
}

func (e *ErrInvalidOp) Error() string {
	return fmt.Sprintf("%sinvalid document operation field %q: %s", e.prefix(), e.Field, e.Reason)
}

func invalidOp(field string) error {
	return &ErrInvalidOp{Field: field}
}

type ErrQuoteNotFound struct {
	opIndex
	Quote   string
	Nearest []string
	// Matches is how many places the quote did match, nonzero only when an occurrence was out
	// of range: the quote is right and the occurrence is not.
	Matches int
}

func (e *ErrQuoteNotFound) Error() string {
	if e.Matches > 0 {
		return fmt.Sprintf(
			"%squote %q matches %d places, so that occurrence is out of range; occurrence is zero-based; nearest blocks: %s",
			e.prefix(), e.Quote, e.Matches, pmdoc.QuoteBlocks(e.Nearest),
		)
	}
	return fmt.Sprintf(`%squote %q not found; quotes match the block text as rendered (inline markdown is tolerated; use "heading:<title>", "block:<id>", "start", or "end" as insert and move anchors); nearest blocks: %s`, e.prefix(), e.Quote, pmdoc.QuoteBlocks(e.Nearest))
}

func (e *ErrQuoteNotFound) Unwrap() error { return pmdoc.ErrTargetNotFound }

// ErrAnchorAmbiguous names a quote or a `heading:` anchor that resolved in several places, with
// the candidates the API hands back so the caller can pick one.
type ErrAnchorAmbiguous struct {
	opIndex
	// Kind is the noun the caller wrote: "quote" or "heading anchor".
	Kind       string
	Target     string
	Candidates []pmdoc.Candidate
}

func (e *ErrAnchorAmbiguous) Error() string {
	// A heading anchor matches whole headings, so there is no surrounding text to quote more of.
	advice := "pass a zero-based occurrence to choose one"
	if e.Kind == "quote" {
		advice = "pass a zero-based occurrence, or quote more of the surrounding text"
	}
	return fmt.Sprintf("%s%s %q matches %d places; %s", e.prefix(), e.Kind, e.Target, len(e.Candidates), advice)
}

// ErrQuoteSpansBlocks names a quote that reached across a block boundary. A quote-anchored edit
// stays inside one textblock, so the fix is to quote less, or to address the blocks by id.
type ErrQuoteSpansBlocks struct {
	opIndex
	Quote string
}

func (e *ErrQuoteSpansBlocks) Error() string {
	return fmt.Sprintf(
		"%squote %q spans more than one block; a quote-anchored edit stays inside one block, so quote text from a single block, or address whole blocks by id",
		e.prefix(), e.Quote,
	)
}

func (e *ErrQuoteSpansBlocks) Unwrap() error { return pmdoc.ErrTargetSpansBlocks }

// stampOperation gives index to an error that renders its own operation, and falls back to
// wrapping everything else with the same prefix.
func stampOperation(index int, err error) error {
	var stamped operationStamped
	if errors.As(err, &stamped) {
		stamped.setOperation(index)
		return err
	}
	return fmt.Errorf("operation %d: %w", index, err)
}

// isEditRefusal reports an error the caller wrote the batch wrong, whose own text is what the
// route serves. Wrapping one in the service's internal prose would bury the operation index and
// the anchor the reader needs.
func isEditRefusal(err error) bool {
	var invalid *ErrInvalidOp
	var ambiguous *ErrAnchorAmbiguous
	return errors.Is(err, pmdoc.ErrTargetNotFound) ||
		errors.Is(err, pmdoc.ErrTargetSpansBlocks) ||
		errors.Is(err, pmdoc.ErrTableWidth) ||
		errors.As(err, &invalid) ||
		errors.As(err, &ambiguous)
}

// ErrInvalidPrecondition identifies a malformed optimistic-concurrency guard.
type ErrInvalidPrecondition struct {
	Reason string
}

func (e *ErrInvalidPrecondition) Error() string {
	return "invalid document edit precondition: " + e.Reason
}

// ErrPreconditionBusy reports that a room's bounded conditional-edit queue is full.
var ErrPreconditionBusy = errors.New("document conditional edit queue is full")

// ErrPreconditionFailed reports a stale document or block token without changing the live tree.
type ErrPreconditionFailed struct {
	CurrentDocument string
	Mismatches      []model.EditPreconditionMismatch
}

func (e *ErrPreconditionFailed) Error() string {
	reasons := make([]string, 0, len(e.Mismatches))
	for _, mismatch := range e.Mismatches {
		if mismatch.Scope == "document" {
			reasons = append(reasons, "document changed")
			continue
		}
		if mismatch.Current == nil {
			reasons = append(reasons, fmt.Sprintf("block %q was removed", mismatch.BlockID))
			continue
		}
		reasons = append(reasons, fmt.Sprintf("block %q changed", mismatch.BlockID))
	}
	return "document edit precondition failed: " + strings.Join(reasons, ", ")
}

// nodeToken hashes a canonical semantic Proof representation, including inline
// marks but excluding stable block identity and server-owned typed attributes.
func nodeToken(node *pmdoc.Node) (string, error) {
	encoded, err := node.TokenJSON()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(encoded)), nil
}

func blockTokens(tree *pmdoc.Node) (map[string]string, error) {
	tokens := make(map[string]string)
	var visit func(*pmdoc.Node) error
	visit = func(node *pmdoc.Node) error {
		if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); id != "" {
			token, err := nodeToken(node)
			if err != nil {
				return err
			}
			tokens[id] = token
		}
		for _, child := range node.Children {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(tree); err != nil {
		return nil, err
	}
	return tokens, nil
}

func validateEditPrecondition(precondition model.EditPrecondition) error {
	hasDocument := precondition.Document != ""
	hasBlocks := len(precondition.Blocks) > 0
	if hasDocument == hasBlocks {
		return &ErrInvalidPrecondition{Reason: "provide exactly one of document or blocks"}
	}
	if hasDocument {
		return nil
	}
	seen := make(map[string]struct{}, len(precondition.Blocks))
	for _, block := range precondition.Blocks {
		if block.ID == "" || block.Token == "" {
			return &ErrInvalidPrecondition{Reason: "each block needs id and token"}
		}
		if _, duplicate := seen[block.ID]; duplicate {
			return &ErrInvalidPrecondition{Reason: fmt.Sprintf("block %q appears more than once", block.ID)}
		}
		seen[block.ID] = struct{}{}
	}
	return nil
}

func checkEditPrecondition(tree *pmdoc.Node, precondition model.EditPrecondition) error {
	if err := validateEditPrecondition(precondition); err != nil {
		return err
	}
	currentDocument, err := nodeToken(tree)
	if err != nil {
		return err
	}
	if precondition.Document != "" {
		if precondition.Document == currentDocument {
			return nil
		}
		return &ErrPreconditionFailed{
			CurrentDocument: currentDocument,
			Mismatches: []model.EditPreconditionMismatch{{
				Scope: "document", Expected: precondition.Document, Current: &currentDocument,
			}},
		}
	}

	current, err := blockTokens(tree)
	if err != nil {
		return err
	}
	mismatches := make([]model.EditPreconditionMismatch, 0)
	for _, expected := range precondition.Blocks {
		token, found := current[expected.ID]
		if !found {
			mismatches = append(mismatches, model.EditPreconditionMismatch{
				Scope: "block", BlockID: expected.ID, Expected: expected.Token,
			})
			continue
		}
		if expected.Token != token {
			currentToken := token
			mismatches = append(mismatches, model.EditPreconditionMismatch{
				Scope: "block", BlockID: expected.ID, Expected: expected.Token, Current: &currentToken,
			})
		}
	}
	if len(mismatches) == 0 {
		return nil
	}
	return &ErrPreconditionFailed{CurrentDocument: currentDocument, Mismatches: mismatches}
}

func requirePreconditionCoverage(tree *pmdoc.Node, ops []model.EditOp, precondition model.EditPrecondition) error {
	if precondition.Document != "" {
		return nil
	}
	covered := make(map[string]struct{}, len(precondition.Blocks))
	for _, block := range precondition.Blocks {
		covered[block.ID] = struct{}{}
	}
	required := make(map[string]struct{})
	_, err := applyOperationsWithValidation(tree, ops, func(current *pmdoc.Node, op model.EditOp) error {
		ids, err := operationPreconditionBlocks(current, op)
		if err != nil {
			return err
		}
		for _, id := range ids {
			required[id] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return err
	}
	missing := make([]string, 0)
	for id := range required {
		if _, found := covered[id]; !found {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return &ErrInvalidPrecondition{Reason: "blocks must include tokens for resolved blocks: " + strings.Join(missing, ", ")}
}

func operationPreconditionBlocks(tree *pmdoc.Node, op model.EditOp) ([]string, error) {
	switch op.Op {
	case "replace":
		return quotePreconditionBlock(tree, op.Find, op.Occurrence)
	case "delete":
		if op.Block != "" {
			if _, err := pmdoc.BlockRange(tree, op.Block); err != nil {
				return nil, err
			}
			return []string{op.Block}, nil
		}
		return quotePreconditionBlock(tree, op.Find, op.Occurrence)
	case "retype", "delete_row", "delete_column":
		if _, err := pmdoc.BlockRange(tree, op.Block); err != nil {
			return nil, err
		}
		return []string{op.Block}, nil
	case "insert", "move":
		return nil, &ErrInvalidPrecondition{Reason: "document token is required for " + op.Op}
	default:
		return nil, nil
	}
}

func quotePreconditionBlock(tree *pmdoc.Node, quote string, occurrence *int) ([]string, error) {
	range_, err := findEditQuote(tree, "find", quote, occurrence)
	if err != nil {
		return nil, err
	}
	blockID, err := pmdoc.BlockIDForRange(tree, range_)
	if err != nil {
		return nil, err
	}
	if blockID == "" {
		return nil, &ErrInvalidPrecondition{Reason: "document token is required for a quote spanning blocks"}
	}
	return []string{blockID}, nil
}

type batchRemoval struct {
	operation int
	parent    string
	cascaded  bool
}

type operationValidator func(*pmdoc.Node, model.EditOp) error

func removedBlockError(blockID string, removal batchRemoval) error {
	reason := fmt.Sprintf(`block %q was removed by operation %d with delete {block:%q}`, blockID, removal.operation, removal.parent)
	if removal.cascaded {
		reason = fmt.Sprintf(`block %q was removed by operation %d as a cascade of delete {block:%q}`, blockID, removal.operation, removal.parent)
	}
	return &ErrInvalidOp{Field: "block", Reason: reason + "; remove it from the atomic batch"}
}

// editBatch is what a resolved batch of operations produced: the tree to write, the token of the
// document before the batch, every operation that left the tree as it was, and what each
// operation wrote (see writes).
type editBatch struct {
	tree       *pmdoc.Node
	beforeTree *pmdoc.Node
	before     string
	unchanged  []int
	// operations is how many operations the batch resolved, which is what tells the one-operation
	// batch - every insertion is that operation's, with nothing to diff - from the rest.
	operations int
	written    []operationWrite
}

// operationWrite is one operation's W: the blocks whose own inline content it wrote. A block the
// batch created without an id of its own is not one: EnsureBlockIDs stamps the tree before the
// write, so every block the update reaches carries an id, and a block that still has none holds
// no text a caller could address (editBatch.writes attributes it to the batch).
type operationWrite struct {
	ids []string
}

// writes resolves each operation's W: which of the blocks the write actually inserted text into
// (candidates, read from the Yjs update the batch made) each operation wrote. It runs after the
// caller's EnsureBlockIDs, which is where a block an operation created without an id gets one.
//
// A one-operation batch needs no diff at all: every run that operation's own update inserted is
// that operation's, whichever block it landed in. That is the case the uncontended path pays for,
// so the batch does not walk the tree for it (applyOperationsWithValidation).
//
// Every candidate is attributed, including the ones no operation's own text change claims. A
// batch writes one Yjs update, and pmdoc.Update rewrites a block it passes over in place when an
// operation inserts or removes a block above it - that untouched paragraph's text is re-inserted
// under the same block id, and a concurrent deletion can take it exactly as it takes an
// operation's own. Nothing in the batch can say which operation caused a collateral rewrite, and
// the batch is atomic, so each such block belongs to every operation: dropping it instead would
// let the paragraph vanish while the edit answered success, which is the report this whole check
// exists to prevent.
func (b editBatch) writes(candidates map[string]struct{}) []map[string]struct{} {
	if b.operations == 1 {
		return []map[string]struct{}{candidates}
	}
	written := make([]map[string]struct{}, len(b.written))
	claimed := map[string]struct{}{}
	for index, write := range b.written {
		written[index] = make(map[string]struct{}, len(write.ids))
		for _, id := range write.ids {
			if _, wanted := candidates[id]; !wanted {
				continue
			}
			written[index][id] = struct{}{}
			claimed[id] = struct{}{}
		}
	}
	for id := range candidates {
		if _, named := claimed[id]; named {
			continue
		}
		for index := range written {
			written[index][id] = struct{}{}
		}
	}
	return written
}

// outcome is the batch's verdict, taken once the caller has stamped the block ids the write
// carries. It compares nodeToken, the same semantic identity the precondition machinery uses:
// canonical markdown renders no anchor mark, so a batch that orphans a human's comment anchor
// while leaving the words alone is a change here. Whether it becomes a version is the caller's:
// with a summary the edit route names one through NamedVersion, which versions unconditionally,
// so such a batch writes a version whose markdown equals the previous one's and stales an
// approval pinned to it; without a summary SnapshotVersion compares renderings and writes
// nothing. LEGION-260's follow-up makes the route version on the rendered markdown and keep
// this verdict as its report to the agent. That same post-batch token is the verdict's Token,
// the document precondition the caller's next edit passes.
func (b editBatch) outcome(applied int) (EditOutcome, error) {
	after, err := nodeToken(b.tree)
	if err != nil {
		return EditOutcome{}, err
	}
	return EditOutcome{
		Applied:        applied,
		Changed:        after != b.before,
		Unchanged:      b.unchanged,
		Token:          after,
		AskBlocksAdded: addedAskBlocks(b.beforeTree, b.tree),
	}, nil
}

// applyOperations applies each operation to its predecessor's tree so a
// following operation resolves the structure created by the preceding one.
func applyOperations(tree *pmdoc.Node, ops []model.EditOp) (editBatch, error) {
	return applyOperationsWithValidation(tree, ops, nil)
}

// applyOperationsWithValidation applies ops as applyOperations does, running validate before each.
// The batch is one caller write, so its operations' markdown and table rows spend one table-padding
// budget; each run of a batch - a conditional one runs to check its anchors and preconditions as
// well as to apply - takes its own, so no run charges the batch's padding twice.
func applyOperationsWithValidation(tree *pmdoc.Node, ops []model.EditOp, validate operationValidator) (editBatch, error) {
	beforeTree := tree
	budget := pmdoc.NewTablePaddingBudget()
	before, err := nodeToken(tree)
	if err != nil {
		return editBatch{}, err
	}
	removed := make(map[string]batchRemoval)
	var unchanged []int
	written := make([]operationWrite, len(ops))
	// Which blocks each operation wrote, told apart only when there is more than one operation to
	// tell apart: blockText is every block's own inline content, carried forward operation by
	// operation so each is compared with the tree its predecessor left rather than with the
	// batch's input. A walk of a thousand-block tree per operation is real work on the
	// uncontended path, and a one-operation batch needs none of it (editBatch.writes).
	tellOperationsApart := len(ops) > 1
	var blockText map[string]string
	if tellOperationsApart {
		blockText = pmdoc.BlockText(tree)
	}
	for index, op := range ops {
		if removal, alreadyRemoved := removed[op.Block]; op.Block != "" && alreadyRemoved {
			return editBatch{}, stampOperation(index, removedBlockError(op.Block, removal))
		}
		if validate != nil {
			if err := validate(tree, op); err != nil {
				return editBatch{}, stampOperation(index, err)
			}
		}
		var removedIDs []string
		if op.Op == "delete" && op.Block != "" && op.Find == "" {
			if _, alreadyRemoved := removed[op.Block]; !alreadyRemoved {
				var err error
				removedIDs, err = pmdoc.BlockDescendantIDs(tree, op.Block)
				if err != nil {
					return editBatch{}, stampOperation(index, err)
				}
			}
		}
		next, err := applyOperation(tree, op, budget)
		if err != nil {
			return editBatch{}, stampOperation(index, err)
		}
		if next.Equal(tree) {
			unchanged = append(unchanged, index)
		}
		if tellOperationsApart {
			written[index].ids, blockText = pmdoc.BlocksGainingText(blockText, next)
		}
		for _, blockID := range removedIDs {
			removed[blockID] = batchRemoval{
				operation: index,
				parent:    op.Block,
				cascaded:  blockID != op.Block,
			}
		}
		tree = next
	}
	return editBatch{
		tree:       tree,
		beforeTree: beforeTree,
		before:     before,
		unchanged:  unchanged,
		operations: len(ops),
		written:    written,
	}, nil
}

func hasTableAnchorMutation(ops []model.EditOp) bool {
	for _, op := range ops {
		if op.Op == "delete_row" || op.Op == "delete_column" {
			return true
		}
	}
	return false
}

type tableAnchorSnapshot struct {
	signature string
	axis      string
	index     int
	markIDs   []string
}

func tableAnchorCheck(tree *pmdoc.Node, op model.EditOp) (tableAnchorSnapshot, []pmdoc.MarkRef, bool, error) {
	var (
		axis  string
		index int
		marks []pmdoc.MarkRef
		err   error
	)
	switch op.Op {
	case "delete_row":
		if op.Block == "" {
			return tableAnchorSnapshot{}, nil, false, nil
		}
		index, err = tableIndex(tree, op.Block, "row", op.Index)
		if err == nil {
			marks, err = pmdoc.TableRowMarks(tree, op.Block, index)
		}
		axis = "row"
	case "delete_column":
		if op.Block == "" {
			return tableAnchorSnapshot{}, nil, false, nil
		}
		index, err = tableIndex(tree, op.Block, "column", op.Index)
		if err == nil {
			marks, err = pmdoc.TableColumnMarks(tree, op.Block, index)
		}
		axis = "column"
	default:
		return tableAnchorSnapshot{}, nil, false, nil
	}
	if err != nil {
		return tableAnchorSnapshot{}, nil, true, invalidTableIndexOp(err)
	}
	refs := make([]string, len(marks))
	markIDs := make([]string, 0, len(marks))
	for index, mark := range marks {
		refs[index] = mark.Type + ":" + mark.ID
		switch mark.Type {
		case string(MarkAsk), string(MarkComment), string(MarkSuggestion):
			markIDs = append(markIDs, mark.ID)
		}
	}
	sort.Strings(refs)
	sort.Strings(markIDs)
	return tableAnchorSnapshot{
		signature: axis + ":" + op.Block + ":" + strconv.Itoa(index) + ":" + strings.Join(refs, ", "),
		axis:      axis,
		index:     index,
		markIDs:   markIDs,
	}, marks, true, nil
}

func (s *Service) prevalidateOperations(ctx context.Context, artifactID string, tree *pmdoc.Node, ops []model.EditOp) ([]tableAnchorSnapshot, error) {
	snapshots := make([]tableAnchorSnapshot, 0)
	_, err := applyOperationsWithValidation(tree, ops, func(current *pmdoc.Node, op model.EditOp) error {
		snapshot, marks, table, err := tableAnchorCheck(current, op)
		if err != nil || !table {
			return err
		}
		if err := s.rejectLiveTableAnchors(ctx, artifactID, snapshot.axis, snapshot.index, marks); err != nil {
			return err
		}
		if err := s.rejectUnindexedTableMarks(ctx, artifactID, snapshot.axis, snapshot.index, marks); err != nil {
			return err
		}
		snapshots = append(snapshots, snapshot)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return snapshots, nil
}

func (s *Service) rejectUnindexedTableMarks(ctx context.Context, artifactID, axis string, index int, marks []pmdoc.MarkRef) error {
	markIDs := make([]string, 0, len(marks))
	for _, mark := range marks {
		switch mark.Type {
		case string(MarkAsk), string(MarkComment), string(MarkSuggestion):
			markIDs = append(markIDs, mark.ID)
		}
	}
	if len(markIDs) == 0 {
		return nil
	}
	rows, err := s.queryFrom(ctx).Query(ctx, `
		select anchor->>'mark_id' from asks
		where anchor->>'artifact_id' = $1 and anchor->>'mark_id' = any($2::text[])
		union
		select anchor->>'mark_id' from comments
		where anchor->>'artifact_id' = $1 and anchor->>'mark_id' = any($2::text[])
	`, artifactID, markIDs)
	if err != nil {
		return fmt.Errorf("list table anchor records: %w", err)
	}
	defer rows.Close()
	indexed := make(map[string]struct{}, len(markIDs))
	for rows.Next() {
		var markID string
		if err := rows.Scan(&markID); err != nil {
			return fmt.Errorf("scan table anchor record: %w", err)
		}
		indexed[markID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate table anchor records: %w", err)
	}
	var missing []string
	for _, markID := range markIDs {
		if _, found := indexed[markID]; !found {
			missing = append(missing, markID)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return &ErrInvalidOp{Field: "index", Reason: fmt.Sprintf("%s index %d has unindexed anchor marks: %s", axis, index, strings.Join(missing, ", "))}
}

func verifyTableAnchorSnapshots(tree *pmdoc.Node, ops []model.EditOp, snapshots []tableAnchorSnapshot) error {
	index := 0
	_, err := applyOperationsWithValidation(tree, ops, func(current *pmdoc.Node, op model.EditOp) error {
		snapshot, _, table, err := tableAnchorCheck(current, op)
		if err != nil || !table {
			return err
		}
		if index >= len(snapshots) || snapshots[index].signature != snapshot.signature {
			return &ErrInvalidOp{Field: "index", Reason: "table anchors changed during validation; retry"}
		}
		index++
		return nil
	})
	if err != nil {
		return err
	}
	if index != len(snapshots) {
		return &ErrInvalidOp{Field: "index", Reason: "table anchors changed during validation; retry"}
	}
	return nil
}

func (s *Service) applyOperations(ctx context.Context, artifactID string, tree *pmdoc.Node, ops []model.EditOp) (editBatch, error) {
	return applyOperationsWithValidation(tree, ops, func(tree *pmdoc.Node, op model.EditOp) error {
		return s.validateTableEditAnchors(ctx, artifactID, tree, op)
	})
}

func (s *Service) validateTableEditAnchors(ctx context.Context, artifactID string, tree *pmdoc.Node, op model.EditOp) error {
	var (
		axis  string
		index int
		marks []pmdoc.MarkRef
		err   error
	)
	switch op.Op {
	case "delete_row":
		if op.Block == "" {
			return nil
		}
		index, err = tableIndex(tree, op.Block, "row", op.Index)
		if err == nil {
			marks, err = pmdoc.TableRowMarks(tree, op.Block, index)
		}
		axis = "row"
	case "delete_column":
		if op.Block == "" {
			return nil
		}
		index, err = tableIndex(tree, op.Block, "column", op.Index)
		if err == nil {
			marks, err = pmdoc.TableColumnMarks(tree, op.Block, index)
		}
		axis = "column"
	default:
		return nil
	}
	if err != nil {
		return invalidTableIndexOp(err)
	}
	return s.rejectLiveTableAnchors(ctx, artifactID, axis, index, marks)
}

func (s *Service) rejectLiveTableAnchors(ctx context.Context, artifactID, axis string, index int, marks []pmdoc.MarkRef) error {
	markIDs := make([]string, 0, len(marks))
	for _, mark := range marks {
		switch mark.Type {
		case string(MarkAsk), string(MarkComment), string(MarkSuggestion):
			markIDs = append(markIDs, mark.ID)
		}
	}
	if len(markIDs) == 0 {
		return nil
	}
	rows, err := s.queryFrom(ctx).Query(ctx, `
		select 'ask', id::text, anchor->>'mark_id'
		from asks
		where anchor->>'artifact_id' = $1 and state = 'open' and anchor->>'mark_id' = any($2::text[])
		union all
		select 'comment', id::text, anchor->>'mark_id'
		from comments
		where anchor->>'artifact_id' = $1 and not resolved and anchor->>'mark_id' = any($2::text[])
		order by 1, 2
	`, artifactID, markIDs)
	if err != nil {
		return fmt.Errorf("list active table anchors: %w", err)
	}
	defer rows.Close()
	var anchors []string
	for rows.Next() {
		var kind, id, markID string
		if err := rows.Scan(&kind, &id, &markID); err != nil {
			return fmt.Errorf("scan active table anchor: %w", err)
		}
		anchors = append(anchors, fmt.Sprintf("%s %s (anchor %s)", kind, id, markID))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate active table anchors: %w", err)
	}
	if len(anchors) == 0 {
		return nil
	}
	return &ErrInvalidOp{
		Field:  "index",
		Reason: fmt.Sprintf("%s index %d would remove active anchors: %s", axis, index, strings.Join(anchors, ", ")),
	}
}

// applyOperation applies op to tree, padding the tables and table rows it writes on budget, its
// batch's.
func applyOperation(tree *pmdoc.Node, op model.EditOp, budget *pmdoc.TablePaddingBudget) (*pmdoc.Node, error) {
	// Every text an operation writes - a replace's with, an insert's markdown, whether it becomes
	// blocks or table rows, and a retype's attributes - reaches the document with line feeds
	// alone (pmdoc.LineFeeds), before any check below reads it.
	op.With = pmdoc.LineFeeds(op.With)
	op.Markdown = pmdoc.LineFeeds(op.Markdown)
	op.Attributes = pmdoc.LineFeedAttrs(op.Attributes)
	switch op.Op {
	case "replace":
		if op.Find == "" {
			return nil, invalidOp("find")
		}
		r, err := findEditQuote(tree, "find", op.Find, op.Occurrence)
		if err != nil {
			return nil, err
		}
		at, _ := pmdoc.ContainingTextblock(tree, r.From)
		code := at.Node.Type == "code_block"
		var with *pmdoc.Node
		level := 0
		if code {
			with = codeReplacement(op.With)
		} else {
			var replacement string
			if replacement, level, err = replacementMarkdown(tree, r, op.Find, op.With); err != nil {
				return nil, err
			}
			if with, err = inlineReplacement(replacement, edgesOf(at, r)); err != nil {
				return nil, err
			}
		}
		if hasHardBreak(with) && at.OneLine() {
			return nil, &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf(
				"with %q carries a hard break, which a heading or a table cell cannot hold: it is written on one line, so the break would end the block there; write the text without the break, or insert a new block after this one",
				op.With,
			)}
		}
		// The text written over the match stays inside every ask and comment anchor the match lay
		// wholly inside, so each anchor's quote is still the whole text it covers.
		pmdoc.AddMarks(with, pmdoc.AnchorMarksCovering(tree, r))
		next, err := pmdoc.Splice(tree, r, with)
		err = invalidSchemaOp("with", err)
		if err == nil && level != 0 {
			next, err = pmdoc.SetHeadingLevel(next, r.From, level)
		}
		if err != nil {
			return nil, err
		}
		if code {
			return next, refuseCodeThatReshapesItsBlock(tree, next, r, at, "with", op.With)
		}
		if err := refuseUnreadableReplacement(tree, next, r, op.With); err != nil {
			return nil, err
		}
		return next, refuseReshapedReplacement(tree, next, r, op.With)
	case "delete":
		if op.Block != "" {
			if op.Find != "" {
				return nil, &ErrInvalidOp{Field: "find", Reason: "delete takes find or block, not both"}
			}
			out, err := pmdoc.DeleteBlock(tree, op.Block)
			return out, invalidSchemaOp("block", err)
		}
		if op.Find == "" {
			return nil, invalidOp("find or block")
		}
		r, err := findEditQuote(tree, "find", op.Find, op.Occurrence)
		if err != nil {
			return nil, err
		}
		if out, removed, err := pmdoc.DeleteTextblock(tree, r); err != nil || removed {
			return out, invalidSchemaOp("find", err)
		}
		empty, err := parseInput("")
		if err != nil {
			return nil, err
		}
		out, err := pmdoc.Splice(tree, r, empty)
		return out, invalidSchemaOp("find", err)
	case "insert":
		if op.Markdown == "" {
			return nil, invalidOp("markdown")
		}
		anchor, after, err := anchorOf(op)
		if err != nil {
			return nil, err
		}
		target, plainText, err := insertTarget(tree, anchorField(after), anchor, op.Occurrence)
		if err != nil {
			return nil, err
		}
		if plainText && pmdoc.TargetSpansBlocks(tree, target) {
			return nil, &ErrQuoteSpansBlocks{Quote: anchor}
		}
		// Rows first, judged against the target table's width; the block path below reads the
		// fragment as a document of its own, whose tables are its own.
		if out, inserted, err := pmdoc.InsertTableRows(tree, target, op.Markdown, after, budget); err != nil || inserted {
			return out, invalidSchemaOp("markdown", err)
		}
		position := target.From
		if after {
			position = target.To
		}
		if anchor != "start" && anchor != "end" {
			position, err = pmdoc.BlockBoundary(tree, target, after)
			if err != nil {
				return nil, err
			}
		}
		// A document with nothing in it holds one empty paragraph, which the insert takes the place
		// of rather than leaving an empty line beside what it writes, so it lands at the start.
		at := pmdoc.Range{From: position, To: position}
		if pmdoc.EmptyDocument(tree) {
			at = pmdoc.Range{From: 0, To: pmdoc.Size(tree)}
		}
		// Front matter opens only the document's start, so only there does the insert read it.
		with, err := parseFragmentInput(op.Markdown, opensDocument(tree, at.From), budget)
		if err != nil {
			return nil, invalidMarkdownOp("markdown", err)
		}
		out, err := pmdoc.Splice(tree, at, with)
		if err != nil {
			return nil, invalidSchemaOp("markdown", err)
		}
		if err := pmdoc.RepeatedBlockID(tree, out, with); err != nil {
			return nil, &ErrInvalidOp{Field: "markdown", Reason: err.Error()}
		}
		// What is stored is the document's rendering, so an insert that leaves it reading back
		// otherwise than it did would store another document than the one it wrote.
		if err := pmdoc.RefuseMisreadWrite(tree, out); err != nil {
			return nil, invalidSchemaOp("markdown", err)
		}
		return out, nil
	case "delete_row":
		if op.Block == "" {
			return nil, invalidOp("block")
		}
		index, err := tableIndex(tree, op.Block, "row", op.Index)
		if err != nil {
			return nil, err
		}
		out, err := pmdoc.DeleteTableRow(tree, op.Block, index)
		return out, invalidTableIndexOp(err)
	case "delete_column":
		if op.Block == "" {
			return nil, invalidOp("block")
		}
		index, err := tableIndex(tree, op.Block, "column", op.Index)
		if err != nil {
			return nil, err
		}
		out, err := pmdoc.DeleteTableColumn(tree, op.Block, index)
		return out, invalidTableIndexOp(err)
	case "move":
		if op.Block == "" {
			return nil, invalidOp("block")
		}
		anchor, after, err := anchorOf(op)
		if err != nil {
			return nil, err
		}
		field := anchorField(after)
		target, _, err := insertTarget(tree, field, anchor, op.Occurrence)
		if err != nil {
			return nil, err
		}
		// The document edges are empty ranges; MoveBlock lands on the edge they name.
		switch anchor {
		case "start":
			after = false
		case "end":
			after = true
		}
		out, err := pmdoc.MoveBlock(tree, op.Block, target, after)
		if errors.Is(err, pmdoc.ErrMoveInsideItself) {
			return nil, &ErrInvalidOp{Field: field, Reason: err.Error()}
		}
		if errors.Is(err, pmdoc.ErrSchema) {
			return nil, &ErrInvalidOp{Field: "block", Reason: err.Error()}
		}
		return out, err
	case "retype":
		if op.Block == "" {
			return nil, invalidOp("block")
		}
		if op.Type == "" {
			return nil, invalidOp("type")
		}
		out, err := pmdoc.RetypeBlock(tree, op.Block, op.Type, pmdoc.Attrs(op.Attributes))
		if errors.Is(err, pmdoc.ErrSchema) {
			field := "attributes"
			switch {
			case errors.Is(err, pmdoc.ErrUnknownBlockType):
				field = "type"
			case errors.Is(err, pmdoc.ErrBlockNotRetypable):
				field = "block"
			}
			return nil, &ErrInvalidOp{Field: field, Reason: err.Error()}
		}
		return out, err
	default:
		return nil, invalidOp("op")
	}
}

// anchorOf returns the one insert or move anchor an operation names.
func anchorOf(op model.EditOp) (anchor string, after bool, err error) {
	if (op.After == "" && op.Before == "") || (op.After != "" && op.Before != "") {
		return "", false, invalidOp("after or before")
	}
	if op.After != "" {
		return op.After, true, nil
	}
	return op.Before, false, nil
}

// anchorField names the operation field an anchor came from, for the errors it produces.
func anchorField(after bool) string {
	if after {
		return "after"
	}
	return "before"
}

// replacementMarkdown resolves `with` against the marker the matched block already renders.
// A replace is inline, so a `with` that opens with the block's own marker would write that
// marker twice (`## ##`, `7. 7\.`, `4. 4\.`, `-    - `). A heading
// rename is the one shape that keeps working: `find` carried the marker through the match, so
// an identical one in `with` is the block's, and it is dropped. Every other repetition is
// refused, and a marker of a different kind stays the literal text it has always been.
func replacementMarkdown(tree *pmdoc.Node, r pmdoc.Range, find, with string) (text string, level int, err error) {
	own, ok := pmdoc.MarkerAt(tree, r.From)
	if !ok || own.Kind == pmdoc.MarkerNone {
		return with, 0, nil
	}
	written, width := pmdoc.LeadingBlockMarker(with)
	if written.Kind != own.Kind {
		return with, 0, nil
	}
	// A `find` that carried the heading's own marker is renaming that heading, so the marker in
	// `with` is the block's and is dropped. A different level applies only when `find` named the
	// block's actual level: the caller has then shown they know what it is. `# ` is the documented
	// level-blind way to select a heading, so a generic `find` renames the text and keeps the
	// level it selected.
	if own.Kind == pmdoc.MarkerHeading && pmdoc.HeadingMarker(find) != "" {
		selector, _ := pmdoc.LeadingBlockMarker(find)
		if selector.Level == own.Level && written.Level != own.Level {
			return with[width:], written.Level, nil
		}
		return with[width:], 0, nil
	}
	return "", 0, &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf(
		"with begins with a marker of the same kind as the matched block's own (%s, which the block renders as %q), so the result would carry it twice; omit the marker to replace the block's text, backslash-escape it (%s) to keep prose that merely looks like a marker, or use insert plus delete to change the block's kind, level or number",
		written.Kind, own.Markdown(), escapedMarkerExample(with, width),
	)}
}

// escapedMarkerExample is the caller's own text with a backslash before the character that makes
// its opening a marker, which is how prose that merely looks like one (`1999. was a year`) is
// written as text. Everything else, the separator the marker needs included, stays as written: an
// example the caller cannot paste back verbatim teaches the wrong escape.
func escapedMarkerExample(with string, width int) string {
	marker := with[:width]
	cut := strings.IndexAny(marker, "#-*+")
	if punctuation := strings.IndexAny(marker, ".)"); punctuation >= 0 {
		cut = punctuation
	}
	if cut < 0 {
		return "`" + with + "`"
	}
	return "`" + marker[:cut] + `\` + marker[cut:] + with[width:] + "`"
}

// invalidSchemaOp reports a tree operation that would leave a container outside
// its content rule as an invalid operation on the field that named the target.
func invalidSchemaOp(field string, err error) error {
	if errors.Is(err, pmdoc.ErrSchema) {
		return &ErrInvalidOp{Field: field, Reason: err.Error()}
	}
	return err
}

func invalidTableIndexOp(err error) error {
	var invalidIndex *pmdoc.TableIndexError
	if errors.As(err, &invalidIndex) {
		return &ErrInvalidOp{Field: "index", Reason: err.Error()}
	}
	return invalidSchemaOp("block", err)
}

func tableIndex(tree *pmdoc.Node, blockID, axis string, raw json.RawMessage) (int, error) {
	supplied := strings.TrimSpace(string(raw))
	if supplied == "" {
		return 0, invalidTableIndex(tree, blockID, axis, "missing", "index is required")
	}
	if supplied == "0" {
		return 0, nil
	}
	if supplied[0] < '1' || supplied[0] > '9' {
		return 0, invalidTableIndex(tree, blockID, axis, supplied, fmt.Sprintf("index %s must be a non-negative integer", supplied))
	}
	for _, character := range supplied[1:] {
		if character < '0' || character > '9' {
			return 0, invalidTableIndex(tree, blockID, axis, supplied, fmt.Sprintf("index %s must be a non-negative integer", supplied))
		}
	}
	index, err := strconv.Atoi(supplied)
	if err != nil {
		return 0, invalidTableIndex(tree, blockID, axis, supplied, fmt.Sprintf("index %s must be a non-negative integer", supplied))
	}
	return index, nil
}

func invalidTableIndex(tree *pmdoc.Node, blockID, axis, supplied, problem string) error {
	return invalidTableIndexOp(pmdoc.TableIndexErrorFor(tree, blockID, axis, supplied, problem))
}

func invalidMarkdownOp(field string, err error) error {
	if errors.Is(err, ErrInvalidMarkdown) {
		return &ErrInvalidOp{Field: field, Reason: err.Error()}
	}
	return err
}

// findEditQuote resolves the quote field names, so a miss reports the field the caller wrote.
func findEditQuote(tree *pmdoc.Node, field, quote string, occurrence *int) (pmdoc.Range, error) {
	r, err := resolveEditQuote(tree, field, quote, occurrence)
	if err != nil {
		return pmdoc.Range{}, err
	}
	if pmdoc.TargetSpansBlocks(tree, r) {
		return pmdoc.Range{}, &ErrQuoteSpansBlocks{Quote: quote}
	}
	return r, nil
}

func resolveEditQuote(tree *pmdoc.Node, field, quote string, occurrence *int) (pmdoc.Range, error) {
	r, err := pmdoc.FindQuote(tree, quote, occurrence, nil)
	if err == nil {
		return r, nil
	}
	var missing *pmdoc.ErrQuoteNotFound
	if errors.As(err, &missing) {
		if delimiter, unbalanced := pmdoc.UnbalancedInlineMark(quote); unbalanced {
			// The quote may simply be wrong about the text and merely carry an odd delimiter, so
			// the nearest blocks that would fix an ordinary miss stay in the message.
			return pmdoc.Range{}, &ErrInvalidOp{Field: field, Reason: fmt.Sprintf(
				"inline marks in %s must be balanced: %q opens a span the quote never closes, so it cannot match the text as rendered; quote the whole marked span or none of it; nearest blocks: %s",
				field, delimiter, pmdoc.QuoteBlocks(missing.Nearest),
			)}
		}
		return pmdoc.Range{}, &ErrQuoteNotFound{Quote: quote, Nearest: missing.Nearest, Matches: missing.Matches}
	}
	var ambiguous *pmdoc.ErrTargetAmbiguous
	if errors.As(err, &ambiguous) {
		return pmdoc.Range{}, &ErrAnchorAmbiguous{Kind: "quote", Target: quote, Candidates: ambiguous.Candidates}
	}
	if errors.Is(err, pmdoc.ErrTargetSpansBlocks) {
		return pmdoc.Range{}, &ErrQuoteSpansBlocks{Quote: quote}
	}
	return pmdoc.Range{}, err
}

func insertTarget(tree *pmdoc.Node, field, anchor string, occurrence *int) (pmdoc.Range, bool, error) {
	switch anchor {
	case "start":
		return pmdoc.Range{}, false, nil
	case "end":
		position := pmdoc.Size(tree)
		return pmdoc.Range{From: position, To: position}, false, nil
	}
	if title, ok := strings.CutPrefix(anchor, "heading:"); ok {
		if title == "" {
			return pmdoc.Range{}, false, invalidOp("heading")
		}
		r, err := pmdoc.FindHeading(tree, title, occurrence)
		var ambiguous *pmdoc.ErrTargetAmbiguous
		if errors.As(err, &ambiguous) {
			return pmdoc.Range{}, false, &ErrAnchorAmbiguous{
				Kind: "heading anchor", Target: title, Candidates: ambiguous.Candidates,
			}
		}
		return r, false, err
	}
	if blockID, ok := strings.CutPrefix(anchor, "block:"); ok {
		if blockID == "" {
			return pmdoc.Range{}, false, invalidOp("block")
		}
		r, err := pmdoc.BlockRange(tree, blockID)
		return r, false, err
	}
	r, err := resolveEditQuote(tree, field, anchor, occurrence)
	if err != nil {
		return pmdoc.Range{}, true, err
	}
	return r, true, nil
}

// refuseUnreadableReplacement refuses a replace that makes the document-level block it lands in
// unreadable: one the parser read back before the replace and refuses after it. What markdown
// reads as a block depends on where the text lands, so the rendered block decides it rather than
// the replacement alone: `<div>x</div>` over a whole paragraph, at a list item's start or after a
// hard break opens an HTML block the Proof schema does not carry, while the same HTML inside a
// line, a table cell or a heading is inline HTML and is kept. A block that was already unreadable,
// or another block that is, is no reason to refuse this replace.
func refuseUnreadableReplacement(before, after *pmdoc.Node, match pmdoc.Range, with string) error {
	_, unreadable, err := replacementBroke(before, after, match, pmdoc.BlockReadError)
	if err != nil || unreadable == nil {
		return err
	}
	return &ErrInvalidOp{Field: "with", Reason: unreadableReason(with, unreadable)}
}

// refuseReshapedReplacement refuses a replace whose text the document reads back as blocks of
// another shape where it lands: a heading and a table cell are written on one line, so a line
// break inside a code span or inline HTML there ends the block, as a hard break would
// (hasHardBreak), and the document reads back a heading and a paragraph, or a row as two rows.
func refuseReshapedReplacement(before, after *pmdoc.Node, match pmdoc.Range, with string) error {
	_, reshaped, err := replacementBroke(before, after, match, pmdoc.BlockShapeError)
	if err != nil || reshaped == nil {
		return err
	}
	return &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf(
		"with %q is text the document reads back as another block where it lands (%v); write it inside a line of text",
		with, reshaped,
	)}
}

// unreadableReason says why a replace left its block unreadable and what to do instead, by cause.
func unreadableReason(with string, unreadable error) string {
	if errors.Is(unreadable, pmdoc.ErrBlockHTML) {
		return fmt.Sprintf(
			"with %q is HTML that opens a block where it lands, and the Proof schema carries no block HTML; keep the HTML inside a line, where it opens no block",
			with,
		)
	}
	return fmt.Sprintf("with %q leaves markdown the document cannot read back where it lands (%v); write it inside a line of text, or insert the block you mean as its own block", with, unreadable)
}

func blockID(block *pmdoc.Node) string {
	id, _ := block.Attrs[pmdoc.BlockIDAttr].(string)
	return id
}

// inlineReplacement parses replace's `with` as one textblock's inline content:
// a quote-anchored replace stays inside its textblock, so a leading list or
// heading marker is text, never a new block.
func inlineReplacement(markdown string, edges textEdges) (*pmdoc.Node, error) {
	inline, err := pmdoc.ParseInline(markdown)
	if err != nil {
		if errors.Is(err, pmdoc.ErrSchema) {
			return nil, &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf("replace is inline; %v (paragraphs: give each one its own replace, then add every extra paragraph in one insert anchored on the last paragraph you rewrote; an insert lands after the top-level block holding the quote, so beside a paragraph in a list it goes after the whole list; any block that is not a paragraph: replace keeps a block's kind, so insert it beside a paragraph you replace, and delete the old block only when no paragraph of the new text is left to take its place)", err)}
		}
		return nil, err
	}
	// An empty `with` deletes the matched span on purpose, and is the only `with` that does: one
	// the caller wrote and that renders to nothing — a line indented four spaces or a tab, which
	// markdown reads as a code block, or whitespace alone — would splice nothing over the match
	// and silently delete the text they meant to replace (LEGION-280).
	if len(inline) == 0 && markdown != "" {
		return nil, &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf(
			"with %q renders to no text (a line indented four spaces or a tab is a code block, and whitespace alone has no inline content), and replace is inline, so there would be nothing to put in the match's place; pass an empty with to delete the matched text, remove the leading indentation, or use insert plus delete to add a code block",
			markdown,
		)}
	}
	if marker, kind := blockMarkerAfterHardBreak(inline); marker != "" {
		return nil, &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf(
			"with continues after a hard line break with %q, a block marker (%s), and replace is inline: that line stays inside the matched block, where the marker is written as escaped literal text and never opens the %s it names — a backslash before it parses to the same text, so escaping it changes nothing; use insert, plus delete for the text it replaces, to add the block, or a plain newline, which renders as a space, to keep the text in this block",
			marker, kind, kind,
		)}
	}
	paragraph := &pmdoc.Node{Type: "paragraph", Children: inline}
	continueText(paragraph, markdown, edges)
	return pmdoc.StripAnchorMarks(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{paragraph}}), nil
}

// hardBreakOrderedMarker and hardBreakBlockquoteMarker are the two block markers
// pmdoc.LeadingBlockMarker cannot answer for a line after a hard break. An ordered item may
// interrupt a paragraph only when it is numbered a lone `1`, as the browser editor's parser reads
// it, so LeadingBlockMarker's any-digit-run pattern would refuse `2024. was a year`, which stays
// prose, and so do `01.`, `001)`, `02.` and `10.`, which the parser reads as text there too. A
// blockquote's `>` is no textblock's own marker, so pmdoc has no MarkerKind for it.
var (
	hardBreakOrderedMarker    = regexp.MustCompile(`^[ \t]*1[.)][ \t]`)
	hardBreakBlockquoteMarker = regexp.MustCompile(`^[ \t]*>`)
)

// hasHardBreak reports whether a parsed replacement carries a hard break.
func hasHardBreak(with *pmdoc.Node) bool {
	for _, paragraph := range with.Children {
		for _, node := range paragraph.Children {
			if node.Type == "hardbreak" {
				return true
			}
		}
	}
	return false
}

// blockMarkerAfterHardBreak reports the block marker a `with` opens a line with after a hard line
// break, and the kind of block that marker names. A hard break puts what follows it at a true
// line start, where a marker is as ambiguous as it is at position 0 — and replace is inline, so
// the marker can only be written as escaped text continuing the matched block, never as the block
// the caller wrote it for. A bare newline is a soft break, which renders as a space and reaches no
// line start, so goldmark's own hardbreak nodes are the whole population; a following text node
// carrying marks opens with its mark's delimiter rather than the marker, so it is prose either way.
func blockMarkerAfterHardBreak(inline []*pmdoc.Node) (marker, kind string) {
	for index, node := range inline {
		if node.Type != "hardbreak" || index+1 == len(inline) {
			continue
		}
		next := inline[index+1]
		if next.Type != "text" || len(next.Marks) != 0 {
			continue
		}
		switch written, width := pmdoc.LeadingBlockMarker(next.Text); written.Kind {
		case pmdoc.MarkerHeading, pmdoc.MarkerBullet:
			return next.Text[:width], string(written.Kind)
		}
		if match := hardBreakOrderedMarker.FindString(next.Text); match != "" {
			return match, string(pmdoc.MarkerOrdered)
		}
		if match := hardBreakBlockquoteMarker.FindString(next.Text); match != "" {
			return match, "blockquote"
		}
	}
	return "", ""
}

// inlineAware parses a suggestion's replacement as blocks written into the document, keeping the
// edge whitespace of a replacement that stays inline. opensDocument says whether the replacement
// lands where the document begins; its tables are padded on budget, the accept's.
func inlineAware(markdown string, edges textEdges, opensDocument bool, budget *pmdoc.TablePaddingBudget) (*pmdoc.Node, error) {
	tree, err := parseFragmentInput(markdown, opensDocument, budget)
	if err != nil {
		return nil, err
	}
	if isInlineDocument(tree) {
		continueText(tree.Children[0], markdown, edges)
	}
	return tree, nil
}

// refuseCodeThatReshapesItsBlock refuses a replacement into a code block that leaves the
// document-level block holding it reading back as blocks of another shape. A code block's text is
// literal, so only the lines around it could read it differently, and the renderer writes a typed
// block's fence longer than any line of colons in its code that the browser editor's parser, or
// this one, could read as that fence (pmdoc's typedFence). Code that only reads back with
// different whitespace keeps its shape and is not refused.
func refuseCodeThatReshapesItsBlock(before, after *pmdoc.Node, match pmdoc.Range, at pmdoc.TextblockAt, field, with string) error {
	_, reshaped, err := replacementBroke(before, after, match, pmdoc.BlockShapeError)
	if err != nil || reshaped == nil {
		return err
	}
	holder := "block"
	for _, ancestor := range at.Ancestors {
		if pmdoc.IsTypedBlock(ancestor.Type) {
			holder = ancestor.Type
			break
		}
	}
	return &ErrInvalidOp{Field: field, Reason: fmt.Sprintf(
		"%s %q changes how the %s holding this code block reads back (%v); move the code block out of the %s",
		field, with, holder, reshaped, holder,
	)}
}

// codeReplacement is what a replacement landing in a code block splices in: a code block's text
// is literal, whitespace, markdown syntax and references alike, so it is the replacement as sent.
func codeReplacement(text string) *pmdoc.Node {
	paragraph := &pmdoc.Node{Type: "paragraph"}
	if text != "" {
		paragraph.Children = []*pmdoc.Node{{Type: "text", Text: text}}
	}
	return &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{paragraph}}
}

// textEdges reports which of a replacement's edges meet the edges of the text it lands in, where
// no text is left to continue and whitespace kept there is stripped on the next read, or, after a
// footnote's marker, opens indented code.
type textEdges struct{ start, end bool }

func edgesOf(at pmdoc.TextblockAt, r pmdoc.Range) textEdges {
	return textEdges{start: r.From == at.Content.From, end: r.To == at.Content.To}
}

// continueText restores the spaces and tabs parsing strips from a replacement's edges, so the
// replacement continues the text around it. They go in unmarked text at the paragraph's own
// edges - ` **x**` is a space and then bold, not a bold ` x`, and ` `c` ` leaves the code span's
// text alone. A line break at an edge is no part of an inline replacement and is not restored,
// and whitespace the parser keeps, such as a no-break space, is in the text already. An edge that
// meets the edge of the text it lands in (textEdges) is left off.
func continueText(paragraph *pmdoc.Node, markdown string, edges textEdges) {
	if len(paragraph.Children) == 0 {
		return
	}
	if leading := edgeSpace(markdown[:len(markdown)-len(strings.TrimLeft(markdown, markdownSpace))]); leading != "" && !edges.start {
		if first := paragraph.Children[0]; first.Type == "text" && len(first.Marks) == 0 {
			first.Text = leading + first.Text
		} else {
			paragraph.Children = append([]*pmdoc.Node{{Type: "text", Text: leading}}, paragraph.Children...)
		}
	}
	if trailing := edgeSpace(markdown[len(strings.TrimRight(markdown, markdownSpace)):]); trailing != "" && !edges.end {
		if last := paragraph.Children[len(paragraph.Children)-1]; last.Type == "text" && len(last.Marks) == 0 {
			last.Text += trailing
		} else {
			paragraph.Children = append(paragraph.Children, &pmdoc.Node{Type: "text", Text: trailing})
		}
	}
}

// markdownSpace is the whitespace a paragraph's parse strips from its edges.
const markdownSpace = " \t\n"

// edgeSpace is an edge's whitespace without its line feeds.
func edgeSpace(run string) string {
	return strings.ReplaceAll(run, "\n", "")
}

func isInlineLeaf(node *pmdoc.Node) bool {
	switch node.Type {
	case "hardbreak", "image", "html", "footnote_reference":
		return true
	default:
		return false
	}
}

func isInlineDocument(tree *pmdoc.Node) bool {
	if len(tree.Children) != 1 || tree.Children[0].Type != "paragraph" {
		return false
	}
	for _, child := range tree.Children[0].Children {
		if child.Type != "text" && !isInlineLeaf(child) {
			return false
		}
	}
	return true
}
