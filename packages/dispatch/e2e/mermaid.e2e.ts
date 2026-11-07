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
// A mermaid block in a document (the issue's spec, open in the editor, or a past version) draws
// too, and the document rows check what it must not do: its own sheet styles no other `svg` or
// `text` on the page, and no reader's browser fetches Google Fonts. The diagram is drawn by
// Dispatch's Mermaid worker, never on the page's thread, so an edit redraws once typing pauses
// with its last drawing held until then and a click on it changes nothing, a block link or a
// past version's highlighted mark below a diagram lands on its block once the diagram draws
// (but never pulls back a reader who scrolled on), and with no worker, or a worker that fails
// after drawing, the block shows its source. The editor's source disclosure stays open, so these
// rows never use the closed-source check. Before the change the editor draws with upstream's
// view, synchronously on the page's thread, its sheet and Google Fonts `@import` in the page, so
// the style, hostile, worker, focus, redraw and no-worker rows fail.
//
// The worker draws with two renderers, and the rows below the document ones read what that
// added. A `timeline`, which beautiful-mermaid has no layout for, draws on each of the eight
// surfaces Dispatch draws a diagram on. Each source reaches the renderer that has a layout for
// it, read from the sheet the drawing carries (`rendererOf`), and a source beautiful-mermaid
// refuses falls through to Mermaid rather than being refused. A source neither draws shows one
// status line of at most 200 characters that quotes at most the first 120 characters of its
// source. And the page's two bounds are told apart: a job that is loading a renderer is under
// the load bound, never the render bound, so a slow download still draws and a renderer that
// never loads is refused in its own words. Before the change the worker had one renderer, so
// every timeline row shows its source as code under `Invalid mermaid header`, both routing rows
// fail, and the two bound rows have no second renderer to wait for.
import { setTimeout as sleep } from "node:timers/promises";

import { type BrowserContext, expect, type Locator, type Page, test } from "@playwright/test";

