package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
)

// ErrWrongMachine is returned for a session with a serve port that another listener owns. The
// owner is the listener the session's stored subscription names (interest.MachineID, the one
// store.Registry.Match fans its topics out on), else, when no subscription is stored, the listener
// it registered through (its entry's MachineID); a session neither names counts as every
// listener's. HandleAgentMessage answers it as owned elsewhere, with no delivery, exception or
// NAK, since the owning listener delivers the message.
var ErrWrongMachine = errors.New("session belongs to a different machine")

// OwnedBy reports whether the listener machineID is the one that delivers to the session entry
// resolved through interest, the rule ErrWrongMachine states: the owner is the listener the
// session's stored subscription names (interest.MachineID), else the one it registered through
// (entry.MachineID), and a session neither names is every listener's. A nil interest names no
// listener of its own; the entry is taken by value, since both callers ask only of a session they
// have resolved. DeliverWithResultForEntry refuses a serve-port session this answers false for,
// and the listener asks the same question before it writes the delivery ledger for a pair
// (promptsHere, cmd/listener/delivery.go), so one rule answers both.
func OwnedBy(machineID string, interest *store.Interest, entry SessionEntry) bool {
	owner := entry.MachineID
	if interest != nil && interest.MachineID != "" {
		owner = interest.MachineID
	}
	return owner == "" || owner == machineID
}

// ErrNoSessionRecord is returned when no session entry was resolved for the session a delivery
// names: as far as the resolution that produced it could tell, no record of that session exists.
// A caller that resolved the entry from a cache alone cannot conclude the session is gone from
// this - the cache trails the bucket it follows - which is why the two causes are separate
// errors: only this one says nothing of the session was found at all, and an operator reading a
// give-up acts on the two differently.
var ErrNoSessionRecord = errors.New("no session record")

// ErrNoPushTarget is returned when a session entry exists and carries nothing this listener can
// push to: no serve port, and not self-subscribed. That is the record a session writes when it
// registers before its server has bound its serve port, and binding the port rewrites it, so
// re-reading it answers the same thing only until that write lands - a caller may wait the port
// out rather than give the message up, and the listener's delivery does, deferring such a message
// for its whole retry window (cmd/listener/unplaceable_delivery.go).
var ErrNoPushTarget = errors.New("no reachable push target")

var foreignSessionID = regexp.MustCompile(`\b01a0[0-9a-f]{4}-[0-9a-f]{4}-7[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12}\b`)

// ErrAddressNotReachable is a delivery refused before any request: the session's address is local to
// the pod that would dial it (hostFor's podLocal), and its registration was accepted by another pod
// (SessionEntry.AcceptedHost), whose loopback is not this pod's. Every pod of a machine computes the
// same address (hostFor reads only the registration and the pod's HostBridge), so retrying it on
// this pod, or on any pod but the one that accepted the registration, reaches the wrong process or
// none. The listener reports it as delivery_failed and does not retry the message.
var ErrAddressNotReachable = errors.New("address not reachable from this listener pod")

// Deliverer pushes a notification to the session a delivery resolved, over that session's serve
// port. It holds no session source of its own: every caller resolves the entry it delivers
// through and hands it over (DeliverWithResultForEntry), so the record delivery acts on is the
// one that caller read.
type Deliverer struct {
	MachineID  string
	HostBridge string
	// PodHost is this listener pod's host name (os.Hostname, the pod's name in Kubernetes), which the
	// subscribe handler stamps as a port registration's AcceptedHost. A pod-local address is refused
	// (ErrAddressNotReachable) for a session whose AcceptedHost names another pod. Empty when the
	// host name is unknown, and then nothing is refused.
	PodHost      string
	RequestLimit time.Duration
	HTTPClient   *http.Client
	// ctx carries every prompt's HTTP request (WithContext), so cancelling it aborts a prompt in
	// flight; nil sends each under context.Background.
	ctx context.Context
}

