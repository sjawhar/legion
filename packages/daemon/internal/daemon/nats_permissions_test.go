package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"

	"github.com/sjawhar/envoy/daemonpermissions"
	"github.com/sjawhar/envoy/testnatsimages"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

const (
	dispatchGrantConsumer = "legion-go-CAPTURE-dispatch"
	githubGrantConsumer   = "legion-go-CAPTURE-github"
)

// The deployment and this proof share daemonpermissions.Grant: core wraps the same values as the
// NATS server grant, while this test drives the daemon's real connection, durable consumers,
// acknowledgements, and exceptions subscription through it.
func TestDaemonPermissionsOnRealServers(t *testing.T) {
	for _, image := range testnatsimages.Images {
		t.Run(image.Name, func(t *testing.T) {
			t.Run("the daemon restarts, acknowledges Dispatch and GitHub, and receives exceptions", func(t *testing.T) {
				grant := daemonpermissions.Grant()
				daemonSeed, admin, js, server, uri := startDaemonGrantServer(t, image.Ref, grant)
				defer admin.Close()
				stream := createDaemonNotificationStream(t, js)

				first := connectDaemonGrant(t, uri, daemonSeed, &syncBuffer{})
				for _, name := range []string{dispatchGrantConsumer, githubGrantConsumer} {
					if _, err := stream.Consumer(t.Context(), name); err != nil {
						t.Fatalf("the first daemon creates %s: %v", name, err)
					}
				}
				first.conn.Close()

				logs := &syncBuffer{}
				restarted := connectDaemonGrant(t, uri, daemonSeed, logs)
				defer restarted.conn.Close()

				exception := make(chan []byte, 1)
				subscription, err := subscribeNoticeExceptions(restarted.conn, func(data []byte) { exception <- append([]byte(nil), data...) })
				if err != nil {
					t.Fatalf("subscribe to role-lane exceptions: %v", err)
				}
				defer subscription.Unsubscribe()
				if err := restarted.conn.Flush(); err != nil {
					t.Fatalf("flush the exceptions subscription: %v", err)
				}

				handler := &failFirstDaemonGrantHandler{}
				stop := runDaemonGrantConsumers(t, restarted, handler)
				defer stop()

				publishDaemonGrantFixture(t, js, "../intake/testdata/dispatch/issue-updated.json")
				publishDaemonGrantFixture(t, js, "../intake/testdata/github/pr-opened.json")
				if err := admin.Publish("notifications.envoy.exceptions.notifications.role.example", []byte("retry this notice")); err != nil {
					t.Fatalf("publish the role-lane exception: %v", err)
				}
				if err := admin.Flush(); err != nil {
					t.Fatalf("flush the role-lane exception: %v", err)
				}

				var got []byte
				testwait.Eventually(t, "the daemon receiving the role-lane exception", func() bool {
					select {
					case got = <-exception:
						return true
					default:
						return false
					}
				})
				if string(got) != "retry this notice" {
					t.Fatalf("role-lane exception = %q, want %q", got, "retry this notice")
				}
				testwait.Eventually(t, "the daemon naking one delivery and applying its redelivery with both event types", func() bool {
					return handler.calls.Load() >= 3
				})
				requireDaemonGrantAcknowledged(t, stream, dispatchGrantConsumer, githubGrantConsumer)
				if strings.Contains(logs.String(), "NATS refused the daemon a permission") {
					t.Fatalf("the daemon logged a permission refusal:\n%s", logs.String())
				}
				if serverLog := daemonGrantServerLogs(t, server); strings.Contains(serverLog, "Permissions Violation") {
					t.Fatalf("the server logged a permissions violation:\n%s", serverLog)
				}
				t.Log("restarted daemon created both consumers, NAKed then acknowledged both event kinds, and received the role-lane exception without a permission refusal")
			})

			t.Run("without the dispatch consumer-create permission, the daemon and server report it", func(t *testing.T) {
				const removed = "$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.>"
				grant := daemonpermissions.Grant()
				held := len(grant.Publish)
				grant.Publish = slices.DeleteFunc(grant.Publish, func(subject string) bool { return subject == removed })
				if len(grant.Publish) != held-1 {
					t.Fatalf("the daemon grant does not contain %s: %v", removed, grant.Publish)
				}

				daemonSeed, admin, js, server, uri := startDaemonGrantServer(t, image.Ref, grant)
				defer admin.Close()
				createDaemonNotificationStream(t, js)

				logs := &syncBuffer{}
				cfg := daemonGrantConfig(uri)
				attempt, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				w := &workflowRuntime{project: cfg.Projects[cfg.Project], log: grantLogger(logs)}
				if err := w.connect(attempt, cfg, natsConnection{seed: daemonSeed}); err == nil {
					t.Fatal("the daemon connected without the dispatch consumer-create permission")
				}

				denied := "$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS." + dispatchGrantConsumer
				var daemonLine, serverLine string
				testwait.Eventually(t, "the daemon log naming the refused JetStream API publish", func() bool {
					daemonLine = matchingLogLine(logs.String(), "NATS refused the daemon a permission", denied)
					return daemonLine != ""
				})
				var serverLogErr error
				testwait.Eventually(t, "the server log naming the refused JetStream API publish", func() bool {
					var serverLog string
					serverLog, serverLogErr = readDaemonGrantServerLogs(server)
					serverLine = matchingLogLine(serverLog, "Publish Violation", denied)
					return serverLine != ""
				})
				if serverLogErr != nil {
					t.Fatalf("read the server log: %v", serverLogErr)
				}
				t.Logf("observed daemon refusal: %s", daemonLine)
				t.Logf("observed server refusal: %s", serverLine)
			})
		})
	}
}

