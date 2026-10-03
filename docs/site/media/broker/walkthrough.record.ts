// docs/site/media/broker/walkthrough.record.ts
//
// Records the raw footage of the broker walkthrough against the rig (rig.sh), one file per
// section, so a weak section is re-recorded alone:
//
//   t1-login.cast       the agent machine starts a machine login and prints its code
//   b1-machine.webm     the operator types the code in Dispatch and approves it
//   t2-session.cast     the login has returned; a session registers with the helper
//   t3-request.cast     the session asks for DEMO_API_KEY; the request waits for approval
//   b2-approve.webm     the request in the Inbox, its record, the approval
//   t4-ran.cast         the command ran with the key; the request names who decided it
//   b3-grants.webm      from the approved record to Settings, where the grant is listed
//
// Terminal sections are asciinema casts of one persistent shell on the agent machine (a private
// tmux server, started without the user's tmux configuration, which can draw the real hostname
// into a pane border; each cast attaches a client to it, and the shell is typed into with
// send-keys); browser sections are Playwright recordings at the viewport's size. sections.json
// records each section's wall-clock length beside its file, the capture-rate check the
// walkthrough's build compares file durations against. Output goes to WALKTHROUGH_RAW_DIR,
// default docs/site/public/media/broker/walkthrough.src/raw.
import { execFileSync } from "node:child_process";
import { mkdirSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";
import { fileURLToPath } from "node:url";

import { type Browser, expect, type Locator, type Page, test } from "@playwright/test";

import { rigState } from "./agent";

const here = dirname(fileURLToPath(import.meta.url));
const rawDir =
  process.env.WALKTHROUGH_RAW_DIR ?? join(here, "../../public/media/broker/walkthrough.src/raw");
const reason = "Publish the docs preview for PR 42 with the demo API";
const cols = 80;
const rows = 20;
const tmuxSocket = "legion-docs-broker-walkthrough";
const shell = "agent";

interface Section {
  file: string;
  /** A terminal section's span, or a browser page's whole life: from its creation to the end of
   *  its context's close. Playwright records a page from its first frame to its close, so the
   *  file is at most this long. */
  wallSeconds: number;
  /** A browser section's scripted actions alone, which its file must at least cover. */
  actSeconds?: number;
}
const sections: Section[] = [];

function tmux(...args: string[]): string {
  return execFileSync("tmux", ["-L", tmuxSocket, "-f", "/dev/null", ...args], {
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  });
}

function screen(): string {
  return tmux("capture-pane", "-p", "-J", "-t", shell);
}

async function waitForScreen(
  pattern: RegExp,
  what: string,
  timeoutMs = 90_000
): Promise<RegExpMatchArray> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const match = screen().match(pattern);
    if (match !== null) return match;
    await sleep(250);
  }
  throw new Error(`the agent's terminal never showed ${what}:\n${screen()}`);
}

/** Types text into the agent's shell a character at a time, at a person's pace. */
async function type(text: string): Promise<void> {
  for (const character of text) {
    tmux("send-keys", "-t", shell, "-l", character);
    await sleep(38 + Math.floor(Math.random() * 30));
  }
}

async function enter(): Promise<void> {
  await sleep(350);
  tmux("send-keys", "-t", shell, "Enter");
}

/** Records one terminal section: an asciinema cast of a client attached to the agent's shell
 *  while `act` runs, ended by detaching that client. */
async function terminalSection(file: string, act: () => Promise<void>): Promise<void> {
  const cast = join(rawDir, file);
  const recorder = `rec-${file.replace(/\W/g, "-")}`;
  const started = Date.now();
  tmux(
    "new-session",
    "-d",
    "-s",
    recorder,
    "-x",
    String(cols),
    "-y",
    String(rows),
    "env",
    "-u",
    "TMUX",
    "asciinema",
    "rec",
    "--overwrite",
    "--quiet",
    "--cols",
    String(cols),
    "--rows",
    String(rows),
    "--command",
    `tmux -L ${tmuxSocket} attach -t ${shell}`,
    cast
  );
  await waitForAttached(true);
  await sleep(1_200);
  await act();
  tmux("detach-client", "-s", shell);
  const deadline = Date.now() + 15_000;
  while (Date.now() < deadline && hasSession(recorder)) await sleep(100);
  if (hasSession(recorder)) throw new Error(`${file}: the recorder never exited`);
  sections.push({ file, wallSeconds: (Date.now() - started) / 1000 });
}

function hasSession(name: string): boolean {
  try {
    tmux("has-session", "-t", name);
    return true;
  } catch {
    return false;
  }
}

