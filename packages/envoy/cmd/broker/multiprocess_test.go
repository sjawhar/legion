//go:build multiprocess

// Two secrets-broker processes serving one Postgres, which is what the broker's Deployment runs
// once it carries more than one replica. Every row here starts real cmd/broker binaries and
// drives them over real HTTP, because each thing it checks is a property of two processes
// together and of nothing either of them can show on its own: one refresh token redeemed against
// both at the same moment, one device code exchanged against both at the same moment, and two
// processes migrating one empty database at the same moment.
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
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
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
	// multiprocessKMSKey is the key the fake secrets file says every secret is encrypted with. It
	// must be exactly productionEnv's BROKER_SECRETS_KMS_KEY_ARN, which every broker here is
	// started with: the policy loader refuses a secret on any other key, so a mismatch shows up
	// as the fixture's one secret never being served.
	multiprocessKMSKey = "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
)

// migrateTimeout bounds how long a process of a pair gets to log "broker listening". It is far
// past listenTimeout because the two processes start together against an unmigrated database and
// one of them waits on the other's migration advisory lock (store.go's pg_advisory_xact_lock),
// a wait that is unbounded by design.
const migrateTimeout = 2 * time.Minute

// brokerPair is two cmd/broker processes serving one database, one fake secrets file and one
// BROKER_PUBLIC_URL, with a client pinned to each.
type brokerPair struct {
	// DatabaseURL is the DSN both processes were started with, which a row connects to directly
	// to read a table no broker route exposes.
	DatabaseURL string
	// Processes and Clients are index-matched: Clients[i] drives Processes[i] and nothing else.
	Processes [2]*brokerProcess
	Clients   [2]*brokertest.Client
}

// startBrokerPair starts two brokers from binary on a database of this test's own — a whole
// database rather than a schema, because the Postgres channel the reread fan-out listens on and
// the advisory lock the migration takes are both database-wide (storetest.DatabaseURL) — and
// returns once both have logged the address they bound.
//
// Neither start waits for its own listening line: both processes are launched before either has
// migrated, which is the state the migration row is about and which a staggered start would hide.
func startBrokerPair(t *testing.T, binary string) *brokerPair {
	t.Helper()
	pair := &brokerPair{DatabaseURL: storetest.DatabaseURL(t)}
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": [
		{"name": "example/agent-secrets/`+multiprocessSecretSlug+`", "kms_key_id": "`+multiprocessKMSKey+`", "tags": {"owner": "`+multiprocessPerson+`", "tier": "agent"}, "value": "`+multiprocessSecretValue+`"}
	]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range pair.Processes {
		pair.Processes[i] = startBroker(t, brokerOptions{
			Binary:             binary,
			NoWaitForListening: true,
			Env: []string{
				"BROKER_DATABASE_URL=" + pair.DatabaseURL,
				"BROKER_LISTEN_ADDR=127.0.0.1:0",
				"BROKER_PUBLIC_URL=" + multiprocessPublicURL,
				"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
				// Nothing here is in flight when a row ends, so a drain would only make every
				// row's cleanup wait out a budget it has no use for.
				"BROKER_DRAIN_SECONDS=0",
			},
		})
	}
	for i, process := range pair.Processes {
		process.AwaitListening(t, migrateTimeout)
		client := brokertest.NewClient(t, multiprocessPublicURL, process.Addr)
		// The binary under test takes its own UI bearer and its own DPoP signer; brokertest
		// supplies neither, for the reasons its own fields give.
		client.UIToken = testUIToken
		client.DPoP = helper.NewDPoPTransport
		pair.Clients[i] = client
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
	conn, err := pgx.Connect(context.Background(), p.DatabaseURL)
	if err != nil {
		t.Fatalf("connect to the brokers' database: %v", err)
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

// TestTwoBrokers drives two real broker processes on one database. Its rows share one pair,
// because what each of them races is a record of its own and no row leaves the shared state the
// next one reads in any other condition; the one row that cannot share — two processes migrating
// a database neither has migrated yet — starts a pair of its own on a fresh database and says so.
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
	t.Run("3_TwoProcessesMigratingAtOnce", func(t *testing.T) {
		fresh := startBrokerPair(t, binary)
		for i, process := range fresh.Processes {
			if process.Addr == "" {
				t.Fatalf("process %d never logged the address it bound:\n%s", i, process.Log())
			}
			requireHealthy(t, process.Addr)
		}

		freshConn := fresh.database(t)
		versions := countRows(t, freshConn, `select count(*) from broker_schema_migrations`)
		distinct := countRows(t, freshConn, `select count(distinct version) from broker_schema_migrations`)
		switch {
		case versions == 0:
			t.Fatal("two brokers booted on an empty database and broker_schema_migrations holds nothing")
		case versions != distinct:
			t.Fatalf("broker_schema_migrations holds %d rows for %d versions, want each version exactly once", versions, distinct)
		}
	})
}
