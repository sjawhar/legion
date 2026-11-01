// Package notify is the daemon's client for the Envoy listener: it publishes persisted workflow
// notices through its publish route, and ends a session's registration through its session route.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const requestTimeout = 10 * time.Second

// refusalBodyLimit bounds how much of a refused publish's body the error carries: the listener
// answers `{"error": ...}` (packages/envoy/cmd/listener/api.go writeJSONError).
const refusalBodyLimit = 4096

// ErrNoHolder is a role publish the listener refused because no live session holds the role: nobody
// claimed it, or its holder's registration lapsed past the gone horizon (writeRoleHolderError,
// packages/envoy/cmd/listener/api.go). Every listener release answers it, 404 with one of
// roleHolderReasons. It is the ordinary state before a role's session claims it, not a broken
// publish path, so a caller can hold what it published until one does.
var ErrNoHolder = errors.New("no live session holds the role")

// ErrHolderUnresponsive is a role publish the listener refused 404 `holder_lapsed` naming a holder
// whose `holder_state` is `unresponsive`: its last registration is past the listener's live window
// but still inside its gone horizon, so the session is not dead. It keeps its role claim and its
// issue claims, nothing is released for it and no replacement is started, and a box under load is
// the ordinary cause (dispatch://INFRA-392). The caller holds what it published and sends it
// again rather than acting on a death: the session answers for itself once its heartbeats land.
var ErrHolderUnresponsive = errors.New("the role's holder is unresponsive")

// roleHolderReasons are the reasons the listener's 404 names for a role with no live holder
// (roleHolderUnclaimed, roleHolderLapsed). A 404 without one of them is some other refusal — a route
// an older listener lacks — and never ErrNoHolder.
var roleHolderReasons = []string{"unclaimed", ReasonHolderLapsed}

// ReasonHolderLapsed is the listener's 404 reason for a role whose holder is past its live window,
// and StateUnresponsive the `holder_state` it carries while that holder is inside the gone horizon
// (writeRoleHolderError, packages/envoy/cmd/listener/api.go). A 404 that names no `holder_state` is
// an older listener's, whose lapsed holder is no holder at all. They are declared here, with the
// rest of the listener's wire words this daemon reads, so the publisher and the controller's
// liveness probe read one copy of them.
const (
	ReasonHolderLapsed = "holder_lapsed"
	StateUnresponsive  = "unresponsive"
)

// RoleRefusal is the body the listener writes when it refuses a role publish or a role lookup
// (writeRoleHolderError and the publish route's refusals, packages/envoy/cmd/listener). Each
// reader takes the members its own decision needs and ignores the rest; a listener of an earlier
// release leaves some of them out, which decodes as their zero values.
type RoleRefusal struct {
	Error       string `json:"error"`
	Reason      string `json:"reason"`
	Holder      string `json:"holder"`
	HolderState string `json:"holder_state"`
	// LastSeen is the holder's last registration in unix milliseconds, absent when the listener
	// can no longer name one.
	LastSeen *int64 `json:"last_seen"`
	EventID  string `json:"event_id"`
}

// The reasons of a role publish the listener answers 504 (ErrNotDelivered). ReasonReceiptTimeout
// says the last forward reached the NATS server and the holder's session sent no receipt for it,
// which a holder that stopped running while its registration lives gives; ReasonDeliveryFailed
// says the listener found no live holder to forward to, could not read the role or session in
// time, or could not show that a forward reached the server, which a hub that is down gives.
const (
	ReasonReceiptTimeout = "receipt_timeout"
	ReasonDeliveryFailed = "delivery_failed"
)

// ErrNotDelivered is a role publish the listener answered 504: it forwarded the message to the
// role's holder, or tried to, and the holder's session did not confirm it within the listener's
// delivery window (publishDeliveryWindow, packages/envoy/cmd/listener/confirmed.go). Reason is
// ReasonReceiptTimeout or ReasonDeliveryFailed; Holder is the session the listener last resolved,
// "" when it resolved none; EventID names the envelope it forwarded. Only a listener
// that confirms a role message before it answers gives it. An earlier listener answers 200 once
// the NATS server took the message and reports a forward that then fails as a role-lane exception
// (notifications.envoy.exceptions.notifications.role.*), so a caller that acts on this answer
// still has to hear those reports while such a listener runs. A 504 without one of the two
// reasons is a plain refusal, never ErrNotDelivered.
type ErrNotDelivered struct {
	Reason  string
	Holder  string
	EventID string
}

