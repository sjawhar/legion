// Builds a narrated walkthrough video from `walkthroughs/<name>.ts`, section by section, to the
// production standard in README.md: each section is recorded on its own and cut to its real
// action; the clip is measured; the narration, written to that length, must fit it; audio is
// padded with silence, never video with frames; the sections are normalised and concatenated.
//
//   DATABASE_URL=<a database this may truncate> ELEVENLABS_API_KEY=<key> \
//     bun docs/site/media/walkthrough.ts <name | path/to/walkthrough.ts> [--only a,b] [--record-only]
//
// `--only` re-records the named sections and reuses the others' last recordings; every browser
// section's action still runs, unrecorded, so each finds the state the ones before it left.
// `--record-only` records and cuts without narrating, to iterate on a section's footage.
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { basename, dirname, join, relative, resolve } from "node:path";
import { parseArgs } from "node:util";

import { type Browser, chromium } from "@playwright/test";

import {
  assertScreenClean,
  type Harness,
  newViewerContext,
  pageErrors,
  REPO,
  WORK,
  waitForSettled,
  withHarness,
} from "./harness";
import { NARRATION_VOICE, narrate } from "./narration";
import { type BrowserSection, type CastSection, drawPointer, type Walkthrough } from "./recording";

/** Finished videos with their captions and posters; committed, served at `/legion/media/videos/`. */
export const VIDEOS = join(REPO, "docs/site/public/media/videos");
const WALKTHROUGHS = join(import.meta.dir, "walkthroughs");

/** Every video's frame, which is a browser section's viewport: Playwright records the page in CSS
 *  pixels, and at this size Dispatch's text stays legible in a docs page's content column. */
const FRAME = { width: 1024, height: 640 };
const FPS = 25;
/** Playwright's recorder opens on a blank frame: recording runs this long before the action,
 *  and the clip starts `CUT_IN` seconds in, past the blank frame and into the still, ready page. */
const LEAD_MS = 400;
/** How much shorter than the wall clock a recording may run: its first frame's delay (measured
 *  under 0.1 s) plus a frame. */
const FIRST_FRAME_SLACK = 0.25;
const CUT_IN = 0.3;
/** A terminal clip is letterboxed on agg's `asciinema` theme background. */
const CAST_THEME = "asciinema";
const CAST_BACKGROUND = "0x121314";
const CAST_FONT_SIZE = 20;
/** Where a section's narration starts in its clip unless the section says otherwise. */
const DEFAULT_AT = 0.4;

function run(command: string, args: readonly string[]): string {
  const result = Bun.spawnSync([command, ...args], { stderr: "pipe", stdout: "pipe" });
  if (result.exitCode !== 0) {
    throw new Error(`${command} ${args.join(" ")} failed:\n${result.stderr.toString().trim()}`);
  }
  return result.stdout.toString();
}

/** A media file's duration in seconds, as ffprobe reads its container. */
function duration(file: string): number {
  return Number(
    run("ffprobe", ["-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", file])
  );
}

const H264 = ["-c:v", "libx264", "-preset", "slow", "-crf", "18", "-pix_fmt", "yuv420p"];

interface Recorded {
  /** Wall-clock seconds the action took. */
  readonly action: number;
}

/** Records one browser section: the ready page, then its action, as one clip. */
async function recordBrowserSection<Seeded>(
  browser: Browser,
  harness: Harness,
  seeded: Seeded,
  section: BrowserSection<Seeded>,
  raw: string | undefined
): Promise<Recorded> {
  const context = await newViewerContext(browser, harness.baseURL, {
    deviceScaleFactor: 1,
    size: FRAME,
    theme: "light",
    viewport: "desktop",
  });
  await drawPointer(context);
  const page = await context.newPage();
  const errors = pageErrors(page);
  try {
    await section.open(page, seeded);
    await waitForSettled(page);
    await assertScreenClean(page, errors);
    if (raw !== undefined) {
      await page.screencast.start({ path: raw, size: FRAME });
      await page.waitForTimeout(LEAD_MS);
    }
    const started = performance.now();
    await section.act(page, seeded);
    const action = (performance.now() - started) / 1000;
    if (raw !== undefined) await page.screencast.stop();
    await assertScreenClean(page, errors);
    return { action };
  } finally {
    await context.close();
  }
}

/** Renders a `.cast` into a frame-sized clip: agg draws the GIF, ffmpeg letterboxes it. agg holds
 *  no final frame (`--last-frame-duration 0`) and keeps the recording's own pace. */
