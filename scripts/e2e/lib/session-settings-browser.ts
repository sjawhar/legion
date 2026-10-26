// The one held-open Chromium page of the dispatch-session-settings driver, signed in to Dispatch as
// alice: the session's live view, its header (what it shows, and its pickers), the composer, the
// thread as shown, the stream's first replay, and a screenshot per check.
//
// Playwright is resolved from packages/dispatch, the package that owns the browser suite, and
// launched on the Chromium the rig names: a pod has no Playwright browser of its own (INFRA-42).
import { createRequire } from "node:module";
import { currentNumber, PERSON, type Rig, until } from "./session-settings-common";

interface Locator {
  selectOption(value: string): Promise<unknown>;
  fill(value: string): Promise<void>;
  click(options?: { timeout?: number }): Promise<void>;
  waitFor(options?: { state?: string; timeout?: number }): Promise<void>;
  getByRole(role: string, options: { name: string }): Locator;
  locator(selector: string): Locator;
}
interface Page {
  goto(url: string, options?: { waitUntil?: string }): Promise<unknown>;
  reload(options?: { waitUntil?: string }): Promise<unknown>;
  screenshot(options: { path: string; fullPage?: boolean }): Promise<unknown>;
  getByRole(role: string, options: { name: string }): Locator;
  getByTestId(id: string): Locator;
  evaluate<T, A>(fn: (arg: A) => T | Promise<T>, arg: A): Promise<T>;
  url(): string;
}

/** What the header shows: the model and thinking level, as the picker's value or the pill's
 *  text, whether each is a picker and what it offers, the switch line and the notice. */
export interface Header {
  readonly model: string | null;
  readonly modelPicker: boolean;
  readonly modelOptions: readonly string[];
  readonly thinking: string | null;
  readonly thinkingPicker: boolean;
  readonly thinkingOptions: readonly string[];
  readonly pickersDisabled: boolean;
  readonly status: string | null;
  readonly notice: string | null;
}

export function headerText(header: Header): string {
  return JSON.stringify({
    model: header.model,
    modelPicker: header.modelPicker,
    notice: header.notice,
    status: header.status,
    thinking: header.thinking,
    thinkingPicker: header.thinkingPicker,
  });
}

export class Browser {
  readonly #rig: Rig;
  readonly page: Page;
  readonly #close: () => Promise<void>;
  #session = "";

  private constructor(rig: Rig, page: Page, close: () => Promise<void>) {
    this.#rig = rig;
    this.page = page;
    this.#close = close;
  }

  static async open(rig: Rig): Promise<Browser> {
    const require = createRequire(
      new URL("../../../packages/dispatch/package.json", import.meta.url)
    );
    const { chromium } = require("@playwright/test");
    const browser = await chromium.launch({ executablePath: rig.chromium });
    const context = await browser.newContext({
      extraHTTPHeaders: { "X-Dispatch-User": PERSON },
      viewport: { height: 1000, width: 1280 },
    });
    const page: Page = await context.newPage();
    return new Browser(rig, page, () => browser.close());
  }

  close(): Promise<void> {
    return this.#close();
  }

  get session(): string {
    return this.#session;
  }

  /** Opens the session's live view on the one page. */
  async live(session: string): Promise<void> {
    this.#session = session;
    await this.page.goto(`${this.#rig.dispatchUrl}/agents/${encodeURIComponent(session)}/live`, {
      waitUntil: "domcontentloaded",
    });
    await this.page.getByTestId("agent-thread").waitFor({ timeout: 60_000 });
  }

  async reload(): Promise<void> {
    await this.page.reload({ waitUntil: "domcontentloaded" });
    await this.page.getByTestId("agent-thread").waitFor({ timeout: 60_000 });
  }

  async header(): Promise<Header> {
    return this.page.evaluate(() => {
      const find = (id: string) => document.querySelector(`[data-testid="${id}"]`);
      const model = find("agent-model-picker") as HTMLSelectElement | null;
      const thinking = find("agent-thinking-picker") as HTMLSelectElement | null;
      const values = (select: HTMLSelectElement | null) =>
        select === null
          ? []
          : Array.from(select.options)
              .filter((option) => !option.disabled)
              .map((option) => option.value);
      const shownThinking =
        thinking === null
          ? (find("agent-session-thinking")?.textContent?.trim() ?? null)
          : thinking.value === ""
            ? "model default"
            : thinking.value;
      return {
        model:
          model === null ? (find("agent-session-model")?.textContent?.trim() ?? null) : model.value,
        modelOptions: values(model),
        modelPicker: model !== null,
        notice: find("agent-settings-notice")?.textContent?.trim() ?? null,
        pickersDisabled: [model, thinking].some(
          (select) => select?.disabled === true || select?.getAttribute("aria-disabled") === "true"
        ),
        status: find("agent-switch-status")?.textContent?.trim() ?? null,
        thinking: shownThinking,
        thinkingOptions: values(thinking),
        thinkingPicker: thinking !== null,
      };
    }, undefined);
  }

