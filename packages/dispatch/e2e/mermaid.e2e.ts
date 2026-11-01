// A fenced `mermaid` block in a body Dispatch renders in full - an issue comment, an ask's
// question on the Inbox, an issue message, a reply under its comment - draws as the diagram the
// document editor draws (an image named "Rendered Mermaid diagram"), with its source closed behind
// a `Mermaid source` disclosure. A source the renderer refuses shows the reason and the source; a
// diagram the worker cannot draw within the time limit is refused the same way while the page
// keeps responding; a label holding markup draws as text, never as an element; a block in another
// language stays code. Every drawing row checks both halves: the image with its labels, and the
// source hidden until the disclosure opens. At the commit before the change each body shows its
// source as code, so every drawing row and the renderer-refusal row fail on their diagram or
// status assertion, and the time-limit row fails at its hold assertion (no worker chunk loads).
//
// A mermaid block in a document (the issue's spec, open in the editor, or a past version) draws
// too, and the document rows check what it must not do: its own sheet styles no other `svg` or
// `text` on the page, no reader's browser fetches Google Fonts or any address hostile markup
// names, and the diagram is drawn by Dispatch's Mermaid worker, never on the page's thread. So an
// edit redraws once typing pauses, a diagram the worker does not draw in time is refused with its
// source shown while the page keeps responding, a block link, a comment link or a highlighted
// mark below a diagram still lands on its block once the diagram draws (but never pulls back a
// reader who scrolled on), and with no worker, or a worker that fails after drawing, the block
// shows its source. The editor's source disclosure stays open, so these rows never use the
// closed-source check. Before the change the editor draws with upstream's view, synchronously on
// the page's thread, its sheet and Google Fonts `@import` in the page, so the style,
// hostile-document, worker, time-limit, focus and no-worker rows fail.
import { setTimeout as sleep } from "node:timers/promises";

import {
  type BrowserContext,
  expect,
  type Locator,
  type Page,
  type TestInfo,
  test,
} from "@playwright/test";

import {
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  getArtifactText,
} from "./api";
import { blockId, documentEditor, markSpan, openSpec, placeCaret } from "./editor";
import {
  expectHeldAndResponsive,
  failMermaidWorkersOnCall,
  holdMermaidWorkers,
  holdWorkerSource,
  recordMermaidWorkers,
  refuseWorkers,
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

const session = {
  actor: { id: "e2e-mermaid-agent", kind: "session" as const },
  as: "agent" as const,
};

const diagramName = "Rendered Mermaid diagram";

/** A body of `lead` and then one flowchart from `from` to `to`, whose source's first line is
 *  `graph TD`. */
function flowchart(lead: string, from: string, to: string): string {
  return `${lead}\n\n\`\`\`mermaid\ngraph TD\n  ${from} --> ${to}\n\`\`\`\n`;
}

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

/** The comment thread `rootId` opened in the Conversation, where its bodies render in full rather
 *  than as the collapsed card's preview: the turn itself on a desktop, the full-screen Thread
 *  view on a phone. The turn shows `lead`, the root comment's first text, before it is expanded,
 *  so the click lands on a turn that has rendered. */
async function openCommentThread(
  page: Page,
  testInfo: TestInfo,
  rootId: string,
  lead: string
): Promise<Locator> {
  const turn = conversationTurns(page).locator(`:scope > li[data-turn="comment:${rootId}"]`);
  await expect(turn).toContainText(lead);
  await turn.getByRole("button", { exact: true, name: "Expand thread" }).click();
  if (testInfo.project.name === "iphone") {
    const thread = page.getByRole("dialog", { name: "Thread" });
    await expect(thread).toBeVisible();
    return thread.locator(`li[data-turn="comment:${rootId}"]`);
  }
  await expect(turn.getByRole("button", { exact: true, name: "Collapse thread" })).toBeVisible();
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
  await card.getByText("Mermaid source", { exact: true }).click();
  await expect(source).toBeVisible();
}

/** Starts recording `page`'s console errors and returns the check both rows whose renderer never
 *  draws end with: the document editor shows `source` in a `pre code`, holds no diagram, hides
 *  the status line, and the renderer said once that it is unavailable. Recording starts at this
 *  call, before the page navigates, so an error logged on the first paint is counted. Both rows
 *  end alike because a block whose worker cannot draw looks the same whether no worker ever
 *  loaded or one failed after a drawing: its source as code, with no stale diagram left above
 *  it. */
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

// SLOW(>=2s): a cold load of the Conversation and one real Mermaid draw, which is 2.2 s in WebKit
// alone; the row is the shared per-row cost plus its own single step.
test("a comment holding a mermaid block draws a diagram, its source behind Mermaid source @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Comment diagram" });
  const comment = await createComment(issue.key, { body: flowchart("Flow:", "Start", "Finish") });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await openCommentThread(page, testInfo, comment.id, "Flow:");
    await expect(card).toContainText("Flow:");
    await expectDrawnDiagram(card, ["Start", "Finish"]);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): a cold load of the Inbox and one real Mermaid draw, which is 2.2 s in WebKit alone;
