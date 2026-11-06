// A fenced `mermaid` block in a body Dispatch renders in full - an issue comment here, and every
// other body surface through the same renderer - draws as the diagram the document editor draws
// (an image named "Rendered Mermaid diagram"), with its source closed behind a `Mermaid source`
// disclosure. A diagram the worker cannot draw within the time limit is refused with the reason
// and the source while the page keeps responding, and drawn markup carrying script, handlers or
// addresses runs nothing and fetches nothing. The drawing row checks both halves: the image with
// its labels, and the source hidden until the disclosure opens. At the commit before the change
// each body shows its source as code, so the drawing row fails on its diagram assertion and the
// time-limit row at its hold assertion (no worker chunk loads).
//
// A mermaid block in a document (the issue's spec, open in the editor) draws too, and the
// document rows check what it must not do: its own sheet styles no other `svg` or `text` on the
// page, and no reader's browser fetches Google Fonts. The diagram is drawn by Dispatch's Mermaid
// worker, never on the page's thread, so an edit redraws once typing pauses with its last drawing
// held until then and a click on it changes nothing, a block link below a diagram lands on its
// block once the diagram draws (but never pulls back a reader who scrolled on), and a worker that
// fails after a drawing leaves the block showing its source. The editor's source disclosure stays
// open, so these rows never use the closed-source check. Before the change the editor draws with
// upstream's view, synchronously on the page's thread, its sheet and Google Fonts `@import` in
// the page, so the style, hostile, worker, focus and redraw rows fail.
import { setTimeout as sleep } from "node:timers/promises";

import { type BrowserContext, expect, type Locator, type Page, test } from "@playwright/test";

import { createComment, createIssue, createProject, editComment, getArtifactText } from "./api";
import { blockId, documentEditor, openSpec, placeCaret } from "./editor";
import {
  expectHeldAndResponsive,
  failMermaidWorkersOnCall,
  holdMermaidWorkers,
  holdWorkerSource,
  recordMermaidWorkers,
  settledScrollTop,
  withoutScrollAnchoring,
} from "./mermaid-helpers";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// What this file's init scripts keep on the page's window.
declare global {
  interface Window {
    /** Each script or handler of `HOSTILE_SVG` that ran. */
    __hostile?: string[];
  }
}

const diagramName = "Rendered Mermaid diagram";

/** A body of `lead` and then one flowchart from `from` to `to`, whose source's first line is
 *  `graph TD`. */
function flowchart(lead: string, from: string, to: string): string {
  return `${lead}\n\n\`\`\`mermaid\ngraph TD\n  ${from} --> ${to}\n\`\`\`\n`;
}

/** The agent whose comment the past-version row anchors, so the comment is a session's rather
 *  than the reader's own. */
const session = {
  actor: { id: "e2e-mermaid-agent", kind: "session" as const },
  as: "agent" as const,
};

/** A document of `lead`, a vertical flowchart of 21 nodes in a chain (drawn taller than any
 *  project's viewport), 30 filler paragraphs and then the paragraph `target`, so `target` sits
 *  below the viewport when the page opens and the drawn diagram pushes it further down. */
function tallDocument(lead: string, target: string): string {
  const chain = Array.from({ length: 20 }, (_, node) => `  n${node} --> n${node + 1}`);
  const filler = Array.from({ length: 30 }, (_, index) => `Filler paragraph ${index + 1}.`);
  return `${lead}\n\n\`\`\`mermaid\ngraph TD\n${chain.join("\n")}\n\`\`\`\n\n${filler.join("\n\n")}\n\n${target}\n`;
}

function diagramIn(card: Locator): Locator {
  return card.getByRole("img", { exact: true, name: diagramName });
}

function conversationTurns(page: Page): Locator {
  return page.getByRole("list", { name: "Conversation turns" });
}

/** The comment thread `rootId` opened in the Conversation, where its bodies render in full
 *  rather than as the collapsed card's preview. The turn shows `lead`, the root comment's first
 *  text, before it is expanded, so the click lands on a turn that has rendered. */
