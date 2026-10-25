import { expect, type Locator, type Page, test } from "@playwright/test";

import type { Issue } from "../web/src/api/types";
import {
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  editArtifact,
  getArtifact,
} from "./api";
import { documentEditor, openSpec } from "./editor";
import { collapsedMarginThread, openMarginThread, referenceAgent } from "./reference-surfaces";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// Each row seeds four targets, a host whose spec has two versions and an anchored comment, then
// reads four surfaces.
test.setTimeout(90_000);

test.beforeEach(async () => {
  await resetDatabase();
});

const KINDS = ["Issue", "Document", "Question", "Comment"] as const;
type Kind = (typeof KINDS)[number];

/** A reference to one target and the title it reads as. */
interface Target {
  readonly address: string;
  readonly title: string;
}

/** What `seedReferenceTitles` made. */
interface Seeded {
  readonly host: Issue;
  readonly comment: { readonly id: string };
  readonly targets: Readonly<Record<Kind, Target>>;
  /** Addresses whose targets do not exist, each with the API path its read answers 404 on. */
  readonly missing: readonly {
    readonly label: string;
    readonly address: string;
    readonly path: string;
  }[];
}

const ANCHOR = "Anchor for the comment.";
const MISSING_ASK = "00000000-0000-4000-8000-000000000000";

/**
 * Seeds an issue, a document, a question and a comment to reference, and a host whose spec writes
 * each target three ways, one paragraph each: bare (`Issue bare: dispatch://…`), as a link whose
 * text is its own address (`Issue linked: [dispatch://…](dispatch://…)`) and as a link its author
 * worded (`Issue worded: [the issue in my words](dispatch://…)`); then two addresses whose targets
 * are missing. A second version follows, so version 1 is a past version. An anchored comment on
 * the spec writes the bare and self-addressed forms of each and the missing addresses.
 */
async function seedReferenceTitles(): Promise<Seeded> {
  await createProject({ key: "CORE", name: "Core" });
  const target = await createIssue({ project: "CORE", title: "Warm the cache" });
  const document = await createIssueArtifact(
    target.key,
    { content: "# Notes\n\nCache notes.\n", name: "notes.md" },
    referenceAgent
  );
  expect(document.artifact.slug).toBe("notes-md");
  const ask = await createAsk(
    target.key,
    { question: "Should the cache be warmed first?" },
    referenceAgent
  );
  const comment = await createComment(
    target.key,
    { body: "The warm cache halves start-up time." },
    referenceAgent
  );
  const targets: Record<Kind, Target> = {
    Issue: { address: `dispatch://${target.key}`, title: `${target.key} Warm the cache` },
    Document: { address: `dispatch://${target.key}/artifact/notes-md`, title: "notes.md" },
    Question: {
      address: `dispatch://${target.key}/ask/${ask.id}`,
      title: "Should the cache be warmed first?",
    },
    Comment: {
      address: `dispatch://${target.key}/comment/${comment.id}`,
      title: "The warm cache halves start-up time.",
    },
  };
  const missing = [
    {
      address: "dispatch://CORE-999999",
      label: "Missing issue",
      path: "/api/v1/issues/CORE-999999",
    },
    {
      address: `dispatch://${target.key}/ask/${MISSING_ASK}`,
      label: "Missing question",
      path: `/api/v1/asks/${MISSING_ASK}`,
    },
  ];
  const spec = [
    ...KINDS.flatMap((kind) => {
      const { address } = targets[kind];
      return [
        `${kind} bare: ${address}`,
        `${kind} linked: [${address}](${address})`,
        `${kind} worded: [the ${kind.toLowerCase()} in my words](${address})`,
      ];
    }),
    ...missing.map(({ address, label }) => `${label}: ${address}`),
    ANCHOR,
    "",
  ].join("\n\n");
  const host = await createIssue({ project: "CORE", spec, title: "Reference title host" });
  await editArtifact(
    host.primary_artifact_id,
    { ops: [{ after: "end", markdown: "A second version.", op: "insert" }] },
    referenceAgent
  );
  await expect
    .poll(async () => (await getArtifact(host.primary_artifact_id)).versions.length)
    .toBeGreaterThan(1);
  const body = [
    ...KINDS.flatMap((kind) => {
      const { address } = targets[kind];
      return [`${kind} bare: ${address}`, `${kind} linked: [${address}](${address})`];
    }),
    ...missing.map(({ address, label }) => `${label}: ${address}`),
  ].join("\n\n");
  const anchored = await createComment(
    host.key,
    { anchor: { artifact: "spec", quote: ANCHOR }, body },
    referenceAgent
  );
  expect(anchored.anchor?.quote).toBe(ANCHOR);
  return { comment: anchored, host, missing, targets };
}

/** The one paragraph of a document view that starts with `label`. */
async function paragraph(scope: Locator, label: string): Promise<Locator> {
  const found = scope.locator("p").filter({ hasText: `${label}:` });
  await expect(found).toHaveCount(1);
  return found;
}