func grantLogger(logs *syncBuffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(logs, nil))
}

func daemonGrantConfig(uri string) config.Config {
	const project = "CAPTURE"
	return config.Config{
		Project: project, NatsURLs: []string{uri}, WorkerRPCTimeout: time.Second,
		Projects: map[string]config.Project{project: {Repo: ghrepo.MustParse("sjawhar/legion")}},
	}
}

func connectDaemonGrant(t *testing.T, uri, seed string, logs *syncBuffer) *workflowRuntime {
	t.Helper()
	cfg := daemonGrantConfig(uri)
	w := &workflowRuntime{project: cfg.Projects[cfg.Project], log: grantLogger(logs)}
	if err := w.connect(t.Context(), cfg, natsConnection{seed: seed}); err != nil {
		t.Fatalf("connect the daemon: %v", err)
	}
	return w
}

func runDaemonGrantConsumers(t *testing.T, w *workflowRuntime, handlers ...intake.Handler) func() {
	t.Helper()
	pool := isolatedOutboxPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.consumers.Run(ctx, pool, handlers...) }()
	return func() {
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("run the daemon consumers: %v", err)
		}
	}
}

type failFirstDaemonGrantHandler struct{ calls atomic.Int32 }

func (h *failFirstDaemonGrantHandler) Apply(context.Context, pgx.Tx, intake.Fact) (intake.Result, error) {
	if h.calls.Add(1) == 1 {
		return intake.Result{}, errors.New("exercise the daemon consumer's NAK path")
	}
	return intake.Result{}, nil
}

func publishDaemonGrantFixture(t *testing.T, js jetstream.JetStream, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var envelope struct {
		Topic string `json:"topic"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if _, err := js.Publish(t.Context(), envelope.Topic, data); err != nil {
		t.Fatalf("publish %s: %v", path, err)
	}
}

func requireDaemonGrantAcknowledged(t *testing.T, stream jetstream.Stream, consumers ...string) {
	t.Helper()
	testwait.Eventually(t, "the daemon acknowledging both retained event types", func() bool {
		for _, name := range consumers {
			consumer, err := stream.Consumer(t.Context(), name)
			if err != nil {
				return false
			}
			info, err := consumer.Info(t.Context())
			if err != nil || info.NumPending != 0 || info.NumAckPending != 0 || info.Delivered.Consumer == 0 {
				return false
			}
		}
		return true
	})
}

func createDaemonNotificationStream(t *testing.T, js jetstream.JetStream) jetstream.Stream {
	t.Helper()
	stream, err := js.CreateStream(t.Context(), jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}})
	if err != nil {
		t.Fatalf("create the notification stream: %v", err)
	}
	return stream
}

func startDaemonGrantServer(t *testing.T, image string, grant daemonpermissions.Permissions) (string, *nats.Conn, jetstream.JetStream, *tcnats.NATSContainer, string) {
	t.Helper()
	daemonSeed, daemonPublic := testnats.User(t)
	adminSeed, adminPublic := testnats.User(t)
	// The server's configuration reads JSON, but not Go's HTML escaping of '>' as \u003e.
	var permissions strings.Builder
	encoder := json.NewEncoder(&permissions)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(map[string]map[string][]string{
		"publish":   {"allow": grant.Publish},
		"subscribe": {"allow": grant.Subscribe},
	}); err != nil {
		t.Fatalf("encode the daemon permissions: %v", err)
	}
	server, uri := testnats.StartNkeyAuthorizedUsers(t, image,
		testnats.NkeyUser{Public: daemonPublic, Permissions: strings.TrimSpace(permissions.String())},
		testnats.NkeyUser{Public: adminPublic})
	admin, err := natsauth.Connect([]string{uri}, adminSeed, nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect the admin user: %v", err)
	}
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatalf("open JetStream as the admin user: %v", err)
	}
	return daemonSeed, admin, js, server, uri
}

func daemonGrantServerLogs(t *testing.T, server *tcnats.NATSContainer) string {
	t.Helper()
	text, err := readDaemonGrantServerLogs(server)
	if err != nil {
		t.Fatalf("read the server log: %v", err)
	}
	return text
}

func readDaemonGrantServerLogs(server *tcnats.NATSContainer) (string, error) {
	logs, err := server.Logs(context.Background())
	if err != nil {
		return "", err
	}
	defer logs.Close()
	text, err := io.ReadAll(logs)
	return string(text), err
}

func matchingLogLine(text string, terms ...string) string {
	for _, line := range strings.Split(text, "\n") {
		if slices.ContainsFunc(terms, func(term string) bool { return !strings.Contains(line, term) }) {
			continue
		}
		return line
	}
	return ""
}
