package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A mention attempt a Retry from the card overtook stands until that Retry has delivered the
// comment (model.MentionAttemptStandsSQL). These are the paths that rule decides, each on an
// issue with no route, where the mention's own send is the only thing that can hand the session
// the comment: a Retry that fails, one that is sent, one still in flight when the lane reaches
// the attempt it overtook (both outcomes), one in another mode whose frame landed but whose
// receipt timed out, a keyed repeat's queued send, and the delivery sweep's, which sends an
// attempt that stands and settles one a later attempt has delivered the comment in the place of.
// Every one of them must leave the session exactly one copy (copiesOfTheComment): never none,
// and never two.

// bothMentionAttemptsSettled reports whether the comment's mention has two attempts and neither
// is pending.
func bothMentionAttemptsSettled(attempts []model.CommentDelivery) bool {
	return len(attempts) == 2 && attempts[0].State != "pending" && attempts[1].State != "pending"
}

// copiesOfTheComment is how many copies of the comment session was handed: one for every attempt
// of its mention the listener took as a new frame - sent, and not a repeat the stream dropped -
// and one for every copy the outbox published to that session's own topic.
func copiesOfTheComment(
	t *testing.T, database *store.Store, published *recordingOutboxPublisher, commentID, session string,
) int {
	t.Helper()
	var copies int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from comment_deliveries
		where comment_id = $1 and state = 'sent' and not duplicate
	`, commentID).Scan(&copies); err != nil {
		t.Fatalf("count the attempts of %s that delivered the comment: %v", commentID, err)
	}
	for _, topic := range published.topicsAbout(t, commentID) {
		if topic == contracts.AgentSubject(session) {
			copies++
		}
	}
	return copies
}

// awaitUnclaimedPendingAttempt waits, for at most 10 s, until the comment's first attempt for
// target is pending with no claim on it: the shape a lane leaves an attempt it stood down from
// because a later attempt was still in flight (claimCommentDeliveryAttempt).
func awaitUnclaimedPendingAttempt(t *testing.T, database *store.Store, commentID, target string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !unclaimedPending(t, database, commentID, target) {
		if time.Now().After(deadline) {
			t.Fatalf("the attempt of %s for %s is not pending and unclaimed: the lane that reached it "+
				"while a later attempt was in flight neither stood down nor gave its claim back", commentID, target)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// mentionAttemptSplit is how the delivery sweep and the outbox each count the comment's attempt
// for target: owed (model.OwedMentionAttemptSQL, the sweep sends it now), and the outbox's
// three-way split of a pending attempt (model.MentionResumedSQL, model.MentionAbandonedSQL,
// model.MentionLiveSQL).
func mentionAttemptSplit(
	t *testing.T, database *store.Store, commentID, target string, attempt int,
) (owed, resumed, abandoned, live bool) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select d.state = 'pending' and `+model.OwedMentionAttemptSQL("d")+`,
		       d.state = 'pending' and `+model.MentionResumedSQL("d")+`,
		       d.state = 'pending' and `+model.MentionAbandonedSQL("d")+`,
		       d.state = 'pending' and `+model.MentionLiveSQL("d")+`
		from comment_deliveries d
		where d.comment_id = $1 and d.target = $2 and d.attempt = $3
	`, commentID, target, attempt).Scan(&owed, &resumed, &abandoned, &live); err != nil {
		t.Fatalf("read how attempt %d of %s for %s counts: %v", attempt, commentID, target, err)
	}
	return owed, resumed, abandoned, live
}

// retryTheQueuedMention asks for a Retry of the queued comment's mention of s1 and answers the
// attempt it reports.
func retryTheQueuedMention(t *testing.T, lane queuedMention) commentDeliveryRead {
	t.Helper()
	retry := dispatchRequest(t, lane.handler, http.MethodPost, "/api/v1/comments/"+lane.queued+"/deliveries",
		map[string]any{"delivery": "steer"}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry the queued mention: status=%d body=%s", retry.Code, retry.Body.String())
	}
	return decodeBody[commentDeliveryRead](t, retry)
}

// sendsOfTheQueuedComment is how many sends of the comment queued behind the stalled one the
// listener took, refused ones included.
func sendsOfTheQueuedComment(listener *mentionListener) int {
	sends := 0
	for _, send := range listener.sent() {
		if send.body == "second" {
			sends++
		}
	}
	return sends
}

