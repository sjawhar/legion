package docs

import (
	"log"
	"testing"
)

// A test that captures the default logger must give the log package its output and flags back
// when it ends: slog.SetDefault points log's output at the capturing handler and clears its
// flags, and setting the previous slog default back undoes neither, so every log.Print after
// the test would write into its finished buffer instead of stderr (internal/logtest).
//
// It runs one of the package's tests that captures the default logger and reads the log package
// back afterwards. The test it ran used to be TestRenderReuseADocumentOutsideTheSchema, which
// reads its warnings off its own service now and takes the default logger no longer, so this
// runs one that still does - and one that costs a few seconds rather than a minute
// (dispatch://INFRA-1075).
func TestRenderReuseGivesTheLogPackageItsOutputBack(t *testing.T) {
	output, flags := log.Writer(), log.Flags()
	t.Run("TestTaskProgressLockedThroughTheBootPassIsCountedByARetry", TestTaskProgressLockedThroughTheBootPassIsCountedByARetry)
	if log.Writer() != output || log.Flags() != flags {
		t.Errorf("after TestTaskProgressLockedThroughTheBootPassIsCountedByARetry ended, log writes to %T with flags %d; want %T with flags %d", log.Writer(), log.Flags(), output, flags)
	}
}
