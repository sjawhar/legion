package githubrest

import (
	"context"
	"fmt"
	"net/url"
)

// ChangedFile is one file GitHub lists in a change, a pull request's or a comparison's.
type ChangedFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
}

// ChangedPaths is every path files touch: each file's name, and a renamed file's old name too,
// since moving a file out of a path changes that path.
func ChangedPaths(files []ChangedFile) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Filename)
		if file.PreviousFilename != "" {
			paths = append(paths, file.PreviousFilename)
		}
	}
	return paths
}

// CompareFilesLimit is the most changed files GitHub's compare of two commits lists: "it includes
// up to 300 changed files for the entire comparison" (REST API, Compare two commits,
// https://docs.github.com/en/rest/commits/commits#compare-two-commits). A comparison that reaches
// it may change a path the list never shows.
const CompareFilesLimit = 300

// Comparison is GitHub's compare of two commits: its status (ahead when the head is the base with
// commits on top, identical, behind or diverged), and the files the comparison changes, nil when
// GitHub answered no list.
type Comparison struct {
	Status string         `json:"status"`
	Files  *[]ChangedFile `json:"files"`
}

// Compare reads GitHub's compare of base with head (GET /compare/{base}...{head}) under the
// client's repository. It asks for one commit a page: the status and the file list cover the whole
// comparison on the first page whatever the page size, and a smaller page reads less. A commit
// GitHub does not know is its 404 Answer.
func (c Client) Compare(ctx context.Context, base, head string) (Comparison, error) {
	var comparison Comparison
	if err := c.Get(ctx, fmt.Sprintf("/compare/%s...%s?per_page=1", url.PathEscape(base), url.PathEscape(head)), &comparison); err != nil {
		return Comparison{}, err
	}
	return comparison, nil
}
