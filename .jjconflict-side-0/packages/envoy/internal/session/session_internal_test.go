package session

import (
	"testing"
	"time"
)

func TestDeliver_DefaultTimeout30s(t *testing.T) {
	d := Deliverer{}
	if got := d.timeout(); got != 30*time.Second {
		t.Fatalf("expected default timeout 30s, got %v", got)
	}
}

func TestDeliver_CustomTimeoutRespected(t *testing.T) {
	d := Deliverer{RequestLimit: 5 * time.Second}
	if got := d.timeout(); got != 5*time.Second {
		t.Fatalf("expected custom timeout 5s, got %v", got)
	}
}
