//go:build multiprocess

// Two secrets-broker processes serving one Postgres, which is what the broker's Deployment runs
// once it carries more than one replica. Every row here starts real cmd/broker binaries and
// drives them over real HTTP, because each thing it checks is a property of whole processes and
// of nothing a handler driven in-process can show: one refresh token redeemed against both at
// the same moment, one device code exchanged against both at the same moment, two processes
// migrating one empty database at the same moment, a retag one process is told to reread being
// enforced by the other within seconds, and that fan-out spending no rate-limit token on the pod
// it reaches.
//
// The last row takes one process rather than a pair, and says so: what a pod does between its
// SIGTERM and its exit — failing its health check while it goes on serving every other route,
// and finishing a request held in flight past the ten-second shutdown budget the broker used to
// have — is a property of one process and of a signal only a real one can be sent.
//
// TestAFixedSessionNameAcrossARollout starts its processes one at a time rather than as a pair,
// each with a configuration of its own, because what it drives is a rollout: two generations of
// the broker, whose configurations fix one session name for different entries, serving one
// database at once. Which process's configuration decides who holds that name is a property of
// processes that start and reserve in an order, which no single process can show.
//
// Its own build tag, and its own CI job, for the reason cmd/dispatch/multiprocess_test.go has
// its: a row here builds a binary, starts two processes, and waits for Postgres to settle a race
// between them, tens of seconds each, which go test's default per-package deadline in the unit
// lane cannot hold.
//
// Both processes of a pair carry one BROKER_PUBLIC_URL, and each test client is pinned to one
// process's real bound address while every URL it builds, signs and sends names that one public
// URL. That is not a shortcut: the broker authenticates against the URL it believes it is
// published at (api/server.go's authenticate, api/handlers_oauth.go's atPublicURL), so in
// production both pods carry their Service's URL and a load balancer picks the pod. A client per
// process, all naming the one public URL, is that load balancer — and it is the only shape in
// which a device code or a refresh token minted through one process can be presented to the
// other at all. See brokertest.NewClient.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	// multiprocessPublicURL is the one BROKER_PUBLIC_URL both processes of a pair carry: their
	// Service's name in production, and here a name under the reserved .test domain, which
	// resolves nowhere, so a client that ever reached the network for it would fail loudly
	// instead of quietly talking to a host nobody chose.
	multiprocessPublicURL = "http://broker.test"
	// multiprocessPerson owns the fixture secret and is every machine login's person: the same
	// one on both processes, since the database they share is what knows them.
	multiprocessPerson = "ada@example.com"
	// multiprocessSecret is the agent-tier secret multiprocessPerson owns, which their own
	// sessions are granted without any approval — enough to prove a session enrolled through one
	// process is served by the other.
	multiprocessSecret      = "AUTO_READ_TOKEN"
	multiprocessSecretSlug  = "auto-read-token"
	multiprocessSecretValue = "auto-read-token-v1"
	// multiprocessNewOwner is who a retag narrows multiprocessSecret to: a person no session
	// here is, so once the retag is in force the owner's own session is "another person's
	// session" to the policy and is answered with an approval request to this person rather
	// than a grant (internal/broker/policy's own table).
	multiprocessNewOwner = "grace@example.com"
)

// migrateTimeout bounds how long a process of a pair gets to log "broker listening". It is far
// past listenTimeout because the two processes start together against an unmigrated database and
// one of them waits on the other's migration advisory lock (store.go's pg_advisory_xact_lock),
// a wait that is unbounded by design.
const migrateTimeout = 2 * time.Minute

// fakeSecretsJSON is the secret store every broker here is started over: BROKER_FAKE_SECRETS_FILE's
// shape (cmd/broker/main.go's own note on it) holding the one agent-tier secret, owned by owner.
// A row that retags the secret writes this same function's output with another owner, so what it
// writes differs from what the processes booted on in the owner tag and nothing else — which is
// what a retag in the real store is.
//
// Its prefix and its key are main_test.go's own constants, the ones productionEnv starts every
// broker here with: the policy loader refuses a secret under another prefix or on another key, a
// refusal that would show up as nothing but this one secret never being served, so the fixture
// and the configuration name one value rather than two that must agree.
func fakeSecretsJSON(owner string) string {
	return `{"secrets": [
		{"name": "` + testSecretsPrefix + multiprocessSecretSlug + `", "kms_key_id": "` + testSecretsKMSKeyARN + `", "tags": {"owner": "` + owner + `", "tier": "agent"}, "value": "` + multiprocessSecretValue + `"}
	]}`
}

// writeFakeSecrets writes the store's content to path, as a write to the real store would land:
// the brokers read the file again on every list and every describe, so this is the only kind of
// "the store changed" these rows can stage.
func writeFakeSecrets(t *testing.T, path, owner string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(fakeSecretsJSON(owner)), 0o600); err != nil {
		t.Fatalf("write the fake secrets file %s: %v", path, err)
	}
}