import { MERMAID_SOURCES } from "../web/src/features/refs/mermaid-sources";
import { contrastRatioSrgb8, oklchToSrgb8, type Rgb8 } from "../web/src/theme/contrast";
import { SLATE_50, SLATE_950 } from "../web/src/theme/palette";
import {
  answerAsk,
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  editComment,
  getArtifactText,
} from "./api";
import { blockId, documentEditor, openSpec, placeCaret } from "./editor";
import {
  delayMermaidChunks,
  expectHeldAndResponsive,
  expectWorkerHeld,
  failMermaidWorkersOnCall,
  holdMermaidWorkers,
  holdWorkerSource,
  recordMermaidAnswers,
  recordMermaidChunks,
  recordMermaidWorkers,
  refuseWorkers,
  rendererOf,
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

/** A body of `lead` and then one fenced mermaid block per source, in the order given. */
function diagramBody(lead: string, ...sources: string[]): string {
  const fences = sources.map((source) => `\`\`\`mermaid\n${source}\n\`\`\``);
  return `${lead}\n\n${fences.join("\n\n")}\n`;
}

/** A body of `lead` and then one flowchart from `from` to `to`, whose source's first line is
 *  `graph TD`. */
function flowchart(lead: string, from: string, to: string): string {
  return diagramBody(lead, `graph TD\n  ${from} --> ${to}`);
}

/** The source of one registered diagram type, from the one table both the browser rows and the
 *  unit row read (`web/src/features/refs/mermaid-sources.ts`), so a type's source is written
 *  once. */
function sourceOf(id: string): string {
  const entry = MERMAID_SOURCES.find((candidate) => candidate.id === id);
  if (entry === undefined) {
    throw new Error(`web/src/features/refs/mermaid-sources.ts has no "${id}" source`);
  }
  return entry.source;
}

/** The type this issue was filed about: upstream Mermaid draws it, beautiful-mermaid has no
 *  layout for it, and its drawing carries its title, its period and its event as three texts. */
const timelineSource = sourceOf("timeline");
const TIMELINE_LABELS = ["Dispatch", "2026", "Drawn"];

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
 *  rather than as the collapsed card's preview: the turn itself above the phone breakpoint, the
 *  full-screen Thread dialog on the phone layout, so a row that runs on `iphone` passes its
 *  project name and a row that runs on a desktop project alone passes none. The turn shows
 *  `lead`, the root comment's first text, before it is expanded, so the click lands on a turn
 *  that has rendered. */
async function openCommentThread(
  page: Page,
  rootId: string,
  lead: string,
  project?: string
): Promise<Locator> {
  const turn = conversationTurns(page).locator(`:scope > li[data-turn="comment:${rootId}"]`);
  await expect(turn).toContainText(lead);
  const expand = turn.getByRole("button", { exact: true, name: "Expand thread" });
  // On the phone layout the thread leaves the turn for the Thread dialog
  // (`conversationThreadScope` in `editor.ts`), which is what says it opened and what holds the
  // bodies the row reads; everywhere else the turn stays in place with a Collapse button.
  const phone = project === "iphone";
  const opened = phone
    ? page.getByRole("dialog", { name: "Thread" })
    : turn.getByRole("button", { exact: true, name: "Collapse thread" });
  // Retried for the reason the reader's click on `Mermaid source` is below: the page moves
  // while the turn's diagrams draw, so a click can land on whatever slid under the pointer and
  // the thread never opens. Measured here on firefox, where the expand click was lost and the
  // row waited out its budget for a `Collapse thread` that never appeared. The expand button is
  // gone once the thread is open, so the retry clicks only while it is still there.
  await expect(async () => {
    if (await expand.isVisible()) await expand.click();
    await expect(opened).toBeVisible({ timeout: 1_000 });
  }).toPass({ timeout: 5_000 });
  return phone ? opened.locator(`li[data-turn="comment:${rootId}"]`) : turn;
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
 *  drop, and each attribute that runs script, links out or fetches. A `foreignObject` is not one
 *  of the dropped elements: the sanitizer keeps it, because a journey's and an eventmodeling's
 *  labels are HTML inside one, and takes what runs or fetches out of it instead - so an `<img>`
 *  is read here wherever it is, a kept `foreignObject` included. `null` when `preview` holds no
 *  shadow root. */
function hostileLeftovers(preview: Locator): Promise<string[] | null> {
  return preview.evaluate((element) => {
    const root = element.shadowRoot;
    if (root === null) return null;
    const dropped = ["script", "image", "img", "use", "a", "iframe"];
    const found: string[] = [];
    for (const node of Array.from(root.querySelectorAll("*"))) {
      if (dropped.includes(node.localName)) found.push(`<${node.localName}>`);
      for (const attribute of Array.from(node.attributes)) {
        const name = attribute.name.toLowerCase();
        const fetches =
          name === "src" || name === "href" || name.endsWith(":href") || name.startsWith("on");
        if (fetches || attribute.value.includes("url(http")) {
          found.push(`${node.localName} ${attribute.name}="${attribute.value}"`);
        }
      }
    }
    return found;
  });
}

/** The elements inside each `foreignObject` the sanitizer kept, one list per kept
 *  `foreignObject`. `HOSTILE_SVG` puts an `<img src=… onerror=…>` in its one, and a label may
 *  hold no such element, so the list a row reads of it is empty. `null` when `preview` holds no
 *  shadow root. */
function keptForeignObjects(preview: Locator): Promise<string[][] | null> {
  return preview.evaluate((element) => {
    const root = element.shadowRoot;
    if (root === null) return null;
    return Array.from(root.querySelectorAll("foreignObject"), (kept) =>
      Array.from(kept.querySelectorAll("*"), (node) => node.localName)
    );
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
  // The one `foreignObject` of the fixture is kept, as a label's host is; the `<img>` it held,
  // which would have fetched and run its `onerror`, is not an element a label may hold.
  expect(await keptForeignObjects(preview)).toEqual([[]]);
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
// @webkit @firefox: the engine's own SVG parser, shadow DOM and ELK layout, over the diagram
// beautiful-mermaid draws, whose sheet is the one that would reach the page and the font.
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

// The version view's call site of `refocusWhenSettled` (`VersionView.tsx`): it refocuses a
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

// ---------------------------------------------------------------------------------------------
// R1: a `timeline` on each of the eight surfaces Dispatch draws a diagram on. The type this
// issue was filed about is the one no flowchart row can stand in for: beautiful-mermaid has no
// layout for it, so each of these rows is the second renderer drawing on that surface. The
// flowchart row beside each of them stays as it was, since it proves the first renderer still
// draws there (R4).

/** `scope` shows the drawn timeline: the diagram carries its title, its period and its event. */
async function expectTimelineDrawing(scope: Locator): Promise<void> {
  const diagram = diagramIn(scope);
  await expect(diagram).toBeVisible();
  for (const label of TIMELINE_LABELS) {
    await expect(diagram).toContainText(label);
  }
}

/** R1 on a body surface: the drawing, and no status line anywhere in the block. */
async function expectDrawnTimeline(scope: Locator): Promise<void> {
  await expectTimelineDrawing(scope);
  await expect(scope.locator(".dispatch-mermaid").getByRole("status")).toHaveCount(0);
}

/** R1 on a document surface, the editor's preview or a past version's: the drawing, and the
 *  block's own status line still hidden. */
async function expectDrawnDocumentTimeline(scope: Locator): Promise<void> {
  await expectTimelineDrawing(scope);
  await expect(scope.locator(".proof-mermaid-status")).toBeHidden();
}

// @webkit @firefox: the worker's DOM shim drawing with Mermaid at all in this engine - its SVG
// parser, its `OffscreenCanvas` text metrics and its shadow DOM - over the `timeline` this issue
// was filed about, which is the type Mermaid draws through that shim.
test("a timeline in a comment draws @phone @webkit @firefox", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Comment timeline" });
  const comment = await createComment(issue.key, {
    body: diagramBody("Comment timeline:", timelineSource),
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await openCommentThread(
      page,
      comment.id,
      "Comment timeline:",
      testInfo.project.name
    );
    await expectDrawnTimeline(card);
  } finally {
    await alice.close();
  }
});

test("a timeline in a reply draws under its comment @phone", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Reply timeline" });
  const root = await createComment(issue.key, { body: "When did each of these land?" });
  await createComment(
    issue.key,
    { body: diagramBody("Here:", timelineSource), reply_to: root.id },
    session
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const thread = await openCommentThread(
      page,
      root.id,
      "When did each of these land?",
      testInfo.project.name
    );
    const reply = thread.getByRole("list", { name: "Replies" }).locator("li[data-reply]");
    await expect(reply).toHaveCount(1);
    await expectDrawnTimeline(reply);
  } finally {
    await alice.close();
  }
});

test("a timeline in an issue message draws in the Conversation @phone", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Message timeline" });
  const message = await createMessage(
    issue.key,
    { body: diagramBody("Message timeline:", timelineSource) },
    session
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = conversationTurns(page).locator(`:scope > li[data-turn="message:${message.id}"]`);
    await expect(card).toContainText("Message timeline:");
    await expectDrawnTimeline(card);
  } finally {
    await alice.close();
  }
});

test("a timeline in a document draws in a past version @phone", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Version timeline:", timelineSource),
    title: "Document timeline",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/artifact/spec?v=1`);
    const versionView = page.getByRole("region", { name: "Document version 1" });
    await expect(versionView).toContainText("Version timeline:");
    await expectDrawnDocumentTimeline(versionView);
  } finally {
    await alice.close();
  }
});

test("a timeline in the document editor's preview draws @phone", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Editor timeline:", timelineSource),
    title: "Editor timeline",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Editor timeline:");
    await expectDrawnDocumentTimeline(documentEditor(page));
  } finally {
    await alice.close();
  }
});

test("a timeline in an ask's question draws on the Inbox card @phone", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Inbox ask timeline" });
  const ask = await createAsk(
    issue.key,
    { question: diagramBody("Does this order hold?", timelineSource) },
    session
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const card = page.getByTestId(`ask-${ask.id}`);
    await expect(card).toContainText("Does this order hold?");
    await expectDrawnTimeline(card);
  } finally {
    await alice.close();
  }
});

test("a timeline in an ask's question draws on the issue page @phone", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Issue page ask timeline" });
  const ask = await createAsk(
    issue.key,
    { question: diagramBody("Does this order hold on the issue?", timelineSource) },
    session
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const turn = conversationTurns(page).locator(`:scope > li[data-turn="ask:${ask.id}"]`);
    await expect(turn).toContainText("Does this order hold on the issue?");
    await expectDrawnTimeline(turn);
  } finally {
    await alice.close();
  }
});

test("a timeline in an ask's answer draws on its answered record @phone", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Answer timeline" });
  const ask = await createAsk(issue.key, { question: "When did each of these land?" }, session);
  await answerAsk(ask.id, {
    expected_edited_at: null,
    selected: [],
    text: diagramBody("Answer timeline:", timelineSource),
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    // `AskCompletionCard`'s own article, which is the answered record: the question above it
    // carries no diagram, so the one drawn here is the answer's.
    const record = page.getByTestId(`ask-${ask.id}`);
    await expect(record).toContainText("Answered by");
    await expectDrawnTimeline(record);
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------------------------
// R2 and R4: which renderer draws which source. A reader cannot tell the two apart from the
// picture, so each row reads the sheet the drawing carries (`rendererOf`): Mermaid prefixes
// every rule that has a selector with the drawing's own id, where beautiful-mermaid's sheet
// styles `text` with its own font and prefixes nothing. The rows run in a document, where the
// source disclosure is open and one page can hold a block per source, and on Chromium alone:
// the routing is the worker's own, the same code in every engine, and a Mermaid load is not
// worth paying four times for it.

/** The sources beautiful-mermaid has a layout for, one per family of `FAST_PATH_HEADERS`
 *  (`mermaid-render.worker.ts`), which must go on reaching it. */
const FAST_PATH_SOURCES: readonly string[] = [
  "graph TD\n  Start --> Finish",
  sourceOf("stateDiagram"),
  sourceOf("sequence"),
  sourceOf("classDiagram"),
  sourceOf("er"),
  sourceOf("xychart"),
];

/** The sources that reach Mermaid: one whose header beautiful-mermaid has no layout for (ELK is
 *  a layout engine of Mermaid's), one whose header hides behind YAML front matter, and two
 *  inside beautiful-mermaid's own families that it refuses - a `graph TD;` written with
 *  semicolons and a `%%` comment above a sequence diagram. The last two are the fault this
 *  issue was filed about: a refusal of the first renderer is not the end of the source. */
const FALL_THROUGH_SOURCES: readonly string[] = [
  sourceOf("flowchart-elk"),
  `---\ntitle: Dispatch\n---\n${sourceOf("flowchart-v2")}`,
  "graph TD;\n  Start --> Finish;",
  `%% Dispatch's own note\n${sourceOf("sequence")}`,
];

/** Every source of `sources` drew in the document editor, in the order they were written, and
 *  `renderer` drew each one. A failure names the source it read, never its place in the list. */
async function expectDrawnBy(
  page: Page,
  sources: readonly string[],
  renderer: "beautiful-mermaid" | "mermaid"
): Promise<void> {
  const previews = documentEditor(page).locator(".proof-mermaid-preview");
  await expect(previews).toHaveCount(sources.length);
  for (const [index, source] of sources.entries()) {
    const preview = previews.nth(index);
    await expect(diagramIn(preview), source).toBeVisible();
    expect(await rendererOf(preview), source).toBe(renderer);
  }
}

// SLOW(>=2s): six real draws of beautiful-mermaid's six families in one document; no renderer
// is loaded for them and there is no wait to shorten.
test("every source beautiful-mermaid has a layout for is drawn by beautiful-mermaid", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the worker's routing is engine-independent");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Fast path:", ...FAST_PATH_SOURCES),
    title: "Fast path routing",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Fast path:");
    await expectDrawnBy(page, FAST_PATH_SOURCES, "beautiful-mermaid");
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): Mermaid's chunks load once for this row and then draw four sources, one of them
// through ELK's own layout; the download and the draws are the whole cost.
test("a source beautiful-mermaid has no layout for, or refuses, is drawn by Mermaid", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the worker's routing is engine-independent");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Fall through:", ...FALL_THROUGH_SOURCES),
    title: "Fall-through routing",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Fall through:");
    await expectDrawnBy(page, FALL_THROUGH_SOURCES, "mermaid");
  } finally {
    await alice.close();
  }
});

// A directive is a `%%` comment to the header reader, so the flowchart under it stays on the
// fast path; either renderer may draw it, and the row reads only that a diagram a reader sees
// today is still a diagram (R4).
test("a flowchart behind an init directive goes on drawing", async ({ browser }, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the worker's routing is engine-independent");
  await createProject({ key: "CORE", name: "Core" });
  const directed = '%%{init: {"theme": "base"}}%%\ngraph TD\n  Start --> Finish';
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Directive:", directed),
    title: "Directive routing",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Directive:");
    const editor = documentEditor(page);
    await expect(diagramIn(editor)).toBeVisible();
    await expect(editor.locator(".proof-mermaid-status")).toBeHidden();
    expect(await rendererOf(editor.locator(".proof-mermaid-preview"))).not.toBe("neither");
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------------------------
// R3: what a source no renderer draws shows. Each row runs in a document, where the source
// disclosure is open and the block's own status element is the one line a reader reads.

/** The status line's prefix, the longest it may be, and the most of its own source a reason may
 *  quote, each under the name the production module writes it under (R3,
 *  `mermaid-status.ts` for the first two, `mermaid-render.worker.ts` for the third). */
const STATUS_PREFIX = "Unable to render Mermaid diagram: ";
const STATUS_LINE_MAX_CHARS = 200;
const SOURCE_CHARS_IN_REASON = 120;

/** What R3 asks of a refused block in the document editor: the source shown as code (read
 *  through `shown`, an excerpt of it), no diagram, and one status line, which is one line of at
 *  most 200 characters holding neither the old `Invalid mermaid header` nor more of the source
 *  than a reason may quote. Answers that line, so a row can read the words of its own refusal.
 *
 *  The quote check reads one character more than a reason may carry: a source longer than that
 *  is cut, and a source shorter than it is never quoted whole by any reason here. */
async function expectDocumentRefusal(page: Page, source: string, shown: string): Promise<string> {
  const editor = documentEditor(page);
  const status = editor.locator(".proof-mermaid-status");
  await expect(status).toHaveCount(1);
  await expect(status).toContainText(STATUS_PREFIX);
  await expect(status).toBeVisible();
  const line = (await status.textContent()) ?? "";
  expect(line.length, line).toBeLessThanOrEqual(STATUS_LINE_MAX_CHARS);
  expect(line).not.toContain("\n");
  expect(line).not.toContain("Invalid mermaid header");
  const oneLine = source.replace(/\s+/g, " ").trim();
  expect(line).not.toContain(oneLine.slice(0, SOURCE_CHARS_IN_REASON + 1));
  await expect(editor.locator("pre code").filter({ hasText: shown })).toBeVisible();
  await expect(diagramIn(editor)).toHaveCount(0);
  return line;
}

/** A source whose first word names no type either renderer draws, written long enough that the
 *  reason quoting it has to cut it. */
const UNTYPED_SOURCE = [
  "notadiagram TD",
  "  Reader --> Dispatch",
  "  Dispatch --> Worker",
  "  Worker --> Renderer",
  "  Renderer --> Drawing",
  "  Drawing --> Reader",
  "  Reader --> Dispatch again",
].join("\n");

test("a source whose first word is no type shows one reason holding part of that source", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Untyped:", UNTYPED_SOURCE),
    title: "Untyped source",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Untyped:");
    const line = await expectDocumentRefusal(page, UNTYPED_SOURCE, "notadiagram TD");
    // The source says more than any wording could, so the reason quotes it - its first 120
    // characters on one line, with an ellipsis where the rest was cut - and names no type of
    // its own, neither renderer's guess at one included.
    const oneLine = UNTYPED_SOURCE.replace(/\s+/g, " ").trim();
    expect(line).toBe(
      `${STATUS_PREFIX}no renderer draws a diagram that begins "${oneLine.slice(0, SOURCE_CHARS_IN_REASON)}…"`
    );
  } finally {
    await alice.close();
  }
});

/** A timeline Mermaid's own parser refuses: `::` separates no period from an event. */
const UNPARSED_TIMELINE = "timeline\n  title Dispatch\n  2026 :: Drawn :";

test("a timeline Mermaid cannot parse shows one line of its parse error", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Unparsed:", UNPARSED_TIMELINE),
    title: "Unparsed timeline",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Unparsed:");
    const line = await expectDocumentRefusal(page, UNPARSED_TIMELINE, "2026 :: Drawn");
    // Mermaid's own message runs to several lines, an excerpt of the source and a ruler under
    // the token it failed on among them; the reader gets its first line, cut to fit.
    expect(line).toMatch(/^Unable to render Mermaid diagram: Parse error on line \d+: /);
  } finally {
    await alice.close();
  }
});