// WithContext returns a copy of d whose prompts run under ctx: cancelling ctx aborts a prompt in
// flight, which then fails with ctx's error like any other failed request.
func (d Deliverer) WithContext(ctx context.Context) *Deliverer {
	d.ctx = ctx
	return &d
}

// DeliveryResult describes a completed delivery attempt.
type DeliveryResult struct {
	Skipped bool
	// Relayed is set with Skipped by Resolve for a message the session's own consumer holds, so
	// the relay of that consumer is what delivers it (ConsumerRelaysStored): the caller pushes
	// nothing and records nothing about it, since a dedupe record it wrote for a push it never
	// made is one the relay reads as a copy the session already has and acks unpushed, and an
	// attempt record is one the relay holds the message under until that record is gone.
	Relayed bool
	// Push is set by Resolve for a serving-port session this listener owns: the caller pushes the
	// message to the session's port (Push) at the address Entry gives. DeliverWithResultForEntry
	// makes that push itself, and never sets it.
	Push  bool
	Entry SessionEntry
}

// DeliverWithResultForEntry delivers through the session entry resolved for
// this attempt. A nil entry produces ErrNoSessionRecord, the same result a
// failed lookup produces, so a caller can avoid a second registry read. It is
// the live and core paths' delivery: the message it pushes is not the durable's copy of a stored
// one, so a session on consumer delivery is left its consumer's copy whatever its sequence
// (Resolve's storedSeq 0).
func (d Deliverer) DeliverWithResultForEntry(item contracts.Envelope, interest store.Interest, entry *SessionEntry) (DeliveryResult, error) {
	result, err := d.Resolve(item, interest, entry, item.Topic, 0)
	if err != nil || !result.Push {
		return result, err
	}
	return DeliveryResult{}, d.Push(d.context(), item, interest.SessionID, result.Entry)
}

// context is what this deliverer's own prompts run under: the one WithContext gave it, or
// context.Background. A delivery handled under a durable message's handling context aborts its
// prompt when the receiver cancels that context at a shutdown.
func (d Deliverer) context() context.Context {
	if d.ctx == nil {
		return context.Background()
	}
	return d.ctx
}

// ConsumerRelaysStored reports whether the session's own consumer holds the stored message of
// stream sequence storedSeq on topic, so the relay of that consumer (cmd/listener's sessionRelays)
// is what delivers it and no other path may push it, nor record anything about it: the dedupe and
// attempt records of such a message are the relay's to write, from its own push, or the relay
// would find a record nothing pushed and ack a copy the session never received, or hold the
// message for a push that is not in flight. A topic the stream does not capture is in no consumer
// whatever the session's delivery is, so it is pushed as before.
func (e SessionEntry) ConsumerRelaysStored(topic string, storedSeq uint64) bool {
	return e.ConsumerHoldsStored(storedSeq) && bus.StreamCaptures(topic)
}

