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
// send-keys); browser sections are Playwright recordings at the viewport's size, each ending on a
// result it holds and then finds in its own recording's last frames. sections.json records each
// section's wall-clock length beside its file, the capture-rate check the walkthrough's build
// compares file durations against, and how closely each browser section's last frames match its
// result. Output goes to WALKTHROUGH_RAW_DIR, default
// docs/site/public/media/broker/walkthrough.src/raw.
import { execFileSync, spawnSync } from "node:child_process";
import { mkdirSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";
import { fileURLToPath } from "node:url";

import { type Browser, expect, type Locator, type Page, test } from "@playwright/test";

import { signIn } from "../../../../packages/dispatch/e2e/users";
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
  /** A browser section's result as its recording shows it: the structural similarity of the
   *  result's region in the file's last frame, and in the frame resultOnCameraSeconds before it,
   *  to the page's own screenshot of that region (1 is identical). */
  resultSimilarity?: [number, number];
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

/** How long a browser section holds on its result, how much of the end of its recording must show
 *  that result, and how alike the two must be. Under load, the browser's frames reach Playwright's
 *  recording a second or two after the page shows them, and the frames still in flight when the
 *  page closes never arrive: a take whose page held a result for 2.5 s ended on the click before
 *  it, though the assertion on the page passed. So the hold outlasts that lag, and the recording
 *  itself is checked. Measured on one take: the result's box scored 0.985-0.997 once the result was
 *  on screen, 0.04-0.70 before it appeared, and 0.88-0.91 with the pointer resting inside it. */
const resultHoldMs = 5_000;
const resultOnCameraSeconds = 2.5;
const resultMinSimilarity = 0.95;

interface ResultFrame {
  clip: { height: number; width: number; x: number; y: number };
  reference: string;
}

/** Holds a page on its result: asserts the result is on screen (inside the viewport, not merely in
 *  the DOM), moves the pointer off it as a reader would, screenshots the viewport as the frame the
 *  recording must end on, notes the tight box around the result's text, then waits out
 *  resultHoldMs. The screenshot is never clipped: Chromium renders a clipped one at the clip's
 *  size, and the recording shows that render as frames of the clip on grey. */
async function holdOnResult(page: Page, result: Locator, reference: string): Promise<ResultFrame> {
  await expect(result).toBeVisible();
  await expect(result).toBeInViewport({ ratio: 1 });
  const text = await result.evaluate((element) => {
    const range = document.createRange();
    range.selectNodeContents(element);
    const { x, y, width, height } = range.getBoundingClientRect();
    return { height, width, x, y };
  });
  await page.mouse.move(text.x + Math.min(text.width / 2, 120), text.y + text.height + 48, {
    steps: 12,
  });
  await sleep(400);
  // A box inside the viewport, a few pixels around the text, where the comparison looks.
  const x = Math.max(0, Math.floor(text.x) - 4);
  const y = Math.max(0, Math.floor(text.y) - 4);
  const clip = {
    height: Math.min(viewport.height - y, Math.ceil(text.height) + 8),
    width: Math.min(viewport.width - x, Math.ceil(text.width) + 8),
    x,
    y,
  };
  await page.screenshot({ path: reference });
  await sleep(resultHoldMs);
  return { clip, reference };
}

/** The structural similarity (ffmpeg's ssim, 1 is identical) between the frame of `video` at
 *  `at` seconds and the page's screenshot, both cropped to the result's box, in gray: the
 *  recording's chroma is subsampled, and cropping it would snap the box to even pixels, a 1-pixel
 *  shift that scores small text as unlike itself. */
function similarity(video: string, at: number, result: ResultFrame): number {
  const box = `crop=${result.clip.width}:${result.clip.height}:${result.clip.x}:${result.clip.y}`;
  const run = spawnSync(
    "ffmpeg",
    [
      "-nostats",
      "-ss",
      at.toFixed(3),
      "-i",
      video,
      "-i",
      result.reference,
      "-lavfi",
      `[0:v]format=gray,${box}[seen];[1:v]format=gray,${box}[shown];[seen][shown]ssim`,
      "-frames:v",
      "1",
      "-f",
      "null",
      "-",
    ],
    { encoding: "utf8" }
  );
  const all = run.stderr.match(/SSIM .*All:([0-9.]+)/);
  if (run.status !== 0 || all === null) {
    throw new Error(`ffmpeg could not compare ${video} at ${at}s:\n${run.stderr}`);
  }
  return Number(all[1]);
}

/** Records one browser section: a fresh signed-in context whose recording is saved as `file`, at
 *  the viewport's size (Playwright records a page at its CSS size whatever the device scale, and
 *  pads a larger video with grey). `act` ends by passing the section's result to `hold`
 *  (holdOnResult), and the saved file fails the section unless its last resultOnCameraSeconds
 *  show that result. It notes two wall-clock lengths the file's duration must fall between: the
 *  actions alone, and the page's whole life (a slow close adds a still tail, which the cut drops). */
async function browserSection(
  browser: Browser,
  file: string,
  operator: string,
  baseURL: string,
  act: (page: Page, hold: (result: Locator) => Promise<void>) => Promise<void>
): Promise<void> {
  const context = await browser.newContext({
    baseURL,
    recordVideo: { dir: join(rawDir, ".video"), size: viewport },
    viewport,
  });
  await signIn(context, operator);
  await context.addInitScript(pointer);
  const created = Date.now();
  const page = await context.newPage();
  // Where the drawn pointer starts, so the first move sweeps from it rather than from the corner.
  await page.mouse.move(viewport.width / 2, viewport.height / 2);
  const acting = Date.now();
  let result: ResultFrame | undefined;
  await act(page, async (locator) => {
    if (result !== undefined) throw new Error(`${file}: a section holds on one result, its last`);
    result = await holdOnResult(page, locator, join(rawDir, ".video", `${file}.result.png`));
  });
  if (result === undefined) throw new Error(`${file}: the section never held on its result`);
  const actSeconds = (Date.now() - acting) / 1000;
  await context.close();
  const wallSeconds = (Date.now() - created) / 1000;
  const video = join(rawDir, file);
  await page.video()?.saveAs(video);
  const length = Number(
    execFileSync(
      "ffprobe",
      ["-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", video],
      { encoding: "utf8" }
    ).trim()
  );
  const resultSimilarity: [number, number] = [
    similarity(video, length - 0.1, result),
    similarity(video, length - resultOnCameraSeconds, result),
  ];
  sections.push({ actSeconds, file, resultSimilarity, wallSeconds });
  if (Math.min(...resultSimilarity) < resultMinSimilarity) {
    throw new Error(
      `${file}: its last ${resultOnCameraSeconds}s do not show the result the page showed ` +
        `(similarity ${resultSimilarity.join(", ")}, need ${resultMinSimilarity}); the page's ` +
        `frame is ${result.reference}; re-record it`
    );
  }
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
      async (page, hold) => {
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
        await hold(page.getByText("Approved. example-host-build can start agent sessions as you."));
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
      async (page, hold) => {
        await page.goto("/");
        const row = page.getByRole("link", { name: /Secret request.*DEMO_API_KEY/s });
        await expect(row).toBeVisible();
        await sleep(2_500);
        await clickVisibly(page, row);
        await expect(page.getByText(reason)).toBeVisible();
        await sleep(4_500);
        await clickVisibly(page, page.getByRole("button", { name: "Approve" }));
        await hold(page.getByText(/^approved/i));
      }
    );

    // T4: the command ran with the key; the request names who decided it.
    await waitForScreen(/DEMO_API_KEY reached this command/, "the command's output");
    await terminalSection("t4-ran.cast", async () => {
      // The command's output alone on screen long enough for the video to open on it.
      await sleep(4_500);
      await type(`agent-secrets status ${requestId}`);
      await enter();
      await waitForScreen(/decided_by: alice/, "who decided");
      await sleep(3_500);
    });

    // B3: from the approved record to Settings, where the grant is listed with its approver.
    await browserSection(
      browser,
      "b3-grants.webm",
      rig.operator,
      rig.dispatchUrl,
      async (page, hold) => {
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
        await hold(grants.getByRole("row", { name: /DEMO_API_KEY/ }));
      }
    );
    rmSync(join(rawDir, ".video"), { force: true, recursive: true });
  } finally {
    tmux("kill-server");
    writeFileSync(join(rawDir, "sections.json"), `${JSON.stringify(sections, null, 2)}\n`);
  }
});
