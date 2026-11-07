import { expect, test } from "@playwright/test";

import { fixture } from "../web/src/__tests__/pmdoc-fixtures";
import { createProject, createProjectDocument } from "./api";
import { connectedDot, documentEditor, expectParagraphReadings } from "./editor";
import { resetDatabase } from "./seed";
import { withAlice } from "./users";

// A letter right after a literal backslash, beside an emphasis run, is one the writer spells as a
// character reference. This family has no past version to read, so its one browser row is the
// row that reads the stored document in a browser: a project document an agent wrote through the
// API, drawn on its own page from the Yjs fragment the server writes. The document and the
// reading it is held to are the one fixture table's. The comment that drew the same paragraphs
// parsed through the headless engine, which
// `web/src/features/refs/fixture-table-readings.test.ts` runs over every document of that table;
// the bytes the writer stores, and that those bytes read back as the written tree in Go and in
// the engine alike, are
// `packages/envoy/internal/dispatch/pmdoc/writer_spelling_test.go`'s first rows.

const agentSession = {
  actor: { kind: "session" as const, id: "e2e-writer-spelling" },
  as: "agent" as const,
};

const entry = fixture("writer-spells-a-flanked-letter-after-a-backslash");

test.beforeEach(async () => {
  await resetDatabase();
});

test("a document with escapes beside emphasis around a tilde is stored, and its page draws the runs the engine reads", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const document = await createProjectDocument(
    "CORE",
    { content: entry.markdown, name: "spelling.md" },
    agentSession
  );
  const { slug } = document.artifact;
  expect(slug).toBe("spelling-md");
  await withAlice(browser, async (page) => {
    await page.goto(`/projects/CORE/documents/${slug}`);
    await expect(connectedDot(page)).toHaveText("connected");
    await expectParagraphReadings(documentEditor(page), entry.reading.paragraphs);
  });
});
