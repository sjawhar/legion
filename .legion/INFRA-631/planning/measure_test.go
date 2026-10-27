package pmdoc_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

func TestMeasureGuides(t *testing.T) {
	for _, p := range []string{"/legion/workspaces/sjawhar/legion/infra-631/packages/envoy/AGENTS.md", "/legion/workspaces/sjawhar/legion/infra-631/packages/envoy/cmd/dispatch/README.md", "/legion/scratch/tmp/p631/ml/t2_agents.md", "/legion/scratch/tmp/p631/ml/t2_readme.md"} {
		b, err := os.ReadFile(p)
		if err != nil { t.Fatal(err) }
		fmt.Printf("%s %+v\n", p, pmdoc.MeasureDocument(string(b)))
	}
}
