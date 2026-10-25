package testnats

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

// This protects the rig's readiness boundary: a server can accept NATS's
// protocol handshake while JetStream is still starting, so a KV test that returns at the handshake
// races CreateKeyValue. The helper must retry the JetStream operation itself.
func TestWaitForJetStreamRetriesTheOperationUntilItIsReady(t *testing.T) {
	attempts := 0
	waitForJetStream(t, func() error {
		attempts++
		if attempts < 3 {
			return errors.New("JetStream is starting")
		}
		return nil
	})
	if attempts != 3 {
		t.Fatalf("JetStream readiness attempts = %d, want 3", attempts)
	}
}

// A create right after a stream delete can meet nats-server still removing the account's streams
// directory, and is answered with the store failure; RecreateKeyValue tries it again. Every other
// answer is the test's own failure and must not be retried into a slower one or a pass.
func TestRetryStreamStoreFailureRetriesOnlyTheStoreFailure(t *testing.T) {
	storeFailure := &natsgo.APIError{Code: 500, ErrorCode: streamCreateErrorCode, Description: streamStoreFailed}
	t.Run("store failure", func(t *testing.T) {
		attempts := 0
		err := retryStreamStoreFailure(func() error {
			attempts++
			if attempts < 3 {
				return storeFailure
			}
			return nil
		})
		if err != nil || attempts != 3 {
			t.Fatalf("err = %v after %d attempts, want nil after 3", err, attempts)
		}
	})
	t.Run("any other error", func(t *testing.T) {
		other := &natsgo.APIError{Code: 500, ErrorCode: streamCreateErrorCode, Description: "insufficient storage resources available"}
		attempts := 0
		err := retryStreamStoreFailure(func() error {
			attempts++
			return other
		})
		if err != other || attempts != 1 {
			t.Fatalf("err = %v after %d attempts, want %v after 1", err, attempts, other)
		}
	})
}

// The package's tests share one server, and each gets it as a fresh container was: a stream and its
// messages left by one test are not there when the next asks for the server, which hands it an
// account of its own.
func TestEachTestGetsTheSharedServerEmpty(t *testing.T) {
	t.Run("leaves a stream and a message", func(t *testing.T) {
		conn := Connect(t, URL(t))
		defer conn.Close()
		js, err := conn.JetStream()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := js.AddStream(&natsgo.StreamConfig{Name: "LEFT_BEHIND", Subjects: []string{"left.>"}}); err != nil {
			t.Fatalf("add stream: %v", err)
		}
		if _, err := js.Publish("left.one", []byte("x")); err != nil {
			t.Fatalf("publish: %v", err)
		}
	})
	t.Run("finds none of it", func(t *testing.T) {
		conn := Connect(t, URL(t))
		defer conn.Close()
		js, err := conn.JetStream()
		if err != nil {
			t.Fatal(err)
		}
		info, err := js.AccountInfo()
		if err != nil {
			t.Fatalf("account info: %v", err)
		}
		if info.Streams != 0 {
			t.Errorf("the second subtest's account holds %d streams, want none of the first's", info.Streams)
		}
	})
}

// A test that publishes a burst without waiting for acks leaves messages the server routes after
// the test ends. They land in that test's own account, so the next test, creating a stream on the
// same subjects, finds none of them.
func TestABurstLeftInFlightDoesNotReachTheNextTest(t *testing.T) {
	const burst = 20000
	config := &natsgo.StreamConfig{Name: "BURST", Subjects: []string{"burst.>"}}
	t.Run("publishes a burst and ends", func(t *testing.T) {
		conn := Connect(t, URL(t))
		defer conn.Close()
		js, err := conn.JetStream()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := js.AddStream(config); err != nil {
			t.Fatalf("add stream: %v", err)
		}
		payload := []byte(strings.Repeat("x", 64))
		for i := range burst {
			if err := conn.Publish(fmt.Sprintf("burst.%d", i%16), payload); err != nil {
				t.Fatalf("publish %d: %v", i, err)
			}
		}
	})
	t.Run("recreates the stream and finds none of it", func(t *testing.T) {
		conn := Connect(t, URL(t))
		defer conn.Close()
		js, err := conn.JetStream()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := js.AddStream(config); err != nil {
			t.Fatalf("add stream: %v", err)
		}
		time.Sleep(time.Second)
		info, err := js.StreamInfo(config.Name)
		if err != nil {
			t.Fatalf("stream info: %v", err)
		}
		if info.State.Msgs != 0 {
			t.Errorf("the recreated stream holds %d of the previous test's %d messages", info.State.Msgs, burst)
		}
	})
}

