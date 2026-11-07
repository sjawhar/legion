package githubrest

import (
	"context"
	"fmt"
	"net/url"
)

// ChangedFile is one file GitHub lists in a change, a pull request's or a comparison's: the name
// it has after the change, and the name it had before, which GitHub fills for a rename alone.
// What the change did to the file (GitHub's "status": added, removed, modified, renamed) is read
// nowhere, so it is not decoded: READY judges a compare entry by its paths alone
// (packages/daemon/internal/api/ready.go, underLegion).
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