// writeFakeSecretsWithNoOwner writes the fixture secret with its owner tag taken off, which the
// policy refuses as owner-tag-missing and logs policy.RefusedMessage for on EVERY read of it.
// That line is the only observable a row has for "this process read the store": the broker logs
// nothing for a read it is happy with, and no route reports what a pod has read. A row that
// counts store reads therefore stages the fixture this way first and counts the lines after.
func writeFakeSecretsWithNoOwner(t *testing.T, path string) {
	t.Helper()
	content := `{"secrets": [
		{"name": "` + testSecretsPrefix + multiprocessSecretSlug + `", "kms_key_id": "` + testSecretsKMSKeyARN + `", "tags": {"tier": "agent"}, "value": "` + multiprocessSecretValue + `"}
	]}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write the fake secrets file %s: %v", path, err)
	}
}

// storeReads is how many times a process has read the fixture secret out of the store since it
// started, counted from the refusal line writeFakeSecretsWithNoOwner's tags make it log on each
// read. A row takes it before and after what it drives; the difference is the reads that drove.
func storeReads(process *brokerProcess) int {
	return strings.Count(process.Log(), policy.RefusedMessage)
}

// newBrokerClient is a brokertest.Client for a process this file started, presenting uiToken on
// the routes Dispatch's server calls. The binary under test takes its own listed caller's token
// and its own DPoP signer; brokertest supplies neither, for the reasons its own fields give.
func newBrokerClient(t *testing.T, publicURL, addr, uiToken string) *brokertest.Client {
	t.Helper()
	client := brokertest.NewClient(t, publicURL, addr)
	client.UIToken = uiToken
	client.DPoP = helper.NewDPoPTransport
	return client
}

// listedCaller is what makes a broker this file starts take a caller on the routes Dispatch's
// server calls: Env, which has it trust a local issuer standing in for the cluster's and list
// apiAccount as the caller "dispatch", and Token, that account's own token from that issuer.
type listedCaller struct {
	Env   []string
	Token string
}

// newListedCaller starts the issuer a listedCaller names, which lives as long as t.
func newListedCaller(t *testing.T) listedCaller {
	t.Helper()
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "signing-key")
	return listedCaller{
		Env: []string{
			"BROKER_K8S_OIDC_ISSUER=" + issuer.URL(),
			"BROKER_K8S_OIDC_AUDIENCE=" + enrolmentAudience,
			"BROKER_API_ACCOUNTS=dispatch=" + apiAccount,
		},
		Token: accountToken(t, issuer, key, apiAccount),
	}
}

// brokerPair is two cmd/broker processes serving one database, one fake secrets file and one
// BROKER_PUBLIC_URL, with a client pinned to each.
type brokerPair struct {
	// DatabaseURL is the DSN both processes were started with, which a row connects to directly
	// to read a table no broker route exposes.
	DatabaseURL string
	// FakeSecretsFile is the file standing in for the secret store, which both processes read
	// again on every call, so rewriting it is the write the reread row needs the store to have
	// taken.
	FakeSecretsFile string
	// Processes and Clients are index-matched: Clients[i] drives Processes[i] and nothing else.
	Processes [2]*brokerProcess
	Clients   [2]*brokertest.Client
}

// startBrokerPair starts two brokers from binary on a database of this test's own — a whole
// database rather than a schema, because the Postgres channel the reread fan-out listens on and
// the advisory lock the migration takes are both database-wide (storetest.DatabaseURL) — and
// returns once both have logged the address they bound. Neither process drains: nothing a row
// sharing this pair leaves in flight when it ends, so a drain would only make every row's
// cleanup wait out a budget it has no use for.
func startBrokerPair(t *testing.T, binary string) *brokerPair {
	t.Helper()
	return startBrokerPairDraining(t, binary, [2]time.Duration{})
}

// startBrokerPairDraining is startBrokerPair with a BROKER_DRAIN_SECONDS of its own for each
// process, for the row about what a pod that is stopping still hears while it goes on serving.
//
// Neither start waits for its own listening line: both processes are launched before either has
// migrated, which is the state the migration row is about and which a staggered start would hide.
func startBrokerPairDraining(t *testing.T, binary string, drains [2]time.Duration) *brokerPair {
	t.Helper()
	pair := &brokerPair{
		DatabaseURL:     storetest.DatabaseURL(t),
		FakeSecretsFile: filepath.Join(t.TempDir(), "fake-secrets.json"),
	}
	writeFakeSecrets(t, pair.FakeSecretsFile, multiprocessPerson)
	// One issuer and one listed caller for both processes, as both pods of a Deployment trust the
	// one cluster's issuer and list the one Dispatch.
	caller := newListedCaller(t)
	for i := range pair.Processes {
		pair.Processes[i] = startBroker(t, brokerOptions{
			Binary:             binary,
			NoWaitForListening: true,
			Env: append([]string{
				"BROKER_DATABASE_URL=" + pair.DatabaseURL,
				"BROKER_LISTEN_ADDR=127.0.0.1:0",
				"BROKER_PUBLIC_URL=" + multiprocessPublicURL,
				"BROKER_FAKE_SECRETS_FILE=" + pair.FakeSecretsFile,
				"BROKER_DRAIN_SECONDS=" + strconv.Itoa(int(drains[i]/time.Second)),
			}, caller.Env...),
		})
	}
	for i, process := range pair.Processes {
		process.AwaitListening(t, migrateTimeout)
		pair.Clients[i] = newBrokerClient(t, multiprocessPublicURL, process.Addr, caller.Token)
	}
	return pair
}

// warm asks each process for its health, so that a race run straight after measures the two
// processes' own handling of one record and not one client's TCP handshake: a client that has
// already talked to its process holds an idle keep-alive connection its next request reuses,
// while one that has not would spend a round trip opening a connection before its request was
// even sent. It is a request through the client, not a bare GET at the address, so the
// connection it leaves idle is the one every later step of the row goes out on.
func (p *brokerPair) warm(t *testing.T) {
	t.Helper()
	for _, client := range p.Clients {
		if status, body := client.Req(t, http.MethodGet, "/healthz", nil, nil); status != http.StatusOK {
			t.Fatalf("GET /healthz at %s = %d: %s", client.Addr, status, body)
		}
	}
}

// database connects to the DSN the pair's processes were started with, so a row can read a table
// no broker route exposes. A row reads such a row through a connection of its own rather than
// through a broker, because what it is checking is the state of the one database the two
// processes share, not what either process is willing to say about it.
func (p *brokerPair) database(t *testing.T) *pgx.Conn {
	t.Helper()
	return connectToBroker(t, p.DatabaseURL)
}

// connectToBroker opens a connection of the test's own to a broker's database, closed when t
// ends. It is a connection outside every broker's pool, which is what a row needs to hold a row
// lock a broker then waits on: a lock taken through the pool the broker draws on would starve it
// of a connection rather than block it on the row.
func connectToBroker(t *testing.T, databaseURL string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("connect to the broker's database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// countRows runs a counting query on the brokers' own database and answers what it counted.
func countRows(t *testing.T, conn *pgx.Conn, query string, args ...any) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// countMigrations is how many migrations the binary embeds, counted from the directory it embeds
// (internal/broker/store's `//go:embed all:migrations`) rather than written down here, so a
// migration added without touching this file still has to show up in the table a row reads.
func countMigrations(t *testing.T) int {
	t.Helper()
	const dir = "../../internal/broker/store/migrations"
	files, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("count the migrations under %s: %d files, %v", dir, len(files), err)
	}
	return len(files)
}

// soloBroker is one cmd/broker process on a database of its own, with a client pinned to it. The
// row about what a pod does between its SIGTERM and its exit takes one of these rather than the
// shared pair: there is no second process in what it checks, and a row that stops a process must
// not leave the pair every other row shares half stopped.
type soloBroker struct {
	Process     *brokerProcess
	Client      *brokertest.Client
	DatabaseURL string
}

// startSoloBroker starts one broker from binary with drain as its BROKER_DRAIN_SECONDS, on a
// database and a fake secrets file of its own, and returns once it has logged the address it
// bound.
//
// Its BROKER_PUBLIC_URL is dev-broker.sh's derive-from-bind convention (http://127.0.0.1:0,
// which main resolves from the real bound address), so the one address its client dials is also
// the URL every proof that client signs names. The pair's shared public URL stands for a load
// balancer in front of two pods, and there is no second pod here to stand in front of.
func startSoloBroker(t *testing.T, binary string, drain time.Duration) *soloBroker {
	t.Helper()
	solo := &soloBroker{DatabaseURL: storetest.DatabaseURL(t)}
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	writeFakeSecrets(t, fakeSecretsFile, multiprocessPerson)
	caller := newListedCaller(t)
	// It does not wait for its own listening line here: its database is fresh and unmigrated, so
	// the wait is AwaitListening's own migrateTimeout rather than startBroker's listenTimeout,
	// which is sized for a broker that has only to bind.
	solo.Process = startBroker(t, brokerOptions{
		Binary:             binary,
		NoWaitForListening: true,
		Env: append([]string{
			"BROKER_DATABASE_URL=" + solo.DatabaseURL,
			"BROKER_LISTEN_ADDR=127.0.0.1:0",
			"BROKER_PUBLIC_URL=http://127.0.0.1:0",
			"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
			"BROKER_DRAIN_SECONDS=" + strconv.Itoa(int(drain/time.Second)),
		}, caller.Env...),
	})
	solo.Process.AwaitListening(t, migrateTimeout)
	solo.Client = newBrokerClient(t, "http://"+solo.Process.Addr, solo.Process.Addr, caller.Token)
	return solo
}

// freshRequest GETs path at addr over a connection of its own and answers the status, the body,
// and whatever went wrong instead. A transport per call, never a client's idle keep-alive
// connection: what a drain is about is whether the listener is still ACCEPTING, which only a
// connection opened after the signal can show, and whether it is gone, which a reused connection
// would not notice at all.
func freshRequest(addr, path string) (int, string, error) {
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	resp, err := (&http.Client{Transport: transport, Timeout: 10 * time.Second}).Get("http://" + addr + path)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

// mustRequest is freshRequest for a caller that is asserting what the broker ANSWERED: a request
// that never reached it at all is this row's failure rather than its observation.
func mustRequest(t *testing.T, addr, path string) (int, string) {
	t.Helper()
	status, body, err := freshRequest(addr, path)
	if err != nil {
		t.Fatalf("GET http://%s%s: %v", addr, path, err)
	}
	return status, body
}

// errorCode is the code an error answer names ({"code", "error"}, api/server.go's writeError),
// for a row that pins which refusal it got rather than only its status.
func errorCode(t *testing.T, body string) string {
	t.Helper()
	var answer struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("read the error code out of %s: %v", body, err)
	}
	return answer.Code
}