// fatalRecorder is t with Fatal recorded and the goroutine ended, as t.Fatal does.
type fatalRecorder struct {
	testing.TB
	message string
}

func (f *fatalRecorder) Fatal(args ...any) { f.message = fmt.Sprint(args...); runtime.Goexit() }
func (f *fatalRecorder) Fatalf(format string, args ...any) {
	f.message = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// A test gets one account however often it asks, so every connection it makes reaches the streams
// any other made; a subtest gets an account of its own, where none of its parent's streams is.
func TestATestGetsOneAccountAndEachSubtestItsOwn(t *testing.T) {
	uri := URL(t)
	if again := URL(t); again != uri {
		t.Fatalf("a second URL for the same test is %q, want its first, %q", again, uri)
	}
	conn := Connect(t, uri)
	defer conn.Close()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.AddStream(&natsgo.StreamConfig{Name: "PARENT", Subjects: []string{"parent.>"}}); err != nil {
		t.Fatalf("add stream: %v", err)
	}
	t.Run("subtest", func(t *testing.T) {
		subURI := URL(t)
		if subURI == uri {
			t.Fatalf("the subtest got its parent's account, %q", uri)
		}
		sub := Connect(t, subURI)
		defer sub.Close()
		subJS, err := sub.JetStream()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := subJS.StreamInfo("PARENT"); !errors.Is(err, natsgo.ErrStreamNotFound) {
			t.Fatalf("the subtest's account answers its parent's stream with %v, want not found", err)
		}
	})
}

// namedTB is a test under another name, so one test can ask a server for accounts as many tests.
type namedTB struct {
	testing.TB
	name string
}

func (n namedTB) Name() string { return n.name }

// ownAccountServer starts an account server of the test's own, declaring accounts accounts, which
// is shut down and removed when the test ends.
func ownAccountServer(t *testing.T, accounts int) *accountServer {
	t.Helper()
	return startAccountServer(t, accounts)
}

// A server whose accounts are all handed out declares twice as many by a reload and hands out the
// next, and an account a test is still using keeps its connection and its watcher across the
// reload.
func TestAServerOutOfAccountsDoublesThemWithoutDisturbingOneInUse(t *testing.T) {
	server := ownAccountServer(t, 2)
	live := Connect(t, server.url(namedTB{t, "live"}))
	defer live.Close()
	js, err := live.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: "live"})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	watcher, err := kv.WatchAll()
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer func() { _ = watcher.Stop() }()
	if entry := <-watcher.Updates(); entry != nil {
		t.Fatalf("the empty bucket's scan delivered %v before its marker", entry)
	}

	server.url(namedTB{t, "second"})
	third := server.url(namedTB{t, "third"})
	if server.declared != 4 {
		t.Fatalf("the server declares %d accounts after handing out its third, want 4", server.declared)
	}
	if want := fmt.Sprintf("nats://t3:t3@%s", server.host); third != want {
		t.Fatalf("the third test's URL is %q, want %q", third, want)
	}
	if _, err := kv.Put("after-the-reload", []byte("1")); err != nil {
		t.Fatalf("put after the reload: %v", err)
	}
	select {
	case entry := <-watcher.Updates():
		if entry == nil || entry.Key() != "after-the-reload" {
			t.Fatalf("the watcher delivered %v after the reload, want the put", entry)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the watcher of an account in use delivered nothing after the reload")
	}
	if status := live.Status(); status != natsgo.CONNECTED {
		t.Fatalf("the connection of an account in use is %v after the reload, want CONNECTED", status)
	}
}

// StartNkeyAuthorized's readiness wait refuses a server that admits a client with no credential:
// such a server enforces no nkey users, and every refusal a test expects of it would pass for the
// wrong reason. A server started with no configuration, which asks for no credential, stands in
// for one.
func TestTheNkeyReadinessWaitRefusesAServerThatAdmitsAnyone(t *testing.T) {
	_, uri := Start(t)
	wait := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		awaitAuth(wait, uri, false)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the readiness wait is still waiting on a server that admits anyone")
	}
	if want := "admitted a client with no credential"; !strings.Contains(wait.message, want) {
		t.Errorf("the readiness wait ended with %q, want it to say %q", wait.message, want)
	}
}

