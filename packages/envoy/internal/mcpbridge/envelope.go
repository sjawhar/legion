package mcpbridge

import (
	"fmt"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
)

// BuildEnvelope constructs an Envoy envelope from an MCP notification and resource read.
func BuildEnvelope(cfg *ServerConfig, notifyURI string, contents []resourceContent) (contracts.Envelope, error) {
	topic, err := cfg.RenderTopic(notifyURI)
	if err != nil {
		return contracts.Envelope{}, err
	}

	now := contracts.NowMillis()
	eventID := id.New()
	summary := buildSummary(cfg, notifyURI, contents)
	dedupeKey := buildDedupeKey(cfg.Source, eventID)

	return contracts.Envelope{
		EventID:        eventID,
		Source:         cfg.Source,
		SourceEventID:  notifyURI,
		Topic:          topic,
		DedupeKey:      dedupeKey,
		IssuedAt:       now,
		PayloadSummary: summary,
		PayloadRef:     notifyURI,
		TraceID:        id.New(),
	}, nil
}

func buildSummary(cfg *ServerConfig, uri string, contents []resourceContent) string {
	// Use text from the first content item if available.
	for _, c := range contents {
		if c.Text != "" {
			return truncateSummary(c.Text, 200)
		}
	}
	// Fallback: generic description.
	return fmt.Sprintf("%s event from %s", cfg.Source, uri)
}

func buildDedupeKey(source string, eventID string) string {
	return source + "." + eventID
}

func truncateSummary(s string, maxChars int) string {
	if utf8.RuneCountInString(s) <= maxChars {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxChars])
}
