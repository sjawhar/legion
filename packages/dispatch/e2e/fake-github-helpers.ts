/** Replaces the fake GitHub's installations wholesale: repositories present are
 * "App installed" with the given Contents permission; absent ones answer 404.
 * `files` (keyed by file name) populate .dispatch/architecture/, and the fake
 * derives the branch commit from their content, so a re-seed with different
 * files moves the commit exactly like a push. */
export async function seedFakeGithub(
  repos: Record<
    string,
    { contents: string; installation_id: number; files?: Record<string, string> }
  >
): Promise<void> {
  const port = process.env.FAKE_GITHUB_PORT ?? "9022";
  const response = await fetch(`http://127.0.0.1:${port}/__fixture/repos`, {
    body: JSON.stringify(repos),
    headers: { "Content-Type": "application/json" },
    method: "PUT",
  });
  if (!response.ok) {
    throw new Error(`seed fake github: ${response.status}`);
  }
}
