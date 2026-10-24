// The browser and bus halves of scripts/e2e/dispatch-user-turns.sh: what a person does on
// Dispatch's conversation page, what the page is served, the one read of the notification stream
// a replay needs, and the bare publish a forger with no listener needs.
//
//   bun scripts/e2e/lib/dispatch-user-turns.ts send <dispatch-url> <login> <session> <mode> <body> <shot.png>
//     opens /agents/<session>/live as <login> (Dispatch's trusted identity header), reads which mode
//     the composer starts on, picks <mode> (steer, aside or btw), types <body>, presses Send, and
//     waits until the thread shows it. Prints {"initial","label","options"}: the composer's mode on
//     opening, the label of the one picked, and every mode it offered, by label.
//   bun scripts/e2e/lib/dispatch-user-turns.ts count <dispatch-url> <login> <session> <shot.png> <body>...
//     opens the same page, waits for the thread to settle, and prints {"shown","tagged"}: how
//     many times the thread shows each <body> as a message, {"<body>": n, ...}, and every user
//     message in the replay the session's stream serves the page that pi-envoy tagged with the
//     Dispatch message it delivered, [{"id","text"}].
//   bun scripts/e2e/lib/dispatch-user-turns.ts complete <dispatch-url> <login> <session> <typed> <pick> <shot.png> [rest]
//     opens the same page on Send, types <typed> (a `/` and part of a command's name), waits for
//     the composer's slash-command list, picks the command named <pick> from it, types <rest> after
//     it when given (the command's arguments), presses Enter, and waits until the thread shows it.
//     Prints {"offered","terminalOnly","composed"}: every command the list offered for <typed> by
//     name, those it marked terminal only, and what the message held when it was sent.
//   bun scripts/e2e/lib/dispatch-user-turns.ts list <dispatch-url> <login> <session> <shot.png> <typed>...
//     opens the same page on Send and, for each <typed>, types it key by key, waits for the
//     composer's slash-command list and reads it, then empties the message; sends nothing. Prints
//     {"<typed>": {"offered","terminalOnly"}, ...}.
//   bun scripts/e2e/lib/dispatch-user-turns.ts forge <dispatch-url> <login> <session> <nats-url> <shot.png> <control> <frame>...
//     opens the same page on Send, reads the composer's list for `/`, publishes each <frame> on
//     agentstream.<session>.frames as a bare bus client while the page watches, reads the list
//     again and counts the page's error screens, then publishes <control>, a well-formed list, and
//     reads the list once more, so a run can tell frames that reached the page and were refused from
//     frames that never reached it. Prints {"before","after","errorScreens","control"}.
//   bun scripts/e2e/lib/dispatch-user-turns.ts envelope <nats-url> <session> <message-id> <seconds>
//     prints the envelope Dispatch published on notifications.agent.<session> for Dispatch message
//     <message-id>, as the notification stream holds it; exits 1 when it holds none.
//   bun scripts/e2e/lib/dispatch-user-turns.ts publish <nats-url> <subject> <data>
//     publishes <data> on <subject> as a bare bus client, with no listener in between: what any
//     process that can reach NATS can put on a session's agent subject.
//   bun scripts/e2e/lib/dispatch-user-turns.ts edit <dispatch-url> <login> <issue> <old> <new> <shot.png>
//     opens <issue>'s Spec tab as <login> (the trusted identity header, which the page's document
//     socket carries too), waits up to 15 s for the document's connection dot to read connected,
//     selects the first <old> in the editor, types <new> over it, and waits until the issue's
//     events hold an artifact.version by <login> newer than any before the edit, then closes the
//     page. Prints {"connected","version","seq"}: the version that settlement wrote and its event's
//     seq. Exits 1 naming the step that failed: a dot that never read connected is a sign-in that
//     did not reach the document's socket.
//
// Playwright is resolved from packages/dispatch, the package that owns the browser suite.
import { createRequire } from "node:module";
import { connect } from "nats";

