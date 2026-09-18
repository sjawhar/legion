import { expect, test } from "@playwright/test";

import { createComment, createIssue, createProject } from "./api";
import { documentEditor, markSpan } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const original = "the quick brown fox";
const replacement = "a slow red hound";

test.beforeEach(async () => {
  await resetDatabase();
});

test("an open Spec suggestion decoration shows its replacement without object-valued DOM attributes", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "SUGG", name: "Suggestion decoration" });
  const issue = await createIssue({
    project: "SUGG",
    spec: original,
    title: "Render an open suggestion",
  });
  const suggestion = await createComment(issue.key, {
    anchor: { artifact: "spec", quote: original },
    body: "Use the replacement.",
    suggestion: { replace_with: replacement },
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/spec`);

    const markedOriginal = markSpan(page, suggestion.id);
    const replacementDecoration = page.locator(
      `.mark-replace-insert[data-mark-id="${suggestion.id}"]`
    );
    await expect(markedOriginal).toBeVisible();
    await expect(replacementDecoration).toBeVisible();

    const observed = await documentEditor(page).evaluate((root, markId) => {
      const nodes = Array.from(root.querySelectorAll("*"));
      const decorationNodes = nodes.filter(
        (node) =>
          node.getAttribute("data-id") === markId ||
          node.getAttribute("data-mark-id") === markId ||
          node.getAttribute("data-proof") === "suggestion"
      );
      return {
        attributeValues: nodes.flatMap((node) =>
          Array.from(node.attributes, (attribute) => attribute.value)
        ),
        dom: decorationNodes.map((node) => node.outerHTML),
        replacementText: root.querySelector(`.mark-replace-insert[data-mark-id="${markId}"]`)
          ?.textContent,
      };
    }, suggestion.id);
    await testInfo.attach("Spec suggestion decoration DOM", {
      body: JSON.stringify(observed, null, 2),
      contentType: "application/json",
    });
    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath("spec-suggestion-decoration.png"),
    });

    expect(observed).toMatchObject({
      attributeValues: expect.not.arrayContaining(["[object Object]"]),
      replacementText: replacement,
    });
  } finally {
    await alice.close();
  }
});
