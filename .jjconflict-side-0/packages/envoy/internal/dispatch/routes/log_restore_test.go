package routes

import (
	"log"
	"testing"
)

// A test that captures the default logger must give the log package its output and flags back
// when it ends: slog.SetDefault points log's output at the capturing handler and clears its
// flags, and setting the previous slog default back undoes neither, so every log.Print after
// the test would write into its finished buffer instead of stderr (internal/logtest).
func TestAssetTestsGiveTheLogPackageItsOutputBack(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"TestStaticHandlerRetainedAssets", TestStaticHandlerRetainedAssets},
		{"TestStaticHandlerWritesNoResponseWhenTheClientCancelsWhileTheStoreReads", TestStaticHandlerWritesNoResponseWhenTheClientCancelsWhileTheStoreReads},
	} {
		output, flags := log.Writer(), log.Flags()
		t.Run(test.name, test.run)
		if log.Writer() != output || log.Flags() != flags {
			t.Errorf("after %s ended, log writes to %T with flags %d; want %T with flags %d", test.name, log.Writer(), log.Flags(), output, flags)
		}
	}
}