// A restart keeps its server's URL even when something else takes the URL's port while the server is
// stopped, as another test's server or a free-port pick can on a busy host.
func TestARestartKeepsItsURLWhenThePortIsContendedWhileStopped(t *testing.T) {
	server, uri := Start(t)
	server.Stop(t)
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse %s: %v", uri, err)
	}
	if taken, err := net.Listen("tcp", u.Host); err == nil {
		t.Cleanup(func() { _ = taken.Close() })
	}
	server.Start(t)
	Connect(t, uri).Close()
}

// Server.Log is everything the server has said, which a test reads to see the server's own account
// of what it did: a server stopped and started again says it is ready twice, once for each server
// that ran behind the one URL.
func TestTheLogHoldsWhatTheServerSaidAcrossARestart(t *testing.T) {
	server, _ := Start(t)
	server.Stop(t)
	server.Start(t)
	if ready := strings.Count(server.Log(), "Server is ready"); ready != 2 {
		t.Errorf("the server said it was ready %d times across a stop and a start, want twice:\n%s", ready, server.Log())
	}
}

// A gate's port must lie below the machine's ephemeral range, where no :0 bind can take it while
// the gate is closed. The range is fed in as text, each kernel's answer in turn, so the band is
// pinned whatever range the machine running this test has: a CI runner's default range would hide
// a band that lands inside the 16000-65535 a Legion worker pod (gVisor) has.
func TestTheGateBandLiesBelowTheEphemeralRangeItIsGiven(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		low   int
		first int
	}{
		{name: "gVisor's range in a Legion worker pod", text: "16000\t65535\n", low: 16000, first: 15000},
		{name: "Linux's default range", text: "32768\t60999\n", low: 32768, first: 31768},
		{name: "text that is no range", text: "not a range\n", low: 16000, first: 15000},
		{name: "empty text", text: "", low: 16000, first: 15000},
		{name: "the lowest range that leaves a band", text: "3048\t65535\n", low: 3048, first: 2048},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, count, err := gatePorts(tc.text)
			if err != nil {
				t.Fatalf("gatePorts(%q) = %v, want a band", tc.text, err)
			}
			if first != tc.first || count != 1000 {
				t.Errorf("gatePorts(%q) = %d ports from %d, want 1000 from %d", tc.text, count, first, tc.first)
			}
			if last := first + count - 1; last >= tc.low {
				t.Errorf("gatePorts(%q) = %d-%d, which reaches the range starting at %d", tc.text, first, last, tc.low)
			}
		})
	}
	t.Run("a range too low to leave a band", func(t *testing.T) {
		first, count, err := gatePorts("1024\t65535\n")
		if err == nil {
			t.Fatalf("gatePorts = %d ports from %d, want a refusal", count, first)
		}
		if !strings.Contains(err.Error(), "1024") {
			t.Errorf("refusal %v does not name the range's first port, 1024", err)
		}
	})
}

// StartGate takes its port from just below this machine's ephemeral range, which it reads from
// ephemeralRangePath. The test reads that file itself, as the rig does, and checks the ports of 20
// gates against it: a StartGate that stopped reading the file and took a band of its own fails
// here on every machine whose range does not start where that band ends.
func TestEveryGatePortLiesJustBelowThisMachinesEphemeralRange(t *testing.T) {
	low := fallbackEphemeralLow
	text, err := os.ReadFile(ephemeralRangePath)
	if fields := strings.Fields(string(text)); err == nil && len(fields) == 2 {
		first, firstErr := strconv.Atoi(fields[0])
		last, lastErr := strconv.Atoi(fields[1])
		if firstErr == nil && lastErr == nil && 0 < first && first <= last && last <= 65535 {
			low = first
		}
	}
	server, _ := Start(t)
	for range 20 {
		gate := StartGate(t, server)
		address, err := url.Parse(gate.URL())
		if err != nil {
			t.Fatalf("parse the gate's URL %s: %v", gate.URL(), err)
		}
		port, err := strconv.Atoi(address.Port())
		if err != nil {
			t.Fatalf("read the port of the gate's URL %s: %v", gate.URL(), err)
		}
		if port >= low || port < low-gatePortCount {
			t.Errorf("a gate took port %d, want one of the %d ports below %d, the first port of this machine's ephemeral range (%s holds %q)", port, gatePortCount, low, ephemeralRangePath, text)
		}
	}
}

