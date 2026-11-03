import { expect, test } from "@playwright/test";

import {
  createComment,
  createIssue,
  createProject,
  createProjectDocument,
  getArtifactText,
} from "./api";
import {
  connectedDot,
  documentEditor,
  expectCommentReading,
  expectParagraphReadings,
  type ParagraphReading,
} from "./editor";
import { resetDatabase } from "./seed";
import { withAlice } from "./users";

// Escaped characters beside emphasis around a tilde: a document an agent writes through the API
// holding them is stored, its page draws the runs a comment holding the same paragraphs draws, and
// its stored text reads back to that same drawing.

const agentSession = {
  actor: { kind: "session" as const, id: "e2e-writer-spelling" },
  as: "agent" as const,
};

/** `p \~\a*~* q` and `p \a*~*\* q`, one backslash each. */
const paragraphs = "p \\~\\a*~* q\n\np \\a*~*\\* q\n";

/** The document's stored markdown, exactly: each letter beside a run is a character reference,
 *  the literal backslash before it is written `\\` so that it does not escape the reference's
 *  `&`, and each escaped tilde is its reference. */
const storedSpelling = "p &#126;\\\\&#97;*&#126;* q\n\np \\\\&#97;*&#126;*\\* q\n";

test.beforeEach(async () => {
  await resetDatabase();
});

async function seed(): Promise<{
  artifactId: string;
  commentId: string;
  issueKey: string;
  slug: string;
}> {
  await createProject({ key: "CORE", name: "Core" });
  const document = await createProjectDocument(
    "CORE",
    { content: paragraphs, name: "spelling.md" },
    agentSession
  );
  const issue = await createIssue({ project: "CORE", title: "Writer spelling" }, agentSession);
  const comment = await createComment(issue.key, { body: paragraphs }, agentSession);
  return {
    artifactId: document.artifact.id,
    commentId: comment.id,
    issueKey: issue.key,
    slug: document.artifact.slug,
  };
}

/** What the engine draws for the two paragraphs, in order: each paragraph's whole text and the
 *  text of each `<em>`, `<strong>` and `<del>` in it. An escaped tilde or asterisk is that
 *  character, `\a` keeps its backslash, and each `*~*` emphasizes its tilde. */
const engineReading: ParagraphReading[] = [
  { emphasized: ["~"], strong: [], struck: [], text: "p ~\\a~ q" },
  { emphasized: ["~"], strong: [], struck: [], text: "p \\a~* q" },
];

test("a document with escapes beside emphasis around a tilde is stored, and its page draws the runs the engine reads", async ({
  browser,
}) => {
  const { slug } = await seed();
  expect(slug).toBe("spelling-md");
  await withAlice(browser, async (page) => {
    await page.goto(`/projects/CORE/documents/${slug}`);
    await expect(connectedDot(page)).toHaveText("connected");
    await expectParagraphReadings(documentEditor(page), engineReading);
  });
});

test("a comment with escapes beside emphasis around a tilde draws the runs the engine reads", async ({
  browser,
}, testInfo) => {
  const { commentId, issueKey } = await seed();
  await withAlice(browser, (page) =>
    expectCommentReading(page, issueKey, commentId, testInfo.project.name, engineReading)
  );
});

test("the stored document reads back to the same runs", async ({ browser }, testInfo) => {
  const { artifactId, issueKey } = await seed();
  const { markdown } = await getArtifactText(artifactId);
  expect(markdown).toBe(storedSpelling);
  // The stored text, posted as a comment, draws what the written paragraphs draw.
  const readBack = await createComment(issueKey, { body: markdown }, agentSession);
  await withAlice(browser, (page) =>
    expectCommentReading(page, issueKey, readBack.id, testInfo.project.name, engineReading)
  );
});
