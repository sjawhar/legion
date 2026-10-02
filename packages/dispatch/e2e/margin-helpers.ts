import { expect, type Page } from "@playwright/test";

import { listComments } from "./api";

// On the phone layout the margin is a bottom sheet over the document. Acting on a selection
// opens it; close it again before selecting another range, as a person would.
export async function setSheet(page: Page, project: string, open: boolean): Promise<void> {
  if (project !== "iphone") return;
  const sheet = page.getByTestId("margin-sheet");
  if ((await sheet.getAttribute("data-expanded")) !== String(open)) {
    if (open) {
      await page.getByRole("button", { name: /Open review panel/ }).click();
    } else {
      await page.mouse.click(1, 1);
    }
  }
  await expect(sheet).toHaveAttribute("data-expanded", String(open));
}

export async function commentWithBody(
  issueKey: string,
  artifactId: string | undefined,
  body: string
) {
  await expect
    .poll(() =>
      listComments(issueKey, artifactId).then((items) => items.find((item) => item.body === body))
    )
    .toBeDefined();
  const comment = (await listComments(issueKey, artifactId)).find((item) => item.body === body);
  if (comment === undefined) throw new Error(`Comment with body ${body} was not created.`);
  return comment;
}
