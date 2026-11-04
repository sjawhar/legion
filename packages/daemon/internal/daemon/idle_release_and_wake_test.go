package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The issues of the rig, one pod each. Each subtest drives the pods it names and no other, so one
// release wave readies them all and no subtest waits for another's.
const (
	idleRigDelivery   = "delivery"
	idleRigChecks     = "checks"
	idleRigRoleWake   = "role-message"
	idleRigAgentWake  = "agent-message"
	idleRigBackstop   = "backstop"
	idleRigKeepNone   = "keeps-nothing"
	idleRigSubscribed = "subscribed-topic"
	idleRigStuck      = "stuck-resume"
	idleRigRestart    = "daemon-restart"
)

// idleRigSubscribedTopic is the topic the subscribed pod's session takes with envoy_subscribe: a
// notification topic of its own, which is neither a role topic nor an agent subject.
const idleRigSubscribedTopic = "notifications.idlerig.watched"

// idleRigSender is the session every message of this test is sent from.
const idleRigSender = "idle-rig-sender"

// TestIdleReleaseAndWake is INFRA-945's A3 acceptance rig: the Go daemon on a real Postgres, a
// real nats-server, a real Envoy listener and the Sandbox runtime on a fake Kubernetes API that
// starts a real `legion launcher` for each pod it reports Ready, whose role processes are
// scripted stand-ins for Oh My Pi that register, claim their role and record each message they
// get. idle_rig_test.go says where each part comes from.
//
// Nine pods are launched and made quiet, and one pass of the daemon's own idle tick releases them
// all: each live claim suspended with the source `released`, and the pod, its disk and its
// Sandbox deleted. Each subtest then drives its own pods' wakes.
func TestIdleReleaseAndWake(t *testing.T) {
	rig := newIdleWakeRig(t,
		idleRigDelivery, idleRigChecks, idleRigRoleWake, idleRigAgentWake, idleRigBackstop,
		idleRigKeepNone, idleRigSubscribed, idleRigStuck, idleRigRestart)

	// The subscribed pod's session takes a topic of its own, as `envoy_subscribe` does, before
	// its process starts. The stuck pod's plan is left alone until its own subtest: a session
	// that acknowledges a task and takes no turn has a turn due, which is a pod the rule keeps.
	subscribed := rig.issue(idleRigSubscribed)
	rig.agents.setPlan(subscribed.key, string(subscribed.role), idleRigPlan{TakeTurn: true, Topics: []string{idleRigSubscribedTopic}})

	rig.launchAll()
	rig.quiet()
	rig.awaitReleased(rig.issues...)
	// What each role was given before its pod was released — the phase task the daemon's boot
	// handed it — is what every count below is taken from.
	rig.markPrompts()

	t.Run("AReleasedPodComesBackOnEachOfTheFiveWakes", func(t *testing.T) {
		idleRigWakeAll(t, rig)
	})

	// This reads what the pass above drove: the five wakes happen once, and this counts what each
	// woken role was given for them. The pass above waited for every one of these tasks, so a
	// count of none here is a task that never came rather than one still on its way.
	t.Run("EachWokenRoleGetsItsTaskOnceNamingTheWake", func(t *testing.T) {
		for _, wake := range idleRigWakes(rig) {
			prompts := rig.newPrompts(wake.issue)
			if len(prompts) != 1 {
				t.Errorf("%s's woken role was given %d tasks, want exactly one: %v", wake.issue.name, len(prompts), idleRigTexts(prompts))
				continue
			}
			if !strings.Contains(prompts[0].Text, wake.task) {
				t.Errorf("%s's woken role was given %q, want a task naming the wake with %q", wake.issue.name, prompts[0].Text, wake.task)
			}
			if wake.issue.name != idleRigDelivery && !strings.Contains(prompts[0].Text, idleRigWakePreamble) {
				t.Errorf("%s's task does not say its pod was released and brought back: %q", wake.issue.name, prompts[0].Text)
			}
		}
	})

	t.Run("ARoleMessageThatKeepsNothingStillWakesTheRole", func(t *testing.T) {
		issue := rig.issue(idleRigKeepNone)
		token := string(rig.token(issue))
		// A role publish has two answers now that the listener keeps a role message for the
		// role's next holder (sjawhar/legion#2588): kept, which the five-wake pass's own role
		// message above takes, and this one. `keep: false` is the field a sender that holds a
		// queue of its own sets (notify.Publish), it keeps nothing, and it is answered 404 as
		// every role publish was before the kept lane. It is the `keep: false` case R3 item 3
		// names, and the one where only the wake can bring the role back: nothing waits on the
		// role's kept lane, so the sender's next attempt is the only way this message ever
		// reaches a session.
		status, body := rig.listener.publish(map[string]any{
			"topic": notify.RoleTopicPrefix + token, "message": "keeps nothing", "keep": false,
			"source": "agent", "source_session": idleRigSender, "idempotency_key": "keep-none-1",
		})
		idleRigKeptNothing(t, status, body, idleRigRoleUnclaimed)
		rig.awaitResumed(issue, 2)
		rig.awaitTask(issue, "The wake is a message somebody published to your role topic ("+token+")")
		if prompts := rig.newPrompts(issue); len(prompts) != 1 {
			t.Fatalf("the woken role was given %v, want exactly one task", idleRigTexts(prompts))
		}
		// Nothing was kept, so the woken session is given nothing until the sender tries again;
		// its next attempt reaches it, once.
		if messages := rig.agents.messages(issue.key, string(issue.role)); len(messages) != 0 {
			t.Errorf("the woken session was given %v, want nothing: the publish kept nothing", idleRigTexts(messages))
		}
		idleRigRetryPublish(t, rig, issue, "keeps nothing", "keep-none-1")
	})

	t.Run("AnEventOnASubscribedTopicDoesNotWakeThePod", func(t *testing.T) {
		issue := rig.issue(idleRigSubscribed)
		token := rig.token(issue)
		before := rig.pods.starts(issue.pod)
		status, body := rig.listener.publish(map[string]any{
			"topic": idleRigSubscribedTopic, "message": "an event on a topic the session took",
			"source": "agent", "source_session": idleRigSender, "idempotency_key": "subscribed-1",
		})
		if status != http.StatusOK && status != http.StatusAccepted {
			t.Fatalf("the topic publish answered %d %s, want it taken", status, body)
		}
		// The listener reports a wake only for a role topic nobody holds and for an agent
		// subject nothing can place; a topic a session took with envoy_subscribe is neither, so
		// the pod stays released and its role waits for the backstop instead.
		idleRigSettle(t, 8*time.Second, func() {
			state := rig.api.claim(token)
			if state.State != string(supervise.StateSuspended) || state.SuspendedBy != string(supervise.SuspendedByReleased) {
				t.Fatalf("%s is %s (suspended by %q) after an event on its subscribed topic, want it still released",
					issue.key, state.State, state.SuspendedBy)
			}
			if rig.kube.pod(issue.pod) {
				t.Fatalf("a pod came back for %s after an event on its subscribed topic", issue.key)
			}
		})
		if started := rig.pods.starts(issue.pod); started != before {
			t.Errorf("%s's pod started %d times, want the %d it had: a subscribed topic wakes nothing", issue.key, started, before)
		}
		for _, resume := range rig.releaseRecords(rig.issue(idleRigDelivery)).Resumes {
			if resume.Issue == issue.key {
				t.Errorf("a resume of %s was recorded (%s): a subscribed topic wakes nothing", issue.key, resume.Kind)
			}
		}
	})

	t.Run("AResumeWithNoFirstTurnAfterTenMinutesIsWarnedAndRecordedStuck", func(t *testing.T) {
		issue := rig.issue(idleRigStuck)
		// From here its session acknowledges the task its wake gives it and takes no turn.
		rig.agents.setPlan(issue.key, string(issue.role), idleRigPlan{TakeTurn: false})
		// A role message, because R5's resume is what the daemon records for a wake it answered
		// (store.RecordPodWakeTx); a delivery the daemon already held is its own account of why
		// the pod came back and records none. This sender sets no keep flag, so the listener
		// keeps the message for the role's next holder and answers 202 kept, and raises the wake
		// report beside that answer as it does beside the 404 (roleSend.noHolder).
		status, body := rig.listener.publish(map[string]any{
			"topic": notify.RoleTopicPrefix + string(rig.token(issue)), "message": "take up your phase",
			"source": "agent", "source_session": idleRigSender, "idempotency_key": "stuck-1",
		})
		idleRigKept(t, status, body, idleRigRoleUnclaimed)
		rig.awaitResumed(issue, 2)
		idleRigEventually(t, 60*time.Second, "the resume of "+issue.key+" recorded", func() bool {
			return len(rig.resumesOf(issue)) == 1
		})
		// The clock the stuck pass reads is the wake's own recorded moment against the daemon's
		// now (warnStuckResumes), so the test moves the wake ten minutes into the past. The role
		// process acknowledged its task and started no turn, so no first turn was ever stamped.
		rig.ageResume(issue, 11*time.Minute)
		idleRigEventually(t, 90*time.Second, "the warning about the resume with no first turn", func() bool {
			return rig.logged("wake: a released pod's resume has had no first turn since its wake",
				map[string]string{"issue": issue.key, "role": string(issue.role)}) == 1
		})
		idleRigEventually(t, 30*time.Second, "the stuck resume on the record", func() bool {
			for _, resume := range rig.resumesOf(issue) {
				if resume.Stuck {
					return true
				}
			}
			return false
		})
		for _, resume := range rig.resumesOf(issue) {
			if resume.FirstTurnAt != "" {
				t.Errorf("the resume of %s records a first turn at %s, want none: its role took no turn", issue.key, resume.FirstTurnAt)
			}
		}
		if warned := rig.logged("wake: a released pod's resume has had no first turn since its wake",
			map[string]string{"issue": issue.key}); warned != 1 {
			t.Errorf("the stuck resume of %s was warned about %d times, want once", issue.key, warned)
		}
	})

	// Last, because it restarts the daemon: the worker stream binds a port of its own on every
	// boot, so the launchers of the pods the earlier subtests woke are left dialling the old one.
	t.Run("AMessageSentDuringTheReleaseReachesTheWokenSessionOnceAcrossARestart", func(t *testing.T) {
		// First the two the daemon was up for: the role message and the direct message of the
		// five-wake pass. Each sender's next attempt reaches the woken session exactly once.
		idleRigRetryPublish(t, rig, rig.issue(idleRigRoleWake), "the role message the sender tried again", "role-retry-1")
		idleRigRetrySend(t, rig, rig.issue(idleRigAgentWake), "the direct message the sender tried again", "agent-retry-1")

		// Then the same two across a restart, on a pod still released: the listener reports both
		// wakes while the daemon is down, its durable consumer holds them, and the daemon acts on
		// them once it is back.
		issue := rig.issue(idleRigRestart)
		token := string(rig.token(issue))
		session := rig.session(issue)
		rig.stopDaemon()
		status, body := rig.listener.publish(map[string]any{
			"topic": notify.RoleTopicPrefix + token, "message": "sent while the daemon was down",
			"source": "agent", "source_session": idleRigSender, "idempotency_key": "restart-role-1",
		})
		idleRigKept(t, status, body, idleRigRoleUnclaimed)
		if status, body := rig.listener.send(map[string]any{
			"target_session": session, "message": "sent to the session while the daemon was down",
			"source": "agent", "source_session": idleRigSender, "idempotency_key": "restart-agent-1",
		}); status != http.StatusNotFound {
			t.Fatalf("the send answered %d %s, want 404", status, body)
		}
		rig.restart()
		rig.awaitResumed(issue, 2)
		// The two wakes the listener reported while the daemon was down reach it on its durable
		// consumer once it is back, and the first of them resumes the claim: a wake acts only on
		// a claim that is still released (workflow's wakeReleased), so the pod comes back once
		// and its role is given exactly one task, naming the wake that woke it.
		// The resume is the claim's; the task reaches its agent once the replacement pod is up and
		// the delivery's own retry has found it, which is a few seconds later. Either wake may be
		// the one that took it, so this waits for the task rather than for one wake's wording.
		idleRigEventually(t, 60*time.Second, "the task the wake gave "+issue.key+"'s woken role", func() bool {
			return len(rig.newPrompts(issue)) > 0
		})
		prompts := rig.newPrompts(issue)
		if len(prompts) != 1 {
			t.Fatalf("the woken role of %s was given %d tasks after the restart, want exactly one: %v",
				issue.key, len(prompts), idleRigTexts(prompts))
		}
		role := "The wake is a message somebody published to your role topic (" + token + ")"
		agent := "The wake is a message somebody sent to your session (" + session + ")"
		if !strings.Contains(prompts[0].Text, role) && !strings.Contains(prompts[0].Text, agent) {
			t.Errorf("the woken role of %s was given %q, want a task naming the role message or the direct message",
				issue.key, prompts[0].Text)
		}
		if !strings.Contains(prompts[0].Text, idleRigWakePreamble) {
			t.Errorf("%s's task does not say its pod was released and brought back: %q", issue.key, prompts[0].Text)
		}
		// The role message was kept for the role's next holder, so the claim the wake's own pod
		// makes is what hands it over (drainKeptRole) and it reaches the woken session once:
		// nothing but this pod coming back claims that role, so the kept copy moves no sooner
		// than the wake. The direct message was kept by nothing — the kept lane is a role's
		// alone — so its sender's next attempt is what reaches the woken session, once.
		idleRigDeliveredOnce(t, rig, issue, "sent while the daemon was down")
		idleRigRetrySend(t, rig, issue, "sent to the session while the daemon was down", "restart-agent-1")
	})

	rig.kube.requireServed(t)
}

