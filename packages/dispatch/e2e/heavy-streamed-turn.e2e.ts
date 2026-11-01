// The one surface where a heavy body grows under a paint: the live session view, where a
// streaming turn arrives whole with every frame. The same bodies drawn where a reader only opens
// them are `heavy-bodies.e2e.ts`, and both specs read a drawn body through `heavy-bodies-page.ts`
// (sjawhar/legion#2519).
//
// A reader who opens the view mid-turn gets a first render that is the turn's own text, pending
// on the worker; the answer to it is painted in batches across tasks, and the next frame lands
// while that paint is still attaching them. The element belongs to that paint until it ends -
// nothing but the paint that replaces it, or the unmount, stops one - so the arriving frame
// writes nothing into it. Written there, its text would stand in front of the blocks still
// arriving behind it, and that is what the reader would read until that frame's own answer and
// its whole paint had landed: the regression this change has now repeated twice
// (sjawhar/legion#2519, `useRenderedMarkdown`).
import { expect, type Page, test } from "@playwright/test";
import { type FakeSession, setLiveSessions } from "./agents";
import { publishAgentStreamFrame } from "./api";
import { expectSettled, looseTextLength, markdownBody } from "./heavy-bodies-page";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

declare global {
  interface Window {
    /** Every sample the streamed turn's recorder has taken, in order. */
    __heavyStreamSamples?: StreamSample[];
    /** Told by the row that the turn's last frame is about to go out, so every sample after it
     *  is recorded with the turn over. */
    __heavyStreamTurnEnded?: () => void;
  }
}

test.beforeEach(async () => {
  await resetDatabase();
});

/**
 * How many paragraphs the turn holds and how long the row leaves between frames, which the
 * environment may override: `DISPATCH_STREAM_PROBE_PARAGRAPHS` and
 * `DISPATCH_STREAM_PROBE_FRAME_MS`. They exist for one thing - driving this row with the window
 * it is about deliberately shut, which is how its guards are shown to be able to fail (the two
 * at the end of the row say what each proves). A body of a paragraph or two paints in one batch,
 * so no frame can land inside a paint: `DISPATCH_STREAM_PROBE_PARAGRAPHS=2` reds the first of
 * them in Chromium, with no part-built sample among the 63 it recorded. Unset, the
 * row runs the shipped shape, which is the only shape CI ever runs: nothing in the suite, the
 * harness or CI sets either variable (sjawhar/legion#2519).
 *
 * A value that is not a positive whole number is refused naming the variable and the value,
 * rather than quietly read as the default: a mistyped override would otherwise run the shipped
 * shape under a title saying it ran another one.
 */
function probed(variable: string, shipped: number): number {
  const given = process.env[variable];
  if (given === undefined || given === "") {
    return shipped;
  }
  const value = Number(given);
  if (!Number.isInteger(value) || value <= 0) {
    throw new Error(
      `${variable} must be a positive whole number of its own unit, not ${JSON.stringify(given)}`
    );
  }
  return value;
}

// The shape is the whole trick, and a row that shrinks it proves nothing. At 4,000 paragraphs
// and 80 ms a frame, this row reds in all three engines at the code before the fix - 36 of 142
// recorded samples held both in Chromium, 27 of 96 in WebKit, 17 of 156 in Firefox, the widest
// standing 63,864 characters in front of 250 blocks. At 900 paragraphs with 150 ms frames the
// pull request measured it green at both heads: that paint is two batches and no frame lands
// inside one.

/** The paragraphs the turn already holds when the reader opens it. Each frame after that is the
 *  whole turn again, one paragraph longer. */
const streamedParagraphs = probed("DISPATCH_STREAM_PROBE_PARAGRAPHS", 4_000);
/** Frames the session publishes while the reader watches, and the gap between them. */
const streamedFrames = 60;
const streamedFrameGapMs = probed("DISPATCH_STREAM_PROBE_FRAME_MS", 80);
/** The turn's last frame, the one that ends it, adds one paragraph like the frames before it. */
const wholeTurnParagraphs = streamedParagraphs + streamedFrames + 1;

/** Loose text shorter than this is not a frame standing in front of a body: a paint attaches
 *  blocks, never prose, so what this passes over is whitespace and nothing a reader reads. The
 *  text of one of these frames is some 60,000 characters. */
const looseTextIsAFrame = 100;

/** The session the turn streams from. Seeded with no `last_seen`, so the fake Envoy answers
 *  every read of it with the current time and the row never meets an aged-out session. */
const streamingSession: FakeSession = {
  capabilities: ["steer"],
  dir: "/workspaces/streaming",
  machine_id: "streaming-host",
  roles: [],
  session_id: "019a1c07-6d4b-7e80-bf39-4a1b5c8d9e07",
  title: "A session mid-turn",
};

