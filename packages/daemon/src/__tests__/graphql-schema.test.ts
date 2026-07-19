import { describe, expect, it } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { gunzipSync } from "node:zlib";
import { buildSchema, parse, validate } from "graphql";
import type { IssueRef } from "../daemon/phase-artifacts";
import { fetchPhaseArtifactsBatch } from "../daemon/phase-artifacts";
import { buildProjectStatusFieldQuery, buildStatusMutation } from "../state/backends/github";
import type { CommandResult, CommandRunner } from "../state/fetch";
import { getCiStatusBatch, getPrReviewStateBatch } from "../state/fetch";
import { fetchGitHubProjectItems } from "../state/github-fetch";

// GitHub's published GraphQL schema (https://docs.github.com/public/fpt/schema.docs.graphql).
// Every query the daemon SENDS is validated against it, so an invalid field
// (e.g. `CheckRun.app`, which shipped twice because mocked-response tests
// can't see the outbound query) fails here instead of only failing live.
// assumeValid: GitHub's published SDL trips graphql-js 17's stricter schema-level
// lint (interface/implementation deprecation mismatch); query validation still runs.
const schema = buildSchema(
  gunzipSync(
    readFileSync(join(import.meta.dir, "..", "..", "schema", "github.docs.graphql.gz"))
  ).toString("utf8"),
  { assumeValid: true }
);

function expectValidQuery(query: string, label: string): void {
  const errors = validate(schema, parse(query));
  expect(errors.map((error) => `${label}: ${error.message}`)).toEqual([]);
}

/** Capture every `query=` argument passed to a gh invocation. */
function captureRunner(respond: (call: number) => CommandResult): {
  runner: CommandRunner;
  queries: string[];
} {
  const queries: string[] = [];
  let calls = 0;
  const runner: CommandRunner = async (command) => {
    const argument = command.find((part) => part.startsWith("query="));
    if (argument) {
      queries.push(argument.slice("query=".length));
    }
    calls += 1;
    return respond(calls);
  };
  return { runner, queries };
}

function ok(payload: unknown): CommandResult {
  return { stdout: JSON.stringify(payload), stderr: "", exitCode: 0 };
}

const emptyPage = {
  pageInfo: { hasNextPage: false, endCursor: null },
  nodes: [],
};

describe("daemon GraphQL queries validate against GitHub's published schema", () => {
  it("schema file is real: a known-invalid query fails validation", () => {
    // The exact bug that shipped twice: CheckRun has no `app` field.
    const bad =
      'query { repository(owner: "o", name: "r") { pullRequest(number: 1) { commits(last: 1) { nodes { commit { statusCheckRollup { contexts(first: 1) { nodes { ... on CheckRun { name app { databaseId } } } } } } } } } } }';
    const errors = validate(schema, parse(bad));
    expect(errors.map((error) => error.message).join("\n")).toContain(
      'Cannot query field "app" on type "CheckRun"'
    );
  });

  it("phase-artifacts batch query is schema-valid", async () => {
    const refs: IssueRef[] = [
      {
        issueId: "acme-api-42",
        status: "In Progress",
        source: { owner: "acme", repo: "api", number: 42 },
        prRef: { owner: "acme", repo: "api", number: 101 },
      },
    ];
    const { runner, queries } = captureRunner(() =>
      ok({
        data: {
          repo0: {
            pr0: {
              isDraft: false,
              headRefOid: "sha",
              merged: false,
              autoMergeRequest: null,
              latestReviews: { nodes: [] },
              commits: { nodes: [] },
            },
          },
        },
      })
    );
    await fetchPhaseArtifactsBatch(refs, {
      reviewerAppId: 42,
      reviewerAppLogin: "legion-reviewer[bot]",
      runner,
    });
    expect(queries).toHaveLength(1);
    expectValidQuery(queries[0] as string, "phase-artifacts");
  });

  it("project items ORG and USER queries are schema-valid", async () => {
    const org = captureRunner(() =>
      ok({ data: { organization: { projectV2: { items: emptyPage } } } })
    );
    await fetchGitHubProjectItems("acme", 2, org.runner);

    const user = captureRunner((call) =>
      call === 1
        ? ok({
            errors: [{ message: "Could not resolve to an Organization with the login of 'acme'." }],
          })
        : ok({ data: { user: { projectV2: { items: emptyPage } } } })
    );
    await fetchGitHubProjectItems("acme", 2, user.runner);

    const captured = [...org.queries, ...user.queries];
    expect(captured.length).toBeGreaterThanOrEqual(3);
    for (const [index, query] of captured.entries()) {
      expectValidQuery(query, `project-items[${index}]`);
    }
  });

  it("PR review-state batch query is schema-valid", async () => {
    const { runner, queries } = captureRunner(() =>
      ok({ data: { repo0: { pr0: { reviewDecision: null } } } })
    );
    await getPrReviewStateBatch(
      { "acme-api-42": { owner: "acme", repo: "api", number: 101 } },
      runner
    );
    expect(queries).toHaveLength(1);
    expectValidQuery(queries[0] as string, "pr-review-state");
  });

  it("CI status batch query is schema-valid", async () => {
    const { runner, queries } = captureRunner(() => ok({ data: { repo0: { pr0: null } } }));
    await getCiStatusBatch({ "acme-api-42": { owner: "acme", repo: "api", number: 101 } }, runner);
    expect(queries).toHaveLength(1);
    expectValidQuery(queries[0] as string, "ci-status");
  });

  it("issue-transition query and mutation are schema-valid", () => {
    expectValidQuery(buildProjectStatusFieldQuery("acme", "api", 42), "transition-status-query");
    expectValidQuery(
      buildStatusMutation("PVT_x", "PVTI_x", "PVTF_x", "opt"),
      "transition-status-mutation"
    );
  });
});
