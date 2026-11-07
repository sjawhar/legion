package docs

import (
	"strconv"

	"github.com/reearth/ygo/crdt"
)

// This file holds what a room reads of the clients a rebuild of its document retired: the list a
// rebuild's seed wrote into the fresh history (rebuiltSeed), which the room's load reads once
// (keepReplica) and the peer update hook refuses a browser's old history against
// (Service.weighPeerUpdate).

// maxEditorClock is 2^53, past which no JavaScript number counts every integer: no clock the
// editor writes, and none a rebuild records for a client it retired, ends beyond it.
const maxEditorClock = 1 << 53

// retiredClientsOf is the clients doc's history lists under retiredMapName, each with the clock it
// had reached when a rebuild replaced the history it belonged to (rebuiltSeed), or nil for a
// history that lists none. An entry that does not read as a client and a clock is not the
// rebuild's, and is skipped. It reads the map itself, so it costs no encode and no walk of the
// document: the list holds one entry for each client of the history the rebuild replaced.
func retiredClientsOf(doc *crdt.Doc) map[crdt.ClientID]uint64 {
	var retired map[crdt.ClientID]uint64
	doc.GetMap(retiredMapName).ForEach(func(key string, value any) {
		client, err := strconv.ParseUint(key, 10, 64)
		clock, isClock := retiredClock(value)
		if err != nil || !isClock {
			return
		}
		if retired == nil {
			retired = make(map[crdt.ClientID]uint64)
		}
		retired[crdt.ClientID(client)] = clock
	})
	return retired
}

// retiredClock reads a retired client's clock as ygo decodes the int64 a rebuild wrote: lib0 Any
// writes an integer within 32 bits as a varint, which decodes as an int64, and a larger one within
// 2^53 as a float64 (sjawhar/ygo v1.55.1-sami.1, encoding/encoder.go:283-299,
// internal/anycodec/decoder.go:276-296). A clock of zero retires nothing.
func retiredClock(value any) (uint64, bool) {
	switch value := value.(type) {
	case int64:
		return uint64(value), value >= 1 && value <= maxEditorClock
	case float64:
		return uint64(value), value >= 1 && value <= maxEditorClock
	}
	return 0, false
}