async function openCommentThread(page: Page, rootId: string, lead: string): Promise<Locator> {
  const turn = conversationTurns(page).locator(`:scope > li[data-turn="comment:${rootId}"]`);
  await expect(turn).toContainText(lead);
  const expand = turn.getByRole("button", { exact: true, name: "Expand thread" });
  const opened = turn.getByRole("button", { exact: true, name: "Collapse thread" });
  // Retried for the reason the reader's click on `Mermaid source` is below: the page moves
  // while the turn's diagrams draw, so a click can land on whatever slid under the pointer and
  // the thread never opens. Measured here on firefox, where the expand click was lost and the
  // row waited out its budget for a `Collapse thread` that never appeared. The expand button is
  // gone once the thread is open, so the retry clicks only while it is still there.
  await expect(async () => {
    if (await expand.isVisible()) await expand.click();
    await expect(opened).toBeVisible({ timeout: 1_000 });
  }).toPass({ timeout: 5_000 });
  return turn;
}

/** Both halves of the control: `card` shows the drawn diagram holding `labels`, and its source
 *  (whose first line is `graph TD`) stays out of sight until `Mermaid source` is opened. */
async function expectDrawnDiagram(card: Locator, labels: readonly string[]): Promise<void> {
  const diagram = diagramIn(card);
  await expect(diagram).toBeVisible();
  for (const label of labels) {
    await expect(diagram).toContainText(label);
  }
  const source = card.getByText("graph TD");
  // Mounted, closed inside the disclosure: toBeHidden alone also passes on no match at all.
  await expect(source).toHaveCount(1);
  await expect(source).toBeHidden();
  const disclosure = card.getByText("Mermaid source", { exact: true });
  // The reader's click is retried until the disclosure opens, because a click can be lost. An
  // instrumented run of these rows caught it: a document-level `click` listener recorded
  // `click ARTICLE connected=true`, so the click landed on the `<article>` behind the summary -
  // the page moved between Playwright's actionability check on the summary and the click's own
  // dispatch - and no `toggle` fired and no figure was redrawn. Nothing had opened the
  // disclosure, so the row waited out its whole expect timeout; it failed that way twice in 100
  // repeats on firefox in a two-lane run of this pod. A click already taken leaves the source
  // visible, so the retry clicks only while it is still hidden rather than closing what the
  // first click opened.
  //
  // A retried click is no proof of the product's own rule, that a source the reader opened is
  // drawn open again after a repaint: this helper only gets the reader's click to land. That
  // rule is proven by the row `a mermaid source a reader opened is open again when the comment
  // is edited` and by the module's own test file,
  // `web/src/features/refs/mermaid-diagrams.test.tsx`.
  await expect(async () => {
    if (await source.isHidden()) await disclosure.click();
    await expect(source).toBeVisible({ timeout: 1_000 });
  }).toPass({ timeout: 5_000 });
}

/** Starts recording `page`'s console errors and returns the check the row whose worker fails
 *  after a drawing ends with: the document editor shows `source` in a `pre code`, holds no
 *  diagram, hides the status line, and the renderer said once that it is unavailable. Recording
 *  starts at this call, before the page navigates, so an error logged on the first paint is
 *  counted. A block whose worker cannot draw looks the same whether no worker ever loaded or one
 *  failed after a drawing: its source as code, with no stale diagram left above it. */
function watchSourceAsCode(page: Page): (source: string) => Promise<void> {
  const consoleErrors: string[] = [];
  page.on("console", (message) => {
    if (message.type() === "error") consoleErrors.push(message.text());
  });
  const editor = documentEditor(page);
  return async (source) => {
    await expect(editor.locator("pre code").filter({ hasText: source })).toBeVisible();
    await expect(diagramIn(editor)).toHaveCount(0);
    await expect(editor.locator(".proof-mermaid-status")).toBeHidden();
    await expect
      .poll(() =>
        consoleErrors.filter((text) =>
          text.includes("Showing Mermaid source as code, the diagram renderer is unavailable")
        )
      )
      .toHaveLength(1);
  };
}