// the row is the shared per-row cost plus its own single step.
test("an ask's question holding a mermaid block draws a diagram on the Inbox card @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ask diagram" });
  const ask = await createAsk(
    issue.key,
    { question: flowchart("Does this order hold?", "Asked", "Answered") },
    session
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const card = page.getByTestId(`ask-${ask.id}`);
    await expect(card).toContainText("Does this order hold?");
    await expectDrawnDiagram(card, ["Asked", "Answered"]);
  } finally {
    await alice.close();
  }
});

test("an issue message holding a mermaid block draws a diagram in the Conversation @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Message diagram" });
  const message = await createMessage(
    issue.key,
    { body: flowchart("The pipeline:", "Sent", "Delivered") },
    session
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = conversationTurns(page).locator(`:scope > li[data-turn="message:${message.id}"]`);
    await expect(card).toContainText("The pipeline:");
    await expectDrawnDiagram(card, ["Sent", "Delivered"]);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): a cold load, a thread expanded and one real Mermaid draw, which is 2.1 s in WebKit
// alone; the row is the shared per-row cost plus its own single step.
test("a reply to a comment holding a mermaid block draws a diagram under its comment @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Reply diagram" });
  const root = await createComment(issue.key, { body: "How does a reply travel?" });
  await createComment(
    issue.key,
    { body: flowchart("Like this:", "Replied", "Read"), reply_to: root.id },
    session
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const thread = await openCommentThread(page, testInfo, root.id, "How does a reply travel?");
    const reply = thread.getByRole("list", { name: "Replies" }).locator("li[data-reply]");
    await expect(reply).toHaveCount(1);
    await expect(reply).toContainText("Like this:");
    await expectDrawnDiagram(reply, ["Replied", "Read"]);
  } finally {
    await alice.close();
  }
});

test("a mermaid source the renderer refuses shows the reason and its source, and no diagram @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Refused diagram" });
  const comment = await createComment(issue.key, {
    body: "Broken:\n\n```mermaid\ngraph TD\n  A -->\n```\n",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await openCommentThread(page, testInfo, comment.id, "Broken:");
    await expect(card).toContainText("Broken:");
    const refusal = card
      .getByRole("status")
      .filter({ hasText: "Unable to render Mermaid diagram" });
    await expect(refusal).toHaveText(/^Unable to render Mermaid diagram: Dangling Mermaid edge/);
    // The disclosure opens on a refusal, so the source is readable without a click. The status
    // quotes the dangling line too, so the source is read from its code block.
    await expect(card.locator("pre code").filter({ hasText: "A -->" })).toBeVisible();
    await expect(diagramIn(card)).toHaveCount(0);
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
test("a diagram too large to draw in time shows the reason, and the page keeps responding @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Large diagram" });
  const comment = await createComment(issue.key, { body: flowchart("Large:", "Start", "Finish") });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const holds = await holdWorkerSource(page);
    await page.clock.install();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await openCommentThread(page, testInfo, comment.id, "Large:");
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

// SLOW(>=2s): a cold load, a thread expanded and one real Mermaid draw, which is 2.0 s in WebKit
// alone; the row is the shared per-row cost plus its own single step.
test("a mermaid label holding markup draws as text, never as an element @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Markup label" });
  const comment = await createComment(issue.key, {
    body: 'Label:\n\n```mermaid\ngraph TD\n  A["<img src=x onerror=window.__mermaidHit=1>"] --> B\n```\n',
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await openCommentThread(page, testInfo, comment.id, "Label:");
    await expect(card).toContainText("Label:");
    // The label's characters are the node's text. Only its head is asserted, since a long label
    // may wrap across text lines whose content joins without the space between them.
    await expectDrawnDiagram(card, ["img src=x"]);
    await expect(diagramIn(card).locator("img")).toHaveCount(0);
    expect(await page.evaluate(() => "__mermaidHit" in window)).toBe(false);
  } finally {
    await alice.close();
  }
});

