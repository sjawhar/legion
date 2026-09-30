// The browser and bus halves of scripts/e2e/dispatch-user-turns.sh: what a person does on
// Dispatch's conversation page, what the page is served, and the one read of the notification
// stream a replay needs.
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
//   bun scripts/e2e/lib/dispatch-user-turns.ts envelope <nats-url> <session> <message-id> <seconds>
//     prints the envelope Dispatch published on notifications.agent.<session> for Dispatch message
//     <message-id>, as the notification stream holds it; exits 1 when it holds none.
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

if (command === "send") await send();
else if (command === "count") await count();
else if (command === "envelope") await envelope();
else refuse(`unknown command ${command ?? "(none)"}: send, count or envelope`);
