package redeliver_test

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/testnats"
)

func TestMain(m *testing.M) { os.Exit(testnats.Main(m)) }
