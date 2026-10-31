package docs

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// admittedSocket is one document socket's own upgrade request context and the connection state
// that context carries: the answer to the token that socket sends reads the admission authorize
// recorded on that state (authorizeSchemaVersion).
type admittedSocket struct {
	ctx        context.Context
	connection *connectionState
}

// captureAdmissions records the upgrade request context and connection state of every socket the
// service admits from here on, in the order their upgrades reach it. Each is sent once its
// authorize has returned, so a test that takes one is ordered after the admission it records.
func captureAdmissions(t *testing.T, service *Service) <-chan admittedSocket {
	t.Helper()
	admitted := make(chan admittedSocket, 4)
	authorize := service.srv.Authorize
	service.srv.Authorize = func(r *http.Request) (ygws.ConnectionConfig, bool) {
		config, ok := authorize(r)
		connection, _ := r.Context().Value(connectionContextKey{}).(*connectionState)
		admitted <- admittedSocket{ctx: r.Context(), connection: connection}
		return config, ok
	}
	return admitted
}

// awaitAdmissionRemoved waits, for up to five seconds, until the service has forgotten admission:
// its socket's ServeHTTP has returned and removed it.
func awaitAdmissionRemoved(t *testing.T, service *Service, admission *schemaAdmission) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		service.schemaAdmissions.mu.Lock()
		_, held := service.schemaAdmissions.sockets[admission]
		service.schemaAdmissions.mu.Unlock()
		if !held {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the revoked socket's ServeHTTP did not remove its admission within five seconds")
		}
	}
}

// Two pages of one room presenting one schema_version share a token, and a re-check can revoke
// one of them while its token is still on its way: ygo calls the token hook on the read loop as
// soon as it reads the token frame, and the revoke's past read deadline stops only later reads,
// so a frame read before the revoke still reaches the hook afterwards. After a rollback the page
// creating up to version 2 is no longer admitted and the page creating up to 1 stays writable.
// The revoked socket's token is the first to arrive, and its own ServeHTTP then returns and
// forgets its admission; the page the fleet still admits sends its token after that, and must be
// answered read-write. An answer matched to the two sockets' shared token rather than to the
// socket that sent it cannot do this in both orders of admission: whichever entry of the pair it
// takes first, the other order leaves a page admitted writable read-only for the life of its
// socket. So both orders are run here. The revoked page's own read-only reconnect is admitted
// beside them, since no socket's answer is another's to take.
//
// The live socket's token goes over the wire, through ygo's hook; the revoked socket's cannot,
// since its read loop has ended, so it is asked of the service with that socket's own request
// context, which is what ygo's hook hands it.
func TestARevokedSocketsTokenLeavesASocketTheFleetStillAdmitsWritable(t *testing.T) {
	for _, revokedFirst := range []bool{true, false} {
		name := "the revoked page's socket admitted second"
		if revokedFirst {
			name = "the revoked page's socket admitted first"
		}
		t.Run(name, func(t *testing.T) {
			service, artifactID, server := newAdmissionServer(t, false)
			admitted := captureAdmissions(t, service)
			// A live process of the newer build, which creates up to version 2: both pages are
			// writable while it serves the database.
			fleetProcess(t, service, 2, 2, 0)
			queries := []url.Values{page(2, 1), page(2, 2)}
			if revokedFirst {
				queries = []url.Values{page(2, 2), page(2, 1)}
			}
			var newerPage, olderPage *gws.Conn
			var newerSocket admittedSocket
			for _, query := range queries {
				connection := mustDialPage(t, server.URL, artifactID, query)
				socket := <-admitted
				if !probeWritable(t, connection, artifactID) {
					t.Fatalf("a page presenting %v was accepted read-only; both pages are writable while the newer build's process serves", query)
				}
				if query.Get("schema_creatable") == "2" {
					newerPage, newerSocket = connection, socket
				} else {
					olderPage = connection
				}
			}

			// The rollback stops the newer build's process, and the next re-check closes the
			// newer page's socket with 1012.
			if _, err := service.store.Pool.Exec(context.Background(), `delete from server_processes`); err != nil {
				t.Fatalf("stop the newer build's process: %v", err)
			}
			if err := service.recheckSchemaAdmissions(context.Background()); err != nil {
				t.Fatalf("re-check the sockets' admission: %v", err)
			}
			requireServiceRestartClose(t, newerPage, "the newer page's socket")

			// The revoked socket's token, read before the revoke, is answered first.
			if config, err := service.authorizeSchemaVersion(newerSocket.ctx, artifactID, "2"); err != nil || !config.ReadOnly {
				t.Errorf("the revoked socket's late token = %#v, %v; want read-only", config, err)
			}
			// Its ServeHTTP then returns and forgets its admission.
			awaitAdmissionRemoved(t, service, newerSocket.connection.admission)

			if !tokenWritable(t, olderPage, artifactID, "2") {
				t.Error("the socket the fleet still admits writable was answered read-only once the revoked socket's token had its answer")
			}
			if socket, authenticated := admission(t, server.URL, artifactID, page(2, 2), "2"); socket || authenticated {
				t.Errorf("the revoked page's reconnect was admitted writable=%t by its socket and %t by its token, want read-only",
					socket, authenticated)
			}
		})
	}
}
