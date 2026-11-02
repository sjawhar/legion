package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// results.go records and reads the results of an issue's checks. A check is a `check` block of
// a Verification document; a result says one run of it passed or failed, at one commit. The
// record route takes a result for a check of the recording issue or of any issue above it
// (refs.IssueAncestors), since a child proves its parent's checks, and appends `result.recorded`
// on the check's own issue. The read route lists the checks of one issue's Verification
// document with every result recorded against each, from any issue, newest first, and says
// whether each still speaks for the work as it stands (currency, below).

// checkResultRole bounds the role a result names: the word the pull-request body line prints.
var checkResultRole = regexp.MustCompile(`^[a-z-]{1,32}$`)

// resultCommitSHA is the commit a result ran at, as the column's own check spells it.
var resultCommitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// checkWhenBeforeMerge is a check's `when` when its block names none: the schema's default
// (pmdoc/schema/blocks.json).
const checkWhenBeforeMerge = "before_merge"

// checkResultSummaryRunes is how much of a check's words the pull-request body line carries.
const checkResultSummaryRunes = 80

// checkResultColumns is the column list every check_results read scans, in scanCheckResult's
// order.
const checkResultColumns = `id::text, issue_key, check_issue_key, check_artifact_id::text, check_block_id,
	check_version, check_sha256, role, surface, run, observed, commit_sha, negative_control,
	failed, skipped, excluded, verdict, screenshots, session, actor, created_at, event_seq`

// resolvedCheck is one `check` block a result names: where it lives, the version its words last
// changed in and their digest as document_blocks holds them, and the words themselves.
type resolvedCheck struct {
	IssueKey   string
	ArtifactID string
	BlockID    string
	Version    int
	SHA256     string
	When       string
	Text       string
}

// checkResultInput is the body POST /api/v1/issues/{key}/results takes.
type checkResultInput struct {
	Actor           *model.Actor `json:"actor"`
	Check           string       `json:"check"`
	Role            string       `json:"role"`
	Surface         string       `json:"surface"`
	Run             string       `json:"run"`
	Observed        string       `json:"observed"`
	Commit          string       `json:"commit"`
	NegativeControl string       `json:"negative_control"`
	Verdict         string       `json:"verdict"`
	Failed          int          `json:"failed"`
	Skipped         int          `json:"skipped"`
	Excluded        int          `json:"excluded"`
	Screenshots     []string     `json:"screenshots"`
}