/** The margin an absence of a request is read after, taken in Node so the page's own clock
 *  cannot skip it. Each row that reads one first forces the drawing into the DOM and walks the
 *  shadow root the markup would fetch from, which is the style resolution that starts a
 *  stylesheet's `@import`, an `<image>`'s load and a fetching `url(…)` paint; this margin covers
 *  a fetch begun a task or two after that pass rather than inside it. */
const REQUEST_MARGIN_MS = 250;

/** Records every request `context` makes to Google Fonts' two hosts and answers it with an empty
 *  stylesheet, so a run needs no outbound network. */
async function recordGoogleFonts(context: BrowserContext): Promise<string[]> {
  const requested: string[] = [];
  for (const host of ["https://fonts.googleapis.com/**", "https://fonts.gstatic.com/**"]) {
    await context.route(host, (route) => {
      requested.push(route.request().url());
      return route.fulfill({ body: "", contentType: "text/css" });
    });
  }
  return requested;
}

/** The text in a diagram's source that makes the page's parser read `HOSTILE_SVG` instead of
 *  the markup the worker drew. */
const HOSTILE_MARKER = "HOSTILE-MARKER";

/** The source of a flowchart whose drawn markup holds `HOSTILE_MARKER`. */
const hostileSource = `graph TD\n  A[${HOSTILE_MARKER}] --> B`;

/** An SVG document that draws a visible box labelled `Hostile label`, and carries each thing a
 *  sanitizer must take out: script, event handlers, external `href`s and `xlink:href`s, a
 *  `<foreignObject>` holding HTML, a `javascript:` link, fetching `url(…)` paint and an
 *  `@import`. Each handler or script that runs pushes to `window.__hostile`; each fetch goes to
 *  `hostile.example`. */
const HOSTILE_SVG = [
  '<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"',
  ' width="240" height="120" viewBox="0 0 240 120" onload="window.__hostile.push(\'svg onload\')">',
  '<style>@import url("https://hostile.example/import.css"); text { fill: #333333; }</style>',
  "<script>window.__hostile.push('script')</script>",
  '<rect x="0" y="0" width="240" height="120" fill="#ffffff" stroke="#333333"',
  " onload=\"window.__hostile.push('rect onload')\"/>",
  '<text x="20" y="40">Hostile label</text>',
  '<image href="https://hostile.example/image.png" width="10" height="10"',
  " onerror=\"window.__hostile.push('image onerror')\"/>",
  '<image xlink:href="https://hostile.example/xlink-image.png" width="10" height="10"',
  " onerror=\"window.__hostile.push('xlink image onerror')\"/>",
  '<use href="https://hostile.example/use.svg#u"/>',
  '<foreignObject x="0" y="0" width="10" height="10">',
  '<img xmlns="http://www.w3.org/1999/xhtml" src="https://hostile.example/fo.png"',
  " onerror=\"window.__hostile.push('foreignObject img onerror')\"/>",
  "</foreignObject>",
  '<a href="javascript:window.__hostile.push(\'a href\')"><text x="20" y="80">Hostile link</text></a>',
  '<rect x="200" y="90" width="10" height="10" fill="url(https://hostile.example/paint.svg#p)"',
  ' style="fill:url(https://hostile.example/style.png)"/>',
  "</svg>",
].join("");

/** Feeds `HOSTILE_SVG` through the real drawing path: the worker still renders the diagram, and
 *  the page's `DOMParser`, which both the editor's and the bodies' sanitizers parse with, reads
 *  `HOSTILE_SVG` in place of any SVG markup holding `HOSTILE_MARKER`. Every other parse is
 *  untouched. Starts `window.__hostile` empty. */
