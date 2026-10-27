// Package notify publishes persisted workflow notices through the Envoy listener.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

const requestTimeout = 10 * time.Second

// refusalBodyLimit bounds how much of a refused publish's body the error carries: the listener
// answers `{"error": ...}` (packages/envoy/cmd/listener/api.go writeJSONError).
const refusalBodyLimit = 4096

// ErrNoHolder is a role publish the listener refused because no live session holds the role: nobody
// claimed it, or its holder's registration lapsed (writeRoleHolderError,
// packages/envoy/cmd/listener/api.go). Every listener release answers it, 404 with one of
// roleHolderReasons. It is the ordinary state before a role's session claims it, not a broken
// publish path, so a caller can hold what it published until one does.
var ErrNoHolder = errors.New("no live session holds the role")

// roleHolderReasons are the reasons the listener's 404 names for a role with no live holder
// (roleHolderUnclaimed, roleHolderLapsed). A 404 without one of them is some other refusal — a route
// an older listener lacks — and never ErrNoHolder.
var roleHolderReasons = []string{"unclaimed", "holder_lapsed"}

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
// "" when it resolved none; EventID and DedupeKey name the envelope it forwarded. Only a listener
// that confirms a role message before it answers gives it. An earlier listener answers 200 once
// the NATS server took the message and reports a forward that then fails as a role-lane exception
// (notifications.envoy.exceptions.notifications.role.*), so a caller that acts on this answer
// still has to hear those reports while such a listener runs. A 504 without one of the two
// reasons is a plain refusal, never ErrNotDelivered.
type ErrNotDelivered struct {
	Reason    string
	Holder    string
	EventID   string
	DedupeKey string
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
		var answer struct {
			Reason    string `json:"reason"`
			Holder    string `json:"holder"`
			EventID   string `json:"event_id"`
			DedupeKey string `json:"dedupe_key"`
		}
		if json.Unmarshal(body, &answer) != nil {
			return refusal
		}
		switch {
		case response.StatusCode == http.StatusNotFound && slices.Contains(roleHolderReasons, answer.Reason):
			return fmt.Errorf("%w: %w", refusal, ErrNoHolder)
		case response.StatusCode == http.StatusGatewayTimeout && (answer.Reason == ReasonReceiptTimeout || answer.Reason == ReasonDeliveryFailed):
			return fmt.Errorf("%w: %w", refusal, ErrNotDelivered{Reason: answer.Reason, Holder: answer.Holder, EventID: answer.EventID, DedupeKey: answer.DedupeKey})
		case response.StatusCode == http.StatusForbidden && answer.Reason == "capability":
			return fmt.Errorf("%w: %w", refusal, ErrRefusedByHolder{Holder: answer.Holder})
		}
		return refusal
	}
	return nil
}