// POST /api/v1/issues/{key}/results  (any authenticated actor)
//
// Records one result against one check. {key} is the recording issue, which need not be the
// check's: a child records a result for a check of its parent's Verification document, and the
// event goes on the parent. The answer carries the stored row and the pull-request body line it
// implies, so every writer writes one grammar.
func (s *server) recordCheckResult(w http.ResponseWriter, r *http.Request) {
	var input checkResultInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	if err := input.refusal(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	issueKey := r.PathValue("key")

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())

	// The recording issue's row is locked whether or not it is open: a result for an after-merge
	// check is taken on a closed issue, and the lock still stops it closing or reopening under
	// the write.
	recordingOpen, err := lockIssueForResult(r.Context(), tx, issueKey)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	check, err := s.resolveCheck(r.Context(), tx, issueKey, input.Check)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	checkOpen := recordingOpen
	if check.IssueKey != issueKey {
		if checkOpen, err = lockIssueForResult(r.Context(), tx, check.IssueKey); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	// The one write a closed issue takes. A check that runs after the merge, or on the default
	// branch, can only be proved once the issue has closed, so refusing it would make the
	// production check unrecordable; every other write on a closed issue — a comment, a
	// document version, a result for a pre-merge check — stays refused as it was.
	if (!recordingOpen || !checkOpen) && check.When != "after_merge" && check.When != "default_branch" {
		writeError(w, "ISSUE_CLOSED", http.StatusConflict,
			fmt.Sprintf("issue is closed: only a result for a check whose when is after_merge or default_branch is recorded there, and %s is %s", check.BlockID, check.When))
		return
	}
	if err := s.refuseResultScreenshots(r.Context(), tx, issueKey, check.IssueKey, input.Screenshots); err != nil {
		s.writeHandlerError(w, err)
		return
	}

	result := model.CheckResult{
		ID: uuid.NewString(), IssueKey: issueKey, CheckIssueKey: check.IssueKey,
		CheckArtifactID: check.ArtifactID, CheckBlockID: check.BlockID,
		CheckVersion: check.Version, CheckSHA256: check.SHA256,
		Role: input.Role, Surface: input.Surface, Run: input.Run, Observed: input.Observed,
		CommitSHA: input.Commit, NegativeControl: input.NegativeControl,
		Failed: input.Failed, Skipped: input.Skipped, Excluded: input.Excluded,
		Verdict: input.Verdict, Screenshots: nonNilStrings(input.Screenshots),
		Session: sessionOf(actor), Actor: actor,
	}
	event, err := s.appendEvent(r.Context(), tx, issueOwner(check.IssueKey).event("result.recorded", actor, model.CheckResultEventPayload{
		ResultID: result.ID, Issue: issueKey, CheckIssue: check.IssueKey,
		CheckArtifactID: check.ArtifactID, CheckBlockID: check.BlockID, CheckVersion: check.Version,
		When: check.When, Verdict: result.Verdict, Role: result.Role, CommitSHA: result.CommitSHA,
	}))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	seq := int64(event.Seq)
	result.EventSeq = &seq
	if err := insertCheckResult(r.Context(), tx, result); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	s.readThroughPullRequests(r.Context(), check.IssueKey, result.CommitSHA)
	WriteJSON(w, http.StatusCreated, map[string]any{
		"result": result,
		"line":   checkResultLine(check, result),
	})
}

// GET /api/v1/issues/{key}/results  (any authenticated caller)
//
// The checks of the issue's Verification document in document order, each with every result
// recorded against it from any issue, newest first, and each result's currency. An issue with
// no Verification document has no checks and no error.
func (s *server) listCheckResults(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	issueKey := r.PathValue("key")
	if err := s.requireIssueExists(r.Context(), issueKey); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	artifactID, err := s.verificationDocumentOf(r.Context(), s.deps.Store.Pool, issueKey)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	body := map[string]any{"issue": issueKey, "verification_artifact_id": nil, "checks": []model.CheckBlock{}}
	if artifactID == "" {
		WriteJSON(w, http.StatusOK, body)
		return
	}
	body["verification_artifact_id"] = artifactID
	checks, err := s.checkBlocksOf(r.Context(), s.deps.Store.Pool, issueKey, artifactID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	currency := newCurrencyReader(s)
	for index := range checks {
		results, err := loadCheckResults(r.Context(), s.deps.Store.Pool, artifactID, checks[index].BlockID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		for result := range results {
			results[result].Currency = currency.of(r.Context(), checks[index], results[result])
		}
		checks[index].Results = results
	}
	body["checks"] = checks
	WriteJSON(w, http.StatusOK, body)
}

// refusal is every refusal a result's own fields earn, before anything is read.
func (input checkResultInput) refusal() error {
	if strings.TrimSpace(input.Check) == "" {
		return errorf(http.StatusBadRequest, "RESULT_INPUT", "check names the check block the result proves")
	}
	if !checkResultRole.MatchString(input.Role) {
		return errorf(http.StatusBadRequest, "RESULT_INPUT",
			"role is required and is at most 32 characters of a-z and -, the word the pull-request line prints")
	}
	if input.Verdict != "pass" && input.Verdict != "fail" {
		return errorf(http.StatusBadRequest, "RESULT_INPUT", "verdict is pass or fail")
	}
	if !resultCommitSHA.MatchString(input.Commit) {
		return errorf(http.StatusBadRequest, "RESULT_INPUT", "commit is the 40-character lowercase hex commit the check ran at")
	}
	for name, count := range map[string]int{"failed": input.Failed, "skipped": input.Skipped, "excluded": input.Excluded} {
		if count < 0 {
			return errorf(http.StatusBadRequest, "RESULT_INPUT", "%s counts cases and cannot be negative", name)
		}
	}
	if input.Verdict == "pass" && (input.Failed > 0 || input.Skipped > 0 || input.Excluded > 0) {
		return errorf(http.StatusBadRequest, "RESULT_CONTRADICTS",
			"a pass counts no failed, skipped or excluded case; this one counts %d failed, %d skipped and %d excluded",
			input.Failed, input.Skipped, input.Excluded)
	}
	return nil
}

// lockIssueForResult takes the issue's row lock and answers whether it is open, without the
// refusal requireOpenIssue makes: the results route decides for itself, since a closed issue
// takes a result for an after-merge check.
func lockIssueForResult(ctx context.Context, tx pgx.Tx, key string) (bool, error) {
	var open bool
	err := tx.QueryRow(ctx, `select closed_at is null from issues where key = $1 for no key update`, key).Scan(&open)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, errorf(http.StatusNotFound, "NOT_FOUND", "issue not found")
	}
	if err != nil {
		return false, fmt.Errorf("lock issue %s: %w", key, err)
	}
	return open, nil
}

// requireIssueExists is the read route's 404 for an issue nobody has.
func (s *server) requireIssueExists(ctx context.Context, key string) error {
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return errorf(http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "database unavailable")
	}
	var exists bool
	if err := s.deps.Store.Pool.QueryRow(ctx, `select true from issues where key = $1`, key).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errorf(http.StatusNotFound, "NOT_FOUND", "issue not found")
		}
		return fmt.Errorf("read issue %s: %w", key, err)
	}
	return nil
}

