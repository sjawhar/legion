import { test } from "@playwright/test";

import { harnessPorts } from "./harness-ports";

interface FakeGithubRepo {
  readonly contents: string;
  readonly actions?: string;
  readonly pull_requests?: string;
  readonly installation_id: number;
  readonly files?: Record<string, string>;
}

/** Replaces the fake GitHub's installations wholesale: repositories present are
 * "App installed" with the given Contents permission; absent ones answer 404.
 * `files` (keyed by file name) populate .dispatch/architecture/, and the fake
 * derives the branch commit from their content, so a re-seed with different
 * files moves the commit exactly like a push. A deployed server
 * (`PLAYWRIGHT_BASE_URL`) talks to no fake, so the test that seeds it there is
 * skipped at that point rather than failed. */
export async function seedFakeGithub(repos: Record<string, FakeGithubRepo>): Promise<void> {
  test.skip(
    Boolean(process.env.PLAYWRIGHT_BASE_URL),
    "the fake GitHub is unavailable with PLAYWRIGHT_BASE_URL"
  );
  await putFakeGithubRepos(repos);
}

/** Makes the local delivery reconcile succeed against an empty acme/widgets history, so a pass that
 * runs inside a test leaves the freshness row Live rather than red with the reconcile's error. Only
 * a test that asserts the row says Live needs it. A deployed server has its own GitHub and is left
 * as it is. */
export async function seedDeliveryReconcileGithub(): Promise<void> {
  if (process.env.PLAYWRIGHT_BASE_URL) return;
  await putFakeGithubRepos({
    "acme/widgets": {
      actions: "read",
      contents: "read",
      installation_id: 690,
      pull_requests: "read",
    },
  });
}

/** Clears every fake GitHub repository before the next scenario. A deployed run has no fake. */
export async function resetFakeGithub(): Promise<void> {
  if (process.env.PLAYWRIGHT_BASE_URL) return;
  await putFakeGithubRepos({});
}

async function putFakeGithubRepos(repos: Record<string, FakeGithubRepo>): Promise<void> {
  const response = await fetch(`http://127.0.0.1:${harnessPorts.fakeGithub.port}/__fixture/repos`, {
    body: JSON.stringify(repos),
    headers: { "Content-Type": "application/json" },
    method: "PUT",
  });
  if (!response.ok) {
    throw new Error(`seed fake github: ${response.status}`);
  }
}
