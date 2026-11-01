package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type commentInput struct {
	Body    string             `json:"body"`
	Anchor  *model.AnchorInput `json:"anchor"`
	ReplyTo *string            `json:"reply_to"`
	// ReplyToBlock is the id of the block of comment ReplyTo the reply answers ("b2"), checked
	// against that comment before reply_to climbs to the thread's head.
	ReplyToBlock *string `json:"reply_to_block"`
	AskID        *string `json:"ask_id"`
	Mentions     []struct {
		Target string `json:"target"`
	} `json:"mentions"`
	Delivery *string `json:"delivery"`
	// Turn is who holds the turn after this ask reply: "agent" for a progress note
	// that keeps the ask waiting on its asker, "human" (the default for a session
	// author) when the human needs to act. Ignored for a human author, whose reply
	// always hands the turn to the agent. Rejected on a comment that is not an ask reply.
	Turn       *string `json:"turn"`
	Suggestion *struct {
		ReplaceWith string `json:"replace_with"`
	} `json:"suggestion"`
	Actor *model.Actor `json:"actor"`
}

// askReplyTurn is who holds the turn once an ask reply by actor is posted: a human's
// reply always hands it to the agent; a session's reply hands it to the human unless
// the request marks it a progress note (turn "agent").
func askReplyTurn(actor model.Actor, requested *string) string {
	if actor.Kind == "user" {
		return "agent"
	}
	if requested != nil {
		return *requested
	}
	return "human"
}

func validateCommentMentions(input commentInput) (string, []string, error) {
	if len(input.Mentions) == 0 {
		if input.Delivery != nil {
			return "", nil, errorf(http.StatusBadRequest, "MENTION_INPUT", "delivery requires mentions")
		}
		return "", nil, nil
	}
	if len(input.Mentions) > 10 {
		return "", nil, errorf(http.StatusBadRequest, "MENTION_INPUT", "mentions allow at most 10 targets")
	}
	delivery := "steer"
	if input.Delivery != nil {
		delivery = *input.Delivery
		if !validDelivery(delivery) {
			return "", nil, errorf(http.StatusBadRequest, "MENTION_INPUT", "delivery must be one of btw, aside, steer")
		}
	}
	targets := make([]string, 0, len(input.Mentions))
	seen := make(map[string]struct{}, len(input.Mentions))
	for _, mention := range input.Mentions {
		if _, err := model.ParseRoute(mention.Target); err != nil {
			return "", nil, errorf(http.StatusBadRequest, "MENTION_INPUT", "mention target must be role:<name> or session:<id>")
		}
		if _, duplicate := seen[mention.Target]; duplicate {
			return "", nil, errorf(http.StatusBadRequest, "MENTION_INPUT", "mentions must not repeat a target")
		}
		seen[mention.Target] = struct{}{}
		targets = append(targets, mention.Target)
	}
	return delivery, targets, nil
}

type commentThreadTarget struct {
	ReplyTo     *string
	AskID       *string
	ReplyRoot   *model.Comment
	AskQuestion string
	AskState    string
	// AskMoved says the ask is an approval request a version moved past the one its agent last
	// handed to the human (approvalMoved): it waits on its agent whatever a reply's turn says.
	AskMoved bool
	// ReplyBlock is the block of the comment reply_to named that the reply answers, nil when it
	// names none: that comment's, not the thread head's that ReplyTo then holds.
	ReplyBlock *namedBlock
}

// openAsk says the thread hangs under an open ask, the only kind with a turn.
func (t commentThreadTarget) openAsk() bool {
	return t.AskID != nil && t.AskState == "open"
}

// replyTurn is who holds the turn once a reply into this thread is posted. Only an open ask
// has a turn to hold: a reply under an answered or resolved ask records none, so the column
// always means "who the ask waits on after this".
func (t commentThreadTarget) replyTurn(actor model.Actor, requested *string) *string {
	if !t.openAsk() {
		return nil
	}
	return new(askReplyTurn(actor, requested))
}

// waitingOnAfter is whom the thread's open ask waits on once a reply recording turn (replyTurn's)
// is its newest comment; "" when the thread is not an open ask's. It is waitingOnExpression for that
// row. The comment write holds the owner row that every version write, hand-back and comment insert
// takes first, so the ask describeAsk read is the one the reply commits beside, and the reply is the
// thread's newest (migration 0065): a moved approval request waits on its agent, and otherwise the
// reply's own turn decides, since a hand-back can only have recorded an older reply.
func (t commentThreadTarget) waitingOnAfter(turn *string) string {
	switch {
	case !t.openAsk():
		return ""
	case t.AskMoved:
		return "agent"
	default:
		return *turn
	}
}