// verificationDocumentOf is the issue's Verification document, or "" when it has none.
func (s *server) verificationDocumentOf(ctx context.Context, q queryer, issueKey string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `select id::text from artifacts where issue_key = $1 and role = 'verification'`, issueKey).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the Verification document of %s: %w", issueKey, err)
	}
	return id, nil
}

// resolveCheck finds the check a result names among the Verification documents of the recording
// issue and of every issue above it. The value is a block id, or the canonical block reference
// (`dispatch://KEY/artifact/verification-md#b-<id>`) that names one document when a block id is
// not unique in that set: an id two documents share is 409 CHECK_AMBIGUOUS, and an id no
// Verification document of the set holds — or one that is not a `check` block — is 400
// UNKNOWN_CHECK.
func (s *server) resolveCheck(ctx context.Context, tx pgx.Tx, issueKey, value string) (resolvedCheck, error) {
	ancestors, err := refs.IssueAncestors(ctx, tx, issueKey)
	if err != nil {
		return resolvedCheck{}, err
	}
	scope := append([]string{issueKey}, ancestors...)

	blockID, artifactFilter := value, ""
	if ref, ok := text.CanonicalBlockRef(value); ok {
		// A block of a document carries its document's fields and no Item; a block of a comment
		// or a message names that item in Item and is no check.
		if ref.Item != "" || ref.IssueKey == "" || ref.ID == "" || ref.Block == "" {
			return resolvedCheck{}, unknownCheck(value)
		}
		blockID = ref.Block
		if artifactFilter, err = s.verificationDocumentBySlug(ctx, tx, ref.IssueKey, ref.ID); err != nil {
			return resolvedCheck{}, err
		}
		if artifactFilter == "" {
			return resolvedCheck{}, unknownCheck(value)
		}
	}

	rows, err := tx.Query(ctx, `
		select a.issue_key, d.artifact_id::text, d.changed_version, d.text_sha256
		from document_blocks d
		join artifacts a on a.id = d.artifact_id
		where a.role = 'verification' and a.issue_key = any($1) and d.type = 'check' and d.block_id = $2
		  and ($3 = '' or d.artifact_id = $3::uuid)
		order by a.issue_key
	`, scope, blockID, artifactFilter)
	if err != nil {
		return resolvedCheck{}, fmt.Errorf("resolve check %q of %s: %w", value, issueKey, err)
	}
	defer rows.Close()
	var found []resolvedCheck
	for rows.Next() {
		check := resolvedCheck{BlockID: blockID}
		if err := rows.Scan(&check.IssueKey, &check.ArtifactID, &check.Version, &check.SHA256); err != nil {
			return resolvedCheck{}, fmt.Errorf("resolve check %q of %s: %w", value, issueKey, err)
		}
		found = append(found, check)
	}
	if err := rows.Err(); err != nil {
		return resolvedCheck{}, fmt.Errorf("resolve check %q of %s: %w", value, issueKey, err)
	}
	if len(found) == 0 {
		return resolvedCheck{}, unknownCheck(value)
	}
	if len(found) > 1 {
		issues := make([]string, len(found))
		for index, check := range found {
			issues[index] = check.IssueKey
		}
		return resolvedCheck{}, errorf(http.StatusConflict, "CHECK_AMBIGUOUS",
			"check %s is a check of the Verification document of %s: name one with its reference, dispatch://<KEY>/artifact/verification-md#b-%s",
			blockID, strings.Join(issues, " and "), blockID)
	}
	check := found[0]
	if check.When, check.Text, err = s.checkBlockWords(ctx, check.ArtifactID, check.BlockID); err != nil {
		return resolvedCheck{}, err
	}
	return check, nil
}

