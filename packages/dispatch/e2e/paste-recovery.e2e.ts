import { expect, test } from "@playwright/test";

import { documentEditor, openWithCaret, paste } from "./editor";
import { resetDatabase } from "./seed";

test.beforeEach(async () => {
  await resetDatabase();
});

// LEGION-566. The live document editor's parserCtx singleton is built once, at boot
// (@milkdown/core's `parser` plugin), and @milkdown/plugin-clipboard reads it, uncaught, for
// every text/plain paste. A paste it refuses - a mark span of a kind this schema has no parser
// for, the same mid-paragraph throw markdown-engine.test.ts's LEGION-540 fixture exercises for
// the headless parser - used to leave that singleton's parser stack open, so the next, perfectly
// ordinary paste in the same document also failed to reach it, for the rest of the session.
// proof-editor's lib.ts now rebuilds the parser on a throw instead of reusing the corrupted one.
test("a refused paste no longer breaks the next paste in the live document", async ({
  browser,
}) => {
  const { alice, page } = await openWithCaret(
    browser,
    "Paste recovery",
    "Intro text.",
    "text.",
    "end"
  );
  try {
    await paste(page, { html: "", text: 'see <span data-dispatch="bogus">this</span> now' });
    await paste(page, { html: "", text: "Fixed two bugs" });
    await expect(documentEditor(page)).toContainText("Fixed two bugs");
  } finally {
    await alice.close();
  }
});