const [command, ...args] = process.argv.slice(2);

function refuse(message: string): never {
  process.stderr.write(`dispatch-user-turns: ${message}\n`);
  process.exit(2);
}

const MODE_LABELS: Record<string, string> = { aside: "Aside", btw: "BTW", steer: "Send" };

interface PageLike {
  goto(url: string): Promise<unknown>;
  screenshot(options: { path: string; fullPage?: boolean }): Promise<unknown>;
  getByRole(role: string, options: { name: string }): LocatorLike;
  getByTestId(id: string): LocatorLike;
  waitForTimeout(ms: number): Promise<void>;
  evaluate<T, A>(fn: (arg: A) => Promise<T>, arg: A): Promise<T>;
  keyboard: { type(text: string): Promise<void> };
  locator(selector: string): LocatorLike;
}
interface LocatorLike {
  locator(selector: string): LocatorLike;
  getByRole(role: string, options: { name: string }): LocatorLike;
  waitFor(options?: { state?: string; timeout?: number }): Promise<void>;
  inputValue(): Promise<string>;
  selectOption(value: string): Promise<unknown>;
  fill(value: string): Promise<void>;
  click(): Promise<void>;
  allTextContents(): Promise<string[]>;
  evaluate<T>(fn: (element: HTMLSelectElement) => T): Promise<T>;
  getAttribute(name: string): Promise<string | null>;
  all(): Promise<LocatorLike[]>;
  press(key: string): Promise<void>;
  pressSequentially(text: string): Promise<void>;
  textContent(): Promise<string | null>;
  count(): Promise<number>;
}

/** A browser page signed in as login through Dispatch's trusted identity header, for run. */
async function withSignedInPage<T>(login: string, run: (page: PageLike) => Promise<T>): Promise<T> {
  const require = createRequire(
    new URL("../../../packages/dispatch/package.json", import.meta.url)
  );
  const { chromium } = require("@playwright/test");
  const browser = await chromium.launch();
  try {
    const context = await browser.newContext({
      extraHTTPHeaders: { "X-Dispatch-User": login },
      viewport: { height: 1000, width: 1280 },
    });
    return await run(await context.newPage());
  } finally {
    await browser.close();
  }
}

async function withPage<T>(
  base: string,
  login: string,
  session: string,
  run: (page: PageLike) => Promise<T>
): Promise<T> {
  return withSignedInPage(login, async (page) => {
    await page.goto(`${base}/agents/${encodeURIComponent(session)}/live`);
    await page.getByTestId("agent-thread").waitFor({ timeout: 30_000 });
    return await run(page);
  });
}

/** Every message the thread shows, a person's and the session's alike, as text. */
async function threadMessages(page: PageLike): Promise<string[]> {
  return page
    .getByTestId("agent-thread")
    .locator(
      '[data-testid="agent-message-user"], [data-testid="agent-message-other"], [data-testid="agent-message-assistant"], [data-testid="agent-dispatch-reply"]'
    )
    .allTextContents();
}

async function send(): Promise<void> {
  const [base, login, session, mode, body, shot] = args;
  if (!base || !login || !session || !mode || !body || !shot) {
    refuse("usage: send <dispatch-url> <login> <session> <mode> <body> <shot.png>");
  }
  const label = MODE_LABELS[mode] ?? refuse(`mode must be steer, aside or btw, not ${mode}`);
  const result = await withPage(base, login, session, async (page) => {
    const picker = page.getByRole("combobox", { name: "Delivery mode" });
    await picker.waitFor({ timeout: 30_000 });
    // The session list loads after the page; the composer's default follows what it advertises.
    await page.waitForTimeout(2_000);
    const initial = await picker.inputValue();
    const options = await picker.evaluate((select) =>
      Array.from(select.options, (option) => option.textContent ?? "")
    );
    await picker.selectOption(mode);
    const picked = await picker.evaluate(
      (select) => select.options[select.selectedIndex]?.textContent ?? ""
    );
    if (picked !== label) throw new Error(`picked ${mode} reads "${picked}", want "${label}"`);
    const composer = page.getByTestId("agent-composer");
    await composer.locator("textarea").fill(body);
    await composer.getByRole("button", { name: "Send" }).click();
    const deadline = Date.now() + 30_000;
    while (!(await threadMessages(page)).some((text) => text.includes(body))) {
      if (Date.now() > deadline) throw new Error(`the thread never showed "${body}"`);
      await page.waitForTimeout(500);
    }
    await page.screenshot({ fullPage: true, path: shot });
    return { initial, label: picked, options };
  });
  process.stdout.write(`${JSON.stringify(result)}\n`);
}

