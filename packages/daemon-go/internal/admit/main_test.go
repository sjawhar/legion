package admit

import (
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testnats"
)

// TestMain removes the NATS container the package's tests share (testnats.Main).
func TestMain(m *testing.M) { os.Exit(testnats.Main(m)) }
