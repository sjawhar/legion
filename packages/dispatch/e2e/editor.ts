import type { Page } from "@playwright/test";

/** Documents render in preview mode by default; enter edit mode before typing into the
 *  CodeMirror pane. A no-op if the editor is already active, or if this page never renders
 *  one — `count()` alone would race a just-navigated page before React mounts the button. */
export async function enterEditMode(page: Page): Promise<void> {
  const editButton = page.getByRole("button", { exact: true, name: "Edit" });
  const appeared = await editButton
    .waitFor({ state: "visible", timeout: 5000 })
    .then(() => true)
    .catch(() => false);
  if (appeared) {
    await editButton.click();
  }
}