// idleRigWakePreamble is the sentence every wake's task opens with (workflow.WakeTask).
var idleRigWakePreamble = workflow.WakeTask("", "")[:len("Your issue's pod had been released while it was idle, and this wake brought it back.")]

// idleRigDeliveredTask is the task the daemon has for the role of the delivery pod, which is R3
// item 1's wake: the delivery is its own account of why the pod came back.
const idleRigDeliveredTask = "the daemon had this task for you while your pod was gone"

// idleRigWake is one of the five wakes of R3: the issue whose pod it brings back, and the words
// the task it gives its woken role names that wake with.
type idleRigWake struct {
	issue idleRigIssue
	task  string
}

// idleRigWakes is the five wakes with the task each gives, written once: the pass that drives
// them waits for each of these tasks and the subtest that counts them reads the same words, so
// neither can assert on a wording the other does not wait for. The role token and the session id
// are the released process's, and a relaunched process registers the same of both (the session
// is derived from the pod and the role, idleRigRoleProcess.register), so this reads the same
// before a wake and after it.
func idleRigWakes(rig *idleWakeRig) []idleRigWake {
	checks, role, agent := rig.issue(idleRigChecks), rig.issue(idleRigRoleWake), rig.issue(idleRigAgentWake)
	return []idleRigWake{
		{rig.issue(idleRigDelivery), idleRigDeliveredTask},
		{checks, "The wake is a CI settlement on your pull request's head: " + idleRigHead(checks.key)[:12] + " passed every check"},
		{role, "The wake is a message somebody published to your role topic (" + string(rig.token(role)) + ")"},
		{agent, "The wake is a message somebody sent to your session (" + rig.session(agent) + ")"},
		{rig.issue(idleRigBackstop), "The wake is the release's backstop running out"},
	}
}

