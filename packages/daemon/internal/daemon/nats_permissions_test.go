package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"os"
	"regexp"
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

// grantProject is the project the proof's daemon runs as.
const grantProject = "CAPTURE"

// The deployment and this proof share daemonpermissions.Grant: core wraps the same values as the
// NATS server grant, while this test drives the daemon's real connection, durable consumers,
// acknowledgements, and exceptions subscription through it, on each server image the hub runs.
// The server traces every protocol operation (-DV), and the subjects the daemon's connections
// published and subscribed to are read back from that trace, since nats.go, not Legion's code,
// chooses most of them.
func TestDaemonPermissionsOnRealServers(t *testing.T) {
	dispatchConsumer := daemonpermissions.DispatchConsumer(grantProject)
	githubConsumer := daemonpermissions.GitHubConsumer(grantProject)
	inbox := daemonpermissions.InboxPrefix(grantProject)
	for _, image := range testnatsimages.Images {
		t.Run(image.Name, func(t *testing.T) {
			t.Run("the daemon restarts, acknowledges Dispatch and GitHub, and receives exceptions", func(t *testing.T) {
				grant := daemonGrant(t)
				srv := startDaemonGrantServer(t, image.Ref, grant)
				stream := createDaemonNotificationStream(t, srv.js)

				first := connectDaemonGrant(t, srv.uri, srv.seed, &syncBuffer{})
				for _, name := range []string{dispatchConsumer, githubConsumer} {
					if _, err := stream.Consumer(t.Context(), name); err != nil {
						t.Fatalf("the first daemon creates %s: %v", name, err)
					}
				}
				first.conn.Close()

				logs := &syncBuffer{}
				restarted := connectDaemonGrant(t, srv.uri, srv.seed, logs)
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

				publishDaemonGrantFixture(t, srv.js, "../intake/testdata/dispatch/issue-updated.json")
				publishDaemonGrantFixture(t, srv.js, "../intake/testdata/github/pr-opened.json")
				if err := srv.admin.Publish("notifications.envoy.exceptions.notifications.role.example", []byte("retry this notice")); err != nil {
					t.Fatalf("publish the role-lane exception: %v", err)
				}
				if err := srv.admin.Flush(); err != nil {
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
				requireDaemonGrantAcknowledged(t, stream, dispatchConsumer, githubConsumer)
				if strings.Contains(logs.String(), "NATS refused the daemon a permission") {
					t.Fatalf("the daemon logged a permission refusal:\n%s", logs.String())
				}
				serverLog := daemonGrantServerLogs(t, srv.server)
				// The readiness probe's credential-free dial is refused with an Authorization Violation;
				// a refused permission is logged as a Publish or Subscription Violation.
				if strings.Contains(serverLog, "Publish Violation") || strings.Contains(serverLog, "Subscription Violation") {
					t.Fatalf("the server logged a permissions violation:\n%s", serverLog)
				}

				publish, subscribe := daemonTrace(serverLog)
				requireEveryGrantEntryUsed(t, "publish", grant.Publish, publish)
				requireEveryGrantEntryUsed(t, "subscribe", grant.Subscribe, subscribe)
				for _, subject := range subscribe {
					if !strings.HasPrefix(subject, inbox+".") && subject != daemonpermissions.RoleExceptions {
						t.Errorf("the daemon subscribed to %s, outside its own inbox and the role-lane exceptions", subject)
					}
				}
				t.Logf("server trace: the daemon published %s", strings.Join(traceSubjects(publish, inbox), ", "))
				t.Logf("server trace: the daemon subscribed to %s", strings.Join(traceSubjects(subscribe, inbox), ", "))
				t.Log("the first daemon created both consumers and the restarted one updated them, NAKed one delivery and acknowledged both event kinds, and received the role-lane exception without a permission refusal, using every entry of its grant")
			})

			// Each control removes one entry the daemon uses, and the daemon and the server then both
			// report the refusal of exactly that subject: the passing run above passes because the grant
			// holds that entry, not because the server enforces nothing.
			for _, control := range []struct {
				name, removed, violation, denied string
			}{
				{
					name:      "without the Dispatch consumer's create, the daemon and server report it",
					removed:   "$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS." + dispatchConsumer + "." + daemonpermissions.DispatchFilter,
					violation: "Publish Violation",
					denied:    "$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS." + dispatchConsumer + "." + daemonpermissions.DispatchFilter,
				},
				{
					name:      "without its own reply inbox, the daemon and server report it",
					removed:   inbox + ".>",
					violation: "Subscription Violation",
					denied:    inbox + ".",
				},
			} {
				t.Run(control.name, func(t *testing.T) {
					grant := daemonGrant(t)
					held := len(grant.Publish) + len(grant.Subscribe)
					drop := func(subject string) bool { return subject == control.removed }
					grant.Publish = slices.DeleteFunc(grant.Publish, drop)
					grant.Subscribe = slices.DeleteFunc(grant.Subscribe, drop)
					if len(grant.Publish)+len(grant.Subscribe) != held-1 {
						t.Fatalf("the daemon grant does not hold %s once: %v", control.removed, grant)
					}

					srv := startDaemonGrantServer(t, image.Ref, grant)
					createDaemonNotificationStream(t, srv.js)

					logs := &syncBuffer{}
					cfg := daemonGrantConfig(srv.uri)
					attempt, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					w := &workflowRuntime{project: cfg.Projects[cfg.Project], log: grantLogger(logs)}
					if err := w.connect(attempt, cfg, daemonGrantConnection(srv.seed)); err == nil {
						t.Fatalf("the daemon connected without %s", control.removed)
					}
					requireGrantRefusal(t, logs, srv.server, control.violation, control.denied)
				})
			}

			t.Run("the grant refuses another consumer's create and every other client's replies", func(t *testing.T) {
				srv := startDaemonGrantServer(t, image.Ref, daemonGrant(t))
				stream := createDaemonNotificationStream(t, srv.js)
				logs := &syncBuffer{}
				conn, err := natsauth.Connect([]string{srv.uri}, srv.seed, nats.Timeout(5*time.Second),
					natsauth.DaemonInbox(grantProject), natsauth.LogEvents(grantLogger(logs)))
				if err != nil {
					t.Fatalf("connect as the daemon user: %v", err)
				}
				defer conn.Close()
				js, err := jetstream.New(conn)
				if err != nil {
					t.Fatalf("open JetStream as the daemon user: %v", err)
				}

				for _, other := range []jetstream.ConsumerConfig{
					{Durable: daemonpermissions.DispatchConsumer("OTHER"), FilterSubject: daemonpermissions.DispatchFilter, AckPolicy: jetstream.AckExplicitPolicy},
					{Durable: "listener-example-host-a", AckPolicy: jetstream.AckExplicitPolicy},
				} {
					attempt, cancel := context.WithTimeout(t.Context(), 3*time.Second)
					_, err := js.CreateConsumer(attempt, "ENVOY_NOTIFICATIONS", other)
					cancel()
					if err == nil {
						t.Fatalf("the daemon user created %s", other.Durable)
					}
					denied := "$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS." + other.Durable
					if other.FilterSubject != "" {
						denied += "." + other.FilterSubject
					}
					requireGrantRefusal(t, logs, srv.server, "Publish Violation", denied)
					if _, err := stream.Consumer(t.Context(), other.Durable); !errors.Is(err, jetstream.ErrConsumerNotFound) {
						t.Fatalf("%s after the refused create: %v, want it absent", other.Durable, err)
					}
				}

				if _, err := conn.SubscribeSync(nats.InboxPrefix + ">"); err != nil {
					t.Fatalf("send the shared-inbox subscription: %v", err)
				}
				if err := conn.Flush(); err != nil {
					t.Fatalf("flush the shared-inbox subscription: %v", err)
				}
				requireGrantRefusal(t, logs, srv.server, "Subscription Violation", `"`+nats.InboxPrefix+`>"`)
			})
		})
	}
}

// daemonGrant is the grant of grantProject's daemon.
func daemonGrant(t *testing.T) daemonpermissions.Permissions {
	t.Helper()
	grant, err := daemonpermissions.Grant(grantProject)
	if err != nil {
		t.Fatalf("the grant of %s: %v", grantProject, err)
	}
	return grant
}

// daemonGrantConnection is the daemon's connection as its own NATS user, the one the grant is for.
func daemonGrantConnection(seed string) natsConnection {
	return natsConnection{seed: seed, source: natsSeedDaemon}
}

// requireGrantRefusal waits for the daemon's log and the server's to name the refusal of denied,
// the server's in a line with violation, and logs both lines.
func requireGrantRefusal(t *testing.T, logs *syncBuffer, server *tcnats.NATSContainer, violation, denied string) {
	t.Helper()
	var daemonLine, serverLine string
	testwait.Eventually(t, "the daemon log naming the refusal of "+denied, func() bool {
		daemonLine = matchingLogLine(logs.String(), "NATS refused the daemon a permission", strings.Trim(denied, `"`))
		return daemonLine != ""
	})
	var serverLogErr error
	testwait.Eventually(t, "the server log naming the refusal of "+denied, func() bool {
		var serverLog string
		serverLog, serverLogErr = readDaemonGrantServerLogs(server)
		serverLine = matchingLogLine(serverLog, violation, denied)
		return serverLine != ""
	})
	if serverLogErr != nil {
		t.Fatalf("read the server log: %v", serverLogErr)
	}
	t.Logf("observed daemon refusal: %s", daemonLine)
	t.Logf("observed server refusal: %s", strings.TrimSpace(serverLine))
}

// traceOperation is one client protocol operation the server traced: the client's connection id and
// the operation as it arrived.
var traceOperation = regexp.MustCompile(`cid:(\d+) .*<<- \[(.*)\]`)

// grantAdminName is the admin connection's name, which its CONNECT carries in the server's trace.
const grantAdminName = "daemon-grant-admin"

// daemonTrace is every subject the daemon's connections published to and subscribed to, in the
// order the server's trace shows them. A daemon connection is one whose CONNECT carries an nkey
// and is not the admin's: the hub image redacts the nkey in its trace, so the user is told by the
// admin connection's name, and the readiness probe's credential-free dial carries no nkey.
func daemonTrace(serverLog string) (publish, subscribe []string) {
	lines := strings.Split(serverLog, "\n")
	daemon := map[string]bool{}
	for _, line := range lines {
		m := traceOperation.FindStringSubmatch(line)
		if m != nil && strings.HasPrefix(m[2], "CONNECT ") && strings.Contains(m[2], `"nkey":`) &&
			!strings.Contains(m[2], `"name":"`+grantAdminName+`"`) {
			daemon[m[1]] = true
		}
	}
	for _, line := range lines {
		m := traceOperation.FindStringSubmatch(line)
		if m == nil || !daemon[m[1]] {
			continue
		}
		switch fields := strings.Fields(m[2]); fields[0] {
		case "PUB", "HPUB":
			publish = append(publish, fields[1])
		case "SUB":
			subscribe = append(subscribe, fields[1])
		}
	}
	return publish, subscribe
}

// requireEveryGrantEntryUsed fails unless each entry of allow admits at least one of subjects: no
// entry of the grant is one the daemon does not use.
func requireEveryGrantEntryUsed(t *testing.T, direction string, allow, subjects []string) {
	t.Helper()
	for _, entry := range allow {
		if !slices.ContainsFunc(subjects, func(subject string) bool { return subjectMatches(entry, subject) }) {
			t.Errorf("%s entry %s admits nothing the daemon sent: %v", direction, entry, subjects)
		}
	}
}

// subjectMatches reports whether pattern, a permission entry, admits subject token by token: `*`
// admits one token, a trailing `>` one or more.
func subjectMatches(pattern, subject string) bool {
	want, got := strings.Split(pattern, "."), strings.Split(subject, ".")
	for i, token := range want {
		if token == ">" {
			return len(got) > i
		}
		if i >= len(got) || (token != "*" && token != got[i]) {
			return false
		}
	}
	return len(got) == len(want)
}

// traceSubjects is subjects without repeats, sorted, with what varies per call written as a
// placeholder: the unique token of a reply inbox, and an acknowledgement's delivery tokens.
func traceSubjects(subjects []string, inbox string) []string {
	seen := map[string]bool{}
	for _, subject := range subjects {
		if rest, ok := strings.CutPrefix(subject, inbox+"."); ok {
			_, after, nested := strings.Cut(rest, ".")
			subject = inbox + ".<nuid>"
			if nested {
				subject += "." + after
			}
		} else if tokens := strings.Split(subject, "."); tokens[0] == "$JS" && tokens[1] == "ACK" && len(tokens) > 4 {
			subject = strings.Join(tokens[:4], ".") + ".<delivery>"
		}
		seen[subject] = true
	}
	return slices.Sorted(maps.Keys(seen))
}

func grantLogger(logs *syncBuffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(logs, nil))
}