async function feedHostileSvg(page: Page): Promise<void> {
  await page.addInitScript(
    ({ marker, svg }) => {
      window.__hostile = [];
      const parse = DOMParser.prototype.parseFromString;
      DOMParser.prototype.parseFromString = function (
        this: DOMParser,
        markup: string,
        type: DOMParserSupportedType
      ) {
        const fed = type === "image/svg+xml" && markup.includes(marker) ? svg : markup;
        return parse.call(this, fed, type);
      };
    },
    { marker: HOSTILE_MARKER, svg: HOSTILE_SVG }
  );
}

/** Records every request `context` makes to `hostile.example` and aborts it. */
async function recordHostileRequests(context: BrowserContext): Promise<string[]> {
  const requested: string[] = [];
  await context.route("https://hostile.example/**", (route) => {
    requested.push(route.request().url());
    return route.abort();
  });
  return requested;
}

/** What of `HOSTILE_SVG` survived into `preview`'s shadow root: each element a sanitizer must
 *  drop, and each attribute that runs script or fetches. `null` when `preview` holds no shadow
 *  root. */
function hostileLeftovers(preview: Locator): Promise<string[] | null> {
  return preview.evaluate((element) => {
    const root = element.shadowRoot;
    if (root === null) return null;
    const dropped = ["script", "foreignObject", "image", "use", "a"];
    const found: string[] = [];
    for (const node of Array.from(root.querySelectorAll("*"))) {
      if (dropped.includes(node.localName)) found.push(`<${node.localName}>`);
      for (const attribute of Array.from(node.attributes)) {
        if (attribute.name.toLowerCase().startsWith("on") || attribute.value.includes("url(http")) {
          found.push(`${node.localName} ${attribute.name}="${attribute.value}"`);
        }
      }
    }
    return found;
  });
}

/** `HOSTILE_SVG` drew as a diagram in `scope`, whose preview is `preview`, and ran nothing,
 *  fetched nothing and left nothing hostile in the preview's shadow root. */