// eventThread is what a comment event says about the thread the comment joined, given the
// derived ask turn after this comment is its newest reply.
func (t commentThreadTarget) eventThread(waitingOn string) commentEventThread {
	thread := commentEventThread{AskQuestion: t.AskQuestion, AskState: t.AskState, AskWaitingOn: waitingOn}
	if t.ReplyTo != nil {
		thread.ThreadRootID = *t.ReplyTo
	}
	return thread
}

func (s *server) createComment(w http.ResponseWriter, r *http.Request) {
	s.createCommentFor(w, r, issueOwner(r.PathValue("key")))
}

// commentRecovery is the recovery point a keyed comment commits with itself
// (bindRequestKeyRecovery): the comment, whose mentions deliver it.
type commentRecovery struct {
	CommentID string `json:"comment_id"`
}

// replayComment answers a repeat of a keyed comment that committed: the comment with its mentions
// and deliveries as they stand, once every mention it still owes has been delivered, in the first
// answer's shape - its advice and ask_waiting_on kept - under the comment's own 201. A mention owes
// its delivery when its process died after the comment committed and before the mention's one
// attempt was sent: that attempt is still pending, and its claim, if any, has lapsed. It is resumed
// under its own number, so the listener drops a frame that did land, and bound to it, as the
// comment's own first delivery is (deliverResolvedCommentMention, attempt 1), so an attempt a
// live sender holds is left to it and no second attempt is opened. A mention someone retried from
// its card has more than one attempt, and owes nothing here.
func (s *server) replayComment(w http.ResponseWriter, r *http.Request, point commentRecovery, stored []byte) {
	ctx, pool := r.Context(), s.deps.Store.Pool
	comment, err := s.loadComment(ctx, pool, point.CommentID)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	owed, err := s.owedCommentMentions(ctx, comment.ID)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if len(owed) > 0 {
		created, err := s.loadCommentCreatedEvent(ctx, pool, comment.ID)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		// Detached from the request, as the comment's own deliveries are: a client that gives up
		// on the repeat must not strand the mention it started.
		sending := context.WithoutCancel(ctx)
		for _, mention := range owed {
			if _, err := s.deliverResolvedCommentMention(
				sending, comment, created, mention, comment.Author, 1, s.newTakeoverRetry(),
			); err != nil {
				s.writeHandlerError(w, r, err)
				return
			}
		}
	}
	hydrated, err := s.hydrateCommentSideTables(ctx, pool, []model.Comment{comment})
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.writeRefreshed(w, r, stored, hydrated[0])
}

