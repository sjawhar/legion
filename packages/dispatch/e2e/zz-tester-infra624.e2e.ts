import { type Browser, type BrowserContext, expect, type Locator, type Page, test } from "@playwright/test";

import { spawn } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { baseUrl, createIssue, createProject, e2eAgentToken, getArtifactText, getIssue } from "./api";
import {
  connectedDot,
  documentEditor,
  documentTransport,
  indexedBlockAsk,
  placeCaret,
  selectTextIn,
} from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// Tester's throwaway probes for INFRA-624 round 2: what a person can do in a broken decision.

const SPEC = 'Intro.\n\n:::ask{#a1 urgency="med" multiple="false"}\nWhich one?\n:::\n\nAfter.\n';

test.setTimeout(300_000);

test.beforeEach(async () => {
  await resetDatabase();
});

function askBlock(page: Page): Locator {
  return documentEditor(page).locator('[data-dispatch-ask-block="a1"]');
}
function askContent(page: Page): Locator {
  return askBlock(page).locator(":scope > [data-proof-block-content]");
}

type DocumentTransport = Awaited<ReturnType<typeof documentTransport>>; // throwaway
type Break = "two-lists" | "para-after";

async function openPair(
  issueKey: string,
  contexts: BrowserContext[],
  errors: string[]
): Promise<{ page: Page; transport: DocumentTransport }[]> {
  const opened = [];
  for (const context of contexts) {
    const page = await context.newPage();
    page.on("pageerror", (error) => errors.push(error.message));
    const transport = await documentTransport(page);
    await page.goto(`/issues/${issueKey}`);
    opened.push({ page, transport });
  }
  for (const { page } of opened) {
    await expect(connectedDot(page)).toHaveText("connected");
    await expect(askBlock(page)).toContainText("Which one?");
  }
  return opened;
}

/** Breaks decision a1 in alice's and bob's pages. For para-after, retries fresh issues until the
 * merge orders bob's paragraph after alice's list. */
async function broken(
  kind: Break,
  alice: BrowserContext,
  bob: BrowserContext,
  errors: string[],
  asAgent = false,
  texts: { a: string; b: string } = { a: "Option A", b: "Option B" }
): Promise<{ pages: Page[]; artifactId: string; issueKey: string }> {
  await createProject({ key: "CORE", name: "Core" });
  for (let attempt = 0; attempt < 8; attempt++) {
    const issue = await createIssue(
      { project: "CORE", spec: SPEC, title: `Broken ${attempt}` },
      asAgent ? { as: "agent", actor: { kind: "session", id: "e2e-tester-cli" } } : {}
    );
    await indexedBlockAsk(issue.key, "a1");
    const opened = await openPair(issue.key, [alice, bob], errors);
    for (const { transport } of opened) {
      transport.hold();
      await transport.sever();
    }
    const [a, b] = opened.map((o) => o.page);
    await placeCaret(a, "after", "Which one?", askContent(a));
    await a.keyboard.press("Enter");
    const [aOwn, aNested] = texts.a.split("\n>");
    await a.keyboard.type(aOwn);
    await a.keyboard.press("Home");
    await a.keyboard.type("- ");
    if (aNested) {
      await placeCaret(a, "after", aOwn, askContent(a));
      await a.keyboard.press("Enter");
      await a.keyboard.press("Tab");
      await a.keyboard.type(aNested);
    }
    await placeCaret(b, "after", "Which one?", askContent(b));
    await b.keyboard.press("Enter");
    if (kind === "two-lists") {
      const [own, nested] = texts.b.split("\n>");
      await b.keyboard.type(own);
      await placeCaret(b, "before", own.slice(0, 12), askContent(b));
      await b.keyboard.type("- ");
      if (nested) {
        await placeCaret(b, "after", own, askContent(b));
        await b.keyboard.press("Enter");
        await b.keyboard.press("Tab");
        await b.keyboard.type(nested);
        console.log(`NESTED bob dom ${JSON.stringify(await askContent(b).innerHTML())}`.slice(0, 600));
      }
    } else {
      await b.keyboard.type("Context");
    }
    await Promise.all(opened.map(({ transport }) => transport.release()));
    const want = ["Which one?", ...texts.a.split("\n>"), ...(kind === "two-lists" ? texts.b.split("\n>") : ["Context"])];
    await expect
      .poll(async () => {
        const md = (await getArtifactText(issue.primary_artifact_id)).markdown;
        return want.every((w) => md.includes(w));
      })
      .toBe(true)
      .catch(async (e) => {
        console.log(`WANT-FAIL md=${JSON.stringify((await getArtifactText(issue.primary_artifact_id)).markdown)} dom=${JSON.stringify(await askContent(a).innerText())}`);
        throw e;
      });
    await a.waitForTimeout(1500);
    const md = (await getArtifactText(issue.primary_artifact_id)).markdown;
    const paraAfter = /- Option A\n\nContext/u.test(md);
    if (kind === "two-lists" || paraAfter) {
      for (const page of [a, b]) {
        await expect(askBlock(page)).toHaveAttribute("data-dispatch-ask-malformed", "true");
      }
      console.log(`${kind} broken (attempt ${attempt}):`, JSON.stringify(md));
      return { pages: [a, b], artifactId: issue.primary_artifact_id, issueKey: issue.key };
    }
    for (const page of [a, b]) await page.close();
  }
  throw new Error("could not produce the paragraph-after-list order in 8 attempts");
}