// Terminate ends the server for good and gives up the port its URL named, so nothing answers there
// any more; the cleanup that runs at the test's end finds the work done and does it no second time.
func TestTerminateEndsTheServerAndGivesUpItsPort(t *testing.T) {
	server, uri := Start(t)
	Connect(t, uri).Close()
	server.Terminate(t)
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse %s: %v", uri, err)
	}
	if conn, err := net.DialTimeout("tcp", u.Host, dialTimeout); err == nil {
		conn.Close()
		t.Fatalf("%s still answers after the server was terminated", u.Host)
	}
}

// MonitorURL is where the server's /varz and /connz are, which a test reads to see what the server
// makes of its own state.
func TestTheMonitorURLAnswersVarz(t *testing.T) {
	server, _ := Start(t)
	response, err := http.Get(server.MonitorURL() + "/varz")
	if err != nil {
		t.Fatalf("GET %s/varz: %v", server.MonitorURL(), err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("GET %s/varz: %s, want 200 OK", server.MonitorURL(), response.Status)
	}
}

// A second Grant on one server judges the outcome of its own reload, not the first Grant's: a
// second grant the server refuses fails the test with the server's reason for that grant, which
// comes back from that Grant's own reload.
func TestASecondGrantWaitsForItsOwnReload(t *testing.T) {
	_, public := User(t)
	grant := StartNkeyGranted(t, public, "first.>")
	grant.Grant(t, "second.>")

	refused := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		grant.Grant(refused, "not..a.subject")
	}()
	<-done
	if !strings.Contains(refused.message, "NATS refused the new grant") ||
		!strings.Contains(refused.message, `subject "not..a.subject" is not a valid subject`) {
		t.Fatalf("a second Grant the server refused ended with %q, want the server's refusal", refused.message)
	}
}

// Server.Reload answers with the server's own reason for a configuration the server will not take,
// and the server goes on serving the configuration it had: its user is still admitted and a client
// with no credential still refused.
func TestARefusedReloadAnswersWithTheServersReasonAndKeepsTheOldConfiguration(t *testing.T) {
	seed, public := User(t)
	server, uri := StartConfig(t, publishGrantConfig(public, []string{"first.>"}), []string{"-js"}, false)

	err := server.Reload(t, publishGrantConfig(public, []string{"not..a.subject"}))
	if err == nil {
		t.Fatal("the server took a configuration granting an invalid subject")
	}
	if want := `subject "not..a.subject" is not a valid subject`; !strings.Contains(err.Error(), want) {
		t.Fatalf("the refusal is %v, want it to say %s", err, want)
	}

	conn, err := ConnectSeed(t, uri, seed)
	if err != nil {
		t.Fatalf("the user of the configuration the server kept was refused: %v", err)
	}
	defer conn.Close()
	if err := conn.Publish("first.one", nil); err != nil {
		t.Fatalf("publish to the subject the kept grant allows: %v", err)
	}
	if _, err := ConnectSeed(t, uri, ""); !errors.Is(err, natsgo.ErrAuthorization) {
		t.Fatalf("a client with no credential after the refused reload: %v, want %v", err, natsgo.ErrAuthorization)
	}
}

// StartConfig gives the server the arguments it was given and no others, as nats-server's own
// command line does: -DV puts the protocol trace in its log, and without -js among them the server
// has no JetStream, whatever a server started by another helper has.
func TestStartConfigRunsTheArgumentsItWasGivenAndNoOther(t *testing.T) {
	seed, public := User(t)
	config := fmt.Sprintf("authorization {\n  users: [ { nkey: %q } ]\n}\n", public)
	server, uri := StartConfig(t, config, []string{"-DV"}, false)
	conn, err := ConnectSeed(t, uri, seed)
	if err != nil {
		t.Fatalf("connect with the seed: %v", err)
	}
	defer conn.Close()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	if _, err := js.AccountInfo(); !errors.Is(err, natsgo.ErrJetStreamNotEnabled) {
		t.Fatalf("JetStream account info on a server given no -js: %v, want %v", err, natsgo.ErrJetStreamNotEnabled)
	}
	if !strings.Contains(server.Log(), "[TRC]") {
		t.Errorf("the log of a server given -DV holds no protocol trace:\n%s", server.Log())
	}
}