async function expectNothingHostile(
  page: Page,
  scope: Locator,
  preview: Locator,
  requested: readonly string[]
): Promise<void> {
  const diagram = diagramIn(scope);
  await expect(diagram).toBeVisible();
  await expect(diagram).toContainText("Hostile label");
  // The shadow-root walk is read first: it is the positive signal that the drawing is installed,
  // and walking it forces the style pass each hostile carrier would fetch from.
  expect(await hostileLeftovers(preview)).toEqual([]);
  await sleep(REQUEST_MARGIN_MS);
  expect(await page.evaluate(() => window.__hostile)).toEqual([]);
  expect(requested).toEqual([]);
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("a comment holding a mermaid block draws a diagram, its source behind Mermaid source", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Comment diagram" });
  const comment = await createComment(issue.key, { body: flowchart("Flow:", "Start", "Finish") });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await openCommentThread(page, comment.id, "Flow:");
    await expect(card).toContainText("Flow:");
    await expectDrawnDiagram(card, ["Start", "Finish"]);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): three edits reach the page over the event feed, each waited out at the page's own
// once-a-second refresh, and two real worker draws - the diagram the reader opens and the one
// an edit puts in its place. The clock cannot spend any of it, for the reason below.
// A body is painted whole, so an edit landing under a reader builds its source disclosure anew.
// What that disclosure shows is the reader's own: a source they opened is open again on every
// later paint of it, with no second click, and a source they never opened - the one an edit
// draws in its place - comes back closed. A refusal draws open whatever came before it. Against
// a build of the commit before the change, the first repaint below draws the opened source
// closed again and the row fails there. No clock runs here: a fast-forward before a diagram has
// drawn would spend the 5 s render limit and refuse the source, which is how this file's
// time-limit rows work on purpose. Every comment the row edits is a person's: an agent session's
// credential is refused on an edit.
test("a mermaid source a reader opened is open again when the comment is edited, and an edited diagram draws closed", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Reopened source" });
  const drawn = await createComment(issue.key, {
    body: flowchart("Reading this:", "Opened", "Kept"),
  });
  const brokenFence = "```mermaid\ngraph TD\n  A -->\n```\n";
  const refused = await createComment(issue.key, { body: `Broken:\n\n${brokenFence}` });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await openCommentThread(page, drawn.id, "Reading this:");
    // Both halves of the control, ending in the reader's one click: the source is open.
    await expectDrawnDiagram(card, ["Opened", "Kept"]);
    const source = card.getByText("graph TD");

    // The edit reaches the page over the event feed, whose refetch the page holds to one a
    // second (`MIN_REFETCH_INTERVAL_MS`, `web/src/api/query-refresh.ts`), so the new lead in the
    // card is the repaint. The diagram's source is byte for byte the one the reader opened, and
    // it is theirs again without another click.
    await editComment(drawn.id, { body: flowchart("Reading this, reworded:", "Opened", "Kept") });
    await expect(card).toContainText("Reading this, reworded:");
    await expect(diagramIn(card)).toBeVisible();
    await expect(source).toBeVisible();

    // An edit of the source itself draws a diagram the reader never opened, so it draws closed.
    await editComment(drawn.id, { body: flowchart("Rewired:", "Replaced", "Drawn") });
    await expect(card).toContainText("Rewired:");
    await expect(diagramIn(card)).toContainText("Replaced");
    await expect(source).toHaveCount(1);
    await expect(source).toBeHidden();

    // A source the renderer refuses is readable at once on every paint of it, edit or no edit.
    const broken = await openCommentThread(page, refused.id, "Broken:");
    const brokenSource = broken.locator("pre code").filter({ hasText: "A -->" });
    await expect(brokenSource).toBeVisible();
    await editComment(refused.id, { body: `Broken, still:\n\n${brokenFence}` });
    await expect(broken).toContainText("Broken, still:");
    await expect(brokenSource).toBeVisible();
    await expect(diagramIn(broken)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
// The worker holds the source (`holdWorkerSource`), so the shipped 5 s limit refuses even this
// two-node flowchart on every runner, and the row proves what the limit does, not how fast a
// machine draws: the page hands the source to the worker chunk, the page's thread keeps answering
// while the worker is busy, and the limit refuses it with the reason and the source shown. A page
// drawing on its own thread never loads the chunk and fails at the hold assertion. The limit is a
// page timer, so the clock fires it rather than the row waiting 5 s for it; the responsiveness
// samples run first, on the clock at real speed (`install` alone does not pause it), since an
// advanced clock would make a sample read its deadline for free.
test("a diagram too large to draw in time shows the reason, and the page keeps responding", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Large diagram" });
  const comment = await createComment(issue.key, { body: flowchart("Large:", "Start", "Finish") });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const holds = await holdWorkerSource(page);
    await page.clock.install();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await openCommentThread(page, comment.id, "Large:");
    await expect(card).toContainText("Large:");
    const refusal = card
      .getByRole("status")
      .filter({ hasText: "Unable to render Mermaid diagram" });
    await expectHeldAndResponsive(page, holds);
    // The samples ran while the worker still held the source, before the time limit refused it.
    await expect(refusal).toHaveCount(0);
    await page.clock.fastForward(5_000);
    await expect(refusal).toHaveText(
      "Unable to render Mermaid diagram: it did not finish within 5 seconds"
    );
    await expect(card.locator("pre code").filter({ hasText: "Start --> Finish" })).toBeVisible();
    await expect(diagramIn(card)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// beautiful-mermaid's sheet sets `text { font-family: 'Inter', system-ui, sans-serif }` and
// `svg { --_text: var(--fg) … }` after an `@import` of Google Fonts. In the page it restyles
// every `svg` and `text` there and fetches the font; in the diagram's own shadow root it styles
// that diagram alone.
// This row is the file's one WebKit and Firefox row: each engine's own SVG parser, shadow DOM and
// ELK layout draw the diagram.
test("a document's mermaid diagram draws, styles nothing outside it, and fetches no Google font @webkit @firefox", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: flowchart("Doc diagram:", "Start", "Finish"),
    title: "Document diagram",
  });

  const alice = await asUser(browser, "alice");
  try {
    const googleFonts = await recordGoogleFonts(alice);
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Doc diagram:");
    const diagram = diagramIn(documentEditor(page));
    await expect(diagram).toBeVisible();
    await expect(diagram).toContainText("Start");
    await expect(diagram).toContainText("Finish");

    // Sentinels of the page's own: an `svg` bringing its own `--fg` (only the diagram's `svg`
    // defines one), and a `text` inside it beside a `span`, which inherits the page's font.
    const sentinels = await page.evaluate(() => {
      const namespace = "http://www.w3.org/2000/svg";
      const svg = document.createElementNS(namespace, "svg");
      svg.setAttribute("style", "--fg: rgb(1, 2, 3)");
      const text = document.createElementNS(namespace, "text");
      text.textContent = "Sentinel";
      svg.append(text);
      const span = document.createElement("span");
      span.textContent = "Sentinel";
      document.body.append(svg, span);
      const read = {
        spanFont: getComputedStyle(span).fontFamily,
        svgText: getComputedStyle(svg).getPropertyValue("--_text"),
        textFont: getComputedStyle(text).fontFamily,
      };
      svg.remove();
      span.remove();
      return read;
    });
    expect(sentinels.svgText).toBe("");
    expect(sentinels.textFont).toBe(sentinels.spanFont);

    // No sheet of the page's own holds the diagram's rules. A cross-origin sheet's rules cannot be
    // read, and an `@import` rule's own sheet is never descended into.
    const pageRules = await page.evaluate(() => {
      const found: string[] = [];
      for (const sheet of Array.from(document.styleSheets)) {
        let rules: CSSRuleList;
        try {
          rules = sheet.cssRules;
        } catch {
          continue;
        }
        for (const rule of Array.from(rules)) {
          if (rule.cssText.includes("--_text") || rule.cssText.includes("fonts.googleapis.com")) {
            found.push(rule.cssText);
          }
        }
      }
      return found;
    });
    expect(pageRules).toEqual([]);

    // The diagram's own sheet survives, inside its shadow root, without its `@import`. Reading it
    // is also the positive signal the absence below is taken after: the sheet whose `@import`
    // would fetch the font is installed and has been resolved.
    const ownStyles = await documentEditor(page)
      .locator(".proof-mermaid-preview")
      .evaluate((preview) =>
        preview.shadowRoot === null
          ? null
          : Array.from(preview.shadowRoot.querySelectorAll("style"), (style) => style.textContent)
      );
    expect(ownStyles, "the preview holds a shadow root").not.toBeNull();
    expect(ownStyles).toContainEqual(expect.stringContaining("--_text"));
    expect(ownStyles?.filter((style) => style?.includes("@import"))).toEqual([]);

    await sleep(REQUEST_MARGIN_MS);
    expect(googleFonts).toEqual([]);
  } finally {
    await alice.close();
  }
});

test("hostile diagram markup in a comment runs nothing and fetches nothing", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Hostile comment diagram" });
  const comment = await createComment(issue.key, {
    body: `Hostile:\n\n\`\`\`mermaid\n${hostileSource}\n\`\`\`\n`,
  });

  const alice = await asUser(browser, "alice");
  try {
    const requested = await recordHostileRequests(alice);
    const page = await alice.newPage();
    await feedHostileSvg(page);
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await openCommentThread(page, comment.id, "Hostile:");
    await expectNothingHostile(page, card, card.locator(".dispatch-mermaid-preview"), requested);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): one real draw, the typing, the redraw the page's own debounce runs after it, and
// the document reads that show the click changed nothing; in WebKit the debounce is 2.3 s, and
// the row waits out no part of it.
test("an edited document diagram redraws once typing pauses, its last drawing stays until then, and a click on it changes nothing", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: flowchart("Typed diagram:", "Start", "Finish"),
    title: "Typed document diagram",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const errors: Error[] = [];
    page.on("pageerror", (error) => errors.push(error));
    await recordMermaidWorkers(page);
    await openSpec(page, issue.key, "Typed diagram:");
    const diagram = diagramIn(documentEditor(page));
    await expect(diagram).toContainText("Finish");
    await page.evaluate(() => window.__mermaidSent?.splice(0));
    const before = (await getArtifactText(issue.primary_artifact_id)).markdown;

    // A click on the drawn diagram asks for no drawing and changes nothing: the document
    // Dispatch holds reads the same after it, and the edit below is the only change it takes.
    await diagram.click();
    expect((await getArtifactText(issue.primary_artifact_id)).markdown).toBe(before);

    await placeCaret(page, "after", "Start --> Finish");
    // Every frame from now until the redrawn diagram shows, whether a diagram is shown: the last
    // drawing must stay through the typing and the pause, never leaving the block without one.
    const shownEachFrame = page.evaluate(
      () =>
        new Promise<boolean[]>((resolve) => {
          const samples: boolean[] = [];
          const sample = () => {
            const preview = document.querySelector<HTMLElement>(
              '[aria-label="Document editor"] .proof-mermaid-preview'
            );
            const svg = preview?.shadowRoot?.querySelector("svg");
            samples.push(preview?.hidden === false && svg !== null && svg !== undefined);
            if (svg?.textContent?.includes("FinishLine")) resolve(samples);
            else requestAnimationFrame(sample);
          };
          sample();
        })
    );
    // One call types the word with no delay, so every key falls well inside the pause the redraw
    // waits for.
    await page.keyboard.type("Line");
    await expect(diagram).toContainText("FinishLine");
    expect(await page.evaluate(() => window.__mermaidSent)).toEqual([
      expect.stringMatching(/Start --> FinishLine$/),
    ]);
    expect(await page.evaluate(() => window.__mermaidWorkers)).toBe(1);
    expect((await shownEachFrame).every(Boolean)).toBe(true);
    // The edit is the only change the document took, the click's included.
    await expect
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
      .toBe(before.replace("Start --> Finish", "Start --> FinishLine"));
    expect(errors).toEqual([]);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): one real draw of the 21-node chain, which must be taller than the viewport for the
// link's landing to be worth proving; the worker draws it for real and no timer is waited on.
test("a block link below a diagram lands on its block once the diagram draws", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: tallDocument("Linked document:", "Target paragraph"),
    title: "Block link below a diagram",
  });
  const target = await blockId(issue.primary_artifact_id, "Target paragraph");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await holdMermaidWorkers(page);
    await page.goto(`/issues/${issue.key}/spec#b-${target}`);
    const editor = documentEditor(page);
    const paragraph = editor.getByText("Target paragraph", { exact: true });
    const diagram = diagramIn(editor);
    await expect(paragraph).toBeInViewport();
    await expect(diagram).toHaveCount(0);
    await withoutScrollAnchoring(page);
    await page.evaluate(() => window.__releaseMermaid?.());
    await expect(diagram).toBeVisible();
    await expect(paragraph).toBeInViewport();
  } finally {
    await alice.close();
  }
});