// The control the paths below are read against: with no Retry at all, the queued attempt is the
// only one of its mention, and its lane sends it once the send ahead of it lands.
func TestTheQueuedAttemptIsSentWhenNoRetryOvertakesIt(t *testing.T) {
	lane := aMentionQueuedBehindAStalledSend(t, "")
	published := &recordingOutboxPublisher{}
	runOutboxBeside(t, lane.deps, published)

	lane.release()
	settled := waitForCommentDeliveries(t, lane.database, lane.queued, "the attempt settled",
		func(attempts []model.CommentDelivery) bool {
			return len(attempts) == 1 && attempts[0].State != "pending"
		})
	if settled[0].State != "sent" {
		t.Fatalf("the queued attempt = %#v, want its lane to have sent it", settled[0])
	}
	if copies := copiesOfTheComment(t, lane.database, published, lane.queued, "s1"); copies != 1 {
		t.Fatalf("s1 was handed %d copies of the comment, want the lane's send alone", copies)
	}
}

// A Retry that fails while the first attempt waits on a held lane delivered nothing, so the
// attempt it overtook stands and its lane sends it: the issue has no route, so that send is the
// only thing that can hand s1 the comment, and s1 gets it exactly once. (The reviewer's
// reproduction of INFRA-606: before this the lane settled the attempt superseded without sending
// it, and s1 got the comment from nothing at all.)
func TestAFailedRetryLeavesTheQueuedAttemptToItsLane(t *testing.T) {
	lane := aMentionQueuedBehindAStalledSend(t, "")
	published := &recordingOutboxPublisher{}
	runOutboxBeside(t, lane.deps, published)
	lane.listener.refuseSends("s1")
	if retried := retryTheQueuedMention(t, lane); retried.Attempt != 2 || retried.State != "failed" {
		t.Fatalf("the Retry = %#v, want an attempt 2 of its own, failed: attempt 1 waits on the held lane "+
			"and s1's host refused this send", retried)
	}
	// s1's host is back before the lane reaches the attempt the Retry overtook.
	lane.listener.allowSends("s1")

	lane.release()
	settled := waitForCommentDeliveries(t, lane.database, lane.queued, "both attempts settled", bothMentionAttemptsSettled)
	if settled[0].State != "sent" {
		t.Fatalf("the attempt the failed Retry overtook = %#v, want the lane to have sent it: the Retry "+
			"delivered nothing and no copy of this comment reaches s1 otherwise", settled[0])
	}
	if settled[1].State != "failed" {
		t.Fatalf("the Retry's attempt = %#v, want it failed", settled[1])
	}
	destinations := awaitPublishedCommentCreated(t, lane.database, lane.queued)
	if slices.Contains(destinations, contracts.AgentSubject("s1")) {
		t.Fatalf("comment.created reached %v, want nothing of s1's own: the mention delivered the comment", destinations)
	}
	if copies := copiesOfTheComment(t, lane.database, published, lane.queued, "s1"); copies != 1 {
		t.Fatalf("s1 was handed %d copies of the comment, want the lane's send alone", copies)
	}
}

// A Retry that is sent while the first attempt waits on a held lane takes the mention over for
// good: the lane settles the attempt it overtook superseded, gives its claim back and sends
// nothing, the sweep owes that attempt no more, and s1 is handed the Retry's copy alone.
func TestARetryThatIsSentSupersedesTheAttemptItOvertook(t *testing.T) {
	lane := aMentionQueuedBehindAStalledSend(t, "")
	published := &recordingOutboxPublisher{}
	runOutboxBeside(t, lane.deps, published)
	if retried := retryTheQueuedMention(t, lane); retried.Attempt != 2 || retried.State != "sent" {
		t.Fatalf("the Retry = %#v, want an attempt 2 of its own, sent", retried)
	}

	lane.release()
	settled := waitForCommentDeliveries(t, lane.database, lane.queued, "both attempts settled", bothMentionAttemptsSettled)
	if settled[0].State != "failed" || settled[0].Error == nil || *settled[0].Error != mentionSupersededError {
		t.Fatalf("the attempt the Retry overtook = %#v, want it settled superseded: the Retry delivered "+
			"the comment", settled[0])
	}
	if !unclaimedSettled(t, lane.database, lane.queued, "session:s1", 1) {
		t.Fatal("the superseded attempt kept the claim its queued send held")
	}
	if owed, _, _, _ := mentionAttemptSplit(t, lane.database, lane.queued, "session:s1", 1); owed {
		t.Fatal("the superseded attempt reads as one the delivery sweep owes")
	}
	ageAttempts(t, lane.database, lane.queued)
	if err := lane.served.resumeOwedDeliveries(context.Background()); err != nil {
		t.Fatalf("resume the owed deliveries: %v", err)
	}
	if sends := sendsOfTheQueuedComment(lane.listener); sends != 1 {
		t.Fatalf("the listener took %d sends of the queued comment, want the Retry's alone", sends)
	}
	if copies := copiesOfTheComment(t, lane.database, published, lane.queued, "s1"); copies != 1 {
		t.Fatalf("s1 was handed %d copies of the comment, want the Retry's alone", copies)
	}
}