  /** Waits until the header satisfies `wanted`; answers it and how long that took. */
  async headerUntil(
    what: string,
    limitMs: number,
    wanted: (header: Header) => boolean
  ): Promise<{ header: Header; tookMs: number }> {
    const from = Date.now();
    let last: Header | undefined;
    const header = await until(
      `the header to show ${what}`,
      limitMs,
      async () => {
        last = await this.header();
        return wanted(last) ? last : undefined;
      },
      () => (last === undefined ? "nothing" : headerText(last))
    );
    return { header, tookMs: Date.now() - from };
  }

  async pickModel(model: string): Promise<void> {
    await this.page.getByRole("combobox", { name: "Model" }).selectOption(model);
  }

  async pickThinking(level: string): Promise<void> {
    await this.page.getByRole("combobox", { name: "Thinking level" }).selectOption(level);
  }

  /** Types `body` in the composer and presses Send, as the person sends a message. */
  async send(body: string): Promise<void> {
    await this.page.getByRole("combobox", { name: "Delivery mode" }).selectOption("steer");
    const composer = this.page.getByTestId("agent-composer");
    await composer.locator("textarea").fill(body);
    await composer.getByRole("button", { name: "Send" }).click({ timeout: 60_000 });
  }

  /** The thread as the page shows it: the viewer's messages, everyone else's with the author
   *  the page names, the session's Dispatch replies, and each streamed turn's model label. */
  async thread(): Promise<{
    mine: string[];
    others: { author: string; text: string }[];
    replies: string[];
    turnModels: string[];
  }> {
    return this.page.evaluate(() => {
      const all = (id: string) => Array.from(document.querySelectorAll(`[data-testid="${id}"]`));
      return {
        mine: all("agent-message-user").map((node) => node.textContent?.trim() ?? ""),
        others: all("agent-message-other").map((node) => ({
          author: node.querySelector("p")?.textContent?.trim() ?? "",
          text: node.textContent?.trim() ?? "",
        })),
        replies: all("agent-dispatch-reply").map((node) => node.textContent?.trim() ?? ""),
        turnModels: all("agent-message-model").map((node) => node.textContent?.trim() ?? ""),
      };
    }, undefined);
  }

  /** The first replay the session's stream serves a page that opens, read over the page's own
   *  connection: its frames. */
  async replay(session: string): Promise<{ kind: string; seq: number; [key: string]: unknown }[]> {
    return this.page.evaluate(async (sessionID: string) => {
      const response = await fetch(`/api/v1/agents/${encodeURIComponent(sessionID)}/stream`);
      if (!response.ok || response.body === null) throw new Error(`stream: ${response.status}`);
      const reader = response.body.getReader();
      const decode = new TextDecoder();
      let buffer = "";
      try {
        for (;;) {
          const { done, value } = await reader.read();
          if (done) throw new Error("the stream ended before its replay");
          buffer += decode.decode(value, { stream: true });
          for (let end = buffer.indexOf("\n\n"); end >= 0; end = buffer.indexOf("\n\n")) {
            const lines = buffer.slice(0, end).split("\n");
            buffer = buffer.slice(end + 2);
            if (!lines.includes("event: replay")) continue;
            const data = lines
              .filter((line) => line.startsWith("data: "))
              .map((line) => line.slice("data: ".length))
              .join("\n");
            return JSON.parse(data).frames;
          }
        }
      } finally {
        await reader.cancel();
      }
    }, session);
  }

  /** Keeps the page as <evidence>/checks/<NN>-<name>.png, NN the running check's number. */
  async shot(name: string): Promise<void> {
    const file = `${this.#rig.evidence}/checks/${String(currentNumber()).padStart(2, "0")}-${name}.png`;
    await this.page.screenshot({ fullPage: true, path: file }).catch(() => undefined);
  }
}
