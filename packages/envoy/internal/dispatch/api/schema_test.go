package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

func TestBlockSchemaRouteServesEmbeddedSchema(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, t.Context(), Deps{})

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/schema/blocks", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/schema/blocks status = %d, body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("schema content type = %q, want application/json", got)
	}
	if response.Body.String() != string(pmdoc.SchemaJSON()) {
		t.Fatalf("schema response differs from embedded schema\nresponse: %s\nembedded: %s", response.Body.String(), pmdoc.SchemaJSON())
	}
	var schema pmdoc.BlockSchema
	if err := json.Unmarshal(response.Body.Bytes(), &schema); err != nil {
		t.Fatalf("decode schema response: %v", err)
	}
	names := make([]string, len(schema.Types))
	for index, typ := range schema.Types {
		names[index] = typ.Name
	}
	if schema.Version != 4 || !slices.Equal(names, []string{"callout", "ask", "decision", "interpretation", "requirement", "plan_task", "check"}) {
		t.Fatalf("schema = version %d, types %v; want version 4 and callout, ask, decision, interpretation, requirement, plan_task, check", schema.Version, names)
	}
}