// aRetryStillInFlightWhenTheLaneArrives posts the two comments of a queued mention on an issue
// with no route, fires a Retry of the queued one whose own send stalls at the listener, and lets
// the lane reach the attempt that Retry overtook while it is still in flight. The lane can
// neither send that attempt - a second copy, in another mode, should the Retry land - nor settle
// it superseded - no copy at all should the Retry fail - so it stands down and leaves it pending
// and unclaimed. It answers the lane, the release of the Retry's send, and the Retry's answer.
func aRetryStillInFlightWhenTheLaneArrives(
	t *testing.T,
) (queuedMention, func(), <-chan *httptest.ResponseRecorder) {
	t.Helper()
	lane := aMentionQueuedBehindAStalledSend(t, "")
	reached, releaseRetry := lane.listener.stall("s1")
	t.Cleanup(releaseRetry)
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- dispatchRequest(t, lane.handler, http.MethodPost,
			"/api/v1/comments/"+lane.queued+"/deliveries", map[string]any{"delivery": "steer"}, "alice")
	}()
	<-reached

	lane.release()
	awaitUnclaimedPendingAttempt(t, lane.database, lane.queued, "session:s1")
	return lane, releaseRetry, answered
}

// A Retry still in flight when the lane reaches the attempt it overtook, which then fails: the
// lane stood down rather than settle that attempt, so it is pending and unclaimed, it stands once
// the Retry fails, and the delivery sweep sends it once it has waited a lease. s1 gets the
// comment, once.
func TestARetryInFlightThatFailsLeavesTheQueuedAttemptToTheSweep(t *testing.T) {
	lane, releaseRetry, answered := aRetryStillInFlightWhenTheLaneArrives(t)
	published := &recordingOutboxPublisher{}
	runOutboxBeside(t, lane.deps, published)

	lane.listener.refuseSends("s1")
	releaseRetry()
	if retried := decodeBody[commentDeliveryRead](t, <-answered); retried.Attempt != 2 || retried.State != "failed" {
		t.Fatalf("the Retry = %#v, want its own attempt 2, failed", retried)
	}
	lane.listener.allowSends("s1")
	ageAttempts(t, lane.database, lane.queued)
	owed, resumed, abandoned, _ := mentionAttemptSplit(t, lane.database, lane.queued, "session:s1", 1)
	if !owed || !resumed || abandoned {
		t.Fatalf("the attempt the lane stood down from reads owed=%v resumed=%v abandoned=%v, want the "+
			"sweep to owe it and the outbox to count it resumed: the Retry that overtook it failed", owed, resumed, abandoned)
	}

	if err := lane.served.resumeOwedDeliveries(context.Background()); err != nil {
		t.Fatalf("resume the owed deliveries: %v", err)
	}
	settled := waitForCommentDeliveries(t, lane.database, lane.queued, "both attempts settled", bothMentionAttemptsSettled)
	if settled[0].State != "sent" {
		t.Fatalf("the attempt the lane stood down from = %#v, want the sweep to have sent it", settled[0])
	}
	if copies := copiesOfTheComment(t, lane.database, published, lane.queued, "s1"); copies != 1 {
		t.Fatalf("s1 was handed %d copies of the comment, want the sweep's send alone", copies)
	}
}