async function report(
  label: string,
  pages: Page[],
  artifactId: string,
  issueKey: string,
  errors: string[]
): Promise<void> {
  await pages[0].waitForTimeout(3000);
  const md = (await getArtifactText(artifactId)).markdown;
  const malformed = await Promise.all(
    pages.map((p) => askBlock(p).getAttribute("data-dispatch-ask-malformed"))
  );
  const contents = await Promise.all(pages.map((p) => askContent(p).innerText()));
  const ask = (await getIssue(issueKey)).open_asks.find((x) => x.block_id === "a1");
  console.log(
    `RESULT ${label}: ${JSON.stringify({
      md,
      malformed,
      contentsEqual: contents[0] === contents[1],
      alice: contents[0],
      ask: ask ? { q: ask.question, o: ask.options.map((o) => o.label) } : null,
      errors,
    })}`
  );
}

async function scenario(
  browser: Browser,
  kind: Break,
  label: string,
  act: (a: Page) => Promise<void>
): Promise<void> {
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  const errors: string[] = [];
  try {
    const { pages, artifactId, issueKey } = await broken(kind, alice, bob, errors);
    await act(pages[0]);
    await report(label, pages, artifactId, issueKey, errors);
  } finally {
    await bob.close();
    await alice.close();
  }
}

function firstListText(a: Page): Promise<string> {
  return askContent(a).locator(":scope > ul").first().innerText();
}


