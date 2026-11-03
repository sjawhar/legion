package githubrest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// Compare asks for one commit a page, since the status and the file list cover the whole comparison
// on the first page whatever its size, and a page of up to 250 commits is read for nothing. It
// tells a comparison with no file list from one with an empty list, and a commit GitHub does not
// know is its 404 Answer.
func TestCompareReadsOneCommitAPageAndKeepsAMissingListMissing(t *testing.T) {
	answers := map[string]string{
		"/repos/acme/widgets/compare/code...handoff": `{"status":"ahead","commits":[{}],"files":[{"filename":".legion/A-1/review.json"},{"filename":"src/new.go","previous_filename":"src/old.go"}]}`,
		"/repos/acme/widgets/compare/code...nolist":  `{"status":"ahead"}`,
		"/repos/acme/widgets/compare/code...empty":   `{"status":"identical","files":[]}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("per_page"); got != "1" {
			t.Errorf("compare %s asked for per_page=%q, want 1", r.URL.Path, got)
		}
		answer, ok := answers[r.URL.Path]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		w.Write([]byte(answer))
	}))
	t.Cleanup(server.Close)
	github := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}

	got, err := github.Compare(context.Background(), "code", "handoff")
	if err != nil || got.Status != "ahead" || got.Files == nil {
		t.Fatalf("Compare(code, handoff) = %+v, %v; want ahead with a file list", got, err)
	}
	if paths := ChangedPaths(*got.Files); !slices.Equal(paths, []string{".legion/A-1/review.json", "src/new.go", "src/old.go"}) {
		t.Fatalf("ChangedPaths = %q, want each name and a rename's old name", paths)
	}
	if got, err := github.Compare(context.Background(), "code", "nolist"); err != nil || got.Files != nil {
		t.Fatalf("Compare(code, nolist) = %+v, %v; want no file list", got, err)
	}
	if got, err := github.Compare(context.Background(), "code", "empty"); err != nil || got.Files == nil || len(*got.Files) != 0 {
		t.Fatalf("Compare(code, empty) = %+v, %v; want an empty file list", got, err)
	}
	var answer *Answer
	if _, err := github.Compare(context.Background(), "unknown", "handoff"); !errors.As(err, &answer) || answer.Status != http.StatusNotFound {
		t.Fatalf("Compare(unknown, handoff) = %v, want GitHub's 404", err)
	}
}
