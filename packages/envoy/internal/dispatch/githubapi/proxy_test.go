package githubapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

type fakeHTTPClient struct {
	status int
	calls  int
}

func (f *fakeHTTPClient) Do(_ *http.Request) (*http.Response, error) {
	f.calls++
	return &http.Response{
		StatusCode: f.status,
		Body:       io.NopCloser(strings.NewReader("{}")),
		Header:     make(http.Header),
	}, nil
}

func accessTestConfig(client auth.HTTPClient) *ProxyConfig {
	return &ProxyConfig{
		Tokens: &auth.Tokens{
			AccessToken:     "tok",
			AccessExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		},
		HTTPClient: client,
	}
}

func TestCheckRepoAccessAllows2xx(t *testing.T) {
	fc := &fakeHTTPClient{status: http.StatusOK}
	if err := CheckRepoAccess(context.Background(), accessTestConfig(fc), "sjawhar", "legion"); err != nil {
		t.Fatalf("expected nil error for 200, got %v", err)
	}
	if fc.calls != 1 {
		t.Errorf("expected exactly one GitHub call, got %d", fc.calls)
	}
}

func TestCheckRepoAccessDeniesNotFound(t *testing.T) {
	fc := &fakeHTTPClient{status: http.StatusNotFound}
	if err := CheckRepoAccess(context.Background(), accessTestConfig(fc), "victim", "private"); !errors.Is(err, ErrRepoForbidden) {
		t.Fatalf("expected ErrRepoForbidden for 404, got %v", err)
	}
}

func TestCheckRepoAccessDeniesForbidden(t *testing.T) {
	fc := &fakeHTTPClient{status: http.StatusForbidden}
	if err := CheckRepoAccess(context.Background(), accessTestConfig(fc), "victim", "private"); !errors.Is(err, ErrRepoForbidden) {
		t.Fatalf("expected ErrRepoForbidden for 403, got %v", err)
	}
}

func TestCheckRepoAccessSurfacesUnexpectedStatus(t *testing.T) {
	fc := &fakeHTTPClient{status: http.StatusInternalServerError}
	err := CheckRepoAccess(context.Background(), accessTestConfig(fc), "sjawhar", "legion")
	if err == nil || errors.Is(err, ErrRepoForbidden) {
		t.Fatalf("expected a non-forbidden error for 500, got %v", err)
	}
}