/** The composer's slash-command list as it stands: every command it offers, by name, and those it
 *  marks terminal only. */
async function offeredCommands(
  page: PageLike
): Promise<{ offered: string[]; terminalOnly: string[] }> {
  const list = page.getByRole("listbox", { name: "Slash commands" });
  await list.waitFor({ timeout: 30_000 });
  const offered: string[] = [];
  const terminalOnly: string[] = [];
  for (const option of await list.locator('[role="option"]').all()) {
    const id = (await option.getAttribute("id")) ?? "";
    const name = id.slice(id.indexOf("-option-") + "-option-".length);
    offered.push(name);
    if ((await option.textContent())?.includes("terminal only")) terminalOnly.push(name);
  }
  return { offered, terminalOnly };
}

async function complete(): Promise<void> {
  const [base, login, session, typed, pick, shot, rest = ""] = args;
  if (!base || !login || !session || !typed || !pick || !shot) {
    refuse("usage: complete <dispatch-url> <login> <session> <typed> <pick> <shot.png> [rest]");
  }
  const result = await withPage(base, login, session, async (page) => {
    const picker = page.getByRole("combobox", { name: "Delivery mode" });
    await picker.waitFor({ timeout: 30_000 });
    await picker.selectOption("steer");
    const input = page.getByTestId("agent-composer").locator("textarea");
    // Typed key by key, as a person does, so the composer sees each keystroke's caret.
    await input.pressSequentially(typed);
    const { offered, terminalOnly } = await offeredCommands(page);
    await page
      .getByRole("listbox", { name: "Slash commands" })
      .locator(`[role="option"][id$="-option-${pick}"]`)
      .click();
    if (rest !== "") await input.pressSequentially(rest);
    const composed = await input.inputValue();
    await input.press("Enter");
    const sent = composed.trimEnd();
    const deadline = Date.now() + 30_000;
    while (!(await threadMessages(page)).some((text) => text.includes(sent))) {
      if (Date.now() > deadline) throw new Error(`the thread never showed "${sent}"`);
      await page.waitForTimeout(500);
    }
    await page.screenshot({ fullPage: true, path: shot });
    return { composed, offered, terminalOnly };
  });
  process.stdout.write(`${JSON.stringify(result)}\n`);
}

async function list(): Promise<void> {
  const [base, login, session, shot, ...prefixes] = args;
  if (!base || !login || !session || !shot || prefixes.length === 0) {
    refuse("usage: list <dispatch-url> <login> <session> <shot.png> <typed>...");
  }
  const result = await withPage(base, login, session, async (page) => {
    const picker = page.getByRole("combobox", { name: "Delivery mode" });
    await picker.waitFor({ timeout: 30_000 });
    await picker.selectOption("steer");
    const input = page.getByTestId("agent-composer").locator("textarea");
    const lists: Record<string, { offered: string[]; terminalOnly: string[] }> = {};
    for (const [index, typed] of prefixes.entries()) {
      await input.pressSequentially(typed);
      lists[typed] = await offeredCommands(page);
      const suffix = typed.replaceAll(/\W+/g, "-").replaceAll(/^-+|-+$/g, "");
      const prefixShot = shot.replace(/(\.[^.]+)$/, `-${index + 1}-${suffix}$1`);
      await page.screenshot({
        fullPage: true,
        path: prefixShot === shot ? `${shot}-${index + 1}` : prefixShot,
      });
      await input.fill("");
    }
    return lists;
  });
  process.stdout.write(`${JSON.stringify(result)}\n`);
}