async function waitForAttached(attached: boolean): Promise<void> {
  const deadline = Date.now() + 15_000;
  while (Date.now() < deadline) {
    const clients = tmux("list-clients", "-t", shell, "-F", "#{client_name}").trim();
    if ((clients !== "") === attached) return;
    await sleep(100);
  }
  throw new Error(`the agent's shell never ${attached ? "gained" : "lost"} its recording client`);
}

/** A pointer drawn into the page, since a headless browser's recording shows none: it follows the
 *  mouse, pulses on a click, and starts each new page where the last one left it, so a viewer sees
 *  what is clicked. */
const pointer = `
  addEventListener("DOMContentLoaded", () => {
    const saved = JSON.parse(sessionStorage.getItem("walkthrough-pointer") ?? "[640,360]");
    let x = saved[0], y = saved[1];
    const dot = document.createElement("div");
    const place = (scale) => { dot.style.transform = "translate(" + x + "px," + y + "px) scale(" + scale + ")"; };
    dot.style.cssText = "position:fixed;left:0;top:0;width:22px;height:22px;margin:-11px 0 0 -11px;" +
      "border-radius:50%;background:rgba(37,99,235,.35);border:2px solid rgba(37,99,235,.9);" +
      "pointer-events:none;z-index:2147483647;transition:transform .12s ease-out";
    place(1);
    document.body.appendChild(dot);
    addEventListener("mousemove", (e) => { x = e.clientX; y = e.clientY; place(1);
      sessionStorage.setItem("walkthrough-pointer", JSON.stringify([x, y])); }, true);
    addEventListener("mousedown", () => place(0.7), true);
    addEventListener("mouseup", () => place(1), true);
  });
`;

const viewport = { height: 720, width: 1280 };

/** Records one browser section: a fresh signed-in context whose recording is saved as `file`, at
 *  the viewport's size (Playwright records a page at its CSS size whatever the device scale, and
 *  pads a larger video with grey). It notes two wall-clock lengths the file's duration must fall
 *  between: the actions alone, and the page's whole life (a slow close adds a still tail, which
 *  the cut drops). */
async function browserSection(
  browser: Browser,
  file: string,
  operator: string,
  baseURL: string,
  act: (page: Page) => Promise<void>
): Promise<void> {
  const context = await browser.newContext({
    baseURL,
    extraHTTPHeaders: { "X-Dispatch-User": operator },
    recordVideo: { dir: join(rawDir, ".video"), size: viewport },
    viewport,
  });
  await context.addInitScript(pointer);
  const created = Date.now();
  const page = await context.newPage();
  // Where the drawn pointer starts, so the first move sweeps from it rather than from the corner.
  await page.mouse.move(viewport.width / 2, viewport.height / 2);
  const acting = Date.now();
  await act(page);
  const actSeconds = (Date.now() - acting) / 1000;
  await context.close();
  const wallSeconds = (Date.now() - created) / 1000;
  await page.video()?.saveAs(join(rawDir, file));
  sections.push({ actSeconds, file, wallSeconds });
}

/** Moves the pointer to a locator's centre in visible steps. */
async function pointAt(page: Page, locator: Locator): Promise<void> {
  const box = await locator.boundingBox();
  if (box === null) throw new Error("pointAt: the target has no box");
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2, { steps: 25 });
}

/** Moves the pointer to a locator's centre in visible steps, then clicks it. */
async function clickVisibly(page: Page, locator: Locator): Promise<void> {
  await pointAt(page, locator);
  await sleep(400);
  await page.mouse.down();
  await sleep(90);
  await page.mouse.up();
}

