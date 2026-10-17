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
	"github.com/stretchr/testify/require"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"

	"github.com/sjawhar/envoy/daemonpermissions"
	"github.com/sjawhar/envoy/testnatsimages"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/testnats"
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

				first := connectDaemonGrant(t, uri, daemonSeed, slogBuffer(t))
				for _, name := range []string{dispatchGrantConsumer, githubGrantConsumer} {
					_, err := stream.Consumer(t.Context(), name)
					require.NoError(t, err, "the first daemon creates %s", name)
				}
				first.conn.Close()

				logs := slogBuffer(t)
				restarted := connectDaemonGrant(t, uri, daemonSeed, logs)
				defer restarted.conn.Close()

				exception := make(chan []byte, 1)
				subscription, err := subscribeNoticeExceptions(restarted.conn, func(data []byte) { exception <- append([]byte(nil), data...) })
				require.NoError(t, err)
				defer subscription.Unsubscribe()
				require.NoError(t, restarted.conn.Flush())

				handler := &failFirstDaemonGrantHandler{}
				stop := runDaemonGrantConsumers(t, restarted, handler)
				defer stop()

				publishDaemonGrantFixture(t, js, "../intake/testdata/dispatch/issue-updated.json")
				publishDaemonGrantFixture(t, js, "../intake/testdata/github/pr-opened.json")
				require.NoError(t, admin.Publish("notifications.envoy.exceptions.notifications.role.example", []byte("retry this notice")))
				require.NoError(t, admin.Flush())

				select {
				case got := <-exception:
					require.Equal(t, []byte("retry this notice"), got)
				case <-time.After(10 * time.Second):
					t.Fatal("the daemon did not receive the role-lane exception")
				}
				require.Eventually(t, func() bool { return handler.calls.Load() >= 3 }, 10*time.Second, 25*time.Millisecond,
					"the daemon naks one delivery and applies its redelivery with both event types")
				requireDaemonGrantAcknowledged(t, stream, dispatchGrantConsumer, githubGrantConsumer)
				require.NotContains(t, logs.String(), "NATS refused the daemon a permission")
				require.NotContains(t, daemonGrantServerLogs(t, server), "Permissions Violation")
				t.Log("restarted daemon created both consumers, NAKed then acknowledged both event kinds, and received the role-lane exception without a permission refusal")
			})

			t.Run("without the dispatch consumer-create permission, the daemon and server report it", func(t *testing.T) {
				const removed = "$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.>"
				grant := daemonpermissions.Grant()
				held := len(grant.Publish)
				grant.Publish = slices.DeleteFunc(grant.Publish, func(subject string) bool { return subject == removed })
				require.Len(t, grant.Publish, held-1, "the daemon grant contains %s", removed)

				daemonSeed, admin, js, server, uri := startDaemonGrantServer(t, image.Ref, grant)
				defer admin.Close()
				createDaemonNotificationStream(t, js)

				logs := slogBuffer(t)
				cfg := daemonGrantConfig(uri)
				attempt, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				w := &workflowRuntime{project: cfg.Projects[cfg.Project], log: logs.logger}
				err := w.connect(attempt, cfg, natsConnection{seed: daemonSeed})
				require.Error(t, err)

				denied := "$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS." + dispatchGrantConsumer
				require.Eventually(t, func() bool {
					return strings.Contains(logs.String(), "NATS refused the daemon a permission") && strings.Contains(logs.String(), denied)
				}, 10*time.Second, 25*time.Millisecond, "the daemon log names the refused JetStream API publish")
				require.Eventually(t, func() bool {
					serverLog := daemonGrantServerLogs(t, server)
					return strings.Contains(serverLog, "Publish Violation") && strings.Contains(serverLog, denied)
				}, 10*time.Second, 25*time.Millisecond, "the server log names the refused JetStream API publish")
				daemonLine := matchingLogLine(logs.String(), "NATS refused the daemon a permission", denied)
				serverLine := matchingLogLine(daemonGrantServerLogs(t, server), "Publish Violation", denied)
				require.NotEmpty(t, daemonLine)
				require.NotEmpty(t, serverLine)
				t.Logf("observed daemon refusal: %s", daemonLine)
				t.Logf("observed server refusal: %s", serverLine)
			})
		})
	}
}

type grantLog struct {
	logger *slog.Logger
	buffer syncBuffer
}

func slogBuffer(t *testing.T) *grantLog {
	t.Helper()
	logs := &grantLog{}
	logs.logger = slog.New(slog.NewTextHandler(&logs.buffer, nil))
	return logs
}

func (l *grantLog) String() string { return l.buffer.String() }

func daemonGrantConfig(uri string) config.Config {
	const project = "CAPTURE"
	return config.Config{
		Project: project, NatsURLs: []string{uri}, WorkerRPCTimeout: time.Second,
		Projects: map[string]config.Project{project: {Repo: ghrepo.MustParse("sjawhar/legion")}},
	}
}

func connectDaemonGrant(t *testing.T, uri, seed string, logs *grantLog) *workflowRuntime {
	t.Helper()
	cfg := daemonGrantConfig(uri)
	w := &workflowRuntime{project: cfg.Projects[cfg.Project], log: logs.logger}
	require.NoError(t, w.connect(t.Context(), cfg, natsConnection{seed: seed}))
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
		require.NoError(t, <-done)
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
	require.NoError(t, err)
	var envelope struct {
		Topic string `json:"topic"`
	}
	require.NoError(t, json.Unmarshal(data, &envelope))
	_, err = js.Publish(t.Context(), envelope.Topic, data)
	require.NoError(t, err)
}

func requireDaemonGrantAcknowledged(t *testing.T, stream jetstream.Stream, consumers ...string) {
	t.Helper()
	require.Eventually(t, func() bool {
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
	}, 10*time.Second, 25*time.Millisecond, "the daemon acknowledges both retained event types")
}

func createDaemonNotificationStream(t *testing.T, js jetstream.JetStream) jetstream.Stream {
	t.Helper()
	stream, err := js.CreateStream(t.Context(), jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}})
	require.NoError(t, err)
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
	require.NoError(t, encoder.Encode(map[string]map[string][]string{
		"publish":   {"allow": grant.Publish},
		"subscribe": {"allow": grant.Subscribe},
	}))
	server, uri := testnats.StartNkeyAuthorizedUsers(t, image,
		testnats.NkeyUser{Public: daemonPublic, Permissions: strings.TrimSpace(permissions.String())},
		testnats.NkeyUser{Public: adminPublic})
	admin, err := natsauth.Connect([]string{uri}, adminSeed, nats.Timeout(5*time.Second))
	require.NoError(t, err)
	js, err := jetstream.New(admin)
	require.NoError(t, err)
	return daemonSeed, admin, js, server, uri
}

func daemonGrantServerLogs(t *testing.T, server *tcnats.NATSContainer) string {
	t.Helper()
	logs, err := server.Logs(context.Background())
	require.NoError(t, err)
	defer logs.Close()
	text, err := io.ReadAll(logs)
	require.NoError(t, err)
	return string(text)
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