/** What the recorder took of the newest assistant turn's body on one mutation of the page. */
interface StreamSample {
  /** `Date.now()` when this was taken, to hold against the moments the row published a frame. */
  readonly at: number;
  /** The `p` and `li` under it: the blocks a paint has attached so far. */
  readonly blocks: number;
  /** Whether this mutation put nodes into a body that already held blocks and took none of them
   *  out: a batch of a paint that was part-way through, which is the window this row is about.
   *  A paint that lands whole - every body painted on the page's own thread - replaces what the
   *  element held, so no batch of it is ever one of these. */
  readonly grew: boolean;
  /** When the paint this sample belongs to put its first batch in, so the row can say which
   *  frames were published while it was part-way through. */
  readonly paintedFrom: number;
  /** The characters in the text nodes directly under it, which is where a frame's own text
   *  would stand. */
  readonly raw: number;
  /** Whether the turn was still streaming when this was taken. */
  readonly streaming: boolean;
}

/** `count` paragraphs, `paragraph 0` to `paragraph <count - 1>`, joined by blank lines. */
function streamedBody(count: number): string {
  return Array.from({ length: count }, (_, index) => `paragraph ${index}`).join("\n\n");
}

/** One snapshot of the one turn: every frame carries the same message id, so each replaces the
 *  turn's whole text, and `seq` orders the snapshots. */
function turnFrame(seq: number, paragraphs: number, streaming: boolean): object {
  return {
    kind: "message",
    message: {
      at: 1_000,
      id: "a-turn-a-reader-opened-while-it-streamed",
      parts: [{ text: streamedBody(paragraphs), type: "text" }],
      role: "assistant",
      streaming,
    },
    seq,
    v: 1,
  };
}

/**
 * Installs the recorder before the page's own scripts, so nothing of the first render happens
 * unwatched. It watches the document rather than its element, which a script running this early
 * can still find nothing of, and reads the newest assistant turn's body on every mutation
 * anywhere in the page: the body itself is not in the document yet when this runs.
 *
 * Besides what the body holds, each sample says how it came to hold it, which is what a mutation
 * record carries and a poll of the element never could: a batch that added nodes and removed
 * none, to an element already holding blocks, is a paint part-way through, and anything else
 * that added nodes - a `replaceChildren` of the whole body, the first render's own text - begins
 * one.
 */
async function recordTheStreamedTurn(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const samples: StreamSample[] = [];
    let streaming = true;
    window.__heavyStreamSamples = samples;
    window.__heavyStreamTurnEnded = () => {
      streaming = false;
    };
    /** The body the sample before this one was of: a fresh element holds nothing of its own. */
    let watched: Element | undefined;
    /** What that body held before this mutation, and when the paint filling it began. */
    let held = 0;
    let paintedFrom = 0;
    const take = (records: MutationRecord[]): void => {
      const turns = document.querySelectorAll('[data-testid="agent-message-assistant"]');
      const body = turns[turns.length - 1]?.querySelector(".dispatch-markdown") ?? null;
      if (body === null) {
        return;
      }
      if (body !== watched) {
        watched = body;
        held = 0;
        paintedFrom = 0;
      }
      let added = 0;
      let removed = 0;
      for (const record of records) {
        if (record.target !== body) {
          continue;
        }
        added += record.addedNodes.length;
        removed += record.removedNodes.length;
      }
      const at = Date.now();
      const grew = added > 0 && removed === 0 && held > 0;
      if (added > 0 && !grew) {
        paintedFrom = at;
      }
      let raw = 0;
      for (const child of body.childNodes) {
        if (child.nodeType === Node.TEXT_NODE) {
          raw += child.textContent?.length ?? 0;
        }
      }
      const blocks = body.querySelectorAll("p, li").length;
      samples.push({ at, blocks, grew, paintedFrom, raw, streaming });
      held = blocks;
    };
    new MutationObserver(take).observe(document, {
      characterData: true,
      childList: true,
      subtree: true,
    });
  });
}

/** Waits until `at`, so the frames keep their cadence whatever each publish costs: a publish
 *  that overran its slot is followed at once by the next rather than pushing every one after
 *  it back. */
function sleepUntil(at: number): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, Math.max(0, at - Date.now()));
  return promise;
}