// tokenAnswer is what one process answered a racing OAuth step with: the token pair it issued, or
// the error it refused with.
type tokenAnswer struct {
	token *oauth2.Token
	err   error
}

// describeRefusal renders what a racing step was refused with, for the line that records the
// loser's answer without pinning it: the broker's own status and body when the broker answered at
// all, and the error itself when nothing did.
func describeRefusal(err error) string {
	var retrieve *oauth2.RetrieveError
	if errors.As(err, &retrieve) {
		return fmt.Sprintf("%d %s", retrieve.Response.StatusCode, retrieve.Body)
	}
	return err.Error()
}

// race runs step against each process of the pair at the same moment: every goroutine is parked
// on the barrier before any of them is released, so neither process is given a head start.
func race[T any](pair *brokerPair, step func(client *brokertest.Client) T) [2]T {
	var answers [2]T
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	for i, client := range pair.Clients {
		ready.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			answers[i] = step(client)
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
	return answers
}

// TestTwoBrokers drives real broker processes on one database. Most of its rows share one pair,
// because what each of them races is a record of its own; the rows that cannot share say why and
// start what they need. Two processes migrating a database neither has migrated yet take a pair
// of their own on a fresh database; the reread row retags the pair's shared secret store and
// leaves it retagged, which only the reread rate-limit row after it reads, and that row reads
// the buckets rather than the tags; and the drain row takes a process and a database of its own.
// The binary is built once for every row here.
func TestTwoBrokers(t *testing.T) {
	binary := buildBroker(t)
	pair := startBrokerPair(t, binary)
	conn := pair.database(t)

	// Two processes redeeming one refresh token at the same moment. What the loser is told
	// depends on the grace window oauthstore gives a rotated token (its refreshGrace, five
	// minutes), which exists so a client whose refresh answer was lost in transit can present the
	// token it still holds again: the second redemption lands inside that window, so it is read
	// as that retry rather than as a reuse. Nothing here pins the loser's answer — the row logs
	// it — and nothing here pins the grace either. What it does pin is what must hold whatever
	// the window is: the machine login survives, no process answers a refresh with a 5xx, and the
	// family is left with exactly one branch.
	//
	// Observed on this machine, every run: there is no loser — both processes answered 200 with a
	// token pair of their own, the second redemption read as the retry the grace exists for, and
	// its rotation ended the branch the first had started.
	t.Run("1_TwoConcurrentRefreshesOfOneRefreshToken", func(t *testing.T) {
		const host = "two-brokers-refresh"
		machineKey := brokertest.NewSigningKey(t)
		authorization := pair.Clients[0].DeviceLogin(t, machineKey, multiprocessPerson, host)
		credentialID := pair.Clients[0].ApproveMachineLogin(t, multiprocessPerson, authorization.UserCode)
		token, err := pair.Clients[0].DeviceToken(t, machineKey, authorization)
		if err != nil {
			t.Fatalf("exchange %s's device code: %s", host, describeRefusal(err))
		}
		if token.RefreshToken == "" {
			t.Fatalf("the exchange for %s answered no refresh token, so there is nothing to race", host)
		}

		pair.warm(t)
		answers := race(pair, func(client *brokertest.Client) tokenAnswer {
			refreshed, err := client.Refresh(t, machineKey, token)
			return tokenAnswer{token: refreshed, err: err}
		})
		for i, answer := range answers {
			if answer.err == nil {
				t.Logf("the process at %s answered the refresh with a token pair", pair.Clients[i].Addr)
				continue
			}
			var retrieve *oauth2.RetrieveError
			if !errors.As(answer.err, &retrieve) {
				t.Fatalf("refresh against %s: %v; want the broker's own answer, not a failure to reach it",
					pair.Clients[i].Addr, answer.err)
			}
			t.Logf("the process at %s refused the refresh with %s", pair.Clients[i].Addr, describeRefusal(answer.err))
			if retrieve.Response.StatusCode >= http.StatusInternalServerError {
				t.Errorf("refresh against %s = %s; a refresh two processes redeem at once must never be a server error",
					pair.Clients[i].Addr, describeRefusal(answer.err))
			}
		}

		// The machine login is still live: its credential still enrolls a session, and that
		// session is served — enrolled through one process, its request decided by the other, its
		// values released by the first.
		enrollmentID, sessionKey := pair.Clients[1].EnrollSession(t, machineKey, credentialID, host+"-session")
		grantID := pair.Clients[0].RequestAgentSecret(t, sessionKey, enrollmentID, "the refresh race left the login live", multiprocessSecret)
		if values := pair.Clients[1].GrantValues(t, sessionKey, enrollmentID, grantID); values[multiprocessSecret] != multiprocessSecretValue {
			t.Fatalf("the grant released %v, want %s=%s", values, multiprocessSecret, multiprocessSecretValue)
		}

		// One live row for the family. A rotated token stays active through its grace so the
		// retry above can present it, so "live" is the branch tip: active and never rotated. Two
		// of those would be two branches, which is what reuse detection could never tell apart
		// again.
		live := countRows(t, conn, `select count(*) from oauth_refresh_tokens
			where credential_id::text = $1 and active and grace_until is null`, credentialID)
		if live != 1 {
			t.Fatalf("the family of credential %s holds %d live refresh tokens, want exactly 1", credentialID, live)
		}
	})

	// Two processes exchanging one approved device code at the same moment. Exactly one of them
	// issues the token pair; the other is refused, and what it is refused with is logged rather
	// than pinned, because which refusal it is depends on how the two transactions interleave.
	// Observed on this machine, every run: the loser is answered 400 invalid_grant — the winner's
	// transaction had already committed, so the library found no live device code to read at all.
	// The other outcome the code names is a 500 server_error (oauth/provider.go's Token: any
	// error from hooks.Before but ErrLoginEnded), which is what the loser gets when both read the
	// device code before either commits; it is a known finding this issue does not fix. So this
	// row asserts that exactly one process won, never what the loser said — and that the record
	// is left with the one launcher credential its approval minted, never a second one some
	// losing exchange also issued.
	t.Run("2_TwoConcurrentDeviceCodeExchanges", func(t *testing.T) {
		const host = "two-brokers-device"
		machineKey := brokertest.NewSigningKey(t)
		authorization := pair.Clients[0].DeviceLogin(t, machineKey, multiprocessPerson, host)
		credentialID := pair.Clients[0].ApproveMachineLogin(t, multiprocessPerson, authorization.UserCode)

		pair.warm(t)
		answers := race(pair, func(client *brokertest.Client) tokenAnswer {
			token, err := client.DeviceToken(t, machineKey, authorization)
			return tokenAnswer{token: token, err: err}
		})
		won := 0
		for i, answer := range answers {
			if answer.err != nil {
				t.Logf("the process at %s refused the exchange with %s", pair.Clients[i].Addr, describeRefusal(answer.err))
				continue
			}
			won++
			t.Logf("the process at %s answered the exchange with a token pair", pair.Clients[i].Addr)
			if answer.token.AccessToken == "" || answer.token.RefreshToken == "" {
				t.Errorf("the exchange won at %s answered access token %t, refresh token %t; want both",
					pair.Clients[i].Addr, answer.token.AccessToken != "", answer.token.RefreshToken != "")
			}
			if got, _ := answer.token.Extra("credential_id").(string); got != credentialID {
				t.Errorf("the exchange won at %s named credential %q, want the approved %s", pair.Clients[i].Addr, got, credentialID)
			}
		}
		if won != 1 {
			t.Fatalf("%d of the two processes exchanged the one device code, want exactly 1", won)
		}

		credentials := countRows(t, conn, `select count(*) from launcher_credentials
			where record_id = (select record_id from launcher_credentials where id::text = $1)`, credentialID)
		if credentials != 1 {
			t.Fatalf("the machine login's record holds %d launcher credentials, want the 1 its approval minted", credentials)
		}
	})

	// Two processes migrating one database at the same moment. It takes a database of its own,
	// unmigrated: store.Migrate serializes on a database-wide advisory lock, so a pair sharing
	// the pair above's already-migrated database would race over nothing.
	//
	// The loser's wait on pg_advisory_xact_lock is unbounded by design — store.go takes it with
	// no timeout, and main.go migrates before it binds — so this row does not try to bound that
	// wait or to say which process wins. It waits for both to come up (startBrokerPair's own
	// AwaitListening, which fails if either never logs "broker listening"), asks both for their
	// health, and then reads what the two of them left in the schema table.
	//
	// What it reads there is the COMPLETE set: as many rows as the binary embeds migrations.
	// Asserting that each version appears once would assert nothing at all — version is that
	// table's primary key — and a count of its own would pass on a half-applied schema, which is
	// the outcome a race between two migrators could actually leave and the one a broker would
	// then serve requests against.
	t.Run("3_TwoProcessesMigratingAtOnce", func(t *testing.T) {
		fresh := startBrokerPair(t, binary)
		for i, process := range fresh.Processes {
			if process.Addr == "" {
				t.Fatalf("process %d never logged the address it bound:\n%s", i, process.Log())
			}
			requireHealthy(t, process.Addr)
		}

		freshConn := fresh.database(t)
		embedded := countMigrations(t)
		versions := countRows(t, freshConn, `select count(*) from broker_schema_migrations`)
		if versions != embedded {
			t.Fatalf("two brokers booted together on an empty database and broker_schema_migrations holds %d rows, want the %d migrations the binary embeds",
				versions, embedded)
		}
	})

	// A reread told to one process takes effect on the other. This is what the fan-out is for: a
	// retag that narrows who may have a secret must be in force across the whole Deployment
	// within seconds, rather than at each pod's own five-minute reload (policyRefresh in
	// main.go). One POST rereads the pod it reached and, on its 200 path alone, tells every
	// other pod serving the same database to do the same, over Postgres LISTEN/NOTIFY
	// (internal/broker/reread).
	//
	// The row is the production story with its control placed before the act: the store is
	// retagged, a fresh session is STILL granted the secret by B, and only then is A told to
	// reread. So the one step that can account for B's answer changing is the notification —
	// not the write, which both processes could see at any moment and neither reads until
	// something makes it.
	//
	// Ten seconds bounds the wait, which is a thirtieth of the five-minute reload that is the
	// fan-out's documented backstop: inside that window no reload has run, so a pass here is
	// the notification and nothing else.
	//
	// Every ask comes from a session enrolled for the first time. requests.Machine.Create hands
	// a still-live grant back for an exact name-set match against the SAME enrollment before it
	// evaluates the policy at all, so a second ask from one enrollment would repeat the first
	// answer whatever the policy now says — forever.
	t.Run("4_ARereadOnOneProcessNarrowsTheSecretOnTheOther", func(t *testing.T) {
		// fanOutWithin bounds how long B may go on granting the secret after A was told to
		// reread it.
		const fanOutWithin = 10 * time.Second
		const host = "two-brokers-reread"
		// A is the process the reread is sent to; B is the one that must learn of it without
		// the test ever telling it anything.
		a, b := pair.Clients[0], pair.Clients[1]
		machineKey := brokertest.NewSigningKey(t)
		authorization := b.DeviceLogin(t, machineKey, multiprocessPerson, host)
		credentialID := b.ApproveMachineLogin(t, multiprocessPerson, authorization.UserCode)
		if _, err := b.DeviceToken(t, machineKey, authorization); err != nil {
			t.Fatalf("exchange %s's device code: %s", host, describeRefusal(err))
		}
		asks := 0
		ask := func() brokertest.AgentSecretAnswer {
			asks++
			enrollmentID, sessionKey := b.EnrollSession(t, machineKey, credentialID, fmt.Sprintf("%s-session-%d", host, asks))
			return b.AskForAgentSecret(t, sessionKey, enrollmentID, "the reread fan-out row", multiprocessSecret)
		}

		if baseline := ask(); baseline.State != "granted" {
			t.Fatalf("before the retag, B (%s) answered a session of %s's own owner %d %s; want it granted at once",
				b.Addr, multiprocessSecret, baseline.Status, baseline.Body)
		}

		writeFakeSecrets(t, pair.FakeSecretsFile, multiprocessNewOwner)

		if control := ask(); control.State != "granted" {
			t.Fatalf("with the store retagged but nothing reread, B (%s) already answered %d %s; want it still granted — a write to the store changes what a pod serves only once something makes that pod read it again",
				b.Addr, control.Status, control.Body)
		}

		status, body := a.Req(t, http.MethodPost, "/v1/secrets/"+multiprocessSecret+"/reread", nil, nil)
		var reread struct {
			Served bool `json:"served"`
		}
		if status != http.StatusOK || json.Unmarshal(body, &reread) != nil || !reread.Served {
			t.Fatalf("POST /v1/secrets/%s/reread at A (%s) = %d %s; want 200 with the retagged secret still served, which is the only answer that fans out",
				multiprocessSecret, a.Addr, status, body)
		}
		rereadAt := time.Now()

		// Polled by asking again, which is the only observable there is: either B grants a
		// fresh session the secret at once or it does not. Every turn is two real requests to
		// B, so nothing here waits on a clock.
		var answer brokertest.AgentSecretAnswer
		for {
			answer = ask()
			if answer.State != "granted" {
				break
			}
			if time.Since(rereadAt) > fanOutWithin {
				t.Fatalf("%s and %d asks after A (%s) reread %s, B (%s) still answered a freshly enrolled session %d %s; want the retag in force on B, which this far inside B's %s reload only the fan-out could have put there",
					fanOutWithin, asks, a.Addr, multiprocessSecret, b.Addr, answer.Status, answer.Body, policyRefresh)
			}
		}
		t.Logf("B (%s) stopped granting %s at once %s after A was told to reread it, on ask %d: %d %s",
			b.Addr, multiprocessSecret, time.Since(rereadAt).Round(time.Millisecond), asks, answer.Status, answer.Body)

		// What B answers instead is pinned, not merely "something other than granted": the
		// secret is now another person's agent-tier secret, so the policy turns the ask into an
		// approval request to that person and releases nothing (internal/broker/policy).
		if answer.Status != http.StatusOK || answer.State != "pending" || answer.GrantID != nil ||
			answer.Approver == nil || *answer.Approver != multiprocessNewOwner {
			t.Fatalf("once the retag had reached B (%s) it answered %d %s; want 200 with state pending, no grant, and %s to approve",
				b.Addr, answer.Status, answer.Body, multiprocessNewOwner)
		}
	})

	// The fan-out spends no rate-limit token on the pod it reaches. Every reread takes the
	// policy's writer lock for one store read, so the public route is capped broker-wide as well
	// as per address (api.DefaultRereadOverallLimit: a burst of ten, one back every second),
	// and that cap's rate is sized as the fleet's whole store budget divided by the replica count
	// squared, since one accepted reread costs a store read on every pod — the sizing row 7
	// below measures. That arithmetic holds only if a reread another pod's notification drives
	// takes nothing from the receiving pod's bucket: otherwise one caller hammering one pod would
	// refuse every other pod's callers too, and the fleet would spend twice what the figure says.
	//
	// So the row empties A's broker-wide bucket with a burst — which is also a burst of
	// notifications at B — and then asks B for a reread of its own, which must be answered 200
	// straight away.
	//
	// The burst is bounded well below the per-address bucket's own burst (thirty, one back every
	// two seconds, of which the row above spent one), so the 429 this row stops on can only be
	// the broker-wide bucket's. The two buckets are refused with the same body, and nothing in
	// the answer would otherwise tell them apart.
	t.Run("5_ANotificationSpendsNoRereadTokenOnTheReceivingPod", func(t *testing.T) {
		const burstBound = 20
		a, b := pair.Clients[0], pair.Clients[1]
		path := "/v1/secrets/" + multiprocessSecret + "/reread"
		var status int
		var body []byte
		sent := 0
		for sent < burstBound && status != http.StatusTooManyRequests {
			status, body = a.Req(t, http.MethodPost, path, nil, nil)
			sent++
			if status != http.StatusOK && status != http.StatusTooManyRequests {
				t.Fatalf("reread %d at A (%s) = %d %s; want 200 until a bucket runs out", sent, a.Addr, status, body)
			}
		}
		if status != http.StatusTooManyRequests {
			t.Fatalf("%d rereads at A (%s) and it still answered %d %s; want the broker-wide bucket to refuse one",
				sent, a.Addr, status, body)
		}
		if code := errorCode(t, string(body)); code != "RATE_LIMITED" {
			t.Fatalf("A (%s) refused reread %d with %s; want RATE_LIMITED", a.Addr, sent, body)
		}
		refusal := string(body)
		t.Logf("A (%s) served %d rereads and refused the next: %s", a.Addr, sent-1, refusal)

		if status, body := b.Req(t, http.MethodPost, path, nil, nil); status != http.StatusOK {
			t.Fatalf("A (%s) refused its own next reread with %s, and B (%s) then answered a reread of its own %d %s; want 200 — the %d notifications B took from A must have spent nothing of B's bucket",
				a.Addr, refusal, b.Addr, status, body, sent-1)
		}
	})

	// What a pod does between its SIGTERM and its exit: it fails its health check at once while
	// it goes on serving every other route for the whole drain, and it finishes a request that
	// is still in flight long past the ten-second shutdown budget the broker used to have.
	//
	// One process, and a database of its own: nothing here is about two pods, and a row that
	// stops a process must not stop one the rows above share. The request held in flight is a
	// real refresh blocked where the broker's own code takes a row lock — oauthstore's
	// RotateRefreshToken opens the rotation with `select … for update` on the redeemed token's
	// row — so a transaction of the test's own holding that row stops a genuine request inside
	// the handler, with nothing faked and nothing sleeping in the broker.
	t.Run("6_ADrainingProcessFailsReadinessAndServesOn", func(t *testing.T) {
		const (
			// drain is this broker's BROKER_DRAIN_SECONDS: long enough to get both observations
			// out inside the window, short enough that the row pays for no budget it does not
			// use.
			drain = 5 * time.Second
			// observeWithin bounds each observation taken against a signalled process, so a row
			// that fails names what the broker answered instead of running a budget out.
			observeWithin = 2 * time.Second
			// oldShutdownBudget is what main.go's shutdownTimeout was before it became the
			// write timeout: the figure this row exists to outlive.
			oldShutdownBudget = 10 * time.Second
			// holdFor is when the blocked refresh is let go, measured from the signal, past
			// oldShutdownBudget by a margin no scheduling hiccup closes.
			holdFor = 12 * time.Second
			// exitWithin bounds the wait for the process to go once its last request has been
			// answered. It covers the whole stop budget the broker logs at boot (this drain
			// plus main.go's 60-second shutdown), so only a broker that hangs fails it.
			exitWithin = 90 * time.Second
			host       = "draining-broker"
		)
		solo := startSoloBroker(t, binary, drain)
		machineKey := brokertest.NewSigningKey(t)
		authorization := solo.Client.DeviceLogin(t, machineKey, multiprocessPerson, host)
		credentialID := solo.Client.ApproveMachineLogin(t, multiprocessPerson, authorization.UserCode)
		token, err := solo.Client.DeviceToken(t, machineKey, authorization)
		if err != nil {
			t.Fatalf("exchange %s's device code: %s", host, describeRefusal(err))
		}

		ctx := context.Background()
		holder, err := connectToBroker(t, solo.DatabaseURL).Begin(ctx)
		if err != nil {
			t.Fatalf("open the transaction that holds the refresh token's row: %v", err)
		}
		defer func() { _ = holder.Rollback(ctx) }()
		// Exactly one row, which is the only one there is to block on: the exchange above minted
		// this login's single refresh token, and a lock that found none would leave the refresh
		// below running straight through instead of waiting.
		locked, err := holder.Exec(ctx, `select 1 from oauth_refresh_tokens
			where credential_id::text = $1 for update`, credentialID)
		if err != nil || locked.RowsAffected() != 1 {
			t.Fatalf("lock %s's refresh token row: %d rows locked, %v", host, locked.RowsAffected(), err)
		}
		watch := connectToBroker(t, solo.DatabaseURL)

		refreshed := make(chan tokenAnswer, 1)
		go func() {
			rotated, err := solo.Client.Refresh(t, machineKey, token)
			refreshed <- tokenAnswer{token: rotated, err: err}
		}()
		// The refresh is really waiting on that row, not merely sent: without this the row
		// could not say anything was in flight at the signal at all, and a sleep in its place
		// would say it without knowing it.
		storetest.AwaitLockWaiters(t, watch, holder, 1)

		signalled := solo.Process.Terminate(t)

		// Inside the drain, readiness fails. Polled because the signal handler flips the flag on
		// a goroutine of its own; every turn is a real request on a connection of its own.
		var health int
		var healthBody string
		for {
			health, healthBody = mustRequest(t, solo.Process.Addr, "/healthz")
			if health != http.StatusOK {
				break
			}
			if time.Since(signalled) > observeWithin {
				t.Fatalf("GET /healthz at %s still answered 200 %s after the SIGTERM; want 503 SHUTTING_DOWN from the moment it begins draining",
					solo.Process.Addr, observeWithin)
			}
		}
		if health != http.StatusServiceUnavailable || errorCode(t, healthBody) != "SHUTTING_DOWN" {
			t.Fatalf("GET /healthz at %s after the SIGTERM = %d %s; want 503 SHUTTING_DOWN",
				solo.Process.Addr, health, healthBody)
		}
		// …and in the same breath a connection opened after that signal still gets a whole
		// answer out of a route that is not the health check. The pair is the claim: the pod
		// takes itself out of its Service's endpoints while it goes on serving the traffic the
		// kubelet is still routing to it.
		if status, body := mustRequest(t, solo.Process.Addr, "/v1/settings"); status != http.StatusOK {
			t.Fatalf("GET /v1/settings at %s, %s into its drain, = %d %s; want 200 — a draining broker serves every route but its health check",
				solo.Process.Addr, time.Since(signalled).Round(time.Millisecond), status, body)
		}

		// This wait IS the experiment, and it is the only bare sleep in this file: the claim is
		// about a request still in flight when the old ten-second shutdown budget would have run
		// out, so the length of the wait is the thing under test rather than a stand-in for some
		// observable the row could have waited on instead.
		time.Sleep(time.Until(signalled.Add(holdFor)))
		if err := holder.Rollback(ctx); err != nil {
			t.Fatalf("release the refresh token's row: %v", err)
		}

		answer := <-refreshed
		if answer.err != nil {
			t.Fatalf("the refresh held in flight across the drain was answered %s; want the broker to finish it:\n%s",
				describeRefusal(answer.err), solo.Process.Log())
		}
		if answer.token.AccessToken == "" || answer.token.RefreshToken == "" {
			t.Fatalf("the refresh held in flight answered access token %t, refresh token %t; want both",
				answer.token.AccessToken != "", answer.token.RefreshToken != "")
		}
		if err := solo.Process.AwaitExit(t, exitWithin); err != nil {
			t.Fatalf("the broker exited with %v after draining; want 0:\n%s", err, solo.Process.Log())
		}
		stopped := time.Since(signalled)
		t.Logf("the broker exited 0 %s after its SIGTERM, having finished the refresh it was holding",
			stopped.Round(time.Millisecond))
		if stopped <= oldShutdownBudget {
			t.Fatalf("the broker exited %s after its SIGTERM, inside the old %s budget; the row says nothing about that budget unless the request it held outlived it",
				stopped.Round(time.Millisecond), oldShutdownBudget)
		}

		// The control, which is what makes the serving window above mean anything: the same
		// binary and the same request, with the drain set to zero and nothing in flight. Here
		// the listener must be gone almost at once, where the drained process answered
		// /v1/settings at the same point after its own signal — so that window came from
		// BROKER_DRAIN_SECONDS and not from a process that happens to be slow to stop.
		control := startSoloBroker(t, binary, 0)
		if status, body := mustRequest(t, control.Process.Addr, "/v1/settings"); status != http.StatusOK {
			t.Fatalf("GET /v1/settings at the control broker %s before its signal = %d %s; want 200",
				control.Process.Addr, status, body)
		}
		controlSignalled := control.Process.Terminate(t)
		for {
			status, body, err := freshRequest(control.Process.Addr, "/v1/settings")
			if err != nil {
				t.Logf("the control broker refused a fresh connection %s after its signal: %v",
					time.Since(controlSignalled).Round(time.Millisecond), err)
				break
			}
			if time.Since(controlSignalled) > observeWithin {
				t.Fatalf("the control broker at %s went on answering GET /v1/settings %d %s for %s after a SIGTERM with BROKER_DRAIN_SECONDS=0; want its listener gone",
					control.Process.Addr, status, body, observeWithin)
			}
		}
	})

	// What the fleet spends of the secret store, measured rather than asserted from the
	// constants alone. Every accepted reread now costs the fleet one store read per pod — its
	// own, plus the one the fan-out drives on each peer — where before the fan-out existed it
	// cost exactly one. Halving the broker-wide bucket therefore does not hold the fleet's
	// metadata traffic where one pod's was: with N pods the fleet's accepted rereads are N ×
	// the per-pod rate and each costs N store reads, so the store sees N² × that rate however
	// the bucket is sized by halving. This row measures the per-reread cost against two real
	// processes and then does that arithmetic on the live limits.
	//
	// The budget it holds them to is the one the issue states: "two pods together cannot send
	// more metadata traffic than one pod was allowed to". One pod was allowed four rereads a
	// second against a store where a reread is one read (api.DefaultRereadOverallLimit before
	// this fan-out: Every 250ms), and one a second against Google Secret Manager, where
	// api.GCPRereadOverallLimit's own comment turns that figure into the 120 metadata reads a
	// minute the public route may spend of the project's shared 600. Those two figures are the
	// product budget — the share of someone else's quota this route is allowed — not an
	// implementation detail, which is why they are written here rather than read from the
	// package whose sizing is under test.
	t.Run("7_TheFleetsStoreTrafficStaysInsideWhatOnePodWasAllowed", func(t *testing.T) {
		const (
			onePodRereadsPerSecond    = 4.0
			onePodGCPRereadsPerSecond = 1.0
			// countWithin bounds the wait for the peer's own read of the store, which arrives
			// over LISTEN/NOTIFY and not on the request's own goroutine.
			countWithin = 10 * time.Second
		)
		fleet := startBrokerPair(t, binary)
		writeFakeSecretsWithNoOwner(t, fleet.FakeSecretsFile)
		before := [2]int{storeReads(fleet.Processes[0]), storeReads(fleet.Processes[1])}

		status, body := fleet.Clients[0].Req(t, http.MethodPost, "/v1/secrets/"+multiprocessSecret+"/reread", nil, nil)
		if status != http.StatusOK {
			t.Fatalf("POST /v1/secrets/%s/reread at %s = %d %s; want the 200 that spends a token of the broker-wide bucket and fans out",
				multiprocessSecret, fleet.Clients[0].Addr, status, body)
		}
		rereadAt := time.Now()
		// The counts this row does its arithmetic on are the ones the loop exits on, read once
		// each and not again: re-reading them afterwards lets a read that straggled in between
		// count too, and a fleet cost of three rather than two would fail the assertion below
		// and say the sizing was wrong when it was right.
		var after [2]int
		for {
			after = [2]int{storeReads(fleet.Processes[0]), storeReads(fleet.Processes[1])}
			if after[0] > before[0] && after[1] > before[1] {
				break
			}
			if time.Since(rereadAt) > countWithin {
				t.Fatalf("%s after one accepted reread at %s, the two processes had read the store %d and %d more times; want each of them to have read it — the fan-out is what makes a reread cost the fleet more than one read",
					countWithin, fleet.Clients[0].Addr, after[0]-before[0], after[1]-before[1])
			}
			time.Sleep(20 * time.Millisecond)
		}
		reads := after[0] - before[0] + after[1] - before[1]
		replicas := len(fleet.Processes)
		t.Logf("one accepted reread at %s cost the %d-pod fleet %d reads of the secret store", fleet.Clients[0].Addr, replicas, reads)

		for _, bucket := range []struct {
			name   string
			every  time.Duration
			onePod float64
		}{
			{"api.DefaultRereadOverallLimit", api.DefaultRereadOverallLimit.Every, onePodRereadsPerSecond},
			{"api.GCPRereadOverallLimit", api.GCPRereadOverallLimit.Every, onePodGCPRereadsPerSecond},
		} {
			perPod := float64(time.Second) / float64(bucket.every)
			fleetReads := float64(replicas) * perPod * float64(reads)
			if fleetReads > bucket.onePod {
				t.Errorf("%s lets each of %d pods accept %.2f rereads a second, and one accepted reread costs the fleet %d store reads, so the fleet reads the store %.2f times a second; one pod alone was allowed %.2f. Sizing this bucket by the replica count holds the fleet's HTTP rereads where one pod's were, not its store traffic, which the fan-out multiplies by the replica count a second time.",
					bucket.name, replicas, perPod, reads, fleetReads, bucket.onePod)
			}
		}
	})

	// A pod that is stopping still takes the other pods' rereads. The drain exists because new
	// requests are still routed to a pod for a while after its SIGTERM, so for that whole window
	// it is one of the pods "serving the same database" that a reread has to reach: a retag that
	// narrows who may have a secret must be in force there too, or the one pod the load balancer
	// is still choosing goes on releasing the secret under the old tags.
	//
	// Its control is the row above and row 4: the same two processes, the same fixture and the
	// same call, with neither of them signalled, put the retag on the peer within milliseconds.
	// What this row changes is the signal, and nothing else.
	t.Run("8_ADrainingPodStillTakesAPeersReread", func(t *testing.T) {
		const (
			// drain is long enough to hold the whole observation inside the window in which the
			// pod is still being sent traffic, which is what the row is about.
			drain = 30 * time.Second
			// observeWithin bounds each observation taken against the signalled process.
			observeWithin = 2 * time.Second
			// fanOutWithin bounds the wait for the peer's reread, as row 4 bounds its own: far
			// inside the five-minute reload, so only the fan-out could have driven one.
			fanOutWithin = 10 * time.Second
		)
		stopping := startBrokerPairDraining(t, binary, [2]time.Duration{0, drain})
		serving, draining := stopping.Processes[0], stopping.Processes[1]
		writeFakeSecretsWithNoOwner(t, stopping.FakeSecretsFile)

		signalled := draining.Terminate(t)
		for {
			status, body := mustRequest(t, draining.Addr, "/healthz")
			if status == http.StatusServiceUnavailable && errorCode(t, body) == "SHUTTING_DOWN" {
				break
			}
			if time.Since(signalled) > observeWithin {
				t.Fatalf("GET /healthz at the signalled process %s answered %d %s for %s; the row needs it draining before it asks what a draining pod hears",
					draining.Addr, status, body, observeWithin)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if status, body := mustRequest(t, draining.Addr, "/v1/settings"); status != http.StatusOK {
			t.Fatalf("GET /v1/settings at the draining process %s = %d %s; the row is about a pod that is still serving, and this one is not",
				draining.Addr, status, body)
		}

		before := [2]int{storeReads(serving), storeReads(draining)}
		status, body := stopping.Clients[0].Req(t, http.MethodPost, "/v1/secrets/"+multiprocessSecret+"/reread", nil, nil)
		if status != http.StatusOK {
			t.Fatalf("POST /v1/secrets/%s/reread at %s = %d %s; want the 200 that fans out",
				multiprocessSecret, stopping.Clients[0].Addr, status, body)
		}
		rereadAt := time.Now()
		for storeReads(draining) == before[1] {
			if time.Since(rereadAt) > fanOutWithin {
				if storeReads(serving) == before[0] {
					t.Fatalf("%s after the reread, the process that served it (%s) had not read the store either; the row cannot say what the draining pod missed when the call itself did nothing",
						fanOutWithin, stopping.Clients[0].Addr)
				}
				still, body := mustRequest(t, draining.Addr, "/v1/settings")
				t.Fatalf("%s and %s after its SIGTERM, the draining process %s had not reread %s, which %s was told to reread and did — while %s still answered GET /v1/settings %d %s. A pod is still being sent requests for the whole of its %s drain, so a retag that narrows who may have the secret is unenforced there until it stops accepting.",
					fanOutWithin, time.Since(signalled).Round(time.Millisecond), draining.Addr, multiprocessSecret,
					stopping.Clients[0].Addr, draining.Addr, still, body, drain)
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Logf("the draining process %s reread %s %s after its SIGTERM, told by %s",
			draining.Addr, multiprocessSecret, time.Since(signalled).Round(time.Millisecond), stopping.Clients[0].Addr)
	})
}

// The rollout's cast: three accounts listed in both generations' configurations for one project
// and one person, which differ in nothing but which of them the configuration fixes rolloutSession
// for. The old configuration fixes it for oldEntryAccount's entry and the new one re-points it to
// newEntryAccount's; podAccount's entry fixes no name in either, so it is the stranger that asks
// for the name and the caller of an exchange that has nothing to do with it.
const (
	oldEntryAccount = "system:serviceaccount:example-ns:example-daemon"
	newEntryAccount = "system:serviceaccount:example-ns:example-scheduler"
	rolloutSession  = "example-daemon-session"
	// oldEntryPrincipal and newEntryPrincipal are who those entries' pods write as, which is
	// what oauth_session_bindings records as a name's holder (exchange's accountPrincipal).
	oldEntryPrincipal = "account:" + oldEntryAccount
	newEntryPrincipal = "account:" + newEntryAccount
	// takeoverWarning is what a broker logs once an exchange has taken a fixed name from the
	// principal holding it, the one line that says a name moved; leftWarning is what it logs when
	// the first exchange it serves finds a name its configuration fixes held by another principal,
	// and leaves it with that holder (oauth's exchangeGrant and reserveSessions).
	takeoverWarning = "broker: the token exchange took a session name the configuration fixes from its holder"
	leftWarning     = "broker: the token exchange left a session name the configuration fixes with its holder until the entry claims it"
)

// rolloutAccounts is BROKER_EXCHANGE_ACCOUNTS for one generation of the rollout: the three
// accounts, with rolloutSession fixed for fixer's entry and for no other.
func rolloutAccounts(fixer string) string {
	entries := make([]string, 0, 3)
	for _, account := range []string{oldEntryAccount, newEntryAccount, podAccount} {
		entry := account + "=" + project + "=" + person
		if account == fixer {
			entry += "=" + rolloutSession
		}
		entries = append(entries, entry)
	}
	return strings.Join(entries, " ")
}

// rolloutBroker is one process of the rollout: what its messages call it, the process whose log a
// row reads, and a client of that process's own address.
type rolloutBroker struct {
	name    string
	process *brokerProcess
	client  *liveBroker
}

// sessionHolder reads who holds session in the database every process of the rollout shares, and
// whether that hold is the configuration's, which never runs out: the one record of whose a name
// is, read past every process's own opinion of it.
func sessionHolder(t *testing.T, conn *pgx.Conn, session string) (string, bool) {
	t.Helper()
	var principal string
	var fixed bool
	if err := conn.QueryRow(context.Background(), `select principal, expires_at = 'infinity' from oauth_session_bindings
		where session = $1`, session).Scan(&principal, &fixed); err != nil {
		t.Fatalf("read the claim on %s: %v", session, err)
	}
	return principal, fixed
}

// linesCarrying is every line of a process's log that carries want.
func linesCarrying(log, want string) []string {
	var lines []string
	for line := range strings.Lines(log) {
		if strings.Contains(line, want) {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return lines
}

// TestAFixedSessionNameAcrossARollout drives a rollout that re-points a fixed session name, on
// real broker processes and one database: the configuration of the process serving a claim
// decides who the name is for, so no process reserving its own configuration's names at its first
// exchange takes a name from another principal. Its rows run in order and each starts from the
// database the one before it left: a stranger asks for the name before its entry ever has, two
// processes of the old generation then serve that entry by turns, and the rollout comes last.
// Every process is started with no drain, since nothing is in flight when one is stopped.
func TestAFixedSessionNameAcrossARollout(t *testing.T) {
	binary := buildBroker(t)
	databaseURL := storetest.URL(t)
	conn := connectToBroker(t, databaseURL)
	cluster, github := oidctest.New(t), oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "cluster-key")
	github.PublishKey(t, "github-key")
	key := proofKey(t)

	// start starts a process of the generation whose configuration fixes rolloutSession for
	// fixer, on the one database, living as long as t: exchangeEnv with that generation's
	// accounts in place of its own, a later value of a variable being the one a process gets, as
	// startBroker's own Env takes precedence over productionEnv.
	start := func(t *testing.T, name, fixer string) rolloutBroker {
		t.Helper()
		process := startBroker(t, brokerOptions{Binary: binary, Env: append(exchangeEnv(t, databaseURL, cluster, github, longAccessSeconds),
			"BROKER_EXCHANGE_ACCOUNTS="+rolloutAccounts(fixer), "BROKER_DRAIN_SECONDS=0")})
		return rolloutBroker{name: fmt.Sprintf("%s (%s)", name, process.Addr), process: process, client: newLiveBroker(process.Addr, key)}
	}
	// exchange is a pod of account exchanging its projected token at broker for a token under
	// session, each pod a fresh one, as each replica of a Deployment's is.
	pods := 0
	exchange := func(t *testing.T, broker rolloutBroker, account, session string) (int, map[string]any) {
		t.Helper()
		pods++
		podToken := mintPodToken(t, cluster, clusterKey, account, exchangeAudience, fmt.Sprintf("rollout-pod-%d", pods))
		return broker.client.exchange(t, exchangeForm(podToken, session))
	}
	refusedTheName := func(status int, body map[string]any) bool {
		description, _ := body["error_description"].(string)
		return status == http.StatusBadRequest && body["error"] == "invalid_grant" && body["access_token"] == nil &&
			strings.Contains(description, "The session name '"+rolloutSession+"' belongs to another identity")
	}

	old := start(t, "the old-configuration process", oldEntryAccount)

	// Reserving takes no name from another principal any more, so it is worth showing that it
	// still does the one thing it is there for: the first exchange this database ever sees is a
	// stranger's, asking for the name the process's configuration fixes for an entry none of
	// whose pods has exchanged, and the reservation that exchange makes first is all that can
	// refuse it.
	t.Run("1_AStrangerIsRefusedAFixedNameItsEntryHasNeverClaimed", func(t *testing.T) {
		status, body := exchange(t, old, podAccount, rolloutSession)
		if !refusedTheName(status, body) {
			t.Fatalf("%s answered the first exchange it served, a pod of %s asking for %s, which its configuration fixes for %s, whose pods have never exchanged, %d %v; want 400 invalid_grant naming the name another identity's",
				old.name, podAccount, rolloutSession, oldEntryAccount, status, body)
		}
		if holder, fixed := sessionHolder(t, conn, rolloutSession); holder != oldEntryPrincipal || !fixed {
			t.Fatalf("after the stranger was refused, %s is held by %q (the configuration's hold: %t); want %s, held for as long as the configuration fixes it",
				rolloutSession, holder, fixed, oldEntryPrincipal)
		}
	})

	// The no-thrash property, which taking the name at the claim must not cost: two processes of
	// one generation, both fixing the name for one entry, serve that entry's exchanges by turns,
	// the second process's first exchange among them. The name never moves, and neither process
	// logs that it took it.
	t.Run("2_TwoProcessesOfOneGenerationNeverTakeTheNameFromEachOther", func(t *testing.T) {
		twin := start(t, "a second old-configuration process", oldEntryAccount)
		for turn, broker := range []rolloutBroker{old, twin, old, twin} {
			status, body := exchange(t, broker, oldEntryAccount, rolloutSession)
			if status != http.StatusOK || body["access_token"] == nil {
				t.Fatalf("turn %d: %s answered a pod of %s asking for %s, which its configuration fixes for that account, %d %v; want 200 with an access token",
					turn+1, broker.name, oldEntryAccount, rolloutSession, status, body)
			}
			if holder, fixed := sessionHolder(t, conn, rolloutSession); holder != oldEntryPrincipal || !fixed {
				t.Fatalf("turn %d: after %s served it, %s is held by %q (the configuration's hold: %t); want %s throughout",
					turn+1, broker.name, rolloutSession, holder, fixed, oldEntryPrincipal)
			}
		}
		for _, broker := range []rolloutBroker{old, twin} {
			if took := linesCarrying(broker.process.Log(), takeoverWarning); len(took) != 0 {
				t.Errorf("%s logged that it took a fixed name from its holder, where both processes fix it for the one entry they served:\n%s",
					broker.name, strings.Join(took, "\n"))
			}
		}
	})

	// The rollout. A process of the new configuration starts and serves the entry that
	// configuration re-points the name to, which takes it from the old entry and says so. Then a
	// process of the old configuration starts and serves an exchange of its own, as one restarted
	// mid-rollout would; its first exchange reserves its configuration's names, which once took
	// this one back and left the new entry refused its own name until a process restarted. After
	// it, the new entry still has the name and the old one, no longer fixed for it, does not.
	t.Run("3_AnOldProcessStartedMidRolloutLeavesTheNameWithTheNewEntry", func(t *testing.T) {
		next := start(t, "the new-configuration process", newEntryAccount)
		status, body := exchange(t, next, newEntryAccount, rolloutSession)
		if status != http.StatusOK || body["access_token"] == nil {
			t.Fatalf("%s answered a pod of %s asking for %s, which its configuration re-points to that account, %d %v; want 200 with an access token",
				next.name, newEntryAccount, rolloutSession, status, body)
		}
		if holder, fixed := sessionHolder(t, conn, rolloutSession); holder != newEntryPrincipal || !fixed {
			t.Fatalf("after %s served the re-pointed entry, %s is held by %q (the configuration's hold: %t); want %s",
				next.name, rolloutSession, holder, fixed, newEntryPrincipal)
		}
		// The take is logged, naming both principals. This is also what makes the absence of the
		// line anywhere else in this test mean something: it is the line a take logs.
		next.process.Await(t, takeoverWarning)
		if took := linesCarrying(next.process.Log(), takeoverWarning); len(took) != 1 ||
			!strings.Contains(took[0], "from="+oldEntryPrincipal) || !strings.Contains(took[0], "to="+newEntryPrincipal) {
			t.Fatalf("%s logged %d takeovers %q; want exactly one, from %s to %s", next.name, len(took), took, oldEntryPrincipal, newEntryPrincipal)
		}

		restarted := start(t, "an old-configuration process restarted mid-rollout", oldEntryAccount)
		if status, body := exchange(t, restarted, podAccount, "example-worker-session"); status != http.StatusOK || body["access_token"] == nil {
			t.Fatalf("%s answered the first exchange it served, a pod of %s under a name of its own, %d %v; want 200 — that first exchange is when it reserves, and one it refused would say nothing about what reserving did",
				restarted.name, podAccount, status, body)
		}
		if holder, fixed := sessionHolder(t, conn, rolloutSession); holder != newEntryPrincipal || !fixed {
			t.Errorf("after %s served its first exchange, %s is held by %q (the configuration's hold: %t); want it still %s's: reserving at an old-configuration process takes no name another principal holds",
				restarted.name, rolloutSession, holder, fixed, newEntryPrincipal)
		}
		if status, body := exchange(t, next, newEntryAccount, rolloutSession); status != http.StatusOK || body["access_token"] == nil {
			t.Errorf("after %s started and served, %s answered a pod of %s asking for %s, which its configuration fixes for that account, %d %v; want 200 — the entry a configuration fixes a name for is never refused its own name by a process started with another",
				restarted.name, next.name, newEntryAccount, rolloutSession, status, body)
		}
		if status, body := exchange(t, next, oldEntryAccount, rolloutSession); !refusedTheName(status, body) {
			t.Errorf("after %s started and served, %s answered a pod of %s, for which its configuration fixes no name, asking for %s, %d %v; want 400 invalid_grant naming the name another identity's",
				restarted.name, next.name, oldEntryAccount, rolloutSession, status, body)
		}
		log := restarted.process.Log()
		if took := linesCarrying(log, takeoverWarning); len(took) != 0 {
			t.Errorf("%s logged that it took a fixed name from its holder:\n%s", restarted.name, strings.Join(took, "\n"))
		}
		if left := linesCarrying(log, leftWarning); len(left) != 1 ||
			!strings.Contains(left[0], "holder="+newEntryPrincipal) || !strings.Contains(left[0], "entry="+oldEntryPrincipal) {
			t.Errorf("%s logged %d names left with their holder %q; want exactly one, %s left with %s until %s claims it",
				restarted.name, len(left), left, rolloutSession, newEntryPrincipal, oldEntryPrincipal)
		}
	})
}