function renderCast(cast: string, gif: string, out: string, window?: readonly [number, number]) {
  // biome-ignore format: a command's flags read as flag-value pairs
  run("agg", [
    "--font-size", String(CAST_FONT_SIZE),
    "--theme", CAST_THEME,
    "--last-frame-duration", "0",
    cast, gif,
  ]);
  const fit =
    `fps=${FPS},scale=${FRAME.width}:${FRAME.height}:force_original_aspect_ratio=decrease,` +
    `pad=${FRAME.width}:${FRAME.height}:(ow-iw)/2:(oh-ih)/2:color=${CAST_BACKGROUND},format=yuv420p`;
  const cut = window === undefined ? [] : ["-ss", String(window[0]), "-to", String(window[1])];
  run("ffmpeg", ["-y", "-v", "error", "-i", gif, ...cut, "-vf", fit, ...H264, "-an", out]);
}

interface Cue {
  readonly from: number;
  readonly to: number;
  readonly text: string;
}

/** Caption cues for one section's narration: a cue per sentence, timed by its share of letters. */
function cues(text: string, start: number, length: number): Cue[] {
  const sentences = text.match(/[^.!?]+[.!?]*(\s+|$)/g)?.map((part) => part.trim()) ?? [text];
  const letters = sentences.reduce((sum, sentence) => sum + sentence.length, 0);
  let at = start;
  return sentences.map((sentence) => {
    const from = at;
    at += (length * sentence.length) / letters;
    return { from, text: sentence, to: at };
  });
}

function vttTime(seconds: number): string {
  const ms = Math.round(seconds * 1000);
  const pad = (value: number, width = 2) => String(value).padStart(width, "0");
  return `${pad(Math.floor(ms / 3_600_000))}:${pad(Math.floor(ms / 60_000) % 60)}:${pad(Math.floor(ms / 1000) % 60)}.${pad(ms % 1000, 3)}`;
}

const { positionals, values } = parseArgs({
  allowPositionals: true,
  options: { only: { type: "string" }, "record-only": { type: "boolean" } },
  strict: true,
});
if (positionals.length !== 1) {
  throw new Error(
    "Usage: bun walkthrough.ts <name | path/to/walkthrough.ts> [--only a,b] [--record-only]"
  );
}
const file = positionals[0].endsWith(".ts")
  ? resolve(positionals[0])
  : join(WALKTHROUGHS, `${positionals[0]}.ts`);
const name = basename(file, ".ts");
// The walkthrough is named on the command line, so it is imported by the path given.
const walkthrough = (await import(file)).default as Walkthrough<unknown>;
const sections = walkthrough.sections;
const ids = sections.map((section) => section.id);
const only = values.only === undefined ? new Set(ids) : new Set(values.only.split(","));
const unknown = [...only].filter((id) => !ids.includes(id));
if (unknown.length > 0) throw new Error(`${name} has no section ${unknown.join(", ")}.`);

const work = join(WORK, "walkthroughs", name);
mkdirSync(join(work, "narration"), { recursive: true });
const rawPath = (id: string) => join(work, `${id}.webm`);
const metaPath = (id: string) => join(work, `${id}.json`);
const clipPath = (id: string) => join(work, `${id}.mp4`);
const isCast = (section: BrowserSection<unknown> | CastSection): section is CastSection =>
  "cast" in section;

// 1. Record. The browser sections run in order against one seeded harness.
const browserSections = sections.filter(
  (section): section is BrowserSection<unknown> => !isCast(section)
);
if (browserSections.length > 0) {
  const seed = walkthrough.seed;
  if (seed === undefined) throw new Error(`${name} has browser sections and no seed.`);
  await withHarness(async (harness) => {
    await harness.reset();
    const seeded = await seed();
    const browser = await chromium.launch();
    try {
      for (const section of browserSections) {
        const record = only.has(section.id);
        if (!record && !existsSync(metaPath(section.id))) {
          throw new Error(`${name}/${section.id} has no earlier recording to reuse; record it.`);
        }
        const raw = record ? rawPath(section.id) : undefined;
        const recorded = await recordBrowserSection(browser, harness, seeded, section, raw);
        if (record) writeFileSync(metaPath(section.id), JSON.stringify(recorded));
      }
    } finally {
      await browser.close();
    }
  });
}

// 2. Cut each section to its action and measure it. The recorder stamps every frame with the wall
//    clock, sends a frame only when the page changes, and holds its last frame to the moment it
//    stopped, so the recording runs as long as the wall clock did, less the moment its first frame
//    took. The clip ends where the action did. A recording more than that moment short of the
//    wall clock lost time, and is refused.
interface Clip {
  readonly seconds: number;
  readonly note: string;
}
const clips: Clip[] = [];
for (const section of sections) {
  if (isCast(section)) {
    const cast = resolve(dirname(file), section.cast);
    renderCast(cast, join(work, `${section.id}.gif`), clipPath(section.id), section.window);
    clips.push({ note: `cast ${relative(REPO, cast)}`, seconds: duration(clipPath(section.id)) });
    continue;
  }
  const { action } = JSON.parse(readFileSync(metaPath(section.id), "utf8")) as Recorded;
  const raw = duration(rawPath(section.id));
  const wall = LEAD_MS / 1000 + action;
  if (raw < wall - FIRST_FRAME_SLACK) {
    throw new Error(
      `${name}/${section.id}: the recording runs ${raw.toFixed(2)} s for ${wall.toFixed(2)} s of ` +
        "wall clock; re-record it (--only)."
    );
  }
  const end = Math.min(wall, raw);
  // biome-ignore format: a command's flags read as flag-value pairs
  run("ffmpeg", [
    "-y", "-v", "error", "-i", rawPath(section.id),
    "-ss", CUT_IN.toFixed(3), "-to", end.toFixed(3),
    "-vf", `fps=${FPS},format=yuv420p`, ...H264, "-an", clipPath(section.id),
  ]);
  clips.push({
    note: `action ${action.toFixed(2)} s, recording ${raw.toFixed(2)} s`,
    seconds: duration(clipPath(section.id)),
  });
}
const report = sections.map(
  (section, index) =>
    `${section.id.padEnd(14)} clip ${clips[index].seconds.toFixed(2)} s (${clips[index].note})`
);
if (values["record-only"]) {
  console.log(report.join("\n"));
  process.exit(0);
}