// owedCommentMentions is each mention of the comment whose one attempt is pending with no live
// claim (claimLapsed), in the mode it was opened with.
func (s *server) owedCommentMentions(ctx context.Context, commentID string) ([]ResolvedMention, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select target, min(delivery), count(*), coalesce(bool_and(state = 'pending' and `+claimLapsed+`), false)
		from comment_deliveries where comment_id = $1
		group by target
		order by target
	`, commentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var owed []ResolvedMention
	for rows.Next() {
		var target, delivery string
		var attempts int
		var stranded bool
		if err := rows.Scan(&target, &delivery, &attempts, &stranded); err != nil {
			return nil, err
		}
		if attempts == 1 && stranded {
			owed = append(owed, ResolvedMention{Target: target, Delivery: delivery})
		}
	}
	return owed, rows.Err()
}

// validateCommentThreadInput refuses the thread a comment names on the request's own terms: the
// field combinations a comment may not carry, and a reply_to that is no comment id at all. It
// reads nothing, so createCommentFor takes it before the documents' writer gates
// (normalizeCommentThreadTarget answers everything that needs the rows).
func validateCommentThreadInput(input commentInput) error {
	if input.ReplyTo != nil && input.AskID != nil {
		return errorf(http.StatusBadRequest, "INVALID_COMMENT", "reply_to and ask_id cannot both be set")
	}
	if input.Anchor != nil && (input.ReplyTo != nil || input.AskID != nil) {
		return errorf(http.StatusBadRequest, "INVALID_COMMENT", "replies cannot carry anchors")
	}
	if input.Suggestion != nil && (input.ReplyTo != nil || input.AskID != nil) {
		return errorf(http.StatusBadRequest, "INVALID_COMMENT", "replies cannot carry suggestions")
	}
	if input.ReplyToBlock != nil && input.AskID != nil {
		return errorf(http.StatusBadRequest, "INVALID_COMMENT", replyBlockInAskThread)
	}
	if input.ReplyToBlock != nil && input.ReplyTo == nil {
		return errorf(http.StatusBadRequest, "INVALID_COMMENT", "reply_to_block requires reply_to, the comment whose block it names")
	}
	if input.ReplyTo != nil {
		if strings.TrimSpace(*input.ReplyTo) == "" {
			return errorf(http.StatusBadRequest, "INVALID_COMMENT", "reply_to must be a full comment id")
		}
		if _, err := uuid.Parse(*input.ReplyTo); err != nil {
			return errorf(http.StatusBadRequest, "INVALID_COMMENT", "reply_to must be a full comment id")
		}
	}
	return nil
}

// normalizeCommentThreadTarget reads the thread the comment joins, under the write's locks. Its
// caller has already refused an input no row can make good (validateCommentThreadInput).
func (s *server) normalizeCommentThreadTarget(
	ctx context.Context,
	tx pgx.Tx,
	owner owner,
	input commentInput,
) (commentThreadTarget, error) {
	if input.ReplyTo != nil {
		parent, err := s.loadOwnedCommentForUpdate(ctx, tx, owner, *input.ReplyTo)
		if err != nil {
			return commentThreadTarget{}, err
		}
		target, err := s.threadHeadOf(ctx, tx, owner, parent)
		if err != nil || input.ReplyToBlock == nil {
			return target, err
		}
		if target.AskID != nil {
			return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", replyBlockInAskThread)
		}
		// The block is the named comment's, which the climb left locked after the owner.
		target.ReplyBlock, err = s.replyBlockOf(ctx, tx, refs.ItemComment, parent.ID, *input.ReplyToBlock, "reply_to_block", "INVALID_COMMENT")
		if err != nil {
			return commentThreadTarget{}, err
		}
		return target, nil
	}
	if input.AskID == nil {
		return commentThreadTarget{}, nil
	}
	if strings.TrimSpace(*input.AskID) == "" {
		return commentThreadTarget{}, s.askIDInputForOwner(ctx, tx, owner)
	}
	if _, err := uuid.Parse(*input.AskID); err != nil {
		return commentThreadTarget{}, s.askIDInputForOwner(ctx, tx, owner)
	}
	return s.describeAsk(ctx, tx, owner, input.AskID)
}

// threadHeadOf climbs reply_to from an already-locked comment to the head of its thread: the
// ask the thread hangs under, or the thread's root comment when it hangs under no ask. The
// walk is bounded like every other parent walk (the outbox's thread walk, the issue ancestor
// walk): comments.reply_to has no acyclicity constraint, so a cyclic chain answers 400
// instead of holding the transaction open.
func (s *server) threadHeadOf(
	ctx context.Context, tx pgx.Tx, owner owner, from model.Comment,
) (commentThreadTarget, error) {
	root := from
	for depth := 1; ; depth++ {
		if root.AskID != nil {
			return s.describeAsk(ctx, tx, owner, root.AskID)
		}
		if root.ReplyTo == nil {
			return commentThreadTarget{ReplyTo: &root.ID, ReplyRoot: &root}, nil
		}
		if depth >= parentDepthCap {
			return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "reply_to must identify a comment on this owner")
		}
		next, err := s.loadOwnedCommentForUpdate(ctx, tx, owner, *root.ReplyTo)
		if err != nil {
			return commentThreadTarget{}, err
		}
		root = next
	}
}

// describeAsk reads the ask on this owner that a reply joins, as the thread it heads, refusing one
// that belongs to another.
func (s *server) describeAsk(
	ctx context.Context, tx pgx.Tx, owner owner, askID *string,
) (commentThreadTarget, error) {
	target := commentThreadTarget{AskID: askID}
	err := tx.QueryRow(ctx, `
		select a.question, a.state, `+approvalMoved+`
		from asks a
		where a.id = $1
		  and a.issue_key is not distinct from $2
		  and a.artifact_id is not distinct from $3
	`, *askID, owner.IssueKey, owner.ArtifactID).Scan(&target.AskQuestion, &target.AskState, &target.AskMoved)
	if errors.Is(err, pgx.ErrNoRows) {
		return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "ask_id must identify an ask on this owner")
	}
	return target, err
}

// commentRoute is the owner's delivery route as the Envoy listener resolved it before the
// comment's transaction opened: the route string the pre-transaction read saw, and the live
// session it names. checked is false where the write consults no route at all - a project
// document, a comment with no mentions, an owner that does not exist - and the locked row is
// then not compared against it.
type commentRoute struct {
	checked   bool
	route     string
	sessionID *string
}

// commentResolution is what the Envoy listener said about a comment before its transaction
// opened: the session each mention names, and the owner's route with the session it names.
// takeovers is each mention's take-over pacing (resolveThroughTakeover), which its sender carries
// on in after the commit. decidesRoutes is set when the comment reaches sessions beyond its
// mentions, so the mentions were resolved through a take-over and the comment's other routes are
// decided on them; the comment.created event records it (mentions_decide_routes).
type commentResolution struct {
	mentions      []ResolvedMention
	takeovers     []takeoverRetry
	route         commentRoute
	decidesRoutes bool
}

// commentWrite is the open comment transaction and what its locked owner said: the owner's
// lifecycle status (nil for a project document) and the resolution that row agreed with.
// ctx carries the writer gates of the documents the comment writes, which release hands on once
// the comment has committed and published.
type commentWrite struct {
	tx      pgx.Tx
	status  *string
	ctx     context.Context
	release func()
	commentResolution
}

// resolveCommentDeliveries makes every Envoy listener lookup a comment needs, with no transaction
// open, so no listener call costs a pooled connection: each mention, and the owner's delivery route
// when the comment mentions anyone. A route the comment already mentions is resolved by that
// mention, so the ordinary mention comment makes no second listener call for it.
//
// The transaction decides from these which sessions the comment's other routes skip: the issue's
// route when a mention resolved to its session (suppress_route), and every session route the
// outbox publishes - the issue's claimant, the thread's sessions, the ask's followers, the
// document's authors - for each session a mention resolved to (the payload's mentions). A comment
// that has such a route - the issue's route is set, it replies to a thread, it is on a document,
// or readCommentRoutes found a session route on its issue or ask - resolves each mention and the
// route through a listener take-over (resolveThroughTakeover), because a mention that did not
// resolve here leaves those routes on, and the same mention resolved by its sender after the
// commit would reach a session those routes reach too: the session would hear the comment twice.
// So the comment.created event records that its routes were decided on these resolutions
// (mentions_decide_routes), and each mention's attempt 1 keeps its resolution for whichever sender
// finishes it, the delivery sweep and a Retry included (deliverResolvedCommentMention). A comment
// with no such route commits at once, and its mentions' senders wait out the take-over under the
// attempts the commit made durable, resolving the mention again. Every lookup runs at once, so a
// comment waits about one take-over window however many mentions it carries
// (validateCommentMentions allows ten).
// The lookups share the request's tracking mark because they call only the listener and never
// take a pooled connection; one that read the database would need a mark of its own
// (store.ForConcurrentRead), as the senders after the commit take.
func (s *server) resolveCommentDeliveries(
	ctx context.Context, owner owner, input commentInput, targets []string, delivery string,
) (commentResolution, error) {
	if len(targets) == 0 {
		return commentResolution{}, nil
	}
	route, sessionRoutes, err := s.readCommentRoutes(ctx, owner, input.AskID)
	if err != nil {
		return commentResolution{}, err
	}
	wait := route.route != "" || sessionRoutes || input.ReplyTo != nil || input.Anchor != nil || owner.ArtifactID != nil
	resolved := commentResolution{
		mentions:      make([]ResolvedMention, len(targets)),
		takeovers:     make([]takeoverRetry, len(targets)),
		route:         route,
		decidesRoutes: wait,
	}
	mentioned := slices.Index(targets, route.route)
	var routeResolution ResolvedMention
	var lookups sync.WaitGroup
	for index, target := range targets {
		lookups.Go(func() {
			resolved.mentions[index], resolved.takeovers[index] = s.resolveThroughTakeover(ctx, target, delivery, wait)
		})
	}
	if route.route != "" && mentioned < 0 {
		lookups.Go(func() { routeResolution, _ = s.resolveThroughTakeover(ctx, route.route, "steer", wait) })
	}
	lookups.Wait()
	switch {
	case route.route == "":
	case mentioned >= 0:
		resolved.route.sessionID = resolved.mentions[mentioned].SessionID
	default:
		resolved.route.sessionID = routeResolution.SessionID
	}
	return resolved, nil
}

// readCommentRoutes reads the owner's delivery route from the pool, locking nothing; the comment's
// transaction re-reads it under the owner's lock (lockCommentOwner). sessionRoutes reports, from
// the same read, whether the outbox reaches a session of the issue beyond the route and the
// comment's mentions whatever the comment's thread or anchor: the issue's claimant is a session
// (the outbox's publishClaimantRoute), the ask the comment replies to by askID has a follower
// (publishFollowerRoutes), or the issue's primary document credits a session
// (publishPrimaryDocumentAuthors, which reads the credits through the outbox's documentCreditsSQL
// and sessionCreditSQL; the query below reads them the same way). Like the route, these are read
// before the transaction and not rechecked under the lock: the outbox reads them again when it
// publishes in any case.
func (s *server) readCommentRoutes(ctx context.Context, owner owner, askID *string) (commentRoute, bool, error) {
	if owner.IssueKey == nil {
		return commentRoute{}, false, nil
	}
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return commentRoute{}, false, errorf(http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "database unavailable")
	}
	// An ask id that is no uuid names no ask; the transaction refuses it (normalizeCommentThreadTarget).
	var ask *string
	if askID != nil {
		if _, err := uuid.Parse(*askID); err == nil {
			ask = askID
		}
	}
	var stored *string
	var sessionRoutes bool
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select i.route,
		       coalesce(i.claimed_by ->> 'kind' = 'session' and coalesce(i.claimed_by ->> 'id', '') <> '', false)
		       or exists (select 1 from ask_followers f where f.ask_id = $2::uuid)
		       or exists (
		           select 1
		           from artifacts a
		           where a.issue_key = i.key and a.is_primary and a.kind = 'doc'
		             and (a.created_by ->> 'kind' = 'session' and coalesce(a.created_by ->> 'id', '') <> ''
		                  or exists (
		                      select 1
		                      from artifact_versions v
		                      cross join lateral jsonb_array_elements(nullif(v.authors, 'null'::jsonb)) as credit(author)
		                      where v.artifact_id = a.id
		                        and credit.author ->> 'kind' = 'session' and coalesce(credit.author ->> 'id', '') <> ''
		                  ))
		       )
		from issues i
		where i.key = $1
	`, *owner.IssueKey, ask).Scan(&stored, &sessionRoutes)
	if errors.Is(err, pgx.ErrNoRows) {
		// The transaction answers for an owner that does not exist; there is nothing to resolve.
		return commentRoute{}, false, nil
	}
	if err != nil {
		return commentRoute{}, false, err
	}
	route := commentRoute{checked: true}
	if stored != nil {
		route.route = *stored
	}
	return route, sessionRoutes, nil
}

