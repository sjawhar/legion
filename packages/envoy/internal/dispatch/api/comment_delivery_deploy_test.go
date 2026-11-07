package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Round 9 of the review, the reviewer's reproduction. A recipient's host remembers the key a
// frame went under for contracts.DeliveryDuplicateWindow - 72 hours, far longer than a deploy
// takes - so a mention the build before this one delivered must be resent under the key THAT
// build sent it under, or the deploy hands its session the comment a second time. That key is
// <comment>:<target>:<the mention's mode> (model.CommentMentionIdempotencyKey), and the attempts
// a stopped build leaves behind are resent by the two paths below: a stranded pending attempt the
// delivery sweep resends, and an attempt failed on a receipt timeout that a same-mode Retry
// resends.

// deployedMention is a mention the previous build already delivered, under a live process of this
// build that has yet to resend it: that process's handler and server, the database and listener
// both builds share, and the comment whose mention of s1 is owed.
type deployedMention struct {
	handler  http.Handler
	database *store.Store
	served   *server
	listener *mentionListener
	comment  string
}

// handPreviousBuildsFrame hands the stand-in listener the frame the build before this one sent:
// the listener takes it under key, and the recipient's host holds that key from then on
// (dropRepeats), exactly as it does for the send a process made before it was replaced.
func handPreviousBuildsFrame(t *testing.T, listenerURL, session, body, key string) {
	t.Helper()
	encoded, err := json.Marshal(map[string]string{
		"target_session": session, "source": "dispatch", "message": body, "idempotency_key": key,
	})
	if err != nil {
		t.Fatalf("encode the previous build's send: %v", err)
	}
	response, err := http.Post(listenerURL+"/v1/messages/send", "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("the previous build's send: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the previous build's send: status=%d, want 200", response.StatusCode)
	}
}

// aMentionThePreviousBuildDelivered posts a comment mentioning s1 from a process that is already
// stopping, so no send of its own runs and the test owns every frame the listener takes; hands
// the listener the frame the previous build's send handed it, under the key that build keyed a
// mention by; and lets leave put the attempt in the state that build left it in. It returns a
// live process of this build, over the same database and listener, for the resend: the deploy.
func aMentionThePreviousBuildDelivered(
	t *testing.T, body string, leave func(t *testing.T, database *store.Store, commentID string),
) deployedMention {
	t.Helper()
	listener, state := newMentionListener(t)
	state.dropRepeats()
	stopping, stop := context.WithCancel(t.Context())
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{
		envoyURL: listener.URL, lifetime: stopping,
	})
	issue := createInteractionIssue(t, handler, "TEST", "A mention across a deploy", "before")
	stop()
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": body, "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	if !unclaimedPending(t, database, comment.ID, "session:s1") {
		t.Fatal("the stopping process sent the mention itself, so the frame the previous build sent is not the test's")
	}
	handPreviousBuildsFrame(t, listener.URL, "s1", body, comment.ID+":session:s1:steer")
	if leave != nil {
		leave(t, database, comment.ID)
	}
	next, stopNext := context.WithCancel(t.Context())
	mux := http.NewServeMux()
	deployed := register(mux, next, deps)
	t.Cleanup(func() {
		stopNext()
		waitBackgroundOf(t, deployed)
	})
	return deployedMention{
		handler: mux, database: database, served: deployed, listener: state, comment: comment.ID,
	}
}

// A mention the previous build delivered is handed to its session once, however this build
// resends the attempt that build left behind: the resend goes under the key the frame that landed
// went under, so the recipient's host drops it and the attempt records a duplicate.
func TestAMentionThePreviousBuildDeliveredIsDroppedAsARepeatAfterTheDeploy(t *testing.T) {
	const body = "The build that sent this one is gone."
	for _, tc := range []struct {
		name   string
		leave  func(t *testing.T, database *store.Store, commentID string)
		resend func(t *testing.T, deployed deployedMention)
	}{
		{
			// A process killed between the send and the settle leaves the attempt pending and
			// claimed by nobody, which is what the stopping process above left.
			name: "the delivery sweep resends the stranded attempt",
			resend: func(t *testing.T, deployed deployedMention) {
				ageAttempts(t, deployed.database, deployed.comment)
				if err := deployed.served.resumeOwedDeliveries(context.Background()); err != nil {
					t.Fatalf("resume the owed deliveries: %v", err)
				}
			},
		},
		{
			name: "a same-mode Retry resends the attempt whose receipt timed out",
			leave: func(t *testing.T, database *store.Store, commentID string) {
				t.Helper()
				// The send landed and its answer missed the window: failed, and saying so
				// (receiptTimeoutText), which is the cause the dashboard offers a Retry for.
				if _, err := database.Pool.Exec(context.Background(), `
					update comment_deliveries set state = 'failed', error = $2, claimed_at = null
					where comment_id = $1 and target = 'session:s1' and attempt = 1
				`, commentID, receiptTimeoutText); err != nil {
					t.Fatalf("leave the attempt as a receipt timeout: %v", err)
				}
			},
			resend: func(t *testing.T, deployed deployedMention) {
				retry := dispatchRequest(t, deployed.handler, http.MethodPost,
					"/api/v1/comments/"+deployed.comment+"/deliveries", map[string]any{"delivery": "steer"}, "alice")
				if retry.Code != http.StatusCreated {
					t.Fatalf("retry the mention: status=%d body=%s", retry.Code, retry.Body.String())
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deployed := aMentionThePreviousBuildDelivered(t, body, tc.leave)

			tc.resend(t, deployed)

			sends, keys, handedOver := framesOf(deployed.listener, "s1", body)
			if handedOver != 1 {
				t.Fatalf("s1 was handed %d frames of the comment, want the previous build's alone: this "+
					"build's resend repeats its key and the host drops it", handedOver)
			}
			wantKey := model.CommentMentionIdempotencyKey(deployed.comment, "session:s1", "steer")
			if sends != 2 || !slices.Equal(keys, []string{wantKey}) {
				t.Fatalf("the listener took %d sends of the comment under keys %v, want the previous "+
					"build's and this build's resend, both under the mention's key %q", sends, keys, wantKey)
			}
			var state string
			var duplicate bool
			if err := deployed.database.Pool.QueryRow(context.Background(), `
				select state, duplicate from comment_deliveries
				where comment_id = $1 and target = 'session:s1' order by attempt desc limit 1
			`, deployed.comment).Scan(&state, &duplicate); err != nil {
				t.Fatalf("read the attempt this build sent: %v", err)
			}
			if state != "sent" || !duplicate {
				t.Fatalf("the attempt this build sent = %s/duplicate=%v, want it sent and recorded a "+
					"duplicate of the frame the previous build delivered", state, duplicate)
			}
		})
	}
}
