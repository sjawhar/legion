package main

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/contracts"
)

func TestCIRecorderDefersDependencyLoadUntilInvocation(t *testing.T) {
	var deps atomic.Pointer[listenerDeps]
	recorder := newCIRecorder(&deps)

	conn, err := nats.Connect(sharedListenerTestNATSURI(t))
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	defer conn.Close()
	resetListenerTestState(t, conn)

	ciStore, err := cistore.Open(conn, cistore.WithReplicas(1), cistore.WithTTL(time.Hour))
	if err != nil {
		t.Fatalf("open CI store: %v", err)
	}
	deps.Store(&listenerDeps{ciStore: ciStore})

	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)
	if err := recorder.Record(contracts.CIObservation{
		Owner:      owner,
		Repo:       repo,
		Number:     number,
		SHA:        sha,
		CheckName:  "build",
		SuiteID:    "900",
		CheckRunID: "800",
		URL:        "https://example-host/checks/800",
		Status:     "completed",
		Conclusion: "success",
		ObservedAt: "2026-09-07T03:00:00Z",
	}); err != nil {
		t.Fatalf("record check: %v", err)
	}
	if err := recorder.RecordSuite(contracts.CIObservation{
		Owner:      owner,
		Repo:       repo,
		Number:     number,
		SHA:        sha,
		SuiteID:    "900",
		AppID:      "77",
		Status:     "completed",
		Conclusion: "success",
		ObservedAt: "2026-09-07T03:00:00Z",
	}); err != nil {
		t.Fatalf("record suite: %v", err)
	}
	if err := recorder.RecordHead(owner, repo, number, sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
}
