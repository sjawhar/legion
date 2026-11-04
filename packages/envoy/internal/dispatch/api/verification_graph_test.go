package api

import (
 "net/http"
 "testing"

 "github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestVerificationGraphReadIsScopedToTheNamedIssuesDocument(t *testing.T) {
 handler, _, _ := newTestServer(t, testServerOptions{})
 own := createInteractionIssue(t, handler, "VGR", "Own", twoRequirements)
 other := createInteractionIssue(t, handler, "VGO", "Other project", twoRequirements)
 uploadDocument(t, handler, "/api/v1/issues/"+own.Key, "verification.md", checkServing("own", "dispatch://"+own.Key+"/spec#b-r1"))
 uploadDocument(t, handler, "/api/v1/issues/"+other.Key, "verification.md", checkServing("other", "dispatch://"+other.Key+"/spec#b-r1"))
 response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+own.Key+"/verification", nil, "alice")
 if response.Code != http.StatusOK { t.Fatalf("verification: %d %s", response.Code, response.Body.String()) }
 answer := decodeBody[struct {
  Issue string `json:"issue"`
  Checks []model.CheckBlock `json:"checks"`
  Serves map[string][]string `json:"serves"`
 }](t, response)
 if answer.Issue != own.Key || len(answer.Checks) != 1 || answer.Checks[0].BlockID != "own" {
  t.Fatalf("cross-issue data in verification: %+v", answer)
 }
 if got := answer.Serves["own"]; len(got) != 1 || got[0] != "dispatch://"+own.Key+"/spec#b-r1" { t.Fatalf("serves = %v", answer.Serves) }
 for _, path := range []string{"/api/v1/issues/"+own.Key+"/verification", "/api/v1/verification-shadow"} {
  if response := dispatchRequest(t, handler, http.MethodGet, path, nil, ""); response.Code != http.StatusUnauthorized { t.Fatalf("anonymous %s: %d", path, response.Code) }
 }
 if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/NOPE-1/verification", nil, "alice"); response.Code != http.StatusNotFound { t.Fatalf("missing issue: %d", response.Code) }
}

func assertVerificationShadow(t *testing.T, handler http.Handler) {
 t.Helper()
 response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/verification-shadow", nil, "alice")
 if response.Code != http.StatusOK { t.Fatalf("verification shadow: %d %s", response.Code, response.Body.String()) }
 report := decodeBody[struct { Mismatches int `json:"mismatches"` }](t, response)
 if report.Mismatches != 0 { t.Fatalf("verification shadow mismatch: %s", response.Body.String()) }
}
