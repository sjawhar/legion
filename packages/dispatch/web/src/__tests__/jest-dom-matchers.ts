/**
 * jest-dom's matchers, able to report their own failures under `bun:test`.
 *
 * `expect.extend` hands a matcher a context whose `utils` is a class instance: its
 * `printReceived`, `printExpected`, `matcherHint` and `stringify` live on that instance's
 * prototype and read its internals. jest-dom destructures them — `const { printReceived } =
 * this.utils` — which unbinds them, so the first one it calls while building a failure message
 * throws `Expected this to be instanceof ExpectMatcherUtils` and that string is the whole of what
 * the reader is told: not the attribute, not the value wanted, not the value found. A passing
 * assertion never builds a message, so the suite stays green and only a failing run shows it.
 *
 * So each matcher is called with a context of plain bound functions instead. Nothing of the
 * matcher's own behaviour changes: the same matchers decide the same pass or fail, and the
 * wrapper only replaces the helpers they build their message with.
 *
 * `web/src/__tests__/jest-dom-messages.test.ts` holds this in place by reading the message a
 * failing `toHaveAttribute` throws.
 */

import * as jestDomMatchers from "@testing-library/jest-dom/matchers";

/** What `expect.extend` gives a matcher as `this`; only the parts this file touches. */
interface MatcherContext {
  readonly utils: object;
}

/** What a matcher answers: whether it passed, and the message it builds if it did not. */
interface MatcherOutcome {
  readonly pass: boolean;
  readonly message: () => string;
}

/** A matcher as `expect.extend` takes one: `unknown` arguments, so the wrapped set is assignable
 *  to Bun's `CustomMatcher`, whose own parameters are contravariant. */
type Matcher = (
  this: MatcherContext,
  ...args: unknown[]
) => MatcherOutcome | Promise<MatcherOutcome>;

/**
 * Every property `source` answers, its own and its prototypes', with each function bound to
 * `receiver` so that it keeps working once it is read off the object. `constructor` is left out:
 * it is of no use to a matcher and binding it would make a class look like a plain function.
 */
function boundProperties(source: object, receiver: object): Record<string, unknown> {
  const bound: Record<string, unknown> = {};
  for (let level: object | null = source; level !== null; level = Object.getPrototypeOf(level)) {
    for (const name of Object.getOwnPropertyNames(level)) {
      if (name === "constructor" || name in bound) continue;
      const value = (source as Record<string, unknown>)[name];
      bound[name] = typeof value === "function" ? value.bind(receiver) : value;
    }
  }
  return bound;
}

/**
 * jest-dom's matchers, each wrapped to run against a context it can read its helpers off.
 *
 * `bun:test` heads a failure with the name the wrapper was written under rather than the one it
 * is registered as - neither `Object.defineProperty(fn, "name", …)` nor a computed key moves it,
 * measured - so that first line reads `expect(received).jestDomMatcher()` for all of them. The
 * line under it is jest-dom's own hint, which names the matcher that ran.
 */
export function reportableJestDomMatchers(): Record<string, Matcher> {
  const wrapped: Record<string, Matcher> = {};
  for (const [name, matcher] of Object.entries(jestDomMatchers) as [string, Matcher][]) {
    if (typeof matcher !== "function") continue;
    wrapped[name] = function jestDomMatcher(this: MatcherContext, ...args: unknown[]) {
      const context = boundProperties(this, this);
      context.utils = boundProperties(this.utils, this.utils);
      return matcher.apply(context as unknown as MatcherContext, args);
    };
  }
  return wrapped;
}
