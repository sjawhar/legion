package core

import (
	"fmt"
	"log/slog"
	"strings"

	"gopkg.in/yaml.v3"
)

// Urgency is the dispatch thread urgency level.
type Urgency string

const (
	UrgencyLow      Urgency = "low"
	UrgencyMed      Urgency = "med"
	UrgencyHigh     Urgency = "high"
	UrgencyBlocking Urgency = "blocking"
)

// QuestionOption is one selectable option in a QuestionInfo.
type QuestionOption struct {
	Label       string `json:"label" yaml:"label"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

// QuestionInfo describes a structured question attached to a thread.
type QuestionInfo struct {
	Question string           `json:"question" yaml:"question"`
	Header   string           `json:"header,omitempty" yaml:"header,omitempty"`
	Options  []QuestionOption `json:"options" yaml:"options"`
	Multiple *bool            `json:"multiple,omitempty" yaml:"multiple,omitempty"`
	Custom   *bool            `json:"custom,omitempty" yaml:"custom,omitempty"`
}

// MetaMarker carries the parsed/built dispatch metadata.
type MetaMarker struct {
	Urgency   Urgency        `yaml:"urgency"`
	RequestID string         `yaml:"requestId"`
	Ask       []QuestionInfo `yaml:"ask,omitempty"`
}


// BuildMetaMarker renders the canonical dispatch metadata comment. The ask
// attribute is omitted entirely when nil.
func BuildMetaMarker(m MetaMarker) string {
	data, err := yaml.Marshal(m)
	if err != nil {
		slog.Error("dispatch: yaml.Marshal MetaMarker failed", "error", err)
		return "---\nurgency: med\nrequestId: ERROR\n---\n"
	}
	return "---\n" + string(data) + "---\n"
}

// ParseMetaMarker reads the leading YAML frontmatter from a body. Returns
// nil when the body doesn't start with `---` or the frontmatter is invalid.
func ParseMetaMarker(body string) *MetaMarker {
	if !strings.HasPrefix(body, "---\n") {
		return nil
	}
	after := body[4:]
	close := strings.Index(after, "\n---")
	if close < 0 {
		return nil
	}
	var m MetaMarker
	if err := yaml.Unmarshal([]byte(after[:close]), &m); err != nil {
		return nil
	}
	switch m.Urgency {
	case UrgencyLow, UrgencyMed, UrgencyHigh, UrgencyBlocking:
	default:
		slog.Warn("dispatch: meta frontmatter has invalid urgency", "urgency", m.Urgency)
		return nil
	}
	if m.RequestID == "" {
		slog.Warn("dispatch: meta frontmatter missing requestId")
		return nil
	}
	return &m
}


// BuildThreadBody renders the canonical thread body: marker, blank line, bold
// subject, blank line, body text.
func BuildThreadBody(marker, subject, body string) string {
	return fmt.Sprintf("%s\n**%s**\n\n%s", marker, subject, body)
}