// A Retry still in flight when the lane reaches the attempt it overtook, which is then sent: the
// Retry's own sender settles the attempt the lane stood down from superseded
// (supersedeOvertakenAttempts), so it is left neither pending nor sent, and s1 is handed one copy.
func TestARetryInFlightThatIsSentSupersedesTheAttemptTheLaneStoodDownFrom(t *testing.T) {
	lane, releaseRetry, answered := aRetryStillInFlightWhenTheLaneArrives(t)
	published := &recordingOutboxPublisher{}
	runOutboxBeside(t, lane.deps, published)

	releaseRetry()
	if retried := decodeBody[commentDeliveryRead](t, <-answered); retried.Attempt != 2 || retried.State != "sent" {
		t.Fatalf("the Retry = %#v, want its own attempt 2, sent", retried)
	}
	settled := waitForCommentDeliveries(t, lane.database, lane.queued, "both attempts settled", bothMentionAttemptsSettled)
	if settled[0].State != "failed" || settled[0].Error == nil || *settled[0].Error != mentionSupersededError {
		t.Fatalf("the attempt the lane stood down from = %#v, want the Retry's sender to have settled it "+
			"superseded once its send landed", settled[0])
	}
	ageAttempts(t, lane.database, lane.queued)
	if err := lane.served.resumeOwedDeliveries(context.Background()); err != nil {
		t.Fatalf("resume the owed deliveries: %v", err)
	}
	if sends := sendsOfTheQueuedComment(lane.listener); sends != 1 {
		t.Fatalf("the listener took %d sends of the queued comment, want the Retry's alone", sends)
	}
	if copies := copiesOfTheComment(t, lane.database, published, lane.queued, "s1"); copies != 1 {
		t.Fatalf("s1 was handed %d copies of the comment, want the Retry's alone", copies)
	}
}

// A keyed repeat puts a stranded attempt back on this process's lane, bound to its number. A
// Retry that overtakes it and fails leaves it standing, so the repeat's queued send delivers the
// comment when its lane reaches it: s1 gets the comment once, from the attempt its first request
// opened.
func TestAKeyedRepeatSendsTheAttemptAFailedRetryOvertook(t *testing.T) {
	listener, state := newMentionListener(t)
	handler, database, deps, served := newRegisteredTestServer(t, testServerOptions{
		envoyURL: listener.URL, envoyTimeout: 30 * time.Second,
	})
	// Only the refreshes this test makes itself run.
	withMentionClaimRefresh(t, served, time.Hour)
	published := &recordingOutboxPublisher{}
	runOutboxBeside(t, deps, published)
	issue := createInteractionIssue(t, handler, "TEST", "A repeated comment a Retry overtook", "before")
	reached, release := state.stall("s1")
	defer release()

	postMentionOfS1(t, handler, issue.Key, "first")
	<-reached
	keyed := map[string]any{"body": "second", "mentions": []map[string]any{{"target": "session:s1"}}}
	comments := "/api/v1/issues/" + issue.Key + "/comments"
	queued := decodeMentionedComment(t,
		keyedRequest(t, handler, http.MethodPost, comments, keyed, "alice", "mention-queued")).ID
	// The process that queued the send stopped writing its claim, so the repeat takes the attempt
	// over and queues it on this process's lane again.
	ageQueuedClaims(t, database, served.mentions, model.DeliveryClaimLapse+time.Second)
	repeat := keyedRequest(t, handler, http.MethodPost, comments, keyed, "alice", "mention-queued")
	if repeat.Code != http.StatusCreated || !replayed(repeat) {
		t.Fatalf("the repeat: status=%d body=%s, want the comment replayed", repeat.Code, repeat.Body.String())
	}
	if !claimedPending(t, database, queued, "session:s1") {
		t.Fatal("the repeat left the queued attempt unclaimed")
	}

	state.refuseSends("s1")
	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+queued+"/deliveries",
		map[string]any{"delivery": "steer"}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry the queued mention: status=%d body=%s", retry.Code, retry.Body.String())
	}
	if retried := decodeBody[commentDeliveryRead](t, retry); retried.Attempt != 2 || retried.State != "failed" {
		t.Fatalf("the Retry = %#v, want its own attempt 2, failed", retried)
	}
	state.allowSends("s1")

	release()
	settled := waitForCommentDeliveries(t, database, queued, "both attempts settled", bothMentionAttemptsSettled)
	if settled[0].State != "sent" {
		t.Fatalf("the attempt the repeat queued = %#v, want its lane to have sent it: the Retry that "+
			"overtook it delivered nothing", settled[0])
	}
	if copies := copiesOfTheComment(t, database, published, queued, "s1"); copies != 1 {
		t.Fatalf("s1 was handed %d copies of the comment, want the repeat's send alone", copies)
	}
}