// A frozen server stops the bytes in either direction without dropping anything: the request times
// out while its connection is still CONNECTED, as a server that has stopped answering leaves it,
// and the traffic resumes when the test thaws it.
func TestAFrozenServerTimesOutARequestWithoutDroppingTheConnection(t *testing.T) {
	server, uri := Start(t)
	conn := Connect(t, uri)
	defer conn.Close()
	sub, err := conn.Subscribe("echo", func(msg *natsgo.Msg) { _ = msg.Respond([]byte("answer")) })
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if _, err := conn.Request("echo", nil, connectTimeout); err != nil {
		t.Fatalf("request before the freeze: %v", err)
	}

	thaw := server.Freeze(t)
	if _, err := conn.Request("echo", nil, time.Second); !errors.Is(err, natsgo.ErrTimeout) {
		t.Fatalf("a request to a frozen server: %v, want %v", err, natsgo.ErrTimeout)
	}
	if status := conn.Status(); status != natsgo.CONNECTED {
		t.Fatalf("the connection to a frozen server is %v, want CONNECTED", status)
	}

	thaw()
	answer, err := conn.Request("echo", nil, connectTimeout)
	if err != nil {
		t.Fatalf("request after the thaw: %v", err)
	}
	if string(answer.Data) != "answer" {
		t.Errorf("the answer after the thaw is %q, want %q", answer.Data, "answer")
	}
}

// Two freezes may overlap, and each thaw may be called more than once: neither ends the test
// binary (a second close of one gate panicked it once), a thaw left over from an earlier freeze
// does not end a later one, and the traffic resumes once every thaw has run.
func TestOverlappingFreezesThawInEitherOrderWithoutPanicking(t *testing.T) {
	for _, secondFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("second thawed first %v", secondFirst), func(t *testing.T) {
			server, uri := Start(t)
			conn := Connect(t, uri)
			defer conn.Close()
			sub, err := conn.Subscribe("echo", func(msg *natsgo.Msg) { _ = msg.Respond([]byte("answer")) })
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			defer func() { _ = sub.Unsubscribe() }()

			first, second := server.Freeze(t), server.Freeze(t)
			if secondFirst {
				first, second = second, first
			}
			first()
			second()
			first()
			second()
			if _, err := conn.Request("echo", nil, connectTimeout); err != nil {
				t.Fatalf("request after every thaw: %v", err)
			}

			later := server.Freeze(t)
			first()
			if _, err := conn.Request("echo", nil, time.Second); !errors.Is(err, natsgo.ErrTimeout) {
				t.Fatalf("a request after a stale thaw of an earlier freeze: %v, want %v", err, natsgo.ErrTimeout)
			}
			later()
			if _, err := conn.Request("echo", nil, connectTimeout); err != nil {
				t.Fatalf("request after the later thaw: %v", err)
			}
		})
	}
}

// The anonymous wait ends at once, failing the test, on a server that refuses a client with no
// credential: that server is up and has no no_auth_user, so waiting out the deadline would only
// report the same refusal later.
func TestTheAnonymousWaitRefusesAServerThatRefusesAnyone(t *testing.T) {
	_, public := User(t)
	uri := StartNkeyAuthorized(t, public)
	wait := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		awaitAuth(wait, uri, true)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the anonymous wait is still waiting on a server that refuses anyone")
	}
	if want := "refused a client with no credential"; !strings.Contains(wait.message, want) {
		t.Errorf("the anonymous wait ended with %q, want it to say %q", wait.message, want)
	}
}

// envoyRoot is the module's directory, packages/envoy, from this package's own.
const envoyRoot = "../.."

// thisPackage is the natslive half of this rig, in image.go and image_test.go, which an untagged
// build does not see at all.
const thisPackage = "github.com/sjawhar/envoy/internal/testnats"

// containerRigs are the packages of this module that may reach testcontainers: this rig, and the
// two whose containers are not NATS and are not this issue's (internal/testpg runs pgvector,
// internal/tests3 SeaweedFS). Every other package is a finding, whichever testcontainers package
// it imports: modules/nats is the obvious way to start a NATS container, and
// testcontainers.Run(ctx, testnats.Image) is the one that needs no such import.
var containerRigs = []string{
	thisPackage,
	"github.com/sjawhar/envoy/internal/testpg",
	"github.com/sjawhar/envoy/internal/tests3",
}

// containerModule is the import prefix every testcontainers package shares.
const containerModule = "github.com/testcontainers/testcontainers-go"

