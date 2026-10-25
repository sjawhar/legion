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
//
// Playwright is resolved from packages/dispatch, the package that owns the browser suite.
import { createRequire } from "node:module";
import { connect } from "nats";

import { readAgentReplay } from "./agent-replay";

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

async function withPage<T>(
  base: string,
  login: string,
  session: string,
  run: (page: PageLike) => Promise<T>
): Promise<T> {
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
    const page: PageLike = await context.newPage();
    await page.goto(`${base}/agents/${encodeURIComponent(session)}/live`);
    await page.getByTestId("agent-thread").waitFor({ timeout: 30_000 });
    return await run(page);
  } finally {
    await browser.close();
  }
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
    return { shown, tagged: await taggedUserTurns(page, base, session) };
  });
  process.stdout.write(`${JSON.stringify(counts)}\n`);
}

/** The user messages in the replay the session's agent view serves a page that opens, which
 * pi-envoy tagged as delivering a Dispatch message: [{"id": <Dispatch message id>, "text"}].
 * Read through the live socket, in the page as its person, so the page and this read are served
 * the same ring. A session with an empty replay, a view Dispatch refused or ended, and a session
 * that never answered each end the read with an error saying which (`readAgentReplay`). */
async function taggedUserTurns(
  page: PageLike,
  base: string,
  session: string
): Promise<{ id: string; text: string }[]> {
  const socket = new URL("/ws/live", base);
  socket.protocol = socket.protocol === "https:" ? "wss:" : "ws:";
  const replay = await page.evaluate(readAgentReplay, {
    emptyAfterMs: 5_000,
    sessionId: session,
    timeoutMs: 30_000,
    url: socket.href,
  });
  if (
    typeof replay !== "object" ||
    replay === null ||
    !("frames" in replay) ||
    !Array.isArray(replay.frames)
  ) {
    throw new Error(`the session's replay holds no frames: ${JSON.stringify(replay)}`);
  }
  return replay.frames.flatMap((entry: unknown) =>
    typeof entry !== "object" ||
    entry === null ||
    !("kind" in entry) ||
    entry.kind !== "message" ||
    !("message" in entry) ||
    typeof entry.message !== "object" ||
    entry.message === null ||
    !("role" in entry.message) ||
    entry.message.role !== "user" ||
    !("dispatchMessageId" in entry.message) ||
    typeof entry.message.dispatchMessageId !== "string" ||
    !("parts" in entry.message) ||
    !Array.isArray(entry.message.parts)
      ? []
      : [
          {
            id: entry.message.dispatchMessageId,
            text: entry.message.parts
              .filter(
                (part: unknown): part is { text?: string; type: string } =>
                  typeof part === "object" &&
                  part !== null &&
                  "type" in part &&
                  part.type === "text"
              )
              .map((part: { text?: string }) => part.text ?? "")
              .join("\n"),
          },
        ]
  );
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

if (command === "send") await send();
else if (command === "complete") await complete();
else if (command === "list") await list();
else if (command === "forge") await forge();
else if (command === "count") await count();
else if (command === "envelope") await envelope();
else if (command === "publish") await publish();
else
  refuse(
    `unknown command ${command ?? "(none)"}: send, complete, list, forge, count, envelope or publish`
  );