/** A flowchart whose last line ends in an arrow that reaches no node: the source
 *  beautiful-mermaid refuses in its own validation (`Dangling Mermaid edge`), which now falls
 *  through to Mermaid, whose parser refuses it in turn. */
const DANGLING_EDGE_SOURCE = "graph TD\n  A -->";

test("a flowchart ending in a dangling edge shows one line of Mermaid's parse error", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Dangling:", DANGLING_EDGE_SOURCE),
    title: "Dangling edge",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Dangling:");
    const line = await expectDocumentRefusal(page, DANGLING_EDGE_SOURCE, "A -->");
    // Before the second renderer this source ended at beautiful-mermaid's refusal, and
    // `Dangling Mermaid edge` is what a reader was shown; it now falls through to Mermaid, so
    // the line is Dispatch's one-line form of that parser's error and names no renderer's own
    // words for the source (`Invalid mermaid header` is read by the helper above).
    expect(line).toMatch(/^Unable to render Mermaid diagram: Parse error on line \d+: /);
    expect(line).not.toContain("Dangling Mermaid edge");
  } finally {
    await alice.close();
  }
});

/** A Mermaid-drawn source one character longer than the longest Mermaid is asked to draw
 *  (`MERMAID_MAX_SOURCE_CHARS`), padded with a `%%` comment so the padding draws nothing and the
 *  header stays the timeline's. */
function overlongTimeline(): string {
  const head = `${timelineSource}\n%% `;
  return head + "x".repeat(50_001 - head.length);
}

test("a Mermaid-drawn source past the size limit shows one reason naming the limit", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the size limit is engine-independent");
  await createProject({ key: "CORE", name: "Core" });
  const source = overlongTimeline();
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Overlong:", source),
    title: "Overlong source",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Overlong:");
    const line = await expectDocumentRefusal(page, source, "title Dispatch");
    expect(line).toBe(`${STATUS_PREFIX}it is longer than 50,000 characters`);
  } finally {
    await alice.close();
  }
});

/** A flowchart drawn with Mermaid's `img:` shape, whose picture would be fetched from
 *  `hostile.example`. ELK's layout keeps the source off beautiful-mermaid's fast path, so the
 *  shape reaches the renderer that has one for it. */
const IMAGE_SHAPE_SOURCE =
  'flowchart-elk TD\n  a@{ img: "https://hostile.example/shape.png", label: "Picture" }';

// SLOW(>=2s): Mermaid's chunks load for this row before its source is refused; the download is
// the whole cost, and the margin below is a quarter of a second of it.
test("a diagram shape that would fetch a picture is refused and fetches nothing", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Picture shape:", IMAGE_SHAPE_SOURCE),
    title: "Picture shape source",
  });

  const alice = await asUser(browser, "alice");
  try {
    const requested = await recordHostileRequests(alice);
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Picture shape:");
    // The refusal is the positive signal the absence below is taken after: the renderer has
    // been through this source and answered for it.
    await expectDocumentRefusal(page, IMAGE_SHAPE_SOURCE, "img:");
    await sleep(REQUEST_MARGIN_MS);
    expect(requested).toEqual([]);
  } finally {
    await alice.close();
  }
});

/** A flowchart of 20 nodes carrying 60,000 characters of labels between them: longer than the
 *  longest source Mermaid is asked to draw, and beautiful-mermaid's to draw all the same. */
