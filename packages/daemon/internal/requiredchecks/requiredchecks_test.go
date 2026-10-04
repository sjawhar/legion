package requiredchecks

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// GitHub pages a branch's rules, 30 to a page unless asked for more: a required_status_checks rule
// on a later page is required as much as one on the first, so a reader that stops at one page
// fails open. The stand-in serves 30 other rules on page 1 and the required one on page 2, and
// answers whatever page size the reader asks for with 30 rules a page all the same.
func TestRequiredReadsEveryPageOfTheBranchsRules(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widgets/rules/branches/main":
			if r.URL.Query().Get("page") == "2" {
				w.Write([]byte(`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"pr-checks-result"}]}}]`))
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/repos/acme/widgets/rules/branches/main?page=2>; rel="next", <http://%s/repos/acme/widgets/rules/branches/main?page=2>; rel="last"`, r.Host, r.Host))
			w.Write([]byte("[" + strings.TrimSuffix(strings.Repeat(`{"type":"pull_request"},`, 30), ",") + "]"))
		case "/repos/acme/widgets/branches/main":
			w.Write([]byte(`{"name":"main","protected":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	required, err := Required(context.Background(), githubrest.Client{Token: "token", API: server.URL + "/repos/acme/widgets"}, "main")
	if err != nil || !slices.Equal(required, []string{"pr-checks-result"}) {
		t.Fatalf("Required = %#v, %v; want the rule on page 2, pr-checks-result", required, err)
	}
}
