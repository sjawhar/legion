package mcp

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBearerMiddlewareAcceptsExtensionAuthorizationHeader(t *testing.T) {
	var received string
	handler := bearerMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		received = bearerFromContext(request.Context())
	}))
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Set("Authorization", "Bearer dispatch-bearer")

	handler.ServeHTTP(httptest.NewRecorder(), request)

	if received != "dispatch-bearer" {
		t.Fatalf("bearer middleware received %q, want extension bearer", received)
	}
}