const dispatchEntry = fileURLToPath(new URL("../../envoy-client/bin/dispatch.ts", import.meta.url));
async function cli(args: string[]): Promise<{ code: number; out: string }> {
  const home = mkdtempSync(join(process.env.TMPDIR ?? tmpdir(), "zzcli-"));
  const child = spawn("bun", [dispatchEntry, ...args], {
    cwd: home,
    env: {
      PATH: process.env.PATH ?? "",
      HOME: home,
      DISPATCH_STATE_DIR: home,
      DISPATCH_HOST: "omp",
      DISPATCH_SESSION_ID: "e2e-tester-cli",
      DISPATCH_URL: baseUrl,
      DISPATCH_TOKEN: e2eAgentToken,
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let out = "";
  child.stdout.on("data", (c) => (out += c));
  child.stderr.on("data", (c) => (out += c));
  const code = await new Promise<number>((r) => child.once("close", (c) => r(c ?? -1)));
  return { code, out };
}

async function agentState(label: string, pages: Page[], artifactId: string, issueKey: string) {
  await pages[0].waitForTimeout(2500);
  const md = (await getArtifactText(artifactId)).markdown;
  const malformed = await Promise.all(pages.map((p) => askBlock(p).getAttribute("data-dispatch-ask-malformed")));
  const row = (await getIssue(issueKey)).open_asks.find((x) => x.block_id === "a1");
  console.log(`STATE ${label}: ${JSON.stringify({ md, malformed, row: row ? { q: row.question, o: row.options.map((o) => o.label) } : null })}`);
}

/** Two-list break whose first list holds two items (Option C added), then a selection from the
 * question into the first list's first item. */
async function crossSelection(a: Page): Promise<{ first: string; second: string }> {
  const first = (await firstListText(a)).includes("Option A") ? "Option A" : "Option B";
  const second = first === "Option A" ? "Option B" : "Option A";
  await placeCaret(a, "after", first, askContent(a).locator(":scope > ul").first());
  await a.keyboard.press("Enter");
  await a.keyboard.type("Option C");
  await expect(askContent(a).locator(":scope > ul").first()).toContainText("Option C");
  return { first, second };
}

async function domMatches(a: Page, label: string, artifactId: string) {
  await a.waitForTimeout(1500);
  const dom = await askContent(a).innerText();
  const md = (await getArtifactText(artifactId)).markdown;
  console.log(`DOM ${label}: ${JSON.stringify(dom)} | md=${JSON.stringify(md)}`);
}

const probes: Record<string, (a: Page, artifactId: string) => Promise<void>> = {
  "X1 execCommand insertText q-into-first, then DOM vs state, then type": async (a, id) => {
    await selectTextIn(a, askContent(a), "one?Opt");
    await a.evaluate(() => document.execCommand("insertText", false, "x"));
    await domMatches(a, "X1 after execCommand", id);
    await placeCaret(a, "after", "Which", askContent(a));
    await a.keyboard.type("Q");
    await domMatches(a, "X1 after typing", id);
  },
  "X2 execCommand insertText replacing one word in the question": async (a, id) => {
    await selectTextIn(a, askContent(a), "one");
    await a.evaluate(() => document.execCommand("insertText", false, "two"));
    await domMatches(a, "X2", id);
  },
  "X3 execCommand insertText replacing a word in the second list": async (a, id) => {
    const second = (await firstListText(a)).includes("Option A") ? "Option B" : "Option A";
    await selectTextIn(a, askContent(a).locator(":scope > ul").nth(1), second);
    await a.evaluate(() => document.execCommand("insertText", false, "Option Q"));
    await domMatches(a, "X3", id);
  },
  "X4 execCommand delete from Option C into the second list": async (a, id) => {
    await selectTextIn(a, askContent(a).locator(":scope > ul").first(), "Option C");
    await a.keyboard.press("Shift+ArrowDown");
    console.log(`X4 sel ${JSON.stringify(await a.evaluate(() => window.getSelection()?.toString()))}`);
    await a.evaluate(() => document.execCommand("delete"));
    await domMatches(a, "X4", id);
  },
  "X5 execCommand delete from the question through the second list": async (a, id) => {
    await selectTextIn(a, askContent(a), "one?");
    await a.keyboard.press("Shift+ArrowDown");
    await a.keyboard.press("Shift+ArrowDown");
    await a.keyboard.press("Shift+ArrowDown");
    await a.keyboard.press("Shift+End");
    console.log(`X5 sel ${JSON.stringify(await a.evaluate(() => window.getSelection()?.toString()))}`);
    await a.evaluate(() => document.execCommand("delete"));
    await domMatches(a, "X5", id);
  },
  "X6 execCommand insertText q-into-first, then Ctrl+Z": async (a, id) => {
    await selectTextIn(a, askContent(a), "one?Opt");
    await a.evaluate(() => document.execCommand("insertText", false, "x"));
    await a.keyboard.press("ControlOrMeta+Z");
    await domMatches(a, "X6", id);
  },
};

for (const [label, act] of Object.entries(probes)) {
  test(label, async ({ browser }) => {
    const alice = await asUser(browser, "alice");
    const bob = await asUser(browser, "bob");
    const errors: string[] = [];
    try {
      const { pages, artifactId, issueKey } = await broken("two-lists", alice, bob, errors);
      await crossSelection(pages[0]);
      await act(pages[0], artifactId);
      await report(label, pages, artifactId, issueKey, errors);
      console.log(`BOB ${label}: ${JSON.stringify(await askContent(pages[1]).innerText())}`);
    } finally {
      await bob.close();
      await alice.close();
    }
  });
}

test("X7 paragraph-after-list: execCommand delete from the option into the paragraph", async ({ browser }) => {
  await scenario(browser, "para-after", "X7", async (a) => {
    await selectTextIn(a, askContent(a), "AContext");
    await a.evaluate(() => document.execCommand("delete"));
  });
});

test("X8 paragraph-after-list: execCommand insertText from the question into the option", async ({ browser }) => {
  await scenario(browser, "para-after", "X8", async (a) => {
    await selectTextIn(a, askContent(a), "one?Opt");
    await a.evaluate(() => document.execCommand("insertText", false, "x"));
  });
});

const caretProbes: Record<string, (a: Page) => Promise<void>> = {
  "K1 IME composition at a caret in the question": async (a) => {
    await placeCaret(a, "after", "Which", askContent(a));
    const cdp = await a.context().newCDPSession(a);
    await cdp.send("Input.imeSetComposition", { selectionEnd: 1, selectionStart: 1, text: "k" });
    await cdp.send("Input.imeSetComposition", { selectionEnd: 2, selectionStart: 2, text: "ka" });
    await cdp.send("Input.insertText", { text: "か" });
  },
  "K2 IME composition at a caret in the second list": async (a) => {
    const second = (await firstListText(a)).includes("Option A") ? "Option B" : "Option A";
    await placeCaret(a, "after", second, askContent(a).locator(":scope > ul").nth(1));
    const cdp = await a.context().newCDPSession(a);
    await cdp.send("Input.imeSetComposition", { selectionEnd: 1, selectionStart: 1, text: "n" });
    await cdp.send("Input.insertText", { text: "ん" });
  },
  "K3 CDP insertText at a caret in the first list": async (a) => {
    await placeCaret(a, "after", "Option C", askContent(a).locator(":scope > ul").first());
    const cdp = await a.context().newCDPSession(a);
    await cdp.send("Input.insertText", { text: "!" });
  },
  "K4 clipboard paste at a caret in the question": async (a) => {
    await a.context().grantPermissions(["clipboard-read", "clipboard-write"]);
    await a.evaluate(() => navigator.clipboard.writeText(" pasted"));
    await placeCaret(a, "after", "Which one?", askContent(a));
    await a.keyboard.press("ControlOrMeta+V");
  },
  "K5 Backspace at the start of the second list": async (a) => {
    const second = (await firstListText(a)).includes("Option A") ? "Option B" : "Option A";
    await placeCaret(a, "before", second, askContent(a).locator(":scope > ul").nth(1));
    await a.keyboard.press("Backspace");
  },
  "K6 Delete at the end of Option C": async (a) => {
    await placeCaret(a, "after", "Option C", askContent(a).locator(":scope > ul").first());
    await a.keyboard.press("Delete");
  },
  "K7 substring case: execCommand insertText over one?Opt": async (a) => {
    const second = (await firstListText(a)).includes("Option A") ? "Option B" : "Option A";
    await placeCaret(a, "after", "Option C", askContent(a).locator(":scope > ul").first());
    await a.keyboard.press("Enter");
    await a.keyboard.type(second);
    await selectTextIn(a, askContent(a), "one?Opt");
    await a.evaluate(() => document.execCommand("insertText", false, "x"));
  },
};

for (const [label, act] of Object.entries(caretProbes)) {
  test(label, async ({ browser }) => {
    await scenario(browser, "two-lists", label, async (a) => {
      await crossSelection(a);
      await act(a);
    });
  });
}

async function agentFlow(label: string, browser: Browser, texts: { a: string; b: string }, steps: (id: string, issueKey: string) => string[][]) {
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  const errors: string[] = [];
  try {
    const { pages, artifactId, issueKey } = await broken("two-lists", alice, bob, errors, true, texts);
    const ask = (await getIssue(issueKey)).open_asks.find((x) => x.block_id === "a1");
    if (!ask) throw new Error("no ask");
    for (const args of steps(ask.id, issueKey)) {
      const r = await cli(args);
      console.log(`CLI ${label} ${JSON.stringify(args.slice(0, 1).concat(args.slice(3)))} exit=${r.code}: ${JSON.stringify(args[0] === "doc-read" ? r.out.trim().slice(0, 1500) : r.out.trim().split("\n")[0].slice(0, 1200))}`);
      await agentState(label, pages, artifactId, issueKey);
    }
    console.log(`ERRORS ${label} ${JSON.stringify(errors)}`);
  } finally {
    await bob.close();
    await alice.close();
  }
}



function blockIds(reason: string): { id: string; type: string; text: string }[] {
  const out = [];
  const re = /block "((?:[^"\\]|\\.)*)" \((\w+)\) reads "((?:[^"\\]|\\.)*)"/gu;
  for (let m = re.exec(reason); m; m = re.exec(reason)) out.push({ id: JSON.parse(`"${m[1]}"`), type: m[2], text: JSON.parse(`"${m[3]}"`) });
  return out;
}

async function threeLists(alice: BrowserContext, bob: BrowserContext, carol: BrowserContext, errors: string[]) {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: SPEC, title: "Three" }, { as: "agent", actor: { kind: "session", id: "e2e-tester-cli" } });
  await indexedBlockAsk(issue.key, "a1");
  const opened = await openPair(issue.key, [alice, bob, carol], errors);
  for (const { transport } of opened) {
    transport.hold();
    await transport.sever();
  }
  const labels = ["Option A", "Option B", "Option C"];
  for (const [i, { page }] of opened.entries()) {
    await placeCaret(page, "after", "Which one?", askContent(page));
    await page.keyboard.press("Enter");
    await page.keyboard.type(labels[i]);
    await page.keyboard.press("Home");
    await page.keyboard.type("- ");
  }
  await Promise.all(opened.map(({ transport }) => transport.release()));
  await expect.poll(async () => {
    const md = (await getArtifactText(issue.primary_artifact_id)).markdown;
    return labels.every((l) => md.includes(l));
  }).toBe(true);
  await opened[0].page.waitForTimeout(2000);
  return { pages: opened.map((o) => o.page), artifactId: issue.primary_artifact_id, issueKey: issue.key };
}

type Plan = (blocks: { id: string; type: string; text: string }[]) => { ops: object[][]; options: string[] };

async function walk(label: string, setup: () => Promise<{ pages: Page[]; artifactId: string; issueKey: string }>, plan: Plan) {
  const { pages, artifactId, issueKey } = await setup();
  const ask = (await getIssue(issueKey)).open_asks.find((x) => x.block_id === "a1");
  if (!ask) throw new Error("no ask");
  const run = async (step: string, args: string[]) => {
    const r = await cli(args);
    console.log(`CLI ${label} ${step} exit=${r.code}: ${JSON.stringify(r.out.trim().split("\n")[0].slice(0, 1600))}`);
    await agentState(`${label} ${step}`, pages, artifactId, issueKey);
    return r;
  };
  const refused = await run("edit-ask urgency", ["edit-ask", "--ask", ask.id, "--urgency", "high"]);
  const blocks = blockIds(refused.out);
  console.log(`BLOCKS ${label}: ${JSON.stringify(blocks)}`);
  const named = /the blocks to delete are ((?:"(?:[^"\\]|\\.)*"(?:, )?)+); then edit the ask/u.exec(refused.out)?.[1];
  const dropIds = named ? [...named.matchAll(/"((?:[^"\\]|\\.)*)"/gu)].map((m) => JSON.parse(`"${m[1]}"`)) : [];
  console.log(`DROP ${label}: ${JSON.stringify(dropIds.map((id) => blocks.find((b) => b.id === id)))}`);
  const { options } = plan(blocks);
  await run("doc-edit (the refusal's deletes, one edit)", ["doc-edit", "--issue", issueKey, "--ops-json", JSON.stringify(dropIds.map((id) => ({ op: "delete", block: id })))]);
  await run("edit-ask final", ["edit-ask", "--ask", ask.id, ...options.flatMap((o) => ["--option", o])]);
}

