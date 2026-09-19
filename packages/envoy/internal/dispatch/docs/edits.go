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

// ErrInvalidOp identifies the malformed user-facing operation field.
type ErrInvalidOp struct {
	Field  string
	Reason string
}

func (e *ErrInvalidOp) Error() string {
	return fmt.Sprintf("invalid document operation field %q: %s", e.Field, e.Reason)
}

func invalidOp(field string) error {
	return &ErrInvalidOp{Field: field}
}

// ErrQuoteNotFound explains how document edit quotes are matched and names the closest blocks.
type ErrQuoteNotFound struct {
	Nearest []string
}

func (e *ErrQuoteNotFound) Error() string {
	return fmt.Sprintf(`quote not found; quotes match the block text as rendered (inline markdown is tolerated; use "heading:<title>", "block:<id>", "start", or "end" as insert and move anchors); nearest blocks: %s`, pmdoc.QuoteBlocks(e.Nearest))
}

func (e *ErrQuoteNotFound) Unwrap() error { return pmdoc.ErrTargetNotFound }

// ErrInvalidPrecondition identifies a malformed optimistic-concurrency guard.
type ErrInvalidPrecondition struct {
	Reason string
}

func (e *ErrInvalidPrecondition) Error() string {
	return "invalid document edit precondition: " + e.Reason
}

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
			return pmdoc.BlockDescendantIDs(tree, op.Block)
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
	range_, err := findEditQuote(tree, quote, occurrence)
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

// applyOperations applies each operation to its predecessor's tree so a
// following operation resolves the structure created by the preceding one.
func applyOperations(tree *pmdoc.Node, ops []model.EditOp) (*pmdoc.Node, error) {
	return applyOperationsWithValidation(tree, ops, nil)
}

func applyOperationsWithValidation(tree *pmdoc.Node, ops []model.EditOp, validate operationValidator) (*pmdoc.Node, error) {
	removed := make(map[string]batchRemoval)
	for index, op := range ops {
		if removal, alreadyRemoved := removed[op.Block]; op.Block != "" && alreadyRemoved {
			return nil, fmt.Errorf("operation %d: %w", index, removedBlockError(op.Block, removal))
		}
		if validate != nil {
			if err := validate(tree, op); err != nil {
				return nil, fmt.Errorf("operation %d: %w", index, err)
			}
		}
		var removedIDs []string
		if op.Op == "delete" && op.Block != "" && op.Find == "" {
			if _, alreadyRemoved := removed[op.Block]; !alreadyRemoved {
				var err error
				removedIDs, err = pmdoc.BlockDescendantIDs(tree, op.Block)
				if err != nil {
					return nil, fmt.Errorf("operation %d: %w", index, err)
				}
			}
		}
		next, err := applyOperation(tree, op)
		if err != nil {
			return nil, fmt.Errorf("operation %d: %w", index, err)
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
	return tree, nil
}

func (s *Service) applyOperations(ctx context.Context, artifactID string, tree *pmdoc.Node, ops []model.EditOp) (*pmdoc.Node, error) {
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
	rows, err := s.store.Pool.Query(ctx, `
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
		r, err := findEditQuote(tree, op.Find, op.Occurrence)
		if err != nil {
			return nil, err
		}
		with, err := inlineReplacement(op.With)
		if err != nil {
			return nil, err
		}
		return pmdoc.Splice(tree, r, with)
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
		r, err := findEditQuote(tree, op.Find, op.Occurrence)
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
		target, plainText, err := insertTarget(tree, anchor, op.Occurrence)
		if err != nil {
			return nil, err
		}
		if plainText && pmdoc.TargetSpansBlocks(tree, target) {
			return nil, pmdoc.ErrTargetSpansBlocks
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
		target, _, err := insertTarget(tree, anchor, op.Occurrence)
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
			field := "before"
			if op.After != "" {
				field = "after"
			}
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

func findEditQuote(tree *pmdoc.Node, quote string, occurrence *int) (pmdoc.Range, error) {
	r, err := resolveEditQuote(tree, quote, occurrence)
	if err != nil {
		return pmdoc.Range{}, err
	}
	if pmdoc.TargetSpansBlocks(tree, r) {
		return pmdoc.Range{}, pmdoc.ErrTargetSpansBlocks
	}
	return r, nil
}

func resolveEditQuote(tree *pmdoc.Node, quote string, occurrence *int) (pmdoc.Range, error) {
	r, err := pmdoc.FindQuote(tree, quote, occurrence, nil)
	if err == nil {
		return r, nil
	}
	var missing *pmdoc.ErrQuoteNotFound
	if errors.As(err, &missing) {
		return pmdoc.Range{}, &ErrQuoteNotFound{Nearest: missing.Nearest}
	}
	return pmdoc.Range{}, err
}

func insertTarget(tree *pmdoc.Node, anchor string, occurrence *int) (pmdoc.Range, bool, error) {
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
		return r, false, err
	}
	if blockID, ok := strings.CutPrefix(anchor, "block:"); ok {
		if blockID == "" {
			return pmdoc.Range{}, false, invalidOp("block")
		}
		r, err := pmdoc.BlockRange(tree, blockID)
		return r, false, err
	}
	r, err := resolveEditQuote(tree, anchor, occurrence)
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
