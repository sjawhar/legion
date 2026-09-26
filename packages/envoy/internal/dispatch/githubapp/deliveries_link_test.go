package githubapp

import (
	"strings"
	"testing"
)

// The next page is fetched with the App's JWT, so a Link header naming another origin is refused
// rather than followed; a header with no rel="next" is the last page.
func TestNextLinkStaysOnTheAPIOrigin(t *testing.T) {
	const base = "https://api.github.com"
	next, err := nextLink(`<https://api.github.com/app/hook/deliveries?cursor=v1_2&per_page=100>; rel="next", <https://api.github.com/app/hook/deliveries?per_page=100>; rel="first"`, base)
	if err != nil || next != "https://api.github.com/app/hook/deliveries?cursor=v1_2&per_page=100" {
		t.Fatalf("next page: got %q, %v", next, err)
	}
	if next, err := nextLink(`<https://api.github.com/app/hook/deliveries?per_page=100>; rel="first"`, base); err != nil || next != "" {
		t.Fatalf("last page: got %q, %v, want no next page", next, err)
	}
	if _, err := nextLink(`<https://collector.example/app/hook/deliveries?cursor=v1_2>; rel="next"`, base); err == nil || !strings.Contains(err.Error(), "leaves the API origin") {
		t.Fatalf("foreign next page: got %v, want a refusal", err)
	}
}