test("a reader who opens the live view while a long turn streams is never shown a frame's text in front of the body", async ({
  browser,
}) => {
  const session = streamingSession.session_id;
  await setLiveSessions([streamingSession]);
  await publishAgentStreamFrame(
    session,
    { frames: [turnFrame(1, streamedParagraphs, true)], session_id: session, v: 1 },
    "replay"
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await recordTheStreamedTurn(page);
    await page.goto(`/agents/${session}/live`);
    const turns = page.getByTestId("agent-message-assistant");
    // The replay in the page is the viewer attached: a frame published before that reaches
    // nobody. The turn is the reader's own text here, with its paint still to come.
    await expect(turns).toHaveCount(1);
    const turn = turns.last();

    /** When the server had each frame of the stream, which is a socket hop short of when the
     *  page had it: what a frame landing inside a paint is read from, below. */
    const publishedAt: number[] = [];
    const firstFrameAt = Date.now();
    for (let frame = 0; frame < streamedFrames; frame += 1) {
      await sleepUntil(firstFrameAt + frame * streamedFrameGapMs);
      await publishAgentStreamFrame(
        session,
        turnFrame(frame + 2, streamedParagraphs + frame + 1, true)
      );
      publishedAt.push(Date.now());
    }
    // Every sample after this is the turn over, so the guards below read only what the
    // streaming window left.
    await page.evaluate(() => window.__heavyStreamTurnEnded?.());
    await publishAgentStreamFrame(
      session,
      turnFrame(streamedFrames + 2, wholeTurnParagraphs, false)
    );

    const body = markdownBody(turn);
    await expect(body.locator("p")).toHaveCount(wholeTurnParagraphs);
    await expectSettled(turn);
    expect(
      await looseTextLength(body),
      "the turn ended with text of its own standing beside the body's blocks"
    ).toBe(0);

    const samples = await page.evaluate(() => window.__heavyStreamSamples ?? []);
    const inFront = samples.filter((sample) => sample.blocks > 0 && sample.raw > looseTextIsAFrame);
    // A paint caught part-way through: a batch of it landed in a body already holding blocks of
    // the same paint. A body painted in one go replaces what the element held, so it leaves
    // none of these however many frames the turn takes.
    const partBuilt = samples.filter((sample) => sample.streaming && sample.grew);
    // ... and a frame that reached the server while such a paint was between two of its batches,
    // which is the moment the defect is about. The frame's own text landing in the element is
    // not what says so: before the first paint, every mount writes it, whatever the node's speed
    // and whatever this row is measuring, so counting it proved only that the page had loaded;
    // after the first paint, it is the defect itself, which `inFront` holds at zero
    // (sjawhar/legion#2519).
    const framesInside = partBuilt.filter((sample) =>
      publishedAt.some((at) => at > sample.paintedFrom && at < sample.at)
    );
    // All three counts are the run's evidence, kept on the result even when it passes: a runner
    // too fast or too slow to reach the window is read here rather than as a silently empty
    // green.
    test.info().annotations.push({
      description:
        `${samples.length} samples, ${partBuilt.length} of them a batch landing in a part-built ` +
        `body mid-stream, ${framesInside.length} of those with a frame published inside that ` +
        `paint`,
      type: "recorded while the turn streamed",
    });
    const worst = inFront.reduce((held, sample) => (sample.raw > held.raw ? sample : held), {
      at: 0,
      blocks: 0,
      grew: false,
      paintedFrom: 0,
      raw: 0,
      streaming: false,
    } satisfies StreamSample);
    expect(
      inFront.length,
      `the reader saw the frame's raw text in front of a body already holding blocks: ` +
        `${inFront.length} of ${samples.length} recorded samples held both, the widest ` +
        `${worst.raw} characters in front of ${worst.blocks} blocks`
    ).toBe(0);
    // The window the row is about was driven at all, which is two things and not one: a paint
    // ran across tasks, and a frame arrived while one was between its batches. A green run means
    // the arriving frame wrote nothing into the element; a run that measured neither is no
    // evidence of that, and on a node faster than this pod - one that paints the whole turn in a
    // single batch, where no frame can land inside one - this row would otherwise stay green for
    // as long as it is in the suite while proving nothing (the pull request measured exactly
    // that at 900 paragraphs and 150 ms frames, green at both heads). Either guard failing with
    // neither override set names the shape as outgrown by the node, not the change as broken:
    // raise the paragraph count. One sample is as loose as each asks for; three runs of each
    // engine on a two-core pod left 17 to 94 part-built samples, and a frame published inside
    // the paint for 17 to 88 of them.
    expect(
      partBuilt.length,
      `no paint of this turn ran across tasks, so this row measured none of the window it is ` +
        `about: no batch landed in a body already holding blocks in ${samples.length} recorded ` +
        `samples, at ${streamedParagraphs} paragraphs and ${streamedFrameGapMs} ms a frame`
    ).toBeGreaterThan(0);
    expect(
      framesInside.length,
      `no frame was published while a paint was part-way through, so this row measured none of ` +
        `the window it is about: none of ${partBuilt.length} part-built samples in ` +
        `${samples.length} recorded ones followed a frame published into its own paint, at ` +
        `${streamedParagraphs} paragraphs and ${streamedFrameGapMs} ms a frame`
    ).toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});