/** The margin a pull back is read after, in Node: long enough for a smooth scroll started by a
 *  focus re-run to have moved the offset the sampler before it read. */
const PULL_BACK_MARGIN_MS = 250;

// SLOW(>=2s): one real draw of the 21-node chain, which must be taller than the viewport for the
// pull back to be worth proving; the worker draws it for real and the two reads of the offset
// are its own stillness sampler, not a wait.
test("a reader who scrolls after a block link lands is not pulled back once the diagram draws", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: tallDocument("Scrolled document:", "Target paragraph"),
    title: "Reader scrolls after a block link",
  });
  const target = await blockId(issue.primary_artifact_id, "Target paragraph");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await holdMermaidWorkers(page);
    await page.goto(`/issues/${issue.key}/spec#b-${target}`);
    const editor = documentEditor(page);
    const paragraph = editor.getByText("Target paragraph", { exact: true });
    const diagram = diagramIn(editor);
    await expect(paragraph).toBeInViewport();
    await expect(diagram).toHaveCount(0);
    await withoutScrollAnchoring(page);
    await settledScrollTop(page);
    // The reader moves on: a wheel over the document, up past the target.
    const box = await paragraph.boundingBox();
    if (box === null) throw new Error("the target paragraph has no box");
    await page.mouse.move(box.x + 10, box.y + box.height / 2);
    await page.mouse.wheel(0, -600);
    await expect(paragraph).not.toBeInViewport();
    const scrolled = await settledScrollTop(page);
    // The document is what scrolls: the link took it far down to the target, and 600 px up from
    // there is still below its top, so the offset read here can show a pull back.
    expect(scrolled).toBeGreaterThan(0);
    await page.evaluate(() => window.__releaseMermaid?.());
    await expect(diagram).toBeVisible();
    // A pull back that never happens is shown by the offset holding still: the focus re-run
    // follows the drawn diagram from the draw's own effect, within a frame or two, so a pull back
    // either moves the offset the sampler reads or starts a smooth scroll inside the Node-side
    // margin after it.
    await settledScrollTop(page);
    await sleep(PULL_BACK_MARGIN_MS);
    expect(await page.evaluate(() => document.scrollingElement?.scrollTop)).toBe(scrolled);
    await expect(paragraph).not.toBeInViewport();
  } finally {
    await alice.close();
  }
});