// A sender that dies after a Retry failed leaves the attempt it queued pending under a claim
// nothing writes again. That attempt stands - the Retry delivered nothing - so the outbox counts
// it resumed rather than abandoned, and the delivery sweep owes and sends it once its claim has
// lapsed: s1 gets the comment once, a lease after the kill.
func TestTheSweepSendsTheAttemptAFailedRetryOvertookOnceItsClaimLapses(t *testing.T) {
	lane := aMentionQueuedBehindAStalledSend(t, "")
	published := &recordingOutboxPublisher{}
	runOutboxBeside(t, lane.deps, published)
	lane.listener.refuseSends("s1")
	if retried := retryTheQueuedMention(t, lane); retried.Attempt != 2 || retried.State != "failed" {
		t.Fatalf("the Retry = %#v, want its own attempt 2, failed", retried)
	}
	lane.listener.allowSends("s1")

	// The process holding the queued send is gone: its attempt is a lease old and nothing has
	// written its claim since.
	ageAttempts(t, lane.database, lane.queued)
	ageQueuedClaims(t, lane.database, lane.served.mentions, model.DeliveryClaimLapse+time.Second)
	owed, resumed, abandoned, live := mentionAttemptSplit(t, lane.database, lane.queued, "session:s1", 1)
	if !owed || !resumed || abandoned || live {
		t.Fatalf("the attempt the failed Retry overtook reads owed=%v resumed=%v abandoned=%v live=%v, want "+
			"the sweep to owe it and the outbox to count it resumed", owed, resumed, abandoned, live)
	}

	if err := lane.served.resumeOwedDeliveries(context.Background()); err != nil {
		t.Fatalf("resume the owed deliveries: %v", err)
	}
	settled := waitForCommentDeliveries(t, lane.database, lane.queued, "both attempts settled", bothMentionAttemptsSettled)
	if settled[0].State != "sent" {
		t.Fatalf("the attempt the failed Retry overtook = %#v, want the sweep to have sent it", settled[0])
	}
	if copies := copiesOfTheComment(t, lane.database, published, lane.queued, "s1"); copies != 1 {
		t.Fatalf("s1 was handed %d copies of the comment, want the sweep's send alone", copies)
	}
}

// framesOfTheQueuedComment is what the listener took of the comment queued behind the stalled
// send: how many sends of it reached s1, the distinct idempotency keys those sends went under,
// and how many of them s1 was actually handed - a send answered as a repeat of a key the
// recipient's host already holds hands it nothing (dropRepeats).
func framesOfTheQueuedComment(listener *mentionListener) (sends int, keys []string, handedOver int) {
	for _, send := range listener.sent() {
		if !send.is("s1", "second") {
			continue
		}
		sends++
		if !slices.Contains(keys, send.key) {
			keys = append(keys, send.key)
		}
		if !send.duplicate {
			handedOver++
		}
	}
	return sends, keys, handedOver
}

// Round 8 of the review, the reviewer's reproduction. A Retry in ANOTHER mode whose frame reached
// s1 but whose receipt missed the send window settles failed, saying the message may already have
// been delivered. The attempt it overtook therefore stands (model.MentionAttemptStandsSQL) and is
// sent after it, in its own pinned mode - and that is only safe because every attempt of one
// mention goes under the mention's one idempotency key
// (model.CommentMentionIdempotencyKey), whatever its mode: s1's host recognises the second frame
// as the repeat it is and hands s1 nothing. With the mode in the key the two frames went under
// different keys and s1 was handed the comment twice.
func TestARetryInAnotherModeThatTimedOutLeavesTheRecipientOneCopy(t *testing.T) {
	const sendWindow = 2 * time.Second
	lane := aMentionQueuedWithinASendWindow(t, "", sendWindow)
	lane.listener.dropRepeats()
	// The Retry's frame reaches s1 and its answer is held past the send window.
	landed, answerRetry := lane.listener.holdAnswer("s1")
	t.Cleanup(answerRetry)
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- dispatchRequest(t, lane.handler, http.MethodPost,
			"/api/v1/comments/"+lane.queued+"/deliveries", map[string]any{"delivery": "btw"}, "alice")
	}()
	<-landed

	// The lane reaches the attempt that Retry overtook while it is still in flight, so it stands
	// down and leaves it pending and unclaimed.
	lane.release()
	awaitUnclaimedPendingAttempt(t, lane.database, lane.queued, "session:s1")
	retried := decodeBody[commentDeliveryRead](t, <-answered)
	if retried.Attempt != 2 || retried.Delivery != "btw" || retried.State != "failed" ||
		deref(retried.Error) != receiptTimeoutText {
		t.Fatalf("the Retry = %#v, want its own attempt 2 in btw, failed with the receipt timeout %q",
			retried, receiptTimeoutText)
	}
	answerRetry()

	// Nothing else can send that attempt now: its lane is gone and the Retry failed, so the
	// delivery sweep owes it.
	ageAttempts(t, lane.database, lane.queued)
	if err := lane.served.resumeOwedDeliveries(context.Background()); err != nil {
		t.Fatalf("resume the owed deliveries: %v", err)
	}
	settled := waitForCommentDeliveries(t, lane.database, lane.queued, "both attempts settled", bothMentionAttemptsSettled)
	if settled[0].State != "sent" || settled[0].Delivery != "steer" || !settled[0].Duplicate {
		t.Fatalf("the attempt the Retry overtook = %#v, want the sweep to have sent it in its own mode "+
			"and s1's host to have dropped it as the repeat it is", settled[0])
	}
	sends, keys, handedOver := framesOfTheQueuedComment(lane.listener)
	wantKey := model.CommentMentionIdempotencyKey(lane.queued, "session:s1")
	if sends != 2 || !slices.Equal(keys, []string{wantKey}) {
		t.Fatalf("the listener took %d sends of the queued comment under keys %v, want the Retry's and "+
			"the sweep's, both under the mention's key %q", sends, keys, wantKey)
	}
	if handedOver != 1 {
		t.Fatalf("s1 was handed %d frames of the queued comment, want the Retry's alone: the second "+
			"frame repeats its key and is dropped", handedOver)
	}
}

