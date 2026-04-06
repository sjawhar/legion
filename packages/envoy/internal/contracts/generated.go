package contracts

import (
	"fmt"
	"strings"
	"time"
)

type Envelope struct {
	EventID        string `json:"event_id"`
	Source         string `json:"source"`
	SourceEventID  string `json:"source_event_id"`
	SourceSession  string `json:"source_session,omitempty"`
	Topic          string `json:"topic"`
	DedupeKey      string `json:"dedupe_key"`
	IssuedAt       int64  `json:"issued_at"`
	ExpiresAt      *int64 `json:"expires_at,omitempty"`
	PayloadSummary string `json:"payload_summary"`
	PayloadRef     string `json:"payload_ref,omitempty"`
	TraceID        string `json:"trace_id"`
}

func (e Envelope) Validate() error {
	if strings.TrimSpace(e.EventID) == "" {
		return fmt.Errorf("event_id is required")
	}
	if strings.TrimSpace(e.Source) == "" {
		return fmt.Errorf("source is required")
	}
	if strings.TrimSpace(e.SourceEventID) == "" {
		return fmt.Errorf("source_event_id is required")
	}
	if strings.TrimSpace(e.Topic) == "" {
		return fmt.Errorf("topic is required")
	}
	if strings.TrimSpace(e.DedupeKey) == "" {
		return fmt.Errorf("dedupe_key is required")
	}
	if e.IssuedAt == 0 {
		return fmt.Errorf("issued_at must be set")
	}
	if strings.TrimSpace(e.PayloadSummary) == "" {
		return fmt.Errorf("payload_summary is required")
	}
	if strings.TrimSpace(e.TraceID) == "" {
		return fmt.Errorf("trace_id is required")
	}
	switch e.Source {
	case "agent", "github", "slack", "whatsapp", "ghostwispr":
	default:
		return fmt.Errorf("source must be one of: agent, github, slack, whatsapp, ghostwispr")
	}
	return nil
}

const AgentTopicPrefix = "notifications.agent."

func NowMillis() int64 {
	return time.Now().UnixMilli()
}

func AgentSubject(session string) string {
	return AgentTopicPrefix + session
}

func GithubSubject(owner string, repo string, kind string) string {
	return "notifications.github." + owner + "." + repo + "." + kind
}

func SlackSubject(team string, channel string, kind string) string {
	return "notifications.slack." + team + "." + channel + "." + kind
}

func SlackThreadSubject(team, channel, threadTs, kind string) string {
	return "notifications.slack." + team + "." + channel + ".thread." + strings.ReplaceAll(threadTs, ".", "_") + "." + kind
}

func GithubResourceSubject(owner string, repo string, resourceType string, resourceNumber string) string {
	return "notifications.github." + owner + "." + repo + "." + resourceType + "." + resourceNumber
}

const GhostWisprTopicPrefix = "notifications.ghostwispr."

func GhostWisprSubject(recordingId string, kind string) string {
	return GhostWisprTopicPrefix + recordingId + "." + kind
}

func WhatsappSubject(phone, jid, kind string) string {
	return "notifications.whatsapp." + phone + "." + jid + "." + kind
}
