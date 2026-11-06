/**
 * The keys a reader pressed before the app's keymap existed.
 *
 * The entry chunk holds the shell's frame and asks for the app with `import()`
 * (dispatch://INFRA-341, R11), so between the page's `load` and the app's first commit there is a
 * window - about 300 ms on a phone at 4x CPU throttling, longer on a cold cache - in which the
 * skeleton is on the screen and nothing is listening for a shortcut. A reader who opens Dispatch
 * and presses `i` in that window used to open the Inbox and now presses it into nothing, because
 * the one `window` keydown listener is bound by `KeymapProvider`'s own effect
 * (`features/shell/KeymapProvider.tsx`) and the registry it resolves against is the app's.
 *
 * So the entry listens from its own task and holds what it hears until the keymap binds, which
 * replays each one through the same `appKeymap.handleKeyDown` a live press takes. The reader's
 * press is answered late rather than lost.
 *
 * What it does not hold: a key pressed into a text box, a chord's own modifier press, and
 * anything after the first few - a reader hammering a key while the page loads means the page,
 * not the keymap. And it holds nothing once the keymap has bound, which is every press after the
 * app's first commit, so a live press is never recorded or replayed.
 */

/** The most a reader can mean by pressing keys at a skeleton. */
const EARLY_KEY_LIMIT = 8;

/**
 * How long a remembered press is still the reader's. A chunk that never arrives leaves the
 * skeleton on the screen, and a key pressed half a minute ago is not an instruction to carry out
 * once it finally does.
 */
const EARLY_KEY_LIFETIME_MS = 10_000;

interface EarlyKey {
  readonly event: KeyboardEvent;
  readonly at: number;
}

let remembered: EarlyKey[] = [];
let listening: ((event: KeyboardEvent) => void) | undefined;

/** A key the app would route to a text box, not to a shortcut. */
function intoAField(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) {
    return false;
  }
  return (
    target.isContentEditable ||
    target instanceof HTMLInputElement ||
    target instanceof HTMLTextAreaElement ||
    target instanceof HTMLSelectElement
  );
}

/** Start holding the reader's presses. Called from the entry chunk, before the app is asked for. */
export function rememberEarlyKeys(): void {
  if (listening !== undefined) {
    return;
  }
  const onKeyDown = (event: KeyboardEvent) => {
    if (remembered.length >= EARLY_KEY_LIMIT || intoAField(event.target)) {
      return;
    }
    // A modifier on its own is half of a chord the keymap resolves itself; the press that
    // completes it is the one worth holding.
    if (
      event.key === "Shift" ||
      event.key === "Control" ||
      event.key === "Alt" ||
      event.key === "Meta"
    ) {
      return;
    }
    remembered.push({ event, at: performance.now() });
  };
  listening = onKeyDown;
  window.addEventListener("keydown", onKeyDown);
}

/**
 * Hand what was held to the keymap, newest press last, and stop holding. Called by
 * `KeymapProvider` the moment its own listener is bound, so a press the reader made at the
 * skeleton reaches the same registry a live one does.
 */
export function replayEarlyKeys(handle: (event: KeyboardEvent) => void): void {
  if (listening !== undefined) {
    window.removeEventListener("keydown", listening);
    listening = undefined;
  }
  const keys = remembered;
  remembered = [];
  const now = performance.now();
  for (const key of keys) {
    if (now - key.at <= EARLY_KEY_LIFETIME_MS) {
      handle(key.event);
    }
  }
}
