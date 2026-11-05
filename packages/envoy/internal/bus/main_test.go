package bus

import (
	"os"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testnats"
)

// TestMain removes the NATS server the package's tests share (testnats.Main).
func TestMain(m *testing.M) { os.Exit(testnats.Main(m)) }

// busWait is how long a test of this package waits FOR AN EVENT: a delivery reaching its handler,
// a reconnect, a publish being answered, a watch ending. It is where the test gives up, never a
// measurement - a longer wait proves nothing less, since the test still fails when the event never
// comes, and on an idle server it returns in milliseconds. Every package of this module now runs
// at once, four CI cores to some 3,800 parallel tests and the race detector besides, so what took
// milliseconds alone takes tens of seconds here. A test whose SUBJECT is a span - Drain closing at
// its deadline, a request window, a publish's own deadline, HoldsSeed's cost - keeps its own
// number and does not use this (dispatch://INFRA-1075).
const busWait = 30 * time.Second
