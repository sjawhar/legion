// The DOM this file's imports read is registered by `register-dom.ts`, the preload before this
// one: Bun's preload list is the one order that holds, and an import written first inside a file
// is not (`register-dom.ts` and package.json's `test:web` script say why).
import { afterAll, afterEach, expect, jest } from "bun:test";
import { configure } from "@testing-library/react";
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
import { reportableJestDomMatchers } from "./jest-dom-matchers";
import { forgetTestAnswers, requestsThatReachedNetwork } from "./network";
import { resetRouteAnswers } from "./send-answers";

// The DOM assertions (`toBeInTheDocument`, `toHaveAttribute`, `toHaveFocus`, …): one matcher that
// says what it asks for and prints the element it read when it fails, in place of a hand-written
// read of an attribute compared with `toBe`. `jest-dom.d.ts` declares them for `bun:test`, and
// `jest-dom-matchers.ts` gives them the matcher context they build that message with, which
// `expect.extend` does not hand them in a form they can read.
expect.extend(reportableJestDomMatchers());

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

// Two budgets, because a 2-core pod with its cores busy is slower than either default and a suite
// that waits on the machine reports as a defect of the change under test. Testing Library's own
// `asyncUtilTimeout` is 1000 ms, which a loaded run of this suite passes while a page is still
// rendering: it failed `settings.test.tsx`'s own heading wait and `mermaid-diagrams.test.tsx`'s
// draw in 13 of 30 loaded runs here, each time a different row, which is why the budget is the
// suite's and not a deadline per row. 10 s is a ceiling that names something wrong rather than
// slow, and a third of the per-test deadline, so a wait that runs out reports as the wait that
// failed and names what it waited for rather than as a bare test timeout. Bun's own per-test
// default is 5 s, which the same runs pass, so the deadline is raised with it.
//
// Under `jest.useFakeTimers()` the same budget is 10 s of *fake* time, not of the machine's:
// Testing Library advances the held clock by its polling interval until the wait succeeds or the
// deadline passes, so a wait that does not succeed now moves that clock ten times as far as it
// did. That cannot turn a row green that was red - a product timer the longer advance reaches
// could only make a wait succeed that used to give up - and the 21 files that install a clock and
// wait were green before this budget and are green after it, so no row's claim rested on the old
// 1000 ms. What it does cost is a failing wait under a held clock reporting ten times slower.
//
// cybermonklabs/legion#2894 (dispatch://INFRA-1505) sets these same two budgets at these same two
// numbers in this same file: whichever of the two merges second keeps one copy of them, and that
// copy is #2894's, which carries the per-call measurements that sized them, plus the two sentences
// here that it does not say - this paragraph, and the 13-of-30 measurement above.
jest.setTimeout(30_000);
configure({ asyncUtilTimeout: 10_000 });

// Unit tests have no Dispatch server to answer GET /api/v1/schema/blocks, so the session cache
// starts already holding the checked-in server schema: every MarkdownBody, ask card, and
// document surface renders through the real schema without a network request, and a test that
// wants a different schema replaces it through its own `api.getBlockSchema` spy before the
// first render in its file.
void blockSchemaCache.load(async () => serverBlockSchema as unknown as BlockSchema);

// A route a test answers - `answerRoute` above this seam, `answerRequests` at it - is that test's:
// the next test answers its own.
afterEach(resetRouteAnswers);
afterEach(forgetTestAnswers);

// A request answered by nothing fails its test (`network.ts`), and one a page swallowed in a
// `catch`, or a write loop made after the file's last test - a retry no test is left to fail -
// fails the file.
const reportReachedNetwork = () => {
  const reached = requestsThatReachedNetwork();
  if (reached.length === 0) return;
  throw new Error(`requests reached the network, answered by nothing: ${reached.join(", ")}`);
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
// answers (`POST /api/v1/asks/<id>/answer`), in numbers that vary from run to run - and each is
// a request no handler answers (`network.ts`), which fails whichever test is running then. So is
// the issue state the last pin answered let go, which the next pin would otherwise write over.
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

// A fake clock a test installed is put back here as well as in that test's own `finally`, because
// Bun keeps it across files: a test that installs it and then fails before its `try` - an awaited
// helper of its own, say - would otherwise leave every later test in the process, and every later
// file, with timers that never fire, so one regression reads as timeouts everywhere and the real
// failure is buried. This runs after the hooks above, and `useRealTimers` on a test that
// installed nothing does nothing.
afterEach(() => {
  jest.useRealTimers();
});
