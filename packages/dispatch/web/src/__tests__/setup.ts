import { afterAll, afterEach, jest } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
// happy-dom has no IndexedDB; the store of a document's unsent edits (`pending-edits.ts`) runs on
// this in-memory one, which keeps IndexedDB's transaction ordering.
import "fake-indexeddb/auto";
import { notifyManager } from "@tanstack/react-query";
import serverBlockSchema from "../../../../envoy/internal/dispatch/pmdoc/schema/blocks.json";
import { resetReachability } from "../api/reachability";
import type { BlockSchema } from "../api/types";
import { blockSchemaCache } from "../features/doc/schema";
import { resetAnswerDrafts } from "../features/inbox/answer-drafts";
import { resetOutstandingAnswers } from "../features/inbox/outstanding-answers";
import { resetReplyDrafts } from "../features/inbox/reply-drafts";
import { resetPinState } from "../features/issue/pin-write";
import { resetWriteStorage } from "../features/writes/write-storage";
import { type Writes, watchWriteStores } from "../features/writes/write-store";
import { guardWritesToNetwork, writesThatReachedNetwork } from "./network-guard";
import { resetRouteAnswers } from "./send-answers";

GlobalRegistrator.register();

// Testing Library reads a global `jest` to tell whether fake timers are installed:
// `jestFakeTimersAreEnabled` (@testing-library/dom@10.4.1, dist/@testing-library/dom.esm.js:171)
// begins `if (typeof jest !== 'undefined' && jest !== null)`. Bun's `jest` is a module binding
// from `bun:test` and no global, so without this line every `waitFor` and `findBy*` under
// `jest.useFakeTimers()` takes the real-clock path and polls timers the test is holding still:
// a wait whose node is already there passes by its first synchronous check, and a wait that
// needs the clock to move can never end. That is what dispatch://INFRA-983 measured on a 2-core
// pod, where four of its files fail 16 tests without this line; on the pod this line was written
// in, nothing discriminates it, so `testing-library-fake-timers.test.tsx` holds it in place by
// asserting the global rather than by timing a wait, and says so.
(globalThis as unknown as { jest: typeof jest }).jest = jest;

// Bun's default per-test deadline is 5000 ms. The lane test of
// `e2e/__tests__/harness-lanes.test.ts` ("the Playwright config refuses a --workers that is not
// the lane count") fails at it on a 2-core pod, where it reads 4.45 s idle and 6.55 s loaded.
// The suite's slowest test, `row-configurations.test.ts`'s "no row runs twice in one
// configuration", reads 13.83 s loaded. 30 s is the round number above twice that reading, and it
// is kept as small as that rule allows, because every hung test costs the whole budget on CI as
// well as in a pod. dispatch://INFRA-1405 sets this same budget for its own loaded rows; the
// overlap is intended.
jest.setTimeout(30_000);

// Testing Library reads the document as it loads, so it must load after
// `GlobalRegistrator.register()` above and a static import would be hoisted past that call.
// `configure` comes from `@testing-library/react`, which forwards it to the
// `@testing-library/dom` copy every `waitFor` reads (`@testing-library/react@16.3.3`
// dist/config.js) and which is the only one of the two this manifest resolves.
// Testing Library's default `asyncUtilTimeout` is 1000 ms. 10 s is above every per-call budget
// this tree keeps but one, and a third of the per-test deadline, so a wait that runs out reports
// as the wait that failed and names what it waited for, rather than as a bare test timeout. The
// slowest test of a loaded run that drives no subprocess reads 5.80 s whole. Under
// `jest.useFakeTimers()` the budget is 10 s of the clock the test moves, not of real time, since
// a wait advances that clock itself. dispatch://INFRA-1405 sets this same budget too, and
// measured there that the 1000 ms default failed 13 of 30 loaded runs, a different row each
// time, which is why the budget is the suite's and not a deadline per row; the overlap is
// intended.
const { configure } = await import("@testing-library/react");
configure({ asyncUtilTimeout: 10_000 });

// Unit tests have no Dispatch server to answer GET /api/v1/schema/blocks, so the session cache
// starts already holding the checked-in server schema: every MarkdownBody, ask card, and
// document surface renders through the real schema without a network request, and a test that
// wants a different schema replaces it through its own `api.getBlockSchema` spy before the
// first render in its file.
void blockSchemaCache.load(async () => serverBlockSchema as unknown as BlockSchema);