func (e ErrNotDelivered) Error() string {
	return fmt.Sprintf("the role's holder %q did not confirm event %s (%s)", e.Holder, e.EventID, e.Reason)
}

// ErrRefusedByHolder is a role publish the listener answered 403 with reason "capability": the
// role's holder, the session Holder names, does not advertise the delivery mode of the payload, or
// the mode cannot be read unambiguously, so the listener forwarded nothing. Sending it again
// changes nothing until another session holds the role. Only a listener that confirms a role
// message before it answers gives it; an earlier listener's role lane drops such a message after
// its 200 without a report.
type ErrRefusedByHolder struct {
	Holder string
}

func (e ErrRefusedByHolder) Error() string {
	return fmt.Sprintf("the role's holder %q does not advertise the delivery mode", e.Holder)
}

// ErrPublishDenied is a role publish the listener answered 403 because the NATS server refused the
// forward under the listener's own publish permissions, so the listener forwarded nothing: its body's
// error opens with publishDeniedPrefix (writeBusUnavailable, packages/envoy/cmd/listener/api.go).
// Message is that error, which names the subject the grant does not cover; it is bounded by
// refusalBodyLimit as the refusal is. Sending it again changes nothing until an operator changes the
// listener's grant. Any other 403 (one whose error does not open with the prefix, or whose body is
// not JSON, as a proxy in front of the listener could write) stays a plain refusal, retried.
type ErrPublishDenied struct {
	Message string
}

// publishDeniedPrefix mirrors the listener's publishDeniedPrefix (packages/envoy/cmd/listener/api.go),
// the opening of every answer it gives a publish its own NATS grant denies. Every listener since
// sjawhar/legion#2167, which added the prefix, writes it, so a daemon that takes this release before
// its listeners reads their denials too.
const publishDeniedPrefix = "envoy: the message bus denied the publish under this listener's own permissions"

func (e ErrPublishDenied) Error() string {
	return fmt.Sprintf("the listener's NATS grant denied the forward (%s)", e.Message)
}

// RoleTopicPrefix is the Envoy subject a role token is reached on (ROLE_TOPIC_PREFIX,
// packages/contracts/src/subject.ts): a notice goes to its architect's, and a review-stuck notice
// names its reviewer's, so the architect need not build it.
const RoleTopicPrefix = "notifications.role."

// ControllerTopic is the topic of project's controller, the one notice topic the daemon publishes
// to; a workflow notice goes to the role topic of the architect that owns its issue instead (the
// daemon's notice executor), since every issue's topic is a subject its phase workers subscribe
// to. It carries the controller notices (record.ControllerNotice), each an outbox row of its own:
// every hold and a tree architect's own failed claim (workflow's noticeWithController), a root in
// triage handed to Legion (admission), a root that gave its admission slot up while its architect
// waits on a person (`awaiting-person`, workflow's enterAwaiting), a root admission took out of the
// workflow because it held no slot and waited for none (`unslotted`, admit's releaseUnslotted), and
// the slot wakes admission sends through wakeController, `slot-free`, `todo` and `tick`
// (admit.Admission.wakeController and its callers say when; the tick alone carries
// `openCapabilities`, the deployment capabilities with no decision,
// admit.Admission.ReportCapabilities). The controller takes no issue for `slot-free`, `todo` or
// `awaiting-person`: it names the free slots in its summary and report, and work reaches Legion
// only when a person hands it over or a role reports a clear defect the controller files
// (skill://legion-controller, "Free slots"). It sits among the issue topics, where no
// issue key (`[A-Z][A-Z0-9]*-[0-9]+`) can be `controller`, and the plugin's controller subscribes to it
// while it holds the controller role (legionControllerNoticeSubject,
// packages/contracts/src/subject.ts). project is the project token panes are told as LEGION_PROJECT
// (packages/pi-legion/src/controller-session.ts), never the Dispatch project key. It is a live
// wake only: the stream retains it, but an Oh My Pi session subscribes over core NATS and is never
// handed a retained copy, so a controller learns what happened before it started from `legion
// state` and Dispatch.
func ControllerTopic(project string) string {
	return "notifications.legion." + project + ".controller"
}