// idleRigWakeAll drives the five wakes of R3, once each, and waits for each pod to come back and
// for the task its wake gives it.
func idleRigWakeAll(t *testing.T, rig *idleWakeRig) {
	t.Helper()
	// 1. A delivery the daemon has for a role of the pod. It reaches the released claim through
	//    deliverResuming, the same path every workflow start takes to a suspended claim.
	delivery := rig.issue(idleRigDelivery)
	rig.deliver(delivery, idleRigDeliveredTask)

	// 2. A CI settlement the daemon records for the head of the issue's pull request, while the
	//    phase worker's phase is open.
	checks := rig.issue(idleRigChecks)
	rig.publishSettlement(checks)

	// 3. A message published to a role topic of the pod, from a sender that sets no keep flag:
	//    nobody holds the role, so the listener keeps the message for the role's next holder and
	//    answers the sender 202 kept (sjawhar/legion#2588), raising the wake report beside that
	//    answer. The kept copy waits for a claim of the role, which nothing but this pod coming
	//    back makes, so the wake is still the only thing that moves this message on.
	role := rig.issue(idleRigRoleWake)
	status, body := rig.listener.publish(map[string]any{
		"topic": notify.RoleTopicPrefix + string(rig.token(role)), "message": "the role message, again",
		"source": "agent", "source_session": idleRigSender, "idempotency_key": "role-wake-1",
	})
	idleRigKept(t, status, body, idleRigRoleUnclaimed)

	// 4. A message sent to the agent subject of a released session.
	agent := rig.issue(idleRigAgentWake)
	if status, body := rig.listener.send(map[string]any{
		"target_session": rig.session(agent), "message": "the direct message, again",
		"source": "agent", "source_session": idleRigSender, "idempotency_key": "agent-wake-1",
	}); status != http.StatusNotFound {
		t.Fatalf("the send answered %d %s, want 404: the session is gone with its pod", status, body)
	}

	// 5. The backstop, which needs nothing sent: it was armed by the release itself, and the
	//    test brings its due time forward rather than waiting the period out.
	backstop := rig.issue(idleRigBackstop)
	rig.dueBackstop(backstop)
	for _, issue := range []idleRigIssue{delivery, checks, role, agent, backstop} {
		rig.awaitResumed(issue, 2)
	}
	// The claim's resume and the task are two steps of one outbox `start` row, in that order: the
	// row resumes the claim, waits out the relaunch, and only then delivers the row's task
	// (outbox's start executor). A wake's task therefore reaches its woken role strictly after
	// the resume above, so every wake waits for its own task as well.
	for _, wake := range idleRigWakes(rig) {
		rig.awaitTask(wake.issue, wake.task)
	}
	for _, issue := range []idleRigIssue{delivery, checks, role, agent, backstop} {
		// A second pod was made for the wake. Whether one stands now is not asserted: a woken pod
		// goes quiet again within the rig's one-minute idle period and is released afresh, which
		// is the rule working, not a wake that failed.
		if started := rig.pods.starts(issue.pod); started < 2 {
			t.Errorf("%s's pod was started %d times, want a second pod for the wake", issue.key, started)
		}
	}
	// Each wake is recorded against the release it ended, with the kind that woke it.
	kinds := map[string]string{
		delivery.key: "",
		checks.key:   pods.WakeChecks,
		role.key:     pods.WakeRoleMessage,
		agent.key:    pods.WakeAgentMessage,
		backstop.key: pods.WakeBackstop,
	}
	recorded := map[string]string{}
	for _, resume := range rig.releaseRecords(delivery).Resumes {
		if want, ours := kinds[resume.Issue]; ours && want != "" {
			recorded[resume.Issue] = resume.Kind
		}
	}
	for issue, want := range kinds {
		if want == "" {
			continue
		}
		if got := recorded[issue]; got != want {
			t.Errorf("the resume of %s is recorded as %q, want %q", issue, got, want)
		}
	}
}