// Resolve decides DeliverWithResultForEntry's delivery of item to interest's session at entry
// without making its push: a result with Push set is a serving-port session this listener owns,
// for the caller to push to (Push); one with Skipped set needs nothing pushed; an error is the
// delivery's failure, ErrWrongMachine included.
//
// storedSubject and storedSeq name the durable path's copy of a stored message: the subject it is
// stored on, which is item's own topic for everything but the stored copy of a role forward (that
// is stored on the holder's agent subject while its envelope keeps the role topic), and its stream
// sequence, 0 for any other delivery. Together they decide whether a session on consumer delivery
// is already held this message by its own consumer (SessionEntry.ConsumerRelaysStored); an empty
// storedSubject names a copy no consumer holds, for a caller that is the only delivery of it left.
func (d Deliverer) Resolve(item contracts.Envelope, interest store.Interest, entry *SessionEntry, storedSubject string, storedSeq uint64) (DeliveryResult, error) {
	if entry == nil {
		return DeliveryResult{}, fmt.Errorf("%w for session %s", ErrNoSessionRecord, interest.SessionID)
	}
	if entry.ConsumerRelaysStored(storedSubject, storedSeq) {
		// The session's consumer holds this stored message, and the listener that owns the session
		// relays it from there, serving port or not: the durable path pushes nothing and reports
		// nothing for it. A topic the stream does not store reaches the session as before (the
		// live lane's push to a serving port; a session's own subscription, which is where a team
		// or other ad-hoc topic still arrives). A durable message stored below the consumer's
		// start is in no consumer, so it is pushed below as for a session with none.
		return DeliveryResult{Skipped: true, Relayed: true}, nil
	}
	if entry.SelfSubscribed && entry.Port == 0 {
		// A portless session on the core path, the rollout fallback of "Session consumers" in
		// packages/envoy/AGENTS.md: it has no consumer of its own (an older plugin, or a listener
		// with no session-consumer grant), there is nothing to push to, and the message reaches it
		// on its own NATS subscription. So this is a delivered outcome, not ErrNoPushTarget: only
		// the push this listener would have made is skipped. A session on consumer delivery took
		// the branch above and takes this message from its consumer.
		return DeliveryResult{Skipped: true}, nil
	}
	if entry.Port > 0 {
		// Only the owning listener delivers to a session: every listener on the bus reads each
		// inbox message from its own durable, and the owner delivers it from its copy: a second
		// listener that could reach the session would deliver the same message again. The owner
		// always has the message and reaches its sessions by hostFor's rule (OwnsServingPort).
		// A non-owner acks silently: no delivery, no exception on any subject, no NAK.
		if !d.OwnsServingPort(interest, *entry) {
			return DeliveryResult{}, ErrWrongMachine
		}
		if _, err := d.address(*entry); err != nil {
			return DeliveryResult{}, err
		}
		return DeliveryResult{Push: true, Entry: *entry}, nil
	}
	return DeliveryResult{}, fmt.Errorf("%w for session %s", ErrNoPushTarget, interest.SessionID)
}

// OwnsServingPort reports whether this listener delivers to the serving-port session of interest
// and entry. The stored subscription names the owner before the entry does, so a session that
// re-registered through another listener stays with the one its topics are matched on
// (store.Registry.Match), which keeps the stored MachineID: the owner of a serving-port session is
// the listener its stored subscription names, else the one its entry names; a session neither
// names is every listener's. A re-registration does not move that subscription (store's
// mergeForUpsert); dispatch://INFRA-429 moves it.
func (d Deliverer) OwnsServingPort(interest store.Interest, entry SessionEntry) bool {
	return OwnedBy(d.MachineID, &interest, entry)
}

func (d Deliverer) Text(item contracts.Envelope) string {
	from := item.Source
	if item.SourceSession != "" {
		from = item.SourceSession
	}
	header := "[NOTIFICATION"
	if strings.HasPrefix(item.Topic, contracts.AgentTopicPrefix) {
		header += " to you"
	}
	header += " from " + from
	if item.Sender != nil && item.Sender.Title != "" {
		header += " (" + item.Sender.Title + ")"
	}
	header += "]"

	lines := []string{
		header,
		"At: " + time.UnixMilli(item.IssuedAt).UTC().Format(time.RFC3339),
		"Event ID: " + item.EventID,
	}
	if item.ExpiresAt != nil {
		lines = append(lines, "By: "+time.UnixMilli(*item.ExpiresAt).UTC().Format(time.RFC3339))
	}
	if item.Urgency != "" {
		lines = append(lines, "Urgency: "+item.Urgency)
	}
	if item.ExpectsReply != "" {
		lines = append(lines, "Expects Reply: "+item.ExpectsReply)
	}
	if item.InReplyTo != "" {
		lines = append(lines, "In Reply To: "+item.InReplyTo)
	}
	if item.Supersedes != "" {
		lines = append(lines, "Supersedes: "+item.Supersedes)
	}
	if item.Source == "agent" && item.SourceSession != "" {
		lines = append(lines, fmt.Sprintf("Reply With: envoy_send(session_id=%q, message=%q)", item.SourceSession, "..."))
	}
	if item.Sender != nil && len(item.Sender.Roles) > 0 {
		lines = append(lines, fmt.Sprintf("Reply Role: envoy_publish(topic=%q, message=%q)", contracts.RoleTopicPrefix+item.Sender.Roles[0], "..."))
	}
	messageRendered := item.Payload != "" && item.Payload != item.PayloadSummary
	if !messageRendered || !strings.HasPrefix(item.Payload, strings.TrimSuffix(item.PayloadSummary, "…")) {
		lines = append(lines, "Summary: "+item.PayloadSummary)
	}
	if messageRendered {
		lines = append(lines, "Message:", item.Payload)
	}
	if foreign := foreignSession(item); foreign != "" {
		sender := item.SourceSession
		if sender == "" {
			sender = "unknown"
		}
		lines = append(lines, "Note: body names session "+foreign+"; the sender is "+sender)
	}
	lines = append(lines, "", "Topic: "+item.Topic)
	return strings.Join(lines, "\n")
}

