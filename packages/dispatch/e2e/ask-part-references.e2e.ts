import { expect, type Locator, type Page, test } from "@playwright/test";

import { createAsk, createComment, createIssue, createProject, getAsk } from "./api";
import { resetDatabase } from "./seed";
import { signIn } from "./users";

// A part of an ask - its question, each of its options and its answer - is a block of the
// reference graph, named `dispatch://KEY/ask/<ask id>#b-<part id>`. These rows drive the surfaces
// a person reaches one through: a reference written in a comment, which the dashboard renders as
// a link carrying the part's own text, the card that link opens, and the ask's page the link
// opens at that part; and the Referenced by panel of a node an option links to, where the part is
// the source and reads as the ask it belongs to. Every part id comes from the ask's own read,
// never from a guess, so a row fails against a server whose asks have no parts rather than citing
// an id that server never minted.

const agent = {
  actor: { id: "e2e-ask-parts", kind: "session" as const },
  as: "agent" as const,
};

test.beforeEach(async ({ context }) => {
  await resetDatabase();
  await signIn(context, "alice");
  await createProject({ key: "CORE", name: "Core" });
});

/** Opens the hover card of `link` from the keyboard, which every project can do (the phone
 *  project has no hover), and answers the card. */
async function openCard(page: Page, link: Locator): Promise<Locator> {
  await page.keyboard.press("Escape");
  await page.mouse.move(0, 0);
  await page.keyboard.press("Tab");
  await link.focus();
  const card = page.getByRole("tooltip");
  await expect(card).toBeVisible();
  return card;
}

test("a reference to an ask's option in a comment is a link that opens the ask at that part", async ({
  page,
}) => {
  const issue = await createIssue({ project: "CORE", title: "Ask part references" });
  const ask = await createAsk(
    issue.key,
    {
      options: [
        { description: "Ship at the start of the week.", label: "Monday" },
        { description: "Ship once the review lands.", label: "Tuesday" },
      ],
      question:
        "The release train ships the reference graph. Monday ships it before the week's reviews " +
        "and holds nothing; Tuesday waits on the review that lands Monday evening. Which day " +
        "does it ship on? Recommendation: Monday, so a reader has the graph for the reviews.",
    },
    agent
  );
  // A write's own answer carries no part id: the ask's own read is what names the part each of
  // its options is stored as.
  const read = await getAsk(ask.id);
  const option = read.ask.options[1];
  expect(
    option?.id,
    "the ask's read names the part each of its options is stored as"
  ).toBeDefined();
  const part = option?.id ?? "";
  // A part's text is what the server stores for it: an option is its label, an em dash and its
  // description.
  const optionText = "Tuesday — Ship once the review lands.";
  const reference = `dispatch://${issue.key}/ask/${ask.id}#b-${part}`;
  await createComment(issue.key, { body: `We take ${reference}.` }, agent);

  await page.goto(`/issues/${issue.key}/conversation`);
  const turns = page.getByRole("list", { name: "Conversation turns" });
  // The link's text is the part's own text, read from the graph, not the reference it was
  // written as.
  const link = turns.getByRole("link", { name: optionText });
  await expect(link).toBeVisible();
  await expect(link).toHaveAttribute("data-dispatch-ref", reference);
  await expect(link).toHaveAttribute(
    "href",
    `/issues/${issue.key}/asks/${ask.id}#b-${encodeURIComponent(part)}`
  );

  // The card the link opens heads with the ask the part belongs to, where a block of a document
  // heads with its document, and shows the part's text.
  const card = await openCard(page, link);
  await expect(card).toContainText(`${issue.key} · Ask`);
  await expect(card).toContainText(optionText);
  await page.keyboard.press("Escape");
  await expect(page.getByRole("tooltip")).toHaveCount(0);

  // Following it opens the ask's own page at that part, with the ask the current turn.
  await link.click();
  await expect(page).toHaveURL(`/issues/${issue.key}/asks/${ask.id}#b-${encodeURIComponent(part)}`);
  const turn = page.locator(`li[data-turn="ask:${ask.id}"][aria-current="true"]`);
  await expect(turn).toBeVisible();
  await expect(turn).toBeInViewport();
  // The ask shows an option as its label and its description, which the part's stored text joins
  // with an em dash: the part the link named is the option the reader lands on.
  await expect(turn).toContainText("Tuesday");
  await expect(turn).toContainText("Ship once the review lands.");
});

test("the backlink an ask's option writes names the ask as its source, not an artifact", async ({
  page,
}) => {
  const target = await createIssue({ project: "CORE", title: "Target of an option's link" });
  const source = await createIssue({ project: "CORE", title: "Asking issue" });
  const ask = await createAsk(
    source.key,
    {
      options: [
        { description: "Ship the reference graph now.", label: "Ship" },
        { description: `Hold until dispatch://${target.key} ships.`, label: "Wait" },
      ],
      question:
        "The reference graph ships this week, and the issue it depends on has not landed. Ship " +
        "now and backfill, or wait for it? Recommendation: ship now, so the graph is readable " +
        "while the reviews run.",
    },
    agent
  );
  // The option's stored text, which the panel shows as the row's excerpt: its label, an em dash
  // and its description.
  const optionText = `Wait — Hold until dispatch://${target.key} ships.`;

  await page.goto(`/issues/${target.key}`);
  const header = page.getByTestId("issue-header");
  await header.getByRole("button", { name: /^Referenced by/ }).click();
  const references = header.getByRole("region", { name: "Referenced by" });

  // The source is the ask the part belongs to, as a block of a comment reads as its comment, and
  // it links to the ask at that part. A part of an ask is no document, so nothing here reads
  // "Artifact", the label a block of a document carries - linked or as the plain text a build
  // that cannot place the part's reference writes.
  const askSource = references.getByRole("link", { name: `Ask · ${source.key}` });
  await expect(askSource).toBeVisible();
  await expect(references.getByText(/^Artifact · /)).toHaveCount(0);
  await expect(references.getByText(optionText, { exact: true })).toBeVisible();
  // One row, not two: an ask's own links are its question's, so the option's citation is the
  // part's edge alone.
  await expect(askSource).toHaveCount(1);

  // It opens the ask at the part the option is stored as, which the ask's own read names.
  const read = await getAsk(ask.id);
  const option = read.ask.options[1];
  expect(
    option?.id,
    "the ask's read names the part each of its options is stored as"
  ).toBeDefined();
  await expect(askSource).toHaveAttribute(
    "href",
    `/issues/${source.key}/asks/${ask.id}#b-${encodeURIComponent(option?.id ?? "")}`
  );
});
