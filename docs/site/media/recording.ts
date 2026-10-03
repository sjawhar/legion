// What a walkthrough file (`walkthroughs/<name>.ts`) is made of, and the helpers its browser
// sections act with. `walkthrough.ts` turns one into a narrated video.
import type { BrowserContext, Locator, Page } from "@playwright/test";

/** One line of narration and where it starts in its section's clip: seconds into the clip, or
 *  the name of a cue the section's action marked, so the line is spoken once its moment has
 *  happened on screen. */
export interface NarrationLine {
  readonly at: number | string;
  readonly text: string;
}

interface SectionBase {
  readonly id: string;
  /** What the narrator says over this section, written after the clip was cut, to its length.
   *  Each line must end before the next one starts, and the last before the clip ends. */
  readonly narration: readonly NarrationLine[];
}

/** Marks the moment of the action a narration line names as its `at`. */
export type Cue = (name: string) => void;

/** A section recorded in the browser against the seeded harness. */
export interface BrowserSection<Seeded> extends SectionBase {
  /** Unrecorded: loads the page this section starts on and waits until it is ready. */
  readonly open: (page: Page, seeded: Seeded) => Promise<void>;
  /** Recorded: the section's action, from the ready page to the state it ends on. */
  readonly act: (page: Page, seeded: Seeded, cue: Cue) => Promise<void>;
  /** Unrecorded: waits for what the action's last step set going, such as the navigation or the
   *  write a click starts, before the page closes. A section that ends on a click ends its clip
   *  there, and the next section opens on the page the click loaded. */
  readonly finish?: (page: Page, seeded: Seeded) => Promise<void>;
}

/** A section rendered from an asciinema recording. */
export interface CastSection extends SectionBase {
  /** The `.cast` file, relative to the walkthrough file. */
  readonly cast: string;
  /** The seconds of the rendered recording to keep; all of it when omitted. */
  readonly window?: readonly [number, number];
}

export interface Walkthrough<Seeded> {
  readonly title: string;
  /** Seeds the freshly reset harness; required when any section is a browser section. */
  readonly seed?: () => Promise<Seeded>;
  readonly sections: readonly (BrowserSection<Seeded> | CastSection)[];
  /** Respellings the narrator needs (`{ jj: "jay-jay" }`); captions keep the written words. */
  readonly pronounce?: Readonly<Record<string, string>>;
}

/** How long the drawn pointer takes to glide to where the mouse moved. */
const GLIDE_MS = 450;

/**
 * Draws a pointer that follows the mouse: a recording shows the page, never the OS cursor.
 * (Playwright's own `screencast.showActions` also stamps every action's title on the frame.)
 */
export async function drawPointer(context: BrowserContext): Promise<void> {
  await context.addInitScript((glide: number) => {
    const install = () => {
      const pointer = document.createElement("div");
      pointer.setAttribute("aria-hidden", "true");
      pointer.innerHTML =
        '<svg width="24" height="24" viewBox="0 0 24 24"><path d="M4 2l6.5 19 2.6-7.6L20.6 11z" ' +
        'fill="#0f172a" stroke="#fff" stroke-width="1.6" stroke-linejoin="round"/></svg>';
      Object.assign(pointer.style, {
        left: "0",
        pointerEvents: "none",
        position: "fixed",
        top: "0",
        transform: "translate(-80px, -80px)",
        transition: `transform ${glide}ms ease-in-out`,
        zIndex: "2147483647",
      });
      document.documentElement.append(pointer);
      window.addEventListener(
        "mousemove",
        (event) => {
          pointer.style.transform = `translate(${event.clientX - 4}px, ${event.clientY - 2}px)`;
        },
        true
      );
      window.addEventListener(
        "mousedown",
        () => pointer.firstElementChild?.animate([{ scale: 1 }, { scale: 0.8 }, { scale: 1 }], 220),
        true
      );
    };
    if (document.readyState === "loading") window.addEventListener("DOMContentLoaded", install);
    else install();
  }, GLIDE_MS);
}