// idleRigRoleUnclaimed is the holder state a role of a released pod resolves to: the role
// process drops its claim as it stops (idleRigRoleProcess.unregister, whose DELETE
// /v1/interests releases every role the session held), so nobody claims the role at all. It is
// the word both answers of a role publish carry for it — the 202's kept_reason and the 404's
// reason (cmd/listener's roleHolderState).
const idleRigRoleUnclaimed = "unclaimed"

// idleRigRoleAnswer is what a role publish's answer says about the keep: the 202's own kept and
// kept_reason (cmd/listener's publishResponse) and the 404's reason (its roleHolderError),
// which name the same holder states in the same words.
type idleRigRoleAnswer struct {
	Kept       bool   `json:"kept"`
	KeptReason string `json:"kept_reason"`
	Reason     string `json:"reason"`
}

func idleRigRoleAnswerOf(t *testing.T, body string) idleRigRoleAnswer {
	t.Helper()
	var answer idleRigRoleAnswer
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("decode the role publish's answer %q: %v", body, err)
	}
	return answer
}

// idleRigKept is the answer a role publish gets when the listener kept the message for the
// role's next holder: 202, kept true, and kept_reason naming the state the resolution found.
// Every field is read, so a 202 that kept nothing cannot pass for one that kept the message.
func idleRigKept(t *testing.T, status int, body, reason string) {
	t.Helper()
	if answer := idleRigRoleAnswerOf(t, body); status != http.StatusAccepted || !answer.Kept || answer.KeptReason != reason {
		t.Fatalf("the role publish answered %d %s, want 202 kept for %q: no live session holds the role, so the listener keeps the message for its next holder",
			status, body, reason)
	}
}

