package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
)

// ErrWrongMachine is returned when a session belongs to a different machine.
// Callers should ACK the message (another listener owns this session).
var ErrWrongMachine = errors.New("session belongs to a different machine")

var foreignSessionID = regexp.MustCompile(`\b01a0[0-9a-f]{4}-[0-9a-f]{4}-7[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12}\b`)

type Deliverer struct {
	MachineID    string
	HostBridge   string
	RequestLimit time.Duration
	HTTPClient   *http.Client
	Sessions     SessionLookup
}

// DeliveryResult describes a completed delivery attempt.
type DeliveryResult struct {
	Skipped bool
}

func (d Deliverer) Deliver(item contracts.Envelope, interest store.Interest) error {
	_, err := d.DeliverWithResult(item, interest)
	return err
}

func (d Deliverer) DeliverWithResult(item contracts.Envelope, interest store.Interest) (DeliveryResult, error) {
	if d.Sessions == nil {
		return DeliveryResult{}, fmt.Errorf("no session registry configured")
	}
	entryVal, err := d.Sessions.Get(interest.SessionID)
	if err != nil {
		return DeliveryResult{}, fmt.Errorf("no live serve port for session %s", interest.SessionID)
	}
	text := d.Text(item)

	if entryVal.SelfSubscribed && entryVal.Port == 0 {
		// The session consumes its own NATS subscription; there is nothing to push to.
		return DeliveryResult{Skipped: true}, nil
	}
	if entryVal.Port > 0 {
		return DeliveryResult{}, d.prompt(entryVal.Port, entryVal.MachineID, interest.SessionID, text)
	}
	return DeliveryResult{}, fmt.Errorf("no live serve port for session %s", interest.SessionID)
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

func (d Deliverer) prompt(port int, machineID string, sessionID string, text string) error {
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
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s:%d/session/%s/prompt_async", d.hostFor(machineID), port, sessionID), bytes.NewReader(body))
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

func (d Deliverer) hostFor(machineID string) string {
	// Local session — use the host bridge (localhost or docker internal)
	if machineID == "" || machineID == d.MachineID {
		if d.HostBridge != "" {
			return d.HostBridge
		}
		return "host.docker.internal"
	}
	// Remote session — use the machine's hostname (resolved via Tailscale MagicDNS)
	return machineID
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
