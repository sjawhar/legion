// What every part of the dispatch-session-settings driver (dispatch-session-settings.ts) shares:
// the run's setup as the shell script wrote it, the person the page signs in as, the transcript
// (`== <name>`, notes, `ok <name>`, and the failure that ends a check), and the polling helpers.

export interface Rig {
  readonly work: string;
  readonly evidence: string;
  readonly omp: string;
  readonly chromium: string;
  readonly dispatchUrl: string;
  readonly envoyUrl: string;
  readonly natsUrl: string;
  /** The two models the caller's keys serve: the profile's default and fallback target first. */
  readonly models: readonly [string, string];
  readonly plugin: string;
  readonly releasedPlugin: string;
  readonly home: string;
  readonly agentDir: string;
  readonly sessionCwd: string;
  readonly tmp: string;
  readonly envoyTokenFile: string;
  readonly dispatchTokenFile: string;
  readonly pidFile: string;
  /** The profile's default thinking level (config.yml `defaultThinkingLevel`). */
  readonly defaultLevel: string;
  /** A catalog model of a provider the run holds no key for. */
  readonly unkeyedModel: string;
}

/** The person the held-open page is signed in as, and whose sends the checks make. */
export const PERSON = "alice";

// ---- the transcript -------------------------------------------------------------------------------

export class CheckFailure extends Error {}

let current = "setup";
let checkNumber = 0;
let checkStarted = Date.now();

/** The check or setup stage running now, as the transcript names it. */
export function currentName(): string {
  return current;
}
/** The running check's number, from 1; 0 during setup. */
export function currentNumber(): number {
  return checkNumber;
}

export function begin(name: string): void {
  checkNumber += 1;
  current = name;
  checkStarted = Date.now();
  process.stdout.write(`== ${name}\n`);
}
export function stage(name: string): void {
  current = name;
  process.stdout.write(`-- ${name}\n`);
}
export function note(text: string): void {
  process.stdout.write(`   ${text}\n`);
}
export function pass(): void {
  process.stdout.write(`ok ${current} (${Math.round((Date.now() - checkStarted) / 1000)}s)\n`);
}
export function fail(message: string): never {
  throw new CheckFailure(message);
}
export function expect(condition: boolean, message: string): void {
  if (!condition) fail(message);
}

// ---- waiting --------------------------------------------------------------------------------------

export const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

/** Polls `probe` every quarter second until it answers something other than undefined, for at
 *  most `limitMs`; the failure names what it waited for and the last thing it saw. */
export async function until<T>(
  what: string,
  limitMs: number,
  probe: () => Promise<T | undefined> | T | undefined,
  last?: () => string
): Promise<T> {
  const deadline = Date.now() + limitMs;
  for (;;) {
    const value = await probe();
    if (value !== undefined) return value;
    if (Date.now() > deadline) {
      fail(
        `timed out after ${limitMs / 1000}s waiting for ${what}${last ? `; last: ${last()}` : ""}`
      );
    }
    await sleep(250);
  }
}
