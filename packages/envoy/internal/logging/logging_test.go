package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"
)

func TestLoggerOutputsValidJSON(t *testing.T) {
	// Capture stderr
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	logger := New("test-machine")
	logger.Info("test message", slog.String("key", "value"))

	w.Close()
	os.Stderr = oldStderr

	// Read captured output
	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	// Parse as JSON
	var logEntry map[string]interface{}
	if err := json.Unmarshal([]byte(output), &logEntry); err != nil {
		t.Fatalf("log output is not valid JSON: %v\nOutput: %s", err, output)
	}

	// Verify required fields
	if logEntry["machine_id"] != "test-machine" {
		t.Errorf("expected machine_id=test-machine, got %v", logEntry["machine_id"])
	}
	if logEntry["level"] != "INFO" {
		t.Errorf("expected level=INFO, got %v", logEntry["level"])
	}
	if logEntry["msg"] != "test message" {
		t.Errorf("expected msg=test message, got %v", logEntry["msg"])
	}
	if logEntry["key"] != "value" {
		t.Errorf("expected key=value, got %v", logEntry["key"])
	}

	// Verify timestamp is ISO 8601
	ts, ok := logEntry["ts"].(string)
	if !ok {
		t.Fatalf("ts field is not a string: %v", logEntry["ts"])
	}
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		t.Errorf("ts is not valid ISO 8601: %v", err)
	}
}

func TestDeliveryLogIncludesDeliveryFields(t *testing.T) {
	// Capture stderr
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	logger := New("test-machine")
	logger.DeliveryLog(slog.LevelInfo, "delivery test", "session-123", "topic.test", "event-456", "delivered")

	w.Close()
	os.Stderr = oldStderr

	// Read captured output
	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	// Parse as JSON
	var logEntry map[string]interface{}
	if err := json.Unmarshal([]byte(output), &logEntry); err != nil {
		t.Fatalf("log output is not valid JSON: %v\nOutput: %s", err, output)
	}

	// Verify delivery-specific fields
	if logEntry["session_id"] != "session-123" {
		t.Errorf("expected session_id=session-123, got %v", logEntry["session_id"])
	}
	if logEntry["topic"] != "topic.test" {
		t.Errorf("expected topic=topic.test, got %v", logEntry["topic"])
	}
	if logEntry["event_id"] != "event-456" {
		t.Errorf("expected event_id=event-456, got %v", logEntry["event_id"])
	}
	if logEntry["delivery_status"] != "delivered" {
		t.Errorf("expected delivery_status=delivered, got %v", logEntry["delivery_status"])
	}

	// Verify base fields still present
	if logEntry["machine_id"] != "test-machine" {
		t.Errorf("expected machine_id=test-machine, got %v", logEntry["machine_id"])
	}
}

func TestLoggerErrorLevel(t *testing.T) {
	// Capture stderr
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	logger := New("test-machine")
	logger.Error("error message", slog.String("error", "test error"))

	w.Close()
	os.Stderr = oldStderr

	// Read captured output
	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	// Parse as JSON
	var logEntry map[string]interface{}
	if err := json.Unmarshal([]byte(output), &logEntry); err != nil {
		t.Fatalf("log output is not valid JSON: %v\nOutput: %s", err, output)
	}

	if logEntry["level"] != "ERROR" {
		t.Errorf("expected level=ERROR, got %v", logEntry["level"])
	}
}