test("record the broker walkthrough's raw footage", async ({ browser }) => {
  test.setTimeout(900_000);
  const rig = rigState();
  rmSync(rawDir, { force: true, recursive: true });
  mkdirSync(rawDir, { recursive: true });

  // One persistent shell on the agent machine, which every terminal section attaches to.
  tmux(
    "new-session",
    "-d",
    "-s",
    shell,
    "-x",
    String(cols),
    "-y",
    String(rows),
    rig.agentExec,
    "bash"
  );
  tmux("set-option", "-g", "status", "off");
  await waitForScreen(/example-host-build:~\/demo[#$] $/m, "its prompt");
  tmux("send-keys", "-t", shell, "clear", "Enter");
  await sleep(500);

  try {
    // T1: the machine login prints its code.
    let code = "";
    await terminalSection("t1-login.cast", async () => {
      await type("agent-secrets launcher login");
      await enter();
      code = (
        await waitForScreen(/machine login code: ([A-Z0-9]{4}-[A-Z0-9]{4})/, "a login code")
      )[1];
      await waitForScreen(
        /approve only if the code matches this terminal/,
        "where to enter the code"
      );
      await sleep(2_500);
    });

    // B1: the operator enters the code and approves the machine.
    await browserSection(
      browser,
      "b1-machine.webm",
      rig.operator,
      rig.dispatchUrl,
      async (page) => {
        await page.goto("/credentials/machine");
        await expect(page.getByRole("heading", { name: "Enter machine login code" })).toBeVisible();
        await sleep(1_200);
        const field = page.getByLabel("Code shown on the machine");
        await clickVisibly(page, field);
        await field.pressSequentially(code, { delay: 140 });
        await sleep(500);
        await clickVisibly(page, page.getByRole("button", { name: "Look up" }));
        await expect(
          page.getByText("Approving lets example-host-build start agent sessions as you.")
        ).toBeVisible();
        await sleep(3_500);
        await clickVisibly(page, page.getByRole("button", { name: "Approve" }));
        await expect(
          page.getByText("Approved. example-host-build can start agent sessions as you.")
        ).toBeVisible();
        await sleep(2_500);
      }
    );

    // T2: the login returned; a session registers with the helper and reads its enrollment.
    await waitForScreen(
      /approve only if the code matches this terminal\n(?:.*\n)*?example-host-build:~\/demo[#$] $/m,
      "the login returning"
    );
    await terminalSection("t2-session.cast", async () => {
      await type("agent-secrets launcher login-status");
      await enter();
      await waitForScreen(/^issued$/m, "issued");
      await sleep(1_500);
      await type("agent-secrets register --wait 10 --exec -- bash");
      await enter();
      await sleep(1_500);
      await type("agent-secrets self");
      await enter();
      await waitForScreen(/operator: alice/, "the session's operator");
      await sleep(3_000);
    });

    // T3: the session asks for DEMO_API_KEY; the request waits on a person.
    tmux("send-keys", "-t", shell, "clear", "Enter");
    await sleep(500);
    let recordId = "";
    let requestId = "";
    await terminalSection("t3-request.cast", async () => {
      await type(`agent-secrets DEMO_API_KEY --reason "${reason}" -- ./check-demo-key.sh`);
      await enter();
      requestId = (
        await waitForScreen(/request (\S+) is waiting for approval/, "the pending request")
      )[1];
      recordId = (await waitForScreen(/\/credentials\/([0-9a-f]{64})/, "the record link"))[1];
      await sleep(3_000);
    });

    // B2: the request in the Inbox, its record, and the approval.
    await browserSection(
      browser,
      "b2-approve.webm",
      rig.operator,
      rig.dispatchUrl,
      async (page) => {
        await page.goto("/");
        const row = page.getByRole("link", { name: /Secret request.*DEMO_API_KEY/s });
        await expect(row).toBeVisible();
        await sleep(2_500);
        await clickVisibly(page, row);
        await expect(page.getByText(reason)).toBeVisible();
        await sleep(4_500);
        await clickVisibly(page, page.getByRole("button", { name: "Approve" }));
        await expect(page.getByText(/^approved/i)).toBeVisible();
        await sleep(2_500);
      }
    );

    // T4: the command ran with the key; the request names who decided it.
    await waitForScreen(/DEMO_API_KEY reached this command/, "the command's output");
    await terminalSection("t4-ran.cast", async () => {
      await sleep(1_800);
      await type(`agent-secrets status ${requestId}`);
      await enter();
      await waitForScreen(/decided_by: alice/, "who decided");
      await sleep(3_500);
    });

    // B3: from the approved record to Settings, where the grant is listed with its approver.
    await browserSection(browser, "b3-grants.webm", rig.operator, rig.dispatchUrl, async (page) => {
      await page.goto(`/credentials/${recordId}`);
      await expect(page.getByText(/^approved/i)).toBeVisible();
      await sleep(1_500);
      await clickVisibly(page, page.getByRole("link", { name: "Settings" }));
      const grants = page.locator("section[aria-labelledby='credential-grants-heading']");
      await expect(grants.getByText("DEMO_API_KEY")).toBeVisible();
      await sleep(1_000);
      await grants.evaluate((section) =>
        section.scrollIntoView({ behavior: "smooth", block: "start" })
      );
      await sleep(1_500);
      await pointAt(page, grants.getByRole("cell", { name: "alice", exact: true }));
      await sleep(2_000);
      await pointAt(page, grants.getByRole("button", { name: "Revoke" }));
      await sleep(3_000);
    });
  } finally {
    tmux("kill-server");
    rmSync(join(rawDir, ".video"), { force: true, recursive: true });
    writeFileSync(join(rawDir, "sections.json"), `${JSON.stringify(sections, null, 2)}\n`);
  }
});