// gatedCommentWrite takes the writer gates of the documents the comment writes - the one its
// anchor marks, and the one its reply's thread root is anchored to (newCommentDocuments) - with
// no transaction open, then opens the comment's transaction under them (beginCommentWrite). The
// listener resolution ran before, so no document waits on the listener.
func (s *server) gatedCommentWrite(
	ctx context.Context, owner owner, input commentInput, resolved commentResolution,
) (commentWrite, error) {
	write, gatedCtx, release, err := gatedWrite(ctx, s.deps.Docs,
		func(ctx context.Context) ([]string, error) {
			return newCommentDocuments(ctx, s.documentQueryer(), owner, input)
		},
		func(ctx context.Context, gated []string) (commentWrite, error) {
			return s.beginCommentWrite(ctx, owner, input, resolved, gated)
		})
	if err != nil {
		return commentWrite{}, err
	}
	write.ctx, write.release = gatedCtx, release
	return write, nil
}

// beginCommentWrite opens the comment's transaction, locks its owner, and verifies that the
// route the listener resolved is still the one the locked issue row carries, and that the
// documents the comment writes are still the ones gated. A route that moved in between leaves
// the resolution unusable, so the write is resolved once more rather than suppressing the wrong
// session's fan-out; documents that moved are gated once more (gatedWrite).
func (s *server) beginCommentWrite(
	ctx context.Context, owner owner, input commentInput, resolved commentResolution, gated []string,
) (commentWrite, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return commentWrite{}, err
	}
	// The transaction is the caller's on success only, so every refusal unwinds here: a later
	// check that forgot its own rollback would hold a pooled connection for the process's life.
	status, err := s.lockCommentOwner(ctx, tx, owner, resolved.route)
	if err == nil {
		var named []string
		if named, err = newCommentDocuments(ctx, tx, owner, input); err == nil && !sameDocuments(gated, named) {
			err = errDocumentsMoved
		}
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		return commentWrite{}, err
	}
	return commentWrite{tx: tx, status: status, commentResolution: resolved}, nil
}

