package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

func (s *server) getBlockSchema(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(pmdoc.SchemaJSON())
}