// A route a test answers (`answerRoute`) is that test's: the next test answers its own.
afterEach(resetRouteAnswers);

// A unit test's write that reaches the network fails its test (`network-guard.ts`: `fetch` and
// the uploads' `XMLHttpRequest` both), and one a write loop makes after the file's last test -
// a retry no test is left to fail - fails the file.
guardWritesToNetwork();
const reportReachedNetwork = () => {
  const reached = writesThatReachedNetwork();
  if (reached.length === 0) return;
  throw new Error(`writes reached the network, answered by nothing: ${reached.join(", ")}`);
};
afterEach(reportReachedNetwork);
afterAll(reportReachedNetwork);

class WebSocketStub {
  binaryType = "arraybuffer";
  identifier = 0;
  readyState = 0;

  addEventListener(..._args: unknown[]): void {}

  close(): void {
    this.readyState = 3;
  }

  removeEventListener(..._args: unknown[]): void {}

  send(..._args: unknown[]): void {}
}

globalThis.WebSocket = WebSocketStub as unknown as typeof WebSocket;

// A failing assertion prints the values it compared, and happy-dom's DOM nodes print as their
// whole object graph - megabytes for one element, which buries or replaces the failure. Nodes
// print as their own markup instead. Bun asks a value for this only when a single-value matcher
// reports (`toBe`, `toBeNull`, `toBeUndefined`, `toContain`); the diff matchers
// (`toEqual`, `toStrictEqual`, `toMatchObject`, `toHaveBeenCalledWith`) and snapshots walk the
// object themselves and ignore it, so compare a node with `toBe` or map it to a string first.
Object.defineProperty(Node.prototype, Bun.inspect.custom, {
  configurable: true,
  value(this: Node): string {
    // A document has no markup and an empty `textContent`, so the node name is what is left to
    // print for one.
    return this instanceof Element
      ? this.outerHTML
      : `${this.nodeName} ${JSON.stringify(this.textContent)}`;
  },
  writable: true,
});

// TanStack batches observer notifications through a scheduler that happy-dom does not
// drive promptly; flushing synchronously keeps query updates inside React's act() scope
// so assertions after setQueryData do not wait on a stalled timer.
notifyManager.setScheduler((callback) => callback());

// Ask-reply and ask-answer drafts live in module-level stores that outlive every render, so a
// draft one test leaves behind would pre-fill the next test's composer or answer box for the
// same ask id. An answer still out when a test ends is held in one of its own
// (`outstanding-answers.ts`), which would keep the next test's reading column on that ask id.
afterEach(resetReplyDrafts);
afterEach(resetAnswerDrafts);
afterEach(resetOutstandingAnswers);

// A write a test leaves out keeps writing what it keeps, and the next test's signed-in app would
// take it up again (`Writes.resume`): each test's are let go, their trying stopped first, since a
// store outlives its test and would go on retrying into the tests and files after it
// (`watchWriteStores`). Without this, one process running every file leaves writes retrying into
// later files - sends to an agent session (`POST /api/v1/agents/<session>/messages`) and asks'
// answers (`POST /api/v1/asks/<id>/answer`), in numbers that vary from run to run - each reaching
// the network guard in whichever test is running then, and every such run fails. So is the issue
// state the last pin answered let go, which the next pin would otherwise write over.
const madeStores = new Set<Writes>();
watchWriteStores((store) => madeStores.add(store));
afterEach(() => {
  for (const store of madeStores) store.clear();
  madeStores.clear();
});
afterEach(resetWriteStorage);
afterEach(resetPinState);
// Whether Dispatch could be reached is the page's to learn afresh: a request one test failed would
// otherwise make the next test's first answer start every waiting write's backoff over.
afterEach(resetReachability);

// A fake clock a test installed is put back here as well as in that test's own `finally` or its
// file's `afterEach`, because Bun keeps it across files: a test that installs it and then fails
// before its `try` - an awaited helper of its own, say - would otherwise leave every later test
// in the process, and every later file, with timers that never fire, so one regression reads as
// timeouts everywhere and the real failure is buried. This runs after the hooks above, and
// `useRealTimers` on a test that installed nothing does nothing.
afterEach(() => {
  jest.useRealTimers();
});
