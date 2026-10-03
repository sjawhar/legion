// What a walkthrough file (`walkthroughs/<name>.ts`) is made of, and the helpers its browser
// sections act with. `walkthrough.ts` turns one into a narrated video.
import type { BrowserContext, Locator, Page } from "@playwright/test";

interface SectionBase {
  readonly id: string;
  /** What the narrator says over this section, written after the clip was cut, to its length. */
  readonly narration: string;
  /** Seconds into the clip the narration starts; 0.4 when omitted. */
  readonly at?: number;
}

/** A section recorded in the browser against the seeded harness. */
export interface BrowserSection<Seeded> extends SectionBase {
  /** Unrecorded: loads the page this section starts on and waits until it is ready. */
  readonly open: (page: Page, seeded: Seeded) => Promise<void>;
  /** Recorded: the section's action, from the ready page to the state it ends on. */
  readonly act: (page: Page, seeded: Seeded) => Promise<void>;
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
 *  a hover the caller makes next lands where the viewer is already looking. */
export async function pointTo(page: Page, target: Locator): Promise<void> {
  await target.scrollIntoViewIfNeeded();
  const box = await target.boundingBox();
  if (box === null) throw new Error("pointTo: the target is not rendered");
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2, { steps: 8 });
  await page.waitForTimeout(GLIDE_MS + 50);
}

/** A pause for the viewer to read what is on screen: real page time, never a held frame. */
export function linger(page: Page, seconds: number): Promise<void> {
  return page.waitForTimeout(seconds * 1000);
}
