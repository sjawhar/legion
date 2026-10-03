// Builds a narrated walkthrough video from `walkthroughs/<name>.ts`, section by section, to the
// production standard in README.md: each section is recorded on its own and cut to its real
// action; the clip is measured; each narration line, written to that length, is placed at the
// moment it describes and must end before the next line starts; audio is padded with silence,
// never video with frames; the sections are normalised and concatenated. The build reports every
// silence of two seconds or more, where the picture has to be moving.
//
//   DATABASE_URL=<a database this may truncate> ELEVENLABS_API_KEY=<key> \
//     bun docs/site/media/walkthrough.ts <name | path/to/walkthrough.ts> [--only a,b] [--record-only]
//
// `--only` re-records the named sections and reuses the others' last recordings; every browser
// section's action still runs, unrecorded, so each finds the state the ones before it left.
// `--record-only` records and cuts without narrating, to iterate on a section's footage; it prints
// each clip's length and the moment of each cue its action marked.
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
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
import { CONTEXT_CHARS, NARRATION_VOICE, narrate } from "./narration";
import { type BrowserSection, type CastSection, drawPointer, type Walkthrough } from "./recording";

/** Finished videos with their captions and posters; committed, served at `/legion/media/videos/`. */
export const VIDEOS = join(REPO, "docs/site/public/media/videos");
const WALKTHROUGHS = join(import.meta.dir, "walkthroughs");

/** Every video's frame, which is a browser section's viewport: Playwright records the page in CSS
 *  pixels, and at this width Dispatch's text stays legible in a docs page's content column. The
 *  height holds the Inbox's top bar, heading and one whole ask card with its Answer button, so
 *  answering one needs no scroll. */
const FRAME = { width: 1024, height: 896 };
const FPS = 25;
/** Recording starts this long before the action, so a clip opens on a still moment of the ready
 *  page. */
const LEAD_MS = 100;
/** A terminal clip is letterboxed on agg's `asciinema` theme background. */
const CAST_THEME = "asciinema";
const CAST_BACKGROUND = "0x121314";
const CAST_FONT_SIZE = 20;
/** The shortest silence the build reports, in seconds: over it the picture must be moving. */
const SILENCE_REPORTED = 2;

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

/** Near-lossless for small UI text. `stillimage` keeps a still page still: without it, x264
 *  re-quantises sharp edges differently frame to frame, and text edges shimmer. */
// biome-ignore format: a command's flags read as flag-value pairs
const H264 = [
  "-c:v", "libx264", "-preset", "slow", "-crf", "15", "-tune", "stillimage", "-pix_fmt", "yuv420p",
];

interface Recorded {
  /** Wall-clock seconds the action took. */
  readonly action: number;
  /** Each cue the action marked, in seconds from the action's start. */
  readonly cues: Readonly<Record<string, number>>;
  /** How many frames the browser drew while recording: the capture rate's record. */
  readonly frames: number;
}

interface ScreencastFrame {
  /** When the browser drew the frame, in wall-clock milliseconds. */
  readonly at: number;
  readonly jpeg: Buffer;
}

/**
 * Writes a section's recording from the frames the browser drew: each is shown from the moment it
 * was drawn until the next one, from `from` to `to` (wall-clock milliseconds), resampled to the
 * video's frame rate and kept lossless for the cut. The browser sends a frame only when the page
 * changes, and each carries its own time, so the recording runs exactly as long as the wall clock
 * did. Playwright's own recorder is not used: it re-encodes the frames as 1 Mbit/s VP8, whose
 * keyframe every 128 frames blurs the page's text and borders for a frame or two.
 */