// Thread 2 of round 8. A lane that stood down leaves its attempt pending and claim-free, and
// nothing binds to it again: the sweep sends only the attempts that stand, and a keyed repeat
// requeues only a mention with a single attempt. So when the Retry that overtook it becomes sent
// without its own sender settling anything - here s1 answers the frame whose send had already
// failed, and the reply turns that attempt sent (replyComment) - no supersede runs at all and the
// attempt would stay pending for good. The delivery sweep settles it superseded instead, as it
// settles the owed attempts nothing can send.
func TestTheSweepSettlesAnOvertakenAttemptNoSenderSettled(t *testing.T) {
	lane, releaseRetry, answered := aRetryStillInFlightWhenTheLaneArrives(t)
	lane.listener.refuseSends("s1")
	releaseRetry()
	if retried := decodeBody[commentDeliveryRead](t, <-answered); retried.Attempt != 2 || retried.State != "failed" {
		t.Fatalf("the Retry = %#v, want its own attempt 2, failed: s1's host refused its send", retried)
	}
	lane.listener.allowSends("s1")
	// s1 answers the frame all the same, which records the reply on attempt 2 and turns it sent.
	reply := bearerRequest(t, lane.handler, http.MethodPost, "/api/v1/comments/"+lane.queued+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 2,
		"target": "session:s1", "body": "Answered after its send was recorded failed.",
	})
	if reply.Code != http.StatusCreated {
		t.Fatalf("s1's reply to the Retry: status=%d body=%s", reply.Code, reply.Body.String())
	}
	answeredAttempts := waitForCommentDeliveries(t, lane.database, lane.queued, "the Retry's attempt sent",
		func(attempts []model.CommentDelivery) bool {
			return len(attempts) == 2 && attempts[1].State == "sent"
		})
	if answeredAttempts[0].State != "pending" {
		t.Fatalf("the attempt the lane stood down from = %#v, want it still pending: the reply settled "+
			"the Retry's attempt without superseding anything", answeredAttempts[0])
	}

	if err := lane.served.resumeOwedDeliveries(context.Background()); err != nil {
		t.Fatalf("resume the owed deliveries: %v", err)
	}
	settled := waitForCommentDeliveries(t, lane.database, lane.queued, "both attempts settled", bothMentionAttemptsSettled)
	if settled[0].State != "failed" || settled[0].Error == nil || *settled[0].Error != mentionSupersededError {
		t.Fatalf("the attempt the lane stood down from = %#v, want the sweep to have settled it "+
			"superseded: the attempt that overtook it delivered the comment", settled[0])
	}
	if !unclaimedSettled(t, lane.database, lane.queued, "session:s1", 1) {
		t.Fatal("the attempt the sweep superseded kept a claim")
	}
	if sends := sendsOfTheQueuedComment(lane.listener); sends != 1 {
		t.Fatalf("the listener took %d sends of the queued comment, want the Retry's alone: a superseded "+
			"attempt is settled, never sent", sends)
	}
}