func foreignSession(item contracts.Envelope) string {
	recipient := ""
	if strings.HasPrefix(item.Topic, contracts.AgentTopicPrefix) {
		recipient = strings.TrimPrefix(item.Topic, contracts.AgentTopicPrefix)
	}
	for _, text := range [...]string{item.PayloadSummary, item.Payload} {
		for {
			match := foreignSessionID.FindStringIndex(text)
			if match == nil {
				break
			}
			candidate := text[match[0]:match[1]]
			if candidate != item.SourceSession && candidate != recipient {
				return candidate
			}
			text = text[match[1]:]
		}
	}
	return ""
}

// Push posts item to sessionID's serving port, the prompt_async push DeliverWithResultForEntry
// makes for a serving-port session this listener owns, at the address address reads from entry,
// and returns nil only on the port's 204. ctx ends the request early. The caller has checked that
// the session is this listener's to deliver: the relay of a session's consumer pushes each of its
// stored messages this way (cmd/listener).
func (d Deliverer) Push(ctx context.Context, item contracts.Envelope, sessionID string, entry SessionEntry) error {
	return d.prompt(ctx, entry, sessionID, d.Text(item))
}

// prompt posts text to entry's serving port. An address no request from this pod can reach the
// session at is refused before one is made (address, ErrAddressNotReachable).
func (d Deliverer) prompt(ctx context.Context, entry SessionEntry, sessionID string, text string) error {
	host, err := d.address(entry)
	if err != nil {
		return err
	}
	type promptBody struct {
		Parts []map[string]string `json:"parts"`
	}
	bodyData := promptBody{
		Parts: []map[string]string{{"type": "text", "text": text}},
	}
	body, err := json.Marshal(bodyData)
	if err != nil {
		return err
	}
	url := "http://" + net.JoinHostPort(host, strconv.Itoa(entry.Port)) + "/session/" + sessionID + "/prompt_async"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := d.httpClient()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("prompt_async returned %d", resp.StatusCode)
	}
	return nil
}

// resume was removed — cold-starting host processes from inside a container
// runs as root and is a security risk. Messages stay in JetStream for retry.

