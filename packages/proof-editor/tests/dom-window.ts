import { Window } from "happy-dom";

/**
 * Runs `run` with a happy-dom window installed as `globalThis.document` and `globalThis.window`,
 * then puts back exactly what was there and closes the window. A key Bun never defined is
 * deleted, not left as undefined, because src/tests/headless-no-dom.test.ts asserts
 * `!("document" in globalThis)`.
 */
export async function withDomWindow<T>(run: (window: Window) => Promise<T> | T): Promise<T> {
  const window = new Window({ url: "http://localhost/" });
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
