package appauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// Whether an App is installed on a repository is GitHub's answer to the App's own JWT on GET
// /repos/{owner}/{repo}/installation: 2xx is installed, 404 is not, and anything else is no
// answer, transient when GitHub's trouble. It is read per repository, as an installation on an
// owner may cover only some of its repositories.
func TestInstalledOnReadsTheRepositorysInstallationForEachApp(t *testing.T) {
	key, privatePEM := testAppKey(t)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		status    int
		want      bool
		wantErr   string
		transient bool
	}{
		{name: "installed", status: http.StatusOK, want: true},
		{name: "not installed", status: http.StatusNotFound},
		{name: "GitHub's trouble", status: http.StatusBadGateway, wantErr: "(502)", transient: true},
		{name: "a refusal", status: http.StatusForbidden, wantErr: "(403)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked = append(asked, r.Method+" "+r.URL.Path)
				verifyAppJWT(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), &key.PublicKey, now, "67890")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"id": 41}`))
			}))
			defer server.Close()
			tokens := New(config.GitHubApps{
				Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
				Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
			}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})

			got, err := tokens.InstalledOn(context.Background(), Review, ghrepo.MustParse("acme/widgets"))
			if want := []string{"GET /repos/acme/widgets/installation"}; len(asked) != 1 || asked[0] != want[0] {
				t.Fatalf("GitHub was asked %v, want %v", asked, want)
			}
			if tc.wantErr == "" {
				if err != nil || got != tc.want {
					t.Fatalf("InstalledOn = %v, %v; want %v", got, err, tc.want)
				}
				return
			}
			var transient *TransientError
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "acme/widgets") || errors.As(err, &transient) != tc.transient {
				t.Fatalf("InstalledOn = %v, %v; want an error naming acme/widgets and %s, transient %v", got, err, tc.wantErr, tc.transient)
			}
		})
	}
}

// An App's bot login is its slug's, read under the App's own JWT (GET /app): it needs no
// installation on any repository, so a daemon serving no repository yet still knows the review
// App's login the engine judges pushes and reviews by. GitHub's trouble is transient.
func TestAppLoginReadsTheAppsSlugUnderItsOwnJWT(t *testing.T) {
	key, privatePEM := testAppKey(t)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/app" {
			t.Errorf("GitHub was asked %s %s, want GET /app alone", r.Method, r.URL.Path)
		}
		verifyAppJWT(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), &key.PublicKey, now, "67890")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"slug":"legion-reviewer"}`))
	}))
	defer server.Close()
	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})

	if login, err := tokens.AppLogin(context.Background(), Review); err != nil || login != "legion-reviewer[bot]" {
		t.Fatalf("AppLogin = %q, %v; want legion-reviewer[bot]", login, err)
	}
	status = http.StatusBadGateway
	var transient *TransientError
	if _, err := tokens.AppLogin(context.Background(), Review); !errors.As(err, &transient) {
		t.Fatalf("AppLogin on GitHub's 502 = %v, want a transient error", err)
	}
}
