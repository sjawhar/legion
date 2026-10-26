package policy_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// TestParseGitHubLoginReadsOneLogin: one GitHub login, lowercased, and nothing else: a second
// login, even the same one again, is no login, nor is white space around it, which the caller
// trims, nor the word none.
func TestParseGitHubLoginReadsOneLogin(t *testing.T) {
	for raw, want := range map[string]string{"Ada": "ada", "x-": "x-", "A-b-C9": "a-b-c9", strings.Repeat("a", 39): strings.Repeat("a", 39)} {
		if got, err := policy.ParseGitHubLogin(raw); err != nil || got != want {
			t.Fatalf("ParseGitHubLogin(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "ada ada", "Ada ADA", "ada acme", " ada", "-ada", "a_b", "none", "NONE", strings.Repeat("a", 40)} {
		if got, err := policy.ParseGitHubLogin(raw); !errors.Is(err, policy.ErrGitHubOwnersLogin) {
			t.Fatalf("ParseGitHubLogin(%q) = %q, %v; want ErrGitHubOwnersLogin", raw, got, err)
		}
	}
}

// TestParseGitHubNarrowingAnswersTheOneCanonicalForm: a narrowing as a caller writes it is checked
// and answered in the form CanonicalGitHubNarrowing gives it, the one both the CLI and the broker
// build: the owner trimmed and lowercased, the repositories sorted, each once.
func TestParseGitHubNarrowingAnswersTheOneCanonicalForm(t *testing.T) {
	want := secrets.GitHubNarrowing{Owner: "ada", Repositories: []string{"proofs", "widgets"}, ReadOnly: true}
	got, err := policy.ParseGitHubNarrowing(" Ada ", []string{"widgets", "proofs", "widgets"}, true)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseGitHubNarrowing = %+v, %v; want %+v", got, err, want)
	}
	if canonical := policy.CanonicalGitHubNarrowing(" Ada ", []string{"widgets", "proofs", "widgets"}, true); !reflect.DeepEqual(canonical, want) {
		t.Fatalf("CanonicalGitHubNarrowing = %+v; want %+v", canonical, want)
	}
	if got, err := policy.ParseGitHubNarrowing("ada", nil, false); err != nil || got.Repositories != nil {
		t.Fatalf("ParseGitHubNarrowing with no repositories = %+v, %v; want no repositories", got, err)
	}
}

// TestParseGitHubNarrowingRefusesWhatGitHubCannotHold: the owner is one GitHub login and each
// repository a name GitHub can hold, at most 500 of them; each refusal opens with the field it
// names, so the API can name it as its body's.
func TestParseGitHubNarrowingRefusesWhatGitHubCannotHold(t *testing.T) {
	tooMany := make([]string, 501)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("repo-%d", i)
	}
	for _, tc := range []struct {
		name         string
		owner        string
		repositories []string
		field        string
		mentions     string
	}{
		{"no owner", "", nil, "owner", `""`},
		{"two owners", "ada acme", nil, "owner", "ada acme"},
		{"one owner twice", "ada ada", nil, "owner", "ada ada"},
		{"an owner no login can be", "-ada", nil, "owner", "-ada"},
		{"a repository with its owner", "ada", []string{"ada/proofs"}, "repositories", "ada/proofs"},
		{"a repository named .", "ada", []string{"."}, "repositories", `"."`},
		{"a repository named ..", "ada", []string{".."}, "repositories", `".."`},
		{"an empty repository name", "ada", []string{""}, "repositories", `""`},
		{"501 repositories", "ada", tooMany, "repositories", "500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := policy.ParseGitHubNarrowing(tc.owner, tc.repositories, true)
			if err == nil || !strings.HasPrefix(err.Error(), tc.field+" ") || !strings.Contains(err.Error(), tc.mentions) {
				t.Fatalf("ParseGitHubNarrowing(%q, %d repositories) = %+v, %v; want an error opening with %q and naming %q", tc.owner, len(tc.repositories), got, err, tc.field, tc.mentions)
			}
		})
	}
}
