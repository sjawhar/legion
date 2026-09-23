package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
)

func TestOutboxMessageCrashRecoveryPostsOnceAgainstScratchDispatch(t *testing.T) {
	baseURL, token := startScratchDispatch(t)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := createScratchDispatchIssue(t, baseURL)
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue, record.MessagePost{Body: "The durable message."}, now))

	first := &outbox{
		pool: pool, records: crashAfterPostStore{Store: records}, dispatch: dispatch.New(baseURL, token),
		now: func() time.Time { return now },
	}
	if err := first.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "simulated crash before finish") {
		t.Fatalf("first delivery = %v, want simulated crash after Dispatch post", err)
	}
	if remaining := outboxRows(t, pool); remaining != 1 {
		t.Fatalf("outbox rows after crash = %d, want leased unfinished row", remaining)
	}

	restarted := &outbox{
		pool: pool, records: records, dispatch: dispatch.New(baseURL, token),
		now: func() time.Time { return now.Add(outboxLease + time.Second) },
	}
	if err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatalf("restart recovery: %v", err)
	}
	if remaining := outboxRows(t, pool); remaining != 0 {
		t.Fatalf("outbox rows after recovery = %d, want finished row", remaining)
	}

	bodies, err := dispatch.New(baseURL, token).MessageBodiesSince(context.Background(), issue, now.Add(-time.Second))
	if err != nil {
		t.Fatalf("read real Dispatch messages: %v", err)
	}
	markers := 0
	for _, body := range bodies {
		if strings.Contains(body, "<!-- legion-outbox:") {
			markers++
		}
	}
	if markers != 1 {
		t.Fatalf("outbox-marked messages = %d in %#v, want one post across crash recovery", markers, bodies)
	}
}

type crashAfterPostStore struct{ record.Store }

func (crashAfterPostStore) FinishOutbox(context.Context, pgx.Tx, int64, string) error {
	return errors.New("simulated crash before finish")
}

func startScratchDispatch(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()
	base, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	adminURL := *base
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(ctx, adminURL.String())
	if err != nil {
		t.Fatalf("open Postgres admin pool: %v", err)
	}
	t.Cleanup(admin.Close)
	database := "dispatch_outbox_test_" + outboxSuffix(t)
	if _, err := admin.Exec(ctx, "create database "+database); err != nil {
		t.Fatalf("create scratch Dispatch database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+database+" with (force)"); err != nil {
			t.Errorf("drop scratch Dispatch database: %v", err)
		}
	})
	serverURL := *base
	serverURL.Path = "/" + database
	port := freePort(t)
	repositoryRoot, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	binary := filepath.Join(t.TempDir(), "dispatch")
	build := exec.Command("go", "build", "-o", binary, "./cmd/dispatch")
	build.Dir = filepath.Join(repositoryRoot, "packages", "envoy")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build scratch Dispatch: %v\n%s", err, output)
	}
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("create scratch Dispatch home: %v", err)
	}
	token := "scratch-dispatch-agent-token"
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	command := exec.Command(binary)
	command.Dir = filepath.Join(repositoryRoot, "packages", "envoy")
	command.Env = append(os.Environ(),
		"DATABASE_URL="+serverURL.String(),
		"DISPATCH_AGENT_TOKEN="+token,
		"DISPATCH_IDENTITY=header:X-Dispatch-User",
		"DISPATCH_ALLOWED_LOGINS=smoke",
		"DISPATCH_NATS_DISABLED=1",
		"DISPATCH_LISTEN_HOST=127.0.0.1",
		fmt.Sprintf("DISPATCH_PORT=%d", port),
		"DISPATCH_SERVER_URL="+baseURL,
		"DISPATCH_WEB_DIST="+filepath.Join(repositoryRoot, "packages", "dispatch", "web", "dist"),
		"HOME="+home,
	)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatalf("start scratch Dispatch: %v", err)
	}
	t.Cleanup(func() {
		if command.Process == nil {
			return
		}
		_ = command.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("scratch Dispatch stopped: %v\n%s", err, output.String())
			}
		case <-time.After(10 * time.Second):
			_ = command.Process.Kill()
			<-done
			t.Error("scratch Dispatch did not stop after SIGINT")
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(baseURL + "/api/v1")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return baseURL, token
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("scratch Dispatch did not become ready:\n%s", output.String())
	return "", ""
}

func createScratchDispatchIssue(t *testing.T, baseURL string) string {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	request := func(path string, body any) *http.Response {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s: %v", path, err)
		}
		req, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(encoded))
		if err != nil {
			t.Fatalf("create %s request: %v", path, err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Dispatch-User", "smoke")
		response, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return response
	}
	project := request("/api/v1/projects", map[string]string{"key": "LEGION", "name": "Scratch Legion"})
	if project.StatusCode != http.StatusCreated {
		project.Body.Close()
		t.Fatalf("create scratch project = %s", project.Status)
	}
	project.Body.Close()
	issue := request("/api/v1/issues", map[string]string{"project": "LEGION", "title": "Crash-safe outbox message"})
	defer issue.Body.Close()
	if issue.StatusCode != http.StatusCreated {
		t.Fatalf("create scratch issue = %s", issue.Status)
	}
	var created struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(issue.Body).Decode(&created); err != nil {
		t.Fatalf("decode scratch issue: %v", err)
	}
	if created.Key == "" {
		t.Fatal("scratch Dispatch created an issue without a key")
	}
	return created.Key
}