// idleRigKeptNothing is the answer a role publish gets when nothing was kept: the 404 whose
// reason names the state the resolution found, which is what a sender's own fallback runs on
// (notify.ErrNoHolder). The absent kept flag is read too, so a publish the listener kept after
// all cannot pass for one that kept nothing.
func idleRigKeptNothing(t *testing.T, status int, body, reason string) {
	t.Helper()
	if answer := idleRigRoleAnswerOf(t, body); status != http.StatusNotFound || answer.Kept || answer.Reason != reason {
		t.Fatalf("the role publish answered %d %s, want 404 %s with nothing kept: no live session holds the role",
			status, body, reason)
	}
}

// idleRigRetryPublish is the sender's next attempt at a role message, which the wake exists to
// make reach a live session: the sender repeats while no live session holds the role — each such
// attempt is itself a wake — and the one attempt the listener accepts reaches the woken session
// exactly once.
//
// It publishes keep: false, the field the one sender that retries from a queue of its own sets
// (the Legion daemon's notices, notify.Publish). Nothing is kept for the role, so the attempt a
// live holder took is the only way this message can reach the session and the count below reads
// that attempt and nothing else; a sender that lets the listener keep the message is the
// five-wake pass's role message and the restart's, whose kept copies are counted where they are
// handed over.
func idleRigRetryPublish(t *testing.T, rig *idleWakeRig, issue idleRigIssue, message, key string) {
	t.Helper()
	publish := func() int {
		status, _ := rig.listener.publish(map[string]any{
			"topic": notify.RoleTopicPrefix + string(rig.token(issue)), "message": message, "keep": false,
			"source": "agent", "source_session": idleRigSender, "idempotency_key": key,
		})
		return status
	}
	idleRigUntil(t, 150*time.Second, "a role message reaching the live holder of "+issue.key, func() bool {
		return publish() == http.StatusOK
	})
	idleRigDeliveredOnce(t, rig, issue, message)
}

