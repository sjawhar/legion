package session

import (
	"errors"
	"fmt"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// ErrNoSessionRecord is returned when a delivery found nothing of the session it names to deliver
// through: no session entry was resolved at all, or the entry resolved records no way the session
// receives a message - no consumer of its own, and no subscription it holds itself, which is what
// a session registered through a plugin older than both leaves behind. Neither cause says the
// session is gone. A caller that resolved the entry from a cache alone cannot conclude that - the
// cache trails the bucket it follows - and an entry that records neither way is rewritten by the
// session's next registration, so a caller may wait rather than give the message up, and the
// listener's delivery does, deferring such a message for its whole retry window
// (cmd/listener/unplaceable_delivery.go).
var ErrNoSessionRecord = errors.New("no session record")

<<<<<<< conflict 1 of 3
%%%%%%% diff from: rmmmynwt 8bfdf258 "fix(dispatch): a version that restores the approved text keeps its approval (#2703)"
\\\\\\\        to: zwxwlryn 92c90c86 "merge: main into INFRA-771, past f5bdbada so worker-image/build has repo-paths.sh"
 // ErrNoPushTarget is returned when a session entry exists and carries nothing this listener can
-// push to: no serve port, and not self-subscribed. That is the record a session writes when it
-// registers before its server has bound its serve port, and binding the port rewrites it, so
-// re-reading it answers the same thing only until that write lands - a caller may wait the port
-// out rather than give the message up, and the listener's delivery does, deferring such a message
-// for its whole retry window (cmd/listener/unplaceable_delivery.go).
+// push to: no serve port, and no consumer of its own holding this message
+// (SessionEntry.ConsumerRelaysStored). That is the record a session writes when it registers
+// before its server has bound its serve port, and binding the port rewrites it, so re-reading it
+// answers the same thing only until that write lands - a caller may wait the port out rather than
+// give the message up, and the listener's delivery does, deferring such a message for its whole
+// retry window (cmd/listener/unplaceable_delivery.go). It is also the record of a session an
+// earlier build left on the deleted core receive path, which no path reaches now.
 var ErrNoPushTarget = errors.New("no reachable push target")
 
 var foreignSessionID = regexp.MustCompile(`\b01a0[0-9a-f]{4}-[0-9a-f]{4}-7[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
 
 // Deliverer pushes a notification to the session a delivery resolved, over that session's serve
 // port. It holds no session source of its own: every caller resolves the entry it delivers
 // through and hands it over (DeliverWithResultForEntry), so the record delivery acts on is the
 // one that caller read.
 type Deliverer struct {
 	MachineID    string
 	HostBridge   string
 	RequestLimit time.Duration
 	HTTPClient   *http.Client
 }
 
+++++++ lmonpkut e2b93abb "ci: the envoy-go lane gets 60 minutes, and its test steps time out inside it (#2739)"
>>>>>>> conflict 1 of 3 ends
// DeliveryResult describes a completed delivery attempt.
type DeliveryResult struct {
	Skipped bool
	// Relayed is set with Skipped by Resolve for a message the session's own consumer holds, so
	// the relay of that consumer is what delivers it (ConsumerRelaysStored): the caller records
	// nothing about it, neither an attempt nor a dedupe key, since it delivered nothing. A
	// dedupe key says the session has the message, and only a delivery may say that.
	Relayed bool
}

// ConsumerRelaysStored reports whether the session's own consumer holds the stored message of
// stream sequence storedSeq on topic, so the relay of that consumer (cmd/listener's sessionRelays)
// is what delivers it and no other path may record anything about it: a dedupe key written for a
// message this listener handed to nobody would have the next copy of the same event skipped as
// one the session already has, and an attempt record would have a redelivery of it skipped as one
// already on its way. A topic the stream does not capture is in no consumer whatever the
// session's delivery is, so the session takes such a message off its own subscription.
func (e SessionEntry) ConsumerRelaysStored(topic string, storedSeq uint64) bool {
	return e.ConsumerHoldsStored(storedSeq) && bus.StreamCaptures(topic)
}

// Resolve decides what a delivery of item to sessionID's session at entry comes to: the session's
// own consumer relays the message, the session takes it off its own NATS subscription, or the
// entry records no way of receiving at all. A result with Skipped set needs nothing of this
// delivery, because the session receives the message another way, and an error is the delivery's
// failure. A nil entry produces ErrNoSessionRecord, the same result a failed lookup produces, so
// a caller can avoid a second registry read.
//
// It is a package function because the decision holds no state at all: every caller resolves the
// entry it decides on and hands it over, so the record the decision acts on is the one that
// caller read, and nothing of the listener it runs in enters the decision.
//
// storedSeq is the stream sequence of the durable path's copy of a stored message, 0 for any
// other delivery. A message is stored on the subject its envelope names as its topic, so that
// topic and the sequence together decide whether a session on consumer delivery is already held
// this message by its own consumer (SessionEntry.ConsumerRelaysStored). A core path's message is
// not the durable's copy of a stored one, so its caller passes a sequence of 0, which leaves a
// session on consumer delivery its consumer's copy whatever that copy's sequence is.
func Resolve(item contracts.Envelope, sessionID string, entry *SessionEntry, storedSeq uint64) (DeliveryResult, error) {
	if entry == nil {
		return DeliveryResult{}, fmt.Errorf("%w for session %s", ErrNoSessionRecord, sessionID)
	}
	if entry.ConsumerRelaysStored(item.Topic, storedSeq) {
		// The session's consumer holds this stored message, and the listener that owns the session
<<<<<<< conflict 2 of 3
%%%%%%% diff from: rmmmynwt 8bfdf258 "fix(dispatch): a version that restores the approved text keeps its approval (#2703)"
\\\\\\\        to: zwxwlryn 92c90c86 "merge: main into INFRA-771, past f5bdbada so worker-image/build has repo-paths.sh"
 		// relays it from there, serving port or not: the durable path pushes nothing and reports
-		// nothing for it. A topic the stream does not store reaches the session as before (the
-		// live lane's push to a serving port; a session's own subscription, which is where a team
-		// or other ad-hoc topic still arrives). A durable message stored below the consumer's
-		// start is in no consumer, so it is pushed below as for a session with none.
+		// nothing for it. A topic the stream does not store reaches a serving port through the
+		// live lane's push, and reaches a portless session on no path of its own. A durable
+		// message stored below the consumer's start is in no consumer, so it is pushed below as
+		// for a session with none.
+++++++ lmonpkut e2b93abb "ci: the envoy-go lane gets 60 minutes, and its test steps time out inside it (#2739)"
		// relays it from there: no other path delivers it, and none records anything about it. A
		// topic the stream does not store is in no consumer, and so is a durable message stored
		// below the consumer's start; such a message reaches the session on its own subscription,
		// which is where a team or other ad-hoc topic arrives, by the branch below.
>>>>>>> conflict 2 of 3 ends
		return DeliveryResult{Skipped: true, Relayed: true}, nil
	}
<<<<<<< conflict 3 of 3
%%%%%%% diff from: rmmmynwt 8bfdf258 "fix(dispatch): a version that restores the approved text keeps its approval (#2703)"
\\\\\\\        to: zwxwlryn 92c90c86 "merge: main into INFRA-771, past f5bdbada so worker-image/build has repo-paths.sh"
-	if entry.SelfSubscribed && entry.Port == 0 {
-		// A portless session on the core path, the rollout fallback of "Session consumers" in
-		// packages/envoy/AGENTS.md: it has no consumer of its own (an older plugin, or a listener
-		// with no session-consumer grant), there is nothing to push to, and the message reaches it
-		// on its own NATS subscription. So this is a delivered outcome, not ErrNoPushTarget: only
-		// the push this listener would have made is skipped. A session on consumer delivery took
-		// the branch above and takes this message from its consumer.
-		return DeliveryResult{Skipped: true}, nil
-	}
 	if entry.Port > 0 {
 		// Only the owning listener delivers to a session: every listener on the bus reads each
 		// inbox message from its own durable, and the owner delivers it from its copy: a second
 		// listener that could reach the session would deliver the same message again. The owner
 		// always has the message and reaches its sessions by hostFor's rule (OwnsServingPort).
 		// A non-owner acks silently: no delivery, no exception on any subject, no NAK.
 		if !d.OwnsServingPort(interest, *entry) {
 			return DeliveryResult{}, ErrWrongMachine
 		}
 		return DeliveryResult{Push: true, Entry: *entry}, nil
 	}
+	// An entry with no serving port whose consumer does not hold this message is unreachable: a
+	// session receives through its own consumer alone, and a registration the listener can give
+	// none is refused with nothing written (cmd/listener's subscribeHandler), so such an entry is
+	// one the previous build wrote for a session on the core path. ErrNoPushTarget raises one
+	// delivery exception per stored message for it, which is the honest answer: a silent skip
+	// would hide a session that receives nothing (dispatch://INFRA-771).
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
 	owner := interest.MachineID
 	if owner == "" {
 		owner = entry.MachineID
 	}
 	return owner == "" || owner == d.MachineID
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
 
 // Push posts item to sessionID's serving port, the prompt_async push DeliverWithResultForEntry makes
 // for a serving-port session this listener owns, at the address hostFor reads from entry, and
 // returns nil only on the port's 204. ctx ends the request early. The caller has checked that the
 // session is this listener's to deliver: the relay of a session's consumer pushes each of its stored
 // messages this way (cmd/listener).
 func (d Deliverer) Push(ctx context.Context, item contracts.Envelope, sessionID string, entry SessionEntry) error {
 	return d.prompt(ctx, entry, sessionID, d.Text(item))
 }
 
 func (d Deliverer) prompt(ctx context.Context, entry SessionEntry, sessionID string, text string) error {
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
 	url := "http://" + net.JoinHostPort(d.hostFor(entry), strconv.Itoa(entry.Port)) + "/session/" + sessionID + "/prompt_async"
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
 func (d Deliverer) hostFor(entry SessionEntry) string {
 	registeredHere := entry.MachineID == "" || entry.MachineID == d.MachineID
 	stayedLocal := d.MachineID != "" && strings.EqualFold(entry.Host, d.MachineID)
 	if (registeredHere || stayedLocal) && d.HostBridge != "" {
 		return d.HostBridge
 	}
 	if entry.Host != "" {
 		return entry.Host
 	}
 	if registeredHere {
 		return "host.docker.internal"
 	}
 	return entry.MachineID
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
+++++++ lmonpkut e2b93abb "ci: the envoy-go lane gets 60 minutes, and its test steps time out inside it (#2739)"
	if entry.SelfSubscribed {
		// The session holds its own NATS subscription and the message reaches it there, so this
		// is a delivered outcome with nothing for this listener to do. A session on consumer
		// delivery took the branch above for a message its consumer holds and takes this one for
		// a message no consumer of its own can carry.
		return DeliveryResult{Skipped: true}, nil
	}
	// The entry records neither way of receiving: nothing reaches this session until it registers
	// again (ErrNoSessionRecord).
	return DeliveryResult{}, fmt.Errorf("%w for session %s", ErrNoSessionRecord, sessionID)
>>>>>>> conflict 3 of 3 ends
}
