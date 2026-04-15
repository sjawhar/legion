package logging

import (
	"encoding/json"
	"log/slog"
	"os"
	"time"
)

// Logger wraps slog with structured JSON output.
type Logger struct {
	machineID string
	logger    *slog.Logger
}

// New creates a new structured logger with the given machine ID.
func New(machineID string) *Logger {
	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	return &Logger{
		machineID: machineID,
		logger:    slog.New(handler),
	}
}

// LogEvent logs a structured event with required fields.
func (l *Logger) LogEvent(level slog.Level, msg string, attrs ...slog.Attr) {
	attrs = append([]slog.Attr{
		slog.String("machine_id", l.machineID),
		slog.String("ts", time.Now().UTC().Format(time.RFC3339Nano)),
	}, attrs...)
	l.logger.LogAttrs(nil, level, msg, attrs...)
}

// Info logs an info-level event.
func (l *Logger) Info(msg string, attrs ...slog.Attr) {
	l.LogEvent(slog.LevelInfo, msg, attrs...)
}

// Warn logs a warning-level event.
func (l *Logger) Warn(msg string, attrs ...slog.Attr) {
	l.LogEvent(slog.LevelWarn, msg, attrs...)
}

// Error logs an error-level event.
func (l *Logger) Error(msg string, attrs ...slog.Attr) {
	l.LogEvent(slog.LevelError, msg, attrs...)
}

// DeliveryLog logs a delivery event with status.
func (l *Logger) DeliveryLog(level slog.Level, msg string, sessionID, topic, eventID, status string, attrs ...slog.Attr) {
	deliveryAttrs := []slog.Attr{
		slog.String("session_id", sessionID),
		slog.String("topic", topic),
		slog.String("event_id", eventID),
		slog.String("delivery_status", status),
	}
	deliveryAttrs = append(deliveryAttrs, attrs...)
	l.LogEvent(level, msg, deliveryAttrs...)
}

// RawJSON writes raw JSON to stderr (for compatibility with existing patterns).
// This is used when we need to emit JSON without slog's automatic formatting.
func RawJSON(data map[string]interface{}) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = os.Stderr.Write(append(b, '\n'))
	return err
}
