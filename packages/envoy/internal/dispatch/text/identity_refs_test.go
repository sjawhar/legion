package text

import (
	"reflect"
	"testing"
)

func TestIdentityReferencesUseCanonicalPeopleAndOpaqueSessionAndRoleIDs(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  Ref
		ok    bool
	}{
		{"dispatch://people/%20ALICE%40Example.com%20", Ref{Kind: "person", ID: "alice@example.com"}, true},
		{"dispatch://sessions/pi-worker-1", Ref{Kind: "session", ID: "pi-worker-1"}, true},
		{"dispatch://roles/reviewer", Ref{Kind: "role", ID: "reviewer"}, true},
		{"dispatch://people/%00", Ref{}, false},
		{"dispatch://sessions/a%2Fb", Ref{}, false},
		{"dispatch://roles/a/b", Ref{}, false},
		{"dispatch://people/alice#b-fake", Ref{}, false},
		{"dispatch://sessions/%2A", Ref{}, false},
		{"dispatch://roles/review%2A", Ref{}, false},
		{"dispatch://sessions/pi.worker-1", Ref{}, false},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, ok := ParseRef(tc.input, "https://dispatch.test")
			if ok != tc.ok || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseRef = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}