func unknownCheck(value string) error {
	return errorf(http.StatusBadRequest, "UNKNOWN_CHECK",
		"%s is no check of this issue's Verification document or of one above it", value)
}

// verificationDocumentBySlug is the Verification document a canonical block reference names, or
// "" when that issue has no such document under that slug.
func (s *server) verificationDocumentBySlug(ctx context.Context, q queryer, issueKey, slug string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `
		select id::text from artifacts where issue_key = $1 and slug = $2 and role = 'verification'
	`, issueKey, slug).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the Verification document %s/%s: %w", issueKey, slug, err)
	}
	return id, nil
}

// checkBlockWords reads one check block's `when` and its words out of the live document. The
// index holds a block's type and digest but not its attributes, so the document itself is the
// one place `when` is written.
func (s *server) checkBlockWords(ctx context.Context, artifactID, blockID string) (string, string, error) {
	blocks, err := s.checkBlocksIn(ctx, artifactID)
	if err != nil {
		return "", "", err
	}
	for _, block := range blocks {
		if block.BlockID == blockID {
			return block.When, block.Text, nil
		}
	}
	// The index holds the block, so the live document has it too unless a write landed between
	// the two reads; the schema's default is what such a block's `when` would be.
	return checkWhenBeforeMerge, "", nil
}

// checkBlocksIn is every `check` block of one document, in document order, with its attributes
// and its words. The document's markdown is read live and parsed; nothing is written.
func (s *server) checkBlocksIn(ctx context.Context, artifactID string) ([]model.CheckBlock, error) {
	markdown, err := s.deps.Docs.Text(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		return nil, fmt.Errorf("read the checks of document %s: %w", artifactID, err)
	}
	var checks []model.CheckBlock
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "check" {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if id == "" {
			return true
		}
		when, _ := node.Attrs["when"].(string)
		if when == "" {
			when = checkWhenBeforeMerge
		}
		surface, _ := node.Attrs["surface"].(string)
		run, _ := node.Attrs["run"].(string)
		negativeControl, _ := node.Attrs["negative_control"].(string)
		checks = append(checks, model.CheckBlock{
			BlockID: id, ArtifactID: artifactID, Text: blockWords(node), When: when,
			Surface: surface, Run: run, NegativeControl: negativeControl, Results: []model.CheckResult{},
		})
		return true
	})
	return checks, nil
}