// lockCommentOwner takes the owner's lock inside an open comment transaction and returns its
// lifecycle status, refusing with errStaleResolution when the locked issue no longer carries the
// route the resolution was taken for.
func (s *server) lockCommentOwner(ctx context.Context, tx pgx.Tx, owner owner, route commentRoute) (*string, error) {
	status, err := s.requireOpenOwnerStatus(ctx, tx, owner)
	if err != nil {
		return nil, err
	}
	if !route.checked {
		return status, nil
	}
	var stored *string
	if err := tx.QueryRow(ctx, `select route from issues where key = $1`, *owner.IssueKey).Scan(&stored); err != nil {
		return nil, err
	}
	locked := ""
	if stored != nil {
		locked = *stored
	}
	if locked != route.route {
		return nil, errStaleResolution
	}
	return status, nil
}

func (s *server) createCommentFor(w http.ResponseWriter, r *http.Request, owner owner) {
	var input commentInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if input.Suggestion != nil {
		input.Suggestion.ReplaceWith = pmdoc.LineFeeds(input.Suggestion.ReplaceWith)
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	if input.Suggestion == nil && strings.TrimSpace(input.Body) == "" {
		writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "comment body is required")
		return
	}
	if length := len16(input.Body); length > contracts.DispatchBodyMax {
		capExceeded(w, "body", length, contracts.DispatchBodyMax)
		return
	}
	delivery, mentionTargets, err := validateCommentMentions(input)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	// What the request refuses itself, before any lookup and before the documents' writer gates:
	// the thread it names, the turn it carries and the shape of its anchor all read no row, so a
	// comment that can never be written waits on nothing. A turn with neither ask_id nor reply_to
	// is one of those: only ask_id, or reply_to climbing to an ask's thread, ever makes the thread
	// an ask's, so no row could make this comment an ask's reply.
	if err := validateCommentThreadInput(input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if input.Turn != nil && input.AskID == nil && input.ReplyTo == nil {
		writeError(w, "TURN_REQUIRES_ASK", http.StatusBadRequest, "turn is only valid on a reply to an ask")
		return
	}
	if input.Turn != nil && *input.Turn != "human" && *input.Turn != "agent" {
		writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "turn must be human or agent")
		return
	}
	if err := validateAnchorInput(input.Anchor); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}

	// Every Envoy listener lookup this comment needs happens here, with no transaction and no
	// pooled connection held (resolveCommentDeliveries). beginCommentWrite re-reads the issue's
	// route under the owner's lock and refuses a resolution the locked row no longer agrees with -
	// and refuses, under that same lock, an owner this pooled read already found missing or
	// closed, which is why a comment the write cannot accept pays for no lookup here.
	if len(mentionTargets) > 0 {
		if err := s.refuseClosedOwnerBeforeResolving(r.Context(), owner); err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
	}
	start, err := resolveThenLock(r.Context(),
		func(ctx context.Context) (commentResolution, error) {
			return s.resolveCommentDeliveries(ctx, owner, input, mentionTargets, delivery)
		},
		func(ctx context.Context, resolved commentResolution) (commentWrite, error) {
			return s.gatedCommentWrite(ctx, owner, input, resolved)
		},
		errorf(http.StatusConflict, "ROUTE_CHANGED",
			"the issue's delivery route changed while this comment was being written; post it again"),
	)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	// The gates end at the commit below, before the mention deliveries: a later writer of these
	// documents waits on this comment's transaction, never on the listener. The defer is the
	// error paths' release.
	defer start.release()
	r = r.WithContext(start.ctx)
	tx, status, route, resolvedMentions := start.tx, start.status, start.route, start.mentions
	mentions := make([]model.Mention, 0, len(resolvedMentions))
	mentionedSessions := make(map[string]struct{}, len(resolvedMentions))
	for _, resolved := range resolvedMentions {
		mentions = append(mentions, model.Mention{
			Target: resolved.Target, Delivery: resolved.Delivery, SessionID: resolved.SessionID,
		})
		if resolved.SessionID != nil {
			mentionedSessions[*resolved.SessionID] = struct{}{}
		}
	}
	defer tx.Rollback(r.Context())
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	threadTarget, err := s.normalizeCommentThreadTarget(r.Context(), tx, owner, input)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	// The turn's value was refused before the gate, as was a turn on a comment that names no
	// thread at all; whether the reply_to this one names climbs to an ask's thread is this
	// transaction's to say.
	if input.Turn != nil && threadTarget.AskID == nil {
		writeError(w, "TURN_REQUIRES_ASK", http.StatusBadRequest, "turn is only valid on a reply to an ask")
		return
	}
	input.ReplyTo = threadTarget.ReplyTo
	input.AskID = threadTarget.AskID
	replyRoot := threadTarget.ReplyRoot
	suppressRoute := false
	suppressedRoute := ""
	var suppressedRouteSessionID *string
	if route.sessionID != nil {
		if _, mentioned := mentionedSessions[*route.sessionID]; mentioned {
			suppressRoute = true
			suppressedRoute = route.route
			suppressedRouteSessionID = route.sessionID
		}
	}
	turn := threadTarget.replyTurn(actor, input.Turn)
	var rowID string
	if err := tx.QueryRow(r.Context(), `select gen_random_uuid()::text`).Scan(&rowID); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	markKind := docs.MarkComment
	if input.Suggestion != nil {
		markKind = docs.MarkSuggestion
	}
	anchor, artifactName, snapshot, err := s.resolveAnchor(documentCtx, tx, owner, input.Anchor, markKind, rowID, actor)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}

	projection := model.Comment{Anchor: anchor}
	if input.Suggestion != nil {
		projection.Suggestion = &model.Suggestion{ReplaceWith: input.Suggestion.ReplaceWith}
	}
	projectionKind, err := s.commentProjectionKind(documentCtx, projection)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	author, err := encodeJSON(actor)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	var anchorJSON any
	if anchor != nil {
		anchorJSON, err = encodeJSON(anchor)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
	}
	var suggestion *model.Suggestion
	var suggestionJSON any
	if input.Suggestion != nil {
		suggestion = &model.Suggestion{ReplaceWith: input.Suggestion.ReplaceWith}
		suggestionJSON, err = encodeJSON(suggestion)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
	}
	var comment model.Comment
	if err := tx.QueryRow(r.Context(), `
		insert into comments (id, issue_key, artifact_id, author, body, anchor, reply_to, ask_id, turn, suggestion, replies_to_block)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		returning created_at
	`, rowID, owner.IssueKey, owner.ArtifactID, author, input.Body, anchorJSON, input.ReplyTo, input.AskID, turn, suggestionJSON,
		threadTarget.ReplyBlock.nodeID()).Scan(&comment.CreatedAt); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	// The recovery point in the comment's own transaction: a repeat under the key is answered with
	// this comment, its owed mentions delivered (replayComment), however this request ends; a repeat
	// that took the key over meanwhile refuses it here, and the comment rolls back.
	if err := bindRequestKeyRecovery(r.Context(), tx, commentRecovery{CommentID: rowID}); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	waitingOn := threadTarget.waitingOnAfter(turn)
	if input.AskID != nil {
		if err := asks.FollowAuthor(r.Context(), tx, *input.AskID, actor); err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
	}
	comment.ID = rowID
	comment.IssueKey = owner.IssueKey
	comment.ArtifactID = owner.ArtifactID
	comment.Author = actor
	comment.Body = input.Body
	comment.Anchor = anchor
	comment.ReplyTo = input.ReplyTo
	comment.AskID = input.AskID
	comment.Turn = turn
	comment.Suggestion = suggestion
	comment.Mentions = mentions
	comment.Deliveries = []model.CommentDelivery{}
	comment.RepliesToBlock = threadTarget.ReplyBlock.nodeID()
	if comment.Anchor != nil {
		if err := s.deps.Docs.ProjectMark(documentCtx, comment.Anchor.ArtifactID, comment.Anchor.MarkID, commentMarkRecord(comment, nil, projectionKind), actor); err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
	}
	var reopenedRoot *model.Comment
	var reopenedArtifactName string
	if replyRoot != nil {
		root := *replyRoot
		// A reply reopens a resolved root, except a decided suggestion's: accepting or rejecting
		// it is final, so the reply joins the thread and leaves who decided it, and when, as they
		// were.
		decided := root.Suggestion != nil && root.Suggestion.Accepted != nil
		if root.Resolved && !decided {
			if _, err := tx.Exec(r.Context(), `
				update comments
				set resolved = false, resolved_by = null, resolved_at = null
				where id = $1
			`, root.ID); err != nil {
				s.writeHandlerError(w, r, err)
				return
			}
			root.Resolved = false
			root.ResolvedBy = nil
			root.ResolvedAt = nil
			reopenedRoot = &root
			reopenedArtifactName, err = s.commentArtifactName(r.Context(), tx, root)
			if err != nil {
				s.writeHandlerError(w, r, err)
				return
			}
		}
		if root.Anchor != nil {
			replies, err := s.loadReplyChain(r.Context(), tx, "reply_to", root.ID)
			if err != nil {
				s.writeHandlerError(w, r, err)
				return
			}
			// A decided or orphaned suggestion's mark is gone, so it has no projection kind to
			// carry; the reply still lands and ProjectMark writes only the marks map, as a reopen
			// or a resolve of that suggestion does.
			rootProjectionKind := ""
			kind, kindErr := s.commentProjectionKind(documentCtx, root)
			if kindErr != nil && !errors.Is(kindErr, docs.ErrAnchorMissing) {
				s.writeHandlerError(w, r, kindErr)
				return
			}
			if kindErr == nil {
				rootProjectionKind = kind
			}
			if err := s.deps.Docs.ProjectMark(documentCtx, root.Anchor.ArtifactID, root.Anchor.MarkID, commentMarkRecord(root, replies, rootProjectionKind), actor); err != nil {
				s.writeHandlerError(w, r, err)
				return
			}
		}
	}
	for _, mention := range resolvedMentions {
		if _, err := tx.Exec(r.Context(), `
			insert into comment_mentions (comment_id, target, delivery, resolved_session_id)
			values ($1, $2, $3, $4)
		`, comment.ID, mention.Target, mention.Delivery, mention.SessionID); err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		resolveError := (*string)(nil)
		if mention.ResolveError != "" {
			resolveError = &mention.ResolveError
		}
		if _, err := tx.Exec(r.Context(), `
			insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, resolve_error, state)
			values ($1, $2, 1, $3, $4, $5, 'pending')
		`, comment.ID, mention.Target, mention.Delivery, mention.SessionID, resolveError); err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
	}
	referenceChanges, err := refs.ReplaceCountedItem(r.Context(), tx, refs.ItemComment, comment.ID, comment.Body, s.deps.ServerURL, comment.CreatedAt)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	events := []model.Event{}
	if snapshot != nil {
		snapshotEvent, err := s.commitArtifactVersionEvent(r.Context(), tx, owner, actor, anchor.ArtifactID, artifactName, *snapshot, nil)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		events = append(events, snapshotEvent)
	}
	if reopenedRoot != nil {
		payload, err := s.commentEventPayload(r.Context(), tx, reopenedRoot, reopenedArtifactName, commentEventThread{}, model.ReferenceChanges{})
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		event, err := s.appendEvent(r.Context(), tx, owner.event(
			"comment.reopened",
			actor,
			payload,
		))
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		events = append(events, event)
	}
	payload, err := s.commentEventPayload(
		r.Context(), tx, &comment, artifactName, threadTarget.eventThread(waitingOn), referenceChanges,
	)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	payload.SuppressRoute = suppressRoute
	payload.SuppressedRoute = suppressedRoute
	payload.SuppressedRouteSessionID = suppressedRouteSessionID
	payload.MentionsDecideRoutes = start.decidesRoutes
	payload.ThreadLogins, err = commentThreadLogins(r.Context(), tx, comment)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, owner.event(
		"comment.created",
		actor,
		payload,
	))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if err := refs.Stamp(r.Context(), tx, "comment", comment.ID, event.ID); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	events = append(events, event)
	var advice *writeAdvice
	if owner.IssueKey != nil && status != nil {
		advice = s.writeAdvice(
			r.Context(), tx, "POST /api/v1/issues/{key}/comments", *owner.IssueKey, actor, "", *status,
		)
	}
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.publish(events...)
	start.release()
	// Each mention's send outlives the request's connection, keeping its values and dropping its
	// cancellation, as createStoredMessage's does: attempt 1 is committed above, and a client that
	// gives up on the comment, or retries it under its key, must not leave that attempt pending
	// for a minute's lease and the delivery sweep. A keyed request's handler runs detached already
	// (keyedWrites); an unkeyed one's mentions are detached here, and http.Server.Shutdown waits
	// for the handler.
	//
	// The mentions are sent at once, each its own claim, send and settle: a sender may wait out a
	// listener take-over before it settles, and one after another those waits would add up past
	// the dispatch command's own request deadline, after the comment has committed, so its retry
	// would post the comment again. Together they wait one window (validateCommentMentions allows
	// ten mentions, so ten senders at most).
	//
	// Each sender takes a tracking mark of its own: the pool's one-connection guard is per mark
	// (store.WithTransactionTracking), so senders sharing the request's would trip it whenever two
	// of them were inside a claim or settle transaction at once, and race on its flag. The marks are
	// taken before any sender starts, so a refusal leaves nothing in flight; the committed attempts
	// stay pending for the delivery sweep.
	sendCtx := context.WithoutCancel(r.Context())
	senders := make([]context.Context, len(resolvedMentions))
	for index := range senders {
		if senders[index], err = store.ForConcurrentRead(sendCtx); err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
	}
	attempts := make([]model.CommentDelivery, len(resolvedMentions))
	failures := make([]error, len(resolvedMentions))
	var sends sync.WaitGroup
	for index, mention := range resolvedMentions {
		sends.Go(func() {
			attempts[index], failures[index] = s.deliverResolvedCommentMention(
				senders[index], comment, event, mention, actor, 1, start.takeovers[index],
			)
		})
	}
	sends.Wait()
	for index := range resolvedMentions {
		if failures[index] != nil {
			s.writeHandlerError(w, r, failures[index])
			return
		}
		comment.Deliveries = append(comment.Deliveries, attempts[index])
	}
	WriteJSON(w, http.StatusCreated, withAdvice(commentWriteResponse{Comment: comment, AskWaitingOn: waitingOn}, advice))
}
