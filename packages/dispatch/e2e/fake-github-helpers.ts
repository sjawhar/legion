import { test } from "@playwright/test";

import { fakeGithubPort } from "./harness-ports";

/** Replaces the fake GitHub's installations wholesale: repositories present are
 * "App installed" with the given Contents permission; absent ones answer 404.
 * `files` (keyed by file name) populate .dispatch/architecture/, and the fake
 * derives the branch commit from their content, so a re-seed with different
 * files moves the commit exactly like a push. A deployed server
 * (`PLAYWRIGHT_BASE_URL`) talks to no fake, so the test that seeds it there is
 * skipped at that point rather than failed. */
export async function seedFakeGithub(
  repos: Record<
    string,
    { contents: string; installation_id: number; files?: Record<string, string> }
  >
): Promise<void> {
  test.skip(
    Boolean(process.env.PLAYWRIGHT_BASE_URL),
    "the fake GitHub is unavailable with PLAYWRIGHT_BASE_URL"
  );
  const response = await fetch(`http://127.0.0.1:${fakeGithubPort}/__fixture/repos`, {
    body: JSON.stringify(repos),
    headers: { "Content-Type": "application/json" },
    method: "PUT",
  });
  if (!response.ok) {
    throw new Error(`seed fake github: ${response.status}`);
  }
}
