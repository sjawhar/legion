import { describe, expect, it } from "bun:test";
import {
  bodyDeclaresGraphQLMutation,
  collectInlineGraphQLBodies,
  isGhMergeIntent,
} from "./gh-merge-intent";

describe("gh merge intent", () => {
  it("collects only literal GraphQL query bodies", () => {
    expect(
      collectInlineGraphQLBodies([
        "api",
        "graphql",
        "-F",
        "variables={}",
        "--raw-field=query=query { viewer { login } }",
      ])
    ).toEqual(["query { viewer { login } }"]);

    for (const args of [
      ["api", "graphql", "-F", "query=@/tmp/mutation.graphql"],
      ["api", "graphql", "-F=query=@-"],
      ["api", "graphql", "--field=query=@-"],
      ["api", "graphql", "--raw-field=query=@-"],
      ["api", "graphql", "--input", "/tmp/mutation.json"],
      ["api", "graphql", "--input=/tmp/mutation.json"],
    ]) {
      expect(collectInlineGraphQLBodies(args)).toBeUndefined();
    }
  });

  it("recognises GraphQL mutations only in literal query bodies", () => {
    expect(
      bodyDeclaresGraphQLMutation("mutation { mergePullRequest(input: {}) { clientMutationId } }")
    ).toBe(true);
    expect(bodyDeclaresGraphQLMutation("query { viewer { login } }")).toBe(false);
  });

  it("requires controller authority for every merge-capable gh invocation", () => {
    for (const args of [
      ["pr", "merge", "5"],
      ["api", "repos/o/r/pulls/5/merge"],
      ["api", "-X", "PUT", "repos/o/r/pulls/5/merge?match_head_commit=abc"],
      ["api", "repos/o/r/merges"],
      ["api", "https://api.github.com/repos/o/r/pulls/5/merge/#fragment"],
      [
        "api",
        "graphql",
        "-f",
        "query=mutation { mergePullRequest(input: {}) { pullRequest { id } } }",
      ],
      ["api", "graphql", "-F", "query=@/tmp/mutation.graphql"],
      ["api", "graphql", "-F=query=@-"],
      ["api", "graphql", "--field=query=@-"],
      ["api", "graphql", "--raw-field=query=@-"],
      ["api", "graphql", "--input", "/tmp/mutation.json"],
      ["alias", "set", "m", "pr merge"],
      ["extension", "install", "merge-helper"],
    ]) {
      expect(isGhMergeIntent(args)).toBe(true);
    }
  });

  it("allows ordinary GraphQL queries without merge intent", () => {
    expect(isGhMergeIntent(["api", "graphql", "-f", "query=query { viewer { login } }"])).toBe(
      false
    );
  });
});