// idleRigRetrySend is the same for a direct message to the session's agent subject.
func idleRigRetrySend(t *testing.T, rig *idleWakeRig, issue idleRigIssue, message, key string) {
	t.Helper()
	send := func() int {
		status, _ := rig.listener.send(map[string]any{
			"target_session": rig.session(issue), "message": message,
			"source": "agent", "source_session": idleRigSender, "idempotency_key": key,
		})
		return status
	}
	idleRigUntil(t, 150*time.Second, "a direct message reaching the live session of "+issue.key, func() bool {
		return send() == http.StatusOK
	})
	idleRigDeliveredOnce(t, rig, issue, message)
}

// idleRigDeliveredOnce holds the once-ness R3 items 3 and 4 ask of a sender whose message the
// wake brought a session back for: the one attempt the listener accepted reaches the woken
// session exactly once, and stays at one.
func idleRigDeliveredOnce(t *testing.T, rig *idleWakeRig, issue idleRigIssue, message string) {
	t.Helper()
	idleRigEventually(t, 60*time.Second, "the message reaching the woken session of "+issue.key, func() bool {
		return idleRigCount(rig.agents.messages(issue.key, string(issue.role)), message) >= 1
	})
	idleRigSettle(t, 6*time.Second, func() {
		if got := idleRigCount(rig.agents.messages(issue.key, string(issue.role)), message); got != 1 {
			t.Fatalf("the woken session of %s recorded the message %d times for the one attempt the listener accepted, want once: %v",
				issue.key, got, idleRigTexts(rig.agents.messages(issue.key, string(issue.role))))
		}
	})
}