function longLabelledFlowchart(): string {
  const label = "L".repeat(3_000);
  const nodes = Array.from({ length: 20 }, (_, node) => `  n${node}["${label}"]`);
  const edges = Array.from({ length: 19 }, (_, node) => `  n${node} --> n${node + 1}`);
  return `graph TD\n${nodes.join("\n")}\n${edges.join("\n")}`;
}

// SLOW(>=2s): one real draw of 60,000 characters of labels, and the document write that seeds
// them; the row waits out no timer.
test("a flowchart of 60,000 characters of labels draws, the size limit being Mermaid's alone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const source = longLabelledFlowchart();
  expect(source.length).toBeGreaterThan(50_000);
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Long labels:", source),
    title: "Long-label flowchart",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Long labels:");
    const editor = documentEditor(page);
    await expect(diagramIn(editor)).toBeVisible();
    await expect(editor.locator(".proof-mermaid-status")).toBeHidden();
    expect(await rendererOf(editor.locator(".proof-mermaid-preview"))).toBe("beautiful-mermaid");
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------------------------
// The worker's bracket and the page's two bounds, which no other row reads.

// SLOW(>=2s): Mermaid's chunks load for the timeline before the flowchart is drawn, and both
// draws are real; there is no wait to shorten.
//
// The one row that proves the worker's bracket holds. Loading Mermaid installs a global
// `document` and 26 others for it (`mermaid-dom-shim.ts`), and ELK, which lays beautiful-
// mermaid's flowcharts out, refuses to build while a global `document` exists. So a flowchart
// drawn after a Mermaid source in the same worker is the case that breaks without the bracket
// `withoutADom` puts around each beautiful-mermaid call, and one page holding a timeline above
// a flowchart is that case: one worker, the timeline first, the flowchart after it.
test("a flowchart after a Mermaid diagram in one page still draws, on beautiful-mermaid @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Both renderers" });
  const comment = await createComment(issue.key, {
    body: diagramBody("Both:", timelineSource, "graph TD\n  Start --> Finish"),
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await openCommentThread(page, comment.id, "Both:", testInfo.project.name);
    const previews = card.locator(".dispatch-mermaid-preview");
    await expect(previews).toHaveCount(2);
    await expect(diagramIn(previews.nth(0))).toBeVisible();
    await expect(diagramIn(previews.nth(1))).toContainText("Finish");
    expect(await rendererOf(previews.nth(0))).toBe("mermaid");
    expect(await rendererOf(previews.nth(1))).toBe("beautiful-mermaid");
    await expect(card.locator(".dispatch-mermaid").getByRole("status")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

/** How long Mermaid's first chunk is held back: longer than `RENDER_TIME_LIMIT_MS`, so a job
 *  under one bound would be refused, and far inside `RENDERER_LOAD_LIMIT_MS`, which is the
 *  bound a loading renderer is really under. */
const CHUNK_DELAY_MS = 6_000;

// SLOW(>=2s): the row holds Mermaid's first chunk back for six seconds, which is the claim
// itself - a renderer that loads for longer than the render limit is still drawn with.
test("a source Mermaid draws still draws when its chunks load slower than the render limit", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the page's two bounds are engine-independent");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Delayed renderer:", "graph TD;\n  Start --> Finish;"),
    title: "Delayed renderer",
  });

  const alice = await asUser(browser, "alice");
  try {
    const chunks = await delayMermaidChunks(alice, CHUNK_DELAY_MS);
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Delayed renderer:");
    const editor = documentEditor(page);
    // Well past the render limit the job would be under with one bound, and well inside the
    // load bound it is really under.
    await expect(diagramIn(editor)).toBeVisible({ timeout: 25_000 });
    await expect(editor.locator(".proof-mermaid-status")).toBeHidden();
    expect(
      chunks(),
      "the page asked for Mermaid's chunks, so the delay was on its path"
    ).toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});

// The worker takes the source and never says it has begun to draw (`holdWorkerSource`'s
// `"none"`), as a renderer whose download never finishes leaves it. The job is under the load
// bound, not the render bound, so the page's own clock - the clock every time-limit row here
// drives - shows the render limit passing with the block still waiting, and the load bound
// ending it.
test("a job the worker never begins is ended by the load bound, not the render bound @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Unbegun job" });
  const comment = await createComment(issue.key, {
    body: flowchart("Unbegun:", "Start", "Finish"),
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const holds = await holdWorkerSource(page, "none");
    await page.clock.install();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await openCommentThread(page, comment.id, "Unbegun:", testInfo.project.name);
    const refusal = card
      .getByRole("status")
      .filter({ hasText: "Unable to render Mermaid diagram" });
    await expectWorkerHeld(holds);
    await page.clock.fastForward(5_000);
    // The render limit has passed and the block still waits: it never started.
    await expect(refusal).toHaveCount(0);
    await expect(diagramIn(card)).toHaveCount(0);
    await page.clock.fastForward(25_000);
    await expect(refusal).toHaveCount(1);
    await expect(card.locator("pre code").filter({ hasText: "Start --> Finish" })).toBeVisible();
    await expect(diagramIn(card)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a job that reaches the load bound says the renderer did not load @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Unloaded renderer" });
  const comment = await createComment(issue.key, {
    body: flowchart("Unloaded:", "Start", "Finish"),
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const holds = await holdWorkerSource(page, "none");
    await page.clock.install();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await openCommentThread(page, comment.id, "Unloaded:", testInfo.project.name);
    await expectWorkerHeld(holds);
    await page.clock.fastForward(30_000);
    const refusal = card
      .getByRole("status")
      .filter({ hasText: "Unable to render Mermaid diagram" });
    await expect(refusal).toHaveText(
      "Unable to render Mermaid diagram: its renderer did not load within 30 seconds"
    );
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------------------------
// R5: the four paths a page takes around the worker - the worker holding the source, no worker
// at all, a worker that fails after a drawing, and a redraw asked while a draw is in flight -
// each with a source only Mermaid draws, and then what a page fetches while it draws. The four
// rows above read those paths with a flowchart, which beautiful-mermaid draws on the worker's
// first call; the twins here read them with a timeline, whose renderer is downloaded and loaded
// first, so a reader whose diagram needs the second renderer is covered on all four. The twins
// are rows of their own rather than a second source in the rows above, because each of those
// reads one block from open to answer and a second block would change what it proves.

// The timeline twin of the comment row above, on the same hold (`holdWorkerSource`) and the same
// clock: the source the held worker takes is one beautiful-mermaid has no layout for, and the
// shipped limit refuses it in the same words while the page's own thread goes on answering.
test("a timeline too large to draw in time shows the reason, and the page keeps responding @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Large timeline" });
  const comment = await createComment(issue.key, {
    body: diagramBody("Large timeline:", timelineSource),
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const holds = await holdWorkerSource(page);
    await page.clock.install();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await openCommentThread(
      page,
      comment.id,
      "Large timeline:",
      testInfo.project.name
    );
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
    await expect(card.locator("pre code").filter({ hasText: "2026 : Drawn" })).toBeVisible();
    await expect(diagramIn(card)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("with no worker a document's timeline shows its source @phone", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Workerless timeline:", timelineSource),
    title: "Workerless document timeline",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const expectSourceAsCode = watchSourceAsCode(page);
    await refuseWorkers(page);
    await openSpec(page, issue.key, "Workerless timeline:");
    await expectSourceAsCode("2026 : Drawn");
  } finally {
    await alice.close();
  }
});

// The timeline twin of the row above it: a worker that drew once can fail later, and the block
// then shows its edited source as code rather than the drawing of a source it no longer matches.
test("a document timeline whose worker fails after its first drawing shows its source @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Failing timeline:", timelineSource),
    title: "Failing document timeline",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const expectSourceAsCode = watchSourceAsCode(page);
    await failMermaidWorkersOnCall(page);
    await openSpec(page, issue.key, "Failing timeline:");
    const editor = documentEditor(page);
    await expect(diagramIn(editor)).toContainText("Drawn");
    await page.evaluate(() => window.__failMermaidWorkers?.());

    await placeCaret(page, "after", "2026 : Drawn");
    await page.keyboard.type("Late");
    await expectSourceAsCode("2026 : DrawnLate");
  } finally {
    await alice.close();
  }
});

// The fourth path: the source is edited while the worker still holds the first draw. Nothing the
// workers post reaches the page until the release (`holdMermaidWorkers`), so the first draw is
// in flight for as long as the edit takes; the queue abandons it, replaces the worker and posts
// the edited source to the new one, and the block ends on the drawing of what the reader wrote
// rather than on the drawing of what they deleted.
//
// SLOW(>=2s): the abandoned first draw costs the worker its life, so Mermaid is loaded twice in
// this one row - once in the worker that took the first source, once in its replacement.
test("a timeline edited while its first drawing is in flight draws the edited source @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Edited timeline:", timelineSource),
    title: "Edited document timeline",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await holdMermaidWorkers(page);
    await openSpec(page, issue.key, "Edited timeline:");
    const editor = documentEditor(page);
    const diagram = diagramIn(editor);
    await expect(diagram).toHaveCount(0);

    await placeCaret(page, "after", "2026 : Drawn");
    await page.keyboard.type("Late");
    await page.evaluate(() => window.__releaseMermaid?.());
    await expect(diagram).toContainText("DrawnLate");
    await expect(editor.locator(".proof-mermaid-status")).toBeHidden();
    expect(await rendererOf(editor.locator(".proof-mermaid-preview"))).toBe("mermaid");
  } finally {
    await alice.close();
  }
});