/** Glides the drawn pointer to the middle of `target` and waits for it to arrive, so a click or
 *  a hover the caller makes next lands where the viewer is already looking. The target must
 *  already be on screen: scroll to it with `scrollBy`, where the viewer sees the page move, since
 *  an instant jump reads as a cut. */
export async function pointTo(page: Page, target: Locator): Promise<void> {
  const box = await target.boundingBox();
  if (box === null) throw new Error("pointTo: the target is not rendered");
  const viewport = page.viewportSize();
  if (viewport === null) throw new Error("pointTo: the page has no viewport");
  if (box.y < 0 || box.x < 0 || box.y + box.height > viewport.height) {
    throw new Error(
      `pointTo: the target is off screen (top ${Math.round(box.y)} px, height ` +
        `${Math.round(box.height)} px, viewport ${viewport.height} px); scrollBy to it first.`
    );
  }
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2, { steps: 8 });
  await page.waitForTimeout(GLIDE_MS + 50);
}

/** Rings `target` with a drawn outline that moves there from whatever it ringed before, and waits
 *  for it to settle, leaving the pointer where it is. The ring is placed in the viewport, so any
 *  scroll fades it out. */
export async function ring(page: Page, target: Locator): Promise<void> {
  const box = await target.boundingBox();
  if (box === null) throw new Error("ring: the target is not rendered");
  await page.evaluate(
    ({ box, glide }) => {
      // Wider than tall, so a ringed line of text leaves the lines above and below it legible.
      const pad = { x: 6, y: 3 };
      const place = {
        height: `${box.height + 2 * pad.y}px`,
        left: `${box.x - pad.x}px`,
        top: `${box.y - pad.y}px`,
        width: `${box.width + 2 * pad.x}px`,
      };
      let outline = document.getElementById("walkthrough-ring");
      if (outline === null) {
        outline = document.createElement("div");
        outline.id = "walkthrough-ring";
        outline.setAttribute("aria-hidden", "true");
        Object.assign(outline.style, {
          ...place,
          border: "2px solid #2563eb",
          borderRadius: "8px",
          boxShadow: "0 0 0 3px rgb(37 99 235 / 0.2)",
          opacity: "0",
          pointerEvents: "none",
          position: "fixed",
          transition: ["opacity", "left", "top", "width", "height"]
            .map((property) => `${property} ${glide}ms ease-in-out`)
            .join(", "),
          zIndex: "2147483646",
        });
        document.documentElement.append(outline);
        // Lay the ring out unseen first, so it fades in where it lands rather than flying in.
        outline.getBoundingClientRect();
      }
      Object.assign(outline.style, { ...place, opacity: "1" });
      const placed = outline;
      window.addEventListener("scroll", () => Object.assign(placed.style, { opacity: "0" }), {
        capture: true,
        once: true,
      });
    },
    { box, glide: GLIDE_MS }
  );
  await page.waitForTimeout(GLIDE_MS + 50);
}

/** Rings `target` and glides the pointer to it together, so each thing a narration line names
 *  changes on screen as it is named: the pointer alone is too small a change to read on a still
 *  page. */
export async function highlight(page: Page, target: Locator): Promise<void> {
  await Promise.all([ring(page, target), pointTo(page, target)]);
}

/** Scrolls the page under the pointer by `pixels` (down when positive) as one steady, visible
 *  movement: a wheel turn every frame, about 0.6 s for a screen's height. */
export async function scrollBy(page: Page, pixels: number): Promise<void> {
  const steps = Math.max(1, Math.round(Math.abs(pixels) / 24));
  for (let step = 0; step < steps; step++) {
    await page.mouse.wheel(0, pixels / steps);
    await page.waitForTimeout(16);
  }
  await page.waitForTimeout(150);
}

/** A pause for the viewer to read what is on screen: real page time, never a held frame. */
export function linger(page: Page, seconds: number): Promise<void> {
  return page.waitForTimeout(seconds * 1000);
}