// checkBlocksOf is checkBlocksIn with each block's reference, issue and indexed version and
// digest filled in from the block index.
func (s *server) checkBlocksOf(ctx context.Context, q queryer, issueKey, artifactID string) ([]model.CheckBlock, error) {
	checks, err := s.checkBlocksIn(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	if len(checks) == 0 {
		return []model.CheckBlock{}, nil
	}
	var slug string
	indexed := make(map[string]model.CheckBlock, len(checks))
	rows, err := q.Query(ctx, `
		select a.slug, d.block_id, d.changed_version, d.text_sha256
		from document_blocks d join artifacts a on a.id = d.artifact_id
		where d.artifact_id = $1 and d.type = 'check'
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read the indexed checks of %s: %w", artifactID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var block model.CheckBlock
		if err := rows.Scan(&slug, &block.BlockID, &block.Version, &block.SHA256); err != nil {
			return nil, fmt.Errorf("read the indexed checks of %s: %w", artifactID, err)
		}
		indexed[block.BlockID] = block
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the indexed checks of %s: %w", artifactID, err)
	}
	for index := range checks {
		checks[index].IssueKey = issueKey
		if slug != "" {
			checks[index].Ref = fmt.Sprintf("dispatch://%s/artifact/%s#b-%s", issueKey, slug, text.EncodeBlockID(checks[index].BlockID))
		}
		if block, ok := indexed[checks[index].BlockID]; ok {
			checks[index].Version, checks[index].SHA256 = block.Version, block.SHA256
		}
	}
	return checks, nil
}

// blockWords is a block's text as a reader sees it: every text node under it, joined by single
// blanks, with the text of a link and every dispatch:// reference dropped, so the pull-request
// body line the result implies carries no link and no Dispatch host.
func blockWords(node *pmdoc.Node) string {
	var words []string
	pmdoc.Walk(node, func(child *pmdoc.Node) bool {
		if child.Type != "text" || child.Text == "" {
			return true
		}
		for _, mark := range child.Marks {
			if mark.Type == "link" {
				return true
			}
		}
		for _, word := range strings.Fields(child.Text) {
			if strings.Contains(word, "dispatch://") || strings.Contains(word, "://") {
				continue
			}
			words = append(words, word)
		}
		return true
	})
	return strings.Join(words, " ")
}

// checkResultLine is the pull-request body line one recorded result implies. Dispatch writes it
// rather than each caller, so every writer writes one grammar; it carries no link to Dispatch
// and no Dispatch host.
func checkResultLine(check resolvedCheck, result model.CheckResult) string {
	line := fmt.Sprintf("- Check %s (%s): %s at %s, recorded by %s.",
		check.BlockID, check.When, result.Verdict, result.CommitSHA[:12], result.Role)
	if summary := text.HeadRunes(check.Text, checkResultSummaryRunes); summary != "" {
		line += " " + summary
	}
	return line
}

// sessionOf is the session a result was recorded by, empty for a person's.
func sessionOf(actor model.Actor) string {
	if actor.Kind == "session" {
		return actor.ID
	}
	return ""
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// refuseResultScreenshots holds a result's screenshots to image documents of the recording
// issue or of the check's issue, named by their dispatch:// reference.
func (s *server) refuseResultScreenshots(ctx context.Context, q queryer, issueKey, checkIssueKey string, screenshots []string) error {
	for _, screenshot := range screenshots {
		ref, ok := text.ParseRef(screenshot, "")
		if !ok || ref.Kind != "artifact" || ref.IssueKey == "" ||
			(ref.IssueKey != issueKey && ref.IssueKey != checkIssueKey) {
			return errorf(http.StatusBadRequest, "RESULT_INPUT",
				"screenshot %q names no image document of %s or %s", screenshot, issueKey, checkIssueKey)
		}
		var found bool
		err := q.QueryRow(ctx, `
			select true from artifacts where issue_key = $1 and slug = $2 and kind = 'image'
		`, ref.IssueKey, ref.ID).Scan(&found)
		if errors.Is(err, pgx.ErrNoRows) {
			return errorf(http.StatusBadRequest, "RESULT_INPUT",
				"screenshot %q names no image document of %s or %s", screenshot, issueKey, checkIssueKey)
		}
		if err != nil {
			return fmt.Errorf("read screenshot %q: %w", screenshot, err)
		}
	}
	return nil
}

func insertCheckResult(ctx context.Context, tx pgx.Tx, result model.CheckResult) error {
	actorJSON, err := encodeJSON(result.Actor)
	if err != nil {
		return err
	}
	return tx.QueryRow(ctx, `
		insert into check_results (
			id, issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
			check_sha256, role, surface, run, observed, commit_sha, negative_control,
			failed, skipped, excluded, verdict, screenshots, session, actor, event_seq
		)
		values ($1::uuid, $2, $3, $4::uuid, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
		returning created_at
	`,
		result.ID, result.IssueKey, result.CheckIssueKey, result.CheckArtifactID, result.CheckBlockID,
		result.CheckVersion, result.CheckSHA256, result.Role, result.Surface, result.Run, result.Observed,
		result.CommitSHA, result.NegativeControl, result.Failed, result.Skipped, result.Excluded,
		result.Verdict, result.Screenshots, result.Session, actorJSON, result.EventSeq,
	).Scan(&result.CreatedAt)
}

// loadCheckResults is every result recorded against one check, from any issue, newest first.
func loadCheckResults(ctx context.Context, q queryer, artifactID, blockID string) ([]model.CheckResult, error) {
	rows, err := q.Query(ctx, `
		select `+checkResultColumns+`
		from check_results
		where check_artifact_id = $1::uuid and check_block_id = $2
		order by created_at desc, id desc
	`, artifactID, blockID)
	if err != nil {
		return nil, fmt.Errorf("read the results of check %s: %w", blockID, err)
	}
	defer rows.Close()
	results := []model.CheckResult{}
	for rows.Next() {
		result, err := scanCheckResult(rows)
		if err != nil {
			return nil, fmt.Errorf("read the results of check %s: %w", blockID, err)
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the results of check %s: %w", blockID, err)
	}
	return results, nil
}

func scanCheckResult(row pgx.Row) (model.CheckResult, error) {
	var result model.CheckResult
	var actorJSON []byte
	if err := row.Scan(
		&result.ID, &result.IssueKey, &result.CheckIssueKey, &result.CheckArtifactID, &result.CheckBlockID,
		&result.CheckVersion, &result.CheckSHA256, &result.Role, &result.Surface, &result.Run,
		&result.Observed, &result.CommitSHA, &result.NegativeControl, &result.Failed, &result.Skipped,
		&result.Excluded, &result.Verdict, &result.Screenshots, &result.Session, &actorJSON,
		&result.CreatedAt, &result.EventSeq,
	); err != nil {
		return model.CheckResult{}, err
	}
	if err := json.Unmarshal(actorJSON, &result.Actor); err != nil {
		return model.CheckResult{}, fmt.Errorf("decode a result's actor: %w", err)
	}
	return result, nil
}

// readThroughPullRequests teaches Dispatch the pull requests GitHub names for a result's commit
// when that commit is the head of none it already knows for the check's issue, so a result
// recorded at the head of a pull request no event has reached Dispatch for is current at once.
// It never fails the record: the row is written, and a currency Dispatch could not establish
// reads as it would have without the call.
func (s *server) readThroughPullRequests(ctx context.Context, checkIssueKey, sha string) {
	if s.deps.GitHub == nil || s.deps.Store == nil || s.deps.Store.Pool == nil {
		return
	}
	known, err := delivery.HeadAttributedTo(ctx, s.deps.Store.Pool, checkIssueKey, sha)
	if err != nil {
		slog.Warn("dispatch: read the pull request heads of a recorded result", "issue", checkIssueKey, "error", err)
		return
	}
	if known {
		return
	}
	repo, err := s.resolvedIssueRepo(ctx, checkIssueKey)
	if err != nil || repo == nil {
		return
	}
	if _, err := delivery.ReadThroughCommitPulls(ctx, s.deps.Store.Pool, s.deps.GitHub, *repo, sha, time.Now()); err != nil {
		slog.Info("dispatch: GitHub named no pull request for a recorded result's commit",
			"repo", *repo, "commit", sha, "error", err)
	}
}

// resolvedIssueRepo is the repository an issue resolves to: its own, the nearest one above it,
// or its project's default (issueRepoLateral); nil when none resolves.
func (s *server) resolvedIssueRepo(ctx context.Context, issueKey string) (*string, error) {
	var scan repoScan
	if err := s.deps.Store.Pool.QueryRow(ctx, `
		select `+issueRepoColumns+` from issues i`+issueRepoLateral+` where i.key = $1
	`, issueKey).Scan(scan.targets()...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("resolve the repository of %s: %w", issueKey, err)
	}
	_, resolved, err := scan.resolve(issueKey)
	return resolved, err
}

// currencyReader answers whether each result still speaks for the work as it stands, over one
// read of one issue's results. A `before_merge` result is current while its commit is the head
// of an open pull request attributed to the check's issue, or the final head of a merged one,
// and the check's words have not changed since: a push that moves the head makes it older with
// no write of Dispatch's, and rewording the check makes its own results older and no other's.
// An `after_merge` or `default_branch` result is current while its commit is on the default
// branch of the check's issue's resolved repository and the words match. Where the issue
// resolves to no repository, or the App cannot read it, the answer is unknown: the commit is
// shown with neither mark. The repository, its default branch and GitHub's answer about each
// commit are each read once per read of the results; a yes about a commit is kept for good and
// a no re-read after five minutes (default_branch_commits).
type currencyReader struct {
	server   *server
	repos    map[string]*string
	branches map[string]string
}

func newCurrencyReader(s *server) *currencyReader {
	return &currencyReader{server: s, repos: map[string]*string{}, branches: map[string]string{}}
}

func (c *currencyReader) of(ctx context.Context, check model.CheckBlock, result model.CheckResult) string {
	if check.When == "before_merge" {
		known, err := delivery.HeadAttributedTo(ctx, c.server.deps.Store.Pool, result.CheckIssueKey, result.CommitSHA)
		if err != nil {
			slog.Warn("dispatch: read a result's pull request head", "issue", result.CheckIssueKey, "error", err)
			return model.CurrencyOlder
		}
		if known && check.SHA256 == result.CheckSHA256 {
			return model.CurrencyCurrent
		}
		return model.CurrencyOlder
	}
	branch, repo := c.defaultBranch(ctx, result.CheckIssueKey)
	if branch == "" {
		return model.CurrencyUnknown
	}
	onDefault, known := c.onDefaultBranch(ctx, repo, branch, result.CommitSHA)
	if !known {
		return model.CurrencyUnknown
	}
	if onDefault && check.SHA256 == result.CheckSHA256 {
		return model.CurrencyCurrent
	}
	return model.CurrencyOlder
}

// defaultBranch is the default branch of the check's issue's resolved repository, and that
// repository; an empty branch means Dispatch cannot say, which is the unknown answer.
func (c *currencyReader) defaultBranch(ctx context.Context, issueKey string) (string, string) {
	repo, cached := c.repos[issueKey]
	if !cached {
		resolved, err := c.server.resolvedIssueRepo(ctx, issueKey)
		if err != nil {
			slog.Warn("dispatch: resolve the repository of a check's issue", "issue", issueKey, "error", err)
		}
		repo = resolved
		c.repos[issueKey] = repo
	}
	if repo == nil {
		return "", ""
	}
	branch, read := c.branches[*repo]
	if read {
		return branch, *repo
	}
	if c.server.deps.GitHub == nil {
		c.branches[*repo] = ""
		return "", *repo
	}
	branch, err := delivery.RepositoryDefaultBranch(ctx, c.server.deps.GitHub, *repo)
	if err != nil {
		if !unreadableRepository(err) {
			slog.Warn("dispatch: read the default branch of a check's repository", "repo", *repo, "error", err)
		}
		branch = ""
	}
	c.branches[*repo] = branch
	return branch, *repo
}

// onDefaultBranch is GitHub's answer about one commit, from default_branch_commits when that
// answer still stands, and read through the App otherwise. The second value is false when
// Dispatch could not get an answer at all.
func (c *currencyReader) onDefaultBranch(ctx context.Context, repo, branch, sha string) (bool, bool) {
	pool := c.server.deps.Store.Pool
	stored, found, err := delivery.ReadDefaultBranchCommit(ctx, pool, repo, sha)
	if err != nil {
		slog.Warn("dispatch: read a commit's default-branch answer", "repo", repo, "commit", sha, "error", err)
	}
	now := time.Now()
	if found && (stored.OnDefault || now.Sub(stored.CheckedAt) < delivery.DefaultBranchCommitStaleAfter) {
		return stored.OnDefault, true
	}
	onDefault, err := delivery.CommitOnDefaultBranch(ctx, c.server.deps.GitHub, repo, branch, sha)
	if err != nil {
		if !unreadableRepository(err) {
			slog.Warn("dispatch: ask whether a commit is on the default branch", "repo", repo, "commit", sha, "error", err)
		}
		return false, false
	}
	if err := delivery.WriteDefaultBranchCommit(ctx, pool, repo, sha, onDefault, now); err != nil {
		slog.Warn("dispatch: record a commit's default-branch answer", "repo", repo, "commit", sha, "error", err)
	}
	return onDefault, true
}

// unreadableRepository reports the App answers that mean "Dispatch cannot see this repository",
// which are the unknown currency rather than a fault worth logging.
func unreadableRepository(err error) bool {
	return errors.Is(err, githubapp.ErrRepositoryUnreadable) ||
		errors.Is(err, githubapp.ErrNoInstallation) ||
		errors.Is(err, githubapp.ErrNoAppKey)
}
