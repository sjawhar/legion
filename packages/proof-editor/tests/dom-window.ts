import { Window } from "happy-dom";

/**
 * Runs `run` with a happy-dom window installed as `globalThis.document` and `globalThis.window`,
 * then puts back exactly what was there and closes the window. A key Bun never defined is
 * deleted, not left as undefined, because src/tests/headless-no-dom.test.ts asserts
 * `!("document" in globalThis)`.
 */
export async function withDomWindow<T>(run: (window: Window) => Promise<T> | T): Promise<T> {
  const window = new Window();
  const previous = (["document", "window"] as const).map(
    (key) => [key, Object.getOwnPropertyDescriptor(globalThis, key)] as const
  );
  Object.assign(globalThis, { document: window.document, window });
  try {
    return await run(window);
  } finally {
    for (const [key, descriptor] of previous) {
      if (descriptor === undefined) Reflect.deleteProperty(globalThis, key);
      else Object.defineProperty(globalThis, key, descriptor);
    }
    await window.happyDOM.close();
  }
}

/** Milkdown parse rules such as the link mark's and `@milkdown/preset-gfm`'s
 *  `table_header_row.parseDOM` check `dom instanceof HTMLElement` at runtime (a paste parses DOM
 *  into a slice), and `withDomWindow` exposes only `document` and `window` from happy-dom's window
 *  onto `globalThis`, not its `HTMLElement` - so such a paste throws `HTMLElement is not defined`
 *  without it. Scoped to the one call that needs it, restored after, the same save/restore shape
 *  `withDomWindow` itself uses. */
export function withHTMLElementGlobal<T>(run: () => T): T {
  const scope = globalThis as { HTMLElement?: unknown; window: { HTMLElement: unknown } };
  const previous = scope.HTMLElement;
  scope.HTMLElement = scope.window.HTMLElement;
  try {
    return run();
  } finally {
    if (previous === undefined) delete scope.HTMLElement;
    else scope.HTMLElement = previous;
  }
}
