import { expect } from "@playwright/test";

import { listComments } from "./api";

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
