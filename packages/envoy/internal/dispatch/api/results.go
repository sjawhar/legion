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
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// results.go records and reads the results of an issue's checks. A check is a `check` block of
// a Verification document; a result says one run of it passed or failed, at one commit. The
// record route takes a result for a check of the recording issue or of any issue above it
// (refs.IssueAncestors) and appends `result.recorded` on the check's own issue. The read route
// lists the checks of one issue's Verification
// document with every result recorded against each, from any issue, newest first, and says
// whether each still speaks for the work as it stands: a `before_merge` result is current at
// the head of a pull request attributed to the check's own issue, so a child's pull request
// never makes a result against its parent's check current. Currency itself is answered in
// results_currency.go.

// checkResultRole bounds the role a result names: the word the pull-request body line prints.
var checkResultRole = regexp.MustCompile(`^[a-z-]{1,32}$`)

// resultCommitSHA is the commit a result ran at, as the column's own check spells it.
var resultCommitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// The three values a check's `when` takes, the closed enum pmdoc/schema/blocks.json declares
// under `choices`. checkWhenBeforeMerge is also the default a check whose block names none
// carries.
const (
	checkWhenBeforeMerge   = "before_merge"
	checkWhenAfterMerge    = "after_merge"
	checkWhenDefaultBranch = "default_branch"
)

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
// check's: a result against a check of an issue above it is taken, and the event goes on that
// issue. Such a result is no proof of the check, since it reads current only at a head of a
// pull request of the check's own issue (currencyReader). The answer carries the stored row and
// the pull-request body line it implies, so every writer writes one grammar.
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
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	// The check is resolved before the transaction opens. Resolving it folds the live
	// Verification document, which for a document no room holds is a cold read bounded by a
	// minute (docs/read_cache.go), and no issue row lock is held across a read that long: four
	// writers parked on one issue's row lock already cost a read of an unrelated issue the whole
	// seventeen seconds the lock was held (store/pool.go). Nothing the lock protects is settled
	// here — the check's words and its `when` are the document's, not the issue's — and what it
	// does protect, the block's indexed version and digest, is read again under it below.
	check, err := s.resolveCheck(r.Context(), pool, issueKey, input.Check)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	// The one repository the check's issue resolves to, read once for the two callers below:
	// the refusal of a commit it does not hold, and the read-through of the pull requests
	// GitHub names for that commit after the write. It is nil when there is nothing to ask
	// GitHub about — a deployment with no App key, an issue that resolves to no repository,
	// and a read of Dispatch's own rows that failed, which is logged and stores the result as
	// every answer but the proof of absence does.
	//
	// It is the check's issue's repository, not the recording issue's: readThroughPullRequests
	// and the currency reader both resolve check_issue_key, and a refusal keyed on the
	// recording issue would disagree with the currency mark about which repository the commit
	// belongs to. It is resolved through the one helper the currency reader resolves through
	// (results_currency.go, server.checkIssueRepository), under the canonical name the
	// repository is called today: a refusal asking under a name the repository was renamed
	// away from reads the 404 GitHub answers for that name's installation, where the mark
	// reads the commit — so it would store every result of such an issue.
	//
	// The App key is checked here and not in the helper: every use of the answer below is a
	// GitHub round trip, while the currency reader needs the repository even with no App key,
	// for the default-branch answers it reads from Dispatch's own stored rows.
	var checkRepo *string
	if s.deps.GitHub != nil {
		checkRepo = s.checkIssueRepository(r.Context(), pool, check.IssueKey)
	}

	// Asked here for the reason resolveCheck is asked here, before the transaction opens: it is
	// a GitHub round trip, and the comment over that resolve records what a slow read under the
	// two issue row locks costs. Nothing is written before it, so a refusal leaves no row and no
	// event behind.
	if err := s.refuseUnknownCommit(r.Context(), checkRepo, input.Commit); err != nil {
		s.writeHandlerError(w, err)
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())

	// Both rows are locked in one key order, whether or not either is open: a result for an
	// after-merge check is taken on a closed issue, and the lock still stops it closing or
	// reopening under the write. The check's issue is usually the recording issue's ancestor,
	// so locking them in call order would let one transaction hold the child while waiting for
	// the parent and another hold the parent while waiting for the child; lockIssueRows is the
	// one key order every writer of two issue rows takes.
	if _, err := lockIssueRows(r.Context(), tx, issueKey, check.IssueKey); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	recordingOpen, checkOpen, err := issuesOpenForResult(r.Context(), tx, issueKey, check.IssueKey)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The version, digest and `when` the row is judged by are the block's as this transaction
	// sees them, read again under the locks: a document version that landed between the resolve
	// and the lock would otherwise have the result recorded against the words it replaced, and
	// a check reworded from before_merge to after_merge would be judged by the `when` it carried
	// before the lock. The digest is read under the locks either way, and the `when` only when
	// that digest moved, which is the only way the block's words can have changed.
	if err := s.refreshResolvedCheck(r.Context(), tx, &check, input.Check); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The one write a closed issue takes. A check that runs after the merge, or on the default
	// branch, can only be proved once the issue has closed, so refusing it would make the
	// production check unrecordable; every other write on a closed issue — a comment, a
	// document version, a result for a pre-merge check — stays refused as it was.
	if (!recordingOpen || !checkOpen) && check.When != checkWhenAfterMerge && check.When != checkWhenDefaultBranch {
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
	if err := insertCheckResult(r.Context(), tx, &result); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The read-through runs before the event: the Verification tab refetches on
	// result.recorded, and no later event refreshes it, so a tab that read the results before
	// the pull request row landed would show this result older until something else happened.
	s.readThroughPullRequests(r.Context(), check.IssueKey, result.CommitSHA, checkRepo)
	s.publish(event)
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
	artifact, held, err := s.verificationDocument(r.Context(), s.deps.Store.Pool, issueKey, "")
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	body := map[string]any{"issue": issueKey, "verification_artifact_id": artifact.ID, "checks": []model.CheckBlock{}}
	if !held {
		WriteJSON(w, http.StatusOK, body)
		return
	}
	checks, err := s.checkBlocksOf(r.Context(), s.deps.Store.Pool, artifact)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	recorded, err := loadCheckResults(r.Context(), s.deps.Store.Pool, artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	currency := newCurrencyReader(s)
	for index := range checks {
		results := recorded[checks[index].BlockID]
		if results == nil {
			results = []model.CheckResult{}
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

// issuesOpenForResult answers whether the recording issue and the check's issue are open, in
// one query over the rows the caller has already locked (lockIssueRows). It makes no refusal of
// its own for a closed issue, as requireOpenOwnerStatus does: the results route decides for itself,
// since a closed issue takes a result for an after-merge check. An issue with no row is the
// route's 404, which is the lock's answer too: lockIssueRows skips a key nothing holds.
func issuesOpenForResult(ctx context.Context, tx pgx.Tx, issueKey, checkIssueKey string) (bool, bool, error) {
	open := map[string]bool{}
	rows, err := tx.Query(ctx, `select key, closed_at is null from issues where key = any($1)`,
		[]string{issueKey, checkIssueKey})
	if err != nil {
		return false, false, fmt.Errorf("read whether %s and %s are open: %w", issueKey, checkIssueKey, err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var isOpen bool
		if err := rows.Scan(&key, &isOpen); err != nil {
			return false, false, fmt.Errorf("read whether %s and %s are open: %w", issueKey, checkIssueKey, err)
		}
		open[key] = isOpen
	}
	if err := rows.Err(); err != nil {
		return false, false, fmt.Errorf("read whether %s and %s are open: %w", issueKey, checkIssueKey, err)
	}
	recording, held := open[issueKey]
	if !held {
		return false, false, errorf(http.StatusNotFound, "NOT_FOUND", "issue not found")
	}
	check, held := open[checkIssueKey]
	if !held {
		return false, false, errorf(http.StatusNotFound, "NOT_FOUND", "issue not found")
	}
	return recording, check, nil
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

// verificationDocument is one issue's Verification document and whether it has one. slug, when
// it is not "", holds the answer to the document filed under it, which is how a canonical block
// reference names one. A row whose role is null answers to its slug alone: a document the
// release before this one created during the rolling deploy carries no role until the next
// version written under its row repairs it (documentRole), and the slug is meanwhile the only
// thing that says what the document is. Every reader of an issue's Verification document in
// this package goes through here — the results routes, the check resolver and the approval
// request's second document.
func (s *server) verificationDocument(ctx context.Context, q queryer, issueKey, slug string) (model.Artifact, bool, error) {
	artifact, err := scanArtifact(q.QueryRow(ctx, `
		select `+artifactColumns+` from artifacts
		where issue_key = $1 and `+verificationRolePredicate+` and ($2 = '' or slug = $2)
		limit 1
	`, issueKey, slug))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Artifact{}, false, nil
	}
	if err != nil {
		return model.Artifact{}, false, fmt.Errorf("read the Verification document of %s: %w", issueKey, err)
	}
	return artifact, true, nil
}

// resolveCheck finds the check a result names among the Verification documents of the recording
// issue and of every issue above it. The value is a block id, or the canonical block reference
// (`dispatch://KEY/artifact/verification-md#b-<id>`) that names one document when a block id is
// not unique in that set: an id two documents share is 409 CHECK_AMBIGUOUS, and an id no
// Verification document of the set holds — or one that is not a `check` block — is 400
// UNKNOWN_CHECK.
func (s *server) resolveCheck(ctx context.Context, q queryer, issueKey, value string) (resolvedCheck, error) {
	ancestors, err := refs.IssueAncestors(ctx, q, issueKey)
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
		document, held, err := s.verificationDocument(ctx, q, ref.IssueKey, ref.ID)
		if err != nil {
			return resolvedCheck{}, err
		}
		if !held {
			return resolvedCheck{}, unknownCheck(value)
		}
		artifactFilter = document.ID
	}

	rows, err := q.Query(ctx, `
		select a.issue_key, a.slug, d.artifact_id::text, d.changed_version, d.text_sha256
		from document_blocks d
		join artifacts a on a.id = d.artifact_id
		where `+aliasedVerificationRole+`
		  and a.issue_key = any($1) and d.type = 'check' and d.block_id = $2
		  and ($3 = '' or d.artifact_id = $3::uuid)
		order by a.issue_key
	`, scope, blockID, artifactFilter)
	if err != nil {
		return resolvedCheck{}, fmt.Errorf("resolve check %q of %s: %w", value, issueKey, err)
	}
	defer rows.Close()
	var found []resolvedCheck
	var references []string
	for rows.Next() {
		check := resolvedCheck{BlockID: blockID}
		var slug string
		if err := rows.Scan(&check.IssueKey, &slug, &check.ArtifactID, &check.Version, &check.SHA256); err != nil {
			return resolvedCheck{}, fmt.Errorf("resolve check %q of %s: %w", value, issueKey, err)
		}
		found = append(found, check)
		if reference, ok := text.FormatBlockRef(text.Ref{
			Kind: "block", IssueKey: check.IssueKey, ID: slug, Block: blockID,
		}); ok {
			references = append(references, reference)
		}
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
			"check %s is a check of the Verification document of %s: name one with its reference, %s",
			blockID, strings.Join(issues, " and "), strings.Join(references, " or "))
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

// refreshResolvedCheck re-reads, under the locks the transaction holds, the indexed version and
// digest of a check resolved before it opened (recordCheckResult), so the row records the block
// as it stands at the commit, and its `when` with them when that block moved: the refusal a
// closed issue earns turns on `when`, which was read before the lock, and a version landing in
// between can have reworded it. A block the index no longer holds is no check any more.
//
// The digest is document_blocks.text_sha256, the sha256 of the block's whole markdown as the
// index writes it (refs.BlockTextSum over markdown[From:To]), its opening line and that line's
// attributes included, so a check reworded from before_merge to after_merge carries a digest of
// its own. A digest the resolve already read is therefore the same words under the same `when`,
// and the live document is not read a second time for them. That read is the slow one to hold
// two issue row locks across: a document version landing between the resolve and the lock is
// exactly what leaves the read cache cold (docs/read_cache.go), so the one case the second read
// would answer differently is the one it would answer slowest in. Only a digest that moved pays
// for it.
func (s *server) refreshResolvedCheck(ctx context.Context, q queryer, check *resolvedCheck, named string) error {
	resolved := check.SHA256
	err := q.QueryRow(ctx, `
		select changed_version, text_sha256 from document_blocks
		where artifact_id = $1::uuid and block_id = $2 and type = 'check'
	`, check.ArtifactID, check.BlockID).Scan(&check.Version, &check.SHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		return unknownCheck(named)
	}
	if err != nil {
		return fmt.Errorf("read the check %s of document %s: %w", check.BlockID, check.ArtifactID, err)
	}
	if check.SHA256 == resolved {
		return nil
	}
	check.When, check.Text, err = s.checkBlockWords(ctx, check.ArtifactID, check.BlockID)
	return err
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

// typedBlocksIn walks the live markdown of one document and hands visit each block of blockType
// in document order, with its block id. The markdown is read live and parsed; nothing is
// written. A rendering can repeat an id; the block index keys one row per id, and so does this,
// so only the first holder of an id is visited. Both readers of a live document's typed blocks
// come through here: the checks of a Verification document (checkBlocksIn) and the requirements
// of a spec (requirementBlocksIn).
func (s *server) typedBlocksIn(ctx context.Context, artifactID, blockType string, visit func(node *pmdoc.Node, id string)) error {
	markdown, err := s.deps.Docs.Text(ctx, artifactID)
	if err != nil {
		return err
	}
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		return fmt.Errorf("read the %s blocks of document %s: %w", blockType, artifactID, err)
	}
	seen := map[string]struct{}{}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != blockType {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if id == "" {
			return true
		}
		if _, repeated := seen[id]; repeated {
			return true
		}
		seen[id] = struct{}{}
		visit(node, id)
		return true
	})
	return nil
}

// checkBlocksIn is every `check` block of one document, in document order, with its attributes
// and its words (typedBlocksIn).
func (s *server) checkBlocksIn(ctx context.Context, artifactID string) ([]model.CheckBlock, error) {
	var checks []model.CheckBlock
	err := s.typedBlocksIn(ctx, artifactID, "check", func(node *pmdoc.Node, id string) {
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
	})
	if err != nil {
		return nil, err
	}
	return checks, nil
}

// checkBlocksOf is checkBlocksIn with each block's reference, issue and indexed version and
// digest filled in. The reference is built from the document's own slug, which is a property of
// the artifacts row and not of a block row, so a Verification document whose blocks the index
// does not hold yet still answers a reference every check can be addressed by.
func (s *server) checkBlocksOf(ctx context.Context, q queryer, artifact model.Artifact) ([]model.CheckBlock, error) {
	checks, err := s.checkBlocksIn(ctx, artifact.ID)
	if err != nil {
		return nil, err
	}
	if len(checks) == 0 {
		return []model.CheckBlock{}, nil
	}
	issueKey := ""
	if artifact.IssueKey != nil {
		issueKey = *artifact.IssueKey
	}
	indexed := make(map[string]model.CheckBlock, len(checks))
	rows, err := q.Query(ctx, `
		select block_id, changed_version, text_sha256
		from document_blocks where artifact_id = $1 and type = 'check'
	`, artifact.ID)
	if err != nil {
		return nil, fmt.Errorf("read the indexed checks of %s: %w", artifact.ID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var block model.CheckBlock
		if err := rows.Scan(&block.BlockID, &block.Version, &block.SHA256); err != nil {
			return nil, fmt.Errorf("read the indexed checks of %s: %w", artifact.ID, err)
		}
		indexed[block.BlockID] = block
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the indexed checks of %s: %w", artifact.ID, err)
	}
	for index := range checks {
		checks[index].IssueKey = issueKey
		checks[index].Ref, _ = text.FormatBlockRef(text.Ref{
			Kind: "block", IssueKey: issueKey, ID: artifact.Slug, Block: checks[index].BlockID,
		})
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
			if strings.Contains(word, "://") {
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
		check.BlockID, check.When, result.Verdict, result.CommitSHA, result.Role)
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

// insertCheckResult writes one result's row and fills in the created_at the row takes, which
// the record route answers with: the pointer is the caller's own value, not a copy of it.
func insertCheckResult(ctx context.Context, tx pgx.Tx, result *model.CheckResult) error {
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

// loadCheckResults is every result recorded against the checks of one Verification document,
// from any issue, by the block id of the check each names, newest first within each. One query
// serves the whole document: a Verification document holding twenty checks is one round trip,
// not twenty.
func loadCheckResults(ctx context.Context, q queryer, artifactID string) (map[string][]model.CheckResult, error) {
	rows, err := q.Query(ctx, `
		select `+checkResultColumns+`
		from check_results
		where check_artifact_id = $1::uuid
		order by check_block_id, created_at desc, id desc
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read the results of document %s: %w", artifactID, err)
	}
	defer rows.Close()
	results := map[string][]model.CheckResult{}
	for rows.Next() {
		result, err := scanCheckResult(rows)
		if err != nil {
			return nil, fmt.Errorf("read the results of document %s: %w", artifactID, err)
		}
		results[result.CheckBlockID] = append(results[result.CheckBlockID], result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the results of document %s: %w", artifactID, err)
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

// refuseUnknownCommit refuses a result at a commit repo definitely does not hold: 400
// RESULT_COMMIT_UNKNOWN, naming the commit and that `owner/repo`. repo is the repository the
// check's issue resolves to, which recordCheckResult reads once for this refusal and for the
// read-through after the write; a nil repo is nothing to ask GitHub about and no refusal. A
// deployment with no App key resolves none, so the key is not checked again here.
//
// It runs before the transaction opens, so a refused result leaves no check_results row and no
// result.recorded event behind. Dispatch stored a result at a mistyped commit no repository
// held once, and a reader could not tell it from the real head, since both print as twelve
// characters.
//
// It refuses on nothing but GitHub's proof that the repository does not hold the commit, which
// delivery.RepositoryHoldsCommit defines as the one 422 whose message says GitHub found no
// commit for the SHA. No other GitHub answer can block the one write a closed issue takes, the
// production check's after-merge result — whose merge commit is on the default branch and is
// held anyway.
func (s *server) refuseUnknownCommit(ctx context.Context, repo *string, sha string) error {
	if repo == nil {
		return nil
	}
	held, err := delivery.RepositoryHoldsCommit(ctx, s.deps.GitHub, *repo, sha)
	if err != nil {
		slog.Info("dispatch: GitHub could not say whether a recorded result's commit is held",
			"repo", *repo, "commit", sha, "error", err)
		return nil
	}
	if held {
		return nil
	}
	return errorf(http.StatusBadRequest, "RESULT_COMMIT_UNKNOWN",
		"%s holds no commit %s: record the result once the commit is pushed", *repo, sha)
}

// readThroughPullRequests teaches Dispatch the pull requests GitHub names for a result's commit
// when that commit is the head of none it already knows for the check's issue, so a result
// recorded at the head of a pull request no event has reached Dispatch for is current at once.
// It asks about the check's own issue and reads through repo, the one repository that issue
// resolves to, which recordCheckResult resolved before the write: the pair
// currencyReader.headOf answers from. A nil repo — a deployment with no App key, an issue
// resolving to no repository, or a resolve that failed — leaves nothing to read through, and
// it is answered before the database is read, since no head this read found could be used.
//
// It never fails the record: the row is written, and a currency Dispatch could not establish
// reads as it would have without the call.
func (s *server) readThroughPullRequests(ctx context.Context, checkIssueKey, sha string, repo *string) {
	if repo == nil || s.deps.Store == nil || s.deps.Store.Pool == nil {
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
	if _, _, err := delivery.ReadThroughCommitPulls(ctx, s.deps.Store.Pool, s.deps.GitHub, *repo, sha, time.Now()); err != nil {
		slog.Info("dispatch: GitHub named no pull request for a recorded result's commit",
			"repo", *repo, "commit", sha, "error", err)
	}
}
