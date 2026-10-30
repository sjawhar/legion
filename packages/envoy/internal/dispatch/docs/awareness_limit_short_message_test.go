package docs

import (
	"net/http"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
)

// Census entry 4 closes a document socket whose first message names no document and type within
// 512 bytes, whatever the message's length: a short message is no exception. Three bytes holding
// one variable-length integer that never ends name nothing, and the socket must close, as one
// whose 600 bytes never name a document does
// (TestADocumentSocketWhoseFirstMessageNamesNoDocumentIsClosedThroughARealUpgrade).
func TestADocumentSocketWhoseShortFirstMessageNamesNoDocumentIsClosed(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "First.\n")
	server := newTestServer(t, service)
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + artifactID
	socket, response, err := gws.DefaultDialer.Dial(url, http.Header{"X-Dispatch-User": []string{"mallory"}})
	if err != nil {
		t.Fatalf("connect a socket to document %s: response=%#v err=%v", artifactID, response, err)
	}
	t.Cleanup(func() { _ = socket.Close() })

	if err := socket.WriteMessage(gws.BinaryMessage, []byte{0xff, 0xff, 0xff}); err != nil {
		t.Fatalf("send a three-byte first message that names no document: %v", err)
	}
	if err := socket.SetReadDeadline(time.Now().Add(socketCloseWait)); err != nil {
		t.Fatalf("set the socket's read deadline: %v", err)
	}
	for {
		_, _, err := socket.ReadMessage()
		if err == nil {
			continue // the document the room sends every socket, or a keep-alive
		}
		if strings.Contains(err.Error(), "i/o timeout") {
			t.Fatalf("the socket is still open %v after a three-byte first message that names neither a document nor a type: want it closed, as census entry 4 says", socketCloseWait)
		}
		return
	}
}
