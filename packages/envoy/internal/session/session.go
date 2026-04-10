package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
)

// ErrWrongMachine is returned when a session belongs to a different machine.
// Callers should ACK the message (another listener owns this session).
var ErrWrongMachine = errors.New("session belongs to a different machine")

type Deliverer struct {
	MachineID    string
	HostBridge   string
	RequestLimit time.Duration
	Sessions     SessionLookup
}

func (d Deliverer) Deliver(item contracts.Envelope, interest store.Interest) error {
	text := d.Text(item)

	if d.Sessions == nil {
		return fmt.Errorf("no session registry configured")
	}
	entryVal, err := d.Sessions.Get(interest.SessionID)
	if err != nil {
		return fmt.Errorf("no live serve port for session %s", interest.SessionID)
	}
	if d.MachineID != "" && entryVal.MachineID != "" && entryVal.MachineID != d.MachineID {
		return ErrWrongMachine
	}
	if entryVal.Port > 0 {
		return d.prompt(entryVal.Port, interest.SessionID, text)
	}
	return fmt.Errorf("no live serve port for session %s", interest.SessionID)
}

func (d Deliverer) Text(item contracts.Envelope) string {
	header := fmt.Sprintf("[NOTIFICATION from %s]", item.Source)
	if item.SourceSession != "" {
		header = fmt.Sprintf("[NOTIFICATION from %s (reply-to: %s)]", item.Source, item.SourceSession)
	}
	body := item.PayloadSummary
	if item.Payload != "" {
		body = item.Payload
	}
	text := fmt.Sprintf("%s\n%s\n\nTopic: %s\nEvent ID: %s", header, body, item.Topic, item.EventID)
	if item.SourceSession != "" {
		text += fmt.Sprintf("\nUse envoy_send(target_session=\"%s\", message=\"...\") to reply to this message.", item.SourceSession)
	}
	return text
}

func (d Deliverer) prompt(port int, sessionID string, text string) error {
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
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s:%d/session/%s/prompt_async", d.host(), port, sessionID), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: d.timeout()}
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

func (d Deliverer) host() string {
	if d.HostBridge != "" {
		return d.HostBridge
	}
	return "host.docker.internal"
}

func (d Deliverer) timeout() time.Duration {
	if d.RequestLimit > 0 {
		return d.RequestLimit
	}
	return 30 * time.Second
}
