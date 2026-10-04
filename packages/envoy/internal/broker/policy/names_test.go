package policy_test

import (
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
)

func TestNameSlugRoundTrip(t *testing.T) {
	for name, slug := range map[string]string{"DEEL_API_KEY": "deel-api-key", "A0": "a0", "A_B": "a-b"} {
		got, err := policy.NameToSlug(name)
		if err != nil || got != slug {
			t.Fatalf("NameToSlug(%q) = %q, %v; want %q", name, got, err, slug)
		}
		if back := policy.SlugToName(slug); back != name {
			t.Fatalf("SlugToName(%q) = %q, want %q", slug, back, name)
		}
	}
	for _, bad := range []string{"deel_api_key", "_X", "A__B", "A-B", ""} {
		if _, err := policy.NameToSlug(bad); !errors.Is(err, policy.ErrNameInvalid) {
			t.Fatalf("NameToSlug(%q) = %v, want ErrNameInvalid", bad, err)
		}
	}
}