// hostFor is the address rule every delivery follows, for a session this listener owns
// (DeliverWithResultForEntry refuses another listener's session with ErrWrongMachine, since that
// session is its owner's to deliver). The address follows where the session runs, read from its
// entry. The session runs on this listener's machine when its entry names this listener or no
// listener (it registered here), or when its recorded Host equals this listener's machine id,
// compared case-insensitively (a plugin records its own machine id as Host, so a session that
// stayed on this machine but re-registered through another listener names this machine). Such a
// session goes through HostBridge when one is configured: the listener may run in a container
// that reaches its own machine's sessions only through that bridge, not by dialling its own host
// name. A session that re-registered through another listener and records another host runs on
// that machine, which this listener's bridge does not reach, so it never goes through the bridge.
// Otherwise the session is reached at its recorded Host, where its process runs; with none
// recorded (its plugin predates the field), at host.docker.internal when it registered through
// this listener, else at the machine id of the listener it registered through. A session owned
// by a listener that cannot reach its port is reached by nobody.
//
// podLocal reports the two answers that are local to the pod that dials them: a loopback bridge
// (isLoopbackHost), and the host.docker.internal fallback. hostFor reads only the registration and
// this pod's bridge, so every pod of a machine computes the same address, and every other answer
// (a bridge that is not loopback, a recorded Host, another listener's machine id) reaches the
// session from each of them alike. A pod-local address reaches the session only from the pod the
// session can reach, which DeliverWithResultForEntry checks (acceptedElsewhere) before it dials.
// A bridge naming host.docker.internal is not the fallback: it is the operator's choice of address
// for this machine, as any bridge that is not loopback is.
func (d Deliverer) hostFor(entry SessionEntry) (host string, podLocal bool) {
	registeredHere := entry.MachineID == "" || entry.MachineID == d.MachineID
	stayedLocal := d.MachineID != "" && strings.EqualFold(entry.Host, d.MachineID)
	if (registeredHere || stayedLocal) && d.HostBridge != "" {
		return d.HostBridge, isLoopbackHost(d.HostBridge)
	}
	if entry.Host != "" {
		return entry.Host, false
	}
	if registeredHere {
		return "host.docker.internal", true
	}
	return entry.MachineID, false
}

// isLoopbackHost reports whether host names this host's loopback: a loopback IP literal (127.0.0.0/8,
// ::1) or localhost, compared case-insensitively.
func isLoopbackHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}

// acceptedElsewhere reports whether entry's registration was accepted by a listener pod other than
// this one (SessionEntry.AcceptedHost against PodHost, compared case-insensitively), so a pod-local
// address dialled here would not reach the session. A registration without AcceptedHost (written by
// a build before the field) and a pod that does not know its own host name refuse nothing, and are
// delivered as before. Pods that share a host (host networking) share its name and its loopback,
// so they deliver each other's registrations.
func (d Deliverer) acceptedElsewhere(entry SessionEntry) bool {
	return entry.AcceptedHost != "" && d.PodHost != "" && !strings.EqualFold(entry.AcceptedHost, d.PodHost)
}

// address is the host every push to entry's serving port dials (hostFor), or
// ErrAddressNotReachable when that host is local to the pod that dials it and another pod accepted
// the session's registration (acceptedElsewhere): no request from this pod reaches the session
// then, and every pod of the machine computes the same address, so no retry here would. Resolve
// refuses such a delivery before it reports a push to make, and prompt before it makes one, so
// every route to a serving port - the durable path's worker, the relay of a session's consumer and
// the live and core lanes - refuses it alike.
func (d Deliverer) address(entry SessionEntry) (string, error) {
	host, podLocal := d.hostFor(entry)
	if podLocal && d.acceptedElsewhere(entry) {
		return "", fmt.Errorf("%w: %s is local to pod %s, and pod %s accepted the session's registration", ErrAddressNotReachable, host, d.PodHost, entry.AcceptedHost)
	}
	return host, nil
}

var hostName = regexp.MustCompile(`^[A-Za-z0-9_]([A-Za-z0-9_.-]{0,251}[A-Za-z0-9_])?$`)

// ValidHost reports whether host can stand as a session's recorded Host in a delivery URL: an
// IP literal (IPv6 unbracketed) or a host name of letters, digits, '_', '.' and '-' that starts
// and ends with a letter, digit or '_' and is at most 253 characters. It refuses anything that
// could change the URL's authority or path - a port, '/', '@', '?', '#', '%', brackets,
// whitespace, control characters. It returns false for the empty string; a caller that accepts
// "no host recorded" checks for empty before calling it.
func ValidHost(host string) bool {
	return net.ParseIP(host) != nil || hostName.MatchString(host)
}

func (d Deliverer) timeout() time.Duration {
	if d.RequestLimit > 0 {
		return d.RequestLimit
	}
	return 30 * time.Second
}

func (d Deliverer) httpClient() *http.Client {
	if d.HTTPClient != nil {
		return d.HTTPClient
	}
	return &http.Client{Timeout: d.timeout()}
}