func daemonGrantConfig(uri string) config.Config {
	return config.Config{
		Project: grantProject, NatsURLs: []string{uri}, WorkerRPCTimeout: time.Second,
		Projects: map[string]config.Project{grantProject: {Repo: ghrepo.MustParse("sjawhar/legion")}},
	}
}

func connectDaemonGrant(t *testing.T, uri, seed string, logs *syncBuffer) *workflowRuntime {
	t.Helper()
	cfg := daemonGrantConfig(uri)
	w := &workflowRuntime{project: cfg.Projects[cfg.Project], log: grantLogger(logs)}
	if err := w.connect(t.Context(), cfg, daemonGrantConnection(seed)); err != nil {
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

// daemonGrantServer is a NATS server admitting the daemon user under one grant and an admin user
// granted everything: the daemon user's seed and public key, the admin's connection and JetStream,
// the server's container (its log is the trace) and its client URL.
type daemonGrantServer struct {
	seed   string
	admin  *nats.Conn
	js     jetstream.JetStream
	server *tcnats.NATSContainer
	uri    string
}

func startDaemonGrantServer(t *testing.T, image string, grant daemonpermissions.Permissions) daemonGrantServer {
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
	admin, err := natsauth.Connect([]string{uri}, adminSeed, nats.Timeout(5*time.Second), nats.Name(grantAdminName))
	if err != nil {
		t.Fatalf("connect the admin user: %v", err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatalf("open JetStream as the admin user: %v", err)
	}
	return daemonGrantServer{seed: daemonSeed, admin: admin, js: js, server: server, uri: uri}
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