test("W1 two lists: delete the second list by id, then edit-ask", async ({ browser }) => {
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    await walk("W1", () => broken("two-lists", alice, bob, [], true), (b) => {
      const lists = b.filter((x) => x.type === "bullet_list");
      return { ops: [[{ op: "delete", block: lists[1].id }]], options: lists.map((l) => l.text.replace(/^[-*] /u, "")) };
    });
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("W2 three lists: delete one list at a time, then both at once", async ({ browser }) => {
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  const carol = await asUser(browser, "carol");
  try {
    await walk("W2", () => threeLists(alice, bob, carol, []), (b) => {
      const lists = b.filter((x) => x.type === "bullet_list");
      return {
        ops: [[{ op: "delete", block: lists[2].id }], [{ op: "delete", block: lists[1].id }, { op: "delete", block: lists[2].id }]],
        options: lists.map((l) => l.text.replace(/^[-*] /u, "")),
      };
    });
  } finally {
    await carol.close();
    await bob.close();
    await alice.close();
  }
});

test("W3 repeated label, the duplicate's list holding another option", async ({ browser }) => {
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    await walk("W3", async () => {
      const r = await broken("two-lists", alice, bob, [], true, { a: "Option A: cheap", b: "Option A: fast" });
      // Bob adds a second option to his list (the list holding the duplicate).
      const bobPage = r.pages[1];
      await placeCaret(bobPage, "after", "Option A: fast", askContent(bobPage));
      await bobPage.keyboard.press("Enter");
      await bobPage.keyboard.type("Option C");
      await expect.poll(async () => (await getArtifactText(r.artifactId)).markdown).toContain("Option C");
      return r;
    }, (b) => {
      const lists = b.filter((x) => x.type === "bullet_list");
      const dup = lists.find((l) => l.text.includes("fast")) ?? lists[1];
      return { ops: [[{ op: "delete", block: dup.id }]], options: ["Option A: cheap", "Option A2: fast", "Option C"] };
    });
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("W4 paragraph after the list: delete it by id, then edit-ask", async ({ browser }) => {
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    await walk("W4", () => broken("para-after", alice, bob, [], true), (b) => {
      const para = b.filter((x) => x.type === "paragraph").at(-1);
      return { ops: [[{ op: "delete", block: para?.id }]], options: ["Option A"] };
    });
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("W5 nested sub-item under the collaborator's option", async ({ browser }) => {
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    await walk("W5", () => broken("two-lists", alice, bob, [], true, { a: "Option A", b: "Option B\n>only if finance agrees" }), (b) => {
      const lists = b.filter((x) => x.type === "bullet_list");
      const nested = lists.find((l) => l.text.includes("finance")) ?? lists[1];
      return { ops: [[{ op: "delete", block: nested.id }]], options: ["Option A", "Option B: only if finance agrees"] };
    });
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("E1 alice empties the question and adds a list while bob adds a list", async ({ browser }) => {
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  const errors: string[] = [];
  try {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", spec: SPEC, title: "Empty question" }, { as: "agent", actor: { kind: "session", id: "e2e-tester-cli" } });
    await indexedBlockAsk(issue.key, "a1");
    const opened = await openPair(issue.key, [alice, bob], errors);
    for (const { transport } of opened) {
      transport.hold();
      await transport.sever();
    }
    const [a, b] = opened.map((o) => o.page);
    await placeCaret(b, "after", "Which one?", askContent(b));
    await b.keyboard.press("Enter");
    await b.keyboard.type("Option B");
    await b.keyboard.press("Home");
    await b.keyboard.type("- ");
    await placeCaret(a, "after", "Which one?", askContent(a));
    await a.keyboard.press("Enter");
    await a.keyboard.type("Option A");
    await a.keyboard.press("Home");
    await a.keyboard.type("- ");
    await selectTextIn(a, askContent(a), "Which one?");
    await a.keyboard.press("Backspace");
    console.log(`E1 alice before release ${JSON.stringify(await askContent(a).innerHTML())}`.slice(0, 700));
    await Promise.all(opened.map(({ transport }) => transport.release()));
    await a.waitForTimeout(4000);
    await agentState("E1 merged", [a, b], issue.primary_artifact_id, issue.key);
    const ask = (await getIssue(issue.key)).open_asks.find((x) => x.block_id === "a1");
    if (!ask) { console.log("E1 no open ask"); return; }
    const r = await cli(["edit-ask", "--ask", ask.id, "--question", "Which one now?", "--option", "Option A", "--option", "Option B"]);
    console.log(`CLI E1 edit-ask exit=${r.code}: ${JSON.stringify(r.out.trim().split("\n")[0].slice(0, 1600))}`);
    console.log(`ERRORS E1 ${JSON.stringify(errors)}`);
  } finally {
    await bob.close();
    await alice.close();
  }
});