test("a code block in another language stays code @phone", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "TypeScript block" });
  const comment = await createComment(issue.key, {
    body: "Code:\n\n```ts\nconst answer = 42;\n```\n",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await openCommentThread(page, testInfo, comment.id, "Code:");
    await expect(card.getByText("const answer = 42;")).toBeVisible();
    await expect(diagramIn(card)).toHaveCount(0);
    await expect(card.getByText("Mermaid source", { exact: true })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// beautiful-mermaid's sheet sets `text { font-family: 'Inter', system-ui, sans-serif }` and
// `svg { --_text: var(--fg) … }` after an `@import` of Google Fonts. In the page it restyles
// every `svg` and `text` there and fetches the font; in the diagram's own shadow root it styles
// that diagram alone.
test("a document's mermaid diagram draws, styles nothing outside it, and fetches no Google font @phone", async ({
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

test("hostile diagram markup in a comment runs nothing and fetches nothing @phone", async ({
  browser,
}, testInfo) => {
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
    const card = await openCommentThread(page, testInfo, comment.id, "Hostile:");
    await expectNothingHostile(page, card, card.locator(".dispatch-mermaid-preview"), requested);
  } finally {
    await alice.close();
  }
});

test("hostile diagram markup in a document runs nothing and fetches nothing @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: `Hostile document:\n\n\`\`\`mermaid\n${hostileSource}\n\`\`\`\n`,
    title: "Hostile document diagram",
  });

  const alice = await asUser(browser, "alice");
  try {
    const requested = await recordHostileRequests(alice);
    const page = await alice.newPage();
    await feedHostileSvg(page);
    await openSpec(page, issue.key, "Hostile document:");
    const editor = documentEditor(page);
    await expectNothingHostile(page, editor, editor.locator(".proof-mermaid-preview"), requested);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): two real worker draws - the open and the redraw after the edit - and the document
// read that shows the edit was the only change the document took. There is no wait to shorten.
test("a document's mermaid diagram redraws as it is edited, and a click on it changes nothing @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: flowchart("Edited diagram:", "Start", "Finish"),
    title: "Edited document diagram",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const errors: Error[] = [];
    page.on("pageerror", (error) => errors.push(error));
    await openSpec(page, issue.key, "Edited diagram:");
    const diagram = diagramIn(documentEditor(page));
    await expect(diagram).toContainText("Finish");
    const before = (await getArtifactText(issue.primary_artifact_id)).markdown;

    await diagram.click();
    expect((await getArtifactText(issue.primary_artifact_id)).markdown).toBe(before);

    // "Start --> Finish" is only in the source: the diagram's labels are separate texts.
    await placeCaret(page, "after", "Start --> Finish");
    await page.keyboard.type("Line");
    await expect(diagram).toContainText("FinishLine");
    // The edit is the only change the document took, the click's included.
    await expect
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
      .toBe(before.replace("Start --> Finish", "Start --> FinishLine"));
    expect(errors).toEqual([]);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): one real draw, the typing, and the redraw the page's own debounce runs after it;
// in WebKit that is 2.3 s, and the row waits out no part of the debounce itself.
test("an edited document diagram redraws once typing pauses, and its last drawing stays until then @phone", async ({
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
    await recordMermaidWorkers(page);
    await openSpec(page, issue.key, "Typed diagram:");
    const diagram = diagramIn(documentEditor(page));
    await expect(diagram).toContainText("Finish");
    await page.evaluate(() => window.__mermaidSent?.splice(0));

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
  } finally {
    await alice.close();
  }
});

// The document twin of the comment row above, on the same hold (`holdWorkerSource`) and the same
// clock: the editor hands the source to the worker chunk, the page keeps answering while the
// clock runs at real speed, and the shipped limit, fired by the fast-forward, refuses it with the
// reason above its source. An editor drawing on the page's thread fails at the hold.
test("a document diagram too large to draw in time shows the reason and its source, and the page keeps responding @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: flowchart("Large document:", "Start", "Finish"),
    title: "Large document diagram",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const holds = await holdWorkerSource(page);
    await page.clock.install();
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(documentEditor(page)).toContainText("Large document:");
    const status = documentEditor(page).locator(".proof-mermaid-status");
    await expectHeldAndResponsive(page, holds);
    // The samples ran while the worker still held the source, before the time limit refused it.
    await expect(status).toBeHidden();
    await page.clock.fastForward(5_000);
    await expect(status).toHaveText(
      "Unable to render Mermaid diagram: it did not finish within 5 seconds"
    );
    await expect(status).toBeVisible();
    await expect(
      documentEditor(page).locator("pre code").filter({ hasText: "Start --> Finish" })
    ).toBeVisible();
    await expect(diagramIn(documentEditor(page))).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): one real draw of the 21-node chain, which must be taller than the viewport for the
// link's landing to be worth proving; the worker draws it for real and no timer is waited on.
test("a block link below a diagram lands on its block once the diagram draws @phone", async ({
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
test("a reader who scrolls after a block link lands is not pulled back once the diagram draws @phone", async ({
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

// SLOW(>=2s): one real draw of the 21-node chain, which must be taller than the viewport for the
// mark's landing to be worth proving; the worker draws it for real and no timer is waited on.
test("a past version's highlighted mark lands on its block once a diagram above it draws @phone", async ({
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

// SLOW(>=2s): one real draw of the 21-node chain, which must be taller than the viewport for the
// link's landing to be worth proving; the worker draws it for real and no timer is waited on.
test("a comment link below a diagram lands on its quote once the diagram draws @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: tallDocument("Commented document:", "Target paragraph holds the quoted words."),
    title: "Comment link below a diagram",
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
    await page.goto(`/issues/${issue.key}/spec?comment=${encodeURIComponent(comment.id)}`);
    const quote = markSpan(page, comment.id);
    const diagram = diagramIn(documentEditor(page));
    await expect(quote).toHaveText("quoted words");
    await expect(quote).toBeInViewport();
    await expect(diagram).toHaveCount(0);
    await withoutScrollAnchoring(page);
    await page.evaluate(() => window.__releaseMermaid?.());
    await expect(diagram).toBeVisible();
    await expect(quote).toBeInViewport();
  } finally {
    await alice.close();
  }
});

test("with no worker a document's diagram shows its source @phone", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: flowchart("Workerless diagram:", "Start", "Finish"),
    title: "Workerless document diagram",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const expectSourceAsCode = watchSourceAsCode(page);
    await refuseWorkers(page);
    await openSpec(page, issue.key, "Workerless diagram:");
    await expectSourceAsCode("Start --> Finish");
  } finally {
    await alice.close();
  }
});

// A worker that drew once can fail later, as when a redeploy removes its chunk: the block then
// shows its edited source as code, never the last drawing above a source it no longer matches.
// SLOW(>=2s): one real Mermaid draw, then the edit that asks the failing worker for the next
// one; in WebKit that is 2.2 s, and neither draw waits on a timer.
test("a document diagram whose worker fails after its first drawing shows its source @phone", async ({
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
