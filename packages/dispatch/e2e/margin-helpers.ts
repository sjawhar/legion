import { expect, type Locator, type Page } from "@playwright/test";

import { listComments } from "./api";
import { markSpan } from "./editor";

/** The text of each mark among `spans`, keyed by mark id: a mark another mark nests inside renders
 *  as more than one span, and an outer span's text includes the spans inside it, so a mark's text
 *  is its own spans' text joined in document order. */
export function markTexts(spans: Locator): Promise<Record<string, string>> {
  return spans.evaluateAll((elements) => {
    const texts: Record<string, string> = {};
    for (const element of elements) {
      const id = element.getAttribute("data-id") ?? "";
      texts[id] = (texts[id] ?? "") + (element.textContent ?? "");
    }
    return texts;
  });
}

export async function markText(page: Page, markId: string): Promise<string> {
  return (await markTexts(markSpan(page, markId)))[markId] ?? "";
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
