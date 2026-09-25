package docs

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"

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
// document before the batch, and every operation that left the tree as it was.
type editBatch struct {
	tree      *pmdoc.Node
	before    string
	unchanged []int
}

// outcome is the batch's verdict, taken once the caller has stamped the block ids the write
// carries. It compares nodeToken, the same semantic identity the precondition machinery uses:
// canonical markdown renders no anchor mark, so a batch that orphans a human's comment anchor
// while leaving the words alone is a change, and gets its version.
func (b editBatch) outcome(applied int) (EditOutcome, error) {
	after, err := nodeToken(b.tree)
	if err != nil {
		return EditOutcome{}, err
	}
	return EditOutcome{Applied: applied, Changed: after != b.before, Unchanged: b.unchanged}, nil
}

// applyOperations applies each operation to its predecessor's tree so a
// following operation resolves the structure created by the preceding one.
func applyOperations(tree *pmdoc.Node, ops []model.EditOp) (editBatch, error) {
	return applyOperationsWithValidation(tree, ops, nil)
}

func applyOperationsWithValidation(tree *pmdoc.Node, ops []model.EditOp, validate operationValidator) (editBatch, error) {
	before, err := nodeToken(tree)
	if err != nil {
		return editBatch{}, err
	}
	removed := make(map[string]batchRemoval)
	var unchanged []int
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
		next, err := applyOperation(tree, op)
		if err != nil {
			return editBatch{}, stampOperation(index, err)
		}
		if next.Equal(tree) {
			unchanged = append(unchanged, index)
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
	return editBatch{tree: tree, before: before, unchanged: unchanged}, nil
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

func applyOperation(tree *pmdoc.Node, op model.EditOp) (*pmdoc.Node, error) {
	switch op.Op {
	case "replace":
		if op.Find == "" {
			return nil, invalidOp("find")
		}
		r, err := findEditQuote(tree, "find", op.Find, op.Occurrence)
		if err != nil {
			return nil, err
		}
		replacement, level, err := replacementMarkdown(tree, r, op.Find, op.With)
		if err != nil {
			return nil, err
		}
		with, err := inlineReplacement(replacement)
		if err != nil {
			return nil, err
		}
		next, err := pmdoc.Splice(tree, r, with)
		if err != nil || level == 0 {
			return next, err
		}
		return pmdoc.SetHeadingLevel(next, r.From, level)
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
		return pmdoc.Splice(tree, r, empty)
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
		with, err := parseInput(op.Markdown)
		if err != nil {
			return nil, invalidMarkdownOp("markdown", err)
		}
		if out, inserted, err := pmdoc.InsertTableRows(tree, target, op.Markdown, after); err != nil || inserted {
			return out, err
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
		return pmdoc.Splice(tree, pmdoc.Range{From: position, To: position}, with)
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
// marker twice (AGENTC-193 read back `## ##`, `7. 7\.`, `4. 4\.` and `-    - `). A heading
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

// inlineReplacement parses replace's `with` as one textblock's inline content:
// a quote-anchored replace stays inside its textblock, so a leading list or
// heading marker is text, never a new block.
func inlineReplacement(markdown string) (*pmdoc.Node, error) {
	inline, err := pmdoc.ParseInline(markdown)
	if err != nil {
		if errors.Is(err, pmdoc.ErrSchema) {
			return nil, &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf("replace is inline; %v (delete the block and insert new blocks instead)", err)}
		}
		return nil, err
	}
	// An empty `with` deletes the matched span on purpose, but a `with` the caller wrote that
	// parses to nothing does not: markdown reads a line indented four spaces or a tab as a code
	// block, which has no inline content, and splicing that over the match would silently delete
	// the text they meant to replace (LEGION-280).
	if len(inline) == 0 && strings.TrimSpace(markdown) != "" {
		return nil, &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf(
			"with %q produced no text: markdown reads a line indented four spaces or a tab as a code block, and replace is inline, so there would be nothing to put in the match's place; remove the leading indentation, or use insert plus delete to add a code block",
			markdown,
		)}
	}
	paragraph := &pmdoc.Node{Type: "paragraph", Children: inline}
	continueText(paragraph, markdown)
	return pmdoc.StripAnchorMarks(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{paragraph}}), nil
}

// inlineAware parses a suggestion's replacement as blocks, keeping the edge
// whitespace of a replacement that stays inline.
func inlineAware(markdown string) (*pmdoc.Node, error) {
	tree, err := parseInput(markdown)
	if err != nil {
		return nil, err
	}
	if isInlineDocument(tree) {
		continueText(tree.Children[0], markdown)
	}
	return tree, nil
}

// continueText restores the leading and trailing whitespace markdown parsing
// drops so a replacement can continue the text around it.
func continueText(paragraph *pmdoc.Node, markdown string) {
	leading := markdown[:len(markdown)-len(strings.TrimLeftFunc(markdown, unicode.IsSpace))]
	trailing := markdown[len(strings.TrimRightFunc(markdown, unicode.IsSpace)):]
	if leading == "" && trailing == "" {
		return
	}
	var first, last *pmdoc.Node
	for _, child := range paragraph.Children {
		if child.Type == "text" {
			if first == nil {
				first = child
			}
			last = child
		}
	}
	if first == nil {
		return
	}
	first.Text = leading + first.Text
	last.Text += trailing
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
