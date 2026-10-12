import { expect, test } from "@playwright/test";

import { createIssue, createIssueArtifact, createProject, editArtifact } from "./api";
import { documentEditor, openSpec } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// A picture line the editor cannot draw gives way to a link to the artifact's page
// (features/doc/picture-view.ts). The link sits far enough down a real spec that the document's
// start is off screen, and a plain click on it opens what it names. This spec is its own file so
// that it runs in Firefox too (e2e/playwright.config.ts): Firefox does not focus a link on
// mousedown, the editing host takes the focus instead, and the editor then restores its own
// selection, which a reader who has not clicked into the text yet has at the document's start.

const agentSession = {
  actor: { kind: "session" as const, id: "e2e-picture-link-click" },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

test("a fallback link far down the document opens the artifact's page on a plain click", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  // Forty paragraphs: the document's start is off screen at every project's viewport, whatever a
  // paragraph's height.
  const filler = Array.from({ length: 40 }, (_, i) => `Filler paragraph ${i + 1}.`).join("\n\n");
  const issue = await createIssue({
    project: "CORE",
    spec: `# Pictures\n\n${filler}`,
    title: "A link far down the spec",
  });
  // A markdown document named without a version is a link at once, with no picture load to wait
  // for.
  await createIssueArtifact(issue.key, { content: "# Notes\n", name: "notes.md" }, agentSession);
  const source = `dispatch://${issue.key}/artifact/notes-md`;
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [{ after: "end", markdown: `![notes.md](${source})\n\nBelow the link.`, op: "insert" }],
    },
    agentSession
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Below the link.");
    const picture = documentEditor(page).locator(`[data-dispatch-doc-picture="${source}"]`);
    await picture.scrollIntoViewIfNeeded();
    const link = picture.getByRole("link", { name: `${issue.key} notes-md` });
    await expect(link).toBeInViewport();
    // The reader has not clicked into the text: the first thing they do is click the link.
    await link.click();
    await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/notes-md`);
  } finally {
    await alice.close();
  }
});