async function forge(): Promise<void> {
  const [base, login, session, url, shot, control, ...frames] = args;
  if (!base || !login || !session || !url || !shot || !control || frames.length === 0) {
    refuse(
      "usage: forge <dispatch-url> <login> <session> <nats-url> <shot.png> <control> <frame>..."
    );
  }
  const nc = await connect({ name: "legion-e2e-user-turns", servers: url, timeout: 10_000 });
  try {
    const result = await withPage(base, login, session, async (page) => {
      const picker = page.getByRole("combobox", { name: "Delivery mode" });
      await picker.waitFor({ timeout: 30_000 });
      await picker.selectOption("steer");
      const input = page.getByTestId("agent-composer").locator("textarea");
      const read = async () => {
        await input.pressSequentially("/");
        const listed = await offeredCommands(page);
        await input.fill("");
        return listed;
      };
      const publish = async (frame: string) => {
        nc.publish(`agentstream.${session}.frames`, new TextEncoder().encode(frame));
        await nc.flush();
      };
      const before = await read();
      for (const frame of frames) await publish(frame);
      // The relay forwards each frame as it arrives; this is time for the page to take them.
      await page.waitForTimeout(3_000);
      const after = await read();
      const errorScreens = await page.getByTestId("error-boundary").count();
      await page.screenshot({ fullPage: true, path: shot });
      await publish(control);
      await page.waitForTimeout(3_000);
      return { after, before, control: await read(), errorScreens };
    });
    process.stdout.write(`${JSON.stringify(result)}\n`);
  } finally {
    await nc.close();
  }
}

async function count(): Promise<void> {
  const [base, login, session, shot, ...bodies] = args;
  if (!base || !login || !session || !shot || bodies.length === 0) {
    refuse("usage: count <dispatch-url> <login> <session> <shot.png> <body>...");
  }
  const counts = await withPage(base, login, session, async (page) => {
    // The stored messages and the session's replay arrive separately; the thread has settled once
    // three seconds pass with nothing new in it.
    let previous = "";
    let stableSince = Date.now();
    const deadline = Date.now() + 60_000;
    for (;;) {
      const current = JSON.stringify(await threadMessages(page));
      if (current !== previous) {
        previous = current;
        stableSince = Date.now();
      } else if (Date.now() - stableSince >= 3_000 && current !== "[]") {
        break;
      }
      if (Date.now() > deadline) throw new Error("the thread did not settle within 60s");
      await page.waitForTimeout(500);
    }
    await page.screenshot({ fullPage: true, path: shot });
    const messages = await threadMessages(page);
    const shown = Object.fromEntries(
      bodies.map((body) => [body, messages.filter((text) => text.includes(body)).length])
    );
    return { shown, tagged: await taggedUserTurns(page, session) };
  });
  process.stdout.write(`${JSON.stringify(counts)}\n`);
}

/** The user messages in the replay the session's stream serves a page that opens, which pi-envoy
 *  tagged as delivering a Dispatch message: [{"id": <Dispatch message id>, "text"}]. Read over
 *  the page's own connection, so the page and this read are served the same ring. */