/** A mindmap of exactly the longest source Mermaid is asked to draw
 *  (`MERMAID_MAX_SOURCE_CHARS`), which is some two thousand nodes for its layout: far more than
 *  the render limit allows on any machine, and not one character more than the size limit, so
 *  the limit the block ends on is the time one. */
function hugeMindmap(): string {
  const lines = ["mindmap", "  root((Dispatch))"];
  let written = lines.join("\n").length;
  for (let node = 0; written < 50_000; node += 1) {
    const line = `    branch ${node} of the Dispatch mindmap`;
    lines.push(line);
    written += line.length + 1;
  }
  return lines.join("\n").slice(0, 50_000);
}

// SLOW(>=2s): the row downloads Mermaid's chunks and then draws a 50,000-character mindmap for
// real until the shipped 5 s limit ends it. The clock is the machine's, not the page's: the
// claim is that a draw this page never finishes is ended, which an advanced clock would grant
// for free.
test("a 50,000-character mindmap is ended by the time limit while the page answers", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the time limit is a page timer, in any engine");
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: `${diagramBody("Huge mindmap:", hugeMindmap())}\nTail paragraph.\n`,
    title: "Huge mindmap",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Huge mindmap:");
    const editor = documentEditor(page);
    const status = editor.locator(".proof-mermaid-status");
    // The draw is in flight: the worker holds the source, and the block shows neither a drawing
    // nor a reason yet.
    await expect(diagramIn(editor)).toHaveCount(0);
    await expect(status).toBeHidden();

    // The page answers an interaction while that draw runs: the editor takes the typing and
    // paints it, on the thread the layout would have held.
    await placeCaret(page, "after", "Tail paragraph.");
    await page.keyboard.type("Typed");
    await expect(editor.getByText("Tail paragraph.Typed")).toBeVisible();
    await expect(status).toBeHidden();

    await expect(status).toHaveText(
      "Unable to render Mermaid diagram: it did not finish within 5 seconds",
      { timeout: 60_000 }
    );
    await expect(diagramIn(editor)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): the row holds Mermaid's first chunk back for six seconds, which is the claim
// itself - a renderer that loads for longer than the render limit still draws the timeline that
// waited for it, rather than being refused under a bound the download is no part of. The delay
// stays well inside the thirty-second load bound, or the row would read that bound instead.
test("a timeline draws when Mermaid's chunks load slower than the render limit @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Delayed timeline:", timelineSource),
    title: "Delayed timeline",
  });

  const alice = await asUser(browser, "alice");
  try {
    const chunks = await delayMermaidChunks(alice, CHUNK_DELAY_MS);
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Delayed timeline:");
    const editor = documentEditor(page);
    // Well past the render limit the job would be under with one bound, and well inside the
    // load bound it is really under.
    await expect(diagramIn(editor)).toBeVisible({ timeout: 25_000 });
    await expectDrawnDocumentTimeline(editor);
    expect(
      chunks(),
      "the page asked for Mermaid's chunks, so the delay was on its path"
    ).toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});

// @webkit @firefox: the shadow DOM, here over a Mermaid-drawn diagram rather than a
// beautiful-mermaid one: the engine keeps that drawing's sheet and its ids inside the one shadow
// root it was mounted in.
test("a timeline's styles and ids reach nothing outside its own shadow root @phone @webkit @firefox", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Scoped timeline:", timelineSource),
    title: "Scoped timeline",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Scoped timeline:");
    const editor = documentEditor(page);
    await expectDrawnDocumentTimeline(editor);
    const preview = editor.locator(".proof-mermaid-preview");

    // Sentinels of the page's own: a `text` inside an `svg`, beside a `span`. Mermaid writes one
    // unprefixed rule, which sets the drawing's own font and fill by its id; an unprefixed
    // selector would reach these two instead.
    const sentinels = await page.evaluate(() => {
      const namespace = "http://www.w3.org/2000/svg";
      const svg = document.createElementNS(namespace, "svg");
      const text = document.createElementNS(namespace, "text");
      text.textContent = "Sentinel";
      svg.append(text);
      const span = document.createElement("span");
      span.textContent = "Sentinel";
      document.body.append(svg, span);
      const read = {
        spanFont: getComputedStyle(span).fontFamily,
        textFill: getComputedStyle(text).fill,
        textFont: getComputedStyle(text).fontFamily,
      };
      svg.remove();
      span.remove();
      return read;
    });
    expect(sentinels.textFont).toBe(sentinels.spanFont);
    expect(sentinels.textFill).toBe("rgb(0, 0, 0)");

    // No sheet of the page's own holds the drawing's rules, every one of which names the
    // drawing's own id; the two rules the page does write for a diagram's figure
    // (`.dispatch-mermaid-preview`, `styles.css`) name a class and are no part of a drawing.
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
          if (rule.cssText.includes("#dispatch-mermaid-")) found.push(rule.cssText);
        }
      }
      return found;
    });
    expect(pageRules).toEqual([]);

    // The drawing names its own id, its markers and its arrowheads; a document that holds any of
    // them would have two elements under one id, and a link or a paint of the page could land in
    // the diagram. Reading the ids is also the positive signal: the drawing is installed.
    const ids = await preview.evaluate((element) =>
      Array.from(element.shadowRoot?.querySelectorAll("[id]") ?? [], (node) => node.id)
    );
    expect(ids.length, "the drawing carries ids of its own").toBeGreaterThan(0);
    const inTheDocument = await page.evaluate(
      (names) => names.filter((name) => document.getElementById(name) !== null),
      ids
    );
    expect(inTheDocument).toEqual([]);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): Mermaid's chunks are downloaded for the timeline, which is most of what this row
// reads - every one of them under `/assets` and none of them anyone else's.
//
// The requests are recorded by a route of the context's rather than by its `request` event: a
// Web Worker's own imports, which is how the renderer is downloaded, reach the event in Chromium
// and not in WebKit, where they would go unread and the row would pass on a page that fetched
// nothing at all. A route reads them in every engine, as `delayMermaidChunks` does. The one
// thing no route sees is the live socket's handshake, which is this origin's `/ws/live`.
test("every request a page makes while a timeline draws is its own origin's @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Fetching timeline:", timelineSource),
    title: "Fetching timeline",
  });

  const alice = await asUser(browser, "alice");
  try {
    const requests: { path: string; url: string }[] = [];
    await alice.route("**/*", async (route) => {
      const url = new URL(route.request().url());
      requests.push({ path: url.pathname, url: url.href });
      await route.continue();
    });
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Fetching timeline:");
    const editor = documentEditor(page);
    await expectDrawnDocumentTimeline(editor);
    await sleep(REQUEST_MARGIN_MS);

    const host = new URL(page.url()).host;
    expect(requests.filter((request) => new URL(request.url).host !== host)).toEqual([]);
    // What the drawing adds to the page's own routes and API calls is script: the worker's entry
    // chunk and the renderer's, every one of them under `/assets`.
    const scripts = requests.filter((request) => request.path.endsWith(".js"));
    expect(scripts.filter((request) => !request.path.startsWith("/assets/"))).toEqual([]);
    expect(
      scripts.filter((request) => request.path.includes("/assets/mermaid-chunk-")).length,
      "the renderer's own chunks are among the requests read here"
    ).toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});

