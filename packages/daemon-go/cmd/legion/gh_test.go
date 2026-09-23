package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestGhRefusesEveryMergeShapeBeforeRedeemingAGrant(t *testing.T) {
	for _, args := range [][]string{
		{"--", "pr", "merge", "7"},
		{"--", "api", "repos/o/r/pulls/7/merge"},
		{"--", "api", "repos/o/r/merges"},
		{"--", "api", "graphql", "-f", "query=mutation { mergePullRequest(input:{}) { clientMutationId } }"},
		{"--", "api", "graphql", "-F", "query=@/tmp/mutation.graphql"},
		{"--", "alias", "set", "m", "pr merge"},
		{"--", "extension", "install", "merge-helper"},
	} {
		var out, errb bytes.Buffer
		code := run(context.Background(), append([]string{"legion", "gh"}, args...), &out, &errb)
		if code != 1 {
			t.Fatalf("gh %v exit = %d, want 1; stderr %s", args, code, errb.String())
		}
		if !strings.Contains(errb.String(), "Legion never merges a pull request") {
			t.Fatalf("gh %v stderr = %q", args, errb.String())
		}
	}
}

func TestGhRefusesGitHubIssueWritesBeforeRedeemingAGrant(t *testing.T) {
	for _, args := range [][]string{
		{"--", "issue", "comment", "7"},
		{"--", "issue", "--repo", "o/r", "close", "7"},
		{"--", "api", "repos/o/r/issues", "-f", "title=x"},
		{"--", "api", "--method=PATCH", "repos/o/r/issues/7", "-F", "state=closed"},
		{"--", "api", "-XDELETE", "https://api.github.com/repos/o/r/issues/comments/99"},
	} {
		var out, errb bytes.Buffer
		code := run(context.Background(), append([]string{"legion", "gh"}, args...), &out, &errb)
		if code != 1 {
			t.Fatalf("gh %v exit = %d, want 1; stderr %s", args, code, errb.String())
		}
		if !strings.Contains(errb.String(), "Legion issues live on Dispatch") {
			t.Fatalf("gh %v stderr = %q", args, errb.String())
		}
	}
}