/** Checks that a document view draws the bare and the self-addressed reference to `target` as
 *  its title: one link per paragraph, carrying the address, with the stored text hidden. */
async function expectDrawnTitles(scope: Locator, kind: Kind, target: Target): Promise<void> {
  for (const form of ["bare", "linked"]) {
    const line = await paragraph(scope, `${kind} ${form}`);
    const title = line.locator("a.dispatch-reference");
    await expect(title).toHaveText(target.title);
    await expect(title).toHaveAttribute("data-dispatch-href", target.address);
    await expect(title).toBeVisible();
    await expect(line.locator(".dispatch-reference-text")).toHaveCount(1);
    await expect(line.locator(".dispatch-reference-text")).toBeHidden();
  }
}

/** Checks that a rendered comment, or its preview, reads the bare and the self-addressed
 *  reference to `target` as its title. */
async function expectCommentTitles(scope: Locator, target: Target): Promise<void> {
  await expect(scope.locator(`a[data-dispatch-ref="${target.address}"]`)).toHaveText([
    target.title,
    target.title,
  ]);
}

/** Resolves once every missing target's read has answered 404 on `page`. Registered before the
 *  navigation that makes the reads. */
function missingAnswered(page: Page, seeded: Seeded): Promise<unknown> {
  return Promise.all(
    seeded.missing.map(({ path }) =>
      page.waitForResponse(
        (response) => new URL(response.url()).pathname === path && response.status() === 404
      )
    )
  );
}

for (const kind of KINDS) {
  test(`a bare address and a link whose text is its own address to the ${kind.toLowerCase()} read as its title in the live editor, a past version, a comment and its preview`, async ({
    browser,
  }, testInfo) => {
    const seeded = await seedReferenceTitles();
    const target = seeded.targets[kind];
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openSpec(page, seeded.host.key, ANCHOR);
      await expectDrawnTitles(documentEditor(page), kind, target);

      // The comment's one-line preview, on its collapsed card, and the comment itself.
      const collapsed = await collapsedMarginThread(page, testInfo.project.name, seeded.comment.id);
      await expectCommentTitles(collapsed, target);
      const card = await openMarginThread(page, testInfo.project.name, seeded.comment.id);
      await expectCommentTitles(card, target);

      await page.goto(`/issues/${seeded.host.key}/artifacts/spec?v=1`);
      const past = page.getByTestId("version-view");
      await expect(past.getByRole("textbox", { name: "Document editor" })).toHaveAttribute(
        "contenteditable",
        "false"
      );
      await expect(past).not.toContainText("A second version.");
      await expectDrawnTitles(past, kind, target);
    } finally {
      await context.close();
    }
  });
}

test("a link its author worded keeps its words in the document, and an address whose target is missing reads as written", async ({
  browser,
}, testInfo) => {
  const seeded = await seedReferenceTitles();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const answered = missingAnswered(page, seeded);
    await openSpec(page, seeded.host.key, ANCHOR);
    await answered;
    const editor = documentEditor(page);
    await expectWordsAndAddresses(editor, seeded);

    // The comment's one-line preview, on its collapsed card, and the comment itself.
    const collapsed = await collapsedMarginThread(page, testInfo.project.name, seeded.comment.id);
    for (const { address } of seeded.missing) {
      await expect(collapsed.locator(`a[data-dispatch-ref="${address}"]`)).toHaveText(address);
    }
    const card = await openMarginThread(page, testInfo.project.name, seeded.comment.id);
    for (const { address } of seeded.missing) {
      await expect(card.locator(`a[data-dispatch-ref="${address}"]`)).toHaveText(address);
    }

    const pastAnswered = missingAnswered(page, seeded);
    await page.goto(`/issues/${seeded.host.key}/artifacts/spec?v=1`);
    await pastAnswered;
    await expectWordsAndAddresses(page.getByTestId("version-view"), seeded);
  } finally {
    await context.close();
  }
});

/** Checks, once every target the document names has drawn its title (so each read has
 *  answered), that a worded link shows its author's words and no title, and that an address whose
 *  target is missing shows as written. */
async function expectWordsAndAddresses(scope: Locator, seeded: Seeded): Promise<void> {
  for (const kind of KINDS) {
    const bare = await paragraph(scope, `${kind} bare`);
    await expect(bare.locator("a.dispatch-reference")).toHaveText(seeded.targets[kind].title);
    const worded = await paragraph(scope, `${kind} worded`);
    await expect(
      worded.getByRole("link", { exact: true, name: `the ${kind.toLowerCase()} in my words` })
    ).toBeVisible();
    await expect(worded.locator("a.dispatch-reference")).toHaveCount(0);
  }
  for (const { address, label } of seeded.missing) {
    const line = await paragraph(scope, label);
    await expect(line.getByText(address)).toBeVisible();
    await expect(line.locator("a.dispatch-reference")).toHaveCount(0);
  }
}