/** A timeline whose own directive carries a `themeCSS` that would fetch twice over: an `@import`
 *  of a sheet and a paint of an address. `themeCSS` is on the renderer's `secure` list, so
 *  Mermaid drops the key from a source's directive and it never reaches the drawing's `<style>`;
 *  were it to reach one, the sanitizer would take the `@import` out of that sheet and the
 *  fetching `url(…)` out of every attribute. */
const THEME_CSS_SOURCE = [
  '%%{init: {"themeCSS": "@import url(https://hostile.example/theme.css);',
  ' .node { fill: url(https://hostile.example/paint.svg#p); }"}}%%',
  `\n${timelineSource}`,
].join("");

// SLOW(>=2s): Mermaid's chunks load for this row before its source is drawn; the download is the
// whole cost, and the margin below is a quarter of a second of it.
test("a source whose themeCSS would fetch draws, and fetches nothing", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the renderer's own configuration is one list");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Theme CSS:", THEME_CSS_SOURCE),
    title: "Theme CSS source",
  });

  const alice = await asUser(browser, "alice");
  try {
    const requested = await recordHostileRequests(alice);
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Theme CSS:");
    const editor = documentEditor(page);
    // The drawing is the positive signal the absence below is taken after: the renderer has been
    // through this source, and the sheet that would fetch is mounted and resolved.
    await expectDrawnDocumentTimeline(editor);
    const ownStyles = await editor
      .locator(".proof-mermaid-preview")
      .evaluate((preview) =>
        Array.from(
          preview.shadowRoot?.querySelectorAll("style") ?? [],
          (style) => style.textContent
        )
      );
    expect(ownStyles.filter((style) => style?.includes("hostile.example"))).toEqual([]);
    await sleep(REQUEST_MARGIN_MS);
    expect(requested).toEqual([]);
  } finally {
    await alice.close();
  }
});

test("a page whose only diagram is a flowchart asks for none of Mermaid's chunks", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the worker's routing is engine-independent");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: flowchart("Flowchart only:", "Start", "Finish"),
    title: "Flowchart-only document",
  });

  const alice = await asUser(browser, "alice");
  try {
    const chunks = await recordMermaidChunks(alice);
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Flowchart only:");
    const editor = documentEditor(page);
    await expect(diagramIn(editor)).toContainText("Finish");
    expect(await rendererOf(editor.locator(".proof-mermaid-preview"))).toBe("beautiful-mermaid");
    await sleep(REQUEST_MARGIN_MS);
    const asked = chunks();
    // The DOM shim rides inside one of those chunks, so a page that fetched none of them loaded
    // no shim either. The worker's own entry chunk is no Mermaid chunk, and is fetched: it is
    // what drew.
    expect(asked.chunks).toEqual([]);
    expect(asked.workerEntries.length, "the page started the worker it drew with").toBeGreaterThan(
      0
    );
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------------------------
// R6: what the sanitizer keeps, and what it takes out. The first row below draws one source of
// every type Mermaid registers and reads both halves of that page at once - that each type draws
// the label its own entry names, and that the drawing a reader sees holds every element its
// renderer emitted - because the two readings are one document of 37 real draws and drawing them
// twice would buy nothing. The rows after it write the markup no renderer should ever draw.

/** The elements the sanitizer keeps (`packages/proof-editor/src/mermaid-svg.ts`'s
 *  `ALLOWED_ELEMENTS`), restated here because the row reads a drawing from outside the app: a
 *  name a renderer emitted that is on this list must still be in the mounted drawing. */
const KEPT_ELEMENTS: readonly string[] = [
  "svg",
  "defs",
  "g",
  "marker",
  "path",
  "line",
  "polyline",
  "polygon",
  "rect",
  "circle",
  "ellipse",
  "text",
  "tspan",
  "title",
  "desc",
  "style",
  "linearGradient",
  "stop",
  "filter",
  "feDropShadow",
  "clipPath",
  "switch",
  "symbol",
  "foreignObject",
];

/** The eight of them the list was widened for, which only Mermaid emits: a reader cannot tell a
 *  dropped gradient from a drawing that never had one, so the row records which drawing carried
 *  which. */
const MERMAID_ONLY_ELEMENTS: readonly string[] = [
  "filter",
  "feDropShadow",
  "linearGradient",
  "stop",
  "clipPath",
  "switch",
  "symbol",
  "foreignObject",
];

/** What the row that draws every registered type reads of one block: the drawing the reader has,
 *  beside the markup the worker answered with for that block's own source. */
interface TypeDrawing {
  /** The element names the renderer emitted, outside a label's own subtree. */
  emitted: string[];
  /** How many elements are painted with a `url(#…)` naming a gradient of the same drawing, which
   *  is how a sankey's links are painted and the one way they are visible at all. */
  gradientPaints: number;
  /** The text of every kept `foreignObject`, where journey and eventmodeling put their labels. */
  labelText: string;
  /** The element names the mounted drawing holds, outside a label's own subtree. */
  mounted: string[];
  /** Whether a drawing is mounted in the block's shadow root at all. */
  drawn: boolean;
  renderer: string | null;
  /** How many shapes the drawing paints, which is what an incomplete picture is read by. */
  shapes: number;
  /** The block's status line, or `null` where it shows none. */
  status: string | null;
  /** The text of every `text` element of the drawing. */
  text: string;
}

/** Reads one `TypeDrawing` per source of `sources`, in the order the sources were written into
 *  the document, pairing each block with what the worker answered for that block's source
 *  (`recordMermaidAnswers`). */
function readTypeDrawings(page: Page, sources: readonly string[]): Promise<TypeDrawing[]> {
  return page.evaluate((written) => {
    const answers = window.__mermaidAnswers ?? [];
    const blocks = Array.from(
      document.querySelectorAll('[aria-label="Document editor"] .proof-mermaid-block')
    );
    const shapeNames = ["rect", "circle", "path", "line", "polygon", "polyline", "ellipse"];
    /** Every element name under `root`, its own included, outside a `foreignObject`'s content:
     *  what a label may hold is a list of the sanitizer's own, which other rows read. */
    const census = (root: Element): string[] => {
      const names = new Set<string>([root.localName]);
      const walk = (node: Element) => {
        for (const child of Array.from(node.children)) {
          names.add(child.localName);
          if (child.localName !== "foreignObject") walk(child);
        }
      };
      walk(root);
      return Array.from(names).sort();
    };
    const shapesIn = (root: Element): number =>
      Array.from(root.querySelectorAll("*")).filter((node) => shapeNames.includes(node.localName))
        .length;
    const gradientPaintsIn = (root: Element): number =>
      Array.from(root.querySelectorAll("*")).filter((node) => {
        const paint = ["fill", "stroke", "style"]
          .map((name) => node.getAttribute(name) ?? "")
          .join(" ");
        const named = /url\(["']?#([^)"']+)["']?\)/.exec(paint);
        const painted = named === null ? null : root.querySelector(`[id="${named[1]}"]`);
        return painted !== null && painted.localName === "linearGradient";
      }).length;
    /** The `<svg>` of the markup a renderer wrote, read with that renderer's own parse:
     *  beautiful-mermaid writes XML, Mermaid HTML (`mermaid-diagrams.ts`). */
    const emittedRoot = (markup: string, renderer: string | null): Element | null => {
      const parsed = new DOMParser().parseFromString(
        markup,
        renderer === "mermaid" ? "text/html" : "image/svg+xml"
      );
      return renderer === "mermaid" ? parsed.body.firstElementChild : parsed.documentElement;
    };
    return written.map((source, index) => {
      const block = blocks[index] ?? null;
      const status = block?.querySelector(".proof-mermaid-status")?.textContent ?? "";
      const drawing =
        block?.querySelector(".proof-mermaid-preview")?.shadowRoot?.querySelector("svg") ?? null;
      const answer = answers.find((candidate) => candidate.source === source) ?? null;
      const emitted =
        answer === null || answer.markup === null
          ? null
          : emittedRoot(answer.markup, answer.renderer);
      return {
        drawn: drawing !== null,
        emitted: emitted === null ? [] : census(emitted),
        gradientPaints: drawing === null ? 0 : gradientPaintsIn(drawing),
        labelText:
          drawing === null
            ? ""
            : Array.from(
                drawing.querySelectorAll("foreignObject"),
                (label) => label.textContent ?? ""
              ).join(" "),
        mounted: drawing === null ? [] : census(drawing),
        renderer: answer?.renderer ?? null,
        shapes: drawing === null ? 0 : shapesIn(drawing),
        status: status.trim() === "" ? null : status.trim(),
        text:
          drawing === null
            ? ""
            : Array.from(drawing.querySelectorAll("text"), (label) => label.textContent ?? "").join(
                " "
              ),
      };
    });
  }, sources);
}

// SLOW(>=2s): 37 real draws, one source of every type Mermaid registers, in one document.
// Mermaid's chunks load once for them and ELK's layout runs inside that; there is no wait to
// shorten, and no part of this row is a timer.
test("every registered diagram type draws, and keeps what its renderer emitted", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one page of 37 drawings is Chromium's to pay");
  test.setTimeout(240_000);
  await createProject({ key: "CORE", name: "Core" });
  const sources = MERMAID_SOURCES.map((entry) => entry.source);
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Every type:", ...sources),
    title: "Every registered type",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const answers = await recordMermaidAnswers(page);
    await openSpec(page, issue.key, "Every type:");
    const previews = documentEditor(page).locator(".proof-mermaid-preview");
    await expect(previews).toHaveCount(sources.length);
    // Every block has been answered for, so the reads below are of a settled page.
    await expect
      .poll(async () => (await answers()).length, { timeout: 180_000 })
      .toBe(sources.length);
    const readings = await readTypeDrawings(page, sources);

    const failures: string[] = [];
    const record: string[] = [];
    for (const [index, entry] of MERMAID_SOURCES.entries()) {
      const reading = readings[index];
      const note = (why: string) => failures.push(`${entry.id}: ${why}`);
      if (reading === undefined) {
        note("has no block in the document");
        continue;
      }
      if (reading.status !== null) note(`shows a status line, "${reading.status}"`);
      if (!reading.drawn) {
        note("mounted no drawing");
        record.push(`${entry.id}: no drawing`);
        continue;
      }
      const where =
        entry.label === null
          ? "its own reading"
          : reading.text.includes(entry.label)
            ? "a text element"
            : reading.labelText.includes(entry.label)
              ? "a foreignObject label"
              : "nowhere";
      if (where === "nowhere") note(`its drawing carries no "${entry.label}"`);
      // The two types a row reads differently, each for the reason its own entry gives: an
      // architecture diagram draws no icon, since Dispatch registers no icon pack, so it is read
      // by the shapes it does draw; the info diagram's only text is Mermaid's version string.
      if (entry.id === "architecture") {
        if (reading.shapes === 0) note("drew none of its shapes");
        if (reading.emitted.includes("image")) note("its renderer emitted an icon to fetch");
      }
      if (entry.id === "info" && reading.text.trim() === "") note("drew no text at all");
      // A sankey's links are painted with the gradients the sanitizer keeps; without them they
      // are drawn in no colour at all and the diagram is two columns of boxes.
      if (entry.id === "sankey" && reading.gradientPaints === 0) {
        note("its links are painted with no gradient of the drawing");
      }
      const kept = reading.emitted.filter((name) => KEPT_ELEMENTS.includes(name));
      const lost = kept.filter((name) => !reading.mounted.includes(name));
      const extra = reading.mounted.filter((name) => !kept.includes(name));
      if (lost.length > 0) note(`the drawing lost ${lost.join(", ")}, which its renderer emitted`);
      if (extra.length > 0) note(`the drawing holds ${extra.join(", ")}, which it was not given`);
      const carried = MERMAID_ONLY_ELEMENTS.filter((name) => reading.mounted.includes(name));
      record.push(
        `${entry.id}: drawn by ${reading.renderer ?? "no renderer"}, label in ${where}, ` +
          `${reading.mounted.length} element names, ${reading.shapes} shapes, ` +
          `kept ${carried.join("/") || "none of the eight"}`
      );
    }

    const drawn = readings.filter((reading) => reading.drawn).length;
    const table = `${drawn} of ${sources.length} types drew\n${record.join("\n")}`;
    await testInfo.attach("every registered diagram type", {
      body: table,
      contentType: "text/plain",
    });
    console.log(table);
    expect(failures, "every type draws and keeps what its renderer emitted").toEqual([]);
    expect(drawn).toBe(sources.length);
  } finally {
    await alice.close();
  }
});

