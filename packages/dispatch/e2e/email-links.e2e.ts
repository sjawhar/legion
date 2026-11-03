import { expect, test } from "@playwright/test";

import { createIssue, createProject, editArtifact, getArtifact, getIssueArtifactText } from "./api";
import { documentEditor, openSpec } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// A bare email address in Markdown a document is written from - a spec an issue starts with, or
// a line an agent inserts - is an autolink, and the server stores it as a link to the address
// with the mail scheme: `[a@b.test](mailto:a@b.test)`. The editor and a past version's read-only
// view render that link with the same href, and the editor's link opener
// (proof-sdk's markdown-link-click.ts) hands it to `window.open`: on a modifier click in the
// editable editor, on a plain click in the read-only view.

declare global {
  interface Window {
    __dispatchOpened?: string[];
  }
}

const agentSession = {
  actor: { kind: "session" as const, id: "e2e-email-links" },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

test("an email address written into a spec is a mailto link in the editor and in a past version", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Contacts\n\nWrite to first@b.test for access.\n",
    title: "Email addresses in specs",
  });

  // An agent writes the second address into the spec, as `dispatch doc-edit` does: a new version,
  // whose number the server assigns. The paragraph after it is what the editor shows once the
  // edit has reached it.
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [
        {
          after: "end",
          markdown: "Or write to second@b.test.\n\nBelow the contacts.",
          op: "insert",
        },
      ],
    },
    agentSession
  );
  const newestVersion = async () =>
    Math.max(
      ...(await getArtifact(issue.primary_artifact_id)).versions.map(({ number }) => number)
    );
  await expect.poll(newestVersion).toBeGreaterThan(1);
  const written = await newestVersion();

  const stored = (await getIssueArtifactText(issue.key, "spec")).markdown;
  expect(stored).toContain("[first@b.test](mailto:first@b.test)");
  expect(stored).toContain("[second@b.test](mailto:second@b.test)");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The page's `window.open` records what it was asked to open and opens nothing.
    await page.addInitScript(() => {
      const opened: string[] = [];
      window.__dispatchOpened = opened;
      window.open = (url?: string | URL) => {
        opened.push(String(url));
        return null;
      };
    });
    const opened = () => page.evaluate(() => window.__dispatchOpened ?? []);

    await openSpec(page, issue.key, "Below the contacts.");
    const editor = documentEditor(page);
    await expect(editor.getByRole("link", { name: "first@b.test" })).toHaveAttribute(
      "href",
      "mailto:first@b.test"
    );
    const second = editor.getByRole("link", { name: "second@b.test" });
    await expect(second).toHaveAttribute("href", "mailto:second@b.test");
    // The editable editor opens a link on a modifier click; a plain click there places the caret.
    await second.click({ modifiers: ["ControlOrMeta"] });
    await expect.poll(opened).toEqual(["mailto:second@b.test"]);

    // The past version renders the same links through the read-only view, which opens one on a
    // plain click.
    await page.getByRole("combobox", { name: "Version" }).selectOption(String(written));
    const versionView = page.getByTestId("version-view");
    const first = versionView.getByRole("link", { name: "first@b.test" });
    await expect(first).toHaveAttribute("href", "mailto:first@b.test");
    await expect(versionView.getByRole("link", { name: "second@b.test" })).toHaveAttribute(
      "href",
      "mailto:second@b.test"
    );
    await first.click();
    await expect.poll(opened).toEqual(["mailto:second@b.test", "mailto:first@b.test"]);
  } finally {
    await alice.close();
  }
});