async function taggedUserTurns(
  page: PageLike,
  session: string
): Promise<{ id: string; text: string }[]> {
  return page.evaluate(async (sessionID) => {
    const response = await fetch(`/api/v1/agents/${encodeURIComponent(sessionID)}/stream`);
    if (!response.ok || response.body === null) throw new Error(`stream: ${response.status}`);
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) throw new Error("the stream ended before its replay");
        buffer += decoder.decode(value, { stream: true });
        for (let end = buffer.indexOf("\n\n"); end >= 0; end = buffer.indexOf("\n\n")) {
          const lines = buffer.slice(0, end).split("\n");
          buffer = buffer.slice(end + 2);
          if (!lines.includes("event: replay")) continue;
          const data = lines
            .filter((line) => line.startsWith("data: "))
            .map((line) => line.slice("data: ".length))
            .join("\n");
          const frames: {
            kind: string;
            message?: {
              role: string;
              dispatchMessageId?: unknown;
              parts: { type: string; text?: string }[];
            };
          }[] = JSON.parse(data).frames;
          return frames.flatMap((frame) =>
            frame.kind === "message" &&
            frame.message?.role === "user" &&
            typeof frame.message.dispatchMessageId === "string"
              ? [
                  {
                    id: frame.message.dispatchMessageId,
                    text: frame.message.parts
                      .filter((part) => part.type === "text")
                      .map((part) => part.text ?? "")
                      .join("\n"),
                  },
                ]
              : []
          );
        }
      }
    } finally {
      await reader.cancel();
    }
  }, session);
}

async function envelope(): Promise<void> {
  const [url, session, messageID, seconds = "10"] = args;
  if (!url || !session || !messageID) {
    refuse("usage: envelope <nats-url> <session> <message-id> <seconds>");
  }
  const nc = await connect({ name: "legion-e2e-user-turns", servers: url, timeout: 10_000 });
  try {
    // An ordered consumer given no deliver policy starts at the stream's first message; nats.js
    // refuses DeliverPolicy.All here, since it sets a start sequence of its own.
    const consumer = await nc.jetstream().consumers.get("ENVOY_NOTIFICATIONS", {
      filterSubjects: [`notifications.agent.${session}`],
    });
    const deadline = Date.now() + Number(seconds) * 1000;
    while (Date.now() < deadline) {
      const message = await consumer.next({ expires: 2_000 });
      if (!message) continue;
      const text = new TextDecoder().decode(message.data);
      const parsed = JSON.parse(text);
      const payload = typeof parsed.payload === "string" ? JSON.parse(parsed.payload) : undefined;
      if (payload?.event?.payload?.id === messageID) {
        process.stdout.write(`${text}\n`);
        return;
      }
      if (message.info.pending === 0) break;
    }
    process.stderr.write(`dispatch-user-turns: no envelope for message ${messageID}\n`);
    process.exitCode = 1;
  } finally {
    await nc.close();
  }
}

async function publish(): Promise<void> {
  const [url, subject, data] = args;
  if (!url || !subject || data === undefined) refuse("usage: publish <nats-url> <subject> <data>");
  const nc = await connect({ name: "legion-e2e-user-turns", servers: url, timeout: 10_000 });
  try {
    nc.publish(subject, new TextEncoder().encode(data));
    await nc.flush();
  } finally {
    await nc.close();
  }
}

/** The issue's `artifact.version` events by login, newest last: {version, seq}, read as login. */
async function personsVersions(
  base: string,
  login: string,
  issue: string
): Promise<{ version: number; seq: number }[]> {
  const response = await fetch(
    `${base}/api/v1/issues/${encodeURIComponent(issue)}/events?order=desc&limit=200`,
    { headers: { "X-Dispatch-User": login } }
  );
  if (!response.ok) throw new Error(`read ${issue}'s events: ${response.status}`);
  const events = (await response.json()) as {
    type: string;
    seq: number;
    actor: { kind: string; id?: string };
    payload: { version?: { number?: number } };
  }[];
  return events
    .filter(
      (event) =>
        event.type === "artifact.version" &&
        event.actor.kind === "user" &&
        event.actor.id === login &&
        typeof event.payload.version?.number === "number"
    )
    .map((event) => ({ seq: event.seq, version: event.payload.version?.number ?? 0 }))
    .sort((left, right) => left.seq - right.seq);
}