/** One hostile source a reader can write, by the way it would run, fetch or link out. Each goes
 *  to Mermaid: `flowchart-elk` is a layout engine of its own, and eventmodeling is a type
 *  beautiful-mermaid has no layout for. The last of them raises `securityLevel` in its own
 *  directive, which the renderer's `secure` list deletes before the source is read. */
const HOSTILE_MERMAID_SOURCES: readonly { id: string; source: string }[] = [
  {
    id: "a script in a label",
    source:
      'flowchart-elk TD\n  a["<script>window.__hostile.push(\'label script\')</script>"] --> b["Drawn"]',
  },
  {
    id: "an img with an onerror in a label",
    source:
      'flowchart-elk TD\n  a["<img src=https://hostile.example/label.png onerror=window.__hostile.push(\'img onerror\')>"] --> b["Drawn"]',
  },
  {
    id: "a click with a javascript: URL",
    source:
      'flowchart-elk TD\n  a["Dispatch"] --> b["Drawn"]\n  click a "javascript:window.__hostile.push(\'click\')"',
  },
  {
    id: "an anchor with a javascript: href in a label",
    source:
      'flowchart-elk TD\n  a["<a href=javascript:window.__hostile.push(\'a href\')>Link</a>"] --> b["Drawn"]',
  },
  {
    id: "a use and an iframe in a label",
    source:
      'flowchart-elk TD\n  a["<use href=https://hostile.example/use.svg#u/><iframe src=https://hostile.example/frame.html></iframe>"] --> b["Drawn"]',
  },
  {
    id: "an html data block holding an img",
    source:
      "eventmodeling\n  tf 1 cmd Dispatch `html` {<img src=https://hostile.example/data.png onerror=window.__hostile.push('data onerror')>}",
  },
  {
    id: "an init directive raising securityLevel",
    source:
      '%%{init: {"securityLevel": "loose"}}%%\nflowchart-elk TD\n  a["<img src=https://hostile.example/loose.png onerror=window.__hostile.push(\'loose onerror\')>"] --> b["Drawn"]',
  },
];

// SLOW(>=2s): Mermaid's chunks load for this row and then draw seven hostile sources, each
// through the real renderer; the download and the draws are the whole cost.
test("hostile Mermaid sources draw nothing that runs, fetches or links out @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Hostile sources:", ...HOSTILE_MERMAID_SOURCES.map((entry) => entry.source)),
    title: "Hostile Mermaid sources",
  });

  const alice = await asUser(browser, "alice");
  try {
    const requested = await recordHostileRequests(alice);
    const page = await alice.newPage();
    await page.addInitScript(() => {
      window.__hostile = [];
    });
    await openSpec(page, issue.key, "Hostile sources:");
    const previews = documentEditor(page).locator(".proof-mermaid-preview");
    await expect(previews).toHaveCount(HOSTILE_MERMAID_SOURCES.length);
    for (const [index, entry] of HOSTILE_MERMAID_SOURCES.entries()) {
      const preview = previews.nth(index);
      await expect(diagramIn(preview), entry.id).toBeVisible();
      // Each source took the real Mermaid path: a refusal, or beautiful-mermaid's own drawing,
      // would prove nothing about the markup Mermaid writes.
      expect(await rendererOf(preview), entry.id).toBe("mermaid");
      // The shadow-root walk is the style pass each hostile carrier would fetch from.
      expect(await hostileLeftovers(preview), entry.id).toEqual([]);
    }
    await sleep(REQUEST_MARGIN_MS);
    expect(await page.evaluate(() => window.__hostile)).toEqual([]);
    expect(requested).toEqual([]);
  } finally {
    await alice.close();
  }
});

