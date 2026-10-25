package api

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// brokerEventInsert is the one source file allowed to insert an event.
const brokerEventInsert = "internal/dispatch/events/broker.go"

var eventInsertStatement = regexp.MustCompile(`\binsert into events\b`)

// TestOnlyTheBrokerInsertsEvents enforces what the live event feed rests on: every row of the
// event log is written by events.Broker.Append, which takes the database-wide commit-order lock
// before its insert and holds it until the transaction commits. That is why events.id is commit
// order, and why the relay (event_stream_relay.go) can read `id > cursor` and skip nothing: no
// row at or below a committed max(id) can still be uncommitted, so no row can appear behind the
// cursor after the cursor has passed it. The same fact is what lets an events subscription
// (live_events.go) drop a live event at or below its fixed since.
//
// An insert written anywhere else would take no such lock. Its row could commit after the relay
// had read past its id, and then reach no open subscription at all until the page subscribed
// again -- a missing comment or ask on an open page, with nothing to show for it in any log.
//
// Its reach is the statement as the module scan can assemble it (TestNoForUpdateOnOwnerTables):
// string literals and chains of literals and package-level constants, in every non-test Go file
// of the module. A test inserts events directly on purpose -- a backlog to page through, a row
// shape to serve -- so test sources are outside it, as they are outside that scan.
func TestOnlyTheBrokerInsertsEvents(t *testing.T) {
	sources := moduleSources(t, "../../..")
	constants, err := stringConstants(sources)
	if err != nil {
		t.Fatal(err)
	}
	var offences []string
	broker := 0
	for _, source := range sources {
		path := filepath.ToSlash(source.path)
		for _, statement := range sqlStatements(source.file, constants) {
			if !eventInsertStatement.MatchString(statement) {
				continue
			}
			if strings.HasSuffix(path, brokerEventInsert) {
				broker++
				continue
			}
			offences = append(offences, path+": "+statement)
		}
	}
	if len(offences) > 0 {
		t.Fatalf("only %s may insert into events, and these statements also do:\n\t%s\n"+
			"Append the event through events.Broker.Append instead. Its commit-order lock is what "+
			"makes events.id commit order, and the live event stream reads the log by id: a row "+
			"inserted without that lock can commit behind the relay's cursor and reach no open "+
			"stream.", brokerEventInsert, strings.Join(offences, "\n\t"))
	}
	if broker != 1 {
		t.Fatalf("%s holds %d event inserts the scan can see, want 1: if the insert moved or was "+
			"written around a value this scan cannot resolve, this guard now passes by seeing "+
			"nothing", brokerEventInsert, broker)
	}
}