// The version view's call site of `refocusAfterDiagrams`: `VersionView.tsx:127` refocuses a
// highlighted mark once a diagram above it has drawn, where the row above drives
// `ProofDocument`'s call. No unit test holds a diagram's draw pending against a version view, and
// the deep-links row that lands a past version's comment link opens a document with no diagram,
// so this row is the only test of that refocus.
// SLOW(>=2s): one real draw of the 21-node chain, which must be taller than the viewport for the
// mark's landing to be worth proving; the worker draws it for real and no timer is waited on.
test("a past version's highlighted mark lands on its block once a diagram above it draws", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: tallDocument("Versioned document:", "Target paragraph holds the quoted words."),
    title: "Highlighted mark below a diagram",
  });
  const comment = await createComment(
    issue.key,
    { anchor: { artifact: "spec", quote: "quoted words" }, body: "Read this." },
    session
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await holdMermaidWorkers(page);
    await page.goto(
      `/issues/${issue.key}/artifact/spec?v=1&comment=${encodeURIComponent(comment.id)}`
    );
    const versionView = page.getByRole("region", { name: "Document version 1" });
    const highlight = versionView.locator(`[data-id="${comment.id}"]`);
    const diagram = diagramIn(versionView);
    await expect(highlight).toHaveText("quoted words");
    // The first landing holds before any diagram draws: the version's editor mounts once, so its
    // one scroll runs after its content has laid out.
    await expect(highlight).toBeInViewport();
    await expect(diagram).toHaveCount(0);
    await withoutScrollAnchoring(page);
    await page.evaluate(() => window.__releaseMermaid?.());
    await expect(diagram).toBeVisible();
    await expect(highlight).toBeInViewport();
  } finally {
    await alice.close();
  }
});

// A worker that drew once can fail later, as when a redeploy removes its chunk: the block then
// shows its edited source as code, never the last drawing above a source it no longer matches.
test("a document diagram whose worker fails after its first drawing shows its source", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: flowchart("Failing diagram:", "Start", "Finish"),
    title: "Failing document diagram",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const expectSourceAsCode = watchSourceAsCode(page);
    await failMermaidWorkersOnCall(page);
    await openSpec(page, issue.key, "Failing diagram:");
    const editor = documentEditor(page);
    const diagram = diagramIn(editor);
    await expect(diagram).toContainText("Finish");
    await page.evaluate(() => window.__failMermaidWorkers?.());

    await placeCaret(page, "after", "Start --> Finish");
    await page.keyboard.type("Line");
    await expectSourceAsCode("Start --> FinishLine");
  } finally {
    await alice.close();
  }
});