// 3. Narrate each section and refuse narration longer than its clip. A narration is cached under
//    everything that shapes it, so a rebuild asks ElevenLabs only for words that changed.
const spoken = sections.map((section) =>
  Object.entries(walkthrough.pronounce ?? {}).reduce(
    (text, [written, said]) => text.replaceAll(written, said),
    section.narration
  )
);
const narrations: { readonly file: string; readonly seconds: number }[] = [];
const overruns: string[] = [];
for (const [index, section] of sections.entries()) {
  const context = {
    next: spoken.slice(index + 1).join(" "),
    previous: spoken.slice(0, index).join(" "),
  };
  const key = createHash("sha256")
    .update(JSON.stringify([NARRATION_VOICE, spoken[index], context]))
    .digest("hex")
    .slice(0, 16);
  const mp3 = join(work, "narration", `${section.id}-${key}.mp3`);
  if (!existsSync(mp3)) await narrate(spoken[index], mp3, context);
  const seconds = duration(mp3);
  narrations.push({ file: mp3, seconds });
  const at = section.at ?? DEFAULT_AT;
  const spare = clips[index].seconds - at - seconds;
  report[index] += `, narration ${seconds.toFixed(2)} s from ${at} s, ${spare.toFixed(2)} s spare`;
  if (spare < 0) overruns.push(`${section.id} by ${(-spare).toFixed(2)} s`);
}
console.log(report.join("\n"));
if (overruns.length > 0) {
  throw new Error(
    `Narration overruns its clip (${overruns.join(", ")}): cut words, never slow or stretch the video.`
  );
}

// 4. Lay each narration on its clip from its start offset, padded with silence to the clip's
//    length and loudness-normalised; then concatenate the sections and write the captions.
const sectionFiles: string[] = [];
const captions = ["WEBVTT", ""];
let offset = 0;
for (const [index, section] of sections.entries()) {
  const at = section.at ?? DEFAULT_AT;
  const seconds = clips[index].seconds.toFixed(3);
  const out = join(work, `${section.id}.section.mp4`);
  // biome-ignore format: a command's flags read as flag-value pairs
  run("ffmpeg", [
    "-y", "-v", "error", "-i", clipPath(section.id), "-i", narrations[index].file,
    "-filter_complex",
    "[1:a]aformat=channel_layouts=stereo,loudnorm=I=-16:TP=-1.5:LRA=11,aresample=44100," +
      `adelay=${Math.round(at * 1000)}:all=1,apad,atrim=0:${seconds}[a]`,
    "-map", "0:v", "-map", "[a]", "-c:v", "copy",
    "-c:a", "aac", "-b:a", "192k", "-ar", "44100", "-ac", "2", "-t", seconds, out,
  ]);
  sectionFiles.push(out);
  for (const cue of cues(section.narration, offset + at, narrations[index].seconds)) {
    captions.push(`${vttTime(cue.from)} --> ${vttTime(cue.to)}`, cue.text, "");
  }
  offset += duration(out);
}
mkdirSync(VIDEOS, { recursive: true });
const list = join(work, "concat.txt");
writeFileSync(list, sectionFiles.map((path) => `file '${path}'`).join("\n"));
const video = join(VIDEOS, `${name}.mp4`);
// biome-ignore format: a command's flags read as flag-value pairs
run("ffmpeg", [
  "-y", "-v", "error", "-f", "concat", "-safe", "0", "-i", list, "-c", "copy",
  "-movflags", "+faststart", video,
]);
writeFileSync(join(VIDEOS, `${name}.vtt`), captions.join("\n"));
const poster = join(VIDEOS, `${name}.jpg`);
run("ffmpeg", ["-y", "-v", "error", "-i", video, "-frames:v", "1", "-q:v", "3", poster]);
console.log(`${relative(REPO, video)}: ${duration(video).toFixed(2)} s — ${walkthrough.title}`);