function writeRecording(frames: readonly ScreencastFrame[], from: number, to: number, out: string) {
  const dir = `${out}.frames`;
  rmSync(dir, { force: true, recursive: true });
  mkdirSync(dir);
  const list = ["ffconcat version 1.0"];
  let last = "";
  for (const [index, frame] of frames.entries()) {
    // A frame drawn before the recording starts is shown from its start; one drawn over in the
    // same millisecond, or before the start, is never shown.
    const start = Math.max(frame.at, from);
    const end = Math.min(frames[index + 1]?.at ?? to, to);
    if (end <= start) continue;
    last = join(dir, `${index}.jpg`);
    writeFileSync(last, frame.jpeg);
    list.push(`file '${last}'`, `duration ${((end - start) / 1000).toFixed(3)}`);
  }
  // The concat demuxer holds the last entry for its duration only when a file follows it.
  list.push(`file '${last}'`);
  const listFile = join(dir, "frames.ffconcat");
  writeFileSync(listFile, list.join("\n"));
  // biome-ignore format: a command's flags read as flag-value pairs
  run("ffmpeg", [
    "-y", "-v", "error", "-f", "concat", "-safe", "0", "-i", listFile,
    "-vf", `fps=${FPS}`, "-c:v", "libx264", "-preset", "ultrafast", "-qp", "0", out,
  ]);
  rmSync(dir, { recursive: true });
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
    const frames: ScreencastFrame[] = [];
    if (raw !== undefined) {
      const first = Promise.withResolvers<void>();
      await page.screencast.start({
        onFrame: ({ data, timestamp }) => {
          frames.push({ at: timestamp, jpeg: data });
          first.resolve();
        },
        quality: 100,
        size: FRAME,
      });
      // The browser sends the page as it stands first, so the clip opens on the ready page.
      await first.promise;
      await page.waitForTimeout(LEAD_MS);
    }
    const startedAt = Date.now();
    const started = performance.now();
    const cues: Record<string, number> = {};
    await section.act(page, seeded, (cue) => {
      if (cue in cues) throw new Error(`${section.id}: the cue ${cue} is marked twice`);
      cues[cue] = (performance.now() - started) / 1000;
    });
    const action = (performance.now() - started) / 1000;
    if (raw !== undefined) {
      const stoppedAt = Date.now();
      await page.screencast.stop();
      writeRecording(frames, startedAt - LEAD_MS, stoppedAt, raw);
    }
    await assertScreenClean(page, errors);
    await section.finish?.(page, seeded);
    return { action, cues, frames: frames.length };
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

interface Caption {
  readonly from: number;
  readonly to: number;
  readonly text: string;
}

/** Captions for one narration line: one per sentence, timed by its share of the letters. */
function captionsFor(text: string, start: number, length: number): Caption[] {
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
const rawPath = (id: string) => join(work, `${id}.mkv`);
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

// 2. Encode each section's recording as its clip and measure it. A recording runs from the lead-in
//    to the end of the action, by the wall clock, so a cue's moment in the clip is its moment in
//    the action plus the lead-in. The report sets the action's wall clock beside the recording's
//    length and the frames the browser drew, the capture rate's record.
interface Clip {
  readonly seconds: number;
  readonly note: string;
  readonly cues: Readonly<Record<string, number>>;
}
const clips: Clip[] = [];
for (const section of sections) {
  if (isCast(section)) {
    const cast = resolve(dirname(file), section.cast);
    renderCast(cast, join(work, `${section.id}.gif`), clipPath(section.id), section.window);
    clips.push({
      cues: {},
      note: `cast ${relative(REPO, cast)}`,
      seconds: duration(clipPath(section.id)),
    });
    continue;
  }
  const { action, cues, frames } = JSON.parse(
    readFileSync(metaPath(section.id), "utf8")
  ) as Recorded;
  // biome-ignore format: a command's flags read as flag-value pairs
  run("ffmpeg", [
    "-y", "-v", "error", "-i", rawPath(section.id),
    "-vf", `fps=${FPS},format=yuv420p`, ...H264, "-an", clipPath(section.id),
  ]);
  clips.push({
    cues: Object.fromEntries(Object.entries(cues).map(([cue, at]) => [cue, LEAD_MS / 1000 + at])),
    note:
      `action ${action.toFixed(2)} s, recording ${duration(rawPath(section.id)).toFixed(2)} s, ` +
      `${frames} frames`,
    seconds: duration(clipPath(section.id)),
  });
}
const report = sections.map((section, index) => {
  const { cues, note, seconds } = clips[index];
  const marked = Object.entries(cues).map(([cue, at]) => `${cue} ${at.toFixed(2)} s`);
  return (
    `${section.id} clip ${seconds.toFixed(2)} s (${note})` +
    (marked.length === 0 ? "" : `; cues ${marked.join(", ")}`)
  );
});
if (values["record-only"]) {
  console.log(report.join("\n"));
  process.exit(0);
}

// 3. Narrate each line, place it at its moment, and refuse a line that runs into the next one or
//    past its clip. A line is cached under everything that shapes it, so a rebuild asks
//    ElevenLabs only for lines whose words or neighbours changed. ElevenLabs pads its speech with
//    silence, so each line is placed and measured with its leading and trailing silence removed.
interface Line {
  readonly section: number;
  readonly text: string;
  readonly spoken: string;
  readonly at: number;
}
const lines: Line[] = sections.flatMap((section, index) => {
  if (section.narration.length === 0) throw new Error(`${name}/${section.id} has no narration.`);
  return section.narration.map((line) => {
    const at = typeof line.at === "number" ? line.at : clips[index].cues[line.at];
    if (at === undefined) {
      throw new Error(`${name}/${section.id}: no cue ${line.at} was marked for "${line.text}".`);
    }
    const spoken = Object.entries(walkthrough.pronounce ?? {}).reduce(
      (text, [written, said]) => text.replaceAll(written, said),
      line.text
    );
    return { at, section: index, spoken, text: line.text };
  });
});
const voiced: { readonly file: string; readonly seconds: number }[] = [];
for (const [index, line] of lines.entries()) {
  const neighbours = (from: number, to?: number) =>
    lines
      .slice(from, to)
      .map((other) => other.spoken)
      .join(" ");
  const context = {
    next: neighbours(index + 1).slice(0, CONTEXT_CHARS),
    previous: neighbours(0, index).slice(-CONTEXT_CHARS),
  };
  const key = createHash("sha256")
    .update(JSON.stringify([NARRATION_VOICE, line.spoken, context]))
    .digest("hex")
    .slice(0, 16);
  const mp3 = join(work, "narration", `${sections[line.section].id}-${key}.mp3`);
  if (!existsSync(mp3)) await narrate(line.spoken, mp3, context);
  const speech = mp3.replace(/\.mp3$/, ".speech.wav");
  if (!existsSync(speech)) {
    const trim = "silenceremove=start_periods=1:start_threshold=-50dB";
    // biome-ignore format: a command's flags read as flag-value pairs
    run("ffmpeg", [
      "-y", "-v", "error", "-i", mp3, "-af", `${trim},areverse,${trim},areverse`, speech,
    ]);
  }
  voiced.push({ file: speech, seconds: duration(speech) });
}
/** The indexes in `lines` of one section's lines, in the order the walkthrough lists them. */
const linesOf = (section: number) =>
  lines.flatMap((line, index) => (line.section === section ? [index] : []));
const refusals: string[] = [];
for (const [index, section] of sections.entries()) {
  const own = linesOf(index);
  for (const [position, lineIndex] of own.entries()) {
    const line = lines[lineIndex];
    const next = own[position + 1];
    const until = next === undefined ? clips[index].seconds : lines[next].at;
    const spare = until - line.at - voiced[lineIndex].seconds;
    report.push(
      `  ${section.id} at ${line.at.toFixed(2)} s, ${voiced[lineIndex].seconds.toFixed(2)} s, ` +
        `${spare.toFixed(2)} s spare: ${line.text}`
    );
    if (line.at < 0) refusals.push(`${section.id}: "${line.text}" starts before its clip`);
    if (spare < 0) {
      refusals.push(
        `${section.id}: "${line.text}" overruns ${next === undefined ? "its clip" : "the next line"} ` +
          `by ${(-spare).toFixed(2)} s`
      );
    }
  }
  // Silence over live action is fine; over a still picture it is dead air. The build names every
  // long silence for the author to check against the footage.
  let quietFrom = 0;
  for (const lineIndex of [...own, undefined]) {
    const until = lineIndex === undefined ? clips[index].seconds : lines[lineIndex].at;
    if (until - quietFrom >= SILENCE_REPORTED) {
      report.push(`  ${section.id} silent ${quietFrom.toFixed(2)}-${until.toFixed(2)} s`);
    }
    if (lineIndex !== undefined) quietFrom = lines[lineIndex].at + voiced[lineIndex].seconds;
  }
}
console.log(report.join("\n"));
if (refusals.length > 0) {
  throw new Error(
    `${refusals.join("\n")}\nCut words; never slow, stretch or freeze the video to fit them.`
  );
}

// 4. Lay each section's lines on a silent track the clip's length, at their moments and
//    loudness-normalised; then concatenate the sections and write the captions.
const sectionFiles: string[] = [];
const captions = ["WEBVTT", ""];
let offset = 0;
for (const [index, section] of sections.entries()) {
  const own = linesOf(index);
  const seconds = clips[index].seconds.toFixed(3);
  const voices = own.map(
    (lineIndex, input) =>
      `[${input + 1}:a]aformat=channel_layouts=stereo,loudnorm=I=-16:TP=-1.5:LRA=11,` +
      `aresample=44100,adelay=${Math.round(lines[lineIndex].at * 1000)}:all=1[v${input}]`
  );
  // The silent bed keeps the audio unbroken from the clip's first frame to its last. `adelay`
  // starts a line by moving its timestamps, so the lines alone would leave the section's audio
  // starting at its first line: a gap Chrome plays straight through, putting every later line
  // early. The lines never overlap, so summing them unscaled leaves each at its own loudness.
  const mixed =
    `anullsrc=r=44100:cl=stereo:d=${seconds}[bed];` +
    `[bed]${own.map((_, input) => `[v${input}]`).join("")}` +
    `amix=inputs=${own.length + 1}:duration=first:normalize=0[a]`;
  const out = join(work, `${section.id}.section.mp4`);
  // biome-ignore format: a command's flags read as flag-value pairs
  run("ffmpeg", [
    "-y", "-v", "error", "-i", clipPath(section.id),
    ...own.flatMap((lineIndex) => ["-i", voiced[lineIndex].file]),
    "-filter_complex", `${voices.join(";")};${mixed}`,
    "-map", "0:v", "-map", "[a]", "-c:v", "copy",
    "-c:a", "aac", "-b:a", "192k", "-ar", "44100", "-ac", "2", "-t", seconds, out,
  ]);
  sectionFiles.push(out);
  for (const lineIndex of own) {
    const line = lines[lineIndex];
    for (const caption of captionsFor(line.text, offset + line.at, voiced[lineIndex].seconds)) {
      captions.push(`${vttTime(caption.from)} --> ${vttTime(caption.to)}`, caption.text, "");
    }
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