// dockerDriver matches a file of this module driving a container runtime by hand: exec's run with
// docker as the command, however the call spells its context argument, and testcontainers' own
// Docker client. The context argument runs to the end of the line rather than to the first ')',
// since the module already passes context.Background() inline and a test's own is t.Context().
// It is written from pieces so that this file, which has to name them, is not itself a finding.
var dockerDriver = regexp.MustCompile(
	`exec\.Command(Context)?\([^\n]*` + `"docker"` + `|NewDockerClient` + `WithOpts`)

// No package of this module outside the rigs above may start a container: Envoy's agents have no
// container runtime, and a test that needs one cannot run where they do. This pins the two ways
// to get one — an import of testcontainers, and a hand-driven docker — against the packages
// allowed to have it. internal/smoke's containers are behind the smoke build tag and the rig's
// own behind natslive, so an untagged go list never sees their imports. The walk does not go by
// tag: it reads every .go file, those included, and passes them because none drives docker by
// hand.
func TestNoPackageOutsideTestnatsNeedsAContainerRuntime(t *testing.T) {
	t.Run("imports", func(t *testing.T) {
		for _, finding := range containerImporters(t) {
			t.Errorf("package %s imports %s, which belongs to a container runtime; a test that needs a NATS server starts it with testnats.Start, StartConfig or URL", finding.pkg, finding.imported)
		}
	})
	t.Run("docker", func(t *testing.T) {
		for _, file := range dockerDriverFiles(t) {
			t.Errorf("%s drives a container runtime itself; a test that has to stop a server's traffic uses testnats.Server.Freeze, and one that has to stop the server, Server.Stop", file)
		}
	})
}

// The file walk's matcher recognises a hand-driven docker in every way a test of this module spells
// the call, the context argument included: the module already passes context.Background() inline,
// and t.Context() is the test's own. Each line is assembled from pieces so this file, which the walk
// also reads, is not a finding itself.
func TestTheDockerDriverMatchCatchesEveryContextArgument(t *testing.T) {
	const run, docker = "exec." + "Command", `"dock` + `er"`
	for _, line := range []string{
		run + "(" + docker + `, "pause", id)`,
		run + "Context(ctx, " + docker + `, "pause", id)`,
		run + "Context(context.Background(), " + docker + `, "pause", id)`,
		run + "Context(t.Context(), " + docker + `, "unpause", id)`,
		"client.NewDocker" + "ClientWithOpts(client.FromEnv)",
	} {
		if !dockerDriver.MatchString(line) {
			t.Errorf("the docker-driver match misses %s", line)
		}
	}
	if line := run + `("go", "list", "./...")`; dockerDriver.MatchString(line) {
		t.Errorf("the docker-driver match flags %s, which runs no container", line)
	}
}

// containerImport is a package of this module importing a testcontainers package, and the import
// it was found by: a package reaching only testcontainers-go/wait has imported neither a runtime
// client nor modules/nats, and the finding has to say which it did import.
type containerImport struct{ pkg, imported string }

// containerImporters lists every package of this module, outside the rigs allowed one, that
// imports a testcontainers package, from its own sources or its tests', with the first such
// import of each.
func containerImporters(t *testing.T) []containerImport {
	t.Helper()
	const format = `{{.ImportPath}} {{join .Imports " "}} {{join .TestImports " "}} {{join .XTestImports " "}}`
	list := exec.Command("go", "list", "-f", format, "./...")
	list.Dir = envoyRoot
	out, err := list.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("go list ./...: %v: %s", err, exit.Stderr)
		}
		t.Fatalf("go list ./...: %v", err)
	}
	var importers []containerImport
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || slices.Contains(containerRigs, fields[0]) {
			continue
		}
		for _, imported := range fields[1:] {
			if imported == containerModule || strings.HasPrefix(imported, containerModule+"/") {
				importers = append(importers, containerImport{pkg: fields[0], imported: imported})
				break
			}
		}
	}
	return importers
}

// dockerDriverFiles lists every Go file under packages/envoy that drives a container runtime
// itself, by its path from there. The nested infra module is left out: its pulumilive lane runs a
// real stack of containers on a host that has a runtime, which is the one place in this tree that
// legitimately drives Docker.
func dockerDriverFiles(t *testing.T) []string {
	t.Helper()
	var drivers []string
	err := filepath.WalkDir(envoyRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path == filepath.Join(envoyRoot, "infra") || entry.Name() == "node_modules" || entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		text, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if dockerDriver.Match(text) {
			relative, err := filepath.Rel(envoyRoot, path)
			if err != nil {
				return err
			}
			drivers = append(drivers, filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", envoyRoot, err)
	}
	return drivers
}
