package logging

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"time"
)

// Logger wraps slog with structured JSON output.
type Logger struct {
	// ids are the attributes every line carries ahead of its own: machine_id, then instance when
	// the logger was built with one (WithInstance).
	ids    []slog.Attr
	logger *slog.Logger
}

// Option configures a Logger New or NewWithWriter builds.
type Option func(*options)

type options struct {
	instance string
}

// WithInstance adds an instance attribute, beside machine_id, to every line the logger writes:
// which process of the machine wrote it, where several run for one machine id. An empty instance
// adds none.
func WithInstance(instance string) Option {
	return func(o *options) { o.instance = instance }
}

// New creates a new structured logger with the given machine ID.
func New(machineID string, opts ...Option) *Logger {
	return NewWithWriter(machineID, os.Stderr, opts...)
}

// NewWithWriter is New writing to w instead of stderr, so a test can read the lines back.
func NewWithWriter(machineID string, w io.Writer, opts ...Option) *Logger {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	ids := []slog.Attr{slog.String("machine_id", machineID)}
	if o.instance != "" {
		ids = append(ids, slog.String("instance", o.instance))
	}
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})
	return &Logger{ids: ids, logger: slog.New(handler)}
}

// Slog is this logger as a plain *slog.Logger carrying the machine id and instance, to hand to a
// package that takes one (store.WithLogger). Its records are the JSON the wrapper's own calls
// produce, so a query keyed on msg, machine_id or instance finds them both. It carries no ts
// attribute: the JSON handler already stamps every record with time, and only the wrapper's own
// calls add the duplicate.
//
// It is not for slog.SetDefault. That would also route the stdlib log package into this handler,
// and the deployed CloudWatch metric filters for publish failures, webhook refusals and dropped
// stream subjects are space-delimited text patterns anchored on that package's date and time
// prefix.
func (l *Logger) Slog() *slog.Logger {
	return slog.New(l.logger.Handler().WithAttrs(l.ids))
}

// LogEvent logs a structured event with required fields.
func (l *Logger) LogEvent(level slog.Level, msg string, attrs ...slog.Attr) {
	all := make([]slog.Attr, 0, len(l.ids)+1+len(attrs))
	all = append(all, l.ids...)
	all = append(all, slog.String("ts", time.Now().UTC().Format(time.RFC3339Nano)))
	all = append(all, attrs...)
	l.logger.LogAttrs(nil, level, msg, all...)
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