/** An eventmodeling frame carrying inline data: the one label a renderer writes as HTML rather
 *  than as SVG text, which the browser lays out only inside a `foreignObject` that is kept and
 *  whose content is in the XHTML namespace. */
const EVENTMODELING_DATA_SOURCE = 'eventmodeling\n  tf 1 cmd Dispatch {"id":7}\n';

// SLOW(>=2s): Mermaid's chunks load for this row before its one frame is drawn.
test("an eventmodeling frame's inline data is laid out as HTML in the drawing @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Inline data:", EVENTMODELING_DATA_SOURCE),
    title: "Eventmodeling inline data",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Inline data:");
    const editor = documentEditor(page);
    const diagram = diagramIn(editor);
    await expect(diagram).toBeVisible();
    await expect(editor.locator(".proof-mermaid-status")).toBeHidden();

    const data = await editor.locator(".proof-mermaid-preview").evaluate((preview) => {
      const block = preview.shadowRoot?.querySelector("foreignObject code") ?? null;
      return block === null ? null : { namespace: block.namespaceURI, text: block.textContent };
    });
    expect(data, "the drawing holds the frame's data block").not.toBeNull();
    expect(data?.text).toContain('"id"');
    // A label the browser lays out is XHTML; one the sanitizer left in the SVG namespace would
    // carry the same text and be drawn nowhere, which is what the visibility below reads.
    expect(data?.namespace).toBe("http://www.w3.org/1999/xhtml");
    await expect(editor.locator(".proof-mermaid-preview foreignObject code")).toBeVisible();
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------------------------
// R7: the two colour schemes a reader reads a diagram in. A Mermaid drawing's canvas is
// transparent, so every mark sits on the reader's own page, and most of its colours are literals
// a source cannot change (`mermaid-upstream.ts`): the pair that has to read is a label against
// the mark under it, and a line against the page behind it. The row reads both, in both schemes,
// through the ratio the palette's own unit rows use (`web/src/theme/contrast.ts`).

/** The two page backgrounds a diagram is drawn on, as 8-bit sRGB, from the swatches the page
 *  writes them from (`web/src/styles.css`: `slate-950` dark, `slate-50` light) rather than from
 *  hex of their own, so the row and the page cannot drift apart. They are the two
 *  `mermaid-upstream.ts` measures its literals against. */
const PAGE_BACKGROUND: Record<"dark" | "light", Rgb8> = {
  dark: oklchToSrgb8(SLATE_950.oklch),
  light: oklchToSrgb8(SLATE_50.oklch),
};

/** WCAG's two thresholds: text against what it sits on, and a line against the page. */
const LABEL_ON_MARK_CONTRAST = 4.5;
const LINE_ON_PAGE_CONTRAST = 3;

/** One diagram of the colour row: the source, and the three elements read of its drawing - a
 *  label, the shape that label sits on, and a line the page shows through. */
interface ColorDiagram {
  id: string;
  /** The line or path drawn with nothing but the page behind it. */
  line: string;
  /** The mark the label sits on. */
  shape: string;
  source: string;
  /** The `text` element of a label on that mark. */
  text: string;
}

const COLOR_DIAGRAMS: readonly ColorDiagram[] = [
  {
    id: "timeline",
    line: "g.lineWrapper line",
    shape: "g.eventWrapper path.node-bkg",
    source: timelineSource,
    text: "g.eventWrapper text",
  },
  {
    id: "mindmap",
    line: "path.edge",
    shape: "g.section-root circle",
    source: "mindmap\n  root((Dispatch))\n    Drawn",
    text: "g.section-root text",
  },
  {
    id: "pie",
    line: "circle.pieOuterCircle",
    shape: "path.pieCircle",
    source: sourceOf("pie"),
    text: "text.slice",
  },
];

/** The computed fill of `diagram`'s label and of the mark under it, and the computed stroke of
 *  the line it draws over the page, read through `preview`'s shadow root. A value is `null`
 *  where the drawing holds no such element, so a failure names which of the three went missing
 *  rather than reading a colour off nothing. */
function readDiagramPaints(
  preview: Locator,
  diagram: ColorDiagram
): Promise<{ line: string | null; shape: string | null; text: string | null }> {
  return preview.evaluate((element, picks) => {
    const root = element.shadowRoot;
    const painted = (selector: string, property: "fill" | "stroke") => {
      const node = root?.querySelector(selector) ?? null;
      return node === null ? null : getComputedStyle(node).getPropertyValue(property);
    };
    return {
      line: painted(picks.line, "stroke"),
      shape: painted(picks.shape, "fill"),
      text: painted(picks.text, "fill"),
    };
  }, diagram);
}

/** A computed colour as 8-bit sRGB; `null` for anything but an `rgb()`/`rgba()` value, which is
 *  what every engine answers a computed `fill` or `stroke` with. */
function rgb8(computed: string | null): Rgb8 | null {
  const read = /^rgba?\(\s*(\d+)[\s,]+(\d+)[\s,]+(\d+)/.exec(computed ?? "");
  return read === null ? null : [Number(read[1]), Number(read[2]), Number(read[3])];
}

// SLOW(>=2s): six real draws, three in each scheme's own context, with Mermaid's chunks loaded
// once per context. There is no wait to shorten.
test("a timeline, a mindmap and a pie read on the page in both colour schemes", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the palette is CSS, read the same everywhere");
  test.setTimeout(180_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: diagramBody("Colour schemes:", ...COLOR_DIAGRAMS.map((diagram) => diagram.source)),
    title: "Colour schemes",
  });

  const failures: string[] = [];
  const ratios: string[] = [];
  for (const scheme of ["dark", "light"] as const) {
    const alice = await asUser(browser, "alice", { colorScheme: scheme });
    try {
      const page = await alice.newPage();
      await openSpec(page, issue.key, "Colour schemes:");
      const previews = documentEditor(page).locator(".proof-mermaid-preview");
      await expect(previews).toHaveCount(COLOR_DIAGRAMS.length);
      for (const [index, diagram] of COLOR_DIAGRAMS.entries()) {
        const preview = previews.nth(index);
        await expect(diagramIn(preview), `${diagram.id}, ${scheme}`).toBeVisible();
        const paints = await readDiagramPaints(preview, diagram);
        const label = rgb8(paints.text);
        const mark = rgb8(paints.shape);
        const line = rgb8(paints.line);
        const note = (why: string) => failures.push(`${diagram.id}, ${scheme}: ${why}`);
        if (label === null) note(`no label at ${diagram.text} (${paints.text})`);
        if (mark === null) note(`no mark at ${diagram.shape} (${paints.shape})`);
        if (line === null) note(`no line at ${diagram.line} (${paints.line})`);
        if (label === null || mark === null || line === null) continue;
        const onMark = contrastRatioSrgb8(label, mark);
        const onPage = contrastRatioSrgb8(line, PAGE_BACKGROUND[scheme]);
        ratios.push(
          `${diagram.id}, ${scheme}: label ${paints.text} on mark ${paints.shape} ` +
            `${onMark.toFixed(2)}:1, line ${paints.line} on the page ${onPage.toFixed(2)}:1`
        );
        if (onMark < LABEL_ON_MARK_CONTRAST) {
          note(`its label reads ${onMark.toFixed(2)}:1 on the mark under it`);
        }
        if (onPage < LINE_ON_PAGE_CONTRAST) {
          note(`its line reads ${onPage.toFixed(2)}:1 against the page`);
        }
      }
    } finally {
      await alice.close();
    }
  }

  const measured = ratios.join("\n");
  await testInfo.attach("mermaid contrast in both colour schemes", {
    body: measured,
    contentType: "text/plain",
  });
  console.log(measured);
  expect(failures, "every label and every line reads in both colour schemes").toEqual([]);
  expect(ratios).toHaveLength(COLOR_DIAGRAMS.length * 2);
});