async function edit(): Promise<void> {
  const [base, login, issue, old, replacement, shot] = args;
  if (!base || !login || !issue || !old || !replacement || !shot) {
    refuse("usage: edit <dispatch-url> <login> <issue> <old> <new> <shot.png>");
  }
  const before = (await personsVersions(base, login, issue)).at(-1)?.seq ?? 0;
  const result = await withSignedInPage(login, async (page) => {
    await page.goto(`${base}/issues/${encodeURIComponent(issue)}/spec`);
    const editor = page.getByRole("textbox", { name: "Document editor" });
    await editor.waitFor({ timeout: 30_000 });
    // The connection dot's accessible name reads "connected" only once the document's socket is
    // signed in and synced (ConnectionDot.tsx; packages/dispatch/e2e/editor.ts, connectedDot).
    try {
      await page.getByRole("status", { name: "connected" }).waitFor({ timeout: 15_000 });
    } catch {
      await page.screenshot({ fullPage: true, path: shot });
      throw new Error(
        "the document's connection dot never read connected in 15 s: the sign-in did not reach its socket"
      );
    }
    // Selects the first `old` the way a drag does: a DOM range over it and the selectionchange
    // ProseMirror's DOMObserver reads (packages/dispatch/e2e/editor.ts, selectEditorText), so the
    // typing below replaces it as a person's typing does.
    await page.evaluate(async (quote: string) => {
      const root = document.querySelector('[role="textbox"][aria-label="Document editor"]');
      if (!(root instanceof HTMLElement)) throw new Error("no document editor on the page");
      root.focus();
      const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
      const runs: { node: Node; start: number; end: number }[] = [];
      let text = "";
      for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
        const start = text.length;
        text += node.textContent ?? "";
        runs.push({ end: text.length, node, start });
      }
      const index = text.indexOf(quote);
      const first = runs.find((run) => run.start <= index && index < run.end);
      const last = runs.find(
        (run) => run.start < index + quote.length && index + quote.length <= run.end
      );
      if (index < 0 || first === undefined || last === undefined) {
        throw new Error(`the editor does not hold: ${quote}`);
      }
      const range = document.createRange();
      range.setStart(first.node, index - first.start);
      range.setEnd(last.node, index + quote.length - last.start);
      const selection = window.getSelection();
      selection?.removeAllRanges();
      selection?.addRange(range);
      document.dispatchEvent(new Event("selectionchange"));
      // ProseMirror reads the selection on its next tick; the typing waits for it.
      const settled = Promise.withResolvers<void>();
      setTimeout(settled.resolve, 100);
      await settled.promise;
    }, old);
    await page.keyboard.type(replacement);
    const deadline = Date.now() + 60_000;
    for (;;) {
      const newer = (await personsVersions(base, login, issue)).filter((v) => v.seq > before);
      const settled = newer.at(-1);
      if (settled !== undefined) {
        await page.screenshot({ fullPage: true, path: shot });
        return { connected: true, seq: settled.seq, version: settled.version };
      }
      if (Date.now() > deadline) {
        await page.screenshot({ fullPage: true, path: shot });
        throw new Error(`no artifact.version by ${login} on ${issue} within 60 s of the edit`);
      }
      await page.waitForTimeout(500);
    }
  });
  process.stdout.write(`${JSON.stringify(result)}\n`);
}

if (command === "send") await send();
else if (command === "complete") await complete();
else if (command === "list") await list();
else if (command === "forge") await forge();
else if (command === "count") await count();
else if (command === "envelope") await envelope();
else if (command === "publish") await publish();
else if (command === "edit") await edit();
else
  refuse(
    `unknown command ${command ?? "(none)"}: send, complete, list, forge, count, envelope, publish or edit`
  );
