import { test } from "@playwright/test";

import { fixture } from "../web/src/__tests__/pmdoc-fixtures";
import { createIssue, createProject } from "./api";
import { expectParagraphReadings } from "./editor";
import { resetDatabase } from "./seed";
import { withAlice } from "./users";

// A run of tildes strikes only when it pairs with a run of its own length, and a run may open
// right after an escaped tilde. Three readers answer that, and only one of them needs a browser:
// a past version hands the stored markdown to the live Milkdown editor (VersionView.tsx's
// `setMarkdown`), so this row is the editor's own parse of what the server stored, and it reads
// the document and its reading from the one fixture table. The spec editor binds to the Yjs
// fragment the server writes, which `fixtures.json` pins without a browser (`pm_json`,
// `yjs_update_v1_b64`); a comment parses through the headless engine, which
// `web/src/features/refs/fixture-table-readings.test.ts` runs over every document of that table;
// and the spelling the stored text takes for an unpaired tilde is
// `packages/envoy/internal/dispatch/pmdoc/writer_spelling_test.go`'s.

const agentSession = {
  actor: { kind: "session" as const, id: "e2e-strikethrough" },
  as: "agent" as const,
};

const entry = fixture("strikethrough-run-lengths");

test.beforeEach(async () => {
  await resetDatabase();
});

test("tilde runs of different lengths stay text in a past version of the spec, and a run after an escaped tilde strikes", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue(
    { project: "CORE", spec: entry.markdown, title: "Tilde runs" },
    agentSession
  );
  await withAlice(browser, async (page) => {
    await page.goto(`/issues/${issue.key}/artifacts/spec?v=1`);
    await expectParagraphReadings(page.getByTestId("version-view"), entry.reading.paragraphs);
  });
});