// idleRigUntil is idleRigEventually at a sender's pace: a second between attempts, so a retry
// loop that is itself a wake does not send a hundred of them a second.
func idleRigUntil(t *testing.T, within time.Duration, what string, attempt func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if attempt() {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// awaitTask waits until the issue's woken role has been given a task carrying want.
func (r *idleWakeRig) awaitTask(issue idleRigIssue, want string) {
	r.t.Helper()
	idleRigEventually(r.t, 60*time.Second, "the woken role of "+issue.key+" given its wake's task", func() bool {
		return idleRigCount(r.newPrompts(issue), want) >= 1
	})
}

// idleRigSettle runs check now and again for the whole of within, so a test that must see
// nothing happen watches for it rather than reading once and moving on.
func idleRigSettle(t *testing.T, within time.Duration, check func()) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		check()
		if t.Failed() || time.Now().After(deadline) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// idleRigTexts is what a failure prints of the messages a role recorded.
func idleRigTexts(messages []idleRigMessage) []string {
	texts := make([]string, 0, len(messages))
	for _, message := range messages {
		texts = append(texts, message.Text)
	}
	return texts
}

// deliver hands the issue's role a task the daemon holds for it, through the route that posts a
// RequestDeliver to the claim's machine — the same event every workflow start carries, and the
// one deliverResuming resumes a released claim for.
func (r *idleWakeRig) deliver(issue idleRigIssue, task string) {
	r.t.Helper()
	token := r.token(issue)
	status, body := r.api.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/deliver",
		api.DeliverRequest{Task: task}, true)
	if status != http.StatusOK && status != http.StatusAccepted {
		r.t.Fatalf("deliver to %s = %d %s", token, status, body)
	}
}

// session is the session id the issue's role process registered.
func (r *idleWakeRig) session(issue idleRigIssue) string {
	r.t.Helper()
	seen := r.agents.role(issue.key, string(issue.role))
	if seen.Session == "" {
		r.t.Fatalf("%s's role process has registered no session", issue.key)
	}
	return seen.Session
}

// resumesOf is every recorded resume of the issue.
func (r *idleWakeRig) resumesOf(issue idleRigIssue) []api.ResumeRecord {
	r.t.Helper()
	var mine []api.ResumeRecord
	for _, resume := range r.releaseRecords(issue).Resumes {
		if resume.Issue == issue.key {
			mine = append(mine, resume)
		}
	}
	return mine
}

// dueBackstop brings the issue's armed backstop due now, which is the clock the backstop pass
// reads: the row's due time against the daemon's own (workflowRuntime.fireDueBackstops).
func (r *idleWakeRig) dueBackstop(issue idleRigIssue) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(),
		`update pod_wake_backstops set due_at = now() - interval '1 minute' where project = $1 and issue = $2 and not fired`,
		r.project, issue.key); err != nil {
		r.t.Fatalf("bring the backstop of %s due: %v", issue.key, err)
	}
}

// ageResume moves the issue's recorded wakes into the past. The stuck pass reads the wake's own
// recorded moment against the daemon's clock and nothing else (warnStuckResumes), so this row is
// the clock the ten minutes of R5 is measured on.
func (r *idleWakeRig) ageResume(issue idleRigIssue, by time.Duration) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(),
		fmt.Sprintf(`update pod_resumes set at = at - interval '%d seconds' where project = $1 and issue = $2`, int(by.Seconds())),
		r.project, issue.key); err != nil {
		r.t.Fatalf("age the resumes of %s: %v", issue.key, err)
	}
}