// Publisher delivers a notice to one listener topic.
type Publisher interface {
	Publish(ctx context.Context, topic, message string, payload any, dedupeKey string) error
}

// HTTPPublisher calls the Envoy listener's publish route.
type HTTPPublisher struct {
	baseURL string
	token   string
	client  *http.Client
}

// New creates a publisher for one Envoy listener. token is the listener's bearer
// (`envoy_token_file`), sent on every publish; "" sends none, for a listener that requires none.
func New(baseURL, token string) *HTTPPublisher {
	return &HTTPPublisher{baseURL: strings.TrimRight(baseURL, "/"), token: token, client: &http.Client{Timeout: requestTimeout}}
}

// Publish posts the listener envelope that preserves the outbox row's idempotent delivery key.
func (p *HTTPPublisher) Publish(ctx context.Context, topic, message string, payload any, dedupeKey string) error {
	// The listener carries a payload as a JSON document in a string (its messageBody,
	// packages/envoy/cmd/listener/api.go:68-80); an object there is refused as invalid JSON.
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode notice payload for %s: %w", topic, err)
	}
	body, err := json.Marshal(struct {
		Topic     string `json:"topic"`
		Message   string `json:"message"`
		Payload   string `json:"payload"`
		DedupeKey string `json:"dedupe_key"`
	}{Topic: topic, Message: message, Payload: string(encoded), DedupeKey: dedupeKey})
	if err != nil {
		return fmt.Errorf("encode notice for %s: %w", topic, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages/publish", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build notice publish request for %s: %w", topic, err)
	}
	request.Header.Set("Content-Type", "application/json")
	if p.token != "" {
		request.Header.Set("Authorization", "Bearer "+p.token)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("publish notice to %s: %w", topic, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, refusalBodyLimit))
		refusal := fmt.Errorf("publish notice to %s: listener returned %s: %s", topic, response.Status, strings.TrimSpace(string(body)))
		var answer RoleRefusal
		if json.Unmarshal(body, &answer) != nil {
			return refusal
		}
		switch {
		case response.StatusCode == http.StatusNotFound && answer.Reason == ReasonHolderLapsed && answer.HolderState == StateUnresponsive:
			return fmt.Errorf("%w: %w", refusal, ErrHolderUnresponsive)
		case response.StatusCode == http.StatusNotFound && slices.Contains(roleHolderReasons, answer.Reason):
			return fmt.Errorf("%w: %w", refusal, ErrNoHolder)
		case response.StatusCode == http.StatusGatewayTimeout && (answer.Reason == ReasonReceiptTimeout || answer.Reason == ReasonDeliveryFailed):
			return fmt.Errorf("%w: %w", refusal, ErrNotDelivered{Reason: answer.Reason, Holder: answer.Holder, EventID: answer.EventID})
		case response.StatusCode == http.StatusForbidden && answer.Reason == "capability":
			return fmt.Errorf("%w: %w", refusal, ErrRefusedByHolder{Holder: answer.Holder})
		case response.StatusCode == http.StatusForbidden && strings.HasPrefix(answer.Error, publishDeniedPrefix):
			return fmt.Errorf("%w: %w", refusal, ErrPublishDenied{Message: answer.Error})
		}
		return refusal
	}
	return nil
}

// DeleteSession removes session's registration from the listener's registry (DELETE
// /v1/sessions/<session>), the clean stop the daemon sends for a Legion role whose agent process
// is gone. The listener then reads the session `gone` at once rather than at the end of its live
// window, so the role claim it held is released and its issue claims are free without the wait a
// stopped process would otherwise cost (dispatch://INFRA-392). A registry that holds no record
// for the session answers 404: it is already gone, which is the outcome asked for, so that is no
// failure.
func (p *HTTPPublisher) DeleteSession(ctx context.Context, session string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, p.baseURL+"/v1/sessions/"+url.PathEscape(session), nil)
	if err != nil {
		return fmt.Errorf("build the session delete for %s: %w", session, err)
	}
	if p.token != "" {
		request.Header.Set("Authorization", "Bearer "+p.token)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("delete session %s: %w", session, err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, refusalBodyLimit))
	return fmt.Errorf("delete session %s: listener returned %s: %s", session, response.Status, strings.TrimSpace(string(body)))
}
